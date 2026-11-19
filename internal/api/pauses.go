package api

// Circuit-breaker pauses and project settings (lane B).
//
// A breaker trips when an API key, a sending domain or the whole project
// has a bounce or complaint rate over the project's thresholds, over at
// least min_recipients recipients in 24 hours. The migration's trigger
// keeps the counts and stores the pause; here a full_access key lists and
// resumes pauses, and reads and changes the thresholds and the
// per-recipient cap.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
)

type pauseResponse struct {
	Object string `json:"object"`
	store.Pause
	Active bool `json:"active"`
}

func pauseView(p store.Pause) pauseResponse {
	return pauseResponse{Object: "pause", Pause: p, Active: p.ResumedAt == nil}
}

// listPauses answers GET /pauses?active=true&limit=N, newest first.
func (s *Server) listPauses(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "limit must be a number from 1 to 100")
			return
		}
		limit = n
	}
	active := false
	if v := q.Get("active"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "active must be true or false")
			return
		}
		active = b
	}
	list, err := s.Store.ListPauses(r.Context(), projectID(r), active, limit)
	if err != nil {
		s.internal(w, "list pauses", err)
		return
	}
	data := make([]pauseResponse, len(list))
	for i, p := range list {
		data[i] = pauseView(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// resumePause answers POST /pauses/{id}/resume. The scope's counters start
// over, so it is not paused again by the same bounces.
func (s *Server) resumePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "pause not found")
		return
	}
	p, err := s.Store.ResumePause(r.Context(), projectID(r), id, apiKeyOf(r).ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "pause not found; GET /pauses lists them")
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "not_paused", "this pause was already resumed; GET /pauses?active=true lists the active ones")
		return
	case err != nil:
		s.internal(w, "resume pause", err)
		return
	}
	s.audit(r, "pause.resume", "pause", p.ID, map[string]any{"scope": p.Scope, "scope_id": p.ScopeID, "reason": p.Reason})
	writeJSON(w, http.StatusOK, pauseView(p))
}

type breakerSettings struct {
	Enabled       bool    `json:"enabled"`
	BounceRate    float64 `json:"bounce_rate"`
	ComplaintRate float64 `json:"complaint_rate"`
	MinRecipients int     `json:"min_recipients"`
}

type projectResponse struct {
	Object                string          `json:"object"`
	ID                    string          `json:"id"`
	Name                  string          `json:"name"`
	MaxPerRecipientPerDay *int            `json:"max_per_recipient_per_day"`
	CircuitBreaker        breakerSettings `json:"circuit_breaker"`
	UpdatedAt             *time.Time      `json:"updated_at"`
}

func projectView(p store.ProjectSettings) projectResponse {
	return projectResponse{
		Object: "project", ID: p.ProjectID, Name: p.Name, MaxPerRecipientPerDay: p.MaxPerRecipientPerDay,
		CircuitBreaker: breakerSettings{Enabled: p.BreakerEnabled, BounceRate: p.BounceRate,
			ComplaintRate: p.ComplaintRate, MinRecipients: p.MinRecipients},
		UpdatedAt: p.UpdatedAt,
	}
}

// getProject answers GET /project: the project's sending settings.
func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProjectSettings(r.Context(), projectID(r))
	if err != nil {
		s.internal(w, "project settings", err)
		return
	}
	writeJSON(w, http.StatusOK, projectView(p))
}

// updateProject answers PATCH /project. Fields left out keep their value;
// "max_per_recipient_per_day": null turns the cap off.
func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		MaxPerRecipientPerDay json.RawMessage `json:"max_per_recipient_per_day"`
		CircuitBreaker        *struct {
			Enabled       *bool    `json:"enabled"`
			BounceRate    *float64 `json:"bounce_rate"`
			ComplaintRate *float64 `json:"complaint_rate"`
			MinRecipients *int     `json:"min_recipients"`
		} `json:"circuit_breaker"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	ctx := r.Context()
	p, err := s.Store.GetProjectSettings(ctx, projectID(r))
	if err != nil {
		s.internal(w, "project settings", err)
		return
	}
	changed := map[string]any{}
	if req.MaxPerRecipientPerDay != nil {
		if string(req.MaxPerRecipientPerDay) == "null" {
			p.MaxPerRecipientPerDay = nil
		} else {
			var n int
			if err := json.Unmarshal(req.MaxPerRecipientPerDay, &n); err != nil || n < 1 || n > 10000 {
				writeError(w, http.StatusUnprocessableEntity, "validation_error",
					"max_per_recipient_per_day must be a whole number from 1 to 10000, or null for no cap")
				return
			}
			p.MaxPerRecipientPerDay = &n
		}
		changed["max_per_recipient_per_day"] = "off"
		if p.MaxPerRecipientPerDay != nil {
			changed["max_per_recipient_per_day"] = *p.MaxPerRecipientPerDay
		}
	}
	if b := req.CircuitBreaker; b != nil {
		rate := func(name string, v *float64, dst *float64) bool {
			if v == nil {
				return true
			}
			if math.IsNaN(*v) || *v <= 0 || *v > 1 {
				writeError(w, http.StatusUnprocessableEntity, "validation_error", fmt.Sprintf(
					"circuit_breaker.%s must be a fraction above 0 and at most 1 (0.05 is 5%%)", name))
				return false
			}
			*dst = *v
			changed["circuit_breaker."+name] = *v
			return true
		}
		if !rate("bounce_rate", b.BounceRate, &p.BounceRate) || !rate("complaint_rate", b.ComplaintRate, &p.ComplaintRate) {
			return
		}
		if b.MinRecipients != nil {
			if *b.MinRecipients < 1 || *b.MinRecipients > 1_000_000 {
				writeError(w, http.StatusUnprocessableEntity, "validation_error",
					"circuit_breaker.min_recipients must be from 1 to 1000000")
				return
			}
			p.MinRecipients = *b.MinRecipients
			changed["circuit_breaker.min_recipients"] = p.MinRecipients
		}
		if b.Enabled != nil {
			p.BreakerEnabled = *b.Enabled
			changed["circuit_breaker.enabled"] = p.BreakerEnabled
		}
	}
	if err := s.Store.SaveProjectSettings(ctx, p); err != nil {
		s.internal(w, "save project settings", err)
		return
	}
	s.audit(r, "project.update", "project", p.ProjectID, changed)
	if p, err = s.Store.GetProjectSettings(ctx, p.ProjectID); err != nil {
		s.internal(w, "project settings", err)
		return
	}
	writeJSON(w, http.StatusOK, projectView(p))
}
