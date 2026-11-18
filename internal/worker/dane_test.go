package worker

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/openmailwire/openmailwire/internal/smtptest"
)

func tlsaFor(usage, selector, mtype uint8, c *x509.Certificate) TLSARecord {
	in := c.Raw
	if selector == SelectorSPKI {
		in = c.RawSubjectPublicKeyInfo
	}
	data := in
	switch mtype {
	case MatchSHA256:
		h := sha256.Sum256(in)
		data = h[:]
	case MatchSHA512:
		h := sha512.Sum512(in)
		data = h[:]
	}
	return TLSARecord{Usage: usage, Selector: selector, MatchingType: mtype, Data: data}
}

func TestTLSARecordUsable(t *testing.T) {
	ok := []TLSARecord{
		{3, 1, 1, make([]byte, 32)}, {3, 0, 2, make([]byte, 64)}, {2, 0, 0, []byte{1}}, {2, 1, 1, make([]byte, 32)},
	}
	for _, r := range ok {
		if !r.Usable() {
			t.Errorf("%v should be usable", r)
		}
	}
	bad := []TLSARecord{
		{0, 0, 1, make([]byte, 32)}, // PKIX-TA: not for SMTP
		{1, 1, 1, make([]byte, 32)}, // PKIX-EE: not for SMTP
		{3, 2, 1, make([]byte, 32)}, // unknown selector
		{3, 1, 3, make([]byte, 32)}, // unknown matching type
		{3, 1, 1, make([]byte, 31)}, // wrong digest length
		{3, 1, 2, make([]byte, 32)},
		{3, 0, 0, nil},
	}
	for _, r := range bad {
		if r.Usable() {
			t.Errorf("%v should not be usable", r)
		}
	}
}

func TestVerifyDANE(t *testing.T) {
	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	self, err := smtptest.SelfSigned("whatever.invalid")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := smtptest.SelfSigned("other.invalid")
	issued, err := ca.Issue("mx.example.test")
	if err != nil {
		t.Fatal(err)
	}
	domainCert, _ := ca.Issue("example.test")
	parse := func(raw [][]byte) []*x509.Certificate {
		var out []*x509.Certificate
		for _, b := range raw {
			c, err := x509.ParseCertificate(b)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		return out
	}
	selfChain := parse(self.Certificate)
	issuedChain := parse(issued.Certificate)
	names := []string{"mx.example.test", "example.test"}

	// DANE-EE: every selector and matching type; names and issuer do not matter.
	for _, sel := range []uint8{SelectorCert, SelectorSPKI} {
		for _, mt := range []uint8{MatchExact, MatchSHA256, MatchSHA512} {
			r := tlsaFor(DANEEE, sel, mt, selfChain[0])
			if err := VerifyDANE(selfChain, []TLSARecord{r}, names); err != nil {
				t.Errorf("DANE-EE %d %d: %v", sel, mt, err)
			}
		}
	}
	if err := VerifyDANE(selfChain, []TLSARecord{tlsaFor(DANEEE, SelectorSPKI, MatchSHA256, other.Leaf)}, names); err == nil {
		t.Error("DANE-EE matched another certificate")
	}
	// One matching record among several is enough.
	recs := []TLSARecord{tlsaFor(DANEEE, SelectorSPKI, MatchSHA256, other.Leaf), tlsaFor(DANEEE, SelectorCert, MatchSHA512, selfChain[0])}
	if err := VerifyDANE(selfChain, recs, names); err != nil {
		t.Errorf("second record should match: %v", err)
	}

	// DANE-TA: the CA in the chain matches, and the leaf must chain to it
	// with a name we expect.
	for _, sel := range []uint8{SelectorCert, SelectorSPKI} {
		r := tlsaFor(DANETA, sel, MatchSHA256, ca.Cert)
		if err := VerifyDANE(issuedChain, []TLSARecord{r}, names); err != nil {
			t.Errorf("DANE-TA selector %d: %v", sel, err)
		}
	}
	ta := []TLSARecord{tlsaFor(DANETA, SelectorCert, MatchSHA256, ca.Cert)}
	if err := VerifyDANE(issuedChain, ta, []string{"mx.elsewhere.test"}); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("DANE-TA with the wrong name: %v", err)
	}
	if err := VerifyDANE(parse(domainCert.Certificate), ta, names); err != nil {
		t.Errorf("DANE-TA: the recipient domain is a valid name too: %v", err)
	}
	if err := VerifyDANE(selfChain, ta, names); err == nil {
		t.Error("DANE-TA accepted a certificate outside the trust anchor")
	}
	// A DANE-TA record must not be read as DANE-EE for the leaf.
	if err := VerifyDANE(issuedChain, []TLSARecord{tlsaFor(DANETA, SelectorSPKI, MatchSHA256, issuedChain[0])}, []string{"nope.test"}); err == nil {
		t.Error("DANE-TA on the leaf skipped the name check")
	}
	if err := VerifyDANE(nil, ta, names); err == nil {
		t.Error("no certificate accepted")
	}
}

// dnssecServer is a local resolver that answers A and TLSA questions from
// fixed data and sets the AD bit only for names marked secure.
type dnssecServer struct {
	addr string
	srv  *dns.Server

	mu       sync.Mutex
	a        map[string]string // name -> IPv4
	tlsa     map[string][]TLSARecord
	secure   map[string]bool
	servfail map[string]bool
	queries  []string // "TYPE name"
	noDO     int      // queries without the DO bit
}

func startDNSSEC(t *testing.T) *dnssecServer {
	t.Helper()
	s := &dnssecServer{a: map[string]string{}, tlsa: map[string][]TLSARecord{}, secure: map[string]bool{}, servfail: map[string]bool{}}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.addr = pc.LocalAddr().String()
	started := make(chan struct{})
	s.srv = &dns.Server{PacketConn: pc, Handler: s, NotifyStartedFunc: func() { close(started) }}
	go s.srv.ActivateAndServe()
	<-started
	t.Cleanup(func() { s.srv.Shutdown() })
	return s
}

func (s *dnssecServer) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	s.mu.Lock()
	s.queries = append(s.queries, dns.TypeToString[q.Qtype]+" "+name)
	if opt := r.IsEdns0(); opt == nil || !opt.Do() {
		s.noDO++
	}
	m := new(dns.Msg)
	m.SetReply(r)
	m.AuthenticatedData = s.secure[name]
	switch {
	case s.servfail[name]:
		m.Rcode = dns.RcodeServerFailure
		m.AuthenticatedData = false
	case q.Qtype == dns.TypeA && s.a[name] != "":
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(s.a[name])})
	case q.Qtype == dns.TypeTLSA && len(s.tlsa[name]) > 0:
		for _, rec := range s.tlsa[name] {
			m.Answer = append(m.Answer, &dns.TLSA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTLSA, Class: dns.ClassINET, Ttl: 60},
				Usage: rec.Usage, Selector: rec.Selector, MatchingType: rec.MatchingType, Certificate: hex.EncodeToString(rec.Data)})
		}
	default:
		m.Rcode = dns.RcodeNameError
	}
	s.mu.Unlock()
	w.WriteMsg(m)
}

func (s *dnssecServer) asked(entry string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.queries {
		if q == entry {
			return true
		}
	}
	return false
}

func TestDANEResolver(t *testing.T) {
	s := startDNSSEC(t)
	usable := TLSARecord{Usage: 3, Selector: 1, MatchingType: 1, Data: make([]byte, 32)}
	pkix := TLSARecord{Usage: 1, Selector: 1, MatchingType: 1, Data: make([]byte, 32)}
	s.mu.Lock() // the server goroutine reads these maps
	for _, h := range []string{"mx.signed.test.", "mx.insecure-tlsa.test.", "mx.bogus.test.", "mx.no-tlsa.test.", "mx.pkix-only.test."} {
		s.a[h], s.secure[h] = "127.0.0.1", true
	}
	s.tlsa["_25._tcp.mx.signed.test."] = []TLSARecord{usable, pkix}
	s.secure["_25._tcp.mx.signed.test."] = true
	s.tlsa["_25._tcp.mx.insecure-tlsa.test."] = []TLSARecord{usable} // answered, but not authenticated
	s.servfail["_25._tcp.mx.bogus.test."] = true
	s.secure["_25._tcp.mx.no-tlsa.test."] = true // authenticated denial
	s.tlsa["_25._tcp.mx.pkix-only.test."] = []TLSARecord{pkix}
	s.secure["_25._tcp.mx.pkix-only.test."] = true
	s.a["mx.unsigned.test."] = "127.0.0.1"
	s.tlsa["_25._tcp.mx.unsigned.test."] = []TLSARecord{usable}
	s.secure["_25._tcp.mx.unsigned.test."] = true
	s.mu.Unlock()

	r := &DANEResolver{Server: s.addr}
	ctx := context.Background()
	recs, err := r.TLSA(ctx, "MX.Signed.Test")
	if err != nil || len(recs) != 1 || recs[0].Usage != 3 {
		t.Fatalf("signed host: %v %v (only the DANE-EE record is usable)", recs, err)
	}
	for _, h := range []string{"mx.insecure-tlsa.test", "mx.no-tlsa.test", "mx.pkix-only.test", "mx.unsigned.test"} {
		if recs, err := r.TLSA(ctx, h); err != nil || recs != nil {
			t.Errorf("%s: want no DANE, got %v %v", h, recs, err)
		}
	}
	if s.asked("TLSA _25._tcp.mx.unsigned.test.") {
		t.Error("TLSA was asked for a host whose address is not DNSSEC-secure")
	}
	if _, err := r.TLSA(ctx, "mx.bogus.test"); err == nil || !strings.Contains(err.Error(), "SERVFAIL") {
		t.Errorf("bogus TLSA must be an error: %v", err)
	}
	s.mu.Lock()
	if s.noDO != 0 {
		t.Errorf("%d queries went out without the DO bit", s.noDO)
	}
	s.mu.Unlock()
	if _, err := (&DANEResolver{Server: "127.0.0.1:1", Timeout: 200 * time.Millisecond}).TLSA(ctx, "mx.signed.test"); err == nil {
		t.Error("an unreachable resolver must be an error")
	}
}
