package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/dkimkey"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

// unsealRetry is how long a delivery whose message could not be unsealed
// waits before it is leased again.
const unsealRetry = 5 * time.Minute

// decodeWorker reads a worker API body. Unknown fields are ignored, so a
// worker newer than the control plane can still report (version skew).
func decodeWorker(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// workerRoleAllowed checks that the token's role may call r's endpoint:
// inbound nodes only the inbound endpoints, delivery and relay workers only
// the others. Tokens created before roles existed ("any") may call both.
// It answers 403 wrong_worker_role and returns false otherwise. Called by
// withWorker (server.go).
func workerRoleAllowed(w http.ResponseWriter, r *http.Request, wk store.Worker) bool {
	inbound := strings.HasPrefix(r.URL.Path, "/internal/inbound")
	switch {
	case wk.Role == "" || wk.Role == protocol.RoleAny:
		return true
	case inbound && wk.Role == protocol.RoleInbound, !inbound && wk.Role != protocol.RoleInbound:
		return true
	case inbound:
		writeError(w, http.StatusForbidden, "wrong_worker_role", "this token belongs to a "+wk.Role+
			" worker, which cannot hand over received mail; inbound nodes need a token created with `openmailwire workers add <name> --role inbound`")
	default:
		writeError(w, http.StatusForbidden, "wrong_worker_role", "this token belongs to an inbound node, which cannot deliver mail;"+
			" delivery workers need a token created with `openmailwire workers add <name> --role delivery` (or --role relay)")
	}
	return false
}

// kindAllowed checks that a delivery token leases as a direct worker and a
// relay token as a relay.
func kindAllowed(w http.ResponseWriter, wk store.Worker, kind string) bool {
	relay := kind == protocol.KindRelay
	switch {
	case wk.Role == protocol.RoleDelivery && relay:
		writeError(w, http.StatusForbidden, "wrong_worker_role", "this token is for a delivery worker, not a relay;"+
			" create a relay token with `openmailwire workers add <name> --role relay`")
		return false
	case wk.Role == protocol.RoleRelay && !relay:
		writeError(w, http.StatusForbidden, "wrong_worker_role", "this token is for a relay worker; set OMW_RELAY_HOST on it,"+
			" or create a delivery token with `openmailwire workers add <name> --role delivery`")
		return false
	}
	return true
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req protocol.HeartbeatRequest
	if err := decodeWorker(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if req.PublicIP != "" && net.ParseIP(req.PublicIP) == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "public_ip is not an IP address")
		return
	}
	if !protocol.ValidKind(req.Kind) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", `kind must be "direct" or "relay"`)
		return
	}
	if !kindAllowed(w, workerOf(r), req.Kind) {
		return
	}
	draining, err := s.Store.Heartbeat(r.Context(), workerOf(r).ID, req)
	if err != nil {
		s.internal(w, "heartbeat", err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.HeartbeatResponse{Draining: draining})
}

func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req protocol.LeaseRequest
	if err := decodeWorker(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if !protocol.ValidKind(req.Kind) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", `kind must be "direct" or "relay"`)
		return
	}
	wk := workerOf(r)
	if !kindAllowed(w, wk, req.Kind) {
		return
	}
	resp := protocol.LeaseResponse{Jobs: []protocol.Job{}}
	if wk.Draining || req.Max < 1 {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if req.Max > 100 {
		req.Max = 100
	}
	ctx := r.Context()
	jobs, err := s.Store.LeaseJobs(ctx, store.LeaseOptions{
		WorkerID: wk.ID, Kind: req.Kind, Max: req.Max, Lease: s.Cfg.LeaseDuration,
		DomainConcurrency: s.Cfg.DomainConcurrency, RelayAfterAttempts: s.Cfg.RelayAfterAttempts,
		Rules: s.Cfg.ProviderRules, Warmup: s.Cfg.Warmup,
	})
	if err != nil {
		s.internal(w, "lease", err)
		return
	}
	keys := map[string]*domainKeys{} // per sending domain, for re-signing
	for _, j := range jobs {
		raw, err := s.Sealer.Open(j.SealedRaw)
		if err != nil {
			// Nothing was sent: give the lease back at once instead of
			// letting it expire into an "unknown" attempt.
			s.Log.Error("unseal message; the delivery is released", "delivery", j.DeliveryID, "err", err)
			if err := s.Store.ReleaseLease(context.WithoutCancel(ctx), j.AttemptID,
				"the control plane could not decrypt the message (is OMW_SECRET_KEY the key it was queued with?); it will try again",
				unsealRetry); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.Log.Error("release lease", "delivery", j.DeliveryID, "err", err)
			}
			continue
		}
		if j.Scheduled && j.DomainID != "" {
			raw = s.redate(ctx, keys, j, raw)
		}
		job := j.Job
		job.Raw = raw
		job.MailFrom = s.bounceAddress(j.DeliveryID, j.ReturnPath)
		resp.Jobs = append(resp.Jobs, job)
	}
	writeJSON(w, http.StatusOK, resp)
}

// domainKeys is a sending domain's name and signing keys, loaded once per
// lease.
type domainKeys struct {
	name string
	keys []dkimkey.Key
	err  error
}

// redate gives a scheduled message the Date of its sending and a fresh
// DKIM signature with the domain's current keys: it was rendered and signed
// when it was queued, up to 30 days earlier. Nothing else in the message
// changes. If the domain or its keys are gone, the message goes out as it
// was queued.
func (s *Server) redate(ctx context.Context, cache map[string]*domainKeys, j store.LeasedJob, raw []byte) []byte {
	dk, ok := cache[j.DomainID]
	if !ok {
		dk = &domainKeys{}
		cache[j.DomainID] = dk
		dom, err := s.Store.DomainByID(ctx, j.DomainID)
		if err == nil {
			dk.name = dom.Name
			dk.keys, err = s.dkimSigningKeys(ctx, dom)
		}
		dk.err = err
	}
	if dk.err != nil {
		s.Log.Warn("scheduled message sent with the date and signature of its queuing", "delivery", j.DeliveryID, "err", dk.err)
		return raw
	}
	out, err := dkimkey.Resign(raw, dk.name, dk.keys, time.Now())
	if err != nil {
		s.Log.Warn("scheduled message sent with the date and signature of its queuing", "delivery", j.DeliveryID, "err", err)
		return raw
	}
	return out
}

// bounceAddress is the envelope sender (VERP): it names the delivery and
// carries an HMAC, so a bounce processor can trust which delivery a bounce
// is about and ignore forged ones.
func (s *Server) bounceAddress(deliveryID, returnPath string) string {
	compact := strings.ReplaceAll(deliveryID, "-", "")
	return "b-" + compact + "-" + secret.MAC(s.MACKey, compact) + "@" + returnPath
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var rep protocol.Report
	if err := decodeWorker(body, &rep); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if !validID(rep.AttemptID) || !protocol.ValidOutcome(rep.Outcome) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "attempt_id or outcome is invalid")
		return
	}
	status, err := s.Store.ReportAttempt(r.Context(), workerOf(r).ID, rep,
		store.ReportOptions{DomainConcurrency: s.Cfg.DomainConcurrency, Rules: s.Cfg.ProviderRules})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "attempt not found")
	case errors.Is(err, store.ErrForbidden):
		// 409, not 403: workers keep reports the control plane refuses with
		// 401 or 403 (a token problem the operator can fix), and this one can
		// never succeed.
		writeError(w, http.StatusConflict, "attempt_of_another_worker", "this attempt was leased to another worker; drop this report")
	case err != nil:
		s.internal(w, "report", err)
	default:
		writeJSON(w, http.StatusOK, protocol.ReportResponse{Status: status})
	}
}
