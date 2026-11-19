package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/webhook"
)

type webhookResponse struct {
	Object    string    `json:"object"`
	ID        string    `json:"id"`
	Endpoint  string    `json:"endpoint"`
	Events    []string  `json:"events"`
	CreatedAt time.Time `json:"created_at"`

	// Shown once: on creation and after a rotation.
	SigningSecret string `json:"signing_secret,omitempty"`

	// Ed25519 ("v1a") signing: the public key a receiver verifies with,
	// and during a rotation's overlap the previous one too.
	AsymmetricSigning bool   `json:"asymmetric_signing"`
	PublicKey         string `json:"public_key,omitempty"`
	PreviousPublicKey string `json:"previous_public_key,omitempty"`

	// Rotation: until when the previous secret (and key) still sign.
	PreviousSecretExpiresAt *time.Time `json:"previous_secret_expires_at,omitempty"`
	SecretRotatedAt         *time.Time `json:"secret_rotated_at,omitempty"`

	// Circuit breaker: consecutive failed attempts, and until when the
	// endpoint gets no requests.
	ConsecutiveFailures int        `json:"consecutive_failures"`
	PausedUntil         *time.Time `json:"paused_until,omitempty"`

	Deliveries []store.WebhookDeliveryView `json:"recent_deliveries,omitempty"`
}

func webhookView(w store.Webhook) webhookResponse {
	now := time.Now()
	v := webhookResponse{
		Object: "webhook", ID: w.ID, Endpoint: w.Endpoint, Events: w.Events, CreatedAt: w.CreatedAt,
		AsymmetricSigning: len(w.Ed25519Public) > 0, PublicKey: webhook.PublicKeyString(w.Ed25519Public),
		SecretRotatedAt: w.SecretRotatedAt, ConsecutiveFailures: w.Failures,
	}
	if w.InOverlap(now) {
		v.PreviousSecretExpiresAt = w.PreviousExpiresAt
		v.PreviousPublicKey = webhook.PublicKeyString(w.Ed25519PreviousPublic)
	}
	if w.BreakerUntil != nil && w.BreakerUntil.After(now) {
		v.PausedUntil = w.BreakerUntil
	}
	return v
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Endpoint          string   `json:"endpoint"`
		Events            []string `json:"events"`
		AsymmetricSigning bool     `json:"asymmetric_signing"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if err := webhook.ValidateEndpointFor(req.Endpoint, s.Cfg.WebhookAllowHTTP, s.Cfg.WebhookAllowPrivate); err != nil {
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
	hook := store.Webhook{ProjectID: projectID(r), Endpoint: req.Endpoint, Events: req.Events, Secret: s.Sealer.Seal([]byte(whsec))}
	if req.AsymmetricSigning {
		seed, pub := webhook.NewEd25519Key()
		hook.Ed25519Key, hook.Ed25519Public = s.Sealer.Seal(seed), pub
	}
	hook, err := s.Store.CreateWebhook(r.Context(), hook, s.webhooksPerProject())
	if errors.Is(err, store.ErrLimitReached) {
		writeError(w, http.StatusConflict, "webhook_limit_reached", fmt.Sprintf(
			"this project already has %d webhooks, the most allowed (OMW_WEBHOOKS_PER_PROJECT). "+
				"Delete one with DELETE /webhooks/{id}, or send several event types to one endpoint", s.webhooksPerProject()))
		return
	}
	if err != nil {
		s.internal(w, "create webhook", err)
		return
	}
	s.audit(r, "webhook.created", "webhook", hook.ID, map[string]any{"endpoint": hook.Endpoint, "events": hook.Events}) // audit log (lane A hook)
	resp := webhookView(hook)
	resp.SigningSecret = whsec // shown once
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) webhooksPerProject() int {
	if s.Cfg.WebhooksPerProject > 0 {
		return s.Cfg.WebhooksPerProject
	}
	return 10
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

// Overlap of a webhook secret rotation: how long the previous secret keeps
// signing next to the new one.
const (
	defaultRotationOverlap = 24 * time.Hour
	maxRotationOverlap     = 7 * 24 * time.Hour
)

// rotateWebhookSecret answers POST /webhooks/{id}/rotate-secret
// {"overlap_seconds"?}. It returns the new signing secret once. Until the
// overlap ends, every request carries signatures by both secrets (and both
// Ed25519 keys), so receivers can switch at any moment within it.
func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		OverlapSeconds *int64 `json:"overlap_seconds"`
	}
	if len(body) > 0 {
		if err := decodeStrict(body, &req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
			return
		}
	}
	overlap := defaultRotationOverlap
	if req.OverlapSeconds != nil {
		if *req.OverlapSeconds < 0 || *req.OverlapSeconds > int64(maxRotationOverlap/time.Second) {
			writeError(w, http.StatusUnprocessableEntity, "validation_error",
				"overlap_seconds must be between 0 and "+strconv.Itoa(int(maxRotationOverlap/time.Second))+" (7 days)")
			return
		}
		overlap = time.Duration(*req.OverlapSeconds) * time.Second
	}
	whsec := webhook.NewSecret()
	seed, pub := webhook.NewEd25519Key()
	hook, err := s.Store.RotateWebhookSecret(r.Context(), projectID(r), id, store.WebhookRotation{
		Secret: s.Sealer.Seal([]byte(whsec)), Ed25519Key: s.Sealer.Seal(seed), Ed25519Public: pub,
		PreviousUntil: time.Now().Add(overlap),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err != nil {
		s.internal(w, "rotate webhook secret", err)
		return
	}
	s.audit(r, "webhook.secret_rotated", "webhook", hook.ID, map[string]any{"overlap": overlap.String()})
	s.Log.Info("webhook secret rotated", "project", hook.ProjectID, "webhook", hook.ID, "overlap", overlap.String())
	resp := webhookView(hook)
	resp.SigningSecret = whsec // shown once
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
