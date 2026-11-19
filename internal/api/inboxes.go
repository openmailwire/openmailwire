package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/mailauth"
	"github.com/openmailwire/openmailwire/internal/mailparse"
	"github.com/openmailwire/openmailwire/internal/store"
)

// inboxRoutes registers the inbox API. root returns the whole API handler,
// which replies use to send through POST /emails in process.
func (s *Server) inboxRoutes(mux *http.ServeMux, root func() http.Handler) {
	// sending_access keys reach only the inboxes their policy lists (see
	// inboxAllowed), and never held messages.
	mux.Handle("POST /inboxes", s.withFullAccess(s.createInbox))
	mux.Handle("GET /inboxes", s.withProject(s.listInboxes))
	mux.Handle("GET /inboxes/{id}", s.withProject(s.getInbox))
	mux.Handle("PATCH /inboxes/{id}", s.withFullAccess(s.updateInbox))
	mux.Handle("DELETE /inboxes/{id}", s.withFullAccess(s.deleteInbox))
	mux.Handle("GET /inboxes/{id}/messages", s.withProject(s.listMessages))
	mux.Handle("GET /messages/{id}", s.withProject(s.getMessage))
	mux.Handle("GET /messages/{id}/attachments/{index}", s.withProject(s.getAttachment))
	mux.Handle("POST /messages/{id}/reply", s.withProject(s.replyToMessage(root)))
	mux.Handle("POST /messages/{id}/release", s.withFullAccess(s.releaseMessage))
	mux.Handle("GET /emails/{id}/replies", s.withProject(s.emailReplies))
	mux.Handle("GET /domains/{id}/reports", s.withFullAccess(s.domainReports))

	// Inbound nodes. Both take only the worker token; keep them together so
	// a check on the token's kind can wrap them in one place.
	mux.Handle("POST /internal/inbound/check", s.withWorker(s.inboundCheck))
	mux.Handle("POST /internal/inbound", s.withWorker(s.inboundReceive))
}

// inboxAllowed reports whether the request's key may use the inbox at
// address: every inbox for a full-access key, only those its policy lists
// (policy.inboxes) for a sending_access key.
func inboxAllowed(r *http.Request, address string) bool {
	k := apiKeyOf(r)
	return k.FullAccess() || k.Policy.AllowsInbox(address)
}

// seesHeld reports whether the request's key sees held messages.
func seesHeld(r *http.Request) bool { return apiKeyOf(r).FullAccess() }

func inboxForbidden(w http.ResponseWriter, address string) {
	writeError(w, http.StatusForbidden, "insufficient_permission", fmt.Sprintf(
		"this API key has sending_access and its policy does not list the inbox %s; a full-access key can add it to the key's policy.inboxes with PATCH /api-keys/{id}", address))
}

type inboxResponse struct {
	Object              string    `json:"object"`
	ID                  string    `json:"id"`
	Address             string    `json:"address"`
	DomainID            string    `json:"domain_id"`
	HoldUnauthenticated bool      `json:"hold_unauthenticated"`
	CreatedAt           time.Time `json:"created_at"`
}

func inboxView(b store.Inbox) inboxResponse {
	return inboxResponse{Object: "inbox", ID: b.ID, Address: b.Address, DomainID: b.DomainID,
		HoldUnauthenticated: b.HoldUnauthenticated, CreatedAt: b.CreatedAt}
}

func (s *Server) createInbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Address             string `json:"address"`
		HoldUnauthenticated *bool  `json:"hold_unauthenticated"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	hold := req.HoldUnauthenticated == nil || *req.HoldUnauthenticated
	a, err := mail.ParseAddress(req.Address)
	if err != nil || a.Name != "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "address must be a plain email address, like support@example.com")
		return
	}
	addr := strings.ToLower(a.Address)
	local, dom, _ := strings.Cut(addr, "@")
	if !isASCII(addr) || local == "" || len(addr) > 254 || strings.Count(addr, "@") != 1 {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "address must be an ASCII email address of at most 254 characters")
		return
	}
	if addr == s.Cfg.FBLAddress || verpRE.MatchString(addr) {
		writeError(w, http.StatusConflict, "address_reserved", "this address is reserved for bounces and complaints; choose another")
		return
	}
	if local == dmarcReportLocal || local == tlsReportLocal {
		if known, err := s.Store.ReturnPathKnown(ctx, dom); err != nil {
			s.internal(w, "return path lookup", err)
			return
		} else if known {
			writeError(w, http.StatusConflict, "address_reserved", "this address receives a domain's DMARC or TLS reports; choose another")
			return
		}
	}
	d, err := s.Store.DomainByName(ctx, projectID(r), dom)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusForbidden, "domain_not_found",
			fmt.Sprintf("the domain %s is not added to this project; add it with POST /domains, then create the inbox", dom))
		return
	}
	if err != nil {
		s.internal(w, "domain lookup", err)
		return
	}
	if d.Status != "verified" && !s.Cfg.AllowUnverified {
		writeError(w, http.StatusForbidden, "domain_not_verified",
			fmt.Sprintf("the domain %s is not verified yet; publish its DNS records and call POST /domains/%s/verify", d.Name, d.ID))
		return
	}
	b, err := s.Store.CreateInbox(ctx, d.ProjectID, d.ID, addr, hold)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "inbox_exists", "an inbox with this address already exists")
		return
	}
	if err != nil {
		s.internal(w, "create inbox", err)
		return
	}
	writeJSON(w, http.StatusCreated, inboxView(b))
}

func (s *Server) listInboxes(w http.ResponseWriter, r *http.Request) {
	bs, err := s.Store.ListInboxes(r.Context(), projectID(r))
	if err != nil {
		s.internal(w, "list inboxes", err)
		return
	}
	out := make([]inboxResponse, 0, len(bs))
	for _, b := range bs {
		if inboxAllowed(r, b.Address) {
			out = append(out, inboxView(b))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) loadInbox(w http.ResponseWriter, r *http.Request) (store.Inbox, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "inbox not found")
		return store.Inbox{}, false
	}
	b, err := s.Store.GetInbox(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "inbox not found")
		return b, false
	}
	if err != nil {
		s.internal(w, "get inbox", err)
		return b, false
	}
	if !inboxAllowed(r, b.Address) {
		inboxForbidden(w, b.Address)
		return b, false
	}
	return b, true
}

func (s *Server) getInbox(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.loadInbox(w, r); ok {
		writeJSON(w, http.StatusOK, inboxView(b))
	}
}

// updateInbox changes an inbox's settings: {"hold_unauthenticated": bool}.
func (s *Server) updateInbox(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadInbox(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		HoldUnauthenticated *bool `json:"hold_unauthenticated"`
	}
	if err := decodeStrict(body, &req); err != nil || req.HoldUnauthenticated == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", `send {"hold_unauthenticated": true|false}`)
		return
	}
	b, err := s.Store.SetInboxHold(r.Context(), b.ProjectID, b.ID, *req.HoldUnauthenticated)
	if err != nil {
		s.internal(w, "update inbox", err)
		return
	}
	writeJSON(w, http.StatusOK, inboxView(b))
}

func (s *Server) deleteInbox(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadInbox(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteInbox(r.Context(), b.ProjectID, b.ID); err != nil {
		s.internal(w, "delete inbox", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "inbox", "id": b.ID, "deleted": true})
}

type messageSummary struct {
	Object                 string    `json:"object"`
	ID                     string    `json:"id"`
	InboxID                string    `json:"inbox_id"`
	From                   string    `json:"from"`
	To                     []string  `json:"to"`
	Subject                string    `json:"subject"`
	ReceivedAt             time.Time `json:"received_at"`
	ThreadID               string    `json:"thread_id"`
	InReplyToEmailID       *string   `json:"in_reply_to_email_id"`
	Snippet                string    `json:"snippet"`
	Trust                  string    `json:"trust"`
	Held                   bool      `json:"held"`
	SuspiciousInstructions bool      `json:"suspicious_instructions"`
}

// maxDerivedSnippets bounds how many messages stored before snippets were
// sealed one list call opens to derive theirs.
const maxDerivedSnippets = 20

func (s *Server) summaries(ctx context.Context, ms []store.InboundMessage) []messageSummary {
	out := make([]messageSummary, 0, len(ms))
	derived := 0
	for _, m := range ms {
		out = append(out, messageSummary{
			Object: "message", ID: m.ID, InboxID: m.InboxID, From: m.From, To: m.To, Subject: m.Subject,
			ReceivedAt: m.ReceivedAt, ThreadID: m.ThreadID, InReplyToEmailID: m.InReplyToEmailID,
			Snippet: s.snippet(ctx, m, &derived), Trust: m.Trust, Held: m.Held, SuspiciousInstructions: len(m.SuspiciousRules) > 0,
		})
	}
	return out
}

// snippet unseals a message's snippet. A message stored before snippets
// were sealed has none: it is derived from the sealed body once and stored
// sealed.
func (s *Server) snippet(ctx context.Context, m store.InboundMessage, derived *int) string {
	if m.Snippet != nil {
		b, err := s.Sealer.Open(m.Snippet)
		if err != nil {
			s.Log.Warn("cannot unseal a snippet", "message", m.ID, "err", err)
			return ""
		}
		return string(b)
	}
	if m.PurgedAt != nil || *derived >= maxDerivedSnippets {
		return ""
	}
	*derived++
	full, err := s.Store.GetInboundMessage(ctx, m.ProjectID, m.ID)
	if err != nil || full.Raw == nil {
		return ""
	}
	p, err := s.openMessage(full)
	if err != nil || p == nil {
		return ""
	}
	sn := p.Snippet(200)
	if err := s.Store.SetSnippet(ctx, m.ID, s.Sealer.Seal([]byte(sn))); err != nil {
		s.Log.Warn("store a derived snippet", "err", err)
	}
	return sn
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	b, ok := s.loadInbox(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	before, thread := q.Get("before"), q.Get("thread_id")
	if (before != "" && !validID(before)) || (thread != "" && !validID(thread)) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "before and thread_id must be ids")
		return
	}
	msgs, more, err := s.Store.ListInboundMessages(r.Context(), b.ProjectID, b.ID, before, thread, limit, seesHeld(r))
	if err != nil {
		s.internal(w, "list messages", err)
		return
	}
	resp := map[string]any{"object": "list", "data": s.summaries(r.Context(), msgs), "has_more": more, "next_before": nil}
	if more {
		resp["next_before"] = msgs[len(msgs)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) loadMessage(w http.ResponseWriter, r *http.Request) (store.InboundMessage, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "message not found")
		return store.InboundMessage{}, false
	}
	m, err := s.Store.GetInboundMessage(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "message not found")
		return m, false
	}
	if err != nil {
		s.internal(w, "get message", err)
		return m, false
	}
	if m.Held && !seesHeld(r) {
		writeError(w, http.StatusNotFound, "not_found",
			"message not found; a message whose sender is not authenticated stays hidden from sending_access keys until a full-access key releases it")
		return m, false
	}
	if !apiKeyOf(r).FullAccess() {
		ib, err := s.Store.GetInbox(r.Context(), m.ProjectID, m.InboxID)
		if err != nil {
			s.internal(w, "message inbox", err)
			return m, false
		}
		if !inboxAllowed(r, ib.Address) {
			inboxForbidden(w, ib.Address)
			return m, false
		}
	}
	return m, true
}

// openMessage unseals and parses a stored message; nil once purged.
func (s *Server) openMessage(m store.InboundMessage) (*mailparse.Message, error) {
	if m.Raw == nil {
		return nil, nil
	}
	raw, err := s.Sealer.Open(m.Raw)
	if err != nil {
		return nil, err
	}
	return mailparse.Parse(raw), nil
}

type attachmentView struct {
	Index       int    `json:"index"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	ContentID   string `json:"content_id,omitempty"`
	Inline      bool   `json:"inline"`
}

type messageResponse struct {
	Object           string            `json:"object"`
	ID               string            `json:"id"`
	InboxID          string            `json:"inbox_id"`
	From             string            `json:"from"`
	To               []string          `json:"to"`
	Cc               []string          `json:"cc"`
	ReplyTo          []string          `json:"reply_to"`
	Subject          string            `json:"subject"`
	Text             string            `json:"text"`
	HTML             string            `json:"html"`
	Headers          map[string]string `json:"headers"`
	Attachments      []attachmentView  `json:"attachments"`
	ThreadID         string            `json:"thread_id"`
	InReplyToEmailID *string           `json:"in_reply_to_email_id"`
	ReceivedAt       time.Time         `json:"received_at"`
	Size             int               `json:"size"`
	TLS              bool              `json:"tls"`
	BodyPurged       bool              `json:"body_purged"`

	// Sender authentication and safety flags.
	MailFrom               string          `json:"mail_from"`
	Trust                  string          `json:"trust"`
	AuthenticationResults  json.RawMessage `json:"authentication_results"`
	Held                   bool            `json:"held"`
	ReleasedAt             *time.Time      `json:"released_at"`
	SuspiciousInstructions bool            `json:"suspicious_instructions"`
	SuspiciousRules        []string        `json:"suspicious_rules"`
	// ReplyToMismatch: Reply-To points to another domain than From, a
	// common trick to divert answers.
	ReplyToMismatch   bool `json:"reply_to_mismatch"`
	MessageIDConflict bool `json:"message_id_conflict"`
}

// addrDomain is the lowercase domain of a formatted address.
func addrDomain(a string) string {
	if i := strings.LastIndexByte(a, '<'); i >= 0 {
		a = strings.TrimSuffix(a[i+1:], ">")
	}
	return strings.ToLower(domainOf(a))
}

// replyToMismatch reports whether a Reply-To address is on a domain
// unrelated to From's (a different organizational domain).
func replyToMismatch(m store.InboundMessage) bool {
	from := addrDomain(m.From)
	for _, rt := range m.ReplyTo {
		if d := addrDomain(rt); d != "" && from != "" && mailauth.OrgDomain(d) != mailauth.OrgDomain(from) {
			return true
		}
	}
	return false
}

func messageView(m store.InboundMessage) messageResponse {
	auth := json.RawMessage(m.AuthResults)
	if len(auth) == 0 {
		auth = json.RawMessage("{}")
	}
	rules := m.SuspiciousRules
	if rules == nil {
		rules = []string{}
	}
	return messageResponse{
		Object: "message", ID: m.ID, InboxID: m.InboxID, From: m.From, To: m.To, Cc: m.Cc, ReplyTo: m.ReplyTo,
		Subject: m.Subject, ThreadID: m.ThreadID, InReplyToEmailID: m.InReplyToEmailID, ReceivedAt: m.ReceivedAt,
		Size: m.Size, TLS: m.TLS, Headers: map[string]string{}, Attachments: []attachmentView{},
		MailFrom: m.MailFrom, Trust: m.Trust, AuthenticationResults: auth, Held: m.Held, ReleasedAt: m.ReleasedAt,
		SuspiciousInstructions: len(rules) > 0, SuspiciousRules: rules, ReplyToMismatch: replyToMismatch(m),
		MessageIDConflict: m.MessageIDConflict,
	}
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMessage(w, r)
	if !ok {
		return
	}
	p, err := s.openMessage(m)
	if err != nil {
		s.internal(w, "open message", err)
		return
	}
	resp := messageView(m)
	resp.BodyPurged = p == nil
	if p != nil {
		resp.Text, resp.HTML, resp.Headers = p.Text, p.HTML, p.Headers
		for i, a := range p.Attachments {
			resp.Attachments = append(resp.Attachments, attachmentView{
				Index: i, Filename: a.Filename, ContentType: a.ContentType, Size: len(a.Data), ContentID: a.ContentID, Inline: a.Inline,
			})
		}
	} else {
		// The body is gone; the threading headers are kept.
		if m.MessageID != "" {
			resp.Headers["message-id"] = "<" + m.MessageID + ">"
		}
		if m.InReplyTo != "" {
			resp.Headers["in-reply-to"] = "<" + m.InReplyTo + ">"
		}
		if len(m.References) > 0 {
			resp.Headers["references"] = "<" + strings.Join(m.References, "> <") + ">"
		}
		if m.Date != "" {
			resp.Headers["date"] = m.Date
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMessage(w, r)
	if !ok {
		return
	}
	p, err := s.openMessage(m)
	if err != nil {
		s.internal(w, "open message", err)
		return
	}
	if p == nil {
		writeError(w, http.StatusGone, "body_purged", "this message's content was deleted after the inbound retention period")
		return
	}
	i, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || i < 0 || i >= len(p.Attachments) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("attachment not found; this message has %d (indexes start at 0)", len(p.Attachments)))
		return
	}
	a := p.Attachments[i]
	ct := a.ContentType
	if _, _, err := mime.ParseMediaType(ct); err != nil || ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
	w.WriteHeader(http.StatusOK)
	w.Write(a.Data)
}

// replyToMessage sends a reply from the inbox address to the message's
// Reply-To (or From), threaded with In-Reply-To and References. It goes
// through POST /emails with the caller's own key, so every sending rule
// (authorizeSend, verified domain, suppression, idempotency) applies.
func (s *Server) replyToMessage(root func() http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.loadMessage(w, r)
		if !ok {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Text string   `json:"text"`
			HTML string   `json:"html"`
			To   []string `json:"to"`
		}
		if err := decodeStrict(body, &req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
			return
		}
		if req.Text == "" && req.HTML == "" {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "text or html is required")
			return
		}
		inbox, err := s.Store.GetInbox(r.Context(), m.ProjectID, m.InboxID)
		if err != nil {
			s.internal(w, "reply inbox", err)
			return
		}
		// One recipient by default: the first Reply-To, or From, or the
		// envelope sender. A sender who lists many Reply-To addresses must
		// not turn an agent's answer into mail to all of them; to answer
		// more people, the caller names them in "to".
		to := req.To
		if len(to) == 0 {
			switch {
			case len(m.ReplyTo) > 0:
				to = m.ReplyTo[:1]
			case m.From != "":
				to = []string{m.From}
			case m.MailFrom != "":
				to = []string{m.MailFrom}
			}
		}
		if len(to) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "no_reply_address", `the message has no Reply-To, From or envelope sender to reply to; pass "to"`)
			return
		}
		var warnings []string
		if len(req.To) == 0 && len(m.ReplyTo) > 1 {
			warnings = append(warnings, fmt.Sprintf(`the message lists %d Reply-To addresses; the reply went only to the first. Pass "to" to answer others`, len(m.ReplyTo)))
		}
		if len(req.To) == 0 && replyToMismatch(m) {
			warnings = append(warnings, "Reply-To is on another domain than From: the reply went to "+addrDomain(to[0])+", not to the sender's domain "+addrDomain(m.From))
		}
		send := map[string]any{"from": inbox.Address, "to": to, "subject": replySubject(m.Subject)}
		if req.Text != "" {
			send["text"] = req.Text
		}
		if req.HTML != "" {
			send["html"] = req.HTML
		}
		if threadable(m.MessageID) {
			refs := []string{}
			for _, ref := range m.References {
				if threadable(ref) {
					refs = append(refs, ref)
				}
			}
			send["headers"] = map[string]string{
				"In-Reply-To": "<" + m.MessageID + ">",
				"References":  references(append(refs, m.MessageID), 900),
			}
		}
		b, _ := json.Marshal(send)
		inner, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://openmailwire.internal/emails", bytes.NewReader(b))
		inner.Header.Set("Authorization", r.Header.Get("Authorization"))
		inner.Header.Set("Content-Type", "application/json")
		if k := r.Header.Get("Idempotency-Key"); k != "" {
			inner.Header.Set("Idempotency-Key", k)
		}
		resp, err := (&http.Client{Transport: inProcess{root}}).Do(inner)
		if err != nil {
			s.internal(w, "reply send", err)
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var sent struct {
			ID string `json:"id"`
		}
		if resp.StatusCode/100 != 2 || json.Unmarshal(out, &sent) != nil || sent.ID == "" {
			// Send refusals (domain, policy, limits) reach the caller as they are.
			for _, h := range []string{"Content-Type", "Retry-After"} {
				if v := resp.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			w.Write(out)
			return
		}
		if err := s.Store.LinkReply(r.Context(), sent.ID, m.ThreadID, m.ID); err != nil {
			s.internal(w, "link reply", err)
			return
		}
		if v := resp.Header.Get("Idempotent-Replayed"); v != "" {
			w.Header().Set("Idempotent-Replayed", v)
		}
		answer := map[string]any{"id": sent.ID, "thread_id": m.ThreadID, "to": to}
		if len(warnings) > 0 {
			answer["warnings"] = warnings
		}
		writeJSON(w, resp.StatusCode, answer)
	}
}

// releaseMessage shows a held message to every key with inbox access.
func (s *Server) releaseMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	m, changed, err := s.Store.ReleaseInboundMessage(r.Context(), projectID(r), id, apiKeyOf(r).ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	if err != nil {
		s.internal(w, "release message", err)
		return
	}
	s.Log.Info("inbound message released", "message", m.ID, "key", apiKeyOf(r).ID, "changed", changed)
	v := messageView(m)
	v.BodyPurged = m.Raw == nil
	writeJSON(w, http.StatusOK, v)
}

// replySubject prefixes "Re: " unless the subject already has it.
func replySubject(subject string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		return "Re: (no subject)"
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "re:") {
		return s
	}
	s = "Re: " + s
	if len(s) > 998 {
		s = strings.ToValidUTF8(s[:998], "")
	}
	return s
}

// threadable reports whether a message identifier can be written back into
// a header unchanged.
func threadable(id string) bool {
	if id == "" || len(id) > 250 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c >= 0x7f || c == '<' || c == '>' {
			return false
		}
	}
	return true
}

// references renders a References header that fits in max bytes. When the
// chain is too long it keeps the first identifier and the most recent ones,
// as RFC 5322 suggests.
func references(ids []string, max int) string {
	render := func(l []string) string { return "<" + strings.Join(l, "> <") + ">" }
	for len(ids) > 2 && len(render(ids)) > max {
		ids = append(ids[:1], ids[2:]...)
	}
	if len(render(ids)) > max {
		ids = ids[len(ids)-1:]
	}
	return render(ids)
}

// emailReplies lists the inbound messages that answer a sent email.
func (s *Server) emailReplies(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	// A sending_access key sees replies only to emails it sent itself, as
	// with GET /emails/{id}.
	if own := ownKeyOnly(r); own != "" {
		v, err := s.Store.GetEmail(r.Context(), projectID(r), id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (v.APIKeyID == nil || *v.APIKeyID != own)) {
			writeError(w, http.StatusNotFound, "not_found", "email not found")
			return
		}
		if err != nil {
			s.internal(w, "get email", err)
			return
		}
	}
	thread, msgs, err := s.Store.RepliesToEmail(r.Context(), projectID(r), id, 100, seesHeld(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	if err != nil {
		s.internal(w, "email replies", err)
		return
	}
	if !apiKeyOf(r).FullAccess() {
		// Only replies that reached inboxes this key may read.
		inboxes, err := s.Store.ListInboxes(r.Context(), projectID(r))
		if err != nil {
			s.internal(w, "list inboxes", err)
			return
		}
		allowed := map[string]bool{}
		for _, b := range inboxes {
			allowed[b.ID] = inboxAllowed(r, b.Address)
		}
		kept := msgs[:0]
		for _, m := range msgs {
			if allowed[m.InboxID] {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "email_id": id, "thread_id": thread, "data": s.summaries(r.Context(), msgs)})
}
