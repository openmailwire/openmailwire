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
	// Record is DKIM, SPF, MX, DMARC, INBOUND, DKIM_NEXT, DKIM_PREVIOUS or
	// DKIM_ED25519.
	Record   string `json:"record"`
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
	// ClaimExpiresAt is when an unverified domain is removed (7 days after
	// it was added); nil once verified.
	ClaimExpiresAt *time.Time `json:"claim_expires_at"`
	// ReportAddresses says where to send the domain's DMARC aggregate and
	// TLS-RPT reports.
	ReportAddresses *ReportAddresses `json:"report_addresses,omitempty"`
}

// ReportAddresses are where a domain's aggregate reports go: put DMARCRUA
// in the DMARC record's rua= and publish TLSRPTRecord.
type ReportAddresses struct {
	DMARCRUA           string `json:"dmarc_rua,omitempty"`
	TLSRPTRUA          string `json:"tls_rpt_rua,omitempty"`
	DMARCRecordExample string `json:"dmarc_record_example,omitempty"`
	TLSRPTRecord       *struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"tls_rpt_record,omitempty"`
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

// DKIMKey is one of a domain's DKIM keys.
type DKIMKey struct {
	Selector string `json:"selector"`
	// Algorithm is "rsa" or "ed25519".
	Algorithm string `json:"algorithm"`
	// State is "active" (signs new mail), "pending" (waits for its DNS
	// record) or "retiring" (the previous key, published until RetireAt).
	State     string     `json:"state"`
	Signs     bool       `json:"signs"`
	Record    DNSRecord  `json:"record"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	RetireAt  *time.Time `json:"retire_at,omitempty"`
}

// DKIMState is the answer of the DKIM calls.
type DKIMState struct {
	// Object is "dkim", "dkim_rotation" or "dkim_ed25519".
	Object string `json:"object"`
	// Status is, for a rotation, "pending" or "rotated"; for Ed25519,
	// "pending", "active" or "disabled".
	Status string `json:"status,omitempty"`
	// Message says what happened and the next step.
	Message string    `json:"message,omitempty"`
	Keys    []DKIMKey `json:"keys"`
	// AuthIssues are authentication problems receivers reported for this
	// sending domain.
	AuthIssues []map[string]any `json:"auth_issues,omitempty"`
}

// DKIM returns the domain's DKIM keys and their records.
func (s *DomainsService) DKIM(ctx context.Context, id string) (*DKIMState, error) {
	var out DKIMState
	if err := s.c.do(ctx, "GET", "/domains/"+seg(id)+"/dkim", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// RotateDKIM rotates the DKIM key in two steps: the first call creates a key
// to publish (Status "pending"), a later call switches signing to it once
// its record is in DNS ("rotated"). The API answers 409
// dkim_record_not_found while the record is missing.
func (s *DomainsService) RotateDKIM(ctx context.Context, id string) (*DKIMState, error) {
	var out DKIMState
	if err := s.c.do(ctx, "POST", "/domains/"+seg(id)+"/dkim/rotate", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetEd25519 turns the extra Ed25519 DKIM signature on or off. Turning it on
// creates a key to publish first; call again once its record is in DNS.
func (s *DomainsService) SetEd25519(ctx context.Context, id string, enabled bool) (*DKIMState, error) {
	var out DKIMState
	body := map[string]bool{"enabled": enabled}
	if err := s.c.do(ctx, "POST", "/domains/"+seg(id)+"/dkim/ed25519", nil, body, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// DomainReportsParams filters GET /domains/{id}/reports. Zero values are left
// out.
type DomainReportsParams struct {
	// Days is 1 to 180; the server's default is 30.
	Days int
}

// DomainReports summarises the DMARC aggregate and TLS-RPT reports received
// for a domain.
type DomainReports struct {
	Object    string         `json:"object"`
	DomainID  string         `json:"domain_id"`
	Domain    string         `json:"domain"`
	Days      int            `json:"days"`
	Addresses map[string]any `json:"addresses,omitempty"`
	// DMARC has messages, pass, fail, pass_rate, by_organization, by_source
	// and unknown_senders.
	DMARC map[string]any `json:"dmarc"`
	// TLS has successful_sessions, failed_sessions, by_organization and
	// failures.
	TLS map[string]any `json:"tls"`
	// Reports are the 50 most recent reports.
	Reports []map[string]any `json:"reports"`
}

// Reports returns the DMARC aggregate and TLS-RPT reports received for the
// domain. params may be nil.
func (s *DomainsService) Reports(ctx context.Context, id string, params *DomainReportsParams) (*DomainReports, error) {
	if params == nil {
		params = &DomainReportsParams{}
	}
	var out DomainReports
	if err := s.c.do(ctx, "GET", "/domains/"+seg(id)+"/reports", query("days", itoa(params.Days)), nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ webhooks

// CreateWebhookRequest is the body of POST /webhooks.
type CreateWebhookRequest struct {
	// Endpoint is an https URL.
	Endpoint string `json:"endpoint"`
	// Events to receive, e.g. EventEmailBounced. Empty means every type.
	Events []string `json:"events,omitempty"`
	// AsymmetricSigning also signs every request with Ed25519 ("v1a").
	// Verify those with the webhook's PublicKey, which holds no secret.
	AsymmetricSigning bool `json:"asymmetric_signing,omitempty"`
}

// RotateWebhookSecretRequest is the optional body of
// POST /webhooks/{id}/rotate-secret.
type RotateWebhookSecretRequest struct {
	// OverlapSeconds is how long the previous secret (and Ed25519 key) keep
	// signing next to the new ones: 0 to 604800, default 86400 when nil.
	// Ptr(0) stops them at once.
	OverlapSeconds *int `json:"overlap_seconds,omitempty"`
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
	// SigningSecret ("whsec_…") is returned once, by Create and
	// RotateSecret.
	SigningSecret string `json:"signing_secret,omitempty"`
	// AsymmetricSigning says whether requests are also signed with Ed25519.
	AsymmetricSigning bool `json:"asymmetric_signing,omitempty"`
	// PublicKey ("whpk_…") verifies the "v1a" signatures.
	PublicKey string `json:"public_key,omitempty"`
	// PreviousPublicKey still signs during a rotation's overlap.
	PreviousPublicKey string `json:"previous_public_key,omitempty"`
	// PreviousSecretExpiresAt is when the previous secret stops signing.
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at,omitempty"`
	SecretRotatedAt         *time.Time `json:"secret_rotated_at,omitempty"`
	// ConsecutiveFailures counts failed attempts in a row; at 5 the
	// endpoint's circuit breaker opens until PausedUntil.
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
	PausedUntil         *time.Time `json:"paused_until,omitempty"`
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

// RotateSecret replaces the webhook's signing secret (and Ed25519 key). The
// previous ones keep signing for the overlap (default one day). The answer
// carries the new SigningSecret, shown once. req may be nil.
func (s *WebhooksService) RotateSecret(ctx context.Context, id string, req *RotateWebhookSecretRequest) (*Webhook, error) {
	if req == nil {
		req = &RotateWebhookSecretRequest{}
	}
	var out Webhook
	if err := s.c.do(ctx, "POST", "/webhooks/"+seg(id)+"/rotate-secret", nil, req, &out, nil); err != nil {
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
	// RequireApproval holds the key's emails for a person to approve when
	// any of its rules matches.
	RequireApproval *RequireApproval `json:"require_approval,omitempty"`
	// CanApprove lets the key approve and reject held emails of other keys.
	CanApprove bool `json:"can_approve,omitempty"`
	// Inboxes are the inbox address patterns a sending_access key may read
	// and reply from, e.g. "support@acme.com" or "*@agents.acme.com".
	Inboxes []string `json:"inboxes,omitempty"`
}

// RequireApproval holds a key's emails for approval when any rule matches.
type RequireApproval struct {
	Always bool `json:"always,omitempty"`
	// FirstContact: a recipient this project has never sent to.
	FirstContact   bool `json:"first_contact,omitempty"`
	HasAttachments bool `json:"has_attachments,omitempty"`
	HasLinks       bool `json:"has_links,omitempty"`
	// RecipientsOver: more than this many recipients (to, cc and bcc).
	RecipientsOver *int `json:"recipients_over,omitempty"`
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
	// AllowedIPs are the networks the key may be used from; nil allows
	// every address.
	AllowedIPs []string `json:"allowed_ips"`
	// ExpiresAt is when the key stops working; nil means never.
	ExpiresAt *time.Time `json:"expires_at"`
	Token     string     `json:"token,omitempty"`
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
	// AllowedIPs are addresses or CIDR prefixes ("10.0.0.0/8") the key may
	// be used from. Empty allows every address.
	AllowedIPs []string `json:"allowed_ips,omitempty"`
	// ExpiresAt is when the key stops working; nil means never.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// UpdateAPIKeyRequest is the body of PATCH /api-keys/{id}. Zero fields are
// left unchanged; a non-nil Policy replaces the key's policy, non-nil
// AllowedIPs its networks and a non-nil ExpiresAt its expiry.
type UpdateAPIKeyRequest struct {
	Name       string
	Policy     *APIKeyPolicy
	AllowedIPs []string
	ExpiresAt  *time.Time
	// ClearAllowedIPs removes the network restriction (allowed_ips: null).
	ClearAllowedIPs bool
	// ClearExpiresAt makes the key never expire (expires_at: null).
	ClearExpiresAt bool
}

// MarshalJSON sends only the fields to change, and null for the cleared ones.
func (r UpdateAPIKeyRequest) MarshalJSON() ([]byte, error) {
	body := map[string]any{}
	if r.Name != "" {
		body["name"] = r.Name
	}
	if r.Policy != nil {
		body["policy"] = r.Policy
	}
	if r.ClearAllowedIPs {
		body["allowed_ips"] = nil
	} else if r.AllowedIPs != nil {
		body["allowed_ips"] = r.AllowedIPs
	}
	if r.ClearExpiresAt {
		body["expires_at"] = nil
	} else if r.ExpiresAt != nil {
		body["expires_at"] = r.ExpiresAt
	}
	return json.Marshal(body)
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

// Update renames a key, or replaces its policy, allowed networks or expiry.
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
	Object    string    `json:"object,omitempty"`
	ID        string    `json:"id"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
	DomainID  string    `json:"domain_id,omitempty"`
	// HoldUnauthenticated holds mail whose sender is not authenticated from
	// sending_access keys until a full-access key releases it.
	HoldUnauthenticated bool `json:"hold_unauthenticated"`
}

// UpdateInboxRequest is the body of PATCH /inboxes/{id}.
type UpdateInboxRequest struct {
	HoldUnauthenticated bool `json:"hold_unauthenticated"`
}

// InboxOption changes how Inboxes.Create creates an inbox.
type InboxOption func(map[string]any)

// HoldUnauthenticated sets whether mail whose sender is not authenticated is
// held from sending_access keys (the server's default is true).
func HoldUnauthenticated(hold bool) InboxOption {
	return func(body map[string]any) { body["hold_unauthenticated"] = hold }
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

// Sender verdicts of a received message (Trust), from SPF, DKIM and DMARC.
const (
	TrustAuthenticated   = "authenticated"
	TrustUnauthenticated = "unauthenticated"
	TrustFailed          = "failed"
)

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
	// Trust is TrustAuthenticated, TrustUnauthenticated or TrustFailed.
	Trust string `json:"trust,omitempty"`
	// Held messages are visible only to full-access keys until released.
	Held bool `json:"held,omitempty"`
	// SuspiciousInstructions: the text matches a prompt-injection rule.
	SuspiciousInstructions bool `json:"suspicious_instructions,omitempty"`
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
	// Trust is TrustAuthenticated, TrustUnauthenticated or TrustFailed.
	Trust string `json:"trust,omitempty"`
	// AuthenticationResults has SPF, each DKIM signature, DMARC, the
	// verdict's reason and an Authentication-Results header.
	AuthenticationResults json.RawMessage `json:"authentication_results,omitempty"`
	// Held messages are visible only to full-access keys until released.
	Held       bool       `json:"held,omitempty"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	// SuspiciousInstructions: the text matches one of SuspiciousRules.
	SuspiciousInstructions bool     `json:"suspicious_instructions,omitempty"`
	SuspiciousRules        []string `json:"suspicious_rules,omitempty"`
	ReplyTo                []string `json:"reply_to,omitempty"`
	// MailFrom is the envelope sender.
	MailFrom string `json:"mail_from,omitempty"`
	// ReplyToMismatch: a Reply-To address is on another organizational
	// domain than From.
	ReplyToMismatch bool `json:"reply_to_mismatch,omitempty"`
	// MessageIDConflict: another message in the inbox has the same
	// Message-ID but different content.
	MessageIDConflict bool  `json:"message_id_conflict,omitempty"`
	Size              int64 `json:"size,omitempty"`
	TLS               bool  `json:"tls,omitempty"`
	BodyPurged        bool  `json:"body_purged,omitempty"`
}

// ReplyRequest is the body of POST /messages/{id}/reply. Set Text, HTML or
// both.
type ReplyRequest struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`
	// To are the recipients. Default: the first Reply-To address, or From.
	To []string `json:"to,omitempty"`
}

// ReplyResponse is the answer of Messages.Reply.
type ReplyResponse struct {
	SendResponse
	ThreadID string `json:"thread_id"`
	// To are the addresses the reply was sent to.
	To []string `json:"to,omitempty"`
	// Warnings are set when other Reply-To addresses were left out, or
	// Reply-To is on another domain than From.
	Warnings []string `json:"warnings,omitempty"`
}

// InboxesService manages inboxes and lists what they received.
type InboxesService struct{ c *Client }

// Create creates an inbox for an address on one of the project's domains.
func (s *InboxesService) Create(ctx context.Context, address string, opts ...InboxOption) (*Inbox, error) {
	body := map[string]any{"address": address}
	for _, opt := range opts {
		opt(body)
	}
	var out Inbox
	if err := s.c.do(ctx, "POST", "/inboxes", nil, body, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns one inbox.
func (s *InboxesService) Get(ctx context.Context, id string) (*Inbox, error) {
	var out Inbox
	if err := s.c.do(ctx, "GET", "/inboxes/"+seg(id), nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Update changes whether mail from unauthenticated senders is held. It needs
// a full-access key.
func (s *InboxesService) Update(ctx context.Context, id string, req *UpdateInboxRequest) (*Inbox, error) {
	if req == nil {
		req = &UpdateInboxRequest{}
	}
	var out Inbox
	if err := s.c.do(ctx, "PATCH", "/inboxes/"+seg(id), nil, req, &out, nil); err != nil {
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
// In-Reply-To and References, and returns the id of the queued email, its
// thread, its recipients and any warnings.
func (s *MessagesService) Reply(ctx context.Context, id string, req *ReplyRequest) (*ReplyResponse, error) {
	var out ReplyResponse
	if err := s.c.do(ctx, "POST", "/messages/"+seg(id)+"/reply", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Release shows a held message to every key with access to its inbox. It
// needs a full-access key, and answers the message without its content.
func (s *MessagesService) Release(ctx context.Context, id string) (*Message, error) {
	var out Message
	if err := s.c.do(ctx, "POST", "/messages/"+seg(id)+"/release", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}
