package mailparse

import (
	"regexp"
	"strconv"
	"strings"
)

// Report kinds.
const (
	KindDSN = "dsn" // a delivery status notification (RFC 3464), or a plain-text bounce
	KindARF = "arf" // an abuse/feedback report (RFC 5965)
)

// Report is what a machine-generated message says. Kind is "" when the
// message is not a report this package understands.
type Report struct {
	Kind string
	DSN  *DSN
	ARF  *ARF
}

// DSN is a delivery status notification.
type DSN struct {
	ReportingMTA string
	Recipients   []DSNRecipient
	// PlainText is set when the bounce was not in RFC 3464 format and was
	// read from its human-readable text instead.
	PlainText bool
	Original  *Original
}

// DSNRecipient is one per-recipient block.
type DSNRecipient struct {
	OriginalRecipient string // lowercase address
	FinalRecipient    string // lowercase address
	Action            string // failed, delayed, delivered, relayed or expanded
	Status            string // enhanced status code, e.g. 5.1.1
	RemoteMTA         string
	Diagnostic        string // the remote server's reply, unfolded
	SMTPCode          int    // from Diagnostic, when it has one
}

// ARF is a feedback report: a recipient marked a message as spam.
type ARF struct {
	FeedbackType     string // lowercase: abuse, fraud, virus, other, not-spam, auth-failure
	UserAgent        string
	Version          string
	OriginalMailFrom string   // lowercase address
	OriginalRcptTo   []string // lowercase addresses
	ReportedDomain   string
	SourceIP         string
	ArrivalDate      string
	Original         *Original
}

// Original is what a report carries of the message it is about.
type Original struct {
	MessageID  string   // without angle brackets
	ReturnPath string   // lowercase address
	From       string   // lowercase address
	To         []string // lowercase addresses
}

// Failed lists the recipients whose action is "failed".
func (d *DSN) Failed() []DSNRecipient {
	var out []DSNRecipient
	for _, r := range d.Recipients {
		if r.Action == "failed" {
			out = append(out, r)
		}
	}
	return out
}

// IsComplaint reports whether the feedback is a complaint about unwanted
// mail. DMARC failure reports (auth-failure) and "not-spam" are not.
func (a *ARF) IsComplaint() bool {
	switch a.FeedbackType {
	case "", "abuse", "fraud", "virus":
		return true
	}
	return false
}

// ParseReport reads a DSN or an ARF report. Anything else gives Kind "".
func ParseReport(raw []byte) Report {
	var dsnBody, arfBody, original []byte
	var texts []string
	walk(raw, func(p part) {
		switch p.mediaType {
		case "message/delivery-status", "message/global-delivery-status":
			if dsnBody == nil {
				dsnBody = p.body
			}
		case "message/feedback-report":
			if arfBody == nil {
				arfBody = p.body
			}
		case "message/rfc822", "message/global", "text/rfc822-headers", "message/rfc822-headers", "message/global-headers":
			if original == nil {
				original = p.body
			}
		case "text/plain":
			texts = append(texts, p.text())
		case "text/html":
			if len(texts) == 0 {
				texts = append(texts, HTMLToText(p.text()))
			}
		}
	})
	var orig *Original
	if original != nil {
		orig = parseOriginal(original)
	}
	switch {
	case arfBody != nil:
		a := parseARF(arfBody)
		a.Original = orig
		return Report{Kind: KindARF, ARF: a}
	case dsnBody != nil:
		d := parseDSN(dsnBody)
		d.Original = orig
		return Report{Kind: KindDSN, DSN: d}
	}
	if d := parsePlainBounce(raw, texts); d != nil {
		d.Original = orig
		return Report{Kind: KindDSN, DSN: d}
	}
	return Report{}
}

// fieldGroups splits a report body into blocks of fields separated by
// blank lines, unfolding continuation lines. Continuations are collected
// and joined once per field, so the work stays linear in the input.
func fieldGroups(b []byte) []header {
	if len(b) > maxHeaderBytes {
		b = b[:maxHeaderBytes]
	}
	type pending struct {
		name  string
		parts []string
	}
	var groups []header
	var cur []pending
	flush := func() {
		if len(cur) == 0 {
			return
		}
		h := make(header, len(cur))
		for i, f := range cur {
			h[i] = field{name: f.name, value: strings.Join(f.parts, " ")}
		}
		groups = append(groups, h)
		cur = nil
	}
	fields := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(cur) > 0 {
				f := &cur[len(cur)-1]
				f.parts = append(f.parts, strings.TrimSpace(line))
			}
			continue
		}
		if fields == maxHeaderFields {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.ContainsAny(strings.TrimSpace(name), " \t") {
			continue
		}
		fields++
		cur = append(cur, pending{name: strings.TrimSpace(name), parts: []string{strings.TrimSpace(value)}})
	}
	flush()
	return groups
}

var (
	enhancedRE = regexp.MustCompile(`\b([245])\.(\d{1,3})\.(\d{1,3})\b`)
	smtpCodeRE = regexp.MustCompile(`^\s*([245]\d\d)(?:[\s\-]|$)`)
)

// typedValue strips the type of a typed field ("rfc822; a@b", "smtp; 550 …",
// "dns; mx.example.com").
func typedValue(v string) string {
	if t, rest, ok := strings.Cut(v, ";"); ok && !strings.ContainsAny(strings.TrimSpace(t), " @<") {
		v = rest
	}
	return strings.TrimSpace(v)
}

func addrValue(v string) string {
	v = typedValue(v)
	if ids := msgIDRE.FindStringSubmatch(v); ids != nil {
		v = ids[1]
	}
	if i := strings.IndexAny(v, " \t("); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.Trim(v, "<> \t"))
}

func parseDSN(b []byte) *DSN {
	d := &DSN{}
	var cur *DSNRecipient
	flush := func() {
		if cur != nil && (cur.FinalRecipient != "" || cur.OriginalRecipient != "" || cur.Action != "" || cur.Status != "") {
			finishRecipient(cur)
			d.Recipients = append(d.Recipients, *cur)
		}
		cur = nil
	}
	for _, g := range fieldGroups(b) {
		flush()
		for _, f := range g {
			name := strings.ToLower(f.name)
			// A recipient block starts at its first recipient field. Some
			// reporters leave out the blank line between blocks, so a
			// repeated field starts a new one too.
			switch name {
			case "original-recipient", "final-recipient", "action", "status", "remote-mta", "diagnostic-code":
				if cur == nil {
					cur = &DSNRecipient{}
				} else if (name == "final-recipient" && cur.FinalRecipient != "") ||
					(name == "original-recipient" && cur.OriginalRecipient != "") ||
					(name == "action" && cur.Action != "") {
					flush()
					cur = &DSNRecipient{}
				}
			}
			switch name {
			case "reporting-mta":
				d.ReportingMTA = typedValue(f.value)
			case "original-recipient":
				cur.OriginalRecipient = addrValue(f.value)
			case "final-recipient":
				cur.FinalRecipient = addrValue(f.value)
			case "action":
				a, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(f.value)), " ")
				cur.Action = strings.Trim(a, "();")
			case "status":
				if m := enhancedRE.FindString(f.value); m != "" {
					cur.Status = m
				}
			case "remote-mta":
				cur.RemoteMTA = typedValue(f.value)
			case "diagnostic-code":
				cur.Diagnostic = collapseSpace(typedValue(f.value))
			}
		}
	}
	flush()
	return d
}

// finishRecipient fills what a sloppy reporter left out.
func finishRecipient(r *DSNRecipient) {
	if m := smtpCodeRE.FindStringSubmatch(r.Diagnostic); m != nil {
		r.SMTPCode, _ = strconv.Atoi(m[1])
	}
	if r.Status == "" {
		if m := enhancedRE.FindString(r.Diagnostic); m != "" {
			r.Status = m
		} else if r.SMTPCode > 0 {
			r.Status = strconv.Itoa(r.SMTPCode/100) + ".0.0"
		}
	}
	if r.Action == "" {
		switch {
		case strings.HasPrefix(r.Status, "5."):
			r.Action = "failed"
		case strings.HasPrefix(r.Status, "4."):
			r.Action = "delayed"
		case strings.HasPrefix(r.Status, "2."):
			r.Action = "delivered"
		}
	}
}

func parseARF(b []byte) *ARF {
	a := &ARF{}
	for _, g := range fieldGroups(b) {
		for _, f := range g {
			switch strings.ToLower(f.name) {
			case "feedback-type":
				a.FeedbackType = strings.ToLower(strings.TrimSpace(f.value))
			case "user-agent":
				a.UserAgent = f.value
			case "version":
				a.Version = f.value
			case "original-mail-from":
				a.OriginalMailFrom = addrValue(f.value)
			case "original-rcpt-to":
				if r := addrValue(f.value); r != "" {
					a.OriginalRcptTo = append(a.OriginalRcptTo, r)
				}
			case "reported-domain":
				a.ReportedDomain = strings.ToLower(f.value)
			case "source-ip":
				a.SourceIP = f.value
			case "arrival-date", "received-date":
				if a.ArrivalDate == "" {
					a.ArrivalDate = f.value
				}
			}
		}
	}
	return a
}

func parseOriginal(b []byte) *Original {
	head, _ := splitMessage(b)
	h := parseHeader(head)
	o := &Original{}
	if ids := MessageIDs(h.get("Message-ID")); len(ids) > 0 {
		o.MessageID = ids[0]
	}
	o.ReturnPath = addrValue(h.get("Return-Path"))
	if from := parseAddresses(h.get("From")); len(from) > 0 {
		o.From = strings.ToLower(from[0].Address)
	}
	for _, a := range parseAddresses(strings.Join(h.all("To"), ", ")) {
		o.To = append(o.To, strings.ToLower(a.Address))
	}
	return o
}

var (
	bounceSubjectRE = regexp.MustCompile(`(?i)(undeliver|undelivered|delivery (status notification|failure|has failed|notification)|failure notice|returned mail|mail delivery (failed|system|subsystem)|non.?delivery|could not be delivered|delivery problem)`)
	plainSMTPRE     = regexp.MustCompile(`(?m)(?:^|[\s:(\[])(5[0-5]\d)(?:[\s:\-.]|$)`)
	angleAddrRE     = regexp.MustCompile(`<([^<>\s]+@[^<>\s]+)>`)
	// Phrases receivers use for an address that does not exist, read only
	// from plain-text bounces that carry no enhanced status code.
	noMailboxRE = regexp.MustCompile(`(?i)(user unknown|unknown user|no such user|no such mailbox|mailbox not found|recipient not found|does not exist|doesn't have an? .{1,40}account|invalid recipient|recipient address rejected)`)
)

// parsePlainBounce reads a bounce that is not in RFC 3464 format (qmail and
// Yahoo's "failure notice", older Exchange, some appliances). It answers
// only when the message looks like a bounce (sent by the mailer daemon or
// postmaster, or titled like one) and names a permanent SMTP failure.
func parsePlainBounce(raw []byte, texts []string) *DSN {
	head, _ := splitMessage(raw)
	h := parseHeader(head)
	sender := ""
	if from := parseAddresses(h.get("From")); len(from) > 0 {
		sender = strings.ToLower(from[0].Address)
	}
	local, _, _ := strings.Cut(sender, "@")
	if local != "mailer-daemon" && local != "postmaster" && !bounceSubjectRE.MatchString(decodeHeader(h.get("Subject"))) {
		return nil
	}
	text := strings.Join(texts, "\n")
	r := DSNRecipient{Action: "failed"}
	var line string
	if m := enhancedRE.FindStringIndex(text); m != nil && text[m[0]] == '5' {
		r.Status = text[m[0]:m[1]]
		line = lineAround(text, m[0])
	}
	if m := plainSMTPRE.FindStringSubmatchIndex(text); m != nil {
		r.SMTPCode, _ = strconv.Atoi(text[m[2]:m[3]])
		if line == "" {
			line = lineAround(text, m[2])
		}
	}
	if r.Status == "" && r.SMTPCode == 0 {
		return nil
	}
	if r.Status == "" {
		r.Status = "5.0.0"
		if noMailboxRE.MatchString(line) {
			r.Status = "5.1.1"
		}
	}
	r.Diagnostic = line
	if m := angleAddrRE.FindStringSubmatch(text); m != nil {
		r.FinalRecipient = strings.ToLower(m[1])
	}
	return &DSN{Recipients: []DSNRecipient{r}, PlainText: true}
}

// lineAround returns the line holding text[i], joined with the next line
// when that one continues it, trimmed to 300 characters.
func lineAround(text string, i int) string {
	start := strings.LastIndexByte(text[:i], '\n') + 1
	end := strings.IndexByte(text[i:], '\n')
	if end < 0 {
		end = len(text)
	} else {
		end += i
		if next := strings.IndexByte(text[end+1:], '\n'); next > 0 {
			cont := text[end+1 : end+1+next]
			if strings.HasPrefix(cont, " ") || strings.HasPrefix(cont, "\t") {
				end = end + 1 + next
			}
		}
	}
	s := collapseSpace(text[start:end])
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300])
	}
	return s
}
