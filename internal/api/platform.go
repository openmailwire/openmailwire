package api

import (
	"net/http"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
)

// platformRoutes registers lane E's endpoints: webhook secret rotation
// and delivery insights.
func (s *Server) platformRoutes(mux *http.ServeMux) {
	mux.Handle("POST /webhooks/{id}/rotate-secret", s.withFullAccess(s.rotateWebhookSecret))
	mux.Handle("GET /insights", s.withFullAccess(s.getInsights))
}

// insightsSlots bounds how many insights queries run at once, server-wide.
var insightsSlots = make(chan struct{}, 2)

// getInsights answers GET /insights: acceptance, deferral and bounce rates
// and time to acceptance, per mailbox provider and per sending IP, for the
// last 24 hours, 7 days and 30 days.
func (s *Server) getInsights(w http.ResponseWriter, r *http.Request) {
	select {
	case insightsSlots <- struct{}{}:
		defer func() { <-insightsSlots }()
	case <-r.Context().Done():
		return
	}
	in, err := s.Store.DeliveryInsights(r.Context(), projectID(r), store.InsightsMaxEmails)
	if err != nil {
		s.internal(w, "insights", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":       "insights",
		"generated_at": time.Now().UTC(),
		// Emails count in a window by when they were sent; providers are
		// recognised by the MX host each attempt talked to.
		"window_basis":      "sent_at",
		"provider_grouping": "mx_host_suffix",
		"emails_considered": in.EmailsConsidered,
		"truncated":         in.Truncated,
		"windows":           in.Windows,
	})
}
