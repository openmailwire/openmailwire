package mailparse

import (
	"fmt"
	"html"
	"path"
	"regexp"
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
		Subject: collapseSpace(decodeHeader(h.get("Subject"))),
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
				if sub := Parse(p.body).Subject; sub != "" {
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

func joinText(have, add string) string {
	if have == "" {
		return add
	}
	return have + "\n" + add
}

func sanitizeFilename(name string, index int) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" || name == ".." {
		return fmt.Sprintf("attachment-%d", index+1)
	}
	if len(name) > 255 {
		name = strings.ToValidUTF8(name[:255], "")
	}
	return name
}

var (
	scriptRE = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)\s*>`)
	breakRE  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6])\b[^>]*>`)
	tagRE    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// HTMLToText is a rough plain-text rendering of HTML, for snippets and for
// agents when a message has no text part.
func HTMLToText(h string) string {
	h = scriptRE.ReplaceAllString(h, " ")
	h = breakRE.ReplaceAllString(h, "\n")
	h = tagRE.ReplaceAllString(h, " ")
	h = html.UnescapeString(h)
	lines := strings.Split(h, "\n")
	out := lines[:0]
	for _, l := range lines {
		if l = collapseSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
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
