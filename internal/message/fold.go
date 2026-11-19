package message

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// RFC 5322 2.1.1: a line MUST be at most 998 characters and SHOULD be at
// most 78, without the CRLF. Every header field Build writes is folded to
// stay within both where it can, and never over 998.
const (
	maxLine  = 998
	softLine = 78
)

// Caps on what goes into header fields.
const (
	// MaxDisplayName is the longest display name, in characters.
	MaxDisplayName = 256
	// MaxAddress is the longest address (RFC 5321 4.5.3.1.3: a path is at
	// most 256 octets with its angle brackets).
	MaxAddress = 254
	// MaxSubject is the longest subject, in bytes. It is folded, so any
	// subject up to this length fits.
	MaxSubject = 998
	// MaxContentType is the longest attachment content_type.
	MaxContentType = 255
)

// ParseAddress parses one address for the field (from, to, cc, reply_to) and
// checks it against the caps, so that Build can write it. Errors name the
// field and what to change.
func ParseAddress(field, s string) (*mail.Address, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %v", field, s, err)
	}
	if err := CheckAddress(field, a); err != nil {
		return nil, err
	}
	return a, nil
}

// CheckAddress checks a parsed address against the caps.
func CheckAddress(field string, a *mail.Address) error {
	switch {
	case len(a.Address) > MaxAddress:
		return fmt.Errorf("%s: the address %.40q… is longer than %d characters", field, a.Address, MaxAddress)
	case utf8.RuneCountInString(a.Name) > MaxDisplayName:
		return fmt.Errorf("%s: the display name of %s is longer than %d characters; shorten it", field, a.Address, MaxDisplayName)
	case !Storable(a.Name) || !Storable(a.Address):
		return fmt.Errorf("%s: %s contains a NUL or other control character, or bytes that are not UTF-8; remove them", field, a.Address)
	}
	for _, r := range a.Name {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return fmt.Errorf("%s: the display name of %s contains a control character; remove it", field, a.Address)
		}
	}
	return nil
}

// Storable reports whether s can be stored as PostgreSQL text: valid UTF-8
// without NUL.
func Storable(s string) bool { return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0 }

// formatAddress writes an address for a header field, folded before the
// "<addr>" and between encoded-words when the line would be long.
func formatAddress(a *mail.Address, used int) string {
	s := a.String()
	if used+len(s) <= softLine || a.Name == "" {
		return s
	}
	// String writes the name, a space, then the address in angle brackets.
	addr := (&mail.Address{Address: a.Address}).String()
	name := s[:len(s)-len(addr)-1]
	// It joins the encoded-words of a non-ASCII name with single spaces;
	// whitespace between encoded-words is not part of the text, so a fold
	// there changes nothing.
	return strings.ReplaceAll(name, "?= =?", "?=\r\n =?") + "\r\n " + addr
}

// foldText returns an unstructured header value (Subject) to write after
// "name: ". Printable ASCII is folded at its spaces; anything else, or a
// word too long to fit on a line, becomes RFC 2047 encoded-words, one per
// line. Unfolding and decoding give back exactly s.
func foldText(name, s string) string {
	used := len(name) + 2
	if !isPrintableASCII(s) {
		return encodeWords(s)
	}
	if used+len(s) <= softLine {
		return s
	}
	if folded, ok := foldAtSpaces(used, s); ok {
		return folded
	}
	return encodeWords(s)
}

// encodeWords encodes s as UTF-8 encoded-words of at most 75 characters,
// each on its own line. Unlike mime.BEncoding, it also encodes ASCII text,
// for words too long to fold.
func encodeWords(s string) string {
	const chunk = 45 // input bytes per word: 60 base64 characters, 72 with the delimiters
	var b strings.Builder
	b.Grow(len(s)*3/2 + len(s)/chunk*15 + 16)
	for len(s) > 0 {
		n := min(chunk, len(s))
		for n > 1 && n < len(s) && !utf8.RuneStart(s[n]) {
			n-- // never split a character between words
		}
		if b.Len() > 0 {
			b.WriteString("\r\n ")
		}
		b.WriteString("=?utf-8?b?")
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(s[:n])))
		b.WriteString("?=")
		s = s[n:]
	}
	return b.String()
}

// foldAtSpaces breaks printable ASCII text before whitespace so that lines
// stay within softLine where the words allow. It fails when a word does
// not fit within maxLine.
func foldAtSpaces(used int, s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s) + len(s)/softLine*3 + 3)
	line := used
	hasText := false // the current line holds something besides the field name
	for i := 0; i < len(s); {
		// A segment is a run of whitespace followed by a run of other
		// characters; a fold goes before its whitespace.
		j := i
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		ws := j > i
		for j < len(s) && s[j] != ' ' && s[j] != '\t' {
			j++
		}
		seg := s[i:j]
		onlyWS := strings.Trim(seg, " \t") == ""
		if ws && hasText && !onlyWS && line+len(seg) > softLine {
			b.WriteString("\r\n")
			line = 0
		}
		b.WriteString(seg)
		line += len(seg)
		if line > maxLine {
			return "", false
		}
		if !onlyWS {
			hasText = true
		}
		i = j
	}
	return b.String(), true
}

// checkContentType refuses content types too long to write.
func checkContentType(ct string) error {
	if len(ct) > MaxContentType {
		return errors.New("content_type is longer than 255 characters")
	}
	return nil
}
