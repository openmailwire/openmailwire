// Package api is the control plane's HTTP interface: the public sending API
// (Resend-compatible where it can be) and the internal worker API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/domains"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

type Server struct {
	Store    *store.Store
	Sealer   *secret.Sealer
	MACKey   []byte // signs bounce (VERP) addresses
	Resolver domains.Resolver
	Cfg      config.Control
	Log      *slog.Logger
	Version  string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)

	mux.Handle("POST /emails", s.withProject(s.sendEmail))
	mux.Handle("GET /emails/{id}", s.withProject(s.getEmail))
	mux.Handle("POST /domains", s.withProject(s.createDomain))
	mux.Handle("GET /domains", s.withProject(s.listDomains))
	mux.Handle("GET /domains/{id}", s.withProject(s.getDomain))
	mux.Handle("POST /domains/{id}/verify", s.withProject(s.verifyDomain))
	mux.Handle("DELETE /domains/{id}", s.withProject(s.deleteDomain))
	mux.Handle("POST /webhooks", s.withProject(s.createWebhook))
	mux.Handle("GET /webhooks", s.withProject(s.listWebhooks))
	mux.Handle("GET /webhooks/{id}", s.withProject(s.getWebhook))
	mux.Handle("DELETE /webhooks/{id}", s.withProject(s.deleteWebhook))

	mux.Handle("POST /internal/worker/heartbeat", s.withWorker(s.heartbeat))
	mux.Handle("POST /internal/worker/lease", s.withWorker(s.lease))
	mux.Handle("POST /internal/worker/report", s.withWorker(s.report))

	var root http.Handler
	mux.Handle("/mcp", s.mcpHandler(func() http.Handler { return root }))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	root = http.MaxBytesHandler(mux, 4<<20)
	return root
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})
}

type ctxKey int

const (
	projectKey ctxKey = iota
	workerKey
)

func bearer(r *http.Request) string {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(t)
}

func (s *Server) withProject(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if !strings.HasPrefix(token, "omw_") {
			writeError(w, http.StatusUnauthorized, "missing_api_key", "send an API key as Authorization: Bearer omw_…")
			return
		}
		key, err := s.Store.KeyForToken(r.Context(), secret.Hash(token))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "API key is invalid or revoked")
			return
		}
		if err != nil {
			s.internal(w, "api key lookup", err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), projectKey, key)))
	})
}

func (s *Server) withWorker(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if !strings.HasPrefix(token, "omwk_") {
			writeError(w, http.StatusUnauthorized, "missing_worker_token", "send a worker token as Authorization: Bearer omwk_…")
			return
		}
		wk, err := s.Store.WorkerForToken(r.Context(), secret.Hash(token))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_worker_token", "worker token is invalid")
			return
		}
		if err != nil {
			s.internal(w, "worker lookup", err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), workerKey, wk)))
	})
}

// apiKeyOf returns the API key that authenticated r.
func apiKeyOf(r *http.Request) store.APIKey { return r.Context().Value(projectKey).(store.APIKey) }

func projectID(r *http.Request) string { return apiKeyOf(r).ProjectID }

func workerOf(r *http.Request) store.Worker { return r.Context().Value(workerKey).(store.Worker) }

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validID(id string) bool { return uuidRE.MatchString(id) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// writeError uses Resend's error shape so existing client code can read it.
func writeError(w http.ResponseWriter, status int, name, message string) {
	writeJSON(w, status, map[string]any{"statusCode": status, "name": name, "message": message})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
