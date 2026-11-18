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
}

// SendRawRequest is the body of POST /emails/raw.
type SendRawRequest struct {
	// Raw is the complete MIME message; it is base64-encoded on the wire.
	Raw      []byte `json:"raw"`
	MailFrom string `json:"mail_from,omitempty"`
	// Rcpts are the envelope recipients. Default: the To and Cc headers.
	Rcpts []string `json:"rcpts,omitempty"`
}

// SendResponse is the id of a queued email.
type SendResponse struct {
	ID string `json:"id"`
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

// Recipient statuses. Queued, sending and deferred are not final.
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
	case StatusQueued, StatusSending, StatusDeferred:
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
