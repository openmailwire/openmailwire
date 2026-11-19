package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MTA-STS (RFC 8461) lets a receiving domain require TLS with a valid
// certificate from senders, and name the MX hosts allowed to receive its
// mail. The domain announces a policy id in DNS and serves the policy over
// HTTPS.

const (
	STSEnforce = "enforce"
	STSTesting = "testing"
	STSNone    = "none"

	stsMaxPolicySize = 64 << 10         // larger policies are refused
	stsMaxAge        = 31557600         // seconds; the RFC's maximum
	stsRecheck       = time.Minute      // how often the TXT record is consulted per domain
	stsFetchRetry    = 5 * time.Minute  // a failed fetch for one policy id is not retried sooner
	stsFetchTimeout  = 20 * time.Second // the whole HTTPS fetch
	stsMaxEntries    = 10000            // domains remembered before old entries are dropped
)

// STSPolicy is a parsed MTA-STS policy.
type STSPolicy struct {
	ID     string // from the _mta-sts TXT record
	Mode   string // enforce, testing or none
	MX     []string
	MaxAge time.Duration
}

// Matches reports whether host is one of the policy's MX patterns. A
// pattern "*.example.com" matches exactly one extra leftmost label:
// "mx1.example.com", but neither "example.com" nor "a.b.example.com".
func (p *STSPolicy) Matches(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, pat := range p.MX {
		if suffix, ok := strings.CutPrefix(pat, "*"); ok {
			label, found := strings.CutSuffix(host, suffix)
			if found && label != "" && !strings.Contains(label, ".") {
				return true
			}
			continue
		}
		if host == pat {
			return true
		}
	}
	return false
}

// STSSource returns a recipient domain's MTA-STS policy, or nil when the
// domain has none. An error is informational: delivery goes on as if the
// domain had no policy (or with the cached one), as RFC 8461 requires.
type STSSource interface {
	Policy(ctx context.Context, domain string) (*STSPolicy, error)
}

// ParseSTSRecord reads the policy id from the TXT records at
// _mta-sts.<domain>. ok is false unless exactly one record starts with
// v=STSv1 and it carries a valid id.
func ParseSTSRecord(txts []string) (id string, ok bool) {
	var rec string
	n := 0
	for _, t := range txts {
		if f := strings.SplitN(t, ";", 2); strings.TrimSpace(f[0]) == "v=STSv1" {
			rec = t
			n++
		}
	}
	if n != 1 {
		return "", false
	}
	for _, f := range strings.Split(rec, ";")[1:] {
		k, v, found := strings.Cut(strings.TrimSpace(f), "=")
		if found && strings.TrimSpace(k) == "id" {
			id = strings.TrimSpace(v)
			return id, stsIDRE.MatchString(id)
		}
	}
	return "", false
}

var (
	stsIDRE      = regexp.MustCompile(`^[a-zA-Z0-9]{1,32}$`)
	stsMXRE      = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	stsDomainRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	errNoSTSMode = errors.New("policy has no valid mode")
)

// ParseSTSPolicy reads a policy body: CRLF- (or LF-) separated "key: value"
// lines. version, mode and max_age are required, and at least one mx unless
// the mode is none. Unknown keys are ignored; for keys other than mx, the
// first occurrence counts.
func ParseSTSPolicy(body []byte) (*STSPolicy, error) {
	p := &STSPolicy{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("policy line %q is not key: value", line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "mx" {
			pat := strings.TrimSuffix(strings.ToLower(v), ".")
			if !stsMXRE.MatchString(pat) {
				return nil, fmt.Errorf("policy mx %q is not a host name or *.domain pattern", v)
			}
			p.MX = append(p.MX, pat)
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		switch k {
		case "version":
			if v != "STSv1" {
				return nil, fmt.Errorf("policy version %q is not STSv1", v)
			}
		case "mode":
			if v != STSEnforce && v != STSTesting && v != STSNone {
				return nil, fmt.Errorf("policy mode %q is not enforce, testing or none", v)
			}
			p.Mode = v
		case "max_age":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || len(v) > 10 {
				return nil, fmt.Errorf("policy max_age %q is not a number of seconds", v)
			}
			p.MaxAge = time.Duration(min(n, stsMaxAge)) * time.Second
		}
	}
	switch {
	case !seen["version"]:
		return nil, errors.New("policy has no version")
	case p.Mode == "":
		return nil, errNoSTSMode
	case !seen["max_age"]:
		return nil, errors.New("policy has no max_age")
	case p.Mode != STSNone && len(p.MX) == 0:
		return nil, errors.New("policy has no mx")
	}
	return p, nil
}

// TXTResolver looks up TXT records; *net.Resolver satisfies it.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// STSFetcher discovers, fetches and caches MTA-STS policies.
//
// A domain's TXT record is consulted at most once a minute. A policy is
// fetched when the record names an id that is not cached, and again in the
// last quarter of its max_age (at most a day before it expires). It is kept
// until its max_age passes, also when the record disappears or the next
// fetch fails.
type STSFetcher struct {
	Resolver TXTResolver
	// HTTP fetches the policies. It is used without following redirects.
	// Nil means a client that verifies certificates against the system
	// roots (or RootCAs).
	HTTP    *http.Client
	RootCAs *x509.CertPool
	Now     func() time.Time

	mu      sync.Mutex
	entries map[string]*stsEntry
	client  *http.Client
}

type stsEntry struct {
	mu        sync.Mutex
	txtID     string // the id in DNS at the last look; "" for none
	checked   time.Time
	policy    *STSPolicy
	expires   time.Time
	failID    string
	failUntil time.Time
}

func (e *stsEntry) valid(now time.Time) *STSPolicy {
	if e.policy != nil && now.Before(e.expires) {
		return e.policy
	}
	return nil
}

func (f *STSFetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *STSFetcher) entry(domain string) *stsEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries == nil {
		f.entries = map[string]*stsEntry{}
	}
	if e, ok := f.entries[domain]; ok {
		return e
	}
	if len(f.entries) >= stsMaxEntries {
		now := f.now()
		for k, e := range f.entries {
			if e.mu.TryLock() {
				stale := e.valid(now) == nil
				e.mu.Unlock()
				if stale {
					delete(f.entries, k)
				}
			}
		}
	}
	e := &stsEntry{}
	f.entries[domain] = e
	return e
}

// Policy returns domain's policy, or nil when it has none. See STSSource.
func (f *STSFetcher) Policy(ctx context.Context, domain string) (*STSPolicy, error) {
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	if !stsDomainRE.MatchString(domain) {
		return nil, nil
	}
	e := f.entry(domain)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := f.now()
	if e.checked.IsZero() || now.Sub(e.checked) >= stsRecheck {
		id, err := f.lookupID(ctx, domain)
		if err != nil {
			// DNS trouble: keep using a cached policy (RFC 8461 section 5.1)
			// and look again on the next delivery.
			return e.valid(now), fmt.Errorf("MTA-STS TXT lookup for %s: %w", domain, err)
		}
		e.txtID, e.checked = id, now
	}
	cached := e.valid(now)
	if e.txtID == "" {
		return cached, nil
	}
	if cached != nil && cached.ID == e.txtID && now.Before(e.expires.Add(-refreshBefore(cached.MaxAge))) {
		return cached, nil
	}
	// A new id, no policy, or a policy near its end: fetch. A cached policy
	// is refreshed before it expires (RFC 8461 section 5.1), so a domain
	// that keeps its policy never drops out of enforcement, even for a
	// moment; when the refresh fails, the cached policy stays until it
	// expires.
	if e.failID == e.txtID && now.Before(e.failUntil) {
		return cached, nil
	}
	p, err := f.fetch(ctx, domain)
	if err != nil {
		e.failID, e.failUntil = e.txtID, now.Add(stsFetchRetry)
		return cached, fmt.Errorf("MTA-STS policy fetch for %s: %w", domain, err)
	}
	p.ID = e.txtID
	e.policy, e.expires = p, now.Add(p.MaxAge)
	return p, nil
}

// refreshBefore is how long before its expiry a cached policy is fetched
// again: a quarter of its max_age, at most a day.
func refreshBefore(maxAge time.Duration) time.Duration {
	return min(maxAge/4, 24*time.Hour)
}

func (f *STSFetcher) lookupID(ctx context.Context, domain string) (string, error) {
	r := f.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	txts, err := r.LookupTXT(ctx, "_mta-sts."+domain)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	id, _ := ParseSTSRecord(txts)
	return id, nil
}

func (f *STSFetcher) httpClient() *http.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client != nil {
		return f.client
	}
	var c http.Client
	if f.HTTP != nil {
		c = *f.HTTP
	} else {
		c = http.Client{Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: f.RootCAs, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        10,
			IdleConnTimeout:     time.Minute,
		}}
	}
	// RFC 8461 section 3.3: senders must not follow redirects.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	f.client = &c
	return f.client
}

// fetch downloads https://mta-sts.<domain>/.well-known/mta-sts.txt. The
// certificate must be valid for mta-sts.<domain>, the answer a 200 with
// text/plain, and the body at most 64 KB.
func (f *STSFetcher) fetch(ctx context.Context, domain string) (*STSPolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, stsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://mta-sts."+domain+"/.well-known/mta-sts.txt", nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("policy host answered %d", resp.StatusCode)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/plain" {
		return nil, fmt.Errorf("policy has media type %q, not text/plain", mt)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, stsMaxPolicySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > stsMaxPolicySize {
		return nil, errors.New("policy is larger than 64 KB")
	}
	return ParseSTSPolicy(body)
}
