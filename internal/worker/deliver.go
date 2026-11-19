package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/textproto"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Resolver is the subset of *net.Resolver the deliverer uses.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// SecureMXResolver is a Resolver that can also say whether an MX answer
// was DNSSEC-validated. DANE applies to MX hosts only when it was (RFC 7672
// section 2.2.1).
type SecureMXResolver interface {
	Resolver
	LookupMXSecure(ctx context.Context, name string) (mxs []*net.MX, secure bool, err error)
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
	// DataTimeout bounds each block of the message as it is written (RFC
	// 5321 section 4.5.3.2: 3 minutes), FinalTimeout the wait for the reply
	// after the message (10 minutes: a spurious timeout there sends the
	// message twice).
	DataTimeout  time.Duration
	FinalTimeout time.Duration
	MaxMX        int

	// STS supplies recipient domains' MTA-STS policies; nil turns MTA-STS
	// off. DANE supplies MX hosts' TLSA records from a validating resolver;
	// nil turns DANE off. Neither applies with Override.
	STS  STSSource
	DANE TLSASource
	// RootCAs verifies MX certificates; nil means the system roots.
	RootCAs *x509.CertPool

	// Connections that end a transaction cleanly are kept for the next
	// message to the same server (RSET in between): at most MaxIdle per
	// server for IdleTimeout, and MaxPerConn messages per connection.
	// NoReuse turns this off.
	NoReuse     bool
	MaxIdle     int
	IdleTimeout time.Duration
	MaxPerConn  int

	once sync.Once
	pool *sessionPool
}

type Result struct {
	Outcome   string
	Code      int
	Enhanced  string
	Response  string
	MXHost    string
	TLS       string
	TLSPolicy string // what an unenforced TLS policy would have changed (protocol.Report.TLSPolicy)
	Stage     string // the SMTP step the deciding reply came from (protocol.Report.Stage)
}

// tlsPolicy is how one SMTP session treats TLS.
type tlsPolicy struct {
	kind   int
	domain string       // the recipient domain (the policy domain, and a DANE-TA name)
	tlsa   []TLSARecord // for tlsDANE: the usable records
}

const (
	tlsOpportunistic = iota // STARTTLS when offered (TLS 1.0 or later); a broken STARTTLS falls back to plaintext
	tlsPlain                // the plaintext retry after a broken STARTTLS
	tlsSTS                  // MTA-STS enforce: STARTTLS and a certificate valid for the MX host
	tlsDANE                 // DANE: STARTTLS and a certificate that matches the TLSA records
	tlsMandatory            // DANE with only unusable TLSA records: STARTTLS required, certificate not checked
)

func (p tlsPolicy) key() string {
	switch p.kind {
	case tlsSTS, tlsDANE, tlsMandatory:
		return strconv.Itoa(p.kind) + ":" + p.domain
	}
	return strconv.Itoa(p.kind)
}

func (d *Deliverer) setDefaults() {
	d.once.Do(func() {
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
			d.CmdTimeout = 5 * time.Minute
		}
		if d.DataTimeout == 0 {
			d.DataTimeout = 3 * time.Minute
		}
		if d.FinalTimeout == 0 {
			d.FinalTimeout = 10 * time.Minute
		}
		if d.MaxMX == 0 {
			d.MaxMX = 5
		}
		if d.EHLO == "" {
			d.EHLO = "localhost"
		}
		if d.MaxIdle == 0 {
			d.MaxIdle = 4
		}
		if d.IdleTimeout == 0 {
			d.IdleTimeout = 20 * time.Second
		}
		if d.MaxPerConn == 0 {
			d.MaxPerConn = 50
		}
		d.pool = &sessionPool{maxIdle: d.MaxIdle, idleTimeout: d.IdleTimeout}
	})
}

// Close closes the connections kept for reuse.
func (d *Deliverer) Close() {
	if d.pool != nil {
		d.pool.closeIdle()
	}
}

type target struct{ host, addr string }

// Deliver tries the recipient domain's mail servers in preference order and
// returns the first authoritative answer.
//
// TLS follows the strictest policy that applies to each MX host:
//   - DANE, when the MX answer and the host's TLSA records are DNSSEC-secure
//     (a validating resolver vouches for them) and some record is usable:
//     TLS is required and the certificate must match them (RFC 7672). DANE
//     takes precedence over MTA-STS (RFC 8461 section 2). When every TLSA
//     record is unusable, TLS is still required, without authentication
//     (RFC 7672 section 2.2), unless MTA-STS in enforce mode asks for more.
//   - MTA-STS in enforce mode: only MX hosts the policy names are used, and
//     TLS 1.2 or later is required with a certificate valid for the host.
//   - Otherwise opportunistic STARTTLS, TLS 1.0 or later: any encryption
//     beats none (RFC 7435). An MTA-STS policy in testing mode only records
//     what enforce mode would have done (Result.TLSPolicy).
//
// When no host satisfies a required policy, the outcome is deferred with
// enhanced code 4.7.5 and a response that names the policy. That outcome
// also wins over later hosts that could not be reached at all, but not over
// a reply from a receiving server.
func (d *Deliverer) Deliver(ctx context.Context, j protocol.Job) Result {
	d.setDefaults()
	domain := strings.ToLower(j.RcptDomain)
	targets, mxSecure, early, ok := d.targets(ctx, domain)
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
	dane := d.DANE != nil && d.Override == "" && mxSecure
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
		if dane {
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
				if usable := usableTLSA(recs); len(usable) > 0 {
					pol = tlsPolicy{kind: tlsDANE, domain: domain, tlsa: usable}
				} else {
					pol = tlsPolicy{kind: tlsMandatory, domain: domain}
				}
			}
		}
		if (pol.kind == tlsOpportunistic || pol.kind == tlsMandatory) && sts != nil && sts.Mode == STSEnforce {
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

func usableTLSA(recs []TLSARecord) []TLSARecord {
	var out []TLSARecord
	for _, r := range recs {
		if r.Usable() {
			out = append(out, r)
		}
	}
	return out
}

// policyFailure is the outcome when a TLS policy forbids using a server.
func policyFailure(host, tlsMode, msg string) Result {
	return Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.7.5", Response: "4.7.5 " + msg, MXHost: host, TLS: tlsMode}
}

// targets resolves the domain's mail servers. mxSecure reports whether the
// MX answer was DNSSEC-validated, which DANE needs.
func (d *Deliverer) targets(ctx context.Context, domain string) ([]target, bool, Result, bool) {
	if d.Override != "" {
		host, _, err := net.SplitHostPort(d.Override)
		if err != nil {
			host = d.Override
		}
		return []target{{host: host, addr: d.Override}}, false, Result{}, true
	}

	var hosts []string
	var mxs []*net.MX
	var err error
	// A resolver that cannot tell whether the answer was validated is
	// trusted as before; NewRunner gives DANE workers the validating one.
	mxSecure := true
	if sr, ok := d.Resolver.(SecureMXResolver); ok {
		mxs, mxSecure, err = sr.LookupMXSecure(ctx, domain)
	} else {
		mxs, err = d.Resolver.LookupMX(ctx, domain)
	}
	switch {
	case err != nil && !isNotFound(err):
		return nil, false, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.3", Response: "MX lookup failed: " + err.Error()}, false
	case err == nil && len(mxs) == 1 && (mxs[0].Host == "." || mxs[0].Host == ""):
		return nil, false, Result{Outcome: protocol.OutcomeBounced, Code: 556, Enhanced: "5.1.10",
			Response: "556 5.1.10 the recipient domain publishes a null MX (RFC 7505) and accepts no mail"}, false
	case err == nil && len(mxs) > 0:
		sortMX(mxs)
		for _, mx := range mxs {
			hosts = append(hosts, strings.TrimSuffix(mx.Host, "."))
		}
	default:
		// No MX: RFC 5321 section 5.1 implicit MX, the domain itself. Its
		// TLSA records are checked like an MX host's (the address lookup
		// in the TLSA source tells whether it is signed).
		if _, err := d.Resolver.LookupIPAddr(ctx, domain); err != nil {
			if isNotFound(err) {
				return nil, false, Result{Outcome: protocol.OutcomeBounced, Code: 550, Enhanced: "5.1.2",
					Response: "550 5.1.2 the recipient domain has no MX and no address record"}, false
			}
			return nil, false, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.3", Response: "address lookup failed: " + err.Error()}, false
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
		return nil, false, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.4",
			Response: "none of the domain's mail servers has a usable address"}, false
	}
	return out, mxSecure, Result{}, true
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

// opportunisticTLS takes any TLS the server offers, from TLS 1.0 and with
// the old RSA key exchange too: encryption without authentication still
// beats plaintext (RFC 7435). Required TLS (MTA-STS, DANE) uses strictTLS.
func opportunisticTLS(host string) *tls.Config {
	suites := []uint16{}
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		switch s.ID {
		case tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA, tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA:
			suites = append(suites, s.ID)
		}
	}
	return &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, CipherSuites: suites}
}

func strictTLS(host string) *tls.Config {
	// The certificate is checked after the handshake, so that a failure can
	// say why.
	return &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

// session delivers one message over a connection to t: a pooled one when
// there is one for the same server and TLS policy, else a new one. next
// reports whether trying another server makes sense; tlsFailed asks the
// caller to retry without TLS.
func (d *Deliverer) session(ctx context.Context, t target, j protocol.Job, pol tlsPolicy) (Result, bool, bool) {
	key := t.addr + "|" + t.host + "|" + pol.key()
	var c *smtpClient
	if !d.NoReuse {
		if c = d.pool.get(key); c != nil {
			c.bind(ctx)
			if err := c.reset(); err != nil {
				c.close()
				c = nil
			}
		}
	}
	if c == nil {
		var res Result
		var next, tlsFailed bool
		c, res, next, tlsFailed = d.open(ctx, t, pol)
		if c == nil {
			return res, next, tlsFailed
		}
	}
	res, next, reusable := transact(c, j, t.host, d.DataTimeout, d.FinalTimeout)
	switch {
	case reusable && !d.NoReuse && c.sent < d.MaxPerConn && ctx.Err() == nil:
		d.pool.put(key, c)
	case reusable:
		c.quit()
	default:
		c.close()
	}
	return res, next, false
}

// open connects to t and gets the session ready for MAIL: greeting, EHLO
// and TLS as the policy asks. On failure the client is nil and res says why.
func (d *Deliverer) open(ctx context.Context, t target, pol tlsPolicy) (*smtpClient, Result, bool, bool) {
	conn, err := dial(ctx, d.SourceIP, t.addr, d.DialTimeout)
	if err != nil {
		return nil, Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.1", Response: "connect to " + t.addr + ": " + err.Error(),
			MXHost: t.host}, true, false
	}
	c := newSMTPClient(conn, t.host, d.CmdTimeout)
	c.bind(ctx)
	res, next, tlsFailed, ok := d.prepare(ctx, c, t, pol)
	if !ok {
		c.close()
		return nil, res, next, tlsFailed
	}
	return c, Result{}, false, false
}

// prepare runs the greeting, EHLO and STARTTLS on a new connection.
func (d *Deliverer) prepare(ctx context.Context, c *smtpClient, t target, pol tlsPolicy) (res Result, next, tlsFailed, ok bool) {
	fail := func(stage, enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: msg, MXHost: t.host, TLS: c.mode, Stage: stage}
	}
	if err := c.greet(); err != nil {
		r := fromError(err, t.host, "", protocol.StageGreeting)
		r.Outcome = protocol.OutcomeDeferred // a refusing greeting is about this server, try the next
		return r, true, false, false
	}
	hello := func(what string) (Result, bool) {
		err := c.hello(d.EHLO)
		if err == nil {
			return Result{}, true
		}
		r := fromError(err, t.host, c.mode, protocol.StageEHLO)
		// About this connection, not the recipient: try the next server.
		r.Outcome = protocol.OutcomeDeferred
		if !isReply(err) {
			r.Enhanced, r.Response = "4.4.2", what+": "+err.Error()
		}
		return r, false
	}
	if r, ok := hello("EHLO"); !ok {
		return r, true, false, false
	}
	c.mode = "none"
	starttls := c.has("STARTTLS")
	switch pol.kind {
	case tlsOpportunistic:
		if !starttls {
			return Result{}, false, false, true
		}
		if err := c.startTLS(ctx, opportunisticTLS(t.host)); err != nil {
			return fail(protocol.StageSTARTTLS, "4.7.0", "STARTTLS: "+err.Error()), true, true, false
		}
		c.mode = "opportunistic"
		if verifyPeer(*c.tlsState, t.host, d.RootCAs) == nil {
			c.mode = "verified"
		}
	case tlsSTS, tlsDANE, tlsMandatory:
		var name string
		switch pol.kind {
		case tlsSTS:
			name = "MTA-STS policy of " + pol.domain + " (enforce)"
		case tlsDANE:
			name = "DANE (TLSA records of " + t.host + ")"
		default:
			name = "DANE (TLSA records of " + t.host + ": none is usable, so TLS is required without authentication, RFC 7672 section 2.2)"
		}
		refuse := func(msg string) (Result, bool, bool, bool) {
			c.cmd(10*time.Second, "QUIT")
			return policyFailure(t.host, c.mode, name+": "+msg), true, false, false
		}
		if !starttls {
			return refuse(t.host + " does not offer STARTTLS")
		}
		if err := c.startTLS(ctx, strictTLS(t.host)); err != nil {
			return policyFailure(t.host, c.mode, name+": STARTTLS with "+t.host+" failed: "+err.Error()), true, false, false
		}
		c.mode = "opportunistic"
		st := *c.tlsState
		switch pol.kind {
		case tlsDANE:
			if err := VerifyDANE(st.PeerCertificates, pol.tlsa, []string{t.host, pol.domain}); err != nil {
				return refuse(err.Error())
			}
			c.mode = "dane"
		case tlsSTS:
			if err := verifyPeer(st, t.host, d.RootCAs); err != nil {
				return refuse("the certificate is not valid for " + t.host + ": " + err.Error())
			}
			c.mode = "verified+mta-sts"
		default:
			if verifyPeer(st, t.host, d.RootCAs) == nil {
				c.mode = "verified"
			}
		}
	default: // tlsPlain
		return Result{}, false, false, true
	}
	if r, ok := hello("EHLO after STARTTLS"); !ok {
		return r, true, false, false
	}
	return Result{}, false, false, true
}

func dial(ctx context.Context, sourceIP, addr string, timeout time.Duration) (net.Conn, error) {
	dialer := net.Dialer{Timeout: timeout}
	if sourceIP != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(sourceIP)}
	}
	return dialer.DialContext(ctx, "tcp", addr)
}

// needs reports what a message requires of the server: SMTPUTF8 for
// non-ASCII addresses or header fields (RFC 6531, 6532), 8BITMIME for any
// 8-bit data (RFC 6152).
func needs(j protocol.Job) (utf8, eightBit bool) {
	end := bytes.Index(j.Raw, []byte("\r\n\r\n"))
	if end < 0 {
		end = len(j.Raw)
	}
	header8 := !isASCIIBytes(j.Raw[:end])
	body8 := !isASCIIBytes(j.Raw[end:])
	return header8 || !isASCII(j.MailFrom) || !isASCII(j.Rcpt), header8 || body8
}

func isASCIIBytes(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// transact runs MAIL, RCPT and DATA on a ready session and maps the
// replies to a Result. next reports whether another server may be tried;
// reusable whether the connection can carry another message.
func transact(c *smtpClient, j protocol.Job, host string, blockTimeout, finalTimeout time.Duration) (res Result, next, reusable bool) {
	mode := c.mode
	fail := func(stage, enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: msg, MXHost: host, TLS: mode, Stage: stage}
	}
	reply := func(stage string, code int, msg string) Result {
		r := fromReply(code, msg)
		r.MXHost, r.TLS, r.Stage = host, mode, stage
		return r
	}

	// Never send what the server cannot take: converting the message would
	// break its DKIM signature, so the attempt is deferred (another MX, or a
	// later attempt, may accept it). 4.6.3: conversion required but not
	// supported (RFC 3463).
	utf8, eightBit := needs(j)
	if utf8 && !c.has("SMTPUTF8") {
		return fail(protocol.StageMail, "4.6.3", "4.6.3 the message needs SMTPUTF8 (non-ASCII addresses or header fields) and "+
			host+" does not offer it"), true, true
	}
	if eightBit && !c.has("8BITMIME") {
		return fail(protocol.StageMail, "4.6.3", "4.6.3 the message has 8-bit data and "+host+" does not offer 8BITMIME"), true, true
	}
	params := ""
	if eightBit {
		params += " BODY=8BITMIME"
	}
	if utf8 {
		params += " SMTPUTF8"
	}
	if c.has("SIZE") {
		params += " SIZE=" + strconv.Itoa(len(j.Raw))
	}

	code, msg, err := c.cmd(c.cmdTimeout, "MAIL FROM:<%s>%s", j.MailFrom, params)
	if err != nil {
		return fail(protocol.StageMail, "4.4.2", "MAIL: "+err.Error()), true, false
	}
	if code/100 != 2 {
		return reply(protocol.StageMail, code, msg), false, code != 421
	}
	code, msg, err = c.cmd(c.cmdTimeout, "RCPT TO:<%s>", j.Rcpt)
	if err != nil {
		return fail(protocol.StageRcpt, "4.4.2", "RCPT: "+err.Error()), true, false
	}
	if code/100 != 2 {
		return reply(protocol.StageRcpt, code, msg), false, code != 421
	}

	// DATA is driven by hand so the final reply text (the receiver's queue
	// ID) is kept and a lost reply can be told apart from a refusal.
	code, msg, err = c.cmd(c.cmdTimeout, "DATA")
	if err != nil {
		return fail(protocol.StageData, "4.4.2", "DATA: "+err.Error()), true, false
	}
	if code != 354 {
		return reply(protocol.StageData, code, msg), false, code != 421
	}

	c.raw.setBlock(blockTimeout)
	dw := c.text.DotWriter()
	if _, err := dw.Write(j.Raw); err != nil {
		c.raw.setBlock(0)
		// The terminating dot never left, so the server cannot have accepted it.
		return fail(protocol.StageMessage, "4.4.2", "connection lost while sending the message body: "+err.Error()), false, false
	}
	if err := dw.Close(); err != nil {
		c.raw.setBlock(0)
		return Result{Outcome: protocol.OutcomeUnknown, Enhanced: "4.4.2", MXHost: host, TLS: mode, Stage: protocol.StageMessage,
			Response: "connection lost while finishing the message; the server may have accepted it: " + err.Error()}, false, false
	}
	c.raw.setBlock(0)
	c.deadline(finalTimeout)
	code, msg, err = c.text.ReadResponse(0)
	if err != nil {
		return Result{Outcome: protocol.OutcomeUnknown, Enhanced: "4.4.2", MXHost: host, TLS: mode, Stage: protocol.StageMessage,
			Response: "no final reply after the message was sent; the server may have accepted it: " + err.Error()}, false, false
	}
	c.sent++
	return reply(protocol.StageMessage, code, msg), false, code != 421
}

var enhancedRE = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}`)

// fromReply maps a reply to a Result. The response is cut to 1000 bytes
// on a character boundary and made valid UTF-8, so the control plane can
// store it as it is.
func fromReply(code int, msg string) Result {
	r := Result{Outcome: protocol.OutcomeDeferred, Code: code}
	switch code / 100 {
	case 2:
		r.Outcome = protocol.OutcomeDelivered
	case 5:
		r.Outcome = protocol.OutcomeBounced
	}
	r.Enhanced = enhancedRE.FindString(msg)
	r.Response = protocol.CleanText(fmt.Sprintf("%d %s", code, msg), 1000)
	return r
}

func isReply(err error) bool {
	var re *replyError
	var te *textproto.Error
	return errors.As(err, &re) || errors.As(err, &te)
}

func fromError(err error, host, tlsMode, stage string) Result {
	var re *replyError
	if errors.As(err, &re) {
		r := fromReply(re.Code, re.Msg)
		r.MXHost, r.TLS, r.Stage = host, tlsMode, re.Stage
		return r
	}
	var te *textproto.Error
	if errors.As(err, &te) {
		r := fromReply(te.Code, te.Msg)
		r.MXHost, r.TLS, r.Stage = host, tlsMode, stage
		return r
	}
	return Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.2", Response: protocol.CleanText(err.Error(), 1000),
		MXHost: host, TLS: tlsMode, Stage: stage}
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
