package submission

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// selfSigned returns a certificate for 127.0.0.1, and its PEM files.
func selfSigned(t *testing.T, cn string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM, keyPEM
}

type posted struct {
	header http.Header
	req    struct {
		Raw      []byte   `json:"raw"`
		MailFrom string   `json:"mail_from"`
		Rcpts    []string `json:"rcpts"`
	}
}

// fakeAPI is a control plane that knows one key, omw_good, and answers
// POST /emails/raw with whatever the test sets.
type fakeAPI struct {
	mu     sync.Mutex
	status int
	body   string
	posts  []posted
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer omw_good" {
		w.WriteHeader(401)
		io.WriteString(w, `{"statusCode":401,"name":"invalid_api_key","message":"API key is invalid or revoked"}`)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /domains":
		io.WriteString(w, `{"data":[]}`)
	case "POST /emails/raw":
		var p posted
		p.header = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&p.req)
		f.mu.Lock()
		f.posts = append(f.posts, p)
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeAPI) answer(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakeAPI) last() posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts[len(f.posts)-1]
}

type fixture struct {
	api     *fakeAPI
	ts      *httptest.Server
	srv     *Server
	addr    string // STARTTLS
	tlsAddr string // implicit TLS
}

func start(t *testing.T, tweak func(*Config)) *fixture {
	t.Helper()
	api := &fakeAPI{status: 200, body: `{"id":"8a6e0f8e-0000-4000-8000-000000000001"}`}
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	cert, _, _ := selfSigned(t, "submission.test")
	cfg := Config{Hostname: "submission.test", APIURL: ts.URL, TLS: &tls.Config{Certificates: []tls.Certificate{cert}}}
	if tweak != nil {
		tweak(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	go srv.ServeTLS(tln)
	t.Cleanup(func() { srv.Close() })
	return &fixture{api: api, ts: ts, srv: srv, addr: ln.Addr().String(), tlsAddr: tln.Addr().String()}
}

// dial connects, says EHLO and upgrades with STARTTLS.
func dial(t *testing.T, addr string) *smtp.Client {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	return c
}

// data sends msg and returns the final reply.
func data(t *testing.T, c *smtp.Client, msg string) (int, string) {
	t.Helper()
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		t.Fatal(err)
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(354)
	c.Text.EndResponse(id)
	if err != nil {
		t.Fatal(err)
	}
	dw := c.Text.DotWriter()
	io.WriteString(dw, msg)
	if err := dw.Close(); err != nil {
		t.Fatal(err)
	}
	code, text, err := c.Text.ReadResponse(0)
	if err != nil {
		var te *textproto.Error
		if !errors.As(err, &te) {
			t.Fatal(err)
		}
	}
	return code, text
}

func send(t *testing.T, c *smtp.Client, from string, rcpts []string, msg string) (int, string) {
	t.Helper()
	if err := c.Mail(from); err != nil {
		t.Fatal(err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			t.Fatal(err)
		}
	}
	return data(t, c, msg)
}

// authPlain sends AUTH PLAIN by hand: net/smtp's Auth hangs up after a
// failure, which hides what the server does next.
func authPlain(t *testing.T, c *smtp.Client, key string) (int, string) {
	t.Helper()
	ir := base64.StdEncoding.EncodeToString([]byte("\x00anyone\x00" + key))
	id, err := c.Text.Cmd("AUTH PLAIN %s", ir)
	if err != nil {
		t.Fatal(err)
	}
	c.Text.StartResponse(id)
	defer c.Text.EndResponse(id)
	code, msg, err := c.Text.ReadResponse(0)
	var te *textproto.Error
	if err != nil && !errors.As(err, &te) {
		t.Fatal(err)
	}
	return code, msg
}

const msg1 = "From: a@acme.test\r\nTo: b@example.test\r\nSubject: hi\r\n\r\n.leading dot\r\nbody\r\n"

func TestSubmitPostsToAPI(t *testing.T) {
	f := start(t, nil)
	c := dial(t, f.addr)
	if err := c.Auth(smtp.PlainAuth("", "anyone", "omw_good", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	code, text := send(t, c, "a@acme.test", []string{"b@example.test", "hidden@example.test"}, msg1)
	if code != 250 || text != "2.0.0 queued as 8a6e0f8e-0000-4000-8000-000000000001" {
		t.Fatalf("got %d %q", code, text)
	}
	p := f.api.last()
	if string(p.req.Raw) != msg1 || p.req.MailFrom != "a@acme.test" ||
		strings.Join(p.req.Rcpts, ",") != "b@example.test,hidden@example.test" {
		t.Fatalf("posted %q %q %v", p.req.Raw, p.req.MailFrom, p.req.Rcpts)
	}
	if p.header.Get("Idempotency-Key") != "" {
		t.Fatal("a message without its own Message-ID and Date must not get an idempotency key")
	}

	// A message with its own Message-ID and Date gets a key derived from the
	// request, so a retry of the same bytes is not sent twice.
	withID := "Message-ID: <1@acme.test>\r\nDate: Tue, 17 Nov 2026 09:00:00 +0000\r\n" + msg1
	send(t, c, "a@acme.test", []string{"b@example.test"}, withID)
	k1 := f.api.last().header.Get("Idempotency-Key")
	send(t, c, "a@acme.test", []string{"b@example.test"}, withID)
	k2 := f.api.last().header.Get("Idempotency-Key")
	if !strings.HasPrefix(k1, "smtp-") || k1 != k2 {
		t.Fatalf("keys %q %q", k1, k2)
	}
	send(t, c, "a@acme.test", []string{"c@example.test"}, withID)
	if k3 := f.api.last().header.Get("Idempotency-Key"); k3 == k1 {
		t.Fatal("another envelope must get another key")
	}
}

func TestReplyMapping(t *testing.T) {
	f := start(t, nil)
	c := dial(t, f.addr)
	if err := c.Auth(smtp.PlainAuth("", "anyone", "omw_good", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{403, `{"name":"domain_not_verified","message":"the From domain acme.test is not verified yet"}`, "550 5.7.1 the From domain acme.test is not verified yet"},
		{422, `{"name":"validation_error","message":"line 3 is 1200 bytes long"}`, "554 5.6.0 line 3 is 1200 bytes long"},
		{429, `{"name":"rate_limit_exceeded","message":"hourly cap of 100 reached"}`, "451 4.7.1 hourly cap of 100 reached"},
		{500, `{"name":"internal_error","message":"internal error"}`, "451 4.3.0 Temporary failure"},
		{503, ``, "451 4.3.0 Temporary failure"},
		{401, `{"name":"invalid_api_key","message":"API key is invalid or revoked"}`, "535 5.7.8 Authentication failed"},
		{413, `{"name":"payload_too_large","message":"too large"}`, "552 5.3.4 too large"},
		{422, `{"message":"multi\r\nline – ünïcode"}`, "554 5.6.0 multi  line ? ?n?code"},
	} {
		f.api.answer(tc.status, tc.body)
		code, text := send(t, c, "a@acme.test", []string{"b@example.test"}, msg1)
		if got := strconv.Itoa(code) + " " + text; !strings.HasPrefix(got, tc.want) {
			t.Errorf("%d %s: got %q, want %q", tc.status, tc.body, got, tc.want)
		}
	}
}

// loginAuth is the client side of AUTH LOGIN, which net/smtp lacks.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch string(fromServer) {
	case "Username:":
		return []byte(a.user), nil
	case "Password:":
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected challenge " + string(fromServer))
}

func TestAuthLogin(t *testing.T) {
	f := start(t, nil)
	if err := dial(t, f.addr).Auth(loginAuth{"wordpress", "omw_bad"}); err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("bad key: %v", err)
	}
	c := dial(t, f.addr)
	if err := c.Auth(loginAuth{"wordpress", "omw_good"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := send(t, c, "a@acme.test", []string{"b@example.test"}, msg1); code != 250 {
		t.Fatalf("got %d", code)
	}
}

func TestPlainConnectionCannotAuthenticate(t *testing.T) {
	f := start(t, nil)
	c, err := smtp.Dial(f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("AUTH is advertised before STARTTLS")
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS is not advertised")
	}
	if code, msg := authPlain(t, c, "omw_good"); code != 523 {
		t.Fatalf("AUTH before STARTTLS: %d %q", code, msg)
	}
	err = c.Mail("a@acme.test")
	var te *textproto.Error
	if !errors.As(err, &te) || te.Code != 530 || !strings.Contains(te.Msg, "STARTTLS") {
		t.Fatalf("MAIL before STARTTLS: %v", err)
	}
	if f.api.count() != 0 {
		t.Fatal("the API was called")
	}
}

func TestMailNeedsAuth(t *testing.T) {
	f := start(t, nil)
	c := dial(t, f.addr)
	err := c.Mail("a@acme.test")
	var te *textproto.Error
	if !errors.As(err, &te) || te.Code != 530 || !strings.Contains(te.Msg, "API key") {
		t.Fatalf("MAIL before AUTH: %v", err)
	}
}

func TestTooManyAuthFailuresCloseTheConnection(t *testing.T) {
	f := start(t, func(c *Config) { c.MaxAuthFailures = 2 })
	c := dial(t, f.addr)
	for i, key := range []string{"omw_wrong", "not-a-key"} {
		if code, msg := authPlain(t, c, key); code != 535 {
			t.Fatalf("attempt %d: %d %q", i+1, code, msg)
		}
	}
	// The server announced 421 and hung up.
	code, msg, err := c.Text.ReadResponse(0)
	if code != 421 || !strings.Contains(msg, "Too many failed authentication attempts") {
		t.Fatalf("after the cap: %d %q %v", code, msg, err)
	}
	if err := c.Noop(); err == nil {
		t.Fatal("the connection is still open")
	}
}

func TestImplicitTLS(t *testing.T) {
	f := start(t, nil)
	conn, err := tls.Dial("tcp", f.tlsAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS is advertised on an implicit TLS connection")
	}
	if err := c.Auth(smtp.PlainAuth("", "anyone", "omw_good", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if code, _ := send(t, c, "a@acme.test", []string{"b@example.test"}, msg1); code != 250 {
		t.Fatalf("got %d", code)
	}
}

func TestSizeLimit(t *testing.T) {
	f := start(t, func(c *Config) { c.MaxSize = 1024 })
	c := dial(t, f.addr)
	if ok, size := c.Extension("SIZE"); !ok || size != "1024" {
		t.Fatalf("SIZE %v %q", ok, size)
	}
	if err := c.Auth(smtp.PlainAuth("", "anyone", "omw_good", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	code, _ := send(t, c, "a@acme.test", []string{"b@example.test"}, msg1+strings.Repeat("0123456789abcdef\r\n", 100))
	if code != 552 {
		t.Fatalf("got %d", code)
	}
	if f.api.count() != 0 {
		t.Fatal("an oversized message reached the API")
	}
}

func TestAPIDownIsTemporary(t *testing.T) {
	f := start(t, nil)
	c := dial(t, f.addr)
	if err := c.Auth(smtp.PlainAuth("", "anyone", "omw_good", "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	f.ts.Close() // the control plane goes away after the login
	code, text := send(t, c, "a@acme.test", []string{"b@example.test"}, msg1)
	if code != 451 || !strings.HasPrefix(text, "4.3.0") {
		t.Fatalf("got %d %q", code, text)
	}
}

func TestReloadsRenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	write := func(cn string, at time.Time) {
		_, c, k := selfSigned(t, cn)
		for path, b := range map[string][]byte{certPath: c, keyPath: k} {
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	served := func(cfg *tls.Config) string {
		cert, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf.Subject.CommonName
	}
	write("first", time.Now().Add(-time.Hour))
	cfg, err := LoadTLS(certPath, keyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := served(cfg); got != "first" {
		t.Fatalf("got %q", got)
	}
	write("second", time.Now())
	if got := served(cfg); got != "second" {
		t.Fatalf("after renewal got %q", got)
	}
	// Half-written renewal: the old certificate stays in use.
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := served(cfg); got != "second" {
		t.Fatalf("after a broken key got %q", got)
	}
}
