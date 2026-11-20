// Package api is the control plane's HTTP interface: the public sending API
// (Resend-compatible where it can be) and the internal worker API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/openmailwire/openmailwire/internal/apidocs"
	"github.com/openmailwire/openmailwire/internal/apikey"
	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/dashboard"
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

	limiter *keyLimiter // per-key request rate; nil when off
	large   slots       // requests with bodies over largeBody
	safety  safetyState // lane B: in-memory state (unsubscribe rate limit)
	dns     dnsState    // lane G: per-domain run locks, operator SPF sync (dnsauto.go)
}

func (s *Server) Handler() http.Handler {
	s.limiter = newKeyLimiter(s.Cfg.APILimits)
	s.large = newSlots(s.Cfg.LargeRequests)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("GET /openapi.json", apidocs.Handler())
	mux.Handle("GET /llms.txt", apidocs.Handler())

	// withProject admits every key, sending_access included; withFullAccess
	// only full_access keys.
	mux.Handle("POST /emails", s.withProject(s.sendEmail))
	mux.Handle("POST /emails/batch", s.withProject(s.sendBatch))
	mux.Handle("POST /emails/raw", s.withProject(s.sendRaw))
	mux.Handle("GET /emails", s.withProject(s.listEmails))
	mux.Handle("GET /emails/{id}", s.withProject(s.getEmail))
	mux.Handle("POST /emails/{id}/cancel", s.withProject(s.cancelEmail))
	mux.Handle("POST /domains", s.withFullAccess(s.createDomain))
	mux.Handle("GET /domains", s.withProject(s.listDomains))
	mux.Handle("GET /domains/{id}", s.withProject(s.getDomain))
	mux.Handle("POST /domains/{id}/verify", s.withFullAccess(s.verifyDomain))
	mux.Handle("DELETE /domains/{id}", s.withFullAccess(s.deleteDomain))
	mux.Handle("POST /webhooks", s.withFullAccess(s.createWebhook))
	mux.Handle("GET /webhooks", s.withFullAccess(s.listWebhooks))
	mux.Handle("GET /webhooks/{id}", s.withFullAccess(s.getWebhook))
	mux.Handle("DELETE /webhooks/{id}", s.withFullAccess(s.deleteWebhook))
	mux.Handle("POST /api-keys", s.withFullAccess(s.createAPIKey))
	mux.Handle("GET /api-keys", s.withFullAccess(s.listAPIKeys))
	mux.Handle("GET /api-keys/{id}", s.withFullAccess(s.getAPIKey))
	mux.Handle("PATCH /api-keys/{id}", s.withFullAccess(s.updateAPIKey))
	mux.Handle("DELETE /api-keys/{id}", s.withFullAccess(s.deleteAPIKey))
	mux.Handle("GET /audit-log", s.withFullAccess(s.listAuditLog))
	mux.Handle("GET /audit-log/verify", s.withFullAccess(s.verifyAuditLog))

	mux.Handle("POST /internal/worker/heartbeat", s.withWorker(s.heartbeat))
	mux.Handle("POST /internal/worker/lease", s.withWorker(s.lease))
	mux.Handle("POST /internal/worker/report", s.withWorker(s.report))
	s.dkimRoutes(mux) // lane C: DKIM rotation and Ed25519 (dkim.go)
	s.dnsRoutes(mux)  // lane G: automatic DNS setup with Cloudflare (dnsauto.go)

	var root http.Handler
	mux.Handle("/mcp", s.mcpHandler(func() http.Handler { return root }))
	s.inboxRoutes(mux, func() http.Handler { return root })
	s.safetyRoutes(mux)   // lane B: approvals, suppressions, unsubscribe, pauses, project, check (safety.go)
	s.platformRoutes(mux) // lane E: webhook secret rotation, insights (platform.go)
	mux.Handle("GET "+dashboard.Prefix, dashboard.Handler())
	mux.Handle("GET "+dashboard.Prefix+"/", dashboard.Handler())

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	root = secureHeaders(s.limitBody(mux))
	return root
}

// limitBody caps request bodies: 4 MB, or more on routes that carry whole
// messages. A body over largeBody also needs one of the server's large
// request slots (OMW_MAX_LARGE_REQUESTS), so that a burst of big sends
// cannot take all the memory.
func (s *Server) limitBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := bodyLimit(r) // per-route limits for the public API (send.go)
		if r.URL.Path == "/internal/inbound" {
			n = s.inboundBodyLimit()
		}
		r2 := *r
		r2.Body = http.MaxBytesReader(w, r.Body, n)
		if r.Body != nil && r.Body != http.NoBody && !isHop(r.Context()) {
			switch {
			case r.ContentLength > largeBody:
				release, ok := s.large.acquire(r.Context())
				if !ok {
					writeBusy(w)
					return
				}
				defer release()
			case r.ContentLength < 0 && n > largeBody:
				lazy := &lazySlot{ReadCloser: r2.Body, slots: s.large, ctx: r.Context()}
				defer lazy.done()
				r2.Body = lazy
			}
		}
		h.ServeHTTP(w, &r2)
	})
}

// health answers whether the control plane can reach its database. The
// version is shown only to a caller with a valid API key or worker token.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database is not reachable")
		return
	}
	out := map[string]string{"status": "ok"}
	token := bearer(r)
	var err error
	switch {
	case strings.HasPrefix(token, "omw_"):
		var k store.APIKey
		if k, err = s.Store.KeyForToken(r.Context(), secret.Hash(token)); err == nil && k.Expired(time.Now()) {
			err = store.ErrNotFound
		}
	case strings.HasPrefix(token, "omwk_"):
		_, err = s.Store.WorkerForToken(r.Context(), secret.Hash(token))
	default:
		err = store.ErrNotFound
	}
	if err == nil {
		out["version"] = s.Version
	}
	writeJSON(w, http.StatusOK, out)
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
		if key.Expired(time.Now()) {
			writeError(w, http.StatusUnauthorized, "api_key_expired", fmt.Sprintf(
				"this API key expired at %s; ask the project's owner for a new key (POST /api-keys)", key.ExpiresAt.UTC().Format(time.RFC3339)))
			return
		}
		ip := s.clientIP(r)
		if !apikey.AllowsIP(key.AllowedIPs, ip) {
			from := "an unknown address"
			if ip.IsValid() {
				from = ip.String()
			}
			writeError(w, http.StatusForbidden, "ip_not_allowed", fmt.Sprintf(
				"this API key may be used only from its allowed_ips, and this request came from %s; call from an allowed network, or ask the key's owner to add yours", from))
			return
		}
		if s.limiter != nil && !isHop(r.Context()) {
			if ok, wait := s.limiter.allow(key.ID, time.Now()); !ok {
				s.limiter.refusal(wait).write(w)
				return
			}
		}
		ctx := context.WithValue(r.Context(), projectKey, key)
		ctx = context.WithValue(ctx, clientIPKey{}, ip)
		next(w, r.WithContext(ctx))
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
		if !workerRoleAllowed(w, r, wk) { // lane C: token roles (workers.go)
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
	if unstorable(err) {
		s.Log.Warn(what+": unstorable text refused", "err", err)
		errUnstorable.write(w)
		return
	}
	s.internalErr(what, err).write(w)
}

// unstorable reports whether the database refused err's text: a NUL, bytes
// that are not UTF-8 (SQLSTATE 22021), or a \u0000 in JSON (22P05). The
// request has to change, so it is the caller's 422, never a 500 that
// clients (and SMTP servers) retry forever.
func unstorable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "22021" || pe.Code == "22P05")
}

var errUnstorable = &policyError{Status: http.StatusUnprocessableEntity, Name: "validation_error",
	Message: "the request contains text that cannot be stored: a NUL character or bytes that are not UTF-8; remove them and send it again"}
