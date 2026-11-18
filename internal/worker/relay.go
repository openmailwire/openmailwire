package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Relay hands every message to one smarthost (another provider) instead of
// the recipients' mail servers. It is what a fallback relay worker runs.
//
// TLS is required and the smarthost's certificate must be valid for Host,
// because the session carries credentials. The message goes through
// unchanged, so its DKIM signature stays valid.
type Relay struct {
	Host        string
	Port        int
	Username    string // AUTH PLAIN when set
	Password    string
	ImplicitTLS bool // TLS from the first byte (port 465) instead of STARTTLS
	EHLO        string
	SourceIP    string
	RootCAs     *x509.CertPool // nil: the system roots
	DialTimeout time.Duration
	CmdTimeout  time.Duration
	DataTimeout time.Duration
}

func (r *Relay) setDefaults() {
	if r.Port == 0 {
		r.Port = 587
	}
	if r.DialTimeout == 0 {
		r.DialTimeout = 30 * time.Second
	}
	if r.CmdTimeout == 0 {
		r.CmdTimeout = 2 * time.Minute
	}
	if r.DataTimeout == 0 {
		r.DataTimeout = 5 * time.Minute
	}
	if r.EHLO == "" {
		r.EHLO = "localhost"
	}
}

// Deliver sends one message through the smarthost. Failures before MAIL
// FROM (connection, TLS, authentication) are about the relay, not the
// recipient, so they are always deferred. MAIL, RCPT and DATA replies map
// to outcomes as for direct delivery.
func (r *Relay) Deliver(ctx context.Context, j protocol.Job) Result {
	r.setDefaults()
	addr := net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
	fail := func(enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: "relay " + addr + ": " + msg, MXHost: r.Host}
	}
	conn, err := dial(ctx, r.SourceIP, addr, r.DialTimeout)
	if err != nil {
		return fail("4.4.1", "connect: "+err.Error())
	}
	defer conn.Close()
	deadline := deadliner(ctx, conn)
	tlsConfig := &tls.Config{ServerName: r.Host, RootCAs: r.RootCAs, MinVersion: tls.VersionTLS12}

	deadline(r.CmdTimeout)
	if r.ImplicitTLS {
		tc := tls.Client(conn, tlsConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail("4.7.0", "TLS: "+err.Error())
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, r.Host)
	if err != nil {
		res := fromError(err, r.Host, "")
		res.Outcome = protocol.OutcomeDeferred
		res.Response = "relay " + addr + " greeting: " + res.Response
		return res
	}
	defer c.Close()
	deadline(r.CmdTimeout)
	if err := c.Hello(r.EHLO); err != nil {
		return fail("4.4.2", "EHLO: "+err.Error())
	}
	if !r.ImplicitTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fail("4.7.0", "does not offer STARTTLS, which is required")
		}
		deadline(r.CmdTimeout)
		if err := c.StartTLS(tlsConfig); err != nil {
			return fail("4.7.0", "STARTTLS: "+err.Error())
		}
	}
	if r.Username != "" {
		ok, mechs := c.Extension("AUTH")
		if !ok || !hasWord(mechs, "PLAIN") {
			return fail("4.7.0", "does not offer AUTH PLAIN")
		}
		deadline(r.CmdTimeout)
		if err := c.Auth(smtp.PlainAuth("", r.Username, r.Password, r.Host)); err != nil {
			res := fromError(err, r.Host, "verified")
			res.Outcome = protocol.OutcomeDeferred
			res.Response = "relay " + addr + " refused AUTH: " + res.Response
			return res
		}
	}
	res, _ := transact(c, j, r.Host, "verified", deadline, r.CmdTimeout, r.DataTimeout)
	return res
}

func hasWord(list, word string) bool {
	for _, w := range strings.Fields(list) {
		if strings.EqualFold(w, word) {
			return true
		}
	}
	return false
}
