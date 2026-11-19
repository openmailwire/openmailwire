package mailparse

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

// Message is a received message, read for an inbox.
type Message struct {
	From       string // "Name <addr>", decoded
	FromAddr   string // lowercase address
	To         []string
	Cc         []string
	ReplyTo    []string
	Subject    string
	MessageID  string   // without angle brackets
	InReplyTo  string   // the first identifier in In-Reply-To
	References []string // identifiers in References, oldest first
	Date       string
	// Headers holds the threading and date headers as written.
	Headers     map[string]string
	Text        string
	HTML        string
	Attachments []Attachment
}

type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Inline      bool
	Data        []byte
}

// Parse reads a message. It never fails: malformed parts are skipped or
// kept as they are.
func Parse(raw []byte) *Message {
	head, _ := splitMessage(raw)
	h := parseHeader(head)
	m := &Message{
		Subject: CleanDisplay(decodeHeader(h.get("Subject"))),
		Date:    collapseSpace(h.get("Date")),
		Headers: map[string]string{},
	}
	if from := parseAddresses(h.get("From")); len(from) > 0 {
		m.From = FormatAddress(from[0])
		m.FromAddr = strings.ToLower(from[0].Address)
	}
	m.To = formatAddresses(parseAddresses(strings.Join(h.all("To"), ", ")))
	m.Cc = formatAddresses(parseAddresses(strings.Join(h.all("Cc"), ", ")))
	m.ReplyTo = formatAddresses(parseAddresses(h.get("Reply-To")))
	if ids := MessageIDs(h.get("Message-ID")); len(ids) > 0 {
		m.MessageID = ids[0]
	}
	if ids := MessageIDs(h.get("In-Reply-To")); len(ids) > 0 {
		m.InReplyTo = ids[0]
	}
	m.References = MessageIDs(h.get("References"))
	for _, k := range []string{"Message-ID", "In-Reply-To", "References", "Date"} {
		if v := collapseSpace(h.get(k)); v != "" {
			m.Headers[strings.ToLower(k)] = v
		}
	}

	walk(raw, func(p part) {
		name := p.filename()
		switch {
		case (p.mediaType == "text/plain" || p.mediaType == "text/html") && p.disposition != "attachment" && name == "":
			t := strings.ReplaceAll(p.text(), "\r\n", "\n")
			if p.mediaType == "text/plain" {
				m.Text = joinText(m.Text, t)
			} else {
				m.HTML = joinText(m.HTML, t)
			}
		default:
			if strings.HasPrefix(p.mediaType, "message/") && name == "" {
				name = "message.eml"
				if sub := embeddedSubject(p.body); sub != "" {
					name = sub + ".eml"
				}
			}
			m.Attachments = append(m.Attachments, Attachment{
				Filename:    sanitizeFilename(name, len(m.Attachments)),
				ContentType: p.mediaType,
				ContentID:   strings.Trim(p.header.get("Content-ID"), "<> "),
				Inline:      p.disposition == "inline",
				Data:        p.body,
			})
		}
	})
	return m
}

// embeddedSubject reads only the header of an embedded message/* part.
// Parsing the whole embedded message would recurse once per nesting level,
// with a fresh depth and part budget each time.
func embeddedSubject(body []byte) string {
	head, _ := splitMessage(body)
	return CleanDisplay(decodeHeader(parseHeader(head).get("Subject")))
}

func joinText(have, add string) string {
	if have == "" {
		return add
	}
	return have + "\n" + add
}

// sanitizeFilename makes a sender's attachment name safe to save and to
// show: only the last path element is kept; control characters, quotes and
// Unicode format characters (category Cf: bidirectional overrides that make
// "invoice\u202Efdp.exe" read as "invoiceexe.pdf", zero-width characters)
// are removed; leading dots, which hide a file on Unix, are dropped.
func sanitizeFilename(name string, index int) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '"' || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(name), "."))
	if name == "" || name == "/" {
		return fmt.Sprintf("attachment-%d", index+1)
	}
	if len(name) > 255 {
		name = strings.ToValidUTF8(name[:255], "")
	}
	return name
}

// Snippet is the start of the message's new text: quoted lines (">") are
// skipped and whitespace is collapsed.
func (m *Message) Snippet(n int) string {
	t := m.Text
	if strings.TrimSpace(t) == "" {
		t = HTMLToText(m.HTML)
	}
	var kept []string
	for _, l := range strings.Split(t, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			continue
		}
		kept = append(kept, l)
	}
	s := collapseSpace(strings.Join(kept, " "))
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}
