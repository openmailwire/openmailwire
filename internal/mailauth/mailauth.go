// Package mailauth authenticates received mail: SPF (RFC 7208) for the
// envelope sender and the HELO name, DKIM (RFC 6376) signatures, and DMARC
// (RFC 7489) alignment with the From domain. From those it gives a trust
// verdict an agent can act on:
//
//   - "authenticated": the From domain is proven, by a DKIM signature or an
//     SPF pass aligned with it (what DMARC calls a pass);
//   - "failed": the From domain publishes a DMARC policy and the message
//     fails it, or the envelope sender is the From domain (or aligned with
//     it) and SPF says the client may not send for it;
//   - "unauthenticated": anything else; the sender may be who they claim,
//     but nothing proves it.
//
// Every DNS lookup goes through a Resolver, so tests need no network, and
// the whole evaluation runs under one deadline.
package mailauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"
)

// Trust verdicts.
const (
	Authenticated   = "authenticated"
	Unauthenticated = "unauthenticated"
	Failed          = "failed"
)

// Resolver is the subset of *net.Resolver the checks use.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// TXTMXResolver is a resolver that answers only TXT and MX questions.
type TXTMXResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Complete returns r as a Resolver. When r cannot look up addresses or
// names, those lookups find nothing (a and mx mechanisms never match).
func Complete(r TXTMXResolver) Resolver {
	if full, ok := r.(Resolver); ok {
		return full
	}
	return partial{r}
}

type partial struct{ TXTMXResolver }

func (partial) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (partial) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
}

func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// Input is what the receiving server knew about a message.
type Input struct {
	IP       net.IP // the connecting client
	HELO     string
	MailFrom string // the envelope sender; "" for a null sender (bounces)
	Raw      []byte // the message as received
	From     string // the RFC5322.From address, lowercase; "" when missing
	// FromCount is how many addresses From holds; DMARC needs exactly one.
	FromCount int
}

// Results is the full evaluation, stored as a message's
// authentication_results.
type Results struct {
	Trust  string       `json:"trust"`
	Reason string       `json:"reason"`
	SPF    SPFResult    `json:"spf"`
	HELO   SPFResult    `json:"spf_helo"`
	DKIM   []DKIMResult `json:"dkim"`
	DMARC  DMARCResult  `json:"dmarc"`
	// Header is the same information as an Authentication-Results header
	// field value (RFC 8601).
	Header string `json:"header"`
}

// DKIMResult is the outcome of one DKIM signature.
type DKIMResult struct {
	Result   string `json:"result"` // pass, fail, temperror, permerror
	Domain   string `json:"domain"`
	Selector string `json:"selector,omitempty"`
	Aligned  bool   `json:"aligned"`
	Reason   string `json:"reason,omitempty"`
}

// DMARCResult is the DMARC evaluation for the From domain.
type DMARCResult struct {
	Result     string `json:"result"` // pass, fail, none, temperror, permerror
	FromDomain string `json:"from_domain,omitempty"`
	Policy     string `json:"policy,omitempty"` // none, quarantine, reject: what the domain asks for a failure
	Record     string `json:"record_domain,omitempty"`
	SPFAligned bool   `json:"spf_aligned"`
	DKIMAlign  bool   `json:"dkim_aligned"`
}

// MaxSignatures bounds the DKIM signatures checked per message.
const MaxSignatures = 5

// Evaluate checks SPF, DKIM and DMARC for one message within timeout.
// authServID names the checking host in Results.Header.
func Evaluate(ctx context.Context, r Resolver, in Input, timeout time.Duration, authServID string) Results {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var res Results
	mailFrom := strings.ToLower(strings.TrimSpace(in.MailFrom))
	helo := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.HELO), "."))
	// SPF for the HELO name (when it is a name), then for the envelope
	// sender, or postmaster@<helo> for a null sender.
	if validDomain(helo) && net.ParseIP(strings.Trim(helo, "[]")) == nil {
		res.HELO = CheckSPF(ctx, r, in.IP, helo, "postmaster@"+helo, helo)
	} else {
		res.HELO = SPFResult{Result: None, Domain: helo, Reason: "HELO is not a domain name"}
	}
	spfDomain := ""
	if _, d, ok := strings.Cut(mailFrom, "@"); ok && mailFrom != "" {
		spfDomain = d
		res.SPF = CheckSPF(ctx, r, in.IP, d, mailFrom, helo)
	} else {
		spfDomain = helo
		res.SPF = res.HELO
	}

	res.DKIM = checkDKIM(ctx, r, in.Raw)

	fromDomain := ""
	if _, d, ok := strings.Cut(in.From, "@"); ok {
		fromDomain = strings.ToLower(strings.TrimSuffix(d, "."))
	}
	res.DMARC = checkDMARC(ctx, r, fromDomain, in.FromCount, spfDomain, &res)
	res.Trust, res.Reason = verdict(res, fromDomain, spfDomain)
	res.Header = header(authServID, res, mailFrom, helo)
	return res
}

func checkDKIM(ctx context.Context, r Resolver, raw []byte) []DKIMResult {
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return r.LookupTXT(ctx, domain)
		},
		MaxVerifications: MaxSignatures,
	})
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) && len(vs) == 0 {
		return []DKIMResult{}
	}
	out := make([]DKIMResult, 0, len(vs))
	for _, v := range vs {
		d := DKIMResult{Domain: strings.ToLower(v.Domain), Result: Pass}
		if v.Err != nil {
			switch {
			case dkim.IsTempFail(v.Err):
				d.Result = TempError
			case dkim.IsPermFail(v.Err):
				d.Result = PermError
			default:
				d.Result = Fail
			}
			d.Reason = truncate(v.Err.Error(), 120)
		}
		out = append(out, d)
	}
	addSelectors(raw, out)
	return out
}

// addSelectors fills in each signature's s= tag, which the verifier does
// not return, from the DKIM-Signature fields in order.
func addSelectors(raw []byte, out []DKIMResult) {
	head := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		head = raw[:i]
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		head = raw[:i]
	}
	n := 0
	lines := bytes.Split(head, []byte("\n"))
	for i := 0; i < len(lines) && n < len(out); i++ {
		l := lines[i]
		if len(l) < 16 || !bytes.EqualFold(l[:15], []byte("dkim-signature:")) {
			continue
		}
		v := string(l[15:])
		for j := i + 1; j < len(lines) && len(lines[j]) > 0 && (lines[j][0] == ' ' || lines[j][0] == '\t'); j++ {
			v += string(lines[j])
		}
		for _, tag := range strings.Split(v, ";") {
			k, val, ok := strings.Cut(strings.TrimSpace(tag), "=")
			if ok && strings.TrimSpace(k) == "s" {
				out[n].Selector = truncate(strings.Join(strings.Fields(val), ""), 63)
			}
		}
		n++
	}
}

// OrgDomain is a name's organizational domain: the registrable domain
// under its public suffix (example.co.uk for mail.example.co.uk).
func OrgDomain(d string) string {
	d = strings.ToLower(strings.TrimSuffix(d, "."))
	if org, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return org
	}
	return d
}

func aligned(a, b string, strict bool) bool {
	if a == "" || b == "" {
		return false
	}
	if strict {
		return a == b
	}
	return OrgDomain(a) == OrgDomain(b)
}

func checkDMARC(ctx context.Context, r Resolver, fromDomain string, fromCount int, spfDomain string, res *Results) DMARCResult {
	out := DMARCResult{Result: None, FromDomain: fromDomain}
	if fromDomain == "" || fromCount != 1 || !validDomain(fromDomain) {
		out.Result = PermError
		// Alignment is still computed for the verdict, against nothing.
		return out
	}
	rec, at, err := lookupDMARC(ctx, r, fromDomain)
	strictSPF, strictDKIM := false, false
	if rec != nil {
		strictSPF, strictDKIM = rec.SPFAlignment == dmarc.AlignmentStrict, rec.DKIMAlignment == dmarc.AlignmentStrict
		out.Policy, out.Record = string(rec.Policy), at
		if at != fromDomain && rec.SubdomainPolicy != "" {
			out.Policy = string(rec.SubdomainPolicy)
		}
	}
	out.SPFAligned = res.SPF.Result == Pass && aligned(spfDomain, fromDomain, strictSPF)
	for i := range res.DKIM {
		res.DKIM[i].Aligned = aligned(res.DKIM[i].Domain, fromDomain, strictDKIM)
		if res.DKIM[i].Result == Pass && res.DKIM[i].Aligned {
			out.DKIMAlign = true
		}
	}
	switch {
	case err != nil:
		out.Result = TempError
	case rec == nil:
		out.Result = None
	case out.SPFAligned || out.DKIMAlign:
		out.Result = Pass
	default:
		out.Result = Fail
	}
	return out
}

// lookupDMARC finds the policy for d, at d or else at its organizational
// domain.
func lookupDMARC(ctx context.Context, r Resolver, d string) (*dmarc.Record, string, error) {
	try := []string{d}
	if org := OrgDomain(d); org != d {
		try = append(try, org)
	}
	for _, name := range try {
		txts, err := r.LookupTXT(ctx, "_dmarc."+name)
		if err != nil {
			if notFound(err) {
				continue
			}
			return nil, "", err
		}
		var recs []*dmarc.Record
		for _, t := range txts {
			if !strings.HasPrefix(strings.TrimSpace(t), "v=DMARC1") {
				continue
			}
			if rec, err := dmarc.Parse(t); err == nil {
				recs = append(recs, rec)
			}
		}
		if len(recs) == 1 {
			return recs[0], name, nil
		}
		// None, or more than one: no policy at this name (RFC 7489 6.6.3).
	}
	return nil, "", nil
}

func verdict(res Results, fromDomain, spfDomain string) (string, string) {
	switch {
	case res.DMARC.Result == Pass:
		how := "an aligned DKIM signature"
		if !res.DMARC.DKIMAlign {
			how = "an SPF pass for the envelope sender " + spfDomain
		}
		return Authenticated, fmt.Sprintf("the From domain %s is proven by %s", fromDomain, how)
	case res.DMARC.Result == Fail:
		return Failed, fmt.Sprintf("the From domain %s publishes a DMARC policy (p=%s) and this message fails it: no aligned DKIM signature or SPF pass", fromDomain, orNone(res.DMARC.Policy))
	case res.DMARC.Result == None && (res.DMARC.SPFAligned || res.DMARC.DKIMAlign):
		return Authenticated, fmt.Sprintf("the From domain %s is proven by an aligned %s (the domain publishes no DMARC policy)", fromDomain, map[bool]string{true: "DKIM signature", false: "SPF pass"}[res.DMARC.DKIMAlign])
	case res.SPF.Result == Fail && aligned(spfDomain, fromDomain, false):
		return Failed, fmt.Sprintf("SPF for %s says the sending server may not send for it", spfDomain)
	case fromDomain == "":
		return Unauthenticated, "the message has no usable From address"
	case res.DMARC.Result == TempError:
		return Unauthenticated, "the DMARC policy of " + fromDomain + " could not be looked up (DNS error)"
	}
	return Unauthenticated, fmt.Sprintf("nothing proves the From domain %s: no aligned DKIM signature or SPF pass, and no DMARC policy", fromDomain)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func header(id string, res Results, mailFrom, helo string) string {
	var results []authres.Result
	spfFrom := mailFrom
	if spfFrom == "" {
		spfFrom = "postmaster@" + helo
	}
	results = append(results, &authres.SPFResult{Value: authres.ResultValue(res.SPF.Result), From: spfFrom, Helo: helo})
	if len(res.DKIM) == 0 {
		results = append(results, &authres.DKIMResult{Value: authres.ResultNone})
	}
	for _, d := range res.DKIM {
		results = append(results, &authres.DKIMResult{Value: authres.ResultValue(d.Result), Domain: d.Domain})
	}
	results = append(results, &authres.DMARCResult{Value: authres.ResultValue(res.DMARC.Result), From: res.DMARC.FromDomain})
	return strings.Join(strings.Fields(authres.Format(id, results)), " ")
}
