package openmailwire

import (
	"context"
	"encoding/json"
	"time"
)

// Tag labels an email; letters, digits, _ and - only.
type Tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Attachment is a file sent with an email.
type Attachment struct {
	Filename string `json:"filename"`
	// Content is the file's raw bytes; it is base64-encoded on the wire.
	Content     []byte `json:"content"`
	ContentType string `json:"content_type,omitempty"`
	// ContentID makes the attachment inline: reference it from the HTML as
	// "cid:<ContentID>".
	ContentID string `json:"content_id,omitempty"`
}

// SendEmailRequest is the body of POST /emails.
type SendEmailRequest struct {
	// From is "Name <addr@domain>" or "addr@domain", on a verified domain.
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Cc      []string          `json:"cc,omitempty"`
	Bcc     []string          `json:"bcc,omitempty"`
	ReplyTo []string          `json:"reply_to,omitempty"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html,omitempty"`
	Text    string            `json:"text,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tags    []Tag             `json:"tags,omitempty"`
	// Attachments: at most 20 files, 10 MB decoded in total. Not allowed in
	// a batch.
	Attachments []Attachment `json:"attachments,omitempty"`
	// ScheduledAt sends later: in the future and at most 30 days ahead.
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	// ExpiresAt stops new attempts after this time. Default 48 h, at most 7 days.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// RetryUncertain retries an attempt whose outcome is unknown (the
	// default); set it to Ptr(false) for receipts.
	RetryUncertain *bool `json:"retry_uncertain,omitempty"`
	// Stream is StreamTransactional (the default) or StreamBroadcast, which
	// adds a signed one-click unsubscribe and skips unsubscribed addresses.
	Stream string `json:"stream,omitempty"`
}

// Streams of an email.
const (
	StreamTransactional = "transactional"
	StreamBroadcast     = "broadcast"
)

// Hold reasons: the require_approval rules that matched.
const (
	HoldAlways         = "always"
	HoldFirstContact   = "first_contact"
	HoldHasAttachments = "has_attachments"
	HoldHasLinks       = "has_links"
	HoldRecipientsOver = "recipients_over"
)

// SendRawRequest is the body of POST /emails/raw.
type SendRawRequest struct {
	// Raw is the complete MIME message; it is base64-encoded on the wire.
	Raw      []byte `json:"raw"`
	MailFrom string `json:"mail_from,omitempty"`
	// Rcpts are the envelope recipients. Default: the To and Cc headers.
	Rcpts []string `json:"rcpts,omitempty"`
}

// SendResponse is the id of a queued email. Held is true when the key's
// require_approval policy holds it: nothing is sent until it is approved.
type SendResponse struct {
	ID          string   `json:"id"`
	Held        bool     `json:"held,omitempty"`
	HoldReasons []string `json:"hold_reasons,omitempty"`
}

// BatchResponse lists the ids of a batch, in order.
type BatchResponse struct {
	Data []SendResponse `json:"data"`
}

// CancelEmailResponse is the answer of POST /emails/{id}/cancel.
type CancelEmailResponse struct {
	Object string `json:"object"`
	ID     string `json:"id"`
}

// Recipient statuses. Queued, sending, deferred and held are not final.
const (
	StatusQueued     = "queued"
	StatusSending    = "sending"
	StatusDeferred   = "deferred"
	StatusDelivered  = "delivered"
	StatusBounced    = "bounced"
	StatusExpired    = "expired"
	StatusSuppressed = "suppressed"
	StatusUnknown    = "unknown"
	StatusCanceled   = "canceled"
	// StatusHeld waits for approval (EmailsService.Approve).
	StatusHeld = "held"
	// StatusRejected was rejected by an approver and is never sent.
	StatusRejected = "rejected"
)

// Recipient is one recipient's delivery state.
type Recipient struct {
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	LastCode      *int       `json:"last_code"`
	LastResponse  string     `json:"last_response"`
}

// Final reports whether the recipient's status will not change any more.
func (r Recipient) Final() bool {
	switch r.Status {
	case StatusQueued, StatusSending, StatusDeferred, StatusHeld:
		return false
	}
	return true
}

// Attempt is one delivery attempt to one recipient.
type Attempt struct {
	ID           string     `json:"id"`
	Recipient    string     `json:"recipient"`
	Worker       string     `json:"worker"`
	SourceIP     string     `json:"source_ip"`
	MXHost       string     `json:"mx_host"`
	Outcome      string     `json:"outcome"`
	SMTPCode     *int       `json:"smtp_code"`
	EnhancedCode string     `json:"enhanced_code"`
	Response     string     `json:"response"`
	TLS          string     `json:"tls"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// Event is one entry of an email's timeline.
type Event struct {
	Type      string          `json:"type"`
	Recipient string          `json:"recipient,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Email is one email with each recipient's state, every attempt and the
// event timeline.
type Email struct {
	Object      string            `json:"object"`
	ID          string            `json:"id"`
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Cc          []string          `json:"cc"`
	Bcc         []string          `json:"bcc"`
	Subject     string            `json:"subject"`
	MessageID   string            `json:"message_id"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	ScheduledAt *time.Time        `json:"scheduled_at,omitempty"`
	LastEvent   string            `json:"last_event"`
	Tags        map[string]string `json:"tags"`
	BodyPurged  bool              `json:"body_purged"`
	ThreadID    string            `json:"thread_id,omitempty"`
	APIKeyID    string            `json:"api_key_id,omitempty"`
	Recipients  []Recipient       `json:"recipients"`
	Attempts    []Attempt         `json:"attempts"`
	Events      []Event           `json:"events"`
}

// EmailSummary is one entry of GET /emails.
type EmailSummary struct {
	ID          string     `json:"id"`
	From        string     `json:"from"`
	To          []string   `json:"to"`
	Subject     string     `json:"subject"`
	CreatedAt   time.Time  `json:"created_at"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	LastEvent   string     `json:"last_event"`
	APIKeyID    string     `json:"api_key_id"`
}

// EmailList is a page of emails, newest first.
type EmailList struct {
	Object     string         `json:"object"`
	Data       []EmailSummary `json:"data"`
	HasMore    bool           `json:"has_more"`
	NextBefore string         `json:"next_before"`
}

// ListEmailsParams filters GET /emails. Zero values are left out.
type ListEmailsParams struct {
	Limit int
	// Before is an email id: list emails older than it (use NextBefore).
	Before   string
	APIKeyID string
}

// EmailsService sends and inspects emails.
type EmailsService struct{ c *Client }

// Send queues an email and returns its id.
func (s *EmailsService) Send(ctx context.Context, req *SendEmailRequest, opts ...RequestOption) (*SendResponse, error) {
	var out SendResponse
	if err := s.c.do(ctx, "POST", "/emails", nil, req, &out, opts); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns one email with each recipient's status, every attempt and
// the event timeline.
func (s *EmailsService) Get(ctx context.Context, id string) (*Email, error) {
	var out Email
	if err := s.c.do(ctx, "GET", "/emails/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns emails, newest first. Pass NextBefore as Before for the next
// page.
func (s *EmailsService) List(ctx context.Context, params *ListEmailsParams) (*EmailList, error) {
	if params == nil {
		params = &ListEmailsParams{}
	}
	q := query("limit", itoa(params.Limit), "before", params.Before, "api_key_id", params.APIKeyID)
	var out EmailList
	if err := s.c.do(ctx, "GET", "/emails", q, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel cancels every recipient not tried yet. The API answers 409
// cannot_cancel if there is none.
func (s *EmailsService) Cancel(ctx context.Context, id string) (*CancelEmailResponse, error) {
	var out CancelEmailResponse
	if err := s.c.do(ctx, "POST", "/emails/"+seg(id)+"/cancel", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Batch queues 1 to 100 emails (without attachments) in one transaction. An
// IdempotencyKey applies to the whole batch.
func (s *EmailsService) Batch(ctx context.Context, emails []SendEmailRequest, opts ...RequestOption) (*BatchResponse, error) {
	if emails == nil {
		emails = []SendEmailRequest{}
	}
	var out BatchResponse
	if err := s.c.do(ctx, "POST", "/emails/batch", nil, emails, &out, opts); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendRaw queues a complete MIME message. Its From domain must be verified.
func (s *EmailsService) SendRaw(ctx context.Context, req *SendRawRequest, opts ...RequestOption) (*SendResponse, error) {
	var out SendResponse
	if err := s.c.do(ctx, "POST", "/emails/raw", nil, req, &out, opts); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrorBody is an error in the API's {statusCode, name, message} shape, as
// CheckResult lists them.
type ErrorBody struct {
	StatusCode int    `json:"statusCode"`
	Name       string `json:"name"`
	Message    string `json:"message"`
}

// CheckWarning is something a check found that would not stop the send.
type CheckWarning struct {
	// Code is no_text_part, html_too_large, link_domain_mismatch,
	// missing_unsubscribe, domain_not_verified or large_message.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CheckResult is what POST /emails/check answers: what a send would do now.
type CheckResult struct {
	Object string `json:"object"`
	// OK is true when there is no refusal and no warning.
	OK bool `json:"ok"`
	// Would is "send", "hold" or "refuse".
	Would  string `json:"would"`
	Stream string `json:"stream"`
	// Size is the message size in bytes as it would be sent (estimated).
	Size        int64          `json:"size"`
	HoldReasons []string       `json:"hold_reasons"`
	Refusals    []ErrorBody    `json:"refusals"`
	Warnings    []CheckWarning `json:"warnings"`
}

// Check reports what Send would do with req now (send, hold or refuse,
// with every refusal and warning), without sending anything.
func (s *EmailsService) Check(ctx context.Context, req *SendEmailRequest) (*CheckResult, error) {
	var out CheckResult
	if err := s.c.do(ctx, "POST", "/emails/check", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// HeldEmail is an email waiting for approval.
type HeldEmail struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	// APIKeyID is the key that sent it.
	APIKeyID    string   `json:"api_key_id"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	Cc          []string `json:"cc"`
	Bcc         []string `json:"bcc"`
	Subject     string   `json:"subject"`
	Stream      string   `json:"stream"`
	HoldReasons []string `json:"hold_reasons"`
	// Recipients are the recipients waiting for approval.
	Recipients []string  `json:"recipients"`
	CreatedAt  time.Time `json:"created_at"`
	// ExpiresAt: held mail nobody decides on expires then.
	ExpiresAt   time.Time  `json:"expires_at"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	// Text is the first 2000 characters of the text, for the reviewer.
	Text          string `json:"text"`
	TextTruncated bool   `json:"text_truncated"`
}

// HeldEmailList is a page of held emails, newest first.
type HeldEmailList struct {
	Object     string      `json:"object"`
	Data       []HeldEmail `json:"data"`
	HasMore    bool        `json:"has_more"`
	NextBefore string      `json:"next_before"`
}

// ListHeldParams pages GET /emails/held. Zero values are left out.
type ListHeldParams struct {
	Limit  int
	Before string
}

// ListHeld returns the emails waiting for approval, newest first.
func (s *EmailsService) ListHeld(ctx context.Context, params *ListHeldParams) (*HeldEmailList, error) {
	if params == nil {
		params = &ListHeldParams{}
	}
	var out HeldEmailList
	q := query("limit", itoa(params.Limit), "before", params.Before)
	if err := s.c.do(ctx, "GET", "/emails/held", q, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecisionRequest is the optional body of Approve and Reject.
type DecisionRequest struct {
	// Note says why; it is kept with the decision and sent in the webhook.
	Note string `json:"note,omitempty"`
}

// Decision is the answer of Approve and Reject.
type Decision struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	// Decision is "approved" or "rejected".
	Decision string `json:"decision"`
	// Recipients were approved (now queued) or rejected.
	Recipients []string `json:"recipients"`
	// Suppressed were approved but suppressed since the email was held, so
	// they are not sent.
	Suppressed []string `json:"suppressed"`
}

func (s *EmailsService) decide(ctx context.Context, id, verb string, req *DecisionRequest) (*Decision, error) {
	if req == nil {
		req = &DecisionRequest{}
	}
	var out Decision
	if err := s.c.do(ctx, "POST", "/emails/"+seg(id)+"/"+verb, nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Approve queues a held email. It needs a full-access key or one whose
// policy has CanApprove; no key approves its own email. req may be nil.
func (s *EmailsService) Approve(ctx context.Context, id string, req *DecisionRequest) (*Decision, error) {
	return s.decide(ctx, id, "approve", req)
}

// Reject rejects a held email: it is never sent. req may be nil.
func (s *EmailsService) Reject(ctx context.Context, id string, req *DecisionRequest) (*Decision, error) {
	return s.decide(ctx, id, "reject", req)
}

// EmailReplies is the answer of GET /emails/{id}/replies.
type EmailReplies struct {
	Object   string `json:"object"`
	EmailID  string `json:"email_id"`
	ThreadID string `json:"thread_id"`
	// Data are the received messages that answer the email, oldest first.
	Data []MessageSummary `json:"data"`
}

// Replies returns the received messages that answer a sent email.
func (s *EmailsService) Replies(ctx context.Context, id string) (*EmailReplies, error) {
	var out EmailReplies
	if err := s.c.do(ctx, "GET", "/emails/"+seg(id)+"/replies", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}
