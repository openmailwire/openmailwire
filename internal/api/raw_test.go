package api

import (
	"strings"
	"testing"
	"time"
)

func TestParseRawKeepsHeaderAndStripsBcc(t *testing.T) {
	in := "From: Agent <agent@acme.test>\n" +
		"To: a@example.test, \"B\" <b@example.test>\n" +
		"Cc: c@example.test\n" +
		"Bcc: hidden@example.test,\n" +
		" other@example.test\n" +
		"Subject: =?utf-8?q?Caf=C3=A9?=\n" +
		"X-Custom:   kept  as is\n" +
		"\n" +
		"line one\r\nline two\rline three"
	m, err := parseRaw([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if m.from.Address != "agent@acme.test" || m.subject != "Café" {
		t.Fatalf("from %v subject %q", m.from, m.subject)
	}
	if strings.Join(m.to, ",") != "a@example.test,b@example.test" || strings.Join(m.cc, ",") != "c@example.test" ||
		strings.Join(m.bcc, ",") != "hidden@example.test,other@example.test" {
		t.Fatalf("to %v cc %v bcc %v", m.to, m.cc, m.bcc)
	}
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	out := string(m.render("id-1@acme.test", now))
	want := "From: Agent <agent@acme.test>\r\n" +
		"To: a@example.test, \"B\" <b@example.test>\r\n" +
		"Cc: c@example.test\r\n" +
		"Subject: =?utf-8?q?Caf=C3=A9?=\r\n" +
		"X-Custom:   kept  as is\r\n" +
		"Message-ID: <id-1@acme.test>\r\n" +
		"Date: Wed, 18 Nov 2026 10:00:00 +0000\r\n" +
		"\r\n" +
		"line one\r\nline two\r\nline three\r\n"
	if out != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out, want)
	}
}

func TestParseRawKeepsMessageIDAndDate(t *testing.T) {
	in := "Message-ID: <abc@client.test>\r\nDate: Tue, 17 Nov 2026 09:00:00 +0100\r\nFrom: a@acme.test\r\nTo: undisclosed-recipients:;\r\n\r\nhi\r\n"
	m, err := parseRaw([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if m.messageID != "abc@client.test" || !m.hasDate || len(m.to) != 0 {
		t.Fatalf("message-id %q date %v to %v", m.messageID, m.hasDate, m.to)
	}
	if out := string(m.render("ignored@acme.test", time.Now())); out != in {
		t.Fatalf("message changed:\n%q", out)
	}
}

func TestParseRawHeaderOnly(t *testing.T) {
	m, err := parseRaw([]byte("From: a@acme.test\r\nTo: b@example.test\r\nDate: Tue, 17 Nov 2026 09:00:00 +0100\r\nMessage-ID: <x@y>"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(m.render("", time.Now())); !strings.HasSuffix(got, "Message-ID: <x@y>\r\n\r\n") {
		t.Fatalf("got %q", got)
	}
}

func TestParseRawRefusals(t *testing.T) {
	long := strings.Repeat("x", 999)
	for _, c := range []struct{ name, in, want string }{
		{"long body line", "From: a@acme.test\r\n\r\nok\r\n" + long + "\r\n", "line 4 is 999 bytes long"},
		{"long header line", "From: a@acme.test\r\nX-Long: " + long + "\r\n\r\n", "line 2 is"},
		{"no header", "\r\nbody\r\n", "has no header"},
		{"not a field", "From: a@acme.test\r\nthis is body text\r\n", "line 2 of the header is not a header field"},
		{"folded first", " From: a@acme.test\r\n\r\n", "starts with a folded line"},
		{"no from", "To: b@example.test\r\n\r\nhi\r\n", "exactly one From header"},
		{"two froms", "From: a@acme.test\r\nFrom: b@acme.test\r\n\r\nhi\r\n", "exactly one From header"},
		{"two from addresses", "From: a@acme.test, b@acme.test\r\n\r\nhi\r\n", "exactly one address"},
		{"bad to", "From: a@acme.test\r\nTo: not an address\r\n\r\nhi\r\n", "To:"},
		{"nul", "From: a@acme.test\r\n\r\nhi\x00\r\n", "NUL byte"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseRaw([]byte(c.in))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestParseRawLineLimitIsExact(t *testing.T) {
	if _, err := parseRaw([]byte("From: a@acme.test\r\n\r\n" + strings.Repeat("x", 998) + "\n")); err != nil {
		t.Fatalf("a 998-byte line must pass: %v", err)
	}
}
