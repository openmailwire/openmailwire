package worker

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/smtptest"
)

func job(rcpt string) protocol.Job {
	return protocol.Job{
		AttemptID: "00000000-0000-4000-8000-000000000001", MailFrom: "b-1@bounce.acme.test",
		Rcpt: rcpt, RcptDomain: "example.test",
		Raw: []byte("From: a@acme.test\r\nTo: " + rcpt + "\r\nSubject: hi\r\n\r\nhello\r\n.leading dot\r\n"),
	}
}

func TestDeliverOutcomes(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.ReplyToRcpt("gone@example.test", "550 5.1.1 no such user")
	srv.ReplyToRcpt("busy@example.test", "451 4.7.1 try again later")
	srv.DropAfterData("drop@example.test")

	d := &Deliverer{Override: srv.Addr, EHLO: "worker.test"}
	cases := map[string]struct {
		outcome, enhanced string
		code              int
	}{
		"ok@example.test":   {protocol.OutcomeDelivered, "2.0.0", 250},
		"gone@example.test": {protocol.OutcomeBounced, "5.1.1", 550},
		"busy@example.test": {protocol.OutcomeDeferred, "4.7.1", 451},
		"drop@example.test": {protocol.OutcomeUnknown, "4.4.2", 0},
	}
	for rcpt, want := range cases {
		res := d.Deliver(context.Background(), job(rcpt))
		if res.Outcome != want.outcome || res.Enhanced != want.enhanced || res.Code != want.code {
			t.Errorf("%s: got %+v", rcpt, res)
		}
	}
	msgs := srv.Messages()
	if len(msgs) != 1 || msgs[0].MailFrom != "b-1@bounce.acme.test" {
		t.Fatalf("server received %+v", msgs)
	}
	if want := "hello\r\n.leading dot\r\n"; string(msgs[0].Data[len(msgs[0].Data)-len(want):]) != want {
		t.Fatalf("dot-stuffing round trip failed: %q", msgs[0].Data)
	}
}

type fakeResolver struct {
	mx  map[string][]*net.MX
	ips map[string][]net.IPAddr
}

func (f fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if v, ok := f.mx[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if v, ok := f.ips[host]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestDeliverViaMX(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	local := []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}

	r := fakeResolver{
		mx: map[string][]*net.MX{
			"example.test": {{Host: "dead.example.test.", Pref: 5}, {Host: "mx.example.test.", Pref: 10}},
			"nullmx.test":  {{Host: ".", Pref: 0}},
		},
		ips: map[string][]net.IPAddr{
			"dead.example.test": {{IP: net.ParseIP("127.0.0.2")}}, // nothing listens there
			"mx.example.test":   local,
			"implicit.test":     local,
		},
	}
	d := &Deliverer{Resolver: r, Port: port, EHLO: "worker.test", IPv4Only: true, DialTimeout: time.Second}

	if res := d.Deliver(context.Background(), job("ok@example.test")); res.Outcome != protocol.OutcomeDelivered || res.MXHost != "mx.example.test" {
		t.Errorf("fallback to second MX: %+v", res)
	}
	j := job("x@implicit.test")
	j.RcptDomain = "implicit.test"
	if res := d.Deliver(context.Background(), j); res.Outcome != protocol.OutcomeDelivered {
		t.Errorf("implicit MX: %+v", res)
	}
	j.RcptDomain = "nullmx.test"
	if res := d.Deliver(context.Background(), j); res.Outcome != protocol.OutcomeBounced || res.Enhanced != "5.1.10" {
		t.Errorf("null MX: %+v", res)
	}
	j.RcptDomain = "nowhere.test"
	if res := d.Deliver(context.Background(), j); res.Outcome != protocol.OutcomeBounced || res.Enhanced != "5.1.2" {
		t.Errorf("nonexistent domain: %+v", res)
	}
}
