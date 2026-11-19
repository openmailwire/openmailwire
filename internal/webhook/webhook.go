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
)

var Types = []string{EmailSent, EmailDelivered, EmailDeliveryDelayed, EmailBounced, EmailFailed, EmailSuppressed,
	EmailDeliveryUnknown, EmailCanceled, EmailComplained, EmailReceived}

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
	}
	if typ == EmailReceived {
		p.Data.Delivery = nil
		if len(to) == 0 && ev.InboxAddress != "" {
			p.Data.To = []string{ev.InboxAddress}
		}
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
