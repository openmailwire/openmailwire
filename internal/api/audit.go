package api

import (
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/openmailwire/openmailwire/internal/store"
)

// audit records in the project's audit log that the request's API key did
// action to a target. Call it after the change succeeded. The change is
// already made, so a failure to record it is logged, not shown to the
// caller. To record inside the change's own transaction, use
// store.AppendAuditTx instead.
//
// Other handlers use it as a one-line hook, e.g.
//
//	s.audit(r, "email.approved", "email", id, map[string]any{"recipients": n})
//
// Data must never hold secrets, tokens or message bodies.
func (s *Server) audit(r *http.Request, action, targetType, targetID string, data map[string]any) {
	key := apiKeyOf(r)
	err := s.Store.AppendAudit(r.Context(), store.AuditEntry{
		ProjectID: key.ProjectID, Actor: store.ActorAPIKey, ActorKeyID: key.ID,
		Action: action, TargetType: targetType, TargetID: targetID, Data: data,
	})
	if err != nil {
		s.Log.Error("audit log", "action", action, "err", err)
	}
}

type auditEntryResponse struct {
	Object string `json:"object"`
	store.AuditRecord
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// listAuditLog pages through the project's audit log, newest first:
// GET /audit-log?limit=50&before=<id>&action=api_key.created
func (s *Server) listAuditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "limit must be a number from 1 to 200")
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "before must be an entry id, as next_before returns")
			return
		}
		before = n
	}
	action := q.Get("action")
	if len(action) > 100 {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "action is longer than 100 characters")
		return
	}
	list, more, err := s.Store.ListAudit(r.Context(), store.AuditQuery{
		ProjectID: projectID(r), Before: before, Action: action, Limit: limit,
	})
	if err != nil {
		s.internal(w, "list audit log", err)
		return
	}
	data := make([]auditEntryResponse, len(list))
	for i, e := range list {
		data[i] = auditEntryResponse{Object: "audit_entry", AuditRecord: e,
			PrevHash: hex.EncodeToString(e.PrevHash), Hash: hex.EncodeToString(e.Hash)}
	}
	var next *string
	if more {
		v := strconv.FormatInt(list[len(list)-1].ID, 10)
		next = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": more, "next_before": next})
}

// verifyAuditLog recomputes the project's whole hash chain. Keep the
// head_hash it returns: if a later check's chain no longer contains it,
// entries were removed from the end.
func (s *Server) verifyAuditLog(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.VerifyAudit(r.Context(), projectID(r))
	if err != nil {
		s.internal(w, "verify audit log", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "audit_verification", "valid": v.Valid, "entries": v.Entries,
		"head_hash": hex.EncodeToString(v.Head), "first_invalid_id": v.FirstInvalidID, "problem": v.Problem,
	})
}
