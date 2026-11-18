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

type HeartbeatRequest struct {
	PublicIP string `json:"public_ip"`
	EHLO     string `json:"ehlo"`
	Capacity int    `json:"capacity"`
	Version  string `json:"version"`
}

type HeartbeatResponse struct {
	Draining bool `json:"draining"`
}

type LeaseRequest struct {
	Max int `json:"max"`
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
	TLS          string `json:"tls"` // "", "none", "opportunistic" or "verified"
}

type ReportResponse struct {
	Status string `json:"status"`
}
