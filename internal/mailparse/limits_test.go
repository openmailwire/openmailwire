package mailparse

import (
	"strings"
	"testing"
	"time"
)

// Deeply nested message/* parts used to make Parse call itself once per
// level, until the stack overflowed and the process died.
func TestNestedMessagePartsDoNotRecurse(t *testing.T) {
	raw := []byte("Subject: outer\n" + strings.Repeat("content-type:message/\n\n", 600000) + "Subject: end\n\nhi\n")
	start := time.Now()
	m := Parse(raw)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("parsing a deeply nested message took %s", took)
	}
	if m.Subject != "outer" {
		t.Fatalf("subject: %q", m.Subject)
	}
}

// An embedded message still names its attachment after its own subject.
func TestEmbeddedMessageAttachmentName(t *testing.T) {
	raw := []byte("Subject: fwd\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\nSubject: Quarterly report\r\n\r\nbody\r\n" +
		"--b--\r\n")
	m := Parse(raw)
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "Quarterly report.eml" {
		t.Fatalf("attachments: %+v", m.Attachments)
	}
}

// Header unfolding used to be quadratic: a 3.2 MB header of short
// continuation lines took over a minute. Compare how the time grows rather
// than an absolute limit, so the test holds under -race and on slow
// machines: 4x the input costs about 4x when linear, 16x when quadratic.
func TestHeaderUnfoldingIsLinear(t *testing.T) {
	cost := func(lines int) time.Duration {
		raw := []byte("Subject: a\r\n" + strings.Repeat(" x\r\n", lines) + "\r\nbody\r\n")
		best := time.Duration(1 << 62)
		for i := 0; i < 3; i++ {
			start := time.Now()
			Parse(raw)
			ParseReport(raw)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	small, large := cost(50000), cost(200000)
	if ratio := float64(large) / float64(small); ratio > 10 {
		t.Fatalf("4x the header took %.1fx as long (%s vs %s): unfolding is not linear", ratio, large, small)
	}
}

// A huge header is cut at maxHeaderBytes instead of being read in full.
func TestHugeHeaderIsBounded(t *testing.T) {
	raw := []byte("Subject: a\r\n" + strings.Repeat(" x\r\n", 800000) + "\r\nbody\r\n")
	start := time.Now()
	Parse(raw)
	ParseReport(raw)
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("a 3.2 MB header took %s", took)
	}
}

func TestFoldedHeaderValueIsJoined(t *testing.T) {
	h := parseHeader([]byte("Subject: one\r\n two\r\n\tthree\r\nTo: a@b.test"))
	if got := h.get("Subject"); got != "one two\tthree" {
		t.Fatalf("unfolded subject: %q", got)
	}
	if h.get("To") != "a@b.test" {
		t.Fatalf("to: %q", h.get("To"))
	}
}
