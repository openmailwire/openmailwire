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

// What each record is for, and the mailbox providers' rules behind it:
// shown in a record's detail until it checks out. Gmail and Yahoo (since
// February 2024) and Microsoft's Outlook.com (since May 2025) require from
// anyone sending them 5,000 or more messages a day: SPF and DKIM, a DMARC
// record (p=none is enough) aligned with the From domain, one-click
// unsubscribe on marketing mail, and a spam rate under 0.3%. Smaller
// senders need SPF or DKIM, and fare better with all three.
const (
	guideDKIM = "DKIM signs every message for this domain. Gmail, Yahoo and Microsoft (Outlook.com) require it from bulk senders " +
		"(5,000+ messages a day to them) and use it to align with your DMARC record."
	guideSPF = "SPF authorizes OpenMailWire's sending IPs for the return path (envelope sender). Gmail, Yahoo and Microsoft require SPF " +
		"or DKIM from every sender, and both from bulk senders."
	guideMX = "The return path needs an MX record so that receivers can send bounces back; many receivers refuse mail whose " +
		"envelope sender domain has no MX (or A) record."
	guideMXUnset = "This server has no bounce host (OMW_BOUNCE_MX or OMW_INBOUND_MX), so this record is optional: point the return " +
		"path at any mail server you run so that receivers accept it, or ask your operator to configure a bounce host."
	guideDMARC = "DMARC tells receivers what to do with mail that fails SPF and DKIM. Gmail and Yahoo (since 2024) and Microsoft " +
		"(Outlook.com, since May 2025) require a DMARC record, at least p=none, from bulk senders, and treat mail without one " +
		"as more likely spam. Start with p=none; add rua=mailto:… to receive reports."
)

// Records lists what the domain owner must publish, all marked pending,
// each with what it is for in its detail.
func Records(s Spec) []Record {
	recs := []Record{
		{Record: "DKIM", Type: "TXT", Name: s.DKIMName(), Value: "v=DKIM1; k=rsa; p=" + s.DKIMPublic, Required: true,
			Status: StatusPending, Detail: guideDKIM},
		{Record: "SPF", Type: "TXT", Name: s.ReturnPath, Value: SPFValue(s.SPFMechanisms), Required: true,
			Status: StatusPending, Detail: guideSPF},
	}
	// The return path's MX is always shown. It is required when this server
	// receives bounces: at OMW_BOUNCE_MX, or else at the inbound host.
	if bounce := s.bounceHost(); bounce != "" {
		recs = append(recs, Record{Record: "MX", Type: "MX", Name: s.ReturnPath, Value: bounce, Priority: 10, Required: true,
			Status: StatusPending, Detail: guideMX})
	} else {
		recs = append(recs, Record{Record: "MX", Type: "MX", Name: s.ReturnPath, Priority: 10, Required: false,
			Status: StatusPending, Detail: guideMXUnset})
	}
	recs = append(recs, Record{Record: "DMARC", Type: "TXT", Name: "_dmarc." + s.Domain, Value: "v=DMARC1; p=none;", Required: true,
		Status: StatusPending, Detail: guideDMARC})
	if s.InboundMX != "" {
		// Only for inboxes: sending never depends on it.
		recs = append(recs, Record{Record: "INBOUND", Type: "MX", Name: s.Domain, Value: s.InboundMX, Priority: 10, Required: false,
			Status: StatusPending, Detail: "needed only to receive mail in inboxes on this domain"})
	}
	return recs
}

// bounceHost is where bounces for the return path go, or "" when this
// server receives none.
func (s Spec) bounceHost() string {
	if s.BounceMX != "" {
		return s.BounceMX
	}
	return s.InboundMX
}

// Check looks every record up. verified is true when all required records
// are in place. A record that is not in place keeps its guidance after the
// reason.
func Check(ctx context.Context, r Resolver, s Spec) (recs []Record, verified bool) {
	recs = Records(s)
	verified = true
	for i := range recs {
		rec := &recs[i]
		guide := rec.Detail
		switch rec.Record {
		case "DKIM":
			rec.Status, rec.Detail = checkDKIM(ctx, r, rec.Name, s.DKIMPublic)
		case "SPF":
			rec.Status, rec.Detail = checkSPF(ctx, r, rec.Name, s.SPFMechanisms)
		case "MX":
			if rec.Value == "" {
				rec.Status, rec.Detail = checkAnyMX(ctx, r, rec.Name)
			} else {
				rec.Status, rec.Detail = checkMX(ctx, r, rec.Name, rec.Value)
			}
		case "DMARC":
			rec.Status, rec.Detail = checkDMARC(ctx, r, s.Domain)
		case "INBOUND":
			rec.Status, rec.Detail = checkMX(ctx, r, rec.Name, s.InboundMX)
		}
		if rec.Status != StatusVerified {
			rec.Detail += ". " + guide
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

// checkAnyMX checks that the return path has some MX record, when this
// server has no bounce host of its own to point it at.
func checkAnyMX(ctx context.Context, r Resolver, name string) (string, string) {
	mxs, err := r.LookupMX(ctx, name)
	if err != nil {
		return lookupFailure(err)
	}
	if len(mxs) == 0 {
		return StatusMissing, "no MX record found"
	}
	return StatusVerified, "MX " + strings.TrimSuffix(mxs[0].Host, ".")
}

// checkDMARC looks for the domain's DMARC record at _dmarc.<domain>, then,
// as receivers do for a subdomain without one, at its parent domains. It
// needs exactly one record, with a valid p= (none, quarantine or reject).
func checkDMARC(ctx context.Context, r Resolver, domain string) (string, string) {
	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels)-1 && i < 6; i++ {
		name := "_dmarc." + strings.Join(labels[i:], ".")
		txts, err := r.LookupTXT(ctx, name)
		if err != nil {
			var de *net.DNSError
			if errors.As(err, &de) && de.IsNotFound {
				continue
			}
			return StatusPending, "lookup failed: " + err.Error()
		}
		var dmarc []string
		for _, t := range txts {
			if v, _, _ := strings.Cut(strings.TrimSpace(t), ";"); strings.EqualFold(strings.ReplaceAll(v, " ", ""), "v=DMARC1") {
				dmarc = append(dmarc, t)
			}
		}
		switch len(dmarc) {
		case 0:
			continue
		case 1:
		default:
			return StatusFailed, "more than one DMARC record at " + name + "; receivers ignore them all, so keep one"
		}
		tags := map[string]string{}
		for _, part := range strings.Split(dmarc[0], ";") {
			if k, v, ok := strings.Cut(part, "="); ok {
				tags[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
			}
		}
		policy := tags["p"]
		if i > 0 && tags["sp"] != "" {
			policy = tags["sp"] // the policy for subdomains
		}
		switch policy {
		case "none", "quarantine", "reject":
		default:
			return StatusFailed, "the DMARC record at " + name + " has no valid policy; it needs p=none, p=quarantine or p=reject"
		}
		detail := ""
		if i > 0 {
			detail = "inherited from " + name + "; "
		}
		if policy == "none" {
			detail += "p=none only monitors: once DMARC reports show that all your mail passes, move to p=quarantine, then p=reject"
		}
		return StatusVerified, strings.TrimSuffix(detail, "; ")
	}
	return StatusMissing, "no DMARC record found at _dmarc." + domain
}
