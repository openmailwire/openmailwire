package mailauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

// fakeDNS answers from maps and counts the questions.
type fakeDNS struct {
	txt     map[string][]string
	ip      map[string][]string
	mx      map[string][]string
	ptr     map[string][]string
	queries int
	fail    map[string]bool // names that answer SERVFAIL
}

func newDNS() *fakeDNS {
	return &fakeDNS{txt: map[string][]string{}, ip: map[string][]string{}, mx: map[string][]string{}, ptr: map[string][]string{}, fail: map[string]bool{}}
}

func nx(name string) error { return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true} }

func (d *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	d.queries++
	if d.fail[name] {
		return nil, &net.DNSError{Err: "server misbehaving", Name: name, IsTemporary: true}
	}
	if v, ok := d.txt[name]; ok {
		return v, nil
	}
	return nil, nx(name)
}

func (d *fakeDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	d.queries++
	var out []*net.MX
	for _, h := range d.mx[name] {
		out = append(out, &net.MX{Host: h + ".", Pref: 10})
	}
	if out == nil {
		return nil, nx(name)
	}
	return out, nil
}

func (d *fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	d.queries++
	var out []net.IPAddr
	for _, s := range d.ip[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	if out == nil {
		return nil, nx(host)
	}
	return out, nil
}

func (d *fakeDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	d.queries++
	if v, ok := d.ptr[addr]; ok {
		return v, nil
	}
	return nil, nx(addr)
}

func spf(t *testing.T, d *fakeDNS, ip, domain string) SPFResult {
	t.Helper()
	return CheckSPF(context.Background(), d, net.ParseIP(ip), domain, "user@"+domain, "mx.client.test")
}

func TestSPF(t *testing.T) {
	d := newDNS()
	d.txt["ip.test"] = []string{"v=spf1 ip4:192.0.2.0/24 ip6:2001:db8::/32 -all"}
	d.txt["soft.test"] = []string{"v=spf1 ~all"}
	d.txt["neutral.test"] = []string{"v=spf1 ?all"}
	d.txt["nomatch.test"] = []string{"v=spf1 ip4:10.0.0.1"}
	d.txt["a.test"] = []string{"v=spf1 a a:other.a.test/24 -all"}
	d.ip["a.test"] = []string{"198.51.100.1"}
	d.ip["other.a.test"] = []string{"203.0.113.9"}
	d.txt["mx.test"] = []string{"v=spf1 mx -all"}
	d.mx["mx.test"] = []string{"mail.mx.test"}
	d.ip["mail.mx.test"] = []string{"198.51.100.25"}
	d.txt["inc.test"] = []string{"v=spf1 include:ip.test -all"}
	d.txt["redir.test"] = []string{"v=spf1 redirect=ip.test"}
	d.txt["redir-none.test"] = []string{"v=spf1 redirect=nothing.test"}
	d.txt["two.test"] = []string{"v=spf1 -all", "v=spf1 +all"}
	d.txt["bad.test"] = []string{"v=spf1 ip4:not-an-ip -all"}
	d.txt["unknown.test"] = []string{"v=spf1 foo:bar -all"}
	d.txt["exists.test"] = []string{"v=spf1 exists:%{i}._spf.exists.test -all"}
	d.ip["192.0.2.10._spf.exists.test"] = []string{"127.0.0.2"}
	d.txt["macro.test"] = []string{"v=spf1 exists:%{ir}.%{l1r-}.%{d2}.macro.test -all"}
	d.ip["10.2.0.192.user.macro.test.macro.test"] = []string{"127.0.0.2"}
	d.txt["ptr.test"] = []string{"v=spf1 ptr -all"}
	d.ptr["192.0.2.33"] = []string{"host.ptr.test."}
	d.ip["host.ptr.test"] = []string{"192.0.2.33"}
	d.fail["temp.test"] = true
	d.txt["mod.test"] = []string{"v=spf1 exp=explain.mod.test unknown-mod=x ip4:192.0.2.10 -all"}
	d.txt["v6.test"] = []string{"v=spf1 a//48 -all"}
	d.ip["v6.test"] = []string{"2001:db8:1:2::1"}

	for _, c := range []struct{ ip, domain, want string }{
		{"192.0.2.10", "ip.test", Pass},
		{"198.51.100.10", "ip.test", Fail},
		{"2001:db8::5", "ip.test", Pass},
		{"::ffff:192.0.2.10", "ip.test", Pass},
		{"192.0.2.10", "soft.test", SoftFail},
		{"192.0.2.10", "neutral.test", Neutral},
		{"192.0.2.10", "nomatch.test", Neutral},
		{"198.51.100.1", "a.test", Pass},
		{"203.0.113.200", "a.test", Pass},
		{"203.0.114.1", "a.test", Fail},
		{"198.51.100.25", "mx.test", Pass},
		{"198.51.100.26", "mx.test", Fail},
		{"192.0.2.10", "inc.test", Pass},
		{"198.51.100.10", "inc.test", Fail},
		{"192.0.2.10", "redir.test", Pass},
		{"192.0.2.10", "redir-none.test", PermError},
		{"192.0.2.10", "two.test", PermError},
		{"192.0.2.10", "bad.test", PermError},
		{"192.0.2.10", "unknown.test", PermError},
		{"192.0.2.10", "nothing.test", None},
		{"192.0.2.10", "exists.test", Pass},
		{"192.0.2.11", "exists.test", Fail},
		{"192.0.2.10", "macro.test", Pass},
		{"192.0.2.33", "ptr.test", Pass},
		{"192.0.2.34", "ptr.test", Fail},
		{"192.0.2.10", "temp.test", TempError},
		{"192.0.2.10", "mod.test", Pass},
		{"2001:db8:1:ffff::9", "v6.test", Pass},
		{"2001:db8:2::9", "v6.test", Fail},
	} {
		if got := spf(t, d, c.ip, c.domain); got.Result != c.want {
			t.Errorf("SPF %s from %s = %s (%s), want %s", c.domain, c.ip, got.Result, got.Reason, c.want)
		}
	}
}

// RFC 7208 4.6.4: more than 10 DNS-querying terms, or more than 2 void
// lookups, is a permerror, and loops end.
func TestSPFLimits(t *testing.T) {
	d := newDNS()
	d.txt["loop.test"] = []string{"v=spf1 include:loop.test -all"}
	var incs []string
	for i := 0; i < 11; i++ {
		name := strings.Repeat("x", i+1) + ".test"
		incs = append(incs, "include:"+name)
		d.txt[name] = []string{"v=spf1 -all"}
	}
	d.txt["many.test"] = []string{"v=spf1 " + strings.Join(incs, " ") + " -all"}
	d.txt["void.test"] = []string{"v=spf1 a:n1.test a:n2.test a:n3.test -all"}
	for _, dom := range []string{"loop.test", "many.test", "void.test"} {
		d.queries = 0
		if got := spf(t, d, "192.0.2.1", dom); got.Result != PermError {
			t.Errorf("%s: %s (%s)", dom, got.Result, got.Reason)
		}
		if d.queries > 25 {
			t.Errorf("%s: %d DNS queries", dom, d.queries)
		}
	}
}

func dkimKey(t *testing.T) (ed25519.PrivateKey, string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)
}

func sign(t *testing.T, key ed25519.PrivateKey, domain, selector, msg string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{Domain: domain, Selector: selector, Signer: key,
		HeaderKeys: []string{"From", "To", "Subject"}}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestVerdicts(t *testing.T) {
	key, txt := dkimKey(t)
	d := newDNS()
	d.txt["sel._domainkey.good.test"] = []string{txt}
	d.txt["good.test"] = []string{"v=spf1 ip4:192.0.2.0/24 -all"}
	d.txt["_dmarc.good.test"] = []string{"v=DMARC1; p=reject"}
	d.txt["nodmarc.test"] = []string{"v=spf1 ip4:192.0.2.0/24 -all"}
	d.txt["sel._domainkey.esp.test"] = []string{txt}
	d.txt["_dmarc.relaxed.test"] = []string{"v=DMARC1; p=quarantine; adkim=r"}
	d.txt["sel._domainkey.mail.relaxed.test"] = []string{txt}
	d.txt["_dmarc.strict.test"] = []string{"v=DMARC1; p=none; adkim=s; aspf=s"}
	d.txt["sel._domainkey.mail.strict.test"] = []string{txt}

	msg := func(from string) string {
		return "From: Someone <" + from + ">\r\nTo: agent@acme.test\r\nSubject: hi\r\n\r\nbody\r\n"
	}
	ev := func(ip, mailFrom, from string, raw []byte) Results {
		if raw == nil {
			raw = []byte(msg(from))
		}
		return Evaluate(context.Background(), d, Input{IP: net.ParseIP(ip), HELO: "mx.client.test", MailFrom: mailFrom, Raw: raw, From: from, FromCount: 1},
			5*time.Second, "mx.openmailwire.test")
	}
	cases := []struct {
		name string
		res  Results
		want string
	}{
		{"dkim aligned", ev("198.51.100.1", "x@elsewhere.test", "ceo@good.test", sign(t, key, "good.test", "sel", msg("ceo@good.test"))), Authenticated},
		{"spf aligned", ev("192.0.2.5", "bounce@good.test", "ceo@good.test", nil), Authenticated},
		{"dmarc fail", ev("198.51.100.1", "x@attacker.test", "ceo@good.test", nil), Failed},
		{"spf aligned, no dmarc", ev("192.0.2.5", "a@nodmarc.test", "ceo@nodmarc.test", nil), Authenticated},
		{"spf fail aligned, no dmarc", ev("198.51.100.1", "a@nodmarc.test", "ceo@nodmarc.test", nil), Failed},
		{"unrelated dkim", ev("198.51.100.1", "x@attacker.test", "ceo@nodmarc.test", sign(t, key, "esp.test", "sel", msg("ceo@nodmarc.test"))), Unauthenticated},
		{"nothing at all", ev("198.51.100.1", "", "someone@unknown.test", nil), Unauthenticated},
		{"relaxed subdomain", ev("198.51.100.1", "", "a@relaxed.test", sign(t, key, "mail.relaxed.test", "sel", msg("a@relaxed.test"))), Authenticated},
		{"strict subdomain", ev("198.51.100.1", "", "a@strict.test", sign(t, key, "mail.strict.test", "sel", msg("a@strict.test"))), Failed},
	}
	tampered := sign(t, key, "good.test", "sel", msg("ceo@good.test"))
	tampered = bytes.Replace(tampered, []byte("body"), []byte("evil"), 1)
	cases = append(cases, struct {
		name string
		res  Results
		want string
	}{"tampered body", ev("198.51.100.1", "x@attacker.test", "ceo@good.test", tampered), Failed})

	for _, c := range cases {
		if c.res.Trust != c.want {
			t.Errorf("%s: trust %s, want %s (%s) %+v", c.name, c.res.Trust, c.want, c.res.Reason, c.res)
		}
		if c.res.Header == "" || !strings.HasPrefix(c.res.Header, "mx.openmailwire.test;") || strings.ContainsAny(c.res.Header, "\r\n") {
			t.Errorf("%s: header %q", c.name, c.res.Header)
		}
	}
	if r := cases[0].res; len(r.DKIM) != 1 || r.DKIM[0].Selector != "sel" || !r.DKIM[0].Aligned || r.DKIM[0].Result != Pass || r.DMARC.Policy != "reject" {
		t.Errorf("dkim details: %+v", r)
	}
	if r := cases[9].res; r.DKIM[0].Result == Pass {
		t.Errorf("tampered signature passed: %+v", r.DKIM)
	}
}

// A slow or silent DNS cannot hold a message for longer than the timeout.
func TestEvaluateTimeout(t *testing.T) {
	slow := blockingDNS{}
	start := time.Now()
	r := Evaluate(context.Background(), slow, Input{IP: net.ParseIP("192.0.2.1"), HELO: "a.test", MailFrom: "x@b.test",
		Raw: []byte("From: x@b.test\r\nDKIM-Signature: v=1; a=rsa-sha256; d=b.test; s=s; h=from; bh=x; b=y\r\n\r\nhi\r\n"), From: "x@b.test", FromCount: 1},
		200*time.Millisecond, "mx")
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("evaluation took %s", took)
	}
	if r.Trust != Unauthenticated || r.SPF.Result != TempError {
		t.Fatalf("results: %+v", r)
	}
}

type blockingDNS struct{}

func (blockingDNS) wait(ctx context.Context) error {
	<-ctx.Done()
	return &net.DNSError{Err: "timeout", IsTimeout: true, IsTemporary: true}
}
func (b blockingDNS) LookupTXT(ctx context.Context, _ string) ([]string, error) {
	return nil, b.wait(ctx)
}
func (b blockingDNS) LookupMX(ctx context.Context, _ string) ([]*net.MX, error) {
	return nil, b.wait(ctx)
}
func (b blockingDNS) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	return nil, b.wait(ctx)
}
func (b blockingDNS) LookupAddr(ctx context.Context, _ string) ([]string, error) {
	return nil, b.wait(ctx)
}

func TestOrgDomain(t *testing.T) {
	for in, want := range map[string]string{
		"mail.example.co.uk": "example.co.uk", "a.b.example.com": "example.com", "example.com.": "example.com", "bounce.acme.test": "acme.test",
	} {
		if got := OrgDomain(in); got != want {
			t.Errorf("OrgDomain(%s) = %s, want %s", in, got, want)
		}
	}
}

func FuzzSPFRecord(f *testing.F) {
	f.Add("v=spf1 ip4:192.0.2.0/24 include:%{d}.x.test a:%{ir}.%{v}._spf.%{d2} mx/24//64 ptr exists:%{l1r-}.e.test redirect=r.test -all")
	f.Fuzz(func(t *testing.T, rec string) {
		if len(rec) > 4096 {
			return
		}
		d := newDNS()
		d.txt["f.test"] = []string{rec}
		CheckSPF(context.Background(), d, net.ParseIP("192.0.2.1"), "f.test", "u@f.test", "h.test")
		if d.queries > 60 {
			t.Fatalf("%d queries for %q", d.queries, rec)
		}
	})
}
