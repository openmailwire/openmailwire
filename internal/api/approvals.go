package api

// Approval holds (lane B): an API key whose policy has require_approval
// sends emails that wait as 'held' until a full_access key, or a key with
// can_approve, approves or rejects them. Held mail that nobody decides on
// expires at its expires_at.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openmailwire/openmailwire/internal/mailparse"
	"github.com/openmailwire/openmailwire/internal/store"
)

const (
	previewRunes = 2000
	previewMax   = 2 << 20 // larger sealed messages are not opened for a preview
)

type heldEmailResponse struct {
	Object      string     `json:"object"`
	ID          string     `json:"id"`
	APIKeyID    *string    `json:"api_key_id"`
	From        string     `json:"from"`
	To          []string   `json:"to"`
	Cc          []string   `json:"cc"`
	Bcc         []string   `json:"bcc"`
	Subject     string     `json:"subject"`
	Stream      string     `json:"stream"`
	HoldReasons []string   `json:"hold_reasons"`
	Recipients  []string   `json:"recipients"` // the ones waiting
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	// Text is the start of the plain text (or the HTML as text), so a
	// reviewer can decide without fetching the message.
	Text      string `json:"text"`
	Truncated bool   `json:"text_truncated"`
}

// listHeld answers GET /emails/held: emails waiting for approval, newest
// first. Approvers see the project's; other keys see their own.
func (s *Server) listHeld(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "limit must be a number from 1 to 50")
			return
		}
		limit = n
	}
	before := q.Get("before")
	if before != "" && !validID(before) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "before must be an email id, as next_before returns")
		return
	}
	key := apiKeyOf(r)
	own := ""
	if !canApprove(key) {
		own = key.ID
	}
	list, more, err := s.Store.ListHeld(r.Context(), key.ProjectID, own, before, limit)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"before names no held email in this list; pass the next_before value of the previous page")
		return
	}
	if err != nil {
		s.internal(w, "list held", err)
		return
	}
	data := make([]heldEmailResponse, 0, len(list))
	for _, h := range list {
		v := heldEmailResponse{
			Object: "held_email", ID: h.ID, APIKeyID: h.APIKeyID, From: h.From, To: h.To, Cc: h.Cc, Bcc: h.Bcc,
			Subject: h.Subject, Stream: h.Stream, HoldReasons: h.Reasons, Recipients: h.Recipients,
			CreatedAt: h.CreatedAt, ExpiresAt: h.ExpiresAt, ScheduledAt: h.ScheduledAt,
		}
		v.Text, v.Truncated = s.preview(h.Raw)
		data = append(data, v)
	}
	var next *string
	if more {
		next = &list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": more, "next_before": next})
}

// preview returns the start of a sealed message's text.
func (s *Server) preview(sealed []byte) (string, bool) {
	if sealed == nil {
		return "", false
	}
	if len(sealed) > previewMax {
		return "", true
	}
	raw, err := s.Sealer.Open(sealed)
	if err != nil {
		s.Log.Error("unseal held email", "err", err)
		return "", false
	}
	m := mailparse.Parse(raw)
	text := m.Text
	if text == "" && m.HTML != "" {
		text = mailparse.HTMLToText(m.HTML)
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= previewRunes {
		return text, false
	}
	return string([]rune(text)[:previewRunes]), true
}

func (s *Server) approveEmail(w http.ResponseWriter, r *http.Request) { s.decideEmail(w, r, true) }
func (s *Server) rejectEmail(w http.ResponseWriter, r *http.Request)  { s.decideEmail(w, r, false) }

// decideEmail answers POST /emails/{id}/approve and /reject, with an
// optional {"note"} kept with the decision.
func (s *Server) decideEmail(w http.ResponseWriter, r *http.Request, approve bool) {
	verb, done := "reject", "rejected"
	if approve {
		verb, done = "approve", "approved"
	}
	key := apiKeyOf(r)
	if !canApprove(key) {
		writeError(w, http.StatusForbidden, "insufficient_permission", fmt.Sprintf(
			"this API key cannot %s held emails: that needs a full_access key, or a key whose policy has can_approve: true (PATCH /api-keys/{id})", verb))
		return
	}
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if len(body) > 0 {
		if err := decodeStrict(body, &req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
			return
		}
	}
	if utf8.RuneCountInString(req.Note) > 1000 || !utf8.ValidString(req.Note) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "note must be valid text of at most 1000 characters")
		return
	}
	ctx := r.Context()
	h, err := s.Store.GetHold(ctx, key.ProjectID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	if err != nil {
		s.internal(w, "get hold", err)
		return
	}
	notHeld := func() {
		writeError(w, http.StatusConflict, "not_held", fmt.Sprintf(
			"email %s has no recipient waiting for approval: it was never held, or it was already approved, rejected or expired; see GET /emails/%s", id, id))
	}
	if h.Held == 0 {
		notHeld()
		return
	}
	if approve {
		if h.APIKeyID != nil && *h.APIKeyID == key.ID {
			writeError(w, http.StatusForbidden, "cannot_approve_own_email",
				"an API key cannot approve the emails it sent itself; approve with a different key that has can_approve, or a full_access key")
			return
		}
		sender := store.APIKey{ProjectID: key.ProjectID}
		if h.APIKeyID != nil {
			sender.ID = *h.APIKeyID
		}
		var domIDs []string
		if h.DomainID != nil {
			domIDs = []string{*h.DomainID}
		}
		if pe := s.checkPause(ctx, sender, domIDs, []string{h.DomainName}); pe != nil {
			pe.write(w)
			return
		}
	}
	var d store.Decision
	if approve {
		d, err = s.Store.ApproveEmail(ctx, key.ProjectID, id, key.ID, req.Note)
	} else {
		d, err = s.Store.RejectEmail(ctx, key.ProjectID, id, key.ID, req.Note)
	}
	switch {
	case errors.Is(err, store.ErrNotHeld):
		notHeld()
		return
	case errors.Is(err, store.ErrPurged):
		writeError(w, http.StatusConflict, "message_purged", fmt.Sprintf(
			"email %s can no longer be sent: its message was purged (OMW_RETENTION). Reject it, and send it again if it is still wanted", id))
		return
	case err != nil:
		s.internal(w, verb+" email", err)
		return
	}
	s.audit(r, "email."+verb, "email", id, map[string]any{"recipients": len(d.Recipients), "suppressed": len(d.Suppressed)})
	if d.Recipients == nil {
		d.Recipients = []string{}
	}
	if d.Suppressed == nil {
		d.Suppressed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "email", "id": id, "decision": done,
		"recipients": d.Recipients, "suppressed": d.Suppressed})
}
