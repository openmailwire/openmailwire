// Package message builds RFC 5322 messages: encoded non-ASCII headers,
// quoted-printable bodies and CRLF line endings, ready for DKIM signing.
package message

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"
)

type Input struct {
	From      string
	To        []string
	Cc        []string
	ReplyTo   []string
	Subject   string
	Text      string
	HTML      string
	Headers   map[string]string
	MessageID string // without angle brackets
	Date      time.Time
}

// Headers OpenMailWire writes itself. Callers cannot set them.
var reserved = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "reply-to": true,
	"subject": true, "date": true, "message-id": true, "mime-version": true,
	"content-type": true, "content-transfer-encoding": true,
	"dkim-signature": true, "return-path": true, "received": true, "sender": true,
}

// ValidateHeaders checks caller-supplied extra headers.
func ValidateHeaders(h map[string]string) error {
	for k, v := range h {
		if k == "" || len(k) > 76 {
			return fmt.Errorf("header name %q is invalid", k)
		}
		for i := 0; i < len(k); i++ {
			if c := k[i]; c <= ' ' || c >= 0x7f || c == ':' {
				return fmt.Errorf("header name %q is invalid", k)
			}
		}
		if reserved[strings.ToLower(k)] {
			return fmt.Errorf("header %q is set by openmailwire and cannot be overridden", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q contains a line break", k)
		}
		if len(v) > 900 {
			return fmt.Errorf("header %q is longer than 900 bytes", k)
		}
	}
	return nil
}

// Build renders the message. The result is not signed.
func Build(in Input) ([]byte, error) {
	if err := ValidateHeaders(in.Headers); err != nil {
		return nil, err
	}
	if in.MessageID == "" {
		return nil, errors.New("message: MessageID is required")
	}
	from, err := mail.ParseAddress(in.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}

	var b bytes.Buffer
	header(&b, "From", from.String())
	for _, f := range []struct {
		name string
		list []string
	}{{"To", in.To}, {"Cc", in.Cc}, {"Reply-To", in.ReplyTo}} {
		if len(f.list) == 0 {
			continue
		}
		parts := make([]string, 0, len(f.list))
		for _, a := range f.list {
			addr, err := mail.ParseAddress(a)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", strings.ToLower(f.name), err)
			}
			parts = append(parts, addr.String())
		}
		header(&b, f.name, strings.Join(parts, ",\r\n "))
	}
	header(&b, "Subject", encodeText(in.Subject))
	date := in.Date
	if date.IsZero() {
		date = time.Now()
	}
	header(&b, "Date", date.Format(time.RFC1123Z))
	header(&b, "Message-ID", "<"+in.MessageID+">")
	header(&b, "MIME-Version", "1.0")

	keys := make([]string, 0, len(in.Headers))
	for k := range in.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		header(&b, k, encodeText(in.Headers[k]))
	}

	const textType = `text/plain; charset="utf-8"`
	const htmlType = `text/html; charset="utf-8"`
	if in.Text != "" && in.HTML != "" {
		mw := multipart.NewWriter(&b)
		header(&b, "Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
		b.WriteString("\r\n")
		for _, p := range []struct{ ct, body string }{{textType, in.Text}, {htmlType, in.HTML}} {
			w, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {p.ct},
				"Content-Transfer-Encoding": {"quoted-printable"},
			})
			if err != nil {
				return nil, err
			}
			if err := writeQP(w, p.body); err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}

	ct, body := textType, in.Text
	if in.HTML != "" {
		ct, body = htmlType, in.HTML
	}
	header(&b, "Content-Type", ct)
	header(&b, "Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	if err := writeQP(&b, body); err != nil {
		return nil, err
	}
	b.WriteString("\r\n")
	return b.Bytes(), nil
}

func header(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}

// encodeText returns s unchanged when it is printable ASCII, otherwise as
// RFC 2047 encoded-words folded onto continuation lines.
func encodeText(s string) string {
	if isPrintableASCII(s) {
		return s
	}
	enc := mime.BEncoding.Encode("utf-8", s)
	return strings.ReplaceAll(enc, "?= =?", "?=\r\n =?")
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t') || c > 0x7e {
			return false
		}
	}
	return true
}

func writeQP(w io.Writer, s string) error {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	q := quotedprintable.NewWriter(w)
	if _, err := io.WriteString(q, s); err != nil {
		return err
	}
	return q.Close()
}
