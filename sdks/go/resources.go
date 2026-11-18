package openmailwire

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// Deleted is the answer of a DELETE.
type Deleted struct {
	Object  string `json:"object"`
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// ------------------------------------------------------------------ domains

// DNSRecord is a record to publish for a domain, with the result of the
// last check.
type DNSRecord struct {
	Record   string `json:"record"` // DKIM, SPF, MX, DMARC or INBOUND
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Required bool   `json:"required"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
}

// Domain is a sending domain.
type Domain struct {
	Object     string      `json:"object"`
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Status     string      `json:"status"` // pending, verified or failed
	CreatedAt  time.Time   `json:"created_at"`
	VerifiedAt *time.Time  `json:"verified_at"`
	CheckedAt  *time.Time  `json:"checked_at"`
	Records    []DNSRecord `json:"records"`
}

// DomainList is the answer of GET /domains.
type DomainList struct {
	Object string   `json:"object"`
	Data   []Domain `json:"data"`
}

// DomainsService manages sending domains.
type DomainsService struct{ c *Client }

// Create adds a domain and returns the DNS records to publish.
func (s *DomainsService) Create(ctx context.Context, name string) (*Domain, error) {
	var out Domain
	if err := s.c.do(ctx, "POST", "/domains", nil, map[string]string{"name": name}, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every domain of the project.
func (s *DomainsService) List(ctx context.Context) (*DomainList, error) {
	var out DomainList
	if err := s.c.do(ctx, "GET", "/domains", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns one domain with its records and their last check.
func (s *DomainsService) Get(ctx context.Context, id string) (*Domain, error) {
	var out Domain
	if err := s.c.do(ctx, "GET", "/domains/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Verify checks the domain's DNS records now.
func (s *DomainsService) Verify(ctx context.Context, id string) (*Domain, error) {
	var out Domain
	if err := s.c.do(ctx, "POST", "/domains/"+seg(id)+"/verify", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a domain.
func (s *DomainsService) Delete(ctx context.Context, id string) (*Deleted, error) {
	var out Deleted
	if err := s.c.do(ctx, "DELETE", "/domains/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ webhooks

// CreateWebhookRequest is the body of POST /webhooks.
type CreateWebhookRequest struct {
	// Endpoint is an https URL.
	Endpoint string `json:"endpoint"`
	// Events to receive, e.g. "email.bounced". Empty means every type.
	Events []string `json:"events,omitempty"`
}

// WebhookDelivery is one attempt to deliver an event to a webhook.
type WebhookDelivery struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastStatus  *int       `json:"last_status"`
	LastError   string     `json:"last_error"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

// Webhook is an endpoint that receives signed events.
type Webhook struct {
	Object    string    `json:"object"`
	ID        string    `json:"id"`
	Endpoint  string    `json:"endpoint"`
	Events    []string  `json:"events"`
	CreatedAt time.Time `json:"created_at"`
	// SigningSecret ("whsec_…") is returned once, by Create.
	SigningSecret string `json:"signing_secret,omitempty"`
	// RecentDeliveries (the 20 most recent) is returned by Get.
	RecentDeliveries []WebhookDelivery `json:"recent_deliveries,omitempty"`
}

// WebhookList is the answer of GET /webhooks.
type WebhookList struct {
	Object string    `json:"object"`
	Data   []Webhook `json:"data"`
}

// WebhooksService manages webhooks.
type WebhooksService struct{ c *Client }

// Create creates a webhook. The answer carries SigningSecret, shown once.
func (s *WebhooksService) Create(ctx context.Context, req *CreateWebhookRequest) (*Webhook, error) {
	var out Webhook
	if err := s.c.do(ctx, "POST", "/webhooks", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every webhook of the project.
func (s *WebhooksService) List(ctx context.Context) (*WebhookList, error) {
	var out WebhookList
	if err := s.c.do(ctx, "GET", "/webhooks", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns one webhook with its 20 most recent deliveries.
func (s *WebhooksService) Get(ctx context.Context, id string) (*Webhook, error) {
	var out Webhook
	if err := s.c.do(ctx, "GET", "/webhooks/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a webhook.
func (s *WebhooksService) Delete(ctx context.Context, id string) (*Deleted, error) {
	var out Deleted
	if err := s.c.do(ctx, "DELETE", "/webhooks/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ API keys

// API key permissions.
const (
	FullAccess    = "full_access"
	SendingAccess = "sending_access"
)

// APIKeyPolicy holds the sending rules the server enforces for a key. Every
// field is optional; nil or empty means no limit.
type APIKeyPolicy struct {
	MaxPerHour *int `json:"max_per_hour,omitempty"`
	MaxPerDay  *int `json:"max_per_day,omitempty"`
	// AllowedRecipients are patterns such as "*@acme.com".
	AllowedRecipients []string `json:"allowed_recipients,omitempty"`
	AllowedFrom       []string `json:"allowed_from,omitempty"`
}

// APIKey is an API key. Token is set only in the answer of Create.
type APIKey struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Prefix     string       `json:"prefix"`
	Permission string       `json:"permission"`
	Policy     APIKeyPolicy `json:"policy"`
	CreatedAt  time.Time    `json:"created_at"`
	LastUsedAt *time.Time   `json:"last_used_at"`
	Token      string       `json:"token,omitempty"`
}

// APIKeyList is the answer of GET /api-keys.
type APIKeyList struct {
	Object string   `json:"object"`
	Data   []APIKey `json:"data"`
}

// CreateAPIKeyRequest is the body of POST /api-keys.
type CreateAPIKeyRequest struct {
	Name string `json:"name"`
	// Permission is FullAccess (the default) or SendingAccess.
	Permission string        `json:"permission,omitempty"`
	Policy     *APIKeyPolicy `json:"policy,omitempty"`
}

// UpdateAPIKeyRequest is the body of PATCH /api-keys/{id}. Zero fields are
// left unchanged; a non-nil Policy replaces the key's policy.
type UpdateAPIKeyRequest struct {
	Name   string        `json:"name,omitempty"`
	Policy *APIKeyPolicy `json:"policy,omitempty"`
}

// APIKeysService manages API keys. It needs a full-access key.
type APIKeysService struct{ c *Client }

// Create creates a key. The answer carries Token, shown once.
func (s *APIKeysService) Create(ctx context.Context, req *CreateAPIKeyRequest) (*APIKey, error) {
	var out APIKey
	if err := s.c.do(ctx, "POST", "/api-keys", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every key of the project.
func (s *APIKeysService) List(ctx context.Context) (*APIKeyList, error) {
	var out APIKeyList
	if err := s.c.do(ctx, "GET", "/api-keys", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns one key.
func (s *APIKeysService) Get(ctx context.Context, id string) (*APIKey, error) {
	var out APIKey
	if err := s.c.do(ctx, "GET", "/api-keys/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Update renames a key or replaces its policy.
func (s *APIKeysService) Update(ctx context.Context, id string, req *UpdateAPIKeyRequest) (*APIKey, error) {
	var out APIKey
	if err := s.c.do(ctx, "PATCH", "/api-keys/"+seg(id), nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete revokes a key.
func (s *APIKeysService) Delete(ctx context.Context, id string) (*Deleted, error) {
	var out Deleted
	if err := s.c.do(ctx, "DELETE", "/api-keys/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ inboxes

// Inbox is an address that receives mail for the project.
type Inbox struct {
	ID        string    `json:"id"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
}

// InboxList is the answer of GET /inboxes.
type InboxList struct {
	Object string  `json:"object"`
	Data   []Inbox `json:"data"`
}

// AddressList is a list of addresses that also accepts a single string.
type AddressList []string

// UnmarshalJSON accepts "a@b.c", ["a@b.c", …] or null.
func (l *AddressList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = nil
		if one != "" {
			*l = AddressList{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

// MessageSummary is one entry of an inbox's message list.
type MessageSummary struct {
	ID               string      `json:"id"`
	From             string      `json:"from"`
	To               AddressList `json:"to"`
	Subject          string      `json:"subject"`
	ReceivedAt       time.Time   `json:"received_at"`
	ThreadID         string      `json:"thread_id"`
	InReplyToEmailID string      `json:"in_reply_to_email_id"`
	Snippet          string      `json:"snippet"`
}

// MessageList is a page of received messages, newest first.
type MessageList struct {
	Object     string           `json:"object"`
	Data       []MessageSummary `json:"data"`
	HasMore    bool             `json:"has_more"`
	NextBefore string           `json:"next_before"`
}

// ListMessagesParams filters an inbox's messages. Zero values are left out.
type ListMessagesParams struct {
	Limit    int
	Before   string
	ThreadID string
}

// MessageAttachment describes one attachment of a received message.
type MessageAttachment struct {
	Index       int    `json:"index"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// Message is a received message.
type Message struct {
	ID      string      `json:"id"`
	InboxID string      `json:"inbox_id"`
	From    string      `json:"from"`
	To      AddressList `json:"to"`
	Cc      AddressList `json:"cc"`
	Subject string      `json:"subject"`
	Text    string      `json:"text"`
	HTML    string      `json:"html"`
	// Headers holds message-id, in-reply-to, references and date.
	Headers          map[string]string   `json:"headers"`
	Attachments      []MessageAttachment `json:"attachments"`
	ThreadID         string              `json:"thread_id"`
	InReplyToEmailID string              `json:"in_reply_to_email_id"`
	ReceivedAt       time.Time           `json:"received_at"`
}

// ReplyRequest is the body of POST /messages/{id}/reply. Set Text, HTML or
// both.
type ReplyRequest struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`
}

// InboxesService manages inboxes and lists what they received.
type InboxesService struct{ c *Client }

// Create creates an inbox for an address on one of the project's domains.
func (s *InboxesService) Create(ctx context.Context, address string) (*Inbox, error) {
	var out Inbox
	if err := s.c.do(ctx, "POST", "/inboxes", nil, map[string]string{"address": address}, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every inbox of the project.
func (s *InboxesService) List(ctx context.Context) (*InboxList, error) {
	var out InboxList
	if err := s.c.do(ctx, "GET", "/inboxes", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes an inbox.
func (s *InboxesService) Delete(ctx context.Context, id string) (*Deleted, error) {
	var out Deleted
	if err := s.c.do(ctx, "DELETE", "/inboxes/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMessages returns messages received by an inbox, newest first.
func (s *InboxesService) ListMessages(ctx context.Context, inboxID string, params *ListMessagesParams) (*MessageList, error) {
	if params == nil {
		params = &ListMessagesParams{}
	}
	q := query("limit", itoa(params.Limit), "before", params.Before, "thread_id", params.ThreadID)
	var out MessageList
	if err := s.c.do(ctx, "GET", "/inboxes/"+seg(inboxID)+"/messages", q, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// MessagesService reads and answers received messages.
type MessagesService struct{ c *Client }

// Get returns one received message with its text, HTML, headers and
// attachment list.
func (s *MessagesService) Get(ctx context.Context, id string) (*Message, error) {
	var out Message
	if err := s.c.do(ctx, "GET", "/messages/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAttachment returns the raw bytes of one attachment (index from
// Message.Attachments[].Index).
func (s *MessagesService) GetAttachment(ctx context.Context, id string, index int) ([]byte, error) {
	var out []byte
	if err := s.c.do(ctx, "GET", "/messages/"+seg(id)+"/attachments/"+strconv.Itoa(index), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// Reply answers a message from its inbox address, threaded with
// In-Reply-To and References, and returns the id of the queued email.
func (s *MessagesService) Reply(ctx context.Context, id string, req *ReplyRequest) (*SendResponse, error) {
	var out SendResponse
	if err := s.c.do(ctx, "POST", "/messages/"+seg(id)+"/reply", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}
