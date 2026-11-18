// Package apikey holds what an API key may do: its permission and its
// sending policy (send caps, and sender and recipient allowlists).
package apikey

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Permissions.
const (
	// FullAccess can call every endpoint.
	FullAccess = "full_access"
	// SendingAccess can send, read and cancel its own emails, and read
	// domains. It suits an agent that should not manage the project.
	SendingAccess = "sending_access"
)

// ValidPermission reports whether p is a known permission.
func ValidPermission(p string) bool { return p == FullAccess || p == SendingAccess }

const (
	maxPatterns   = 500
	maxPatternLen = 254
	maxCap        = 10_000_000
)

// Policy limits what a key may send. Every field is optional; an absent
// field means no limit.
type Policy struct {
	// MaxPerHour and MaxPerDay cap the recipients the key queues in a
	// rolling hour and a rolling 24 hours. Suppressed and canceled
	// recipients do not count.
	MaxPerHour *int `json:"max_per_hour,omitempty"`
	MaxPerDay  *int `json:"max_per_day,omitempty"`
	// AllowedRecipients and AllowedFrom are address patterns, matched
	// against lowercase addresses. "*" matches any characters within the
	// local part or within the domain part: "*@acme.com", "ops@*.acme.com".
	AllowedRecipients []string `json:"allowed_recipients,omitempty"`
	AllowedFrom       []string `json:"allowed_from,omitempty"`
}

// Parse decodes a policy, refusing unknown fields, and normalizes it.
func Parse(b []byte) (Policy, error) {
	var p Policy
	if len(bytes.TrimSpace(b)) == 0 {
		return p, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: %w", err)
	}
	return p, p.Normalize()
}

// Normalize validates p and lowercases and deduplicates its patterns. The
// errors name the field and what to do.
func (p *Policy) Normalize() error {
	for _, c := range []struct {
		name string
		v    *int
	}{{"max_per_hour", p.MaxPerHour}, {"max_per_day", p.MaxPerDay}} {
		if c.v != nil && (*c.v < 1 || *c.v > maxCap) {
			return fmt.Errorf("policy.%s must be between 1 and %d; leave it out for no limit, or revoke the key to stop it sending", c.name, maxCap)
		}
	}
	var err error
	if p.AllowedRecipients, err = normalizePatterns("allowed_recipients", p.AllowedRecipients); err != nil {
		return err
	}
	if p.AllowedFrom, err = normalizePatterns("allowed_from", p.AllowedFrom); err != nil {
		return err
	}
	return nil
}

func normalizePatterns(field string, in []string) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("policy.%s is empty; leave it out to allow every address", field)
	}
	if len(in) > maxPatterns {
		return nil, fmt.Errorf("policy.%s has more than %d patterns", field, maxPatterns)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		pat := strings.ToLower(strings.TrimSpace(raw))
		if err := ValidatePattern(pat); err != nil {
			return nil, fmt.Errorf("policy.%s: %w", field, err)
		}
		if !seen[pat] {
			seen[pat] = true
			out = append(out, pat)
		}
	}
	return out, nil
}

var patternRE = regexp.MustCompile("^[a-z0-9!#$%&'*+/=?^_`{|}~.-]+@[a-z0-9*.-]+$")

// ValidatePattern checks one lowercase address pattern.
func ValidatePattern(p string) error {
	if len(p) > maxPatternLen {
		return fmt.Errorf("pattern %q is longer than %d characters", p, maxPatternLen)
	}
	if !patternRE.MatchString(p) {
		return fmt.Errorf("pattern %q is not an address pattern; write it like agent@acme.com, *@acme.com or *@*.acme.com", p)
	}
	return nil
}

// Match reports whether the lowercase address addr matches pattern. "*"
// matches any run of characters, but never across the "@".
func Match(pattern, addr string) bool {
	pl, pd, ok := strings.Cut(pattern, "@")
	if !ok {
		return false
	}
	al, ad, ok := strings.Cut(addr, "@")
	if !ok || strings.Contains(ad, "@") {
		return false
	}
	return glob(pl, al) && glob(pd, ad)
}

// glob matches s against p, where '*' matches any (possibly empty) run of
// bytes. It runs in O(len(p)·len(s)) at worst, with no recursion.
func glob(p, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

func matchAny(patterns []string, addr string) bool {
	addr = strings.ToLower(addr)
	for _, p := range patterns {
		if Match(p, addr) {
			return true
		}
	}
	return false
}

// AllowsSender reports whether the policy lets the key send from addr.
func (p Policy) AllowsSender(addr string) bool {
	return p.AllowedFrom == nil || matchAny(p.AllowedFrom, addr)
}

// FirstDisallowedRecipient returns the first recipient the policy does not
// allow, or ok = false when every recipient is allowed.
func (p Policy) FirstDisallowedRecipient(rcpts []string) (rcpt string, ok bool) {
	if p.AllowedRecipients == nil {
		return "", false
	}
	for _, r := range rcpts {
		if !matchAny(p.AllowedRecipients, r) {
			return r, true
		}
	}
	return "", false
}

// HasCaps reports whether the policy limits how much the key sends.
func (p Policy) HasCaps() bool { return p.MaxPerHour != nil || p.MaxPerDay != nil }

// Describe lists patterns for an error message, shortened when long.
func Describe(patterns []string) string {
	const show = 10
	if len(patterns) <= show {
		return strings.Join(patterns, ", ")
	}
	return strings.Join(patterns[:show], ", ") + fmt.Sprintf(" and %d more", len(patterns)-show)
}
