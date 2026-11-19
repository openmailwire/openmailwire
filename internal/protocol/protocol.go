// Package protocol holds the JSON shapes that the control plane and its
// workers exchange on the internal worker API.
package protocol

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Outcomes a worker can report for one delivery attempt.
const (
	OutcomeDelivered = "delivered" // the receiving server accepted the message (2xx after DATA)
	OutcomeDeferred  = "deferred"  // temporary failure; the control plane schedules a retry
	OutcomeBounced   = "bounced"   // permanent failure (5xx)
	OutcomeUnknown   = "unknown"   // the connection ended after the body was sent but before the final reply
)

// ValidOutcome reports whether s is one of the outcomes above.
func ValidOutcome(s string) bool {
	switch s {
	case OutcomeDelivered, OutcomeDeferred, OutcomeBounced, OutcomeUnknown:
		return true
	}
	return false
}

// Worker kinds. A direct worker delivers to the recipients' mail servers
// from its own IP; a relay worker hands mail to a smarthost (another
// provider) and gets the deliveries that direct workers could not finish.
const (
	KindDirect = "direct"
	KindRelay  = "relay"
)

// ValidKind reports whether s is a worker kind. Empty means direct, so
// direct workers stay compatible with control planes that predate kinds.
func ValidKind(s string) bool {
	return s == "" || s == KindDirect || s == KindRelay
}

// Worker token roles. A token is created for one role: delivery workers
// lease and report, relay workers do the same as relays, inbound nodes hand
// over received mail. Tokens created before roles existed have RoleAny.
const (
	RoleAny      = "any"
	RoleDelivery = "delivery"
	RoleInbound  = "inbound"
	RoleRelay    = "relay"
)

// ValidRole reports whether s is a role a new token can have.
func ValidRole(s string) bool {
	return s == RoleDelivery || s == RoleInbound || s == RoleRelay
}

type HeartbeatRequest struct {
	PublicIP string `json:"public_ip"`
	EHLO     string `json:"ehlo"`
	Capacity int    `json:"capacity"`
	Version  string `json:"version"`
	Kind     string `json:"kind,omitempty"` // "direct" (or empty) or "relay"
	// SelfCheck is the result of the worker's start-up checks (DNS, reverse
	// DNS, port 25). Nil from workers that predate it.
	SelfCheck *SelfCheck `json:"self_check,omitempty"`
}

// SelfCheck is what a worker found about its own setup when it started.
type SelfCheck struct {
	OK        bool        `json:"ok"`
	Skipped   string      `json:"skipped,omitempty"` // why the checks did not run (development)
	Checks    []CheckItem `json:"checks,omitempty"`
	CheckedAt time.Time   `json:"checked_at"`
}

// CheckItem is one self-check.
type CheckItem struct {
	Name   string `json:"name"` // ehlo_fqdn, ehlo_a_record, reverse_dns, port25_egress
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type HeartbeatResponse struct {
	Draining bool `json:"draining"`
}

type LeaseRequest struct {
	Max  int    `json:"max"`
	Kind string `json:"kind,omitempty"` // "direct" (or empty) or "relay"
}

// Job is one recipient of one message, leased to one worker for one attempt.
type Job struct {
	AttemptID      string    `json:"attempt_id"`
	DeliveryID     string    `json:"delivery_id"`
	MailFrom       string    `json:"mail_from"`
	Rcpt           string    `json:"rcpt"`
	RcptDomain     string    `json:"rcpt_domain"`
	Raw            []byte    `json:"raw"` // the DKIM-signed message, CRLF line endings
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type LeaseResponse struct {
	Jobs []Job `json:"jobs"`
}

// Report is the result of one attempt. Reports are idempotent: the control
// plane ignores a second report for the same attempt.
type Report struct {
	AttemptID    string `json:"attempt_id"`
	Outcome      string `json:"outcome"`
	SMTPCode     int    `json:"smtp_code"`
	EnhancedCode string `json:"enhanced_code"`
	Response     string `json:"response"`
	MXHost       string `json:"mx_host"`
	SourceIP     string `json:"source_ip"`
	// TLS is "", "none", "opportunistic", "verified" (the certificate is
	// valid for the host), "verified+mta-sts" (an MTA-STS policy in enforce
	// mode required it) or "dane" (it matched the host's TLSA records).
	TLS string `json:"tls"`
	// TLSPolicy notes what a receiver's TLS policy would have changed when it
	// was not enforced, e.g. an MTA-STS policy in testing mode that this
	// attempt failed. Empty when there is nothing to note.
	TLSPolicy string `json:"tls_policy,omitempty"`
	// Stage is the SMTP step whose reply decided the outcome (Stage*
	// constants). Empty from workers that predate it, and for failures that
	// are not a reply (a refused connection, a TLS policy).
	Stage string `json:"stage,omitempty"`
}

// SMTP steps a reply can come from.
const (
	StageGreeting = "greeting"
	StageEHLO     = "ehlo"
	StageSTARTTLS = "starttls"
	StageAuth     = "auth"
	StageMail     = "mail"
	StageRcpt     = "rcpt"
	StageData     = "data"    // the reply to the DATA command
	StageMessage  = "message" // the final reply, after the message was sent
)

// CleanText makes s storable as PostgreSQL text and JSON: invalid UTF-8
// becomes U+FFFD, NUL bytes are dropped, and the result is cut to at most
// max bytes on a character boundary. Text from receivers (SMTP replies) can
// be anything.
func CleanText(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

type ReportResponse struct {
	Status string `json:"status"`
}
