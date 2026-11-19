package mailparse

import (
	"regexp"
	"sort"
	"strings"
)

// Rules that flag text written to steer an AI agent rather than to inform
// a person. They are deliberately few and specific: a flag is a hint shown
// next to the message (suspicious_instructions), never a reason to drop
// mail, so a false negative costs little and a false positive is annoying.
//
// Each rule has a name, reported in suspicious_rules:
//
//	override_instructions  "ignore / disregard / forget (all) (the) previous / prior / above instructions / prompts / rules"
//	role_marker            a chat role or prompt-format marker at the start of a line ("System:", "Assistant:",
//	                       "<|im_start|>", "[INST]", "### Instruction"), or "you are now in … mode"
//	addresses_agent        words addressed to a model: "note to the AI", "dear assistant", "AI agent:", "if you are an AI/LLM"
//	tool_instruction       naming an OpenMailWire tool ("call send_email") or asking to forward or export mail in bulk
//	                       ("forward all emails", "send the last 20 messages to")
//	conceal_from_user      "do not tell / inform / mention … the user / human / owner"
//	claims_approval        "the user / owner / admin has approved / authorized / confirmed"
var suspiciousRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"override_instructions", regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\s+(all\s+|any\s+)?(of\s+)?(the\s+|your\s+|my\s+)?(previous|prior|above|earlier|preceding|original|system)\s+(instructions|prompts?|rules|directions|guidelines)\b`)},
	{"role_marker", regexp.MustCompile(`(?im)(^\s*(system|assistant|developer)\s*:|<\|im_start\|>|<\|im_end\|>|\[/?INST\]|^\s*#{2,}\s*(instruction|system)|\byou\s+are\s+now\s+in\s+\w+\s+mode\b)`)},
	{"addresses_agent", regexp.MustCompile(`(?i)(\bnote\s+to\s+(the\s+)?(ai|assistant|agent|llm|model)\b|\bdear\s+(ai|assistant|agent|llm)\b|\b(ai|llm)\s+(agent|assistant)\s*:|\bif\s+you\s+are\s+an?\s+(ai|llm|language\s+model|assistant)\b)`)},
	{"tool_instruction", regexp.MustCompile(`(?i)(\b(call|use|invoke|run)\s+(the\s+)?(send_email|reply_to_message|list_messages|read_message|wait_for_reply|list_inboxes|cancel_email|add_domain|verify_domain)\b|\b(forward|send|export|copy)\s+(all|every|each|the\s+last\s+\d+)\s+(of\s+)?(the\s+|your\s+|my\s+|these\s+)?(e-?mails?|messages|conversations|threads)\b)`)},
	{"conceal_from_user", regexp.MustCompile(`(?i)\b(do\s+not|don't|never)\s+(tell|inform|mention|notify|alert|show)\b[^.\n]{0,40}\b(user|human|owner|operator|admin)\b`)},
	{"claims_approval", regexp.MustCompile(`(?i)\b(the\s+)?(user|owner|operator|admin|administrator)\s+(has\s+)?(already\s+)?(approved|authori[sz]ed|confirmed|allowed)\b`)},
}

// maxSuspiciousScan bounds the text the rules read; instructions meant for
// an agent are near the top or in short hidden blocks.
const maxSuspiciousScan = 256 << 10

// SuspiciousInstructions lists the names of the rules the texts match, in
// order, without repeats; nil when none do.
func SuspiciousInstructions(texts ...string) []string {
	hit := map[string]bool{}
	for _, t := range texts {
		if len(t) > maxSuspiciousScan {
			t = strings.ToValidUTF8(t[:maxSuspiciousScan], "")
		}
		for _, r := range suspiciousRules {
			if !hit[r.name] && r.re.MatchString(t) {
				hit[r.name] = true
			}
		}
	}
	if len(hit) == 0 {
		return nil
	}
	out := make([]string, 0, len(hit))
	for name := range hit {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SuspiciousText is the text the rules read for a message: its subject,
// the sender's display name, the plain text, and the HTML as text with
// hidden content included (hidden instructions are the point of the flag).
func (m *Message) SuspiciousText() []string {
	out := []string{m.Subject, m.From, m.Text}
	if m.HTML != "" {
		out = append(out, htmlAllText(m.HTML))
	}
	return out
}

// htmlAllText is HTML as text, hidden elements included.
func htmlAllText(h string) string {
	if len(h) > maxSuspiciousScan*4 {
		h = h[:maxSuspiciousScan*4]
	}
	return tagStripper.Replace(stripTags(h))
}

var tagStripper = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")

// stripTags removes everything between '<' and '>' in one pass.
func stripTags(h string) string {
	var b strings.Builder
	b.Grow(len(h))
	in := false
	for i := 0; i < len(h); i++ {
		switch c := h[i]; {
		case c == '<':
			in = true
			b.WriteByte(' ')
		case c == '>':
			in = false
		case !in:
			b.WriteByte(c)
		}
	}
	return b.String()
}
