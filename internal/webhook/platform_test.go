package webhook

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// R4#7: NAT64 and 6to4 forms of internal IPv4 addresses passed the guard.
func TestAddressPolicy(t *testing.T) {
	blocked := []string{
		// The audit's proofs: NAT64 with cloud metadata, loopback, private.
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "64:ff9b::a00:5", "64:ff9b:1::a00:5",
		"64:ff9b:1:ffff::8.8.8.8",              // local-use NAT64 is never globally reachable
		"2002:a9fe:a9fe::1",                    // 6to4 of 169.254.169.254
		"2002:0808:0808::1",                    // 6to4 of a public address: tunnels are refused too
		"2001:0:4136:e378:8000:63bf:3fff:fdd2", // Teredo
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::127.0.0.1", "::1", "::",
		"fc00::1", "fd12:3456::1", "fe80::1", "ff02::1", "100::1", "5f00::1",
		"2001:db8::1", "3fff::1", "2001:2::1", "2620:4f:8000::1",
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "192.0.0.9", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "255.255.255.255", "192.88.99.1", "192.31.196.1",
	}
	for _, s := range blocked {
		if !internal(net.ParseIP(s)) {
			t.Errorf("%s must be refused", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "2a00:1450:4001:80b::200e",
		"64:ff9b::808:808", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		if internal(net.ParseIP(s)) {
			t.Errorf("%s is a public address and must be allowed", s)
		}
	}
	if Public(netip.MustParseAddr("fe80::1%en0")) {
		t.Error("a zoned address must be refused")
	}

	// The dialer refuses before connecting.
	c := NewHTTPClient(false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://[64:ff9b::a9fe:a9fe]/latest/meta-data/", nil)
	if _, err := c.Do(req); !errors.Is(err, errBlocked) {
		t.Errorf("dial to NAT64-mapped 169.254.169.254 was attempted, not refused: %v", err)
	}

	for _, u := range []string{"http://[64:ff9b::7f00:1]/h", "http://127.0.0.1/h", "http://[::ffff:10.0.0.1]/h", "http://[2002:a00:1::1]/h"} {
		if ValidateEndpointFor(u, true, false) == nil {
			t.Errorf("%s accepted as an endpoint", u)
		}
		if ValidateEndpointFor(u, true, true) != nil {
			t.Errorf("%s refused although private addresses are allowed", u)
		}
	}
	if ValidateEndpointFor("https://hooks.example.com/x", false, false) != nil || ValidateEndpointFor("https://8.8.8.8/x", false, false) != nil {
		t.Error("a public endpoint was refused")
	}
}

// R4#6: webhook connections never closed (IdleConnTimeout 0, no global
// cap). The audit's proof needs 95 s idle; this runs the same check with a
// short idle timeout, and checks the production settings.
func TestIdleConnectionsClose(t *testing.T) {
	tr := NewHTTPClient(false).Transport.(*http.Transport)
	if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout > 90*time.Second || tr.MaxIdleConns <= 0 ||
		tr.MaxIdleConnsPerHost <= 0 || tr.MaxConnsPerHost <= 0 || tr.ResponseHeaderTimeout <= 0 ||
		tr.ResponseHeaderTimeout >= RequestTimeout || tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("transport limits: idle %v, max idle %d/%d per host, max per host %d, ttfb %v",
			tr.IdleConnTimeout, tr.MaxIdleConns, tr.MaxIdleConnsPerHost, tr.MaxConnsPerHost, tr.ResponseHeaderTimeout)
	}

	const n = 20
	var mu sync.Mutex
	open := 0
	c := newHTTPClient(true, 200*time.Millisecond)
	for i := 0; i < n; i++ {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
			mu.Lock()
			defer mu.Unlock()
			switch s {
			case http.StateNew:
				open++
			case http.StateClosed, http.StateHijacked:
				open--
			}
		}
		srv.Start()
		defer srv.Close()
		resp, err := c.Post(srv.URL, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		left := open
		mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d idle webhook connections still open", left)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A slow endpoint is cut off by the time-to-first-byte timeout, not the
// whole request timeout.
func TestTimeToFirstByte(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := newHTTPClient(true, time.Second)
	c.Transport.(*http.Transport).ResponseHeaderTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := c.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("no time-to-first-byte timeout: %v after %v", err, time.Since(start))
	}
	if !strings.HasPrefix(describe(err), "timed out") {
		t.Fatalf("describe: %q", describe(err))
	}
}

// R4#8: what a project sees about a failed attempt never names an
// address, a port or the resolver.
func TestDescribeIsGeneric(t *testing.T) {
	dial := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 5432}, Err: err}
	}
	cases := []error{
		fmt.Errorf("Post %q: %w", "http://localhost:5432/x", dial(errBlocked)),
		fmt.Errorf("Post: %w", &net.DNSError{Err: "no such host", Name: "db.internal", Server: "10.0.0.2:53", IsNotFound: true}),
		fmt.Errorf("Post: %w", dial(&net.OpError{Op: "connect", Err: syscall.ECONNREFUSED})),
		fmt.Errorf("Post: %w", dial(syscall.ECONNRESET)),
		fmt.Errorf("Post: %w", context.DeadlineExceeded),
		fmt.Errorf("Post: %w", io.EOF),
		fmt.Errorf("%w: secret: value is sealed with key 0a0b0c0d", errUnseal),
		errors.New("something else at 10.0.0.7:8080"),
	}
	seen := map[string]bool{}
	for _, err := range cases {
		d := describe(err)
		for _, leak := range []string{"::1", "5432", "localhost", "db.internal", "10.0.0", "dial", "0a0b0c0d"} {
			if strings.Contains(d, leak) {
				t.Errorf("describe(%v) = %q reveals %q", err, d, leak)
			}
		}
		seen[d] = true
	}
	if describe(cases[0]) != describe(cases[1]) {
		t.Errorf("a refused address and an unknown name must read the same: %q vs %q", describe(cases[0]), describe(cases[1]))
	}
	if len(seen) < 5 {
		t.Errorf("descriptions should still tell causes apart: %v", seen)
	}
}

func TestSignEd25519(t *testing.T) {
	seed, pub := NewEd25519Key()
	body := []byte(`{"type":"email.sent"}`)
	sig, err := SignEd25519(seed, "msg_1", 1700000000, body)
	if err != nil || !strings.HasPrefix(sig, "v1a,") {
		t.Fatalf("sign: %q %v", sig, err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(sig, "v1a,"))
	if !ed25519.Verify(pub, []byte("msg_1.1700000000."+string(body)), raw) {
		t.Fatal("v1a signature does not verify")
	}
	if !strings.HasPrefix(PublicKeyString(pub), "whpk_") || PublicKeyString(nil) != "" {
		t.Fatal("public key format")
	}
	if _, err := SignEd25519([]byte("short"), "msg_1", 1, body); err == nil {
		t.Fatal("bad seed accepted")
	}
}

// TestSDKVector pins the Ed25519 test vector the four SDKs verify
// (seed 00 01 … 1f, id msg_2Lh9KQHvQbLvjGF3s3hQqZ1u, timestamp 1700000000).
func TestSDKVector(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	body := []byte(`{"type":"email.delivered","data":{"email_id":"e1"}}`)
	sig, _ := SignEd25519(seed, "msg_2Lh9KQHvQbLvjGF3s3hQqZ1u", 1700000000, body)
	const (
		wantPub = "whpk_A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
		wantSig = "v1a,De0zbXw3fa+WDUWMYmuFVJsBTTtbEkQJi1dS49Vm48H2mguOCtQ8embnNXP20I+H5cLV4SkJSc/+h7BGhrm2Dw=="
	)
	if PublicKeyString(pub) != wantPub || sig != wantSig {
		t.Fatalf("public key %s\nsignature  %s", PublicKeyString(pub), sig)
	}
}
