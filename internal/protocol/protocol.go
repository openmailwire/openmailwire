// Package protocol holds the JSON shapes that the control plane and its
// workers exchange on the internal worker API.
package protocol

import "time"

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

type HeartbeatRequest struct {
	PublicIP string `json:"public_ip"`
	EHLO     string `json:"ehlo"`
	Capacity int    `json:"capacity"`
	Version  string `json:"version"`
	Kind     string `json:"kind,omitempty"` // "direct" (or empty) or "relay"
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
}

type ReportResponse struct {
	Status string `json:"status"`
}
