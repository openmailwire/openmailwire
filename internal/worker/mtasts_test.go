package worker

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/smtptest"
)

func TestParseSTSRecord(t *testing.T) {
	cases := []struct {
		txts []string
		id   string
		ok   bool
	}{
		{[]string{"v=STSv1; id=20160831085700Z;"}, "20160831085700Z", true},
		{[]string{"v=STSv1;id=abc123"}, "abc123", true},
		{[]string{"some other record", "v=STSv1; id=x1"}, "x1", true},
		{[]string{"v=STSv1; id=a", "v=STSv1; id=b"}, "", false}, // two policies: none
		{[]string{"v=STSv1;"}, "", false},                       // no id
		{[]string{"v=STSv1; id=has-dash"}, "has-dash", false},   // id must be alphanumeric
		{[]string{"v=STSv1; id=" + strings.Repeat("a", 33)}, "", false},
		{[]string{"v=STSv2; id=abc"}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		id, ok := ParseSTSRecord(c.txts)
		if ok != c.ok || (ok && id != c.id) {
			t.Errorf("ParseSTSRecord(%q) = %q, %v", c.txts, id, ok)
		}
	}
}

func TestParseSTSPolicy(t *testing.T) {
	p, err := ParseSTSPolicy([]byte("version: STSv1\r\nmode: enforce\r\nmx: mail.example.com\r\nmx: *.Example.NET.\r\nmax_age: 604800\r\nextension: ignored\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != STSEnforce || p.MaxAge != 7*24*time.Hour || len(p.MX) != 2 || p.MX[0] != "mail.example.com" || p.MX[1] != "*.example.net" {
		t.Fatalf("parsed %+v", p)
	}
	if p, err := ParseSTSPolicy([]byte("version: STSv1\nmode: testing\nmx: mx.example.com\nmax_age: 9999999999\nmode: enforce\n")); err != nil || p.Mode != STSTesting || p.MaxAge != stsMaxAge*time.Second {
		t.Fatalf("LF lines, capped max_age, first mode wins: %+v %v", p, err)
	}
	if p, err := ParseSTSPolicy([]byte("version: STSv1\nmode: none\nmax_age: 86400\n")); err != nil || p.Mode != STSNone {
		t.Fatalf("mode none needs no mx: %+v %v", p, err)
	}
	bad := map[string]string{
		"no version":      "mode: enforce\nmx: a.example.com\nmax_age: 1\n",
		"wrong version":   "version: STSv2\nmode: enforce\nmx: a.example.com\nmax_age: 1\n",
		"no mode":         "version: STSv1\nmx: a.example.com\nmax_age: 1\n",
		"unknown mode":    "version: STSv1\nmode: strict\nmx: a.example.com\nmax_age: 1\n",
		"no mx":           "version: STSv1\nmode: enforce\nmax_age: 1\n",
		"no max_age":      "version: STSv1\nmode: enforce\nmx: a.example.com\n",
		"max_age digits":  "version: STSv1\nmode: enforce\nmx: a.example.com\nmax_age: 99999999999\n",
		"negative age":    "version: STSv1\nmode: enforce\nmx: a.example.com\nmax_age: -1\n",
		"bad mx":          "version: STSv1\nmode: enforce\nmx: a.*.example.com\nmax_age: 1\n",
		"not key: value":  "version: STSv1\nmode enforce\n",
		"html error page": "<html><body>Not found</body></html>",
	}
	for name, body := range bad {
		if p, err := ParseSTSPolicy([]byte(body)); err == nil {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}
}

func TestSTSMatches(t *testing.T) {
	p := &STSPolicy{MX: []string{"mail.example.com", "*.example.net"}}
	cases := map[string]bool{
		"mail.example.com":   true,
		"MAIL.example.com.":  true,
		"mx1.example.com":    false,
		"mx1.example.net":    true,
		"MX2.Example.Net.":   true,
		"example.net":        false, // the wildcard needs one label
		"a.b.example.net":    false, // and only one
		"mx1.example.net.ru": false,
		"badexample.net":     false,
	}
	for host, want := range cases {
		if got := p.Matches(host); got != want {
			t.Errorf("Matches(%q) = %v, want %v", host, got, want)
		}
	}
}

// txtMap answers TXT lookups from a map the test changes.
type txtMap struct {
	mu sync.Mutex
	m  map[string][]string
}

func (r *txtMap) set(name string, v ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(v) == 0 {
		delete(r.m, name)
		return
	}
	r.m[name] = v
}

func (r *txtMap) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.m[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// policyHosts serves MTA-STS policies over HTTPS for several mta-sts.<domain>
// names, all on one local server, and counts the fetches.
type policyHosts struct {
	mu      sync.Mutex
	bodies  map[string]string // host -> body
	fetches map[string]int
}

func (p *policyHosts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.fetches[r.Host]++
	body, ok := p.bodies[r.Host]
	p.mu.Unlock()
	switch {
	case r.URL.Path != "/.well-known/mta-sts.txt" || !ok:
		http.NotFound(w, r)
	case r.Host == "mta-sts.redirect.test":
		http.Redirect(w, r, "https://mta-sts.good.test/.well-known/mta-sts.txt", http.StatusMovedPermanently)
	case r.Host == "mta-sts.html.test":
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(body))
	}
}

func (p *policyHosts) set(host, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bodies[host] = body
}

func (p *policyHosts) count(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetches[host]
}

func TestSTSFetcher(t *testing.T) {
	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"mta-sts.good.test", "mta-sts.redirect.test", "mta-sts.html.test", "mta-sts.big.test", "mta-sts.missing.test"}
	cert, err := ca.Issue(names...)
	if err != nil {
		t.Fatal(err)
	}
	hosts := &policyHosts{bodies: map[string]string{}, fetches: map[string]int{}}
	srv := httptest.NewUnstartedServer(hosts)
	srv.TLS = smtptest.TLSConfig(cert)
	srv.StartTLS()
	defer srv.Close()
	// A second server whose certificate is for another name.
	wrong, err := ca.Issue("somewhere-else.test")
	if err != nil {
		t.Fatal(err)
	}
	wrongSrv := httptest.NewUnstartedServer(hosts)
	wrongSrv.TLS = smtptest.TLSConfig(wrong)
	wrongSrv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is expected
	wrongSrv.StartTLS()
	defer wrongSrv.Close()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			target := srv.Listener.Addr().String()
			if strings.HasPrefix(addr, "mta-sts.wrongcert.test:") {
				target = wrongSrv.Listener.Addr().String()
			}
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
	}}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	dns := &txtMap{m: map[string][]string{}}
	f := &STSFetcher{Resolver: dns, HTTP: client, Now: func() time.Time { return now }}
	ctx := context.Background()

	enforce := "version: STSv1\r\nmode: enforce\r\nmx: *.good.test\r\nmax_age: 3600\r\n"
	hosts.set("mta-sts.good.test", enforce)
	dns.set("_mta-sts.good.test", "v=STSv1; id=one")

	p, err := f.Policy(ctx, "Good.Test")
	if err != nil || p == nil || p.Mode != STSEnforce || p.ID != "one" || !p.Matches("mx.good.test") {
		t.Fatalf("first fetch: %+v %v", p, err)
	}
	if p, _ := f.Policy(ctx, "good.test"); p == nil || hosts.count("mta-sts.good.test") != 1 {
		t.Fatalf("second call must come from the cache; fetches=%d", hosts.count("mta-sts.good.test"))
	}

	// Same id after the TXT recheck interval: still cached.
	now = now.Add(2 * stsRecheck)
	if p, _ := f.Policy(ctx, "good.test"); p == nil || hosts.count("mta-sts.good.test") != 1 {
		t.Fatalf("unchanged id must not refetch; fetches=%d", hosts.count("mta-sts.good.test"))
	}

	// A new id: the policy is fetched again.
	hosts.set("mta-sts.good.test", strings.Replace(enforce, "enforce", "testing", 1))
	dns.set("_mta-sts.good.test", "v=STSv1; id=two")
	now = now.Add(2 * stsRecheck)
	if p, err := f.Policy(ctx, "good.test"); err != nil || p.Mode != STSTesting || p.ID != "two" || hosts.count("mta-sts.good.test") != 2 {
		t.Fatalf("new id: %+v %v fetches=%d", p, err, hosts.count("mta-sts.good.test"))
	}

	// The TXT record disappears: the cached policy holds until max_age.
	dns.set("_mta-sts.good.test")
	now = now.Add(2 * stsRecheck)
	if p, _ := f.Policy(ctx, "good.test"); p == nil || p.ID != "two" {
		t.Fatalf("cached policy must survive a missing TXT record: %+v", p)
	}
	now = now.Add(time.Hour)
	if p, _ := f.Policy(ctx, "good.test"); p != nil {
		t.Fatalf("expired policy still used: %+v", p)
	}

	// Fetches that must fail: a redirect (not followed), a non-text body, a
	// body over 64 KB, a 404, and a certificate for the wrong name.
	hosts.set("mta-sts.redirect.test", enforce)
	hosts.set("mta-sts.html.test", enforce)
	hosts.set("mta-sts.big.test", enforce+strings.Repeat("x-pad: "+strings.Repeat("a", 1000)+"\r\n", 70))
	hosts.set("mta-sts.wrongcert.test", enforce)
	for _, d := range []string{"redirect.test", "html.test", "big.test", "missing.test", "wrongcert.test"} {
		dns.set("_mta-sts."+d, "v=STSv1; id=abc")
		if p, err := f.Policy(ctx, d); p != nil || err == nil {
			t.Errorf("%s: want no policy and an error, got %+v %v", d, p, err)
		}
	}
	if hosts.count("mta-sts.good.test") != 2 {
		t.Fatal("the redirect was followed")
	}
	// A failed fetch is not retried at once.
	before := hosts.count("mta-sts.html.test")
	now = now.Add(2 * stsRecheck)
	if p, _ := f.Policy(ctx, "html.test"); p != nil || hosts.count("mta-sts.html.test") != before {
		t.Fatalf("failed fetch retried too soon: %d -> %d", before, hosts.count("mta-sts.html.test"))
	}
	now = now.Add(stsFetchRetry)
	f.Policy(ctx, "html.test")
	if hosts.count("mta-sts.html.test") != before+1 {
		t.Fatal("failed fetch never retried")
	}

	// No TXT record at all: no policy, no fetch.
	if p, err := f.Policy(ctx, "nothing.test"); p != nil || err != nil {
		t.Fatalf("no record: %+v %v", p, err)
	}
	// Names that cannot be a domain are never fetched.
	if p, _ := f.Policy(ctx, "evil.test/../x"); p != nil {
		t.Fatal("fetched a policy for a malformed domain")
	}
}
