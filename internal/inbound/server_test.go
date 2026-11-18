package inbound

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// fakeControl is a control plane that knows one address.
type fakeControl struct {
	mu        sync.Mutex
	got       []protocol.InboundMessage
	deliverTo int // HTTP status for /internal/inbound
	checks    int
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer omwk_test" {
		w.WriteHeader(401)
		return
	}
	switch r.URL.Path {
	case "/internal/inbound/check":
		var req protocol.InboundCheckRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.checks++
		f.mu.Unlock()
		json.NewEncoder(w).Encode(protocol.InboundCheckResponse{Accept: req.Rcpt == "ok@x.test", Kind: "inbox"})
	case "/internal/inbound":
		var m protocol.InboundMessage
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		status := f.deliverTo
		if status == 0 {
			status = 200
			f.got = append(f.got, m)
		}
		f.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{"results":[],"name":"x","message":"y"}`))
	}
}

func start(t *testing.T, cfg Config) (*fakeControl, string) {
	t.Helper()
	fc := &fakeControl{}
	cp := httptest.NewServer(fc)
	t.Cleanup(cp.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hostname = "mx.test"
	srv := New(cfg, &Client{URL: cp.URL, Token: "omwk_test", HTTP: cp.Client()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fc, ln.Addr().String()
}

func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mx.test"}, DNSNames: []string{"mx.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func send(addr string, startTLS bool, rcpts []string, body string) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		return err
	}
	if startTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("STARTTLS not offered")
		}
		if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
			return err
		}
	}
	if err := c.Mail("sender@remote.test"); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	io.WriteString(w, body)
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func code(err error) int {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code
	}
	return 0
}

func TestStartTLSAndHandoff(t *testing.T) {
	fc, addr := start(t, Config{TLS: selfSigned(t)})
	if err := send(addr, true, []string{"OK@x.test"}, "Subject: hi\r\n\r\nbody\r\n"); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.got) != 1 {
		t.Fatalf("control plane got %d messages", len(fc.got))
	}
	m := fc.got[0]
	raw := string(m.Raw)
	if !m.TLS || m.HELO != "client.test" || m.MailFrom != "sender@remote.test" || m.RemoteIP != "127.0.0.1" ||
		len(m.Rcpts) != 1 || m.Rcpts[0] != "ok@x.test" {
		t.Fatalf("envelope: %+v", m)
	}
	if !strings.HasPrefix(raw, "Received: from client.test ([127.0.0.1])\r\n\tby mx.test (OpenMailWire) with ESMTPS id ") ||
		!strings.Contains(raw, "\r\n\tfor <ok@x.test>;\r\n\t") || !strings.HasSuffix(raw, "Subject: hi\r\n\r\nbody\r\n") {
		t.Fatalf("raw:\n%s", raw)
	}
}

func TestRefusals(t *testing.T) {
	fc, addr := start(t, Config{MaxSize: 1000})

	// Unknown recipient: refused at RCPT, and the message is never sent.
	if err := send(addr, false, []string{"nobody@x.test"}, "x"); code(err) != 550 || !strings.Contains(err.Error(), "5.1.1") {
		t.Fatalf("unknown recipient: %v", err)
	}
	// No STARTTLS without a certificate.
	if err := send(addr, true, []string{"ok@x.test"}, "x"); err == nil || !strings.Contains(err.Error(), "STARTTLS not offered") {
		t.Fatalf("starttls without a certificate: %v", err)
	}
	// Too large.
	if err := send(addr, false, []string{"ok@x.test"}, strings.Repeat("xxxxxxxxx\r\n", 200)); code(err) != 552 {
		t.Fatalf("too large: %v", err)
	}
	// The control plane refuses the message as malformed, or fails.
	for status, want := range map[int]int{422: 554, 500: 451, 401: 451} {
		fc.mu.Lock()
		fc.deliverTo = status
		fc.mu.Unlock()
		if err := send(addr, false, []string{"ok@x.test"}, "Subject: x\r\n\r\nx\r\n"); code(err) != want {
			t.Fatalf("control plane %d: got %v, want %d", status, err, want)
		}
	}
}

func TestConnectionLimits(t *testing.T) {
	_, addr := start(t, Config{MaxConns: 1, MaxConnDuration: 300 * time.Millisecond})
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	r := bufio.NewReader(first)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "220 mx.test") {
		t.Fatalf("greeting %q", line)
	}

	// A second connection while the first is open is turned away.
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(second).ReadString('\n')
	second.Close()
	if !strings.HasPrefix(line, "421 4.3.2") {
		t.Fatalf("over the limit: %q", line)
	}

	// The first connection is closed once its lifetime is over, however
	// slowly it talks.
	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	for {
		if _, err := r.ReadString('\n'); err != nil {
			break
		}
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("connection outlived MaxConnDuration")
	}

	// Its slot is free again.
	time.Sleep(50 * time.Millisecond)
	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if line, _ := bufio.NewReader(third).ReadString('\n'); !strings.HasPrefix(line, "220 ") {
		t.Fatalf("after the slot freed: %q", line)
	}
}
