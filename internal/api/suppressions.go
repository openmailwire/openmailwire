package api

// The suppressions API (lane B): addresses the project never sends to.
// Bounces and complaints add them for every stream; one-click unsubscribes
// add them for broadcast mail only. Every key can read; changing needs
// full access.

import (
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openmailwire/openmailwire/internal/store"
)

var reasonRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// suppressionAddress validates and lowercases one address.
func suppressionAddress(a string) (string, *policyError) {
	l, err := envelope([]string{a})
	if err != nil || len(l) != 1 {
		return "", invalid("address must be one email address, like someone@example.com")
	}
	return l[0], nil
}

func suppressionStream(v string, allowEmpty bool) (string, *policyError) {
	switch v {
	case "":
		if allowEmpty {
			return "", nil
		}
		return store.SuppressAll, nil
	case store.SuppressAll, store.SuppressBroadcast:
		return v, nil
	}
	return "", invalid(`stream must be "all" (no mail at all) or "broadcast" (no broadcast mail; transactional mail still goes out)`)
}

// suppressionFilter reads the filters GET /suppressions and the export
// share.
func suppressionFilter(r *http.Request) (store.SuppressionQuery, *policyError) {
	q := r.URL.Query()
	f := store.SuppressionQuery{ProjectID: projectID(r), Reason: q.Get("reason")}
	if f.Reason != "" && !reasonRE.MatchString(f.Reason) {
		return f, invalid("reason must be a reason name such as hard_bounce, complaint, unsubscribe or manual")
	}
	var pe *policyError
	if f.Stream, pe = suppressionStream(q.Get("stream"), true); pe != nil {
		return f, pe
	}
	if a := q.Get("address"); a != "" {
		if f.Address, pe = suppressionAddress(a); pe != nil {
			return f, pe
		}
	}
	f.Domain = strings.ToLower(strings.TrimSpace(q.Get("domain")))
	return f, nil
}

func encodeSuppressionCursor(v store.Suppression) string {
	return base64.RawURLEncoding.EncodeToString([]byte(v.CreatedAt.UTC().Format(time.RFC3339Nano) + "\n" + v.Address + "\n" + v.Stream))
}

func decodeSuppressionCursor(s string) (*store.SuppressionCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(string(b), "\n")
	if len(parts) != 3 {
		return nil, false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, false
	}
	return &store.SuppressionCursor{CreatedAt: at, Address: parts[1], Stream: parts[2]}, true
}

// listSuppressions answers GET /suppressions?limit&before&reason&stream&address&domain,
// newest first.
func (s *Server) listSuppressions(w http.ResponseWriter, r *http.Request) {
	f, pe := suppressionFilter(r)
	if pe != nil {
		pe.write(w)
		return
	}
	q := r.URL.Query()
	f.Limit = 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "limit must be a number from 1 to 100")
			return
		}
		f.Limit = n
	}
	if v := q.Get("before"); v != "" {
		c, ok := decodeSuppressionCursor(v)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "before must be the next_before value of the previous page")
			return
		}
		f.After = c
	}
	list, more, err := s.Store.ListSuppressions(r.Context(), f)
	if err != nil {
		s.internal(w, "list suppressions", err)
		return
	}
	var next *string
	if more {
		c := encodeSuppressionCursor(list[len(list)-1])
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list, "has_more": more, "next_before": next})
}

// addSuppression answers POST /suppressions {"address", "reason"?, "detail"?, "stream"?}.
func (s *Server) addSuppression(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Address string `json:"address"`
		Reason  string `json:"reason"`
		Detail  string `json:"detail"`
		Stream  string `json:"stream"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	addr, pe := suppressionAddress(req.Address)
	if pe != nil {
		pe.write(w)
		return
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}
	if !reasonRE.MatchString(req.Reason) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"reason must be 1-32 lowercase letters, digits or underscores, like manual or customer_request; put free text in detail")
		return
	}
	if utf8.RuneCountInString(req.Detail) > 300 || !utf8.ValidString(req.Detail) || strings.ContainsRune(req.Detail, 0) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "detail must be text of at most 300 characters")
		return
	}
	stream, pe := suppressionStream(req.Stream, false)
	if pe != nil {
		pe.write(w)
		return
	}
	v, created, err := s.Store.AddSuppression(r.Context(), projectID(r), store.Suppression{
		Address: addr, Stream: stream, Reason: req.Reason, Detail: req.Detail,
	})
	if err != nil {
		s.internal(w, "add suppression", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.audit(r, "suppression.add", "suppression", "", map[string]any{"rcpt_domain": domainOf(addr), "stream": stream, "reason": req.Reason})
	}
	writeJSON(w, status, map[string]any{"object": "suppression", "address": v.Address, "stream": v.Stream,
		"reason": v.Reason, "detail": v.Detail, "created_at": v.CreatedAt, "created": created})
}

// deleteSuppression answers DELETE /suppressions/{address}?stream=: the
// address can be sent to again.
func (s *Server) deleteSuppression(w http.ResponseWriter, r *http.Request) {
	addr, pe := suppressionAddress(r.PathValue("address"))
	if pe != nil {
		pe.write(w)
		return
	}
	stream, pe := suppressionStream(r.URL.Query().Get("stream"), true)
	if pe != nil {
		pe.write(w)
		return
	}
	removed, err := s.Store.DeleteSuppression(r.Context(), projectID(r), addr, stream)
	if err != nil {
		s.internal(w, "delete suppression", err)
		return
	}
	if len(removed) == 0 {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("%s is not suppressed in this project", addr))
		return
	}
	s.audit(r, "suppression.delete", "suppression", "", map[string]any{"rcpt_domain": domainOf(addr), "rows": len(removed)})
	writeJSON(w, http.StatusOK, map[string]any{"object": "suppression", "address": addr, "deleted": true, "removed": removed})
}

// exportSuppressions answers GET /suppressions/export with every matching
// suppression as CSV (address, stream, reason, detail, created_at), oldest
// first.
func (s *Server) exportSuppressions(w http.ResponseWriter, r *http.Request) {
	f, pe := suppressionFilter(r)
	if pe != nil {
		pe.write(w)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="suppressions.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	cw.Write([]string{"address", "stream", "reason", "detail", "created_at"})
	err := s.Store.EachSuppression(r.Context(), f, func(v store.Suppression) error {
		return cw.Write([]string{csvCell(v.Address), v.Stream, csvCell(v.Reason), csvCell(v.Detail), v.CreatedAt.UTC().Format(time.RFC3339)})
	})
	cw.Flush()
	if err != nil {
		// The status is sent already; a cut-off file is the signal.
		s.Log.Error("export suppressions", "err", err)
	}
}

// csvCell keeps a spreadsheet from reading a value as a formula.
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
