package worker

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/smtptest"
)

func relayTo(srv *smtptest.Server, user, pass string) *Relay {
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	return &Relay{Host: host, Port: port, Username: user, Password: pass, EHLO: "relay-worker.test", DialTimeout: time.Second}
}

func TestRelay(t *testing.T) {
	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	smarthost, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer smarthost.Close()
	smarthost.EnableSTARTTLS(smtptest.TLSConfig(cert))
	smarthost.RequireTLS()
	smarthost.RequireAuth("omw", "s3cret")
	smarthost.ReplyToRcpt("gone@example.test", "550 5.1.1 no such user")
	ctx := context.Background()

	r := relayTo(smarthost, "omw", "s3cret")
	r.RootCAs = ca.Pool
	j := job("someone@example.test")
	res := r.Deliver(ctx, j)
	if res.Outcome != protocol.OutcomeDelivered || res.Code != 250 || res.TLS != "verified" || res.MXHost != "127.0.0.1" {
		t.Fatalf("relay delivery: %+v", res)
	}
	msgs := smarthost.Messages()
	if len(msgs) != 1 || !msgs[0].TLS || msgs[0].AuthUser != "omw" || msgs[0].MailFrom != j.MailFrom || msgs[0].Rcpts[0] != j.Rcpt {
		t.Fatalf("smarthost got %+v", msgs)
	}
	if !bytes.Equal(msgs[0].Data, j.Raw) {
		t.Fatalf("the message changed on the way:\n%q\n%q", msgs[0].Data, j.Raw)
	}

	// The relay's replies to MAIL, RCPT and DATA map as for direct delivery.
	if res := r.Deliver(ctx, job("gone@example.test")); res.Outcome != protocol.OutcomeBounced || res.Enhanced != "5.1.1" {
		t.Errorf("RCPT 550: %+v", res)
	}

	// Failures before MAIL FROM are about the relay: always deferred.
	bad := relayTo(smarthost, "omw", "wrong")
	bad.RootCAs = ca.Pool
	if res := bad.Deliver(ctx, j); res.Outcome != protocol.OutcomeDeferred || res.Code != 535 || !strings.Contains(res.Response, "refused AUTH") {
		t.Errorf("wrong password: %+v", res)
	}
	untrusted := relayTo(smarthost, "omw", "s3cret") // system roots do not know the test CA
	if res := untrusted.Deliver(ctx, j); res.Outcome != protocol.OutcomeDeferred || !strings.Contains(res.Response, "STARTTLS") {
		t.Errorf("untrusted certificate: %+v", res)
	}
	plain, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if res := relayTo(plain, "", "").Deliver(ctx, j); res.Outcome != protocol.OutcomeDeferred || !strings.Contains(res.Response, "does not offer STARTTLS") {
		t.Errorf("no STARTTLS: %+v", res)
	}
	noAuth, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer noAuth.Close()
	noAuth.EnableSTARTTLS(smtptest.TLSConfig(cert))
	nr := relayTo(noAuth, "omw", "s3cret")
	nr.RootCAs = ca.Pool
	if res := nr.Deliver(ctx, j); res.Outcome != protocol.OutcomeDeferred || !strings.Contains(res.Response, "AUTH PLAIN") {
		t.Errorf("no AUTH offered: %+v", res)
	}
	if len(smarthost.Messages()) != 1 || len(plain.Messages()) != 0 || len(noAuth.Messages()) != 0 {
		t.Error("a refused relay session still sent the message")
	}
	closed := relayTo(plain, "", "")
	closed.Port = 1
	if res := closed.Deliver(ctx, j); res.Outcome != protocol.OutcomeDeferred || res.Enhanced != "4.4.1" {
		t.Errorf("connection refused: %+v", res)
	}
}

func TestRelayImplicitTLS(t *testing.T) {
	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smtptest.StartImplicitTLS(smtptest.TLSConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.RequireAuth("omw", "s3cret")
	r := relayTo(srv, "omw", "s3cret")
	r.RootCAs, r.ImplicitTLS = ca.Pool, true
	if res := r.Deliver(context.Background(), job("x@example.test")); res.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("implicit TLS: %+v", res)
	}
	if m := srv.Messages(); len(m) != 1 || !m[0].TLS || m[0].AuthUser != "omw" {
		t.Fatalf("got %+v", m)
	}
}
