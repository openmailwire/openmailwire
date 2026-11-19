package openmailwire

import (
	"context"
	"encoding/json"
	"time"
)

// ------------------------------------------------------------------ suppressions

// Suppression streams.
const (
	// SuppressAll stops every email to the address.
	SuppressAll = "all"
	// SuppressBroadcast stops only broadcast email, as a one-click
	// unsubscribe does.
	SuppressBroadcast = "broadcast"
)

// Suppression is an address OpenMailWire does not send to.
type Suppression struct {
	Address string `json:"address"`
	// Stream is SuppressAll or SuppressBroadcast.
	Stream string `json:"stream"`
	// Reason is hard_bounce, complaint, unsubscribe, manual, or a name given
	// to Create.
	Reason    string    `json:"reason"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// SuppressionList is a page of suppressions, newest first.
type SuppressionList struct {
	Object     string        `json:"object"`
	Data       []Suppression `json:"data"`
	HasMore    bool          `json:"has_more"`
	NextBefore string        `json:"next_before"`
}

// SuppressionFilter selects suppressions. Zero values are left out.
type SuppressionFilter struct {
	// Reason keeps only this reason, e.g. "hard_bounce".
	Reason string
	// Stream keeps only SuppressAll or only SuppressBroadcast rows.
	Stream string
	// Address keeps only this address.
	Address string
	// Domain keeps only addresses at this domain.
	Domain string
}

func (f *SuppressionFilter) query() []string {
	return []string{"reason", f.Reason, "stream", f.Stream, "address", f.Address, "domain", f.Domain}
}

// ListSuppressionsParams filters and pages GET /suppressions. Zero values are
// left out.
type ListSuppressionsParams struct {
	Limit int
	// Before is the NextBefore of the previous page (an opaque cursor).
	Before string
	SuppressionFilter
}

// CreateSuppressionRequest is the body of POST /suppressions.
type CreateSuppressionRequest struct {
	Address string `json:"address"`
	// Reason is a short name such as "customer_request"; default "manual".
	Reason string `json:"reason,omitempty"`
	// Detail is free text kept with it, at most 300 characters.
	Detail string `json:"detail,omitempty"`
	// Stream is SuppressAll (the default) or SuppressBroadcast.
	Stream string `json:"stream,omitempty"`
}

// SuppressionCreated is the answer of Suppressions.Create.
type SuppressionCreated struct {
	Object string `json:"object"`
	Suppression
	// Created is false when the address was already suppressed for that
	// stream; the existing row is returned.
	Created bool `json:"created"`
}

// SuppressionDeleted is the answer of Suppressions.Delete.
type SuppressionDeleted struct {
	Object  string        `json:"object"`
	Address string        `json:"address"`
	Deleted bool          `json:"deleted"`
	Removed []Suppression `json:"removed"`
}

// SuppressionsService manages the addresses OpenMailWire does not send to.
type SuppressionsService struct{ c *Client }

// List returns suppressed addresses, newest first. Pass NextBefore as Before
// for the next page. params may be nil.
func (s *SuppressionsService) List(ctx context.Context, params *ListSuppressionsParams) (*SuppressionList, error) {
	if params == nil {
		params = &ListSuppressionsParams{}
	}
	q := query(append([]string{"limit", itoa(params.Limit), "before", params.Before}, params.SuppressionFilter.query()...)...)
	var out SuppressionList
	if err := s.c.do(ctx, "GET", "/suppressions", q, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Create stops sending to an address. It needs a full-access key.
func (s *SuppressionsService) Create(ctx context.Context, req *CreateSuppressionRequest) (*SuppressionCreated, error) {
	var out SuppressionCreated
	if err := s.c.do(ctx, "POST", "/suppressions", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete sends to an address again: it removes the address's suppressions,
// or only those of stream when it is not empty. It needs a full-access key.
func (s *SuppressionsService) Delete(ctx context.Context, address, stream string) (*SuppressionDeleted, error) {
	var out SuppressionDeleted
	if err := s.c.do(ctx, "DELETE", "/suppressions/"+seg(address), query("stream", stream), nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Export returns every matching suppression as CSV text. filter may be nil.
func (s *SuppressionsService) Export(ctx context.Context, filter *SuppressionFilter) (string, error) {
	if filter == nil {
		filter = &SuppressionFilter{}
	}
	var out string
	if err := s.c.do(ctx, "GET", "/suppressions/export", query(filter.query()...), nil, &out, nil); err != nil {
		return "", err
	}
	return out, nil
}

// ------------------------------------------------------------------ pauses

// Pause is a circuit breaker that stopped sending from an API key, a domain
// or the project. While Active, sends from its scope are refused with
// 403 sending_paused.
type Pause struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	// Scope is "api_key", "domain" or "project".
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	// ScopeName is the key's or project's name, or the domain.
	ScopeName string `json:"scope_name,omitempty"`
	// Reason is "bounce_rate" or "complaint_rate".
	Reason string `json:"reason"`
	// Rate is the rate when it tripped, as a fraction, over 24 hours.
	Rate       float64    `json:"rate"`
	Threshold  float64    `json:"threshold"`
	Recipients int        `json:"recipients,omitempty"`
	Bounces    int        `json:"bounces,omitempty"`
	Complaints int        `json:"complaints,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResumedAt  *time.Time `json:"resumed_at,omitempty"`
	// ResumedBy is the key that resumed it.
	ResumedBy string `json:"resumed_by,omitempty"`
	Active    bool   `json:"active"`
}

// PauseList is the answer of GET /pauses.
type PauseList struct {
	Object string  `json:"object"`
	Data   []Pause `json:"data"`
}

// ListPausesParams filters GET /pauses. Zero values are left out.
type ListPausesParams struct {
	// Active lists only pauses still in force.
	Active bool
	Limit  int
}

// PausesService lists and resumes circuit-breaker pauses.
type PausesService struct{ c *Client }

// List returns the pauses, newest first. params may be nil.
func (s *PausesService) List(ctx context.Context, params *ListPausesParams) (*PauseList, error) {
	if params == nil {
		params = &ListPausesParams{}
	}
	active := ""
	if params.Active {
		active = "true"
	}
	var out PauseList
	if err := s.c.do(ctx, "GET", "/pauses", query("active", active, "limit", itoa(params.Limit)), nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Resume lets the paused key, domain or project send again. It needs a
// full-access key.
func (s *PausesService) Resume(ctx context.Context, id string) (*Pause, error) {
	var out Pause
	if err := s.c.do(ctx, "POST", "/pauses/"+seg(id)+"/resume", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ project

// CircuitBreaker holds when sending pauses by itself. In an update, nil
// fields keep their value.
type CircuitBreaker struct {
	Enabled *bool `json:"enabled,omitempty"`
	// BounceRate pauses a scope whose bounces over 24 hours are above this
	// fraction of its recipients (default 0.05).
	BounceRate *float64 `json:"bounce_rate,omitempty"`
	// ComplaintRate pauses above this fraction of complaints (default 0.001).
	ComplaintRate *float64 `json:"complaint_rate,omitempty"`
	// MinRecipients: rates count only once a scope queued this many
	// recipients in 24 hours (default 50).
	MinRecipients *int `json:"min_recipients,omitempty"`
}

// Project holds the project's sending settings.
type Project struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	// MaxPerRecipientPerDay caps the emails one recipient gets in any 24
	// hours, across every key; nil is no cap.
	MaxPerRecipientPerDay *int           `json:"max_per_recipient_per_day"`
	CircuitBreaker        CircuitBreaker `json:"circuit_breaker"`
	UpdatedAt             *time.Time     `json:"updated_at,omitempty"`
}

// UpdateProjectRequest is the body of PATCH /project. Nil fields keep their
// value.
type UpdateProjectRequest struct {
	// MaxPerRecipientPerDay sets the per-recipient cap, 1 to 10000.
	MaxPerRecipientPerDay *int
	// ClearMaxPerRecipientPerDay turns the cap off (null).
	ClearMaxPerRecipientPerDay bool
	CircuitBreaker             *CircuitBreaker
}

// MarshalJSON sends only the fields to change, and null to turn the cap off.
func (r UpdateProjectRequest) MarshalJSON() ([]byte, error) {
	body := map[string]any{}
	if r.ClearMaxPerRecipientPerDay {
		body["max_per_recipient_per_day"] = nil
	} else if r.MaxPerRecipientPerDay != nil {
		body["max_per_recipient_per_day"] = *r.MaxPerRecipientPerDay
	}
	if r.CircuitBreaker != nil {
		body["circuit_breaker"] = r.CircuitBreaker
	}
	return json.Marshal(body)
}

// ProjectService reads and changes the project's sending settings.
type ProjectService struct{ c *Client }

// Get returns the project's per-recipient cap and circuit-breaker settings.
func (s *ProjectService) Get(ctx context.Context) (*Project, error) {
	var out Project
	if err := s.c.do(ctx, "GET", "/project", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Update changes them. It needs a full-access key.
func (s *ProjectService) Update(ctx context.Context, req *UpdateProjectRequest) (*Project, error) {
	if req == nil {
		req = &UpdateProjectRequest{}
	}
	var out Project
	if err := s.c.do(ctx, "PATCH", "/project", nil, req, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ audit log

// AuditEntry is one entry of the project's hash-chained audit log. Hash is
// SHA-256 of PrevHash's bytes followed by Entry.
type AuditEntry struct {
	Object    string    `json:"object"`
	ID        int64     `json:"id"`
	ProjectID string    `json:"project_id"`
	CreatedAt time.Time `json:"created_at"`
	// Actor is "api_key" (ActorKeyID), "cli" or "system".
	Actor      string `json:"actor"`
	ActorKeyID string `json:"actor_key_id"`
	// Action is "<thing>.<verb>", e.g. "api_key.created".
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Data       json.RawMessage `json:"data"`
	// Entry is the exact JSON text that was hashed.
	Entry    string `json:"entry"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// AuditEntryList is a page of audit log entries, newest first.
type AuditEntryList struct {
	Object     string       `json:"object"`
	Data       []AuditEntry `json:"data"`
	HasMore    bool         `json:"has_more"`
	NextBefore string       `json:"next_before"`
}

// ListAuditLogParams filters and pages GET /audit-log. Zero values are left
// out.
type ListAuditLogParams struct {
	Limit int
	// Before is an entry id: only older entries (NextBefore of the previous
	// page).
	Before string
	// Action keeps only this action, e.g. "api_key.created".
	Action string
}

// AuditVerification is the answer of GET /audit-log/verify.
type AuditVerification struct {
	Object  string `json:"object"`
	Valid   bool   `json:"valid"`
	Entries int64  `json:"entries"`
	// HeadHash is the last entry's hash. Keep it: if a later check's chain
	// no longer contains it, entries were removed from the end.
	HeadHash       string `json:"head_hash"`
	FirstInvalidID *int64 `json:"first_invalid_id"`
	Problem        string `json:"problem,omitempty"`
}

// AuditLogService reads the project's audit log. It needs a full-access key.
type AuditLogService struct{ c *Client }

// List returns audit log entries, newest first. params may be nil.
func (s *AuditLogService) List(ctx context.Context, params *ListAuditLogParams) (*AuditEntryList, error) {
	if params == nil {
		params = &ListAuditLogParams{}
	}
	q := query("limit", itoa(params.Limit), "before", params.Before, "action", params.Action)
	var out AuditEntryList
	if err := s.c.do(ctx, "GET", "/audit-log", q, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// Verify checks the whole hash chain on the server.
func (s *AuditLogService) Verify(ctx context.Context) (*AuditVerification, error) {
	var out AuditVerification
	if err := s.c.do(ctx, "GET", "/audit-log/verify", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	return &out, nil
}

// ------------------------------------------------------------------ insights

// Insight windows.
const (
	Window24h = "24h"
	Window7d  = "7d"
	Window30d = "30d"
)

// InsightStats are delivery counts and rates. The rates are nil without
// attempts.
type InsightStats struct {
	Attempts                   int      `json:"attempts"`
	Accepted                   int      `json:"accepted"`
	Deferred                   int      `json:"deferred"`
	Bounced                    int      `json:"bounced"`
	Unknown                    int      `json:"unknown"`
	AcceptanceRate             *float64 `json:"acceptance_rate"`
	DeferralRate               *float64 `json:"deferral_rate"`
	BounceRate                 *float64 `json:"bounce_rate"`
	TimeToAcceptanceP50Seconds *float64 `json:"time_to_acceptance_p50_seconds"`
	TimeToAcceptanceP95Seconds *float64 `json:"time_to_acceptance_p95_seconds"`
}

// InsightGroup is the stats of one mailbox provider or one sending IP.
type InsightGroup struct {
	InsightStats
	// Provider is "google", "microsoft", "yahoo", … or "other", "unknown".
	Provider  string `json:"provider,omitempty"`
	SendingIP string `json:"sending_ip,omitempty"`
}

// InsightWindow is the stats of one period.
type InsightWindow struct {
	// Window is Window24h, Window7d or Window30d.
	Window      string         `json:"window"`
	Total       InsightStats   `json:"total"`
	ByProvider  []InsightGroup `json:"by_provider"`
	BySendingIP []InsightGroup `json:"by_sending_ip"`
}

// Insights is the answer of GET /insights.
type Insights struct {
	Object           string    `json:"object"`
	GeneratedAt      time.Time `json:"generated_at"`
	WindowBasis      string    `json:"window_basis,omitempty"`
	ProviderGrouping string    `json:"provider_grouping,omitempty"`
	EmailsConsidered int       `json:"emails_considered"`
	// Truncated: more than 100,000 emails were sent in 30 days, and the
	// longer windows cover only the newest 100,000.
	Truncated bool            `json:"truncated"`
	Windows   []InsightWindow `json:"windows"`
}

// InsightsParams changes Insights.Get. Zero values change nothing.
type InsightsParams struct {
	// Window keeps only this window (Window24h, Window7d or Window30d) in
	// Windows. The server always computes all three; the SDK filters.
	Window string
}

// InsightsService reads delivery insights. It needs a full-access key.
type InsightsService struct{ c *Client }

// Get returns acceptance, deferral and bounce rates and time to acceptance,
// per mailbox provider and sending IP, for the last 24 hours, 7 days and 30
// days. params may be nil.
func (s *InsightsService) Get(ctx context.Context, params *InsightsParams) (*Insights, error) {
	var out Insights
	if err := s.c.do(ctx, "GET", "/insights", nil, nil, &out, nil); err != nil {
		return nil, err
	}
	if params != nil && params.Window != "" {
		kept := []InsightWindow{}
		for _, w := range out.Windows {
			if w.Window == params.Window {
				kept = append(kept, w)
			}
		}
		out.Windows = kept
	}
	return &out, nil
}
