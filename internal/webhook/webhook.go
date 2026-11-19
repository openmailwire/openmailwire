// Package webhook turns timeline events into signed HTTP callbacks.
//
// Requests are signed the way Resend signs them (Svix headers) and also with
// the Standard Webhooks headers; both use the same HMAC-SHA256 scheme, so an
// existing Resend webhook handler verifies OpenMailWire requests unchanged.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openmailwire/openmailwire/internal/secret"
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

// NewSecret returns a signing secret in the "whsec_<base64>" form.
func NewSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "whsec_" + base64.StdEncoding.EncodeToString(b)
}

func secretKey(whsec string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimPrefix(whsec, "whsec_"))
}

// Sign returns the "v1,<base64>" signature of body for message id and unix
// timestamp ts.
func Sign(whsec, id string, ts int64, body []byte) (string, error) {
	key, err := secretKey(whsec)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d.", id, ts)
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil)), nil
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
	return nil
}

var errBlocked = errors.New("webhook endpoint resolves to a private or local address")

// NewHTTPClient returns a client that refuses to connect to loopback,
// private, link-local and other internal addresses unless allowPrivate is
// set. The check runs on the address actually dialed, after DNS, so a
// hostname that resolves to an internal IP is refused too.
func NewHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || internal(ip) {
				return errBlocked
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 4,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var cgnat = &net.IPNet{IP: net.IP{100, 64, 0, 0}, Mask: net.CIDRMask(10, 32)}

func internal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || cgnat.Contains(ip)
}

// Dispatcher fans events out to webhook deliveries and sends them.
type Dispatcher struct {
	Store  *store.Store
	Sealer *secret.Sealer
	HTTP   *http.Client
	Log    *slog.Logger
	// UserAgent is sent with every request.
	UserAgent string
}

// Run works until ctx is cancelled, checking for new events every tick.
func (d *Dispatcher) Run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		d.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step fans out new events and sends the deliveries that are due.
func (d *Dispatcher) Step(ctx context.Context) {
	for {
		n, err := d.Store.FanoutWebhooks(ctx, 500, Build)
		if err != nil {
			if ctx.Err() == nil {
				d.Log.Error("webhook fanout", "err", err)
			}
			break
		}
		if n == 0 {
			break
		}
	}
	due, err := d.Store.ClaimWebhookDeliveries(ctx, 50, 2*time.Minute)
	if err != nil {
		if ctx.Err() == nil {
			d.Log.Error("webhook claim", "err", err)
		}
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for _, w := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(w store.DueWebhook) {
			defer func() { <-sem; wg.Done() }()
			d.send(ctx, w)
		}(w)
	}
	wg.Wait()
}

func (d *Dispatcher) send(ctx context.Context, w store.DueWebhook) {
	status, err := d.post(ctx, w)
	ok := err == nil && status >= 200 && status < 300
	errText := ""
	if err != nil {
		errText = err.Error()
	} else if !ok {
		errText = "endpoint answered " + strconv.Itoa(status)
	}
	var next *time.Time
	if !ok && w.Attempts <= len(Schedule) {
		t := time.Now().Add(Schedule[w.Attempts-1])
		next = &t
	}
	if err := d.Store.FinishWebhookDelivery(context.WithoutCancel(ctx), w.ID, ok, status, errText, next); err != nil {
		d.Log.Error("webhook finish", "delivery", w.ID, "err", err)
	}
	if !ok {
		d.Log.Warn("webhook attempt failed", "delivery", w.ID, "type", w.Type, "attempt", w.Attempts, "err", errText)
	}
}

func (d *Dispatcher) post(ctx context.Context, w store.DueWebhook) (int, error) {
	whsec, err := d.Sealer.Open(w.Secret)
	if err != nil {
		return 0, fmt.Errorf("unseal secret: %w", err)
	}
	id := "msg_" + strings.ReplaceAll(w.ID, "-", "")
	ts := time.Now().Unix()
	sig, err := Sign(string(whsec), id, ts, w.Payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Endpoint, bytes.NewReader(w.Payload))
	if err != nil {
		return 0, err
	}
	tsStr := strconv.FormatInt(ts, 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", d.UserAgent)
	req.Header.Set("svix-id", id)
	req.Header.Set("svix-timestamp", tsStr)
	req.Header.Set("svix-signature", sig)
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", tsStr)
	req.Header.Set("webhook-signature", sig)
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}
