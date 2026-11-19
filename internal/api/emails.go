package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/message"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

const (
	maxRecipients = 50
	maxExpiry     = 7 * 24 * time.Hour
	maxSchedule   = 30 * 24 * time.Hour
	maxBatch      = 100
)

// strList accepts "a@b.c" or ["a@b.c", …], like Resend.
type strList []string

func (l *strList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one != "" {
			*l = []string{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("must be a string or an array of strings")
	}
	*l = many
	return nil
}

type tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type attachmentRequest struct {
	Filename string `json:"filename"`
	// Content is the file, base64-encoded. It stays a slice of the request
	// body until it is decoded, so a 10 MB file is never also a 13 MB
	// string.
	Content     jsonSpan `json:"content"`
	ContentType string   `json:"content_type"`
	ContentID   string   `json:"content_id"`
}

type sendRequest struct {
	From           string              `json:"from"`
	To             strList             `json:"to"`
	Cc             strList             `json:"cc"`
	Bcc            strList             `json:"bcc"`
	ReplyTo        strList             `json:"reply_to"`
	Subject        string              `json:"subject"`
	HTML           string              `json:"html"`
	Text           string              `json:"text"`
	Headers        map[string]string   `json:"headers"`
	Tags           []tag               `json:"tags"`
	ExpiresAt      *time.Time          `json:"expires_at"`
	ScheduledAt    *time.Time          `json:"scheduled_at"`
	RetryUncertain *bool               `json:"retry_uncertain"`
	Attachments    []attachmentRequest `json:"attachments"`
}

var tagRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// sendPlan is a validated send request, ready to render.
type sendPlan struct {
	req            sendRequest
	from           string // the sender address, lowercase
	fromDomain     string
	recipients     []string
	tags           map[string]string
	attachments    []message.Attachment
	retryUncertain bool
	scheduled      *time.Time
	expires        time.Time
}

// validateSend checks a send request without touching the database.
func validateSend(req sendRequest, now time.Time, defaultExpiry time.Duration) (*sendPlan, *policyError) {
	if req.From == "" {
		return nil, invalid("from is required, like \"Acme <hello@acme.com>\" on a verified domain")
	}
	from, err := message.ParseAddress("from", req.From)
	if err != nil {
		return nil, invalid("%v", err)
	}
	p := &sendPlan{req: req, from: strings.ToLower(from.Address), tags: map[string]string{}, retryUncertain: true}
	p.fromDomain = domainOf(p.from)
	switch {
	case len(req.To) == 0:
		return nil, invalid("to is required")
	case strings.TrimSpace(req.Subject) == "":
		return nil, invalid("subject is required")
	case len(req.Subject) > message.MaxSubject:
		return nil, invalid("subject is longer than %d bytes", message.MaxSubject)
	case !message.Storable(req.Subject) || strings.ContainsFunc(req.Subject, isControl):
		return nil, invalid("subject contains a NUL or other control character; remove it")
	case req.HTML == "" && req.Text == "":
		return nil, invalid("html or text is required")
	}
	if p.recipients, err = envelope(req.To, req.Cc, req.Bcc); err != nil {
		return nil, invalid("%v", err)
	}
	for _, a := range req.ReplyTo {
		if _, err := message.ParseAddress("reply_to", a); err != nil {
			return nil, invalid("%v", err)
		}
	}
	if err := message.ValidateHeaders(req.Headers); err != nil {
		return nil, invalid("%v", err)
	}
	for _, t := range req.Tags {
		if !tagRE.MatchString(t.Name) || !tagRE.MatchString(t.Value) {
			return nil, invalid("tag names and values may contain only letters, digits, _ and - (1-256 characters)")
		}
		p.tags[t.Name] = t.Value
	}
	if p.attachments, err = decodeAttachments(req.Attachments); err != nil {
		return nil, invalid("%v", err)
	}
	p.req.Attachments = nil // decoded; let the request body go

	start := now
	if req.ScheduledAt != nil {
		at := req.ScheduledAt.UTC()
		if !at.After(now) || at.After(now.Add(maxSchedule)) {
			return nil, invalid("scheduled_at must be in the future and at most 30 days ahead (RFC 3339, e.g. %s)",
				now.Add(time.Hour).Format(time.RFC3339))
		}
		p.scheduled, start = &at, at
	}
	p.expires = start.Add(defaultExpiry)
	if req.ExpiresAt != nil {
		if !req.ExpiresAt.After(start) || req.ExpiresAt.After(start.Add(maxExpiry)) {
			if p.scheduled != nil {
				return nil, invalid("expires_at must be after scheduled_at and within 7 days of it")
			}
			return nil, invalid("expires_at must be in the future and within 7 days")
		}
		p.expires = req.ExpiresAt.UTC()
	}
	if req.RetryUncertain != nil {
		p.retryUncertain = *req.RetryUncertain
	}
	return p, nil
}

// decodeAttachments decodes base64 content and checks the limits.
func decodeAttachments(in []attachmentRequest) ([]message.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > message.MaxAttachments {
		return nil, fmt.Errorf("at most %d attachments per email", message.MaxAttachments)
	}
	out := make([]message.Attachment, 0, len(in))
	total := 0
	for i, a := range in {
		data, err := decodeContent(a.Content)
		if errors.Is(err, errNoContent) {
			return nil, fmt.Errorf("attachments[%d]: content is required (the file, base64-encoded)", i)
		}
		if err != nil {
			return nil, fmt.Errorf("attachments[%d]: content is not valid base64: %v", i, err)
		}
		if total += len(data); total > message.MaxAttachmentBytes {
			return nil, fmt.Errorf("attachments total more than %d MB decoded; link large files instead", message.MaxAttachmentBytes>>20)
		}
		out = append(out, message.Attachment{
			Filename: a.Filename, ContentType: a.ContentType,
			ContentID: strings.TrimSuffix(strings.TrimPrefix(a.ContentID, "<"), ">"), Data: data,
		})
	}
	return out, message.ValidateAttachments(out)
}

var errNoContent = errors.New("no content")

// decodeContent decodes an attachment's content straight from the JSON
// string in the request, without copying it first.
func decodeContent(span jsonSpan) ([]byte, error) {
	if span.isNull() || string(span) == `""` {
		return nil, errNoContent
	}
	if len(span) < 2 || span[0] != '"' {
		return nil, errors.New("content must be a string")
	}
	inner := span[1 : len(span)-1]
	if bytes.ContainsAny(inner, "\\ \t") {
		// Escapes (such as \n line breaks) or spaces: the rare slow path.
		var s string
		if err := json.Unmarshal(span, &s); err != nil {
			return nil, err
		}
		if s == "" {
			return nil, errNoContent
		}
		return decodeBase64(s)
	}
	if len(inner)%4 != 0 {
		return base64.RawStdEncoding.DecodeString(strings.TrimRight(string(inner), "="))
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(inner)))
	n, err := base64.StdEncoding.Decode(out, inner)
	return out[:n], err
}

// decodeBase64 accepts standard base64, with or without padding and line
// breaks.
func decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if len(s)%4 != 0 {
		return base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	return base64.StdEncoding.DecodeString(s)
}

// newEmail renders and signs the message for a validated plan. The caller
// sets the idempotency key and request hash; queue sets the project and key.
func (s *Server) newEmail(p *sendPlan, dom store.Domain, now time.Time) (store.NewEmail, *policyError) {
	id := secret.UUID()
	req := p.req
	raw, err := message.Build(message.Input{
		From: req.From, To: req.To, Cc: req.Cc, ReplyTo: req.ReplyTo,
		Subject: req.Subject, Text: req.Text, HTML: req.HTML, Headers: req.Headers,
		MessageID: id + "@" + dom.Name, Date: now, Attachments: p.attachments,
	})
	if err != nil {
		return store.NewEmail{}, invalid("%v", err)
	}
	signed, pe := s.signMessage(dom, raw)
	if pe != nil {
		return store.NewEmail{}, pe
	}
	return store.NewEmail{
		ID: id, DomainID: dom.ID, From: req.From, To: req.To, Cc: req.Cc, Bcc: req.Bcc, Subject: req.Subject,
		MessageID: id + "@" + dom.Name, ReturnPath: dom.ReturnPath, Raw: s.Sealer.Seal(signed),
		Tags: p.tags, RetryUncertain: p.retryUncertain, ExpiresAt: p.expires, ScheduledAt: p.scheduled,
		Recipients: p.recipients,
	}, nil
}

func (s *Server) sendEmail(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req sendRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	idem, pe := idempotencyKey(r)
	if pe != nil {
		pe.write(w)
		return
	}
	hash := sha256.Sum256(body)
	if s.replayed(w, r, idem, hash[:]) {
		return
	}
	now := time.Now().UTC()
	plan, pe := validateSend(req, now, s.Cfg.DefaultExpiry)
	if pe != nil {
		pe.write(w)
		return
	}
	if pe := s.authorizeSend(r, plan.from, plan.recipients); pe != nil {
		pe.write(w)
		return
	}
	dom, pe := s.senderDomain(r.Context(), projectID(r), plan.fromDomain)
	if pe != nil {
		pe.write(w)
		return
	}
	e, pe := s.newEmail(plan, dom, now)
	if pe != nil {
		pe.write(w)
		return
	}
	e.IdempotencyKey, e.RequestHash = idem, hash[:]
	out, pe := s.queue(r, []store.NewEmail{e})
	if pe != nil {
		pe.write(w)
		return
	}
	if out[0].Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": out[0].ID})
}

// sendBatch queues up to 100 emails, all or none. Every item is validated
// and checked against the key's policy before anything is stored; the caps
// apply to the batch as a whole.
func (s *Server) sendBatch(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var items []jsonSpan
	if err := json.Unmarshal(body, &items); err != nil || len(items) == 0 || len(items) > maxBatch {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("the body must be a JSON array of 1 to %d emails, each shaped like a POST /emails body", maxBatch))
		return
	}
	idem, pe := idempotencyKey(r)
	if pe != nil {
		pe.write(w)
		return
	}
	ctx, key := r.Context(), apiKeyOf(r)
	hashes := make([][]byte, len(items))
	for i, raw := range items {
		h := sha256.Sum256(raw)
		hashes[i] = h[:]
	}
	if idem != "" && s.batchReplayed(w, r, idem, hashes) {
		return
	}

	now := time.Now().UTC()
	plans := make([]*sendPlan, len(items))
	var all []string
	for i, raw := range items {
		prefix := fmt.Sprintf("emails[%d]: ", i)
		var req sendRequest
		if err := decodeStrict(raw, &req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", prefix+err.Error())
			return
		}
		if req.Attachments != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error",
				prefix+"attachments are not supported in a batch; send emails with attachments one at a time with POST /emails")
			return
		}
		plan, pe := validateSend(req, now, s.Cfg.DefaultExpiry)
		if pe == nil {
			pe = allowedBy(key.Policy, plan.from, plan.recipients)
		}
		if pe != nil {
			pe.prefixed(prefix).write(w)
			return
		}
		plans[i] = plan
		all = append(all, plan.recipients...)
	}
	if pe := s.checkCaps(ctx, key, all); pe != nil {
		pe.write(w)
		return
	}

	doms := map[string]store.Domain{}
	es := make([]store.NewEmail, len(plans))
	for i, plan := range plans {
		prefix := fmt.Sprintf("emails[%d]: ", i)
		dom, seen := doms[plan.fromDomain]
		if !seen {
			var pe *policyError
			if dom, pe = s.senderDomain(ctx, key.ProjectID, plan.fromDomain); pe != nil {
				pe.prefixed(prefix).write(w)
				return
			}
			doms[plan.fromDomain] = dom
		}
		e, pe := s.newEmail(plan, dom, now)
		if pe != nil {
			pe.prefixed(prefix).write(w)
			return
		}
		if idem != "" {
			e.IdempotencyKey, e.RequestHash = batchItemKey(idem, i), hashes[i]
		}
		es[i] = e
	}
	out, pe := s.queue(r, es)
	if pe != nil {
		pe.write(w)
		return
	}
	writeJSON(w, http.StatusOK, batchResponse(out))
}

func batchResponse(out []store.Inserted) map[string]any {
	data := make([]map[string]string, len(out))
	for i, o := range out {
		data[i] = map[string]string{"id": o.ID}
	}
	return map[string]any{"data": data}
}

// batchReplayed answers a batch whose every item was already stored under
// batchItemKey(idem, index) with the same body. Partly stored batches
// cannot exist (batches are all or nothing), so anything else goes on to
// queue, which applies the idempotency rules per item.
func (s *Server) batchReplayed(w http.ResponseWriter, r *http.Request, idem string, hashes [][]byte) bool {
	out := make([]store.Inserted, len(hashes))
	for i, h := range hashes {
		id, err := s.Store.FindIdempotent(r.Context(), projectID(r), apiKeyOf(r).ID, batchItemKey(idem, i), h)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return false
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, "idempotency_conflict", fmt.Sprintf(
				"this Idempotency-Key was already used with a different request (emails[%d] differs)", i))
			return true
		case err != nil:
			s.internal(w, "find idempotent", err)
			return true
		}
		out[i] = store.Inserted{ID: id, Replayed: true}
	}
	w.Header().Set("Idempotent-Replayed", "true")
	writeJSON(w, http.StatusOK, batchResponse(out))
	return true
}

// envelope validates and deduplicates every recipient address.
func envelope(lists ...[]string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, a := range l {
			addr, err := message.ParseAddress("recipient", a)
			if err != nil {
				return nil, err
			}
			e := strings.ToLower(addr.Address)
			if !isASCII(e) {
				return nil, fmt.Errorf("recipient %q: internationalized addresses (SMTPUTF8) are not supported yet", a)
			}
			if domainOf(e) == "" || strings.Count(e, "@") != 1 {
				return nil, fmt.Errorf("recipient %q has no domain", a)
			}
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	if len(out) > maxRecipients {
		return nil, fmt.Errorf("at most %d recipients per email (to, cc and bcc together)", maxRecipients)
	}
	return out, nil
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// isControl reports whether r is a control character other than tab.
func isControl(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

type emailResponse struct {
	Object      string                `json:"object"`
	ID          string                `json:"id"`
	APIKeyID    *string               `json:"api_key_id"`
	From        string                `json:"from"`
	To          []string              `json:"to"`
	Cc          []string              `json:"cc"`
	Bcc         []string              `json:"bcc"`
	Subject     string                `json:"subject"`
	MessageID   string                `json:"message_id"`
	CreatedAt   time.Time             `json:"created_at"`
	ScheduledAt *time.Time            `json:"scheduled_at"`
	ExpiresAt   time.Time             `json:"expires_at"`
	LastEvent   string                `json:"last_event"`
	Tags        map[string]string     `json:"tags"`
	BodyPurged  bool                  `json:"body_purged"`
	Recipients  []store.RecipientView `json:"recipients"`
	Attempts    []store.AttemptView   `json:"attempts"`
	Events      []store.EventView     `json:"events"`
}

func (s *Server) getEmail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	v, err := s.Store.GetEmail(r.Context(), projectID(r), id)
	if own := ownKeyOnly(r); err == nil && own != "" && (v.APIKeyID == nil || *v.APIKeyID != own) {
		err = store.ErrNotFound // a sending_access key sees only its own emails
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	if err != nil {
		s.internal(w, "get email", err)
		return
	}
	resp := emailResponse{
		Object: "email", ID: v.ID, APIKeyID: v.APIKeyID, From: v.From, To: v.To, Cc: v.Cc, Bcc: v.Bcc, Subject: v.Subject,
		MessageID: "<" + v.MessageID + ">", CreatedAt: v.CreatedAt, ScheduledAt: v.ScheduledAt, ExpiresAt: v.ExpiresAt,
		Tags: v.Tags, BodyPurged: v.BodyPurged, Recipients: v.Recipients, Attempts: v.Attempts, Events: v.Events,
	}
	if n := len(v.Events); n > 0 {
		resp.LastEvent = v.Events[n-1].Type
	}
	if resp.Attempts == nil {
		resp.Attempts = []store.AttemptView{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// listEmails pages through the project's emails, newest first.
func (s *Server) listEmails(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "limit must be a number from 1 to 100")
			return
		}
		limit = n
	}
	before, keyID := q.Get("before"), q.Get("api_key_id")
	if before != "" && !validID(before) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "before must be an email id, as next_before returns")
		return
	}
	if keyID != "" && !validID(keyID) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "api_key_id must be an API key id (GET /api-keys)")
		return
	}
	if own := ownKeyOnly(r); own != "" {
		if keyID != "" && keyID != own {
			writeError(w, http.StatusForbidden, "insufficient_permission",
				"this API key has sending_access, so it can list only its own emails; leave out api_key_id")
			return
		}
		keyID = own
	}
	list, more, err := s.Store.ListEmails(r.Context(), store.ListEmailsQuery{
		ProjectID: projectID(r), APIKeyID: keyID, Before: before, Limit: limit,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"before names no email in this list; pass the next_before value of the previous page")
		return
	}
	if err != nil {
		s.internal(w, "list emails", err)
		return
	}
	var next *string
	if more {
		next = &list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list, "has_more": more, "next_before": next})
}

// cancelEmail cancels the recipients of an email that have not been
// attempted yet, typically a scheduled send.
func (s *Server) cancelEmail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	canceled, err := s.Store.CancelEmail(r.Context(), projectID(r), id, ownKeyOnly(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	if err != nil {
		s.internal(w, "cancel email", err)
		return
	}
	if len(canceled) == 0 {
		writeError(w, http.StatusConflict, "cannot_cancel", fmt.Sprintf(
			"nothing to cancel: only recipients still queued and never attempted can be canceled, and this email has none left; see GET /emails/%s", id))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "email", "id": id, "canceled": canceled})
}
