// Package dnsprovider publishes DNS records through a DNS host's API, so
// that a domain's records can be set up without copying them by hand. It
// has one implementation, Cloudflare (API v4, over plain net/http).
//
// A provider holds an API token. It never logs it, never puts it in an
// error, and sends it only to its configured API base URL, as a bearer
// token, without following redirects.
package dnsprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Record is one DNS record at a provider.
type Record struct {
	ID   string
	Type string // TXT, MX, CNAME, A, AAAA, …
	// Name is the full name, lower case, without the trailing dot.
	Name string
	// Content is the record's data: for TXT the text itself (quotes and
	// the split into 255-byte strings removed, see NormalizeTXT), for MX
	// the host.
	Content  string
	Priority int  // MX only
	Proxied  bool // served through the provider's HTTP proxy (Cloudflare's orange cloud)
}

// Zone is a DNS zone at a provider.
type Zone struct {
	ID     string
	Name   string
	Status string // the provider's word for it; Cloudflare: active, pending, …
}

// Contains reports whether name is the zone's apex or below it.
func (z Zone) Contains(name string) bool {
	name, zn := canon(name), canon(z.Name)
	return zn != "" && (name == zn || strings.HasSuffix(name, "."+zn))
}

// Statuses of a zone setting the provider may or may not let us read.
const (
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
	StatusUnknown  = "unknown" // the token may not read it, or the provider did not answer
)

// Provider is a DNS host's API, acting with one API token.
type Provider interface {
	// Name is the provider's id, e.g. "cloudflare".
	Name() string
	// VerifyToken checks that the token is valid and active.
	VerifyToken(ctx context.Context) error
	// FindZone finds the zone that holds domain: the domain itself, or the
	// closest parent the token can see.
	FindZone(ctx context.Context, domain string) (Zone, error)
	// Zone returns the zone with this id.
	Zone(ctx context.Context, id string) (Zone, error)
	// Records lists every record at name (all types).
	Records(ctx context.Context, zoneID, name string) ([]Record, error)
	Create(ctx context.Context, zoneID string, r Record) (Record, error)
	// Update replaces the content (and MX priority) of the record r.ID.
	Update(ctx context.Context, zoneID string, r Record) (Record, error)
	Delete(ctx context.Context, zoneID, recordID string) error
	// EmailRouting says whether the provider's own mail forwarding owns the
	// zone's MX records (Cloudflare Email Routing). StatusUnknown when the
	// token may not read it.
	EmailRouting(ctx context.Context, zoneID string) string
	// DNSSEC returns the zone's DNSSEC status ("active", "disabled",
	// "pending", …), or StatusUnknown.
	DNSSEC(ctx context.Context, zoneID string) string
}

// Kind classifies a provider error.
type Kind int

const (
	// KindAuth: the token is invalid, expired, or lacks a permission.
	KindAuth Kind = iota + 1
	// KindZoneNotFound: no zone the token can see holds the domain.
	KindZoneNotFound
	// KindUnavailable: the provider could not be reached, failed (5xx) or
	// kept rate-limiting us. Trying later may work.
	KindUnavailable
	// KindRejected: the provider refused this one request (another 4xx),
	// e.g. a record that conflicts with one already there.
	KindRejected
	// KindNotFound: the record (or other object) does not exist.
	KindNotFound
)

func (k Kind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindZoneNotFound:
		return "zone not found"
	case KindUnavailable:
		return "unavailable"
	case KindRejected:
		return "rejected"
	case KindNotFound:
		return "not found"
	}
	return "unknown"
}

// Error is a provider error. Its message never contains the token.
type Error struct {
	Kind   Kind
	Status int    // HTTP status; 0 when no answer came
	Op     string // what was being done, e.g. "list records at bounce.example.com"
	// Msg is what the provider said (or what went wrong), cleaned and cut
	// to a reasonable length.
	Msg string
	// Codes are the provider's own error codes, when it gave any.
	Codes []int
	// RetryAfter is how long the provider asked us to wait (429).
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("dns provider")
	if e.Op != "" {
		b.WriteString(": " + e.Op)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	if e.Msg != "" {
		b.WriteString(": " + e.Msg)
	}
	return b.String()
}

// Is matches the sentinel errors below by kind: errors.Is(err, ErrAuth).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Op == "" && t.Status == 0 && t.Msg == "" && t.Kind == e.Kind
}

// Sentinels for errors.Is.
var (
	ErrAuth         = &Error{Kind: KindAuth}
	ErrZoneNotFound = &Error{Kind: KindZoneNotFound}
	ErrUnavailable  = &Error{Kind: KindUnavailable}
	ErrRejected     = &Error{Kind: KindRejected}
	ErrNotFound     = &Error{Kind: KindNotFound}
)

// KindOf returns err's kind, or 0 when it is not a provider error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// canon lowercases a DNS name and removes the trailing dot.
func canon(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// Canon is the form names are compared in: lower case, no trailing dot.
func Canon(name string) string { return canon(name) }

// NormalizeTXT returns a TXT record's text as resolvers see it: when
// content is a list of quoted strings ("v=DKIM1; …" "…"), the strings are
// unquoted and joined; otherwise content is returned trimmed.
func NormalizeTXT(content string) string {
	t := strings.TrimSpace(content)
	if !strings.HasPrefix(t, `"`) {
		return t
	}
	var b strings.Builder
	for {
		t = strings.TrimLeft(t, " \t")
		if t == "" {
			return b.String()
		}
		if t[0] != '"' {
			return strings.TrimSpace(content)
		}
		i, closed := 1, false
		for i < len(t) {
			c := t[i]
			if c == '\\' && i+1 < len(t) {
				b.WriteByte(t[i+1])
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				break
			}
			b.WriteByte(c)
			i++
		}
		if !closed {
			return strings.TrimSpace(content)
		}
		t = t[i+1:]
	}
}

// QuoteTXT formats text as TXT record content: quoted strings of at most
// 255 bytes each, the form RFC 1035 zone files and Cloudflare's API use.
func QuoteTXT(text string) string {
	var parts []string
	for len(text) > 0 || len(parts) == 0 {
		n := min(255, len(text))
		chunk := text[:n]
		text = text[n:]
		chunk = strings.ReplaceAll(chunk, `\`, `\\`)
		chunk = strings.ReplaceAll(chunk, `"`, `\"`)
		parts = append(parts, `"`+chunk+`"`)
	}
	return strings.Join(parts, " ")
}

// SameTXT reports whether two TXT texts are the same record for a
// receiver: equal after collapsing runs of spaces, ignoring case.
func SameTXT(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(NormalizeTXT(a)), " "), strings.Join(strings.Fields(NormalizeTXT(b)), " "))
}

// HasTag reports whether a TXT text starts with a version tag such as
// "v=spf1", "v=DKIM1", "v=DMARC1" or "v=TLSRPTv1", ignoring case and the
// spaces around "=". "v=spf1" matches "v=spf1 -all" but not "v=spf10".
func HasTag(text, tag string) bool {
	k, rest, ok := strings.Cut(strings.TrimSpace(NormalizeTXT(text)), "=")
	tk, tv, _ := strings.Cut(tag, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(k), tk) {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	if i := strings.IndexAny(rest, "; \t"); i >= 0 {
		rest = rest[:i]
	}
	return strings.EqualFold(rest, tv)
}
