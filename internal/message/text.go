package message

import (
	"html"
	"strings"
	"unicode/utf8"
)

// HTMLToText renders HTML as readable plain text, for the text/plain part
// that Build adds when a message has only HTML. Mailbox providers and
// screen readers expect one, and a message without it scores worse with
// spam filters.
//
// It is deliberately simple: scripts, styles and comments are dropped,
// block elements become line breaks, list items get "- ", links keep
// their address ("text (https://…)"), images their alt text, and entities
// are decoded.
func HTMLToText(src string) string {
	var out strings.Builder
	out.Grow(len(src) / 2)
	var href []string // the open links' addresses
	pre := 0          // depth of <pre>
	space := false    // a space is pending between words
	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			lt = len(src) - i
		}
		if lt > 0 {
			text := html.UnescapeString(src[i : i+lt])
			if pre > 0 {
				out.WriteString(text)
				space = false
			} else {
				fields := strings.FieldsFunc(text, isHTMLSpace)
				space = space || startsWithSpace(text)
				for _, f := range fields {
					if space && !endsWithBreak(&out) {
						out.WriteByte(' ')
					}
					out.WriteString(f)
					space = true
				}
				if len(fields) > 0 {
					space = endsWithSpace(text)
				}
			}
			i += lt
			continue
		}
		// A tag, comment or declaration at src[i].
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		end := tagEnd(src, i)
		tag := src[i+1 : end]
		i = end + 1
		if end >= len(src) {
			break
		}
		closing := strings.HasPrefix(tag, "/")
		name := strings.ToLower(tagName(strings.TrimPrefix(tag, "/")))
		switch name {
		case "script", "style", "head", "title", "template", "noscript":
			if !closing && !strings.HasSuffix(tag, "/") {
				// Skip everything up to the matching end tag.
				j := indexFold(src[i:], "</"+name)
				if j < 0 {
					i = len(src)
				} else {
					i += j
				}
			}
		case "br":
			out.WriteString("\n")
			space = false
		case "p", "div", "section", "article", "header", "footer", "main", "nav", "aside", "address",
			"h1", "h2", "h3", "h4", "h5", "h6", "table", "ul", "ol", "dl", "blockquote", "figure", "form", "center":
			paragraph(&out)
			space = false
		case "tr", "dt", "dd", "figcaption", "caption":
			newline(&out)
			space = false
		case "li":
			if !closing {
				newline(&out)
				out.WriteString("- ")
			}
			space = false
		case "td", "th":
			if !closing {
				space = true
			}
		case "hr":
			paragraph(&out)
			out.WriteString("----------")
			paragraph(&out)
			space = false
		case "pre":
			paragraph(&out)
			if closing {
				pre = max(pre-1, 0)
			} else {
				pre++
			}
			space = false
		case "a":
			if closing {
				if n := len(href); n > 0 {
					if h := href[n-1]; h != "" {
						out.WriteString(" (" + h + ")")
						space = true
					}
					href = href[:n-1]
				}
			} else {
				h := strings.TrimSpace(html.UnescapeString(attr(tag, "href")))
				if h == "" || strings.HasPrefix(h, "#") || strings.HasPrefix(strings.ToLower(h), "javascript:") {
					h = ""
				}
				href = append(href, strings.TrimPrefix(h, "mailto:"))
			}
		case "img":
			if alt := strings.TrimSpace(html.UnescapeString(attr(tag, "alt"))); alt != "" {
				if space {
					out.WriteByte(' ')
				}
				out.WriteString(alt)
				space = true
			}
		}
	}
	return tidy(out.String())
}

func isHTMLSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\u00a0'
}

func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return s != "" && isHTMLSpace(r)
}

func endsWithSpace(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && isHTMLSpace(r)
}

func endsWithBreak(b *strings.Builder) bool {
	s := b.String()
	return s == "" || s[len(s)-1] == '\n' || s[len(s)-1] == ' '
}

// newline ends the current line unless it is empty.
func newline(b *strings.Builder) {
	if s := b.String(); s != "" && s[len(s)-1] != '\n' {
		b.WriteByte('\n')
	}
}

// paragraph leaves one empty line, unless the text is empty.
func paragraph(b *strings.Builder) {
	s := b.String()
	switch {
	case s == "" || strings.HasSuffix(s, "\n\n"):
	case strings.HasSuffix(s, "\n"):
		b.WriteByte('\n')
	default:
		b.WriteString("\n\n")
	}
}

// tagEnd returns the index of the ">" that ends the tag starting at
// src[i], skipping quoted attribute values, or len(src).
func tagEnd(src string, i int) int {
	var quote byte
	for j := i + 1; j < len(src); j++ {
		c := src[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j
		}
	}
	return len(src)
}

func tagName(tag string) string {
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return tag[:i]
		}
	}
	return tag
}

// attr returns the value of the named attribute in a tag's text.
func attr(tag, name string) string {
	lower := strings.ToLower(tag)
	for from := 0; ; {
		k := strings.Index(lower[from:], name)
		if k < 0 {
			return ""
		}
		k += from
		from = k + len(name)
		if k == 0 || !isHTMLSpace(rune(lower[k-1])) {
			continue
		}
		j := from
		for j < len(tag) && isHTMLSpace(rune(tag[j])) {
			j++
		}
		if j >= len(tag) || tag[j] != '=' {
			continue
		}
		j++
		for j < len(tag) && isHTMLSpace(rune(tag[j])) {
			j++
		}
		if j >= len(tag) {
			return ""
		}
		if q := tag[j]; q == '"' || q == '\'' {
			if e := strings.IndexByte(tag[j+1:], q); e >= 0 {
				return tag[j+1 : j+1+e]
			}
			return tag[j+1:]
		}
		e := j
		for e < len(tag) && !isHTMLSpace(rune(tag[e])) && tag[e] != '>' {
			e++
		}
		return tag[j:e]
	}
}

// indexFold is strings.Index, ignoring ASCII case.
func indexFold(s, sub string) int {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if strings.EqualFold(s[i:i+n], sub) {
			return i
		}
	}
	return -1
}

// tidy trims the end of each line and keeps at most one empty line in a
// row.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := true
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, l)
		blank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
