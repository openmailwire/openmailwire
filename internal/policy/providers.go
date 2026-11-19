package policy

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Mailbox providers. Many recipient domains share one provider's servers
// (every Google Workspace domain is Gmail as far as rate limits go), and
// providers limit and judge senders per sending IP. So the control plane
// groups recipient domains by the provider their MX hosts belong to, and
// applies concurrency limits, throttling backoff and IP warm-up per provider
// and sending IP. Domains whose MX belongs to no known provider are their own
// group.
//
// The rules ship in providers.json and can be replaced with
// OMW_PROVIDER_RULES.

//go:embed providers.json
var defaultRules []byte

// Rules are the provider rules in force.
type Rules struct {
	Providers []Provider
	byName    map[string]*Provider
}

// Provider is one mailbox provider.
type Provider struct {
	Name       string   // lowercase, no dots: never confused with a domain
	MXSuffixes []string // MX hosts that end in one of these belong to it
	// Concurrency caps in-flight deliveries per sending IP to this provider;
	// 0 means OMW_DOMAIN_CONCURRENCY.
	Concurrency int
	// Warmup is the daily cap per sending IP for a new IP's first days, day
	// 1 first. After the last day the IP is warm. Empty: no warm-up.
	Warmup []int
}

type rulesFile struct {
	DefaultWarmup []int `json:"default_warmup"`
	Providers     []struct {
		Name        string          `json:"name"`
		MXSuffixes  []string        `json:"mx_suffixes"`
		Concurrency int             `json:"concurrency"`
		Warmup      json.RawMessage `json:"warmup"` // "default", a list of daily caps, or absent
	} `json:"providers"`
	Comment string `json:"comment"`
}

var (
	providerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	suffixRE       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// DefaultRules returns the built-in rules.
func DefaultRules() *Rules {
	r, err := ParseRules(defaultRules)
	if err != nil {
		panic("policy: built-in provider rules: " + err.Error())
	}
	return r
}

// LoadRules reads a rules file, or returns the built-in rules for "".
func LoadRules(path string) (*Rules, error) {
	if path == "" {
		return DefaultRules(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("OMW_PROVIDER_RULES: %w", err)
	}
	r, err := ParseRules(b)
	if err != nil {
		return nil, fmt.Errorf("OMW_PROVIDER_RULES %s: %w", path, err)
	}
	return r, nil
}

// ParseRules parses a rules file (see providers.json).
func ParseRules(b []byte) (*Rules, error) {
	var f rulesFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if err := validWarmup(f.DefaultWarmup); err != nil {
		return nil, fmt.Errorf("default_warmup: %w", err)
	}
	r := &Rules{byName: map[string]*Provider{}}
	for _, p := range f.Providers {
		if !providerNameRE.MatchString(p.Name) {
			return nil, fmt.Errorf("provider name %q must be lowercase letters, digits and dashes", p.Name)
		}
		if r.byName[p.Name] != nil {
			return nil, fmt.Errorf("provider %q is listed twice", p.Name)
		}
		if len(p.MXSuffixes) == 0 {
			return nil, fmt.Errorf("provider %q has no mx_suffixes", p.Name)
		}
		if p.Concurrency < 0 {
			return nil, fmt.Errorf("provider %q: concurrency must not be negative", p.Name)
		}
		pr := Provider{Name: p.Name, Concurrency: p.Concurrency}
		for _, s := range p.MXSuffixes {
			s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
			if !suffixRE.MatchString(s) {
				return nil, fmt.Errorf("provider %q: %q is not a host name suffix", p.Name, s)
			}
			pr.MXSuffixes = append(pr.MXSuffixes, s)
		}
		switch w := bytes.TrimSpace(p.Warmup); {
		case len(w) == 0, string(w) == "null", string(w) == "[]":
		case string(w) == `"default"`:
			pr.Warmup = f.DefaultWarmup
		default:
			if err := json.Unmarshal(w, &pr.Warmup); err != nil {
				return nil, fmt.Errorf(`provider %q: warmup must be "default" or a list of daily caps`, p.Name)
			}
			if err := validWarmup(pr.Warmup); err != nil {
				return nil, fmt.Errorf("provider %q warmup: %w", p.Name, err)
			}
		}
		r.Providers = append(r.Providers, pr)
		r.byName[pr.Name] = &r.Providers[len(r.Providers)-1]
	}
	// byName points into Providers; rebuild after appends moved it.
	for i := range r.Providers {
		r.byName[r.Providers[i].Name] = &r.Providers[i]
	}
	return r, nil
}

func validWarmup(w []int) error {
	for i, n := range w {
		if n < 1 {
			return errors.New("daily caps must be positive")
		}
		if i > 0 && n < w[i-1] {
			return errors.New("daily caps must not shrink from one day to the next")
		}
	}
	return nil
}

// ProviderForMX returns the provider whose servers host is, or "" when it
// belongs to none.
func (r *Rules) ProviderForMX(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return ""
	}
	for _, p := range r.Providers {
		for _, s := range p.MXSuffixes {
			if host == s || strings.HasSuffix(host, "."+s) {
				return p.Name
			}
		}
	}
	return ""
}

// Provider returns the named provider, or nil.
func (r *Rules) Provider(name string) *Provider {
	if r == nil {
		return nil
	}
	return r.byName[name]
}

// Concurrency is the per-IP in-flight cap for a throttling scope (a
// provider name, or a recipient domain for domains of no known provider).
func (r *Rules) Concurrency(scope string, def int) int {
	if p := r.Provider(scope); p != nil && p.Concurrency > 0 {
		return p.Concurrency
	}
	return def
}

// WarmupCap returns the day's cap for a sending IP on its day-th day of
// warm-up (day 1 is the first) toward scope. ok is false when no cap
// applies: the provider has no warm-up, or the IP is past it.
func (r *Rules) WarmupCap(scope string, day int) (limit int, ok bool) {
	p := r.Provider(scope)
	if p == nil || len(p.Warmup) == 0 {
		return 0, false
	}
	if day < 1 {
		day = 1
	}
	if day > len(p.Warmup) {
		return 0, false
	}
	return p.Warmup[day-1], true
}

// WarmupDays is the length of the longest warm-up schedule.
func (r *Rules) WarmupDays() int {
	n := 0
	for _, p := range r.Providers {
		n = max(n, len(p.Warmup))
	}
	return n
}
