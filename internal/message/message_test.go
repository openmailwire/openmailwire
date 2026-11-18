package message

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildMultipartRoundTrip(t *testing.T) {
	subject := "کد ورود شما — 123456 — a long subject that needs more than one encoded word to fit"
	raw, err := Build(Input{
		From:      "Acme Login <login@acme.test>",
		To:        []string{"Rahel <rahel@example.test>", "bo@example.test"},
		ReplyTo:   []string{"support@acme.test"},
		Subject:   subject,
		Text:      "Your code is 123456.\nIt expires in 10 minutes.",
		HTML:      "<p dir=\"rtl\">کد شما: <b>123456</b></p>",
		Headers:   map[string]string{"List-Unsubscribe": "<https://acme.test/u/1>"},
		MessageID: "abc@acme.test",
		Date:      time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatalf("line %d is %d bytes", i, len(line))
		}
		if bytes.IndexByte(line, '\n') >= 0 {
			t.Fatalf("bare LF on line %d", i)
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	got, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if got != subject {
		t.Fatalf("subject round trip: got %q", got)
	}
	if msg.Header.Get("Message-ID") != "<abc@acme.test>" {
		t.Fatalf("message-id: %q", msg.Header.Get("Message-ID"))
	}
	to, err := msg.Header.AddressList("To")
	if err != nil || len(to) != 2 || to[0].Name != "Rahel" {
		t.Fatalf("to: %v %v", to, err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(body))
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	if !strings.Contains(parts[0], "It expires in 10 minutes.") || !strings.Contains(parts[1], "کد شما") {
		t.Fatalf("parts did not round trip: %q", parts)
	}
}

func TestBuildSinglePartText(t *testing.T) {
	raw, err := Build(Input{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "Hi", Text: "hello", MessageID: "x@acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("content-type: %q", msg.Header.Get("Content-Type"))
	}
}

func TestValidateHeaders(t *testing.T) {
	bad := []map[string]string{
		{"From": "x"},
		{"X-Bad": "line\r\nInjected: yes"},
		{"Bad Name": "x"},
		{"DKIM-Signature": "x"},
	}
	for _, h := range bad {
		if ValidateHeaders(h) == nil {
			t.Errorf("accepted %v", h)
		}
	}
	if err := ValidateHeaders(map[string]string{"X-Entity-Ref-ID": "42", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"}); err != nil {
		t.Fatal(err)
	}
}
