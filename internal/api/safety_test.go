package api

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLinkHosts(t *testing.T) {
	hosts, found := linkHosts("see https://Example.com/a, or www.other.test/x.",
		`<a href="https://evil.example/login?a=1&amp;b=2">x</a> <a href='//cdn.test/i'>i</a> <a href=/relative>r</a>
		 <a href="#top">t</a> <a href="mailto:a@b.test">m</a> <a href="cid:logo">c</a>`)
	if !found || !slices.Equal(hosts, []string{"cdn.test", "evil.example", "example.com", "www.other.test"}) {
		t.Fatalf("hosts %v, found %v", hosts, found)
	}
	if _, found := linkHosts("no links here, just a@b.test", `<a href="#x">x</a> <a href="mailto:x@y.test">y</a>`); found {
		t.Fatal("mailto and in-page links are not web links")
	}
	if _, found := linkHosts("", `<a href="/account">a</a>`); !found {
		t.Fatal("a relative href is a link")
	}
	got := foreignHosts([]string{"www.acme.co.uk", "acme.co.uk", "mail.acme.co.uk", "other.co.uk", "evil.example"}, "news.acme.co.uk")
	if !slices.Equal(got, []string{"other.co.uk", "evil.example"}) {
		t.Fatalf("foreign hosts %v", got)
	}
}

func TestUnsubToken(t *testing.T) {
	s := &Server{MACKey: []byte(strings.Repeat("m", 32))}
	pid := "1c2d3e4f-5a6b-4c7d-8e9f-0a1b2c3d4e5f"
	tok := s.unsubToken(pid, "Jane@Example.org")
	gotPID, addr, ok := s.parseUnsubToken(tok)
	if !ok || gotPID != pid || addr != "jane@example.org" {
		t.Fatalf("parse: %v %q %q", ok, gotPID, addr)
	}
	if strings.ContainsAny(tok, "/+=?&") {
		t.Fatalf("token is not URL-safe: %s", tok)
	}
	other := &Server{MACKey: []byte(strings.Repeat("n", 32))}
	if _, _, ok := other.parseUnsubToken(tok); ok {
		t.Fatal("a token verified under another key")
	}
	// Changing the address (or project) breaks the signature.
	forged := s.unsubToken(pid, "boss@example.org")
	payload, _, _ := strings.Cut(forged, ".")
	_, sig, _ := strings.Cut(tok, ".")
	if _, _, ok := s.parseUnsubToken(payload + "." + sig); ok {
		t.Fatal("a forged payload verified")
	}
	for _, bad := range []string{"", ".", "abc", "abc.def", strings.Repeat("A", 2000)} {
		if _, _, ok := s.parseUnsubToken(bad); ok {
			t.Fatalf("%q verified", bad)
		}
	}
	if m := maskAddress("jane@example.org"); m != "j***@example.org" {
		t.Fatalf("mask %q", m)
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(1, 3)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if w := l.take("1.2.3.4", now); w != 0 {
			t.Fatalf("request %d limited: %v", i, w)
		}
	}
	if w := l.take("1.2.3.4", now); w <= 0 || w > time.Second {
		t.Fatalf("fourth request: wait %v", w)
	}
	if w := l.take("5.6.7.8", now); w != 0 {
		t.Fatal("another IP shares the bucket")
	}
	if w := l.take("1.2.3.4", now.Add(1100*time.Millisecond)); w != 0 {
		t.Fatalf("refill: %v", w)
	}
	for addr, want := range map[string]string{
		"192.0.2.7:4242":             "192.0.2.7",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::1]:443": "2001:db8:1:2::/64",
	} {
		if got := unsubKey(netip.MustParseAddrPort(addr).Addr()); got != want {
			t.Errorf("unsubKey(%s) = %s, want %s", addr, got, want)
		}
	}
}

func TestPercent(t *testing.T) {
	for f, want := range map[float64]string{0.05: "5%", 0.001: "0.1%", 0.075: "7.5%", 1: "100%", 0.3333: "33.33%"} {
		if got := percent(f); got != want {
			t.Errorf("percent(%v) = %s, want %s", f, got, want)
		}
	}
}
