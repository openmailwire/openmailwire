package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/submission"
)

// startSubmission runs the submission role in-process against e's control
// plane, on random localhost ports. It returns the STARTTLS address and the
// implicit TLS one.
func (e *env) startSubmission() (addr, tlsAddr string) {
	e.t.Helper()
	srv, err := submission.New(submission.Config{
		Hostname: "submission.openmailwire.test", APIURL: e.ts.URL, Log: e.log, Version: "test",
		TLS: &tls.Config{Certificates: []tls.Certificate{testCert(e.t)}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	tln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	go srv.Serve(ln)
	go srv.ServeTLS(tln)
	e.t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), tln.Addr().String()
}

// testCert is a fresh self-signed certificate for 127.0.0.1.
func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "submission.openmailwire.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// smtpLogin connects with STARTTLS and logs in with AUTH PLAIN.
func smtpLogin(t *testing.T, addr, key string) (*smtp.Client, error) {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Hello("app.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	return c, c.Auth(smtp.PlainAuth("", "any-username", key, "127.0.0.1"))
}

// smtpSend runs one transaction and returns the reply to the message.
func smtpSend(t *testing.T, c *smtp.Client, from string, rcpts []string, msg string) (int, string) {
	t.Helper()
	if err := c.Mail(from); err != nil {
		t.Fatal(err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			t.Fatal(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, msg)
	// Close returns the error reply, but not the text of a success, so read
	// the final reply by hand.
	err = w.Close()
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code, te.Msg
	}
	if err != nil {
		t.Fatal(err)
	}
	return 250, ""
}

var queuedRE = regexp.MustCompile(`queued as ([0-9a-f-]{36})`)

// smtpSendQueued sends and returns the email id from "250 2.0.0 queued as
// <id>".
func smtpSendQueued(t *testing.T, c *smtp.Client, from string, rcpts []string, msg string) string {
	t.Helper()
	if err := c.Mail(from); err != nil {
		t.Fatal(err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			t.Fatal(err)
		}
	}
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
	code, text, err := c.Text.ReadResponse(250)
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	m := queuedRE.FindStringSubmatch(text)
	if code != 250 || !strings.HasPrefix(text, "2.0.0 queued as ") || m == nil {
		t.Fatalf("DATA answered %d %q", code, text)
	}
	return m[1]
}

func TestSubmissionDeliversSignedWithoutBcc(t *testing.T) {
	e := newEnv(t, nil)
	_, dkimTXT := e.addVerifiedDomain("sub.test")
	e.startWorker()
	addr, _ := e.startSubmission()

	c, err := smtpLogin(t, addr, e.apiKey)
	if err != nil {
		t.Fatal(err)
	}
	msg := "From: App <app@sub.test>\r\n" +
		"To: to@example.test\r\n" +
		"Bcc: hidden@example.test\r\n" +
		"Subject: Over SMTP\r\n" +
		"\r\n" +
		"Hello over SMTP.\r\n"
	id := smtpSendQueued(t, c, "app@sub.test", []string{"to@example.test", "hidden@example.test"}, msg)
	c.Quit()

	e.waitStatus(id, "to@example.test", "delivered")
	v := e.waitStatus(id, "hidden@example.test", "delivered")
	if v["subject"] != "Over SMTP" {
		t.Fatalf("subject %v", v["subject"])
	}
	if bcc, _ := v["bcc"].([]any); len(bcc) != 1 || bcc[0] != "hidden@example.test" {
		t.Fatalf("bcc %v", v["bcc"])
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	for _, m := range msgs {
		data := string(m.Data)
		verifyDKIM(t, m.Data, dkimTXT)
		head, _, _ := strings.Cut(data, "\r\n\r\n")
		if strings.Contains(strings.ToLower(head), "\r\nbcc:") || strings.Contains(data, "hidden@") {
			t.Fatalf("the Bcc header reached %v:\n%s", m.Rcpts, data)
		}
		if !strings.Contains(head, "Message-ID: <"+id+"@sub.test>") || !strings.Contains(head, "\r\nDate: ") {
			t.Fatalf("Message-ID or Date was not added:\n%s", head)
		}
		if !strings.HasPrefix(m.MailFrom, "b-") || !strings.HasSuffix(m.MailFrom, "@bounce.sub.test") {
			t.Fatalf("envelope sender %q is not the return path", m.MailFrom)
		}
	}
}

func TestSubmissionImplicitTLS(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("tls.test")
	_, tlsAddr := e.startSubmission()

	conn, err := tls.Dial("tcp", tlsAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("app.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Auth(smtp.PlainAuth("", "any-username", e.apiKey, "127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	id := smtpSendQueued(t, c, "app@tls.test", []string{"to@example.test"},
		"From: app@tls.test\r\nTo: to@example.test\r\nSubject: port 465\r\n\r\nhi\r\n")
	e.waitStatus(id, "to@example.test", "queued")
}

func TestSubmissionRefusals(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("ok.test")
	addr, _ := e.startSubmission()

	t.Run("AUTH before STARTTLS", func(t *testing.T) {
		c, err := smtp.Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Hello("app.example.test"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := c.Extension("AUTH"); ok {
			t.Fatal("AUTH is offered before STARTTLS")
		}
		ir := base64.StdEncoding.EncodeToString([]byte("\x00u\x00" + e.apiKey))
		id, err := c.Text.Cmd("AUTH PLAIN %s", ir)
		if err != nil {
			t.Fatal(err)
		}
		c.Text.StartResponse(id)
		code, msg, err := c.Text.ReadResponse(235)
		c.Text.EndResponse(id)
		if err == nil || code/100 != 5 {
			t.Fatalf("AUTH before STARTTLS answered %d %q", code, msg)
		}
		if err := c.Mail("app@ok.test"); err == nil || !strings.Contains(err.Error(), "530") {
			t.Fatalf("MAIL before STARTTLS: %v", err)
		}
	})

	t.Run("bad key", func(t *testing.T) {
		for _, key := range []string{"omw_not-a-real-key", "hunter2"} {
			_, err := smtpLogin(t, addr, key)
			var te *textproto.Error
			if !errors.As(err, &te) || te.Code != 535 {
				t.Fatalf("key %q: %v", key, err)
			}
		}
	})

	c, err := smtpLogin(t, addr, e.apiKey)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unverified From domain", func(t *testing.T) {
		if code, d := e.call("POST", "/domains", "", map[string]string{"name": "pending.test"}); code != 201 {
			t.Fatalf("add domain: %d %v", code, d)
		}
		code, text := smtpSend(t, c, "app@pending.test", []string{"to@example.test"},
			"From: app@pending.test\r\nTo: to@example.test\r\nSubject: s\r\n\r\nhi\r\n")
		if code != 550 || !strings.HasPrefix(text, "5.7.1 ") || !strings.Contains(text, "pending.test is not verified") ||
			!strings.Contains(text, "/verify") {
			t.Fatalf("got %d %q", code, text)
		}
	})

	t.Run("another project's From domain", func(t *testing.T) {
		other := e.otherProject()
		if code, d := other.call("POST", "/domains", "", map[string]string{"name": "theirs.test"}); code != 201 {
			t.Fatalf("add domain: %d %v", code, d)
		}
		code, text := smtpSend(t, c, "app@ok.test", []string{"to@example.test"},
			"From: ceo@theirs.test\r\nTo: to@example.test\r\nSubject: s\r\n\r\nhi\r\n")
		if code != 550 || !strings.Contains(text, "theirs.test is not added to this project") {
			t.Fatalf("got %d %q", code, text)
		}
	})

	t.Run("malformed message", func(t *testing.T) {
		code, text := smtpSend(t, c, "app@ok.test", []string{"to@example.test"},
			"From: app@ok.test\r\nSubject: s\r\n\r\n"+strings.Repeat("x", 1200)+"\r\n")
		if code != 554 || !strings.HasPrefix(text, "5.6.0 ") || !strings.Contains(text, "998") {
			t.Fatalf("got %d %q", code, text)
		}
	})

	var n int
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM emails`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused messages were stored: %d %v", n, err)
	}
}

func TestRawEmailAPI(t *testing.T) {
	e := newEnv(t, nil)
	_, dkimTXT := e.addVerifiedDomain("raw.test")
	e.startWorker()

	// Bare LF line endings, as a script might write them.
	raw := "From: Raw Bot <bot@raw.test>\n" +
		"To: one@example.test\n" +
		"Cc: two@example.test\n" +
		"Bcc: three@example.test\n" +
		"Subject: =?utf-8?q?R=C3=A4w?=\n" +
		"\n" +
		"line one\n" +
		"line two\n"
	body := map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(raw))}
	code, r := e.call("POST", "/emails/raw", "raw-1", body)
	if code != 200 {
		t.Fatalf("send: %d %v", code, r)
	}
	id := r["id"].(string)
	if code, r := e.call("POST", "/emails/raw", "raw-1", body); code != 200 || r["id"] != id {
		t.Fatalf("idempotent replay: %d %v", code, r)
	}
	other := map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(raw + "more\n"))}
	if code, r := e.call("POST", "/emails/raw", "raw-1", other); code != 409 || r["name"] != "idempotency_conflict" {
		t.Fatalf("same key, other message: %d %v", code, r)
	}

	var v map[string]any
	for _, rcpt := range []string{"one@example.test", "two@example.test", "three@example.test"} {
		v = e.waitStatus(id, rcpt, "delivered")
	}
	if v["subject"] != "Räw" || v["from"] != `"Raw Bot" <bot@raw.test>` {
		t.Fatalf("subject %v from %v", v["subject"], v["from"])
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 3 {
		t.Fatalf("got %d messages", len(msgs))
	}
	for _, m := range msgs {
		data := string(m.Data)
		verifyDKIM(t, m.Data, dkimTXT)
		if strings.Count(data, "\n") != strings.Count(data, "\r\n") {
			t.Fatalf("bare LF left in:\n%q", data)
		}
		if strings.Contains(data, "three@") {
			t.Fatalf("Bcc leaked to %v", m.Rcpts)
		}
		if !strings.Contains(data, "\r\n\r\nline one\r\nline two\r\n") {
			t.Fatalf("body changed:\n%q", data)
		}
	}

	t.Run("rcpts replace the header recipients", func(t *testing.T) {
		code, r := e.call("POST", "/emails/raw", "", map[string]any{
			"raw":       base64.StdEncoding.EncodeToString([]byte(raw)),
			"mail_from": "bot@raw.test",
			"rcpts":     []string{"Only@Example.test"},
		})
		if code != 200 {
			t.Fatalf("send: %d %v", code, r)
		}
		_, v := e.call("GET", "/emails/"+r["id"].(string), "", nil)
		recips, _ := v["recipients"].([]any)
		if len(recips) != 1 || recips[0].(map[string]any)["email"] != "only@example.test" {
			t.Fatalf("recipients %v", v["recipients"])
		}
	})

	t.Run("20 MB body limit", func(t *testing.T) {
		// Larger than the 4 MB limit of the other endpoints.
		big := "From: bot@raw.test\r\nTo: one@example.test\r\nSubject: big\r\n\r\n" +
			strings.Repeat(strings.Repeat("z", 76)+"\r\n", 6<<20/78)
		code, r := e.call("POST", "/emails/raw", "", map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(big))})
		if code != 200 {
			t.Fatalf("a 6 MB message: %d %v", code, r)
		}
		tooBig := `{"raw":"` + strings.Repeat("A", 20<<20) + `"}`
		req := httptest.NewRequest("POST", "/emails/raw", strings.NewReader(tooBig))
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 413 || !strings.Contains(rec.Body.String(), "payload_too_large") {
			t.Fatalf("over 20 MB: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		e.call("POST", "/domains", "", map[string]string{"name": "unverified.test"})
		for _, c := range []struct {
			raw, name, want string
			status          int
		}{
			{"From: a@raw.test\r\nTo: x@example.test\r\n\r\n" + strings.Repeat("y", 999) + "\r\n", "validation_error", "998", 422},
			{"To: x@example.test\r\n\r\nhi\r\n", "validation_error", "From", 422},
			{"From: a@raw.test\r\n\r\nhi\r\n", "validation_error", "no recipients", 422},
			{"From: a@unverified.test\r\nTo: x@example.test\r\n\r\nhi\r\n", "domain_not_verified", "/verify", 403},
			{"From: a@nowhere.test\r\nTo: x@example.test\r\n\r\nhi\r\n", "domain_not_found", "POST /domains", 403},
		} {
			code, r := e.call("POST", "/emails/raw", "", map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(c.raw))})
			msg, _ := r["message"].(string)
			if code != c.status || r["name"] != c.name || !strings.Contains(msg, c.want) {
				t.Errorf("%q: got %d %v", c.raw[:20], code, r)
			}
		}
		if code, r := e.call("POST", "/emails/raw", "", map[string]any{"raw": "%%%"}); code != 422 {
			t.Errorf("bad base64: %d %v", code, r)
		}
	})
}
