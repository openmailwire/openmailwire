package openmailwire

import (
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
	ErrWebhookSecret    = errors.New("openmailwire: the signing secret is not a valid whsec_… value")
	ErrWebhookSignature = errors.New("openmailwire: no webhook signature matches")
)

// WebhookEvent is the JSON body of a webhook request.
type WebhookEvent struct {
	Type      string           `json:"type"`
	CreatedAt time.Time        `json:"created_at"`
	Data      WebhookEventData `json:"data"`
}

// WebhookEventData is the email an event is about. To holds only the
// recipient the event concerns.
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

// VerifyWebhook checks that a webhook request was signed by OpenMailWire with
// secret ("whsec_…") and is fresh.
//
//   - It accepts both header families: svix-id/svix-timestamp/svix-signature
//     and webhook-id/webhook-timestamp/webhook-signature.
//   - It accepts any of several space-separated "v1,<base64>" signatures.
//   - It compares signatures in constant time.
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
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || unix < 0 || strings.HasPrefix(ts, "+") {
		return fmt.Errorf("%w: %q is not a unix time", ErrWebhookTimestamp, ts)
	}
	age := time.Since(time.Unix(unix, 0))
	if age > tolerance {
		return fmt.Errorf("%w: too old", ErrWebhookTimestamp)
	}
	if -age > tolerance {
		return fmt.Errorf("%w: in the future", ErrWebhookTimestamp)
	}

	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) == 0 {
		return ErrWebhookSecret
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	expected := []byte(base64.StdEncoding.EncodeToString(m.Sum(nil)))

	ok := false
	for _, part := range strings.Split(sigs, " ") {
		version, sig, found := strings.Cut(part, ",")
		if found && version == "v1" && hmac.Equal([]byte(sig), expected) {
			ok = true
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
