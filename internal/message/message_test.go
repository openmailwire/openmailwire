package message

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
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

// tree parses a MIME entity into "type[child,child]" and collects the
// decoded leaf parts.
type leaf struct {
	header textproto.MIMEHeader
	body   []byte
}

func tree(t *testing.T, ct string, body io.Reader, leaves *[]leaf, header textproto.MIMEHeader) string {
	t.Helper()
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("content-type %q: %v", ct, err)
	}
	if !strings.HasPrefix(mt, "multipart/") {
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		switch header.Get("Content-Transfer-Encoding") {
		case "base64":
			for _, line := range strings.Split(string(data), "\r\n") {
				if len(line) > 76 {
					t.Fatalf("base64 line of %d characters", len(line))
				}
			}
			if data, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(string(data), "\r\n", "")); err != nil {
				t.Fatal(err)
			}
		case "quoted-printable":
			if data, err = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data))); err != nil {
				t.Fatal(err)
			}
		}
		*leaves = append(*leaves, leaf{header, data})
		return mt
	}
	mr := multipart.NewReader(body, params["boundary"])
	var kids []string
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		kids = append(kids, tree(t, p.Header.Get("Content-Type"), p, leaves, p.Header))
	}
	return mt + "[" + strings.Join(kids, ",") + "]"
}

func parse(t *testing.T, raw []byte) (string, []leaf) {
	t.Helper()
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
	var leaves []leaf
	shape := tree(t, msg.Header.Get("Content-Type"), msg.Body, &leaves, textproto.MIMEHeader(msg.Header))
	return shape, leaves
}

func TestBuildAttachmentStructure(t *testing.T) {
	logo := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0, 1, 2, 0xff}, 100)
	pdf := []byte("%PDF-1.4 a small file")
	base := Input{
		From: "a@acme.test", To: []string{"b@example.test"}, Subject: "Report", MessageID: "x@acme.test",
		Text: "See the report.", HTML: `<p>See the report.</p><img src="cid:logo">`,
	}
	cases := []struct {
		name  string
		atts  []Attachment
		shape string
	}{
		{"none", nil, "multipart/alternative[text/plain,text/html]"},
		{"regular", []Attachment{{Filename: "report.pdf", Data: pdf}},
			"multipart/mixed[multipart/alternative[text/plain,text/html],application/pdf]"},
		{"inline", []Attachment{{Filename: "logo.png", ContentID: "logo", Data: logo}},
			"multipart/related[multipart/alternative[text/plain,text/html],image/png]"},
		{"both", []Attachment{{Filename: "report.pdf", Data: pdf}, {Filename: "logo.png", ContentID: "logo", Data: logo}},
			"multipart/mixed[multipart/related[multipart/alternative[text/plain,text/html],image/png],application/pdf]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			in.Attachments = c.atts
			raw, err := Build(in)
			if err != nil {
				t.Fatal(err)
			}
			shape, leaves := parse(t, raw)
			if shape != c.shape {
				t.Fatalf("structure %s, want %s", shape, c.shape)
			}
			if string(leaves[0].body) != "See the report." {
				t.Fatalf("text part: %q", leaves[0].body)
			}
			for _, l := range leaves[2:] {
				_, params, _ := mime.ParseMediaType(l.header.Get("Content-Disposition"))
				switch params["filename"] {
				case "report.pdf":
					if !bytes.Equal(l.body, pdf) || !strings.HasPrefix(l.header.Get("Content-Disposition"), "attachment") ||
						l.header.Get("Content-ID") != "" {
						t.Fatalf("pdf part: %v %q", l.header, l.body)
					}
				case "logo.png":
					if !bytes.Equal(l.body, logo) || !strings.HasPrefix(l.header.Get("Content-Disposition"), "inline") ||
						l.header.Get("Content-ID") != "<logo>" {
						t.Fatalf("logo part: %v", l.header)
					}
				default:
					t.Fatalf("unexpected part %v", l.header)
				}
			}
		})
	}
}

func TestBuildAttachmentTextOnlyAndNames(t *testing.T) {
	name := "گزارش ماه; نسخه \"نهایی\".csv"
	raw, err := Build(Input{
		From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", MessageID: "x@acme.test", Text: "hi",
		Attachments: []Attachment{
			{Filename: name, Data: []byte("a,b\n1,2\n")},
			{Filename: "blob", ContentType: "application/vnd.acme+json; charset=utf-8", Data: make([]byte, 5000)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	shape, leaves := parse(t, raw)
	if shape != "multipart/mixed[text/plain,text/csv,application/vnd.acme+json]" {
		t.Fatalf("structure %s", shape)
	}
	_, params, err := mime.ParseMediaType(leaves[1].header.Get("Content-Disposition"))
	if err != nil || params["filename"] != name {
		t.Fatalf("filename round trip: %q %v", params["filename"], err)
	}
	_, params, _ = mime.ParseMediaType(leaves[2].header.Get("Content-Type"))
	if params["charset"] != "utf-8" || params["name"] != "blob" || len(leaves[2].body) != 5000 {
		t.Fatalf("blob part: %v %d", leaves[2].header, len(leaves[2].body))
	}
}

func TestValidateAttachments(t *testing.T) {
	many := make([]Attachment, MaxAttachments+1)
	for i := range many {
		many[i] = Attachment{Filename: "f.txt", Data: []byte("x")}
	}
	bad := map[string][]Attachment{
		"too many":      many,
		"too big":       {{Filename: "a.bin", Data: make([]byte, MaxAttachmentBytes/2+1)}, {Filename: "b.bin", Data: make([]byte, MaxAttachmentBytes/2)}},
		"no name":       {{Data: []byte("x")}},
		"control":       {{Filename: "a\r\nX-Injected: 1", Data: []byte("x")}},
		"cid brackets":  {{Filename: "a.png", ContentID: "<a>", Data: []byte("x")}},
		"cid space":     {{Filename: "a.png", ContentID: "a b", Data: []byte("x")}},
		"cid twice":     {{Filename: "a.png", ContentID: "a", Data: []byte("x")}, {Filename: "b.png", ContentID: "a", Data: []byte("x")}},
		"bad type":      {{Filename: "a", ContentType: "not a type", Data: []byte("x")}},
		"multipart":     {{Filename: "a", ContentType: "multipart/mixed; boundary=x", Data: []byte("x")}},
		"message":       {{Filename: "a.eml", ContentType: "message/rfc822", Data: []byte("x")}},
		"type no slash": {{Filename: "a", ContentType: "text", Data: []byte("x")}},
	}
	for name, atts := range bad {
		if err := ValidateAttachments(atts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := []Attachment{{Filename: "a.png", ContentID: "logo@acme.test", Data: make([]byte, MaxAttachmentBytes)}}
	if err := ValidateAttachments(ok); err != nil {
		t.Fatal(err)
	}
}
