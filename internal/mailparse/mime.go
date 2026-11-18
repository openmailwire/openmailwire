// Package mailparse reads mail that arrives from the internet: messages for
// inboxes, delivery status notifications (RFC 3464) and complaint reports
// (ARF, RFC 5965).
//
// Received mail is often malformed, so every parser here is lenient: it
// never fails on bad syntax, it extracts what it can, and it bounds the work
// one message can cause (MIME depth and part count).
package mailparse

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

const (
	maxDepth = 8
	maxParts = 200
)

// field is one header field, unfolded.
type field struct{ name, value string }

// header keeps fields in order. Lookups ignore case.
type header []field

func (h header) get(name string) string {
	for _, f := range h {
		if strings.EqualFold(f.name, name) {
			return strings.TrimSpace(f.value)
		}
	}
	return ""
}

func (h header) all(name string) []string {
	var out []string
	for _, f := range h {
		if strings.EqualFold(f.name, name) {
			out = append(out, strings.TrimSpace(f.value))
		}
	}
	return out
}

// splitMessage separates the header block from the body. Without a blank
// line, everything is header (as in text/rfc822-headers).
func splitMessage(raw []byte) (head, body []byte) {
	if len(raw) > 0 && (raw[0] == '\n' || bytes.HasPrefix(raw, []byte("\r\n"))) {
		_, rest, _ := bytes.Cut(raw, []byte("\n"))
		return nil, rest
	}
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	lf := bytes.Index(raw, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return raw[:crlf], raw[crlf+4:]
	case lf >= 0:
		return raw[:lf], raw[lf+2:]
	}
	return raw, nil
}

// parseHeader reads header fields leniently: lines that are neither a field
// nor a continuation are skipped.
func parseHeader(head []byte) header {
	var h header
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(h) > 0 {
				h[len(h)-1].value += line
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		h = append(h, field{name: name, value: value})
	}
	return h
}

var (
	paramRE = regexp.MustCompile(`(?i)\b(boundary|charset|name|filename|report-type)\s*=\s*("([^"]*)"|[^;\s]+)`)
)

// mediaType parses a Content-Type or Content-Disposition value. When the
// value is malformed it falls back to the token before ';' and the
// parameters it can find.
func mediaType(v string) (string, map[string]string) {
	if v == "" {
		return "", map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(v)
	if err == nil {
		return strings.ToLower(mt), params
	}
	mt, _, _ = strings.Cut(v, ";")
	params = map[string]string{}
	for _, m := range paramRE.FindAllStringSubmatch(v, -1) {
		val := m[2]
		if m[3] != "" || strings.HasPrefix(val, `"`) {
			val = m[3]
		}
		params[strings.ToLower(m[1])] = val
	}
	return strings.ToLower(strings.TrimSpace(mt)), params
}

// decodeTransfer undoes a Content-Transfer-Encoding. Damaged input yields
// whatever could be decoded.
func decodeTransfer(cte string, b []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		clean := make([]byte, 0, len(b))
		for _, c := range b {
			if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
				clean = append(clean, c)
			}
		}
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil {
			// Missing padding is common; try without it.
			out2 := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
			if n2, err2 := base64.RawStdEncoding.Decode(out2, bytes.TrimRight(clean, "=")); err2 == nil {
				return out2[:n2]
			}
		}
		return out[:n]
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		if err != nil && len(out) == 0 {
			return b
		}
		return out
	}
	return b
}

// toUTF8 converts text in the given charset to UTF-8. Unknown charsets and
// invalid bytes become U+FFFD rather than an error.
func toUTF8(charset string, b []byte) string {
	cs := strings.ToLower(strings.Trim(charset, `"' `))
	// 8-bit text labelled us-ascii is almost always UTF-8 in practice.
	if cs == "" || cs == "utf-8" || cs == "utf8" || (cs == "us-ascii" || cs == "ascii") && utf8.Valid(b) {
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "\uFFFD")
	}
	return strings.ToValidUTF8(string(out), "\uFFFD")
}

var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(strings.ToLower(charset))
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(input), nil
}}

// decodeHeader decodes RFC 2047 encoded-words, keeping the value as it is
// when that fails. Raw 8-bit header bytes are taken as UTF-8.
func decodeHeader(v string) string {
	out, err := wordDecoder.DecodeHeader(v)
	if err != nil {
		out = v
	}
	return fixC1(strings.ToValidUTF8(out, "\uFFFD"))
}

// fixC1 maps C1 control characters to their windows-1252 meaning. The
// standard library decodes "iso-8859-1" words literally, but mailers that
// say iso-8859-1 mean windows-1252, as browsers assume.
func fixC1(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= 0x80 && r <= 0x9f }) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r >= 0x80 && r <= 0x9f {
			return charmap.Windows1252.DecodeByte(byte(r))
		}
		return r
	}, s)
}

var looseAddrRE = regexp.MustCompile(`[^\s<>"(),;:\[\]]+@[^\s<>"(),;:\[\]]+`)

// parseAddresses reads an address list. When the list does not parse, it
// falls back to anything shaped like an address.
func parseAddresses(v string) []*mail.Address {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	p := mail.AddressParser{WordDecoder: wordDecoder}
	if list, err := p.ParseList(v); err == nil {
		return list
	}
	var out []*mail.Address
	for _, a := range looseAddrRE.FindAllString(v, -1) {
		out = append(out, &mail.Address{Address: a})
	}
	return out
}

// FormatAddress renders an address for display and for reuse in the send
// API: "Name <addr>", with the name quoted when it needs to be.
func FormatAddress(a *mail.Address) string {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return a.Address
	}
	if strings.ContainsAny(name, "\"()<>[]:;@\\,.") {
		name = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name + " <" + a.Address + ">"
}

// formatAddresses formats an address list.
func formatAddresses(list []*mail.Address) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, FormatAddress(a))
	}
	return out
}

var msgIDRE = regexp.MustCompile(`<([^<>\s]+)>`)

// MessageIDs extracts message identifiers (without angle brackets) from a
// Message-ID, In-Reply-To or References value.
func MessageIDs(v string) []string {
	var out []string
	for _, m := range msgIDRE.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		if t := strings.TrimSpace(v); t != "" && strings.Contains(t, "@") && !strings.ContainsAny(t, " \t<>") {
			out = append(out, t)
		}
	}
	return out
}

// part is one leaf of a MIME tree, with its transfer encoding undone.
type part struct {
	header      header
	mediaType   string
	params      map[string]string
	disposition string
	dispParams  map[string]string
	body        []byte
}

func (p part) filename() string {
	name := p.dispParams["filename"]
	if name == "" {
		name = p.params["name"]
	}
	return decodeHeader(name)
}

func (p part) text() string { return toUTF8(p.params["charset"], p.body) }

// walk visits every leaf of a message. Embedded messages (message/rfc822
// and friends) are leaves: walk does not descend into them.
func walk(raw []byte, visit func(part)) {
	head, body := splitMessage(raw)
	count := 0
	walkPart(parseHeader(head), body, 0, &count, visit)
}

func walkPart(h header, body []byte, depth int, count *int, visit func(part)) {
	*count++
	if *count > maxParts {
		return
	}
	mt, params := mediaType(h.get("Content-Type"))
	if mt == "" {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" && depth < maxDepth {
		for _, sub := range splitMultipart(body, params["boundary"]) {
			sh, sb := splitMessage(sub)
			walkPart(parseHeader(sh), sb, depth+1, count, visit)
		}
		return
	}
	disp, dparams := mediaType(h.get("Content-Disposition"))
	visit(part{
		header: h, mediaType: mt, params: params, disposition: disp, dispParams: dparams,
		body: decodeTransfer(h.get("Content-Transfer-Encoding"), body),
	})
}

// splitMultipart returns the body parts between boundary delimiters. A
// missing closing delimiter ends the last part at the end of the body.
func splitMultipart(body []byte, boundary string) [][]byte {
	delim := "--" + boundary
	var parts [][]byte
	start := -1
	for off := 0; off < len(body); {
		end := bytes.IndexByte(body[off:], '\n')
		next := len(body)
		line := body[off:]
		if end >= 0 {
			line = body[off : off+end]
			next = off + end + 1
		}
		t := strings.TrimRight(string(line), " \t\r")
		if t == delim || t == delim+"--" {
			if start >= 0 {
				stop := off
				if stop >= 2 && body[stop-2] == '\r' && body[stop-1] == '\n' {
					stop -= 2
				} else if stop >= 1 && body[stop-1] == '\n' {
					stop--
				}
				if stop < start {
					stop = start
				}
				parts = append(parts, body[start:stop])
			}
			if t == delim+"--" {
				return parts
			}
			start = next
		}
		off = next
	}
	if start >= 0 && start < len(body) {
		parts = append(parts, body[start:])
	}
	return parts
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
