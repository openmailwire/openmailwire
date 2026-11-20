package dnsprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultCloudflareBase is Cloudflare's API v4. OMW_CLOUDFLARE_API_BASE
// replaces it (tests point it at a local fake).
const DefaultCloudflareBase = "https://api.cloudflare.com/client/v4"

// CloudflareName is the provider id in requests and responses.
const CloudflareName = "cloudflare"

// RecordComment is set on every record OpenMailWire creates, so the zone's
// owner can tell them apart in Cloudflare's dashboard.
const RecordComment = "Managed by OpenMailWire"

// Cloudflare talks to Cloudflare's API v4 with one API token. The token
// needs Zone > DNS > Edit on the zone (Zone > Zone > Read comes with it);
// Zone > Email Routing > Read is optional.
type Cloudflare struct {
	base  string
	token string
	http  *http.Client
	// Retries is how often a request is tried again: after a 429 (any
	// request: Cloudflare did not process it), and after a 5xx or a network
	// error (only requests that are safe to repeat; never a create).
	Retries int
	// MaxWait caps one wait for a 429's Retry-After. When Cloudflare asks
	// for longer, the request gives up with KindUnavailable.
	MaxWait time.Duration
	// sleep waits between attempts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

// Timeouts for every Cloudflare request.
const (
	cfRequestTimeout = 20 * time.Second
	cfDialTimeout    = 10 * time.Second
)

// NewCloudflare returns a client for the API at base (DefaultCloudflareBase
// when empty) that acts with token.
func NewCloudflare(base, token string) *Cloudflare {
	if base == "" {
		base = DefaultCloudflareBase
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   cfDialTimeout,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
	return &Cloudflare{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http: &http.Client{
			Timeout:   cfRequestTimeout,
			Transport: tr,
			// The token goes to the API base only, never to wherever a
			// redirect points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Retries: 3,
		MaxWait: 30 * time.Second,
		sleep:   sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SetSleep replaces how the client waits between attempts (tests).
func (c *Cloudflare) SetSleep(f func(ctx context.Context, d time.Duration) error) { c.sleep = f }

// The client never prints its token, whatever logs or formats it.
func (c *Cloudflare) String() string               { return "cloudflare(" + c.base + ")" }
func (c *Cloudflare) GoString() string             { return c.String() }
func (c *Cloudflare) LogValue() slog.Value         { return slog.StringValue(c.String()) }
func (c *Cloudflare) Name() string                 { return CloudflareName }
func (c *Cloudflare) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

type cfMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []cfMessage     `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

// Cloudflare error codes that mean the token is not usable: invalid or
// malformed token, authentication error, missing permission.
var cfAuthCodes = map[int]bool{6003: true, 6111: true, 9103: true, 9106: true, 9109: true, 10000: true, 10001: true}

// Cloudflare's codes for "this exact record already exists".
var cfDuplicateCodes = map[int]bool{81057: true, 81058: true}

// IsDuplicate reports whether err is Cloudflare refusing to create a record
// that already exists with the same content.
func IsDuplicate(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	for _, c := range e.Codes {
		if cfDuplicateCodes[c] {
			return true
		}
	}
	return false
}

// do sends one API request and decodes its result into out. It retries as
// Retries and MaxWait say, and turns every failure into an *Error.
func (c *Cloudflare) do(ctx context.Context, op, method, path string, q url.Values, body any, out any) (*cfEnvelope, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, &Error{Kind: KindRejected, Op: op, Msg: "cannot encode the request"}
		}
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	// A create that failed with a 5xx may have happened; repeating it could
	// make a second record. Everything else is safe to repeat.
	repeatable := method != http.MethodPost
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, &Error{Kind: KindRejected, Op: op, Msg: "invalid request"}
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "openmailwire")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &Error{Kind: KindUnavailable, Op: op, Msg: "no answer in time"}
			}
			if repeatable && attempt < c.Retries {
				if c.sleep(ctx, backoff) != nil {
					return nil, &Error{Kind: KindUnavailable, Op: op, Msg: "no answer in time"}
				}
				backoff *= 2
				continue
			}
			return nil, &Error{Kind: KindUnavailable, Op: op, Msg: c.clean(netErrText(err))}
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		var env cfEnvelope
		decodeErr := json.Unmarshal(data, &env)
		status := resp.StatusCode
		e := &Error{Status: status, Op: op, Msg: c.messages(env.Errors, status)}
		for _, m := range env.Errors {
			e.Codes = append(e.Codes, m.Code)
		}
		switch {
		case status == http.StatusTooManyRequests:
			wait := retryAfter(resp.Header.Get("Retry-After"), backoff)
			e.Kind, e.RetryAfter = KindUnavailable, wait
			e.Msg = "Cloudflare is rate-limiting this token (HTTP 429)"
			if attempt >= c.Retries || wait > c.MaxWait {
				return nil, e
			}
			if c.sleep(ctx, wait) != nil {
				return nil, e
			}
			backoff *= 2
			continue
		case status >= 500, rerr != nil:
			if repeatable && attempt < c.Retries {
				if c.sleep(ctx, backoff) != nil {
					e.Kind = KindUnavailable
					return nil, e
				}
				backoff *= 2
				continue
			}
			e.Kind = KindUnavailable
			return nil, e
		case status == http.StatusUnauthorized, status == http.StatusForbidden, hasCode(env.Errors, cfAuthCodes):
			e.Kind = KindAuth
			return nil, e
		case status == http.StatusNotFound:
			e.Kind = KindNotFound
			return nil, e
		case status >= 300 || decodeErr != nil || !env.Success:
			e.Kind = KindRejected
			if decodeErr != nil && status < 300 {
				e.Kind, e.Msg = KindUnavailable, "unexpected answer from the API"
			}
			return nil, e
		}
		if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return nil, &Error{Kind: KindUnavailable, Status: status, Op: op, Msg: "unexpected answer from the API"}
			}
		}
		return &env, nil
	}
}

func hasCode(msgs []cfMessage, codes map[int]bool) bool {
	for _, m := range msgs {
		if codes[m.Code] {
			return true
		}
	}
	return false
}

// retryAfter reads a Retry-After header (seconds or an HTTP date), or
// returns def.
func retryAfter(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return def
}

func netErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "the API did not answer in time"
	}
	return "cannot reach the API: " + err.Error()
}

// messages joins what Cloudflare said, or the HTTP status text.
func (c *Cloudflare) messages(msgs []cfMessage, status int) string {
	var parts []string
	for _, m := range msgs {
		if len(parts) == 3 {
			break
		}
		if m.Code != 0 {
			parts = append(parts, c.clean(m.Message)+" (code "+strconv.Itoa(m.Code)+")")
		} else {
			parts = append(parts, c.clean(m.Message))
		}
	}
	if len(parts) == 0 {
		return http.StatusText(status)
	}
	return strings.Join(parts, "; ")
}

// clean makes provider text safe to show: printable, short, and never the
// token (Cloudflare does not echo it, but a proxy in between might).
func (c *Cloudflare) clean(s string) string {
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[token]")
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// VerifyToken checks the token with GET /user/tokens/verify.
func (c *Cloudflare) VerifyToken(ctx context.Context) error {
	const op = "verify the API token"
	var r struct {
		Status string `json:"status"`
	}
	if _, err := c.do(ctx, op, http.MethodGet, "/user/tokens/verify", nil, nil, &r); err != nil {
		return err
	}
	if r.Status != "active" {
		return &Error{Kind: KindAuth, Op: op, Msg: "the token's status is " + strconv.Quote(r.Status) + ", not active"}
	}
	return nil
}

type cfZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (z cfZone) zone() Zone { return Zone{ID: z.ID, Name: canon(z.Name), Status: z.Status} }

var cfIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidCloudflareID reports whether id looks like a Cloudflare zone or
// record id (32 lowercase hex digits).
func ValidCloudflareID(id string) bool { return cfIDRE.MatchString(id) }

// FindZone looks the domain up, then each parent name, until a zone the
// token can see has that name. It asks for at most 10 names.
func (c *Cloudflare) FindZone(ctx context.Context, domain string) (Zone, error) {
	d := canon(domain)
	labels := strings.Split(d, ".")
	for i := 0; i+1 < len(labels) && i < 10; i++ {
		name := strings.Join(labels[i:], ".")
		var zs []cfZone
		if _, err := c.do(ctx, "find the zone "+name, http.MethodGet, "/zones",
			url.Values{"name": {name}, "per_page": {"5"}}, nil, &zs); err != nil {
			return Zone{}, err
		}
		for _, z := range zs {
			if canon(z.Name) == name {
				return z.zone(), nil
			}
		}
	}
	return Zone{}, &Error{Kind: KindZoneNotFound, Op: "find the zone of " + d,
		Msg: "no zone this token can see holds " + d}
}

// Zone returns the zone with this id.
func (c *Cloudflare) Zone(ctx context.Context, id string) (Zone, error) {
	op := "read the zone " + id
	if !ValidCloudflareID(id) {
		return Zone{}, &Error{Kind: KindZoneNotFound, Op: op, Msg: "a zone id is 32 hexadecimal digits"}
	}
	var z cfZone
	if _, err := c.do(ctx, op, http.MethodGet, "/zones/"+id, nil, nil, &z); err != nil {
		if KindOf(err) == KindNotFound {
			err.(*Error).Kind = KindZoneNotFound
		}
		return Zone{}, err
	}
	return z.zone(), nil
}

type cfRecord struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Priority *int   `json:"priority"`
	Proxied  bool   `json:"proxied"`
}

func (r cfRecord) record() Record {
	out := Record{ID: r.ID, Type: strings.ToUpper(r.Type), Name: canon(r.Name), Content: r.Content, Proxied: r.Proxied}
	switch out.Type {
	case "TXT":
		out.Content = NormalizeTXT(r.Content)
	case "MX", "CNAME":
		out.Content = canon(r.Content)
	}
	if r.Priority != nil {
		out.Priority = *r.Priority
	}
	return out
}

// Records lists the records at name, at most 500 of them.
func (c *Cloudflare) Records(ctx context.Context, zoneID, name string) ([]Record, error) {
	name = canon(name)
	var out []Record
	for page := 1; page <= 5; page++ {
		var rs []cfRecord
		env, err := c.do(ctx, "list the records at "+name, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dns_records",
			url.Values{"name": {name}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}, nil, &rs)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if canon(r.Name) == name {
				out = append(out, r.record())
			}
		}
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages {
			break
		}
	}
	return out, nil
}

// recordBody is the JSON Cloudflare takes for a record. OpenMailWire's
// records are never proxied: Cloudflare's proxy carries only HTTP.
func recordBody(r Record) map[string]any {
	b := map[string]any{"type": r.Type, "name": canon(r.Name), "ttl": 1, "proxied": false, "comment": RecordComment}
	switch r.Type {
	case "TXT":
		b["content"] = QuoteTXT(NormalizeTXT(r.Content))
	case "MX":
		b["content"] = canon(r.Content)
		b["priority"] = r.Priority
	default:
		b["content"] = r.Content
	}
	return b
}

// Create adds a record.
func (c *Cloudflare) Create(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out cfRecord
	if _, err := c.do(ctx, "create the "+r.Type+" record at "+canon(r.Name), http.MethodPost,
		"/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, recordBody(r), &out); err != nil {
		return Record{}, err
	}
	return out.record(), nil
}

// Update replaces the content of record r.ID.
func (c *Cloudflare) Update(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out cfRecord
	if _, err := c.do(ctx, "update the "+r.Type+" record at "+canon(r.Name), http.MethodPatch,
		"/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(r.ID), nil, recordBody(r), &out); err != nil {
		return Record{}, err
	}
	return out.record(), nil
}

// Delete removes a record. A record that is already gone is not an error.
func (c *Cloudflare) Delete(ctx context.Context, zoneID, recordID string) error {
	_, err := c.do(ctx, "delete a record", http.MethodDelete,
		"/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil, nil, nil)
	if KindOf(err) == KindNotFound {
		return nil
	}
	return err
}

// EmailRouting reads GET /zones/{id}/email/routing. A token without
// Zone > Email Routing > Read gets 403 there: StatusUnknown, not an error.
func (c *Cloudflare) EmailRouting(ctx context.Context, zoneID string) string {
	var r struct {
		Enabled bool `json:"enabled"`
	}
	if _, err := c.do(ctx, "read Email Routing", http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/email/routing", nil, nil, &r); err != nil {
		return StatusUnknown
	}
	if r.Enabled {
		return StatusEnabled
	}
	return StatusDisabled
}

// DNSSEC reads GET /zones/{id}/dnssec.
func (c *Cloudflare) DNSSEC(ctx context.Context, zoneID string) string {
	var r struct {
		Status string `json:"status"`
	}
	if _, err := c.do(ctx, "read DNSSEC", http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dnssec", nil, nil, &r); err != nil || r.Status == "" {
		return StatusUnknown
	}
	return r.Status
}
