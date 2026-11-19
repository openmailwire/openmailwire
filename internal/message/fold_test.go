package message

import (
	"bytes"
	"io"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func longestLine(b []byte) (int, string) {
	max, which := 0, ""
	for _, l := range bytes.Split(b, []byte("\r\n")) {
		if len(l) > max {
			max, which = len(l), string(l)
			if len(which) > 40 {
				which = which[:40] + "..."
			}
		}
	}
	return max, which
}

// Audit R3#10 (and R1#9): Build wrote header fields on one line without
// folding: a 998-byte subject gave a 1,007-byte line, long display names
// lines of 1,222 bytes and more, a long Persian name 1,834 bytes, a long
// content_type 1,108 bytes. Every line must be at most 998 bytes, and what
// cannot be folded to fit is refused with a reason.
func TestBuildNeverWritesLinesOver998(t *testing.T) {
	persian := func(n int) string { return strings.TrimSpace(strings.Repeat("محمد ", n/5)) }
	cases := []struct {
		name    string
		in      Input
		refused string // "" when Build must succeed
	}{
		{name: "ascii subject of 998 bytes", in: Input{Subject: strings.Repeat("a", 998)}},
		{name: "ascii subject of 998 bytes with spaces", in: Input{Subject: strings.Repeat("word ", 200)[:998]}},
		{name: "non-ASCII subject of 998 bytes", in: Input{Subject: strings.Repeat("ب", 499)}},
		{name: "ascii display name of 256 characters in To", in: Input{To: []string{`"` + strings.Repeat("A", 256) + `" <x@example.com>`}}},
		{name: "ascii display name of 1200 characters in To", in: Input{To: []string{`"` + strings.Repeat("A", 1200) + `" <x@example.com>`}},
			refused: "display name"},
		{name: "Persian display name of 250 characters in From", in: Input{From: persian(250) + " <hello@acme.test>"}},
		{name: "Persian display name of 300 characters in From", in: Input{From: persian(300) + " <hello@acme.test>"},
			refused: "display name"},
		{name: "many recipients with long names", in: Input{To: func() []string {
			var l []string
			for i := 0; i < 50; i++ {
				l = append(l, persian(100)+" <r"+strings.Repeat("x", 60)+"@example.com>")
			}
			return l
		}()}},
		{name: "attachment content_type of 255 characters", in: Input{Attachments: []Attachment{{Filename: "a.pdf",
			ContentType: `application/pdf; x-note="` + strings.Repeat("n", 229) + `"`, Data: []byte("x")}}}},
		{name: "attachment content_type parameter of 1100 characters", in: Input{Attachments: []Attachment{{Filename: "a.pdf",
			ContentType: `application/pdf; x-note="` + strings.Repeat("n", 1100) + `"`, Data: []byte("x")}}}, refused: "content_type"},
		{name: "non-ASCII filename of 255 bytes", in: Input{Attachments: []Attachment{{Filename: strings.Repeat("ف", 127), Data: []byte("x")}}}},
		{name: "address over 254", in: Input{To: []string{strings.Repeat("l", 64) + "@" + strings.Repeat("d", 190) + ".com"}}, refused: "longer than 254"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			if in.From == "" {
				in.From = "hello@acme.test"
			}
			if in.To == nil {
				in.To = []string{"x@example.com"}
			}
			if in.Subject == "" {
				in.Subject = "s"
			}
			in.Text, in.MessageID, in.Date = "hi", "id@acme.test", time.Now()
			raw, err := Build(in)
			if c.refused != "" {
				if err == nil || !strings.Contains(err.Error(), c.refused) {
					n, _ := longestLine(raw)
					t.Fatalf("want a refusal about %q, got err=%v (longest line %d)", c.refused, err, n)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n, l := longestLine(raw); n > 998 {
				t.Fatalf("a %d-byte line (%q)", n, l)
			}
			// Unfolding and decoding give back what was asked for.
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
			if err != nil || subject != in.Subject {
				t.Fatalf("subject round trip: %v\ngot  %q\nwant %q", err, subject, in.Subject)
			}
			from, err := msg.Header.AddressList("From")
			want, _ := mail.ParseAddress(in.From)
			if err != nil || len(from) != 1 || from[0].Name != want.Name || from[0].Address != want.Address {
				t.Fatalf("from round trip: %v %v", from, err)
			}
			to, err := msg.Header.AddressList("To")
			if err != nil || len(to) != len(in.To) {
				t.Fatalf("to round trip: %d addresses, %v", len(to), err)
			}
			for i, a := range in.To {
				want, _ := mail.ParseAddress(a)
				if to[i].Name != want.Name || to[i].Address != want.Address {
					t.Fatalf("to[%d]: got %q <%s>", i, to[i].Name, to[i].Address)
				}
			}
		})
	}
}

func TestFoldTextKeepsShortValues(t *testing.T) {
	if got := foldText("Subject", "Hello"); got != "Hello" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("hello world ", 20)
	got := foldText("Subject", long)
	if strings.ReplaceAll(got, "\r\n", "") != long {
		t.Fatalf("unfolding changed the text: %q", got)
	}
	for _, l := range strings.Split("Subject: "+got, "\r\n") {
		if len(l) > 78 {
			t.Fatalf("line of %d bytes: %q", len(l), l)
		}
	}
}

func TestHTMLOnlyGetsTextPart(t *testing.T) {
	raw, err := Build(Input{
		From: "a@acme.test", To: []string{"b@example.test"}, Subject: "Hi", MessageID: "x@acme.test",
		HTML: `<html><head><title>T</title><style>p{color:red}</style></head><body>
			<h1>Welcome,&nbsp;Rahel</h1><p>Your code is <b>123456</b>.</p>
			<p>Read <a href="https://acme.test/terms">the terms</a> or <a href="#top">go up</a>.</p>
			<ul><li>one</li><li>two</li></ul><script>alert(1)</script><!-- hidden --></body></html>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	root, leaves := parse(t, raw)
	if root != "multipart/alternative[text/plain,text/html]" {
		t.Fatalf("structure %s", root)
	}
	text := strings.ReplaceAll(string(leaves[0].body), "\r\n", "\n")
	want := "Welcome, Rahel\n\nYour code is 123456.\n\nRead the terms (https://acme.test/terms) or go up.\n\n- one\n- two"
	if text != want {
		t.Fatalf("text part:\n%q\nwant\n%q", text, want)
	}
}

func TestHTMLToText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", "plain"},
		{"a<br>b<br/>c", "a\nb\nc"},
		{"<p>one</p><p>two</p>", "one\n\ntwo"},
		{"x &amp; y &lt;z&gt;", "x & y <z>"},
		{"<pre>  keep\n    this</pre>", "  keep\n    this"},
		{`<img src="a.png" alt="Logo"> text`, "Logo text"},
		{`<a href="mailto:ops@acme.test">write</a>`, "write (ops@acme.test)"},
		{`<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr></table>`, "a b\nc"},
		{`<p title="x>y">quoted</p>`, "quoted"},
		{"<SCRIPT>bad()</SCRIPT>ok", "ok"},
		{"unterminated <b", "unterminated"},
	} {
		if got := HTMLToText(c.in); got != c.want {
			t.Errorf("HTMLToText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLargeAttachmentBase64Lines(t *testing.T) {
	data := bytes.Repeat([]byte{0xfe, 0x01, 0x7a}, 1<<16)
	raw, err := Build(Input{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", Text: "t",
		MessageID: "x@acme.test", Attachments: []Attachment{{Filename: "a.bin", Data: data}}})
	if err != nil {
		t.Fatal(err)
	}
	_, leaves := parse(t, raw)
	got, _ := io.ReadAll(bytes.NewReader(leaves[1].body))
	if !bytes.Equal(got, data) {
		t.Fatalf("attachment did not round trip: %d bytes, want %d", len(got), len(data))
	}
	if n, l := longestLine(raw); n > 76 && !strings.HasPrefix(l, "Content-") && !strings.HasPrefix(l, "Subject") {
		t.Fatalf("a %d-byte line: %q", n, l)
	}
}
