package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openmailwire/openmailwire/internal/api"
	"github.com/openmailwire/openmailwire/internal/smtptest"
)

func TestDevDeliver(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(file, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = devDeliver(context.Background(), []string{
		"--inbound", srv.Addr, "--to", "agent@example.com", "--to", "Ops <ops@example.com>",
		"--subject", "Grüße", "--text", "line one\n.leading dot\n", "--attach", file, "--in-reply-to", "abc@example.com",
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "accepted Message-ID <") {
		t.Fatalf("output %q", out.String())
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	m := msgs[0]
	if m.MailFrom != "alex@sender.test" || strings.Join(m.Rcpts, ",") != "agent@example.com,ops@example.com" {
		t.Fatalf("envelope %q %v", m.MailFrom, m.Rcpts)
	}
	if bytes.Contains(bytes.ReplaceAll(m.Data, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("a bare LF in the message")
	}
	msg, err := mail.ReadMessage(bytes.NewReader(m.Data))
	if err != nil {
		t.Fatal(err)
	}
	h := msg.Header
	subj, _ := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
	if subj != "Grüße" || h.Get("In-Reply-To") != "<abc@example.com>" || h.Get("References") != "<abc@example.com>" ||
		h.Get("Date") == "" || !strings.HasSuffix(h.Get("Message-ID"), "@sender.test>") || h.Get("From") != `"Alex Sender" <alex@sender.test>` {
		t.Fatalf("header %v", h)
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content type %q", h.Get("Content-Type"))
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	text, err := mr.NextPart() // decodes quoted-printable
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(text); string(b) != "line one\r\n.leading dot\r\n" {
		t.Fatalf("text %q", b)
	}
	att, err := mr.NextPart()
	if err != nil || att.FileName() != "report.csv" {
		t.Fatalf("attachment %v %v", att, err)
	}

	// A recipient the node refuses: the error says what to do.
	srv.ReplyToRcpt("nobody@example.com", "550 5.1.1 no such inbox")
	err = devDeliver(context.Background(), []string{"--inbound", srv.Addr, "--to", "nobody@example.com"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "550") || !strings.Contains(err.Error(), "POST /inboxes") {
		t.Fatalf("refused recipient: %v", err)
	}
	// Bad input never reaches the network.
	for _, args := range [][]string{
		{"--inbound", srv.Addr},
		{"--inbound", srv.Addr, "--to", "not an address"},
		{"--inbound", "nohost", "--to", "a@b.test"},
		{"--inbound", srv.Addr, "--to", "a@b.test", "--attach", "/no/such/file"},
		{"--inbound", srv.Addr, "--to", "a@b.test", "stray"},
	} {
		if err := devDeliver(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	if n := len(srv.Messages()); n != 1 {
		t.Fatalf("%d messages after the refusals", n)
	}
}

func TestDevCert(t *testing.T) {
	cert, key, err := writeDevCert(t.TempDir(), "smtp.local", "192.0.2.5")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cert)
	block, _ := pem.Decode(raw)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil || len(pair.Certificate) != 1 {
		t.Fatal(err)
	}
	for _, h := range []string{"localhost", "smtp.local", "127.0.0.1", "192.0.2.5"} {
		if err := c.VerifyHostname(h); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
}

func TestPrintSPFSync(t *testing.T) {
	var b strings.Builder
	printSPFSync(&b, api.OperatorSPF{Name: "_spf.mail.example.net", Zone: "example.net", Action: "update",
		Old: []string{"v=spf1 ip4:203.0.113.7 -all"}, New: "v=spf1 ip4:198.51.100.9 ip4:203.0.113.7 -all",
		IPs: []string{"198.51.100.9", "203.0.113.7"}}, false)
	want := "TXT _spf.mail.example.net (Cloudflare zone example.net)\n" +
		"- v=spf1 ip4:203.0.113.7 -all\n+ v=spf1 ip4:198.51.100.9 ip4:203.0.113.7 -all\n" +
		"updated (2 IPs of delivery workers seen in the last 7 days)\n"
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	printSPFSync(&b, api.OperatorSPF{Name: "_spf.mail.example.net", Zone: "example.net", Action: "create", New: "v=spf1 ip4:203.0.113.7 -all",
		IPs: []string{"203.0.113.7"}}, true)
	if !strings.Contains(b.String(), "+ v=spf1 ip4:203.0.113.7 -all\ndry run: would be created") {
		t.Fatalf("dry run: %s", b.String())
	}
	b.Reset()
	printSPFSync(&b, api.OperatorSPF{Name: "_spf.mail.example.net", Action: "skip", Reason: "no delivery worker"}, false)
	if !strings.Contains(b.String(), "nothing changed: no delivery worker") {
		t.Fatalf("skip: %s", b.String())
	}
}
