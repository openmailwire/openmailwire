package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/smtptest"
)

type staticSTS struct{ p *STSPolicy }

func (s staticSTS) Policy(context.Context, string) (*STSPolicy, error) { return s.p, nil }

type fakeTLSA struct {
	recs map[string][]TLSARecord
	err  error
}

func (f fakeTLSA) TLSA(_ context.Context, host string) ([]TLSARecord, error) {
	return f.recs[host], f.err
}

// mxServers are three receivers for mx.example.test: one with a certificate
// from a CA the deliverer trusts, one with a self-signed certificate, and one
// without STARTTLS.
type mxServers struct {
	ca                   *smtptest.CA
	good, selfSig, noTLS *smtptest.Server
	selfCert             tls.Certificate
}

func startMXServers(t *testing.T) *mxServers {
	t.Helper()
	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	good, err := ca.Issue("mx.example.test")
	if err != nil {
		t.Fatal(err)
	}
	self, err := smtptest.SelfSigned("mx.example.test")
	if err != nil {
		t.Fatal(err)
	}
	m := &mxServers{ca: ca, selfCert: self}
	for _, s := range []**smtptest.Server{&m.good, &m.selfSig, &m.noTLS} {
		srv, err := smtptest.Start()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		*s = srv
	}
	m.good.EnableSTARTTLS(smtptest.TLSConfig(good))
	m.selfSig.EnableSTARTTLS(smtptest.TLSConfig(self))
	return m
}

// deliverer sends to srv as if it were the MX host of example.test.
func (m *mxServers) deliverer(srv *smtptest.Server, sts *STSPolicy, tlsa TLSASource) *Deliverer {
	_, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	d := &Deliverer{
		Resolver: fakeResolver{
			mx:  map[string][]*net.MX{"example.test": {{Host: "mx.example.test.", Pref: 10}}},
			ips: map[string][]net.IPAddr{"mx.example.test": {{IP: net.ParseIP("127.0.0.1")}}},
		},
		Port: port, EHLO: "worker.test", IPv4Only: true, DialTimeout: time.Second, RootCAs: m.ca.Pool, DANE: tlsa,
	}
	if sts != nil {
		d.STS = staticSTS{sts}
	}
	return d
}

func policy(mode string, mx ...string) *STSPolicy {
	return &STSPolicy{ID: "1", Mode: mode, MX: mx, MaxAge: time.Hour}
}

func TestDeliverMTASTS(t *testing.T) {
	m := startMXServers(t)
	ctx := context.Background()
	cases := []struct {
		name      string
		srv       *smtptest.Server
		sts       *STSPolicy
		outcome   string
		tls       string
		enhanced  string
		response  string // substring
		tlsPolicy string // substring; "" means none
	}{
		{"enforce, valid certificate", m.good, policy(STSEnforce, "mx.example.test"), protocol.OutcomeDelivered, "verified+mta-sts", "2.0.0", "", ""},
		{"enforce, wildcard", m.good, policy(STSEnforce, "*.example.test"), protocol.OutcomeDelivered, "verified+mta-sts", "2.0.0", "", ""},
		{"enforce, MX not in policy", m.good, policy(STSEnforce, "*.elsewhere.test"), protocol.OutcomeDeferred, "", "4.7.5", "MTA-STS policy of example.test (enforce): MX host mx.example.test is not one of", ""},
		{"enforce, untrusted certificate", m.selfSig, policy(STSEnforce, "mx.example.test"), protocol.OutcomeDeferred, "opportunistic", "4.7.5", "MTA-STS policy of example.test (enforce): the certificate is not valid", ""},
		{"enforce, no STARTTLS", m.noTLS, policy(STSEnforce, "mx.example.test"), protocol.OutcomeDeferred, "none", "4.7.5", "does not offer STARTTLS", ""},
		{"testing, untrusted certificate", m.selfSig, policy(STSTesting, "mx.example.test"), protocol.OutcomeDelivered, "opportunistic", "2.0.0", "", "(testing): TLS with mx.example.test was \"opportunistic\""},
		{"testing, MX not in policy", m.good, policy(STSTesting, "other.example.test"), protocol.OutcomeDelivered, "verified", "2.0.0", "", "is not one of other.example.test"},
		{"testing, all good", m.good, policy(STSTesting, "mx.example.test"), protocol.OutcomeDelivered, "verified", "2.0.0", "", ""},
		{"none", m.noTLS, policy(STSNone), protocol.OutcomeDelivered, "none", "2.0.0", "", ""},
		{"no policy", m.noTLS, nil, protocol.OutcomeDelivered, "none", "2.0.0", "", ""},
	}
	for _, c := range cases {
		before := len(c.srv.Messages())
		res := m.deliverer(c.srv, c.sts, nil).Deliver(ctx, job("rcpt@example.test"))
		if res.Outcome != c.outcome || res.TLS != c.tls || res.Enhanced != c.enhanced ||
			!strings.Contains(res.Response, c.response) || (c.tlsPolicy == "") != (res.TLSPolicy == "") ||
			!strings.Contains(res.TLSPolicy, c.tlsPolicy) {
			t.Errorf("%s: got %+v", c.name, res)
		}
		got := len(c.srv.Messages()) - before
		if want := map[bool]int{true: 1, false: 0}[c.outcome == protocol.OutcomeDelivered]; got != want {
			t.Errorf("%s: the server received %d messages, want %d", c.name, got, want)
		}
	}
}

// A host refused by policy explains the outcome better than a later host
// that could not be reached at all.
func TestDeliverPolicyFailureWins(t *testing.T) {
	m := startMXServers(t)
	d := m.deliverer(m.selfSig, policy(STSEnforce, "*.example.test"), nil)
	d.Resolver = fakeResolver{
		mx: map[string][]*net.MX{"example.test": {{Host: "a.example.test.", Pref: 5}, {Host: "b.example.test.", Pref: 10}}},
		ips: map[string][]net.IPAddr{
			"a.example.test": {{IP: net.ParseIP("127.0.0.1")}},
			"b.example.test": {{IP: net.ParseIP("127.0.0.2")}}, // nothing listens there
		},
	}
	res := d.Deliver(context.Background(), job("rcpt@example.test"))
	if res.Outcome != protocol.OutcomeDeferred || res.Enhanced != "4.7.5" || res.MXHost != "a.example.test" ||
		!strings.Contains(res.Response, "the certificate is not valid for a.example.test") {
		t.Fatalf("%+v", res)
	}
}

func TestDeliverDANE(t *testing.T) {
	m := startMXServers(t)
	ctx := context.Background()
	pin := tlsaFor(DANEEE, SelectorSPKI, MatchSHA256, m.selfCert.Leaf)
	other, _ := smtptest.SelfSigned("other.test")
	wrong := tlsaFor(DANEEE, SelectorSPKI, MatchSHA256, other.Leaf)
	recs := func(r ...TLSARecord) TLSASource { return fakeTLSA{recs: map[string][]TLSARecord{"mx.example.test": r}} }

	// DANE-EE pins the self-signed certificate: PKIX does not matter.
	if res := m.deliverer(m.selfSig, nil, recs(pin)).Deliver(ctx, job("a@example.test")); res.Outcome != protocol.OutcomeDelivered || res.TLS != "dane" {
		t.Errorf("matching TLSA: %+v", res)
	}
	// A mismatch, a server without STARTTLS, and a failed lookup all defer.
	for name, c := range map[string]struct {
		srv  *smtptest.Server
		tlsa TLSASource
		want string
	}{
		"mismatch":     {m.selfSig, recs(wrong), "matches none of the TLSA records"},
		"no STARTTLS":  {m.noTLS, recs(pin), "does not offer STARTTLS"},
		"lookup error": {m.good, fakeTLSA{err: errors.New("TLSA lookup: SERVFAIL")}, "DANE: cannot tell whether mx.example.test requires TLS"},
	} {
		before := len(c.srv.Messages())
		res := m.deliverer(c.srv, nil, c.tlsa).Deliver(ctx, job("a@example.test"))
		if res.Outcome != protocol.OutcomeDeferred || res.Enhanced != "4.7.5" || !strings.Contains(res.Response, c.want) {
			t.Errorf("%s: %+v", name, res)
		}
		if len(c.srv.Messages()) != before {
			t.Errorf("%s: the message was sent anyway", name)
		}
	}
	// No TLSA records: opportunistic as usual.
	if res := m.deliverer(m.noTLS, nil, recs()).Deliver(ctx, job("a@example.test")); res.Outcome != protocol.OutcomeDelivered || res.TLS != "none" {
		t.Errorf("no TLSA records: %+v", res)
	}
	// DANE takes precedence over MTA-STS for a host with TLSA records.
	res := m.deliverer(m.selfSig, policy(STSEnforce, "*.elsewhere.test"), recs(pin)).Deliver(ctx, job("a@example.test"))
	if res.Outcome != protocol.OutcomeDelivered || res.TLS != "dane" {
		t.Errorf("DANE over MTA-STS: %+v", res)
	}
	// The override (development) ignores TLS policies.
	d := m.deliverer(m.noTLS, policy(STSEnforce, "*.elsewhere.test"), recs(pin))
	d.Override = m.noTLS.Addr
	if res := d.Deliver(ctx, job("a@example.test")); res.Outcome != protocol.OutcomeDelivered {
		t.Errorf("override: %+v", res)
	}
}

// TestDeliverDANEResolver runs the deliverer against the local validating
// resolver: the whole DANE path, from the TLSA query to the certificate.
func TestDeliverDANEResolver(t *testing.T) {
	m := startMXServers(t)
	s := startDNSSEC(t)
	s.mu.Lock()
	s.a["mx.example.test."], s.secure["mx.example.test."] = "127.0.0.1", true
	s.tlsa["_25._tcp.mx.example.test."] = []TLSARecord{tlsaFor(DANEEE, SelectorCert, MatchSHA512, m.selfCert.Leaf)}
	s.secure["_25._tcp.mx.example.test."] = true
	s.mu.Unlock()
	res := m.deliverer(m.selfSig, nil, &DANEResolver{Server: s.addr}).Deliver(context.Background(), job("a@example.test"))
	if res.Outcome != protocol.OutcomeDelivered || res.TLS != "dane" {
		t.Fatalf("%+v", res)
	}
	// Once the answer is no longer authenticated, DANE does not apply.
	s.mu.Lock()
	s.secure["_25._tcp.mx.example.test."] = false
	s.mu.Unlock()
	res = m.deliverer(m.good, nil, &DANEResolver{Server: s.addr}).Deliver(context.Background(), job("a@example.test"))
	if res.Outcome != protocol.OutcomeDelivered || res.TLS != "verified" {
		t.Fatalf("unauthenticated TLSA: %+v", res)
	}
}
