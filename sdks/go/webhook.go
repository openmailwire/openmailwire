package openmailwire

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultWebhookTolerance is how far a webhook timestamp may be from now.
const DefaultWebhookTolerance = 5 * time.Minute

// Errors returned (wrapped) by VerifyWebhook.
var (
	ErrWebhookHeaders   = errors.New("openmailwire: missing webhook headers: expected svix-id, svix-timestamp and svix-signature, or webhook-id, webhook-timestamp and webhook-signature")
	ErrWebhookTimestamp = errors.New("openmailwire: webhook timestamp is invalid or outside the tolerance")
	ErrWebhookSecret    = errors.New("openmailwire: the signing secret is not a valid whsec_… value or whpk_… public key")
	ErrWebhookSignature = errors.New("openmailwire: no webhook signature matches")
)

// Webhook event types (WebhookEvent.Type, CreateWebhookRequest.Events).
// Newer servers may add more.
const (
	EventEmailSent            = "email.sent"
	EventEmailDelivered       = "email.delivered"
	EventEmailDeliveryDelayed = "email.delivery_delayed"
	EventEmailBounced         = "email.bounced"
	EventEmailFailed          = "email.failed"
	EventEmailSuppressed      = "email.suppressed"
	EventEmailDeliveryUnknown = "email.delivery_unknown"
	EventEmailCanceled        = "email.canceled"
	EventEmailComplained      = "email.complained"
	// EventEmailReceived: a message arrived in an inbox; Data.EmailID is
	// the received message (Messages.Get).
	EventEmailReceived = "email.received"
	EventEmailHeld     = "email.held"
	EventEmailApproved = "email.approved"
	EventEmailRejected = "email.rejected"
	EventSendingPaused = "sending.paused"
)

// WebhookEvent is the JSON body of a webhook request.
type WebhookEvent struct {
	Type      string           `json:"type"`
	CreatedAt time.Time        `json:"created_at"`
	Data      WebhookEventData `json:"data"`
}

// WebhookEventData is the email an event is about. To holds only the
// recipient the event concerns; for email.received it is the inbox address,
// and HeaderTo is the message's own To header.
type WebhookEventData struct {
	EmailID   string            `json:"email_id"`
	CreatedAt time.Time         `json:"created_at"`
	From      string            `json:"from"`
	To        []string          `json:"to"`
	Subject   string            `json:"subject"`
	Tags      map[string]string `json:"tags,omitempty"`
	Bounce    *struct {
		Type    string `json:"type"`
		SubType string `json:"subType"`
		Message string `json:"message"`
	} `json:"bounce,omitempty"`
	Delivery *struct {
		AttemptID    string `json:"attempt_id,omitempty"`
		MXHost       string `json:"mx_host,omitempty"`
		SMTPCode     int    `json:"smtp_code,omitempty"`
		EnhancedCode string `json:"enhanced_code,omitempty"`
		Response     string `json:"response,omitempty"`
	} `json:"delivery,omitempty"`
	// Complaint is set for email.complained.
	Complaint *struct {
		FeedbackType string `json:"feedback_type,omitempty"`
		UserAgent    string `json:"user_agent,omitempty"`
	} `json:"complaint,omitempty"`
	// Approval is set for email.held (Reasons), email.approved (ApprovedBy,
	// Note) and email.rejected (RejectedBy, Note).
	Approval *struct {
		Reasons    []string `json:"reasons,omitempty"`
		ApprovedBy string   `json:"approved_by,omitempty"`
		RejectedBy string   `json:"rejected_by,omitempty"`
		Note       string   `json:"note,omitempty"`
	} `json:"approval,omitempty"`
	// Pause is set for sending.paused.
	Pause *Pause `json:"pause,omitempty"`

	// The fields below are set for email.received.
	InboxID          string   `json:"inbox_id,omitempty"`
	Inbox            string   `json:"inbox,omitempty"`
	ThreadID         string   `json:"thread_id,omitempty"`
	InReplyToEmailID string   `json:"in_reply_to_email_id,omitempty"`
	MessageID        string   `json:"message_id,omitempty"`
	Cc               []string `json:"cc,omitempty"`
	HeaderTo         []string `json:"header_to,omitempty"`
	// Trust is TrustAuthenticated, TrustUnauthenticated or TrustFailed.
	Trust          string `json:"trust,omitempty"`
	Authentication *struct {
		SPF   string `json:"spf,omitempty"`
		DKIM  string `json:"dkim,omitempty"`
		DMARC string `json:"dmarc,omitempty"`
	} `json:"authentication,omitempty"`
	Held                   bool `json:"held,omitempty"`
	SuspiciousInstructions bool `json:"suspicious_instructions,omitempty"`
}

func headerValue(h http.Header, name string) string {
	if v := h.Get(name); v != "" {
		return v
	}
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// VerifyWebhook checks that a webhook request was signed by OpenMailWire and
// is fresh.
//
// secret is either the webhook's signing secret ("whsec_…"), checked against
// the "v1" (HMAC-SHA256) signatures, or its Ed25519 public key ("whpk_…",
// from Webhooks.Get), checked against the "v1a" signatures of webhooks
// created with asymmetric signing.
//
//   - It accepts both header families: svix-id/svix-timestamp/svix-signature
//     and webhook-id/webhook-timestamp/webhook-signature.
//   - It accepts any of several space-separated signatures, so it keeps
//     working while a secret is rotated.
//   - It compares HMAC signatures in constant time.
//   - It refuses timestamps more than tolerance from now; 0 means
//     DefaultWebhookTolerance.
//
// body must be the request body exactly as received.
func VerifyWebhook(secret string, header http.Header, body []byte, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultWebhookTolerance
	}
	id, ts, sigs := headerValue(header, "svix-id"), headerValue(header, "svix-timestamp"), headerValue(header, "svix-signature")
	if id == "" || ts == "" || sigs == "" {
		id, ts, sigs = headerValue(header, "webhook-id"), headerValue(header, "webhook-timestamp"), headerValue(header, "webhook-signature")
	}
	if id == "" || ts == "" || sigs == "" {
		return ErrWebhookHeaders
	}
	// At most 15 digits, as the other SDKs: a far-future value such as
	// 9223372036854775807 would overflow time arithmetic below.
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || unix < 0 || len(ts) > 15 || strings.HasPrefix(ts, "+") {
		return fmt.Errorf("%w: %q is not a unix time", ErrWebhookTimestamp, ts)
	}
	now := time.Now().Unix()
	tol := int64(tolerance / time.Second)
	if now-unix > tol {
		return fmt.Errorf("%w: too old", ErrWebhookTimestamp)
	}
	if unix-now > tol {
		return fmt.Errorf("%w: in the future", ErrWebhookTimestamp)
	}

	signed := make([]byte, 0, len(id)+len(ts)+2+len(body))
	signed = append(append(append(signed, id+"."...), ts+"."...), body...)
	ok := false
	if pk, isPublic := strings.CutPrefix(secret, "whpk_"); isPublic {
		pub, err := base64.StdEncoding.DecodeString(pk)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return ErrWebhookSecret
		}
		for _, part := range strings.Split(sigs, " ") {
			version, sig, found := strings.Cut(part, ",")
			if !found || version != "v1a" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(sig)
			if err == nil && len(raw) == ed25519.SignatureSize && ed25519.Verify(pub, signed, raw) {
				ok = true
				break
			}
		}
	} else {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
		if err != nil || len(key) == 0 {
			return ErrWebhookSecret
		}
		m := hmac.New(sha256.New, key)
		m.Write(signed)
		expected := []byte(base64.StdEncoding.EncodeToString(m.Sum(nil)))
		for _, part := range strings.Split(sigs, " ") {
			version, sig, found := strings.Cut(part, ",")
			if found && version == "v1" && hmac.Equal([]byte(sig), expected) {
				ok = true
			}
		}
	}
	if !ok {
		return ErrWebhookSignature
	}
	return nil
}

// ParseWebhook verifies a webhook request like VerifyWebhook and decodes its
// body.
func ParseWebhook(secret string, header http.Header, body []byte, tolerance time.Duration) (*WebhookEvent, error) {
	if err := VerifyWebhook(secret, header, body, tolerance); err != nil {
		return nil, err
	}
	var ev WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("openmailwire: decoding webhook body: %w", err)
	}
	return &ev, nil
}
