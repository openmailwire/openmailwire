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
}

type Result struct {
	Outcome  string
	Code     int
	Enhanced string
	Response string
	MXHost   string
	TLS      string
}

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
func (d *Deliverer) Deliver(ctx context.Context, j protocol.Job) Result {
	d.setDefaults()
	targets, early, ok := d.targets(ctx, strings.ToLower(j.RcptDomain))
	if !ok {
		return early
	}
	last := Result{Outcome: protocol.OutcomeDeferred, Enhanced: "4.4.1", Response: "no mail server for the domain could be reached"}
	for _, t := range targets {
		res, next, tlsFailed := d.session(ctx, t, j, true)
		if tlsFailed {
			// Opportunistic TLS: a broken STARTTLS falls back to plaintext,
			// as other MTAs do, unless policy says otherwise (MTA-STS, DANE:
			// see the roadmap).
			res, next, _ = d.session(ctx, t, j, false)
		}
		if !next {
			return res
		}
		last = res
		if ctx.Err() != nil {
			break
		}
	}
	return last
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
func (d *Deliverer) session(ctx context.Context, t target, j protocol.Job, useTLS bool) (res Result, next, tlsFailed bool) {
	fail := func(enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: msg, MXHost: t.host, TLS: res.TLS}
	}
	dialer := net.Dialer{Timeout: d.DialTimeout}
	if d.SourceIP != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(d.SourceIP)}
	}
	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fail("4.4.1", "connect to "+t.addr+": "+err.Error()), true, false
	}
	defer conn.Close()
	deadline := func(dur time.Duration) {
		dl := time.Now().Add(dur)
		if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
			dl = cd
		}
		conn.SetDeadline(dl)
	}

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
	if useTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			deadline(d.CmdTimeout)
			if err := c.StartTLS(&tls.Config{ServerName: t.host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil {
				return fail("4.7.0", "STARTTLS: "+err.Error()), true, true
			}
			res.TLS = "opportunistic"
			if st, ok := c.TLSConnectionState(); ok && verifyPeer(st, t.host) == nil {
				res.TLS = "verified"
			}
		}
	}

	deadline(d.CmdTimeout)
	if err := c.Mail(j.MailFrom); err != nil {
		r := fromError(err, t.host, res.TLS)
		return r, !isReply(err), false
	}
	deadline(d.CmdTimeout)
	if err := c.Rcpt(j.Rcpt); err != nil {
		r := fromError(err, t.host, res.TLS)
		return r, !isReply(err), false
	}

	// DATA is driven by hand so the final reply text (the receiver's queue
	// ID) is kept and a lost reply can be told apart from a refusal.
	deadline(d.CmdTimeout)
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		return fail("4.4.2", "DATA: "+err.Error()), true, false
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(3)
	c.Text.EndResponse(id)
	if err != nil {
		r := fromError(err, t.host, res.TLS)
		return r, !isReply(err), false
	}

	deadline(d.DataTimeout)
	dw := c.Text.DotWriter()
	if _, err := dw.Write(j.Raw); err != nil {
		// The terminating dot never left, so the server cannot have accepted it.
		return fail("4.4.2", "connection lost while sending the message body: "+err.Error()), false, false
	}
	if err := dw.Close(); err != nil {
		return Result{Outcome: protocol.OutcomeUnknown, Enhanced: "4.4.2", MXHost: t.host, TLS: res.TLS,
			Response: "connection lost while finishing the message; the server may have accepted it: " + err.Error()}, false, false
	}
	code, msg, err := c.Text.ReadResponse(2)
	if err != nil {
		if isReply(err) {
			return fromError(err, t.host, res.TLS), false, false
		}
		return Result{Outcome: protocol.OutcomeUnknown, Enhanced: "4.4.2", MXHost: t.host, TLS: res.TLS,
			Response: "no final reply after the message was sent; the server may have accepted it: " + err.Error()}, false, false
	}
	r := fromReply(code, msg)
	r.MXHost, r.TLS = t.host, res.TLS
	deadline(30 * time.Second)
	c.Quit()
	return r, false, false
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

func verifyPeer(st tls.ConnectionState, host string) error {
	if len(st.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	opts := x509.VerifyOptions{DNSName: host, Intermediates: x509.NewCertPool()}
	for _, c := range st.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := st.PeerCertificates[0].Verify(opts)
	return err
}
