package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/webhook"
)

type webhookResponse struct {
	Object        string                      `json:"object"`
	ID            string                      `json:"id"`
	Endpoint      string                      `json:"endpoint"`
	Events        []string                    `json:"events"`
	CreatedAt     time.Time                   `json:"created_at"`
	SigningSecret string                      `json:"signing_secret,omitempty"`
	Deliveries    []store.WebhookDeliveryView `json:"recent_deliveries,omitempty"`
}

func webhookView(w store.Webhook) webhookResponse {
	return webhookResponse{Object: "webhook", ID: w.ID, Endpoint: w.Endpoint, Events: w.Events, CreatedAt: w.CreatedAt}
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Endpoint string   `json:"endpoint"`
		Events   []string `json:"events"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if err := webhook.ValidateEndpoint(req.Endpoint, s.Cfg.WebhookAllowHTTP); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	for _, e := range req.Events {
		if !webhook.ValidType(e) {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "unknown event type "+e)
			return
		}
	}
	whsec := webhook.NewSecret()
	hook, err := s.Store.CreateWebhook(r.Context(), store.Webhook{
		ProjectID: projectID(r), Endpoint: req.Endpoint, Events: req.Events, Secret: s.Sealer.Seal([]byte(whsec)),
	})
	if err != nil {
		s.internal(w, "create webhook", err)
		return
	}
	s.audit(r, "webhook.created", "webhook", hook.ID, map[string]any{"endpoint": hook.Endpoint, "events": hook.Events}) // audit log (lane A hook)
	resp := webhookView(hook)
	resp.SigningSecret = whsec // shown once
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.Store.ListWebhooks(r.Context(), projectID(r))
	if err != nil {
		s.internal(w, "list webhooks", err)
		return
	}
	out := make([]webhookResponse, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, webhookView(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	hook, err := s.Store.GetWebhook(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err != nil {
		s.internal(w, "get webhook", err)
		return
	}
	resp := webhookView(hook)
	if resp.Deliveries, err = s.Store.RecentWebhookDeliveries(r.Context(), hook.ID, 20); err != nil {
		s.internal(w, "webhook deliveries", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	err := s.Store.DeleteWebhook(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err != nil {
		s.internal(w, "delete webhook", err)
		return
	}
	s.audit(r, "webhook.deleted", "webhook", id, nil) // audit log (lane A hook)
	writeJSON(w, http.StatusOK, map[string]any{"object": "webhook", "id": id, "deleted": true})
}
