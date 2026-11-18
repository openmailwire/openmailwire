package mailparse

import (
	"bytes"
	"encoding/base64"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// fixture loads testdata/name. With crlf, line endings become CRLF, as on
// the wire; every parser must give the same answer either way.
func fixture(t *testing.T, name string, crlf bool) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if crlf {
		b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	}
	return b
}

func eachEnding(t *testing.T, fn func(t *testing.T, crlf bool)) {
	t.Run("lf", func(t *testing.T) { fn(t, false) })
	t.Run("crlf", func(t *testing.T) { fn(t, true) })
}

func TestDSNFixtures(t *testing.T) {
	cases := []struct {
		file      string
		rcpt      string
		status    string
		code      int
		diag      string // a substring of the diagnostic
		remoteMTA string
		reporting string
		plain     bool
		msgID     string
	}{
		{"postfix-dsn.eml", "nobody@example.org", "5.1.1", 550,
			"550 5.1.1 <nobody@example.org>: Recipient address rejected: User unknown in local recipient table",
			"mx1.example.org", "mail.example.org", false, "0f6c2d1e-9a8b-4c7d-8e9f-00112233aabb@acme.test"},
		{"gmail-dsn.eml", "missing.person@gmail.com", "5.1.1", 550,
			"550-5.1.1 The email account that you tried to reach does not exist.",
			"gmail-smtp-in.l.google.com. (142.250.27.26, the server for the domain gmail.com.)", "googlemail.com", false,
			"7a1b2c3d-4e5f-4071-8293-a4b5c6d7e8f9@acme.test"},
		{"m365-ndr.eml", "jdoe@contoso.com", "5.1.10", 550,
			"550 5.1.10 RESOLVER.ADR.RecipientNotFound; Recipient not found by SMTP address lookup",
			"", "DM6PR11MB4321.namprd11.prod.outlook.com", false, "1c2d3e4f-5a6b-47c8-d9e0-f1a2b3c4d5e6@acme.test"},
		{"yahoo-bounce.eml", "gone.user@yahoo.com", "5.1.1", 554,
			"554: delivery error: dd This user doesn't have a yahoo.com account",
			"", "", true, ""},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			eachEnding(t, func(t *testing.T, crlf bool) {
				rep := ParseReport(fixture(t, c.file, crlf))
				if rep.Kind != KindDSN || rep.DSN == nil {
					t.Fatalf("kind %q, want dsn", rep.Kind)
				}
				d := rep.DSN
				if d.PlainText != c.plain || d.ReportingMTA != c.reporting {
					t.Fatalf("plain=%v reporting=%q", d.PlainText, d.ReportingMTA)
				}
				failed := d.Failed()
				if len(failed) != 1 {
					t.Fatalf("want 1 failed recipient, got %+v", d.Recipients)
				}
				r := failed[0]
				if r.FinalRecipient != c.rcpt || r.Status != c.status || r.SMTPCode != c.code || r.RemoteMTA != c.remoteMTA {
					t.Fatalf("recipient %+v", r)
				}
				if !strings.Contains(r.Diagnostic, c.diag) {
					t.Fatalf("diagnostic %q does not contain %q", r.Diagnostic, c.diag)
				}
				if c.msgID != "" && (d.Original == nil || d.Original.MessageID != c.msgID) {
					t.Fatalf("original %+v", d.Original)
				}
			})
		})
	}
}

func TestARFFixtures(t *testing.T) {
	eachEnding(t, func(t *testing.T, crlf bool) {
		rep := ParseReport(fixture(t, "arf-yahoo.eml", crlf))
		if rep.Kind != KindARF {
			t.Fatalf("kind %q", rep.Kind)
		}
		a := rep.ARF
		want := ARF{
			FeedbackType: "abuse", UserAgent: "Yahoo!-Mail-Feedback/2.0", Version: "0.1",
			OriginalMailFrom: "b-3c4d5e6f7a8b49c0d1e2f3a4b5c6d7e8-9f8e7d6c5b@bounce.acme.test",
			OriginalRcptTo:   []string{"complainer@yahoo.com"}, ReportedDomain: "acme.test",
			SourceIP: "203.0.113.7", ArrivalDate: "Tue, 17 Nov 2026 09:58:40 +0000",
			Original: &Original{
				MessageID:  "3c4d5e6f-7a8b-49c0-d1e2-f3a4b5c6d7e8@acme.test",
				ReturnPath: "b-3c4d5e6f7a8b49c0d1e2f3a4b5c6d7e8-9f8e7d6c5b@bounce.acme.test",
				From:       "news@acme.test", To: []string{"complainer@yahoo.com"},
			},
		}
		if !reflect.DeepEqual(*a, want) {
			t.Fatalf("got  %+v %+v\nwant %+v %+v", *a, a.Original, want, want.Original)
		}
		if !a.IsComplaint() {
			t.Fatal("abuse report is a complaint")
		}

		// Microsoft's JMRP: no Original-Rcpt-To and a redacted To; the
		// envelope sender is all that identifies the recipient.
		rep = ParseReport(fixture(t, "arf-microsoft.eml", crlf))
		if rep.Kind != KindARF {
			t.Fatalf("kind %q", rep.Kind)
		}
		a = rep.ARF
		if a.FeedbackType != "abuse" || len(a.OriginalRcptTo) != 0 ||
			a.OriginalMailFrom != "b-4d5e6f7a8b9c40d1e2f3a4b5c6d7e8f9-1a2b3c4d5e@bounce.acme.test" ||
			a.Original == nil || a.Original.MessageID != "4d5e6f7a-8b9c-40d1-e2f3-a4b5c6d7e8f9@acme.test" ||
			a.Original.ReturnPath != a.OriginalMailFrom {
			t.Fatalf("jmrp: %+v %+v", a, a.Original)
		}
	})
}

func TestReportEdgeCases(t *testing.T) {
	// A DMARC failure report uses the ARF format but is not a complaint.
	dmarc := "From: dmarc@example.org\r\nContent-Type: multipart/report; report-type=feedback-report; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: message/feedback-report\r\n\r\nFeedback-Type: auth-failure\r\nAuth-Failure: dkim\r\n\r\n--b--\r\n"
	if rep := ParseReport([]byte(dmarc)); rep.Kind != KindARF || rep.ARF.IsComplaint() {
		t.Fatalf("dmarc report: %+v", rep)
	}

	// An out-of-office reply is not a bounce, even though it is automatic.
	ooo := "From: Someone <someone@example.org>\r\nSubject: Out of office\r\nAuto-Submitted: auto-replied\r\n\r\nI am away until Monday. Call 555 0100 if urgent.\r\n"
	if rep := ParseReport([]byte(ooo)); rep.Kind != "" {
		t.Fatalf("auto-reply read as %+v", rep)
	}

	// Sloppy DSN: base64-encoded status part, no blank line between the
	// per-message and per-recipient fields or between recipients, Action
	// missing on one of them, and a delayed recipient.
	status := "Reporting-MTA: dns; mx.example.org\r\n" +
		"Final-Recipient: rfc822; <a@example.org>\r\nStatus: 5.2.1 (mailbox disabled)\r\n" +
		"Diagnostic-Code: smtp; 550 5.2.1 mailbox disabled\r\n" +
		"Final-Recipient: rfc822; b@example.org\r\nAction: delayed\r\nStatus: 4.4.1\r\n"
	sloppy := "From: MAILER-DAEMON@mx.example.org\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=\"x\"\r\n\r\n" +
		"--x\r\nContent-Type: text/plain\r\n\r\nfailed\r\n" +
		"--x\r\nContent-Type: message/delivery-status\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		b64(status) + "\r\n--x" // no closing delimiter
	rep := ParseReport([]byte(sloppy))
	if rep.Kind != KindDSN || len(rep.DSN.Recipients) != 2 {
		t.Fatalf("sloppy dsn: %+v", rep.DSN)
	}
	a, b := rep.DSN.Recipients[0], rep.DSN.Recipients[1]
	if a.FinalRecipient != "a@example.org" || a.Action != "failed" || a.Status != "5.2.1" || a.SMTPCode != 550 {
		t.Fatalf("first recipient %+v", a)
	}
	if b.FinalRecipient != "b@example.org" || b.Action != "delayed" || len(rep.DSN.Failed()) != 1 {
		t.Fatalf("second recipient %+v", b)
	}
	if rep.DSN.ReportingMTA != "mx.example.org" {
		t.Fatalf("reporting mta %q", rep.DSN.ReportingMTA)
	}

	// Garbage never panics and is not a report.
	for _, g := range []string{"", "\r\n", "no header at all", "Content-Type: multipart/report; boundary=\r\n\r\n--\r\n--", "Content-Type: multipart/mixed; boundary=a\r\n\r\n--a\r\n--a\r\n--a--"} {
		if rep := ParseReport([]byte(g)); rep.Kind != "" {
			t.Fatalf("%q read as %+v", g, rep)
		}
		Parse([]byte(g))
	}
}

func TestParseMessage(t *testing.T) {
	eachEnding(t, func(t *testing.T, crlf bool) {
		m := Parse(fixture(t, "reply-multipart.eml", crlf))
		if m.From != "María López <Maria@Example.NET>" || m.FromAddr != "maria@example.net" {
			t.Fatalf("from %q %q", m.From, m.FromAddr)
		}
		if !reflect.DeepEqual(m.ReplyTo, []string{`"Lopez, Maria" <maria.replies@example.net>`}) {
			t.Fatalf("reply-to %q", m.ReplyTo)
		}
		if !reflect.DeepEqual(m.To, []string{"Acme Support <support@acme.test>"}) ||
			!reflect.DeepEqual(m.Cc, []string{"ops@example.net", "Second Person <second@example.net>"}) {
			t.Fatalf("to %q cc %q", m.To, m.Cc)
		}
		if m.Subject != "Re: Your order has shipped – déjà vu" {
			t.Fatalf("subject %q", m.Subject)
		}
		if m.MessageID != "CAFx9q1=reply-1@mail.example.net" || m.InReplyTo != "7a1b2c3d-4e5f-4071-8293-a4b5c6d7e8f9@acme.test" ||
			!reflect.DeepEqual(m.References, []string{"first-thread-message@acme.test", "7a1b2c3d-4e5f-4071-8293-a4b5c6d7e8f9@acme.test"}) {
			t.Fatalf("threading %q %q %q", m.MessageID, m.InReplyTo, m.References)
		}
		if m.Headers["references"] != "<first-thread-message@acme.test> <7a1b2c3d-4e5f-4071-8293-a4b5c6d7e8f9@acme.test>" ||
			m.Headers["date"] != "Tue, 17 Nov 2026 12:04:07 +0100" {
			t.Fatalf("headers %q", m.Headers)
		}
		if !strings.HasPrefix(m.Text, "Thanks – the parcel arrived, but the receipt is missing. Could you send it again?") ||
			!strings.Contains(m.Text, "María") {
			t.Fatalf("text %q", m.Text)
		}
		if !strings.Contains(m.HTML, "Thanks – the parcel") {
			t.Fatalf("html %q", m.HTML)
		}
		if got := m.Snippet(200); got != "Thanks – the parcel arrived, but the receipt is missing. Could you send it again? María On Tue, 17 Nov 2026 at 10:20, Acme Support <support@acme.test> wrote:" {
			t.Fatalf("snippet %q", got)
		}
		if len(m.Attachments) != 2 {
			t.Fatalf("attachments %+v", m.Attachments)
		}
		pdf, img := m.Attachments[0], m.Attachments[1]
		if pdf.Filename != "factura-ñ.pdf" || pdf.ContentType != "application/pdf" || !bytes.HasPrefix(pdf.Data, []byte("%PDF-1.4")) || pdf.Inline {
			t.Fatalf("pdf %+v", pdf)
		}
		if img.Filename != "attachment-2" || img.ContentType != "image/png" || img.ContentID != "ii_photo1" || !img.Inline ||
			!bytes.HasPrefix(img.Data, []byte("\x89PNG")) {
			t.Fatalf("image %+v", img)
		}
	})
}

func TestParseMessageOddities(t *testing.T) {
	// No MIME at all, a header with no space after the colon, an
	// unparseable From and a filename that tries to climb directories.
	m := Parse([]byte("From:not an address <x@y.test\nSubject:hi\n\nplain body\n"))
	if m.Subject != "hi" || m.Text != "plain body\n" || m.FromAddr != "x@y.test" {
		t.Fatalf("plain: %+v", m)
	}
	m = Parse([]byte("Content-Type: multipart/mixed; boundary=z\n\n--z\nContent-Type: text/plain\nContent-Disposition: attachment; filename=\"../../etc/passwd\"\n\nroot\n--z--\n"))
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "passwd" || m.Text != "" {
		t.Fatalf("traversal: %+v", m.Attachments)
	}
	// HTML-only message: the snippet comes from the HTML.
	m = Parse([]byte("Content-Type: text/html; charset=utf-8\n\n<html><head><style>p{}</style></head><body><p>Hello&nbsp;<b>there</b></p><p>> not a quote</p></body></html>\n"))
	if m.Snippet(50) != "Hello there" {
		t.Fatalf("html snippet %q", m.Snippet(50))
	}
	// Deeply nested multiparts stop at the depth limit without panicking.
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=b0\n\n")
	for i := 0; i < 50; i++ {
		b.WriteString("--b" + strconv.Itoa(i) + "\nContent-Type: multipart/mixed; boundary=b" + strconv.Itoa(i+1) + "\n\n")
	}
	Parse([]byte(b.String()))
}

func TestMessageIDs(t *testing.T) {
	for in, want := range map[string][]string{
		"<a@b>":                       {"a@b"},
		" <a@b> (comment) <c@d> ":     {"a@b", "c@d"},
		"a@b":                         {"a@b"},
		"<a@b>\r\n\t<c@d>":            {"a@b", "c@d"},
		"Your message of 1 Jan <x@y>": {"x@y"},
		"garbage":                     nil,
	} {
		if got := MessageIDs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("MessageIDs(%q) = %q, want %q", in, got, want)
		}
	}
}

// b64 encodes s the way mailers do: base64 in 76-character lines.
func b64(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var out strings.Builder
	for len(enc) > 76 {
		out.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	out.WriteString(enc)
	return out.String()
}
