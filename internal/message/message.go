// Package message builds RFC 5322 messages: encoded non-ASCII headers,
// quoted-printable bodies, base64 attachments and CRLF line endings, ready
// for DKIM signing.
package message

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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
	// Attachments with a ContentID are inline (multipart/related); the
	// others are regular attachments (multipart/mixed).
	Attachments []Attachment
}

// Attachment is a file sent with the message, base64-encoded.
type Attachment struct {
	Filename string
	// ContentType defaults to the type for the filename's extension, or
	// application/octet-stream.
	ContentType string
	// ContentID makes the attachment inline: the HTML refers to it as
	// cid:<ContentID>. Without angle brackets.
	ContentID string
	Data      []byte
}

// Limits on attachments.
const (
	MaxAttachments     = 20
	MaxAttachmentBytes = 10 << 20 // decoded, all attachments together
)

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
		if !Storable(v) || strings.ContainsFunc(v, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) {
			return fmt.Errorf("header %q contains a NUL or other control character, or bytes that are not UTF-8", k)
		}
	}
	return nil
}

// Build renders the message. The result is not signed.
//
// Every header line is at most 998 bytes: subjects and display names are
// folded, and Build refuses what cannot be folded to fit (see the Max…
// caps). When the message has HTML but no text, a text/plain part is made
// from the HTML (HTMLToText).
func Build(in Input) ([]byte, error) {
	if err := ValidateHeaders(in.Headers); err != nil {
		return nil, err
	}
	if in.MessageID == "" {
		return nil, errors.New("message: MessageID is required")
	}
	from, err := ParseAddress("from", in.From)
	if err != nil {
		return nil, err
	}
	if len(in.Subject) > MaxSubject {
		return nil, fmt.Errorf("subject is longer than %d bytes", MaxSubject)
	}
	if in.Text == "" && in.HTML != "" {
		in.Text = HTMLToText(in.HTML)
	}
	atts, err := attachmentParts(in.Attachments)
	if err != nil {
		return nil, err
	}

	b := bytes.NewBuffer(make([]byte, 0, estimateSize(in)))
	header(b, "From", formatAddress(from, len("From: ")))
	for _, f := range []struct {
		name string
		list []string
	}{{"To", in.To}, {"Cc", in.Cc}, {"Reply-To", in.ReplyTo}} {
		if len(f.list) == 0 {
			continue
		}
		parts := make([]string, 0, len(f.list))
		for i, a := range f.list {
			addr, err := ParseAddress(strings.ToLower(f.name), a)
			if err != nil {
				return nil, err
			}
			used := 1 // the continuation line's leading space
			if i == 0 {
				used = len(f.name) + 2
			}
			parts = append(parts, formatAddress(addr, used))
		}
		header(b, f.name, strings.Join(parts, ",\r\n "))
	}
	header(b, "Subject", foldText("Subject", in.Subject))
	date := in.Date
	if date.IsZero() {
		date = time.Now()
	}
	header(b, "Date", date.Format(time.RFC1123Z))
	header(b, "Message-ID", "<"+in.MessageID+">")
	header(b, "MIME-Version", "1.0")

	keys := make([]string, 0, len(in.Headers))
	for k := range in.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		header(b, k, encodeText(in.Headers[k]))
	}

	body := bodyPart(in.Text, in.HTML)
	var inline, regular []part
	for i, a := range in.Attachments {
		if a.ContentID != "" {
			inline = append(inline, atts[i])
		} else {
			regular = append(regular, atts[i])
		}
	}
	if len(inline) > 0 {
		body = multipartPart("related", append([]part{body}, inline...))
	}
	if len(regular) > 0 {
		body = multipartPart("mixed", append([]part{body}, regular...))
	}
	for _, h := range body.header {
		header(b, h[0], h[1])
	}
	b.WriteString("\r\n")
	if err := body.write(b); err != nil {
		return nil, err
	}
	if !body.multipart {
		b.WriteString("\r\n")
	}
	return b.Bytes(), nil
}

// part is one MIME entity: its headers, in order, and its body.
type part struct {
	header    [][2]string
	write     func(io.Writer) error
	multipart bool
}

const (
	textType = `text/plain; charset="utf-8"`
	htmlType = `text/html; charset="utf-8"`
)

func textPart(ct, body string) part {
	return part{
		header: [][2]string{{"Content-Type", ct}, {"Content-Transfer-Encoding", "quoted-printable"}},
		write:  func(w io.Writer) error { return writeQP(w, body) },
	}
}

// bodyPart is the text, the HTML, or both as multipart/alternative.
func bodyPart(text, html string) part {
	switch {
	case text != "" && html != "":
		return multipartPart("alternative", []part{textPart(textType, text), textPart(htmlType, html)})
	case html != "":
		return textPart(htmlType, html)
	default:
		return textPart(textType, text)
	}
}

func multipartPart(subtype string, children []part) part {
	boundary := newBoundary()
	return part{
		header:    [][2]string{{"Content-Type", "multipart/" + subtype + `; boundary="` + boundary + `"`}},
		multipart: true,
		write: func(w io.Writer) error {
			for i, c := range children {
				sep := "\r\n--" + boundary + "\r\n"
				if i == 0 {
					sep = sep[2:]
				}
				if _, err := io.WriteString(w, sep); err != nil {
					return err
				}
				for _, h := range c.header {
					if _, err := io.WriteString(w, h[0]+": "+h[1]+"\r\n"); err != nil {
						return err
					}
				}
				if _, err := io.WriteString(w, "\r\n"); err != nil {
					return err
				}
				if err := c.write(w); err != nil {
					return err
				}
			}
			_, err := io.WriteString(w, "\r\n--"+boundary+"--\r\n")
			return err
		},
	}
}

// newBoundary returns a random multipart boundary. The "=_" cannot occur
// in quoted-printable or base64 output, so no part can contain it.
func newBoundary() string {
	b := make([]byte, 15)
	rand.Read(b)
	return "=_omw_" + hex.EncodeToString(b)
}

var contentIDRE = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+/=?^_`{|}~.@-]{1,250}$")

// ValidateAttachments checks attachments against the limits and refuses
// names, types and content IDs that cannot be written safely.
func ValidateAttachments(atts []Attachment) error {
	_, err := attachmentParts(atts)
	return err
}

// estimateSize is about how large the rendered message is, so that it is
// written into one buffer instead of one that keeps growing (and copying).
func estimateSize(in Input) int {
	n := 4096 + len(in.Subject)*2
	for _, l := range [][]string{in.To, in.Cc, in.ReplyTo} {
		for _, a := range l {
			n += len(a)*2 + 8
		}
	}
	for k, v := range in.Headers {
		n += len(k) + len(v)*2 + 4
	}
	// Quoted-printable grows typical text by a few percent.
	n += (len(in.Text) + len(in.HTML)) * 21 / 20
	for _, a := range in.Attachments {
		enc := base64.StdEncoding.EncodedLen(len(a.Data))
		n += enc + enc/76*2 + 512
	}
	return n
}

func attachmentParts(atts []Attachment) ([]part, error) {
	if len(atts) > MaxAttachments {
		return nil, fmt.Errorf("at most %d attachments per email", MaxAttachments)
	}
	total := 0
	cids := map[string]bool{}
	parts := make([]part, 0, len(atts))
	for i, a := range atts {
		total += len(a.Data)
		if total > MaxAttachmentBytes {
			return nil, fmt.Errorf("attachments total more than %d MB decoded; link large files instead", MaxAttachmentBytes>>20)
		}
		if err := validFilename(a.Filename); err != nil {
			return nil, fmt.Errorf("attachments[%d]: %w", i, err)
		}
		ct, err := attachmentType(a)
		if err != nil {
			return nil, fmt.Errorf("attachments[%d]: %w", i, err)
		}
		disposition := "attachment"
		h := [][2]string{{"Content-Type", ct}}
		if a.ContentID != "" {
			if !contentIDRE.MatchString(a.ContentID) {
				return nil, fmt.Errorf("attachments[%d]: content_id %q may hold only letters, digits and !#$%%&'*+/=?^_`{|}~.@- (1-250 characters), without angle brackets", i, a.ContentID)
			}
			if cids[a.ContentID] {
				return nil, fmt.Errorf("attachments[%d]: content_id %q is used twice", i, a.ContentID)
			}
			cids[a.ContentID] = true
			disposition = "inline"
			h = append(h, [2]string{"Content-ID", "<" + a.ContentID + ">"})
		}
		h = append(h,
			[2]string{"Content-Disposition", fold(mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}))},
			[2]string{"Content-Transfer-Encoding", "base64"})
		data := a.Data
		parts = append(parts, part{header: h, write: func(w io.Writer) error { return writeBase64(w, data) }})
	}
	return parts, nil
}

func validFilename(name string) error {
	switch {
	case name == "":
		return errors.New("filename is required")
	case len(name) > 255:
		return errors.New("filename is longer than 255 bytes")
	case !utf8.ValidString(name):
		return errors.New("filename is not valid UTF-8")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("filename contains a control character")
		}
	}
	return nil
}

// attachmentType returns the Content-Type header value, with the name
// parameter that older mail clients use.
func attachmentType(a Attachment) (string, error) {
	if err := checkContentType(a.ContentType); err != nil {
		return "", err
	}
	ct := a.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(strings.ToLower(path.Ext(a.Filename)))
		if ct == "" {
			ct = "application/octet-stream"
		}
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.Contains(mt, "/") {
		return "", fmt.Errorf("content_type %q is not a media type like application/pdf", a.ContentType)
	}
	if strings.HasPrefix(mt, "multipart/") || strings.HasPrefix(mt, "message/") {
		return "", fmt.Errorf("content_type %s cannot be sent as an attachment; use application/octet-stream", mt)
	}
	params["name"] = a.Filename
	v := mime.FormatMediaType(mt, params)
	if v == "" {
		return "", fmt.Errorf("content_type %q is not a media type like application/pdf", a.ContentType)
	}
	return fold(v), nil
}

// fold puts each parameter of a header value on its own line. Unfolding
// gives back exactly the same value.
func fold(v string) string { return strings.ReplaceAll(v, "; ", ";\r\n ") }

// writeBase64 writes data in 76-character lines (RFC 2045), a few lines at
// a time, so that a large attachment is never copied whole.
func writeBase64(w io.Writer, data []byte) error {
	const perLine = 57 // input bytes per 76-character line
	const lines = 64
	var buf [lines * 78]byte
	for len(data) > 0 {
		n := 0
		for l := 0; l < lines && len(data) > 0; l++ {
			chunk := data[:min(perLine, len(data))]
			data = data[len(chunk):]
			base64.StdEncoding.Encode(buf[n:], chunk)
			n += base64.StdEncoding.EncodedLen(len(chunk))
			if len(data) > 0 {
				buf[n], buf[n+1] = '\r', '\n'
				n += 2
			}
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
	}
	return nil
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
