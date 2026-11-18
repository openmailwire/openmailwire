// Package domains computes the DNS records a sending domain must publish and
// checks them against public DNS.
package domains

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Resolver is the subset of *net.Resolver the checks use.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// NewResolver returns the system resolver, or one that asks server
// ("host:53") directly, bypassing local caches.
func NewResolver(server string) *net.Resolver {
	if server == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, server)
		},
	}
}

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Normalize lowercases and validates a domain name. Internationalized names
// must be passed in punycode (xn--…).
func Normalize(name string) (string, error) {
	n := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if n == "" || len(n) > 253 {
		return "", errors.New("domain name is empty or too long")
	}
	labels := strings.Split(n, ".")
	if len(labels) < 2 {
		return "", errors.New("domain needs at least two labels, like example.com")
	}
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return "", fmt.Errorf("domain label %q is invalid", l)
		}
	}
	return n, nil
}

const (
	StatusPending  = "pending"
	StatusVerified = "verified"
	StatusFailed   = "failed"
	StatusMissing  = "missing"
)

type Record struct {
	Record   string `json:"record"` // DKIM, SPF, MX, DMARC or INBOUND
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Required bool   `json:"required"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

type Spec struct {
	Domain        string
	Selector      string
	DKIMPublic    string
	ReturnPath    string   // envelope sender domain, e.g. bounce.example.com
	BounceMX      string   // host that receives bounces; empty = not configured
	InboundMX     string   // host that receives mail for inboxes; empty = not configured
	SPFMechanisms []string // e.g. include:_spf.openmailwire.example or ip4:203.0.113.7
}

func (s Spec) DKIMName() string { return s.Selector + "._domainkey." + s.Domain }

func SPFValue(mechanisms []string) string {
	parts := append([]string{"v=spf1"}, mechanisms...)
	return strings.Join(append(parts, "-all"), " ")
}

// Records lists what the domain owner must publish, all marked pending.
func Records(s Spec) []Record {
	recs := []Record{
		{Record: "DKIM", Type: "TXT", Name: s.DKIMName(), Value: "v=DKIM1; k=rsa; p=" + s.DKIMPublic, Required: true, Status: StatusPending},
		{Record: "SPF", Type: "TXT", Name: s.ReturnPath, Value: SPFValue(s.SPFMechanisms), Required: true, Status: StatusPending},
	}
	if s.BounceMX != "" {
		recs = append(recs, Record{Record: "MX", Type: "MX", Name: s.ReturnPath, Value: s.BounceMX, Priority: 10, Required: true, Status: StatusPending})
	}
	recs = append(recs, Record{Record: "DMARC", Type: "TXT", Name: "_dmarc." + s.Domain, Value: "v=DMARC1; p=none;", Required: false, Status: StatusPending})
	if s.InboundMX != "" {
		// Only for inboxes: sending never depends on it.
		recs = append(recs, Record{Record: "INBOUND", Type: "MX", Name: s.Domain, Value: s.InboundMX, Priority: 10, Required: false, Status: StatusPending})
	}
	return recs
}

// Check looks every record up. verified is true when all required records
// are in place.
func Check(ctx context.Context, r Resolver, s Spec) (recs []Record, verified bool) {
	recs = Records(s)
	verified = true
	for i := range recs {
		rec := &recs[i]
		switch rec.Record {
		case "DKIM":
			rec.Status, rec.Detail = checkDKIM(ctx, r, rec.Name, s.DKIMPublic)
		case "SPF":
			rec.Status, rec.Detail = checkSPF(ctx, r, rec.Name, s.SPFMechanisms)
		case "MX":
			rec.Status, rec.Detail = checkMX(ctx, r, rec.Name, s.BounceMX)
		case "DMARC":
			rec.Status, rec.Detail = checkDMARC(ctx, r, rec.Name)
		case "INBOUND":
			rec.Status, rec.Detail = checkMX(ctx, r, rec.Name, s.InboundMX)
			if rec.Status != StatusVerified {
				rec.Detail += " (needed only to receive mail in inboxes on this domain)"
			}
		}
		if rec.Required && rec.Status != StatusVerified {
			verified = false
		}
	}
	return recs, verified
}

func lookupFailure(err error) (string, string) {
	var de *net.DNSError
	if errors.As(err, &de) && de.IsNotFound {
		return StatusMissing, "no record found"
	}
	return StatusPending, "lookup failed: " + err.Error()
}

func checkDKIM(ctx context.Context, r Resolver, name, pub string) (string, string) {
	txts, err := r.LookupTXT(ctx, name)
	if err != nil {
		return lookupFailure(err)
	}
	if len(txts) == 0 {
		return StatusMissing, "no record found"
	}
	for _, t := range txts {
		if dkimPublicKey(t) == pub {
			return StatusVerified, ""
		}
	}
	return StatusFailed, "a TXT record exists but its p= key does not match"
}

func dkimPublicKey(txt string) string {
	for _, part := range strings.Split(txt, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == "p" {
			return strings.Join(strings.Fields(v), "")
		}
	}
	return ""
}

func checkSPF(ctx context.Context, r Resolver, name string, mechanisms []string) (string, string) {
	txts, err := r.LookupTXT(ctx, name)
	if err != nil {
		return lookupFailure(err)
	}
	var spf []string
	for _, t := range txts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=spf1") {
			spf = append(spf, t)
		}
	}
	switch len(spf) {
	case 0:
		return StatusMissing, "no SPF record found"
	case 1:
	default:
		return StatusFailed, "more than one SPF record; receivers treat that as an error"
	}
	have := map[string]bool{}
	for _, f := range strings.Fields(strings.ToLower(spf[0])) {
		have[strings.TrimPrefix(f, "+")] = true
	}
	var missing []string
	for _, m := range mechanisms {
		if !have[strings.ToLower(m)] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return StatusFailed, "SPF record does not authorize " + strings.Join(missing, ", ")
	}
	return StatusVerified, ""
}

func checkMX(ctx context.Context, r Resolver, name, want string) (string, string) {
	mxs, err := r.LookupMX(ctx, name)
	if err != nil {
		return lookupFailure(err)
	}
	if len(mxs) == 0 {
		return StatusMissing, "no MX record found"
	}
	for _, mx := range mxs {
		if strings.EqualFold(strings.TrimSuffix(mx.Host, "."), strings.TrimSuffix(want, ".")) {
			return StatusVerified, ""
		}
	}
	return StatusFailed, "the MX record points to a different host"
}

func checkDMARC(ctx context.Context, r Resolver, name string) (string, string) {
	txts, err := r.LookupTXT(ctx, name)
	if err != nil {
		st, _ := lookupFailure(err)
		return st, "no DMARC record found (recommended, not required)"
	}
	for _, t := range txts {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(t)), "V=DMARC1") {
			return StatusVerified, ""
		}
	}
	return StatusMissing, "no DMARC record found (recommended, not required)"
}
