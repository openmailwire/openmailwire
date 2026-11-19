// Package webhook turns timeline events into signed HTTP callbacks.
//
// Requests are signed the way Resend signs them (Svix headers) and also with
// the Standard Webhooks headers; both use the same HMAC-SHA256 scheme, so an
// existing Resend webhook handler verifies OpenMailWire requests unchanged.
// A webhook may also sign with Ed25519 (Standard Webhooks "v1a"), so a
// receiver can verify with a public key instead of a shared secret.
package webhook

import (
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
)

// Event types a webhook can subscribe to. All of them are per recipient.
const (
	EmailSent            = "email.sent"
	EmailDelivered       = "email.delivered"
	EmailDeliveryDelayed = "email.delivery_delayed"
	EmailBounced         = "email.bounced"
	EmailFailed          = "email.failed"
	EmailSuppressed      = "email.suppressed"
	EmailDeliveryUnknown = "email.delivery_unknown"
	EmailCanceled        = "email.canceled"
	EmailComplained      = "email.complained" // the recipient reported the email as spam (ARF)
	EmailReceived        = "email.received"   // an inbox received a message

	// Agent safety (lane B).
	EmailHeld     = "email.held"     // held for approval by the sending key's require_approval policy
	EmailApproved = "email.approved" // approved; delivery starts
	EmailRejected = "email.rejected" // rejected; never sent
	SendingPaused = "sending.paused" // a circuit breaker paused a key, domain or project
)

var Types = []string{EmailSent, EmailDelivered, EmailDeliveryDelayed, EmailBounced, EmailFailed, EmailSuppressed,
	EmailDeliveryUnknown, EmailCanceled, EmailComplained, EmailReceived,
	EmailHeld, EmailApproved, EmailRejected, SendingPaused}

func ValidType(t string) bool {
	for _, v := range Types {
		if v == t {
			return true
		}
	}
	return false
}

// ExternalType maps a timeline event to the webhook event it produces, or ""
// for none.
func ExternalType(internal string) string {
	switch internal {
	case "queued":
		return EmailSent
	case "delivered", "late_report_delivered":
		return EmailDelivered
	case "deferred", "unknown_retry", "lease_expired":
		return EmailDeliveryDelayed
	case "bounced":
		return EmailBounced
	case "expired":
		return EmailFailed
	case "suppressed":
		return EmailSuppressed
	case "unknown":
		return EmailDeliveryUnknown
	case "canceled":
		return EmailCanceled
	case "complained":
		return EmailComplained
	case "received":
		return EmailReceived
	case "held":
		return EmailHeld
	case "approved":
		return EmailApproved
	case "rejected":
		return EmailRejected
	case "sending_paused":
		return SendingPaused
	}
	return ""
}

type payload struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      data      `json:"data"`
}

type data struct {
	EmailID   string            `json:"email_id"`
	CreatedAt time.Time         `json:"created_at"`
	From      string            `json:"from"`
	To        []string          `json:"to"`
	Subject   string            `json:"subject"`
	Tags      map[string]string `json:"tags,omitempty"`
	Bounce    *bounce           `json:"bounce,omitempty"`
	Delivery  *delivery         `json:"delivery,omitempty"`
	Complaint *complaint        `json:"complaint,omitempty"`

	// email.received only. email_id is then the inbound message's id
	// (GET /messages/{id}).
	Cc               []string `json:"cc,omitempty"`
	MessageID        string   `json:"message_id,omitempty"`
	InboxID          string   `json:"inbox_id,omitempty"`
	Inbox            string   `json:"inbox,omitempty"`
	ThreadID         string   `json:"thread_id,omitempty"`
	InReplyToEmailID string   `json:"in_reply_to_email_id,omitempty"`

	// Lane B. email.held/approved/rejected: the hold reasons, or who
	// decided with their note. sending.paused: the pause and its rates.
	Approval json.RawMessage `json:"approval,omitempty"`
	Pause    json.RawMessage `json:"pause,omitempty"`
	// Lane D (v0.6): "to" is the inbox that received the message; the
	// message's own To header is header_to. Sender authentication comes
	// from the "received" event's data.
	HeaderTo               []string          `json:"header_to,omitempty"`
	Trust                  string            `json:"trust,omitempty"`
	Authentication         map[string]string `json:"authentication,omitempty"`
	Held                   *bool             `json:"held,omitempty"`
	SuspiciousInstructions *bool             `json:"suspicious_instructions,omitempty"`
}

type complaint struct {
	FeedbackType string `json:"feedback_type,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
}

type bounce struct {
	Type    string `json:"type"`
	SubType string `json:"subType"`
	Message string `json:"message"`
}

type delivery struct {
	AttemptID    string `json:"attempt_id,omitempty"`
	MXHost       string `json:"mx_host,omitempty"`
	SMTPCode     int    `json:"smtp_code,omitempty"`
	EnhancedCode string `json:"enhanced_code,omitempty"`
	Response     string `json:"response,omitempty"`
}

// Payload renders the JSON body for one event. "to" holds only the
// recipient the event is about, so a handler that acts on every address in
// "to" (suppressing it on a bounce, say) never touches the others.
func Payload(ev store.EventRow, typ string) ([]byte, error) {
	to := ev.To
	if ev.Recipient != "" {
		to = []string{ev.Recipient}
	}
	p := payload{Type: typ, CreatedAt: ev.CreatedAt.UTC(), Data: data{
		EmailID: ev.EmailID, CreatedAt: ev.EmailCreatedAt.UTC(), From: ev.From, To: to, Subject: ev.Subject,
	}}
	if len(ev.Tags) > 0 {
		p.Data.Tags = ev.Tags
	}
	var d delivery
	if len(ev.Data) > 0 {
		json.Unmarshal(ev.Data, &d)
	}
	if d != (delivery{}) {
		p.Data.Delivery = &d
	}
	if typ == EmailBounced {
		sub := "General"
		if strings.HasPrefix(d.EnhancedCode, "5.1.") {
			sub = "NoEmail"
		}
		p.Data.Bounce = &bounce{Type: "Permanent", SubType: sub, Message: d.Response}
	}
	if typ == EmailComplained {
		var c complaint
		json.Unmarshal(ev.Data, &c)
		p.Data.Complaint = &c
		// Lane D: a report that does not say which recipient complained is
		// about the email, not about every recipient of it.
		if ev.Recipient == "" {
			p.Data.To = []string{}
		}
	}
	switch typ { // lane B
	case EmailHeld, EmailApproved, EmailRejected:
		p.Data.Delivery = nil
		if len(ev.Data) > 0 && json.Valid(ev.Data) {
			p.Data.Approval = ev.Data
		}
	case SendingPaused:
		p.Data.Delivery = nil
		var d struct {
			Pause json.RawMessage `json:"pause"`
		}
		json.Unmarshal(ev.Data, &d)
		p.Data.Pause = d.Pause
	}
	if typ == EmailReceived {
		p.Data.Delivery = nil
		// Lane D: "to" is the inbox, whatever the sender wrote in To.
		p.Data.HeaderTo = ev.To
		p.Data.To = []string{ev.InboxAddress}
		var auth struct {
			Trust                  string            `json:"trust"`
			Authentication         map[string]string `json:"authentication"`
			Held                   *bool             `json:"held"`
			SuspiciousInstructions *bool             `json:"suspicious_instructions"`
		}
		json.Unmarshal(ev.Data, &auth)
		p.Data.Trust, p.Data.Authentication, p.Data.Held, p.Data.SuspiciousInstructions =
			auth.Trust, auth.Authentication, auth.Held, auth.SuspiciousInstructions
		p.Data.Cc = ev.Cc
		if ev.MessageID != "" {
			p.Data.MessageID = "<" + ev.MessageID + ">"
		}
		p.Data.InboxID, p.Data.Inbox = ev.InboxID, ev.InboxAddress
		p.Data.ThreadID, p.Data.InReplyToEmailID = ev.ThreadID, ev.InReplyToEmailID
	}
	return json.Marshal(p)
}

// Build lists the deliveries an event produces: one per webhook of the
// event's project that subscribes to its type and existed when it happened.
func Build(ev store.EventRow, hooks []store.Webhook) []store.NewWebhookDelivery {
	typ := ExternalType(ev.Type)
	if typ == "" {
		return nil
	}
	var out []store.NewWebhookDelivery
	var body []byte
	for _, h := range hooks {
		if h.CreatedAt.After(ev.CreatedAt) || !subscribed(h.Events, typ) {
			continue
		}
		if body == nil {
			b, err := Payload(ev, typ)
			if err != nil {
				return out
			}
			body = b
		}
		out = append(out, store.NewWebhookDelivery{WebhookID: h.ID, EventID: ev.ID, Type: typ, Payload: body})
	}
	return out
}

func subscribed(events []string, typ string) bool {
	if len(events) == 0 {
		return true
	}
	for _, e := range events {
		if e == typ {
			return true
		}
	}
	return false
}

// Schedule is the wait after attempt n (1-based). About a day in total.
var Schedule = []time.Duration{5 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 5 * time.Hour, 10 * time.Hour, 10 * time.Hour}

// ValidateEndpoint checks a webhook URL before it is stored.
func ValidateEndpoint(raw string, allowHTTP bool) error {
	return ValidateEndpointFor(raw, allowHTTP, true)
}

// ValidateEndpointFor is ValidateEndpoint that also refuses an IP literal
// that is not a public address, unless allowPrivate is set. Host names are
// checked when they are dialed (NewHTTPClient), after DNS.
func ValidateEndpointFor(raw string, allowHTTP, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("endpoint must be an absolute URL")
	}
	if u.User != nil {
		return errors.New("endpoint must not contain credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return errors.New("endpoint must use https")
		}
	default:
		return errors.New("endpoint must use https")
	}
	if !allowPrivate {
		if a, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !Public(a) {
			return errors.New("endpoint must be a public address; loopback, private and reserved addresses are refused")
		}
	}
	return nil
}
