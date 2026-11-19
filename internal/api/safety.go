package api

// Agent safety and compliance (v0.6, lane B). This file holds the hooks the
// send paths call; the endpoints live in approvals.go, suppressions.go,
// unsubscribe.go, pauses.go and check.go.
//
//	planSafety / rawSafety  per email: stream, Feedback-ID and unsubscribe
//	                        headers, and the key's require_approval rules
//	checkPause              in authorizeSend: a tripped circuit breaker
//	reserveSafety           in queue, before storing: pauses again and the
//	                        project's per-recipient cap, under a lock
//	settleSafety            in queue, after storing: contacts and breaker
//	                        counts, or releasing what reserveSafety took

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/openmailwire/openmailwire/internal/apikey"
	"github.com/openmailwire/openmailwire/internal/mailparse"
	"github.com/openmailwire/openmailwire/internal/store"
)

// safetyState is the Server's in-memory state for lane B. Its zero value is
// ready to use.
type safetyState struct {
	once  sync.Once
	unsub *ipLimiter // POST and GET /u/{token}, per client IP
}

func (st *safetyState) init() {
	st.once.Do(func() { st.unsub = newIPLimiter(unsubRate, unsubBurst) })
}

func (s *Server) safetyRoutes(mux *http.ServeMux) {
	s.safety.init()
	mux.Handle("POST /emails/check", s.withProject(s.checkEmail))
	mux.Handle("GET /emails/held", s.withProject(s.listHeld))
	mux.Handle("POST /emails/{id}/approve", s.withProject(s.approveEmail))
	mux.Handle("POST /emails/{id}/reject", s.withProject(s.rejectEmail))
	mux.Handle("GET /suppressions", s.withProject(s.listSuppressions))
	mux.Handle("POST /suppressions", s.withFullAccess(s.addSuppression))
	mux.Handle("GET /suppressions/export", s.withProject(s.exportSuppressions))
	mux.Handle("DELETE /suppressions/{address}", s.withFullAccess(s.deleteSuppression))
	mux.Handle("GET /pauses", s.withProject(s.listPauses))
	mux.Handle("POST /pauses/{id}/resume", s.withFullAccess(s.resumePause))
	mux.Handle("GET /project", s.withProject(s.getProject))
	mux.Handle("PATCH /project", s.withFullAccess(s.updateProject))
	mux.HandleFunc("GET /u/{token}", s.unsubscribePage)
	mux.HandleFunc("POST /u/{token}", s.unsubscribeNow)
}

// audit records a change made with the request's key.
//
// TODO(merge): lane A adds the append-only audit log (GET /audit-log).
// Route this through its helper, so approvals, rejections, resumed pauses,
// suppression changes and project settings are recorded there with the
// acting key. Until then they are logged (never with full addresses).
func (s *Server) audit(r *http.Request, action, target string, detail map[string]any) {
	args := []any{"action", action, "target", target, "project", projectID(r), "key", apiKeyOf(r).ID}
	for k, v := range detail {
		args = append(args, k, v)
	}
	s.Log.Info("audit", args...)
}

// ---- streams and headers ----

// safetyPlan is what lane B adds to one email.
type safetyPlan struct {
	stream  string            // store.StreamTransactional or store.StreamBroadcast
	headers map[string]string // Feedback-ID, and List-Unsubscribe(-Post) for broadcast
	hold    *store.Hold       // non-nil when the email waits for approval
}

func parseStream(v string) (string, *policyError) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", store.StreamTransactional:
		return store.StreamTransactional, nil
	case store.StreamBroadcast:
		return store.StreamBroadcast, nil
	}
	return "", invalid(`stream must be "transactional" (the default: receipts, codes, replies a recipient expects) or "broadcast" (newsletters and other bulk mail, which get a one-click unsubscribe link)`)
}

const (
	hdrFeedbackID   = "Feedback-ID"
	hdrListUnsub    = "List-Unsubscribe"
	hdrListUnsubOne = "List-Unsubscribe-Post"
)

// safetyHeaders returns the headers OpenMailWire sets on an email of this
// stream: a Feedback-ID always (Gmail's feedback loop reports complaint
// rates by it), and the RFC 8058 one-click unsubscribe pair on broadcast
// mail, which goes to exactly one recipient.
func (s *Server) safetyHeaders(key store.APIKey, stream string, recipients []string) (map[string]string, *policyError) {
	h := map[string]string{hdrFeedbackID: feedbackID(stream, key)}
	if stream != store.StreamBroadcast {
		return h, nil
	}
	if len(recipients) != 1 {
		return nil, invalid(`a broadcast email goes to exactly one recipient, because its unsubscribe link belongs to that address: put one address in to (no cc or bcc), and send many with POST /emails/batch`)
	}
	if s.Cfg.PublicURL == "" {
		return nil, &policyError{Status: http.StatusUnprocessableEntity, Name: "unsubscribe_unavailable", Message: "broadcast emails carry a one-click unsubscribe link (RFC 8058) on this server's public address, and OMW_PUBLIC_URL is not set. Ask the operator to set OMW_PUBLIC_URL (e.g. https://mail.example.com); until then only transactional mail can be sent"}
	}
	h[hdrListUnsub] = "<" + s.Cfg.PublicURL + "/u/" + s.unsubToken(key.ProjectID, recipients[0]) + ">"
	h[hdrListUnsubOne] = "List-Unsubscribe=One-Click"
	return h, nil
}

// feedbackID is "<stream>:<api key id>:<project id>:omw". The last field
// is the sender id Gmail's Postmaster Tools groups reports by.
func feedbackID(stream string, key store.APIKey) string {
	return stream + ":" + key.ID + ":" + key.ProjectID + ":omw"
}

// planSafety completes a validated JSON send for lane B: its stream and
// headers, and a hold when the key's require_approval rules match. It
// stores nothing.
func (s *Server) planSafety(r *http.Request, p *sendPlan) *policyError {
	stream, pe := parseStream(p.req.Stream)
	if pe != nil {
		return pe
	}
	for k := range p.req.Headers {
		switch {
		case strings.EqualFold(k, hdrFeedbackID):
			return invalid("headers: Feedback-ID is set by OpenMailWire (%s); leave it out", feedbackID(stream, apiKeyOf(r)))
		case stream == store.StreamBroadcast && (strings.EqualFold(k, hdrListUnsub) || strings.EqualFold(k, hdrListUnsubOne)):
			return invalid("headers: %s is set by OpenMailWire on broadcast email (one-click unsubscribe); leave it out", k)
		}
	}
	key := apiKeyOf(r)
	headers, pe := s.safetyHeaders(key, stream, p.recipients)
	if pe != nil {
		return pe
	}
	hold, pe := s.holdFor(r.Context(), key, p.recipients, func() (bool, bool) {
		_, links := linkHosts(p.req.Text, p.req.HTML)
		return len(p.attachments) > 0, links
	})
	if pe != nil {
		return pe
	}
	p.safety = &safetyPlan{stream: stream, headers: headers, hold: hold}
	return nil
}

// holdFor applies the key's require_approval rules. body reports whether
// the email has attachments and links; it is called only when a rule needs
// it.
func (s *Server) holdFor(ctx context.Context, key store.APIKey, recipients []string, body func() (attachments, links bool)) (*store.Hold, *policyError) {
	rules := key.Policy.RequireApproval
	if rules == nil {
		return nil, nil
	}
	f := apikey.Facts{Recipients: len(recipients)}
	if rules.HasAttachments || rules.HasLinks {
		f.HasAttachments, f.HasLinks = body()
	}
	f.FirstContact = func() (bool, error) {
		fresh, err := s.Store.NewContacts(ctx, key.ProjectID, recipients)
		return len(fresh) > 0, err
	}
	reasons, err := key.Policy.HoldReasons(f)
	if err != nil {
		return nil, s.internalErr("hold rules", err)
	}
	if len(reasons) == 0 {
		return nil, nil
	}
	return &store.Hold{Reasons: reasons}, nil
}

// messageHeaders are the caller's extra headers plus the ones planSafety
// added.
func (p *sendPlan) messageHeaders() map[string]string {
	if p.safety == nil || len(p.safety.headers) == 0 {
		return p.req.Headers
	}
	h := make(map[string]string, len(p.req.Headers)+len(p.safety.headers))
	for k, v := range p.req.Headers {
		h[k] = v
	}
	for k, v := range p.safety.headers {
		h[k] = v
	}
	return h
}

func (p *sendPlan) stream() string {
	if p.safety == nil {
		return store.StreamTransactional
	}
	return p.safety.stream
}

func (p *sendPlan) hold() *store.Hold {
	if p.safety == nil {
		return nil
	}
	return p.safety.hold
}

// streamHeader lets a raw message (SMTP submission) pick its stream; it is
// removed before sending.
const streamHeader = "X-OMW-Stream"

// rawSafety is planSafety for a raw message. It edits m: the stream header
// and any Feedback-ID go, and on broadcast mail any List-Unsubscribe pair
// is replaced by OpenMailWire's own.
func (s *Server) rawSafety(r *http.Request, stream string, m *rawMessage, data []byte, recipients []string) (*safetyPlan, *policyError) {
	for i, f := range m.fields {
		if strings.EqualFold(fieldName(f), streamHeader) {
			if stream == "" {
				stream = strings.TrimSpace(fieldValue(f))
			}
			m.dropFields[i] = true
		}
	}
	stream, pe := parseStream(stream)
	if pe != nil {
		return nil, pe
	}
	key := apiKeyOf(r)
	headers, pe := s.safetyHeaders(key, stream, recipients)
	if pe != nil {
		return nil, pe
	}
	for i, f := range m.fields {
		name := fieldName(f)
		if strings.EqualFold(name, hdrFeedbackID) ||
			(stream == store.StreamBroadcast && (strings.EqualFold(name, hdrListUnsub) || strings.EqualFold(name, hdrListUnsubOne))) {
			m.dropFields[i] = true
		}
	}
	for _, k := range []string{hdrFeedbackID, hdrListUnsub, hdrListUnsubOne} {
		if v, ok := headers[k]; ok {
			m.fields = append(m.fields, []byte(k+": "+v+"\r\n"))
		}
	}
	hold, pe := s.holdFor(r.Context(), key, recipients, func() (bool, bool) {
		p := mailparse.Parse(data)
		_, links := linkHosts(p.Text, p.HTML)
		return len(p.Attachments) > 0, links
	})
	if pe != nil {
		return nil, pe
	}
	return &safetyPlan{stream: stream, headers: headers, hold: hold}, nil
}

// ---- refusals ----

// checkPause refuses a send from a paused key, sending domain or project.
// authorizeSend calls it; reserveSafety checks again just before storing.
func (s *Server) checkPause(ctx context.Context, key store.APIKey, domainIDs, domainNames []string) *policyError {
	p, err := s.Store.ActivePause(ctx, key.ProjectID, key.ID, domainIDs, domainNames)
	if err != nil {
		return s.internalErr("active pause", err)
	}
	if p == nil {
		return nil
	}
	return pausedRefusal(*p)
}

func pausedRefusal(p store.Pause) *policyError {
	scope := map[string]string{
		"api_key": fmt.Sprintf("the API key %q (%s)", p.ScopeName, p.ScopeID),
		"domain":  "the sending domain " + p.ScopeName,
		"project": "this project",
	}[p.Scope]
	what, why := "bounce", "why recipients bounce (GET /emails, GET /suppressions) and remove bad addresses"
	n := p.Bounces
	if p.Reason == "complaint_rate" {
		what, why, n = "complaint", "why recipients report the mail as spam: check consent, content and frequency", p.Complaints
	}
	return &policyError{Status: http.StatusForbidden, Name: "sending_paused", Message: fmt.Sprintf(
		"sending is paused for %s: its %s rate over the last 24 hours is %s (%d of %d recipients), above the %s limit. Nothing is sent from it until a full_access key resumes it with POST /pauses/%s/resume, after finding out %s",
		scope, what, percent(p.Rate), n, p.Recipients, percent(p.Threshold), p.ID, why)}
}

func percent(f float64) string {
	s := fmt.Sprintf("%.2f", f*100)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

func recipientRefusal(ce *store.RecipientCapError) *policyError {
	pe := &policyError{Status: http.StatusTooManyRequests, Name: "recipient_rate_limited", RetryAfter: ce.RetryAfter}
	if ce.RetryAfter == 0 {
		pe.Message = fmt.Sprintf("this project sends at most %d emails per recipient per day (max_per_recipient_per_day), and this request alone has %d for %s; send fewer",
			ce.Limit, ce.Adding, ce.Address)
		return pe
	}
	pe.Message = fmt.Sprintf("%s was sent %d emails in the last 24 hours and this project allows at most %d per recipient per day (max_per_recipient_per_day), across all its API keys. Retry after %s (see the Retry-After header), or ask the project's owner to raise it with PATCH /project",
		ce.Address, ce.Used, ce.Limit, ce.RetryAfter)
	return pe
}

// ---- around storing ----

// reserveSafety runs in queue just before the emails are stored: it
// refuses a paused scope, and claims the per-recipient cap under the
// project's lock. reserved says whether settleSafety must release claims
// for emails that end up not stored.
func (s *Server) reserveSafety(r *http.Request, es []store.NewEmail) (reserved bool, pe *policyError) {
	ctx, key := r.Context(), apiKeyOf(r)
	var domainIDs []string
	seen := map[string]bool{}
	for _, e := range es {
		if e.DomainID != "" && !seen[e.DomainID] {
			seen[e.DomainID] = true
			domainIDs = append(domainIDs, e.DomainID)
		}
	}
	if pe := s.checkPause(ctx, key, domainIDs, nil); pe != nil {
		return false, pe
	}
	limit, err := s.Store.MaxPerRecipientPerDay(ctx, key.ProjectID)
	if err != nil {
		return false, s.internalErr("project settings", err)
	}
	if limit == 0 {
		return false, nil
	}
	rs := make([]store.Reservation, len(es))
	for i, e := range es {
		rs[i] = store.Reservation{EmailID: e.ID, Stream: e.Stream, Recipients: e.Recipients}
	}
	ce, err := s.Store.ReserveRecipients(ctx, key.ProjectID, limit, rs, true)
	if err != nil {
		return false, s.internalErr("reserve recipients", err)
	}
	if ce != nil {
		return false, recipientRefusal(ce)
	}
	return true, nil
}

// settleSafety runs in queue after InsertEmails: stored emails' recipients
// become contacts and count toward the circuit breakers; claims of emails
// not stored (an error, or an idempotent replay) are released. Failures are
// logged: the send itself has succeeded or failed already.
func (s *Server) settleSafety(r *http.Request, es []store.NewEmail, out []store.Inserted, insertErr error, reserved bool) {
	ctx := context.WithoutCancel(r.Context())
	var stored, dropped []string
	for i, e := range es {
		if insertErr == nil && i < len(out) && !out[i].Replayed {
			stored = append(stored, e.ID)
		} else {
			dropped = append(dropped, e.ID)
		}
	}
	if reserved && len(dropped) > 0 {
		if err := s.Store.ReleaseRecipients(ctx, dropped); err != nil {
			s.Log.Error("release recipient sends", "err", err)
		}
	}
	if err := s.Store.RecordQueued(ctx, stored); err != nil {
		s.Log.Error("record queued", "err", err)
	}
}

// ---- answers ----

// sentBody answers a send: {"id"}, plus "held" and "hold_reasons" when the
// email waits for approval.
func sentBody(o store.Inserted, hold *store.Hold) map[string]any {
	b := map[string]any{"id": o.ID}
	if hold != nil && !o.Replayed {
		b["held"] = true
		b["hold_reasons"] = hold.Reasons
	}
	return b
}

// batchSent answers a batch like batchResponse, marking held emails.
func batchSent(out []store.Inserted, es []store.NewEmail) map[string]any {
	data := make([]map[string]any, len(out))
	for i, o := range out {
		data[i] = sentBody(o, es[i].Hold)
	}
	return map[string]any{"data": data}
}

// canApprove reports whether the key may approve and reject held emails.
func canApprove(k store.APIKey) bool { return k.FullAccess() || k.Policy.CanApprove }
