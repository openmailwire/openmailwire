package mailparse

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// HTMLToText is a rough plain-text rendering of HTML, for snippets and for
// agents when a message has no text part.
//
// It keeps only what a person reading the message would see. Scripts,
// styles, the head, templates and comments are dropped, and so is every
// element hidden from view, because senders use hidden text to slip
// instructions past a human reader to an AI agent:
//   - the hidden attribute (and noscript and iframe fallback content);
//   - style display:none, visibility:hidden or collapse, mso-hide:all;
//   - a zero font-size or opacity;
//   - a zero height, max-height, width or max-width with overflow:hidden;
//   - absolute or fixed positioning, or text-indent, 1000px or more off the
//     page to the left or top.
//
// It uses an HTML tokenizer, so the work is linear in the input whatever
// its shape.
func HTMLToText(h string) string {
	z := html.NewTokenizer(strings.NewReader(h))
	var b strings.Builder
	skip, depth := "", 0 // the element whose content is being dropped, and its nesting
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return tidyLines(b.String())
		case html.TextToken:
			if skip == "" {
				b.Write(z.Text())
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			raw, hasAttr := z.TagName()
			name := string(raw)
			if skip != "" {
				switch {
				case skip == "head" && name == "body":
					skip = "" // an unclosed head ends where the body starts
				case name == skip && tt == html.StartTagToken:
					depth++
				}
				if skip != "" {
					continue
				}
			}
			hidden := invisibleElements[name] || hasAttr && hiddenByAttributes(z)
			if hidden {
				if tt == html.StartTagToken && !voidElements[name] {
					skip, depth = name, 1
				}
				continue
			}
			if lineBreaks[name] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			raw, _ := z.TagName()
			name := string(raw)
			if skip != "" {
				if name == skip {
					if depth--; depth == 0 {
						skip = ""
					}
				}
				continue
			}
			if lineBreaks[name] {
				b.WriteByte('\n')
			}
		}
	}
}

// tidyLines collapses white space within lines and drops empty lines.
func tidyLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if l = collapseSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

var (
	invisibleElements = map[string]bool{"script": true, "style": true, "head": true, "title": true, "template": true, "noscript": true, "iframe": true}
	voidElements      = map[string]bool{
		"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
		"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
	}
	lineBreaks = map[string]bool{
		"br": true, "p": true, "div": true, "li": true, "tr": true, "table": true, "blockquote": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	}
)

// hiddenByAttributes reads the current tag's attributes.
func hiddenByAttributes(z *html.Tokenizer) bool {
	for {
		key, val, more := z.TagAttr()
		switch string(key) {
		case "hidden":
			return true
		case "style":
			if hiddenStyle(string(val)) {
				return true
			}
		}
		if !more {
			return false
		}
	}
}

// hiddenStyle reports whether an inline style hides its element.
func hiddenStyle(style string) bool {
	props := map[string]string{}
	for _, decl := range strings.Split(strings.ToLower(style), ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "!important"))
		props[strings.TrimSpace(k)] = v
	}
	clipped := props["overflow"] == "hidden"
	switch {
	case props["display"] == "none",
		props["visibility"] == "hidden", props["visibility"] == "collapse",
		props["mso-hide"] == "all",
		isZero(props["font-size"]), isZero(props["opacity"]),
		clipped && (isZero(props["height"]) || isZero(props["max-height"]) || isZero(props["width"]) || isZero(props["max-width"])),
		(props["position"] == "absolute" || props["position"] == "fixed") && (farOff(props["left"]) || farOff(props["top"])),
		farOff(props["text-indent"]):
		return true
	}
	return false
}

// cssNumber reads the number at the start of a CSS length or number.
func cssNumber(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	end := 0
	for end < len(v) && (v[end] >= '0' && v[end] <= '9' || v[end] == '.' || v[end] == '-' || v[end] == '+') {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(v[:end], 64)
	return n, err == nil
}

func isZero(v string) bool {
	n, ok := cssNumber(v)
	return ok && n == 0
}

func farOff(v string) bool {
	n, ok := cssNumber(v)
	return ok && n <= -1000
}
