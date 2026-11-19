package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openmailwire/openmailwire/internal/message"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

// The POST /emails/raw body limit is maxSendBody (send.go, 20 MB). Base64
// adds a third, so the message itself can be about 15 MB.

// maxLineLength is RFC 5322's limit on a line, without its CRLF.
const maxLineLength = 998

type rawRequest struct {
	Raw      jsonSpan `json:"raw"` // base64; decoded straight from the request body
	MailFrom string   `json:"mail_from"`
	Rcpts    []string `json:"rcpts"`
}

// decodeRaw decodes the raw field without first copying it into a string.
func decodeRaw(span jsonSpan) ([]byte, error) {
	if span.isNull() || string(span) == `""` {
		return nil, errNoContent
	}
	if span[0] != '"' {
		return nil, errors.New("raw must be a string")
	}
	inner := []byte(span[1 : len(span)-1])
	if bytes.IndexByte(inner, '\\') >= 0 {
		var s string
		if err := json.Unmarshal(span, &s); err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(s)
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(inner)))
	n, err := base64.StdEncoding.Decode(out, inner)
	return out[:n], err
}

// sendRaw queues a complete RFC 5322 message, as written by a mail library
// or relayed by the SMTP submission server. The message is kept as sent,
// except that Bcc is removed, Message-ID and Date are added when missing,
// line endings become CRLF, and a DKIM signature is added on top.
func (s *Server) sendRaw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req rawRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	idem, pe := idempotencyKey(r)
	if pe != nil {
		pe.write(w)
		return
	}
	hash := sha256.Sum256(body)
	if s.replayed(w, r, idem, hash[:]) {
		return
	}
	data, err := decodeRaw(req.Raw)
	if errors.Is(err, errNoContent) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "raw is required: the whole message (headers and body), base64-encoded")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "raw must be standard base64 (with padding): "+err.Error())
		return
	}
	req.Raw = nil // decoded; let the request body go
	msg, err := parseRaw(data)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if req.MailFrom != "" {
		if _, err := mail.ParseAddress(req.MailFrom); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", fmt.Sprintf("mail_from %q: %v", req.MailFrom, err))
			return
		}
	}

	var recipients []string
	if len(req.Rcpts) > 0 {
		recipients, err = envelope(req.Rcpts)
	} else {
		recipients, err = envelope(msg.to, msg.cc, msg.bcc)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if len(recipients) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"the message has no recipients: pass rcpts, or address it with To, Cc or Bcc")
		return
	}
	from := strings.ToLower(msg.from.Address)
	if pe := s.authorizeSend(r, from, recipients); pe != nil {
		pe.write(w)
		return
	}

	dom, pe := s.senderDomain(ctx, projectID(r), domainOf(from))
	if pe != nil {
		pe.write(w)
		return
	}

	id := secret.UUID()
	now := time.Now().UTC()
	messageID := msg.messageID
	if messageID == "" {
		messageID = id + "@" + dom.Name
	}
	signed, pe := s.signMessage(dom, msg.render(messageID, now))
	if pe != nil {
		pe.write(w)
		return
	}

	// Bcc is whoever gets the message without being named in To or Cc.
	bcc := msg.bcc
	if len(req.Rcpts) > 0 {
		named := map[string]bool{}
		for _, l := range [][]string{msg.to, msg.cc} {
			for _, a := range l {
				named[strings.ToLower(a)] = true
			}
		}
		bcc = nil
		for _, a := range recipients {
			if !named[a] {
				bcc = append(bcc, a)
			}
		}
	}

	// queue sets the project and API key, and re-checks the key's caps.
	out, pe := s.queue(r, []store.NewEmail{{
		ID: id, DomainID: dom.ID, From: displayAddress(msg.from),
		To: msg.to, Cc: msg.cc, Bcc: bcc, Subject: msg.subject,
		MessageID: messageID, ReturnPath: dom.ReturnPath,
		IdempotencyKey: idem, RequestHash: hash[:], Raw: s.Sealer.Seal(signed),
		RetryUncertain: true, ExpiresAt: now.Add(s.Cfg.DefaultExpiry), Recipients: recipients,
	}})
	if pe != nil {
		pe.write(w)
		return
	}
	if out[0].Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": out[0].ID})
}

// rawMessage is a parsed message whose header fields are kept byte for
// byte, so that sending changes nothing but what sendRaw documents.
type rawMessage struct {
	data       []byte   // the whole message, CRLF line endings
	blank      bool     // data has the blank line that ends the header
	fields     [][]byte // each field with its folded lines and CRLFs
	body       []byte   // after the blank line
	from       *mail.Address
	to, cc     []string // addresses only
	bcc        []string
	subject    string
	messageID  string // without angle brackets; "" when missing
	hasDate    bool
	dropFields map[int]bool
}

// parseRaw normalizes line endings to CRLF, checks line lengths and parses
// the header. Its errors are written for the sender to act on.
func parseRaw(data []byte) (*rawMessage, error) {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("the message contains a NUL byte; encode binary content with base64 in a MIME part")
	}
	data = toCRLF(data)
	for n, line := 1, data; len(line) > 0; n++ {
		i := bytes.Index(line, []byte("\r\n"))
		if i > maxLineLength {
			return nil, fmt.Errorf("line %d is %d bytes long, but SMTP allows at most %d; encode long lines with quoted-printable or base64 (mail libraries do this for you)",
				n, i, maxLineLength)
		}
		line = line[i+2:]
	}

	head, body := data, []byte(nil)
	if bytes.HasPrefix(data, []byte("\r\n")) {
		return nil, errors.New("the message has no header: it must start with header fields such as From, To and Subject")
	}
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		head, body = data[:i+2], data[i+4:]
	}

	m := &rawMessage{data: data, blank: len(body) > 0 || bytes.HasSuffix(data, []byte("\r\n\r\n")),
		body: body, dropFields: map[int]bool{}}
	// Every field, by its lowercase name, unfolded. These names are exact:
	// a name with whitespace before its colon is refused below, because Go's
	// textproto would keep "From " apart from "From", while DKIM signers and
	// mail clients treat it as a second From (audit R1#3).
	values := map[string][]string{}
	for n, rest := 1, head; len(rest) > 0; n++ {
		i := bytes.Index(rest, []byte("\r\n"))
		line := rest[:i+2]
		rest = rest[i+2:]
		if line[0] == ' ' || line[0] == '\t' {
			if len(m.fields) == 0 {
				return nil, errors.New("the message starts with a folded line; the first line must be a header field")
			}
			last := len(m.fields) - 1
			m.fields[last] = append(m.fields[last], line...)
			continue
		}
		name, err := fieldName(line)
		if err != nil {
			return nil, fmt.Errorf("line %d of the header: %v", n, err)
		}
		if name == "" {
			return nil, fmt.Errorf("line %d of the header is not a header field (Name: value); separate the header from the body with a blank line", n)
		}
		m.fields = append(m.fields, append([]byte(nil), line...))
	}
	for _, f := range m.fields {
		name, _ := fieldName(f)
		name = strings.ToLower(name)
		values[name] = append(values[name], strings.TrimSpace(fieldValue(f)))
	}

	if len(values["from"]) != 1 {
		return nil, errors.New("the message must have exactly one From header")
	}
	for _, name := range []string{"Sender", "Reply-To", "Subject"} {
		if len(values[strings.ToLower(name)]) > 1 {
			return nil, fmt.Errorf("the message has more than one %s header; keep one", name)
		}
	}
	from, err := mail.ParseAddressList(values["from"][0])
	if err != nil {
		return nil, fmt.Errorf("From: %v", err)
	}
	if len(from) != 1 {
		return nil, errors.New("From must hold exactly one address")
	}
	m.from = from[0]
	if !message.Storable(m.from.Name) || !message.Storable(m.from.Address) || strings.ContainsFunc(m.from.Name, isControl) {
		return nil, errors.New("From: the display name holds a NUL or other control character, or bytes that are not UTF-8; encode non-ASCII names as UTF-8 encoded-words (mail libraries do this)")
	}
	for _, f := range []struct {
		name string
		dst  *[]string
	}{{"To", &m.to}, {"Cc", &m.cc}, {"Bcc", &m.bcc}} {
		vals := values[strings.ToLower(f.name)]
		if len(vals) == 0 || strings.TrimSpace(strings.Join(vals, "")) == "" {
			continue
		}
		list, err := mail.ParseAddressList(strings.Join(vals, ", "))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f.name, err)
		}
		for _, a := range list {
			if !message.Storable(a.Address) {
				return nil, fmt.Errorf("%s: an address holds bytes that are not UTF-8", f.name)
			}
			*f.dst = append(*f.dst, a.Address)
		}
	}
	var subject string
	if v := values["subject"]; len(v) > 0 {
		subject = v[0]
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(subject); err == nil {
		subject = dec
	}
	switch {
	case strings.IndexByte(subject, 0) >= 0:
		return nil, errors.New("Subject contains a NUL character once decoded; remove it")
	case !utf8.ValidString(subject):
		return nil, errors.New("Subject is not valid UTF-8: write non-ASCII text as RFC 2047 encoded-words in UTF-8 (=?utf-8?b?…?=), which mail libraries do for you")
	}
	m.subject = subject

	for i, f := range m.fields {
		name, _ := fieldName(f)
		switch strings.ToLower(name) {
		case "bcc", "resent-bcc":
			m.dropFields[i] = true
		case "message-id":
			v := strings.Trim(strings.TrimSpace(fieldValue(f)), "<>")
			if v == "" || m.messageID != "" {
				m.dropFields[i] = true // blank, or a second one
				continue
			}
			if !message.Storable(v) {
				return nil, errors.New("Message-ID holds a NUL or bytes that are not UTF-8; remove the header and one is added for you")
			}
			m.messageID = v
		case "date":
			if strings.TrimSpace(fieldValue(f)) == "" {
				m.dropFields[i] = true
				continue
			}
			m.hasDate = true
		}
	}
	return m, nil
}

// render writes the message to send: the kept header fields, then a
// Message-ID and Date when the sender left them out, then the body. When
// nothing changes, it is the message itself, not a copy.
func (m *rawMessage) render(messageID string, now time.Time) []byte {
	if len(m.dropFields) == 0 && m.messageID != "" && m.hasDate && m.blank {
		return m.data
	}
	var b bytes.Buffer
	b.Grow(len(m.data) + 128)
	for i, f := range m.fields {
		if !m.dropFields[i] {
			b.Write(f)
		}
	}
	if m.messageID == "" {
		b.WriteString("Message-ID: <" + messageID + ">\r\n")
	}
	if !m.hasDate {
		b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(m.body)
	return b.Bytes()
}

// fieldName returns the name of the header field that starts line, or ""
// when the line is not a field. A name with whitespace before its colon
// (RFC 5322's obsolete syntax) is an error: parsers disagree about it, so it
// can smuggle a second From past the checks (audit R1#3).
func fieldName(line []byte) (string, error) {
	i := bytes.IndexByte(line, ':')
	if i <= 0 {
		return "", nil
	}
	name := string(line[:i])
	if trimmed := strings.TrimRight(name, " \t"); trimmed != name {
		if trimmed == "" {
			return "", nil
		}
		return "", fmt.Errorf("the header field %.40q has whitespace before its colon; write it as %q", trimmed+":", trimmed+":")
	}
	for j := 0; j < len(name); j++ {
		if c := name[j]; c <= ' ' || c >= 0x7f {
			return "", nil
		}
	}
	return name, nil
}

// fieldValue returns a field's value, unfolded.
func fieldValue(f []byte) string {
	_, v, _ := bytes.Cut(f, []byte(":"))
	v = bytes.ReplaceAll(v, []byte("\r\n"), nil)
	return string(v)
}

// displayAddress is "Name <addr>", or just "addr" without a name.
func displayAddress(a *mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return a.String()
}

// toCRLF turns bare LF and bare CR into CRLF, and ends the message with
// CRLF. A message that already does both is returned as it is.
func toCRLF(b []byte) []byte {
	crlf := bytes.Count(b, []byte("\r\n"))
	if crlf == bytes.Count(b, []byte("\r")) && crlf == bytes.Count(b, []byte("\n")) &&
		(len(b) == 0 || bytes.HasSuffix(b, []byte("\r\n"))) {
		return b
	}
	var out bytes.Buffer
	out.Grow(len(b) + len(b)/40 + 2)
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case '\r':
			out.WriteString("\r\n")
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
		case '\n':
			out.WriteString("\r\n")
		default:
			out.WriteByte(c)
		}
	}
	if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\r\n")) {
		out.WriteString("\r\n")
	}
	return out.Bytes()
}
