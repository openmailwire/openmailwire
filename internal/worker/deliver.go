package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Resolver is the subset of *net.Resolver the deliverer uses.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Deliverer hands one message to one recipient's mail server over SMTP.
type Deliverer struct {
	EHLO        string // must match the sending IP's reverse DNS in production
	SourceIP    string // local address to send from; empty lets the OS choose
	Override    string // host:port that receives every message (development only)
	Port        int    // 25 unless testing
	IPv4Only    bool   // receivers reject IPv6 senders without reverse DNS
	Resolver    Resolver
	DialTimeout time.Duration
	CmdTimeout  time.Duration
	DataTimeout time.Duration
	MaxMX       int

	// STS supplies recipient domains' MTA-STS policies; nil turns MTA-STS
	// off. DANE supplies MX hosts' TLSA records from a validating resolver;
	// nil turns DANE off. Neither applies with Override.
	STS  STSSource
	DANE TLSASource
	// RootCAs verifies MX certificates; nil means the system roots.
	RootCAs *x509.CertPool
}

type Result struct {
	Outcome   string
	Code      int
	Enhanced  string
	Response  string
	MXHost    string
	TLS       string
	TLSPolicy string // what an unenforced TLS policy would have changed (protocol.Report.TLSPolicy)
}

// tlsPolicy is how one SMTP session treats TLS.
type tlsPolicy struct {
	kind   int
	domain string       // the recipient domain (the policy domain, and a DANE-TA name)
	tlsa   []TLSARecord // for tlsDANE
}

const (
	tlsOpportunistic = iota // STARTTLS when offered; a broken STARTTLS falls back to plaintext
	tlsPlain                // the plaintext retry after a broken STARTTLS
	tlsSTS                  // MTA-STS enforce: STARTTLS and a certificate valid for the MX host
	tlsDANE                 // DANE: STARTTLS and a certificate that matches the TLSA records
)

func (d *Deliverer) setDefaults() {
	if d.Port == 0 {
		d.Port = 25
	}
	if d.Resolver == nil {
		d.Resolver = net.DefaultResolver
	}
	if d.DialTimeout == 0 {
		d.DialTimeout = 30 * time.Second
	}
	if d.CmdTimeout == 0 {
		d.CmdTimeout = 2 * time.Minute
	}
	if d.DataTimeout == 0 {
		d.DataTimeout = 5 * time.Minute
	}
	if d.MaxMX == 0 {
		d.MaxMX = 5
	}
	if d.EHLO == "" {
		d.EHLO = "localhost"
	}
}

type target struct{ host, addr string }

// Deliver tries the recipient domain's mail servers in preference order and
// returns the first authoritative answer.
//
// TLS follows the strictest policy that applies to each MX host:
//   - DANE, when the host has usable TLSA records that a validating resolver
//     vouches for: TLS is required and the certificate must match them
//     (RFC 7672). DANE takes precedence over MTA-STS (RFC 8461 section 2).
//   - MTA-STS in enforce mode: only MX hosts the policy names are used, and
//     TLS is required with a certificate valid for the host.
//   - Otherwise opportunistic STARTTLS. An MTA-STS policy in testing mode
//     only records what enforce mode would have done (Result.TLSPolicy).
//
// When no host satisfies a required policy, the outcome is deferred with
// enhanced code 4.7.5 and a response that names the policy. That outcome
// also wins over later hosts that could not be reached at all, but not over
// a reply from a receiving server.
func (d *Deliverer) Deliver(ctx context.Context, j protocol.Job) Result {
	d.setDefaults()
	domain := strings.ToLower(j.RcptDomain)
	targets, early, ok := d.targets(ctx, domain)
	if !ok {
		return early
	}
	var sts *STSPolicy
	if d.STS != nil && d.Override == "" {
		// A lookup or fetch error never blocks delivery (RFC 8461 section
		// 5.1): without a valid policy, deliver as if there were none.
		sts, _ = d.STS.Policy(ctx, domain)
		if sts != nil && sts.Mode == STSNone {
			sts = nil
		}
	}
	tlsa := map[string][]TLSARecord{}
	tlsaErr := map[string]error{}

	last := Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.1", Response: "no mail server for the domain could be reached"}
	var refused *Result // the last host a TLS policy refused
	refuse := func(r Result) {
		last = r
		refused = &r
	}
	for _, t := range targets {
		pol := tlsPolicy{kind: tlsOpportunistic, domain: domain}
		if d.DANE != nil && d.Override == "" {
			recs, seen := tlsa[t.host]
			if !seen {
				recs, tlsaErr[t.host] = d.DANE.TLSA(ctx, t.host)
				tlsa[t.host] = recs
			}
			if err := tlsaErr[t.host]; err != nil {
				refuse(policyFailure(t.host, "", "DANE: cannot tell whether "+t.host+" requires TLS: "+err.Error()))
				continue
			}
			if len(recs) > 0 {
				pol = tlsPolicy{kind: tlsDANE, domain: domain, tlsa: recs}
			}
		}
		if pol.kind == tlsOpportunistic && sts != nil && sts.Mode == STSEnforce {
			if !sts.Matches(t.host) {
				refuse(policyFailure(t.host, "", fmt.Sprintf("MTA-STS policy of %s (enforce): MX host %s is not one of %s",
					domain, t.host, strings.Join(sts.MX, ", "))))
				continue
			}
			pol.kind = tlsSTS
		}

		res, next, tlsFailed := d.session(ctx, t, j, pol)
		if tlsFailed {
			// Opportunistic TLS: a broken STARTTLS falls back to plaintext,
			// as other MTAs do.
			res, next, _ = d.session(ctx, t, j, tlsPolicy{kind: tlsPlain, domain: domain})
		}
		if sts != nil && sts.Mode == STSTesting && res.TLS != "" {
			switch {
			case !sts.Matches(t.host):
				res.TLSPolicy = fmt.Sprintf("MTA-STS policy of %s (testing): MX host %s is not one of %s; enforce mode would not use it",
					domain, t.host, strings.Join(sts.MX, ", "))
			case res.TLS != "verified":
				res.TLSPolicy = fmt.Sprintf("MTA-STS policy of %s (testing): TLS with %s was %q, not verified; enforce mode would defer",
					domain, t.host, res.TLS)
			}
		}
		if !next {
			return res
		}
		if res.Enhanced == "4.7.5" && res.Code == 0 {
			refuse(res)
		} else {
			last = res
		}
		if ctx.Err() != nil {
			break
		}
	}
	if refused != nil && last.Code == 0 {
		return *refused
	}
	return last
}

// policyFailure is the outcome when a TLS policy forbids using a server.
func policyFailure(host, tlsMode, msg string) Result {
	return Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.7.5", Response: "4.7.5 " + msg, MXHost: host, TLS: tlsMode}
}

func (d *Deliverer) targets(ctx context.Context, domain string) ([]target, Result, bool) {
	if d.Override != "" {
		host, _, err := net.SplitHostPort(d.Override)
		if err != nil {
			host = d.Override
		}
		return []target{{host: host, addr: d.Override}}, Result{}, true
	}

	var hosts []string
	mxs, err := d.Resolver.LookupMX(ctx, domain)
	switch {
	case err != nil && !isNotFound(err):
		return nil, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.3", Response: "MX lookup failed: " + err.Error()}, false
	case err == nil && len(mxs) == 1 && (mxs[0].Host == "." || mxs[0].Host == ""):
		return nil, Result{Outcome: protocol.OutcomeBounced, Code: 556, Enhanced: "5.1.10",
			Response: "556 5.1.10 the recipient domain publishes a null MX (RFC 7505) and accepts no mail"}, false
	case err == nil && len(mxs) > 0:
		sortMX(mxs)
		for _, mx := range mxs {
			hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
		}
	default:
		// No MX: RFC 5321 section 5.1 implicit MX, the domain itself.
		if _, err := d.Resolver.LookupIPAddr(ctx, domain); err != nil {
			if isNotFound(err) {
				return nil, Result{Outcome: protocol.OutcomeBounced, Code: 550, Enhanced: "5.1.2",
					Response: "550 5.1.2 the recipient domain has no MX and no address record"}, false
			}
			return nil, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.3", Response: "address lookup failed: " + err.Error()}, false
		}
		hosts = []string{domain}
	}
	if len(hosts) > d.MaxMX {
		hosts = hosts[:d.MaxMX]
	}

	var out []target
	for _, h := range hosts {
		ips, err := d.Resolver.LookupIPAddr(ctx, h)
		if err != nil {
			continue
		}
		n := 0
		for _, ip := range ips {
			if d.IPv4Only && ip.IP.To4() == nil {
				continue
			}
			out = append(out, target{host: h, addr: net.JoinHostPort(ip.IP.String(), strconv.Itoa(d.Port))})
			if n++; n == 2 {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.4",
			Response: "none of the domain's mail servers has a usable address"}, false
	}
	return out, Result{}, true
}

// sortMX orders by preference and shuffles equal preferences (RFC 5321 5.1).
func sortMX(mxs []*net.MX) {
	rand.Shuffle(len(mxs), func(i, j int) { mxs[i], mxs[j] = mxs[j], mxs[i] })
	sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
}

func isNotFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

// session runs one SMTP conversation. next reports whether trying another
// server makes sense; tlsFailed asks the caller to retry without TLS.
func (d *Deliverer) session(ctx context.Context, t target, j protocol.Job, pol tlsPolicy) (res Result, next, tlsFailed bool) {
	fail := func(enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: msg, MXHost: t.host, TLS: res.TLS}
	}
	conn, err := dial(ctx, d.SourceIP, t.addr, d.DialTimeout)
	if err != nil {
		return fail("4.4.1", "connect to "+t.addr+": "+err.Error()), true, false
	}
	defer conn.Close()
	deadline := deadliner(ctx, conn)

	deadline(d.CmdTimeout)
	c, err := smtp.NewClient(conn, t.host)
	if err != nil {
		r := fromError(err, t.host, res.TLS)
		r.Outcome = protocol.OutcomeDeferred // a refusing greeting is about this server, try the next
		return r, true, false
	}
	defer c.Close()
	deadline(d.CmdTimeout)
	if err := c.Hello(d.EHLO); err != nil {
		return fail("4.4.2", "EHLO: "+err.Error()), true, false
	}
	res.TLS = "none"
	starttls, _ := c.Extension("STARTTLS")
	switch pol.kind {
	case tlsOpportunistic:
		if starttls {
			deadline(d.CmdTimeout)
			if err := c.StartTLS(&tls.Config{ServerName: t.host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
				return fail("4.7.0", "STARTTLS: "+err.Error()), true, true
			}
			res.TLS = "opportunistic"
			if st, ok := c.TLSConnectionState(); ok && verifyPeer(st, t.host, d.RootCAs) == nil {
				res.TLS = "verified"
			}
		}
	case tlsSTS, tlsDANE:
		name := "MTA-STS policy of " + pol.domain + " (enforce)"
		if pol.kind == tlsDANE {
			name = "DANE (TLSA records of " + t.host + ")"
		}
		refuse := func(msg string) (Result, bool, bool) {
			deadline(30 * time.Second)
			c.Quit()
			return policyFailure(t.host, res.TLS, name+": "+msg), true, false
		}
		if !starttls {
			return refuse(t.host + " does not offer STARTTLS")
		}
		deadline(d.CmdTimeout)
		// The certificate is checked below, so that the failure can say why.
		if err := c.StartTLS(&tls.Config{ServerName: t.host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
			return policyFailure(t.host, res.TLS, name+": STARTTLS with "+t.host+" failed: "+err.Error()), true, false
		}
		res.TLS = "opportunistic"
		st, _ := c.TLSConnectionState()
		if pol.kind == tlsDANE {
			if err := VerifyDANE(st.PeerCertificates, pol.tlsa, []string{t.host, pol.domain}); err != nil {
				return refuse(err.Error())
			}
			res.TLS = "dane"
		} else {
			if err := verifyPeer(st, t.host, d.RootCAs); err != nil {
				return refuse("the certificate is not valid for " + t.host + ": " + err.Error())
			}
			res.TLS = "verified+mta-sts"
		}
	}

	r, next := transact(c, j, t.host, res.TLS, deadline, d.CmdTimeout, d.DataTimeout)
	return r, next, false
}

func dial(ctx context.Context, sourceIP, addr string, timeout time.Duration) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	if sourceIP != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(sourceIP)}
	}
	return dialer.DialContext(ctx, "tcp", addr)
}

// deadliner returns a function that sets conn's deadline dur from now, or
// at ctx's deadline when that comes first.
func deadliner(ctx context.Context, conn net.Conn) func(time.Duration) {
	return func(dur time.Duration) {
		dl := time.Now().Add(dur)
		if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
			dl = cd
		}
		conn.SetDeadline(dl)
	}
}

// transact runs MAIL, RCPT and DATA on a ready session and maps the
// replies to a Result. next reports whether another server may be tried.
func transact(c *smtp.Client, j protocol.Job, host, tlsMode string, deadline func(time.Duration), cmdTimeout, dataTimeout time.Duration) (Result, bool) {
	fail := func(enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: msg, MXHost: host, TLS: tlsMode}
	}
	deadline(cmdTimeout)
	if err := c.Mail(j.MailFrom); err != nil {
		return fromError(err, host, tlsMode), !isReply(err)
	}
	deadline(cmdTimeout)
	if err := c.Rcpt(j.Rcpt); err != nil {
		return fromError(err, host, tlsMode), !isReply(err)
	}

	// DATA is driven by hand so the final reply text (the receiver's queue
	// ID) is kept and a lost reply can be told apart from a refusal.
	deadline(cmdTimeout)
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		return fail("4.4.2", "DATA: "+err.Error()), true
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(3)
	c.Text.EndResponse(id)
	if err != nil {
		return fromError(err, host, tlsMode), !isReply(err)
	}

	deadline(dataTimeout)
	dw := c.Text.DotWriter()
	if _, err := dw.Write(j.Raw); err != nil {
		// The terminating dot never left, so the server cannot have accepted it.
		return fail("4.4.2", "connection lost while sending the message body: "+err.Error()), false
	}
	if err := dw.Close(); err != nil {
		return Result{Outcome: protocol.OutcomeUnknown, Enhanced: "4.4.2", MXHost: host, TLS: tlsMode,
			Response: "connection lost while finishing the message; the server may have accepted it: " + err.Error()}, false
	}
	code, msg, err := c.Text.ReadResponse(2)
	if err != nil {
		if isReply(err) {
			return fromError(err, host, tlsMode), false
		}
		return Result{Outcome: protocol.OutcomeUnknown, Enhanced: "4.4.2", MXHost: host, TLS: tlsMode,
			Response: "no final reply after the message was sent; the server may have accepted it: " + err.Error()}, false
	}
	r := fromReply(code, msg)
	r.MXHost, r.TLS = host, tlsMode
	deadline(30 * time.Second)
	c.Quit()
	return r, false
}

var enhancedRE = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}`)

func fromReply(code int, msg string) Result {
	r := Result{Outcome: protocol.OutcomeDeferred, Code: code}
	switch code / 100 {
	case 2:
		r.Outcome = protocol.OutcomeDelivered
	case 5:
		r.Outcome = protocol.OutcomeBounced
	}
	r.Enhanced = enhancedRE.FindString(msg)
	r.Response = fmt.Sprintf("%d %s", code, msg)
	if len(r.Response) > 1000 {
		r.Response = r.Response[:1000]
	}
	return r
}

func isReply(err error) bool {
	var te *textproto.Error
	return errors.As(err, &te)
}

func fromError(err error, host, tlsMode string) Result {
	var te *textproto.Error
	if errors.As(err, &te) {
		r := fromReply(te.Code, te.Msg)
		r.MXHost, r.TLS = host, tlsMode
		return r
	}
	return Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.2", Response: err.Error(), MXHost: host, TLS: tlsMode}
}

// verifyPeer checks the server's certificate against roots (nil: the
// system roots) and host.
func verifyPeer(st tls.ConnectionState, host string, roots *x509.CertPool) error {
	if len(st.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	opts := x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range st.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := st.PeerCertificates[0].Verify(opts)
	return err
}
