package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
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
	Host         string
	Port         int
	Username     string // AUTH PLAIN when set
	Password     string
	ImplicitTLS  bool // TLS from the first byte (port 465) instead of STARTTLS
	EHLO         string
	SourceIP     string
	RootCAs      *x509.CertPool // nil: the system roots
	DialTimeout  time.Duration
	CmdTimeout   time.Duration
	DataTimeout  time.Duration // per block of the message
	FinalTimeout time.Duration // for the reply after the message
}

func (r *Relay) setDefaults() {
	if r.Port == 0 {
		r.Port = 587
	}
	if r.DialTimeout == 0 {
		r.DialTimeout = 30 * time.Second
	}
	if r.CmdTimeout == 0 {
		r.CmdTimeout = 5 * time.Minute
	}
	if r.DataTimeout == 0 {
		r.DataTimeout = 3 * time.Minute
	}
	if r.FinalTimeout == 0 {
		r.FinalTimeout = 10 * time.Minute
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
	fail := func(stage, enhanced, msg string) Result {
		return Result{Outcome: protocol.OutcomeDeferred, Enhanced: enhanced, Response: "relay " + addr + ": " + msg, MXHost: r.Host, Stage: stage}
	}
	refused := func(err error, stage, what string) Result {
		res := fromError(err, r.Host, "", stage)
		res.Outcome = protocol.OutcomeDeferred
		res.Response = "relay " + addr + " " + what + ": " + res.Response
		return res
	}
	conn, err := dial(ctx, r.SourceIP, addr, r.DialTimeout)
	if err != nil {
		return fail("", "4.4.1", "connect: "+err.Error())
	}
	tlsConfig := &tls.Config{ServerName: r.Host, RootCAs: r.RootCAs, MinVersion: tls.VersionTLS12}
	if r.ImplicitTLS {
		tc := tls.Client(conn, tlsConfig)
		conn.SetDeadline(time.Now().Add(r.CmdTimeout))
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fail(protocol.StageGreeting, "4.7.0", "TLS: "+err.Error())
		}
		conn = tc
	}
	c := newSMTPClient(conn, r.Host, r.CmdTimeout)
	c.bind(ctx)
	defer c.close()
	if err := c.greet(); err != nil {
		return refused(err, protocol.StageGreeting, "greeting")
	}
	if err := c.hello(r.EHLO); err != nil {
		return refused(err, protocol.StageEHLO, "EHLO")
	}
	if !r.ImplicitTLS {
		if !c.has("STARTTLS") {
			return fail(protocol.StageSTARTTLS, "4.7.0", "does not offer STARTTLS, which is required")
		}
		if err := c.startTLS(ctx, tlsConfig); err != nil {
			return fail(protocol.StageSTARTTLS, "4.7.0", "STARTTLS: "+err.Error())
		}
		if err := c.hello(r.EHLO); err != nil {
			return refused(err, protocol.StageEHLO, "EHLO after STARTTLS")
		}
	}
	c.mode = "verified"
	if r.Username != "" {
		if !hasWord(c.ext["AUTH"], "PLAIN") {
			return fail(protocol.StageAuth, "4.7.0", "does not offer AUTH PLAIN")
		}
		if err := c.authPlain(r.Username, r.Password); err != nil {
			return refused(err, protocol.StageAuth, "refused AUTH")
		}
	}
	res, _, reusable := transact(c, j, r.Host, r.DataTimeout, r.FinalTimeout)
	if reusable {
		c.cmd(10*time.Second, "QUIT")
	}
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
