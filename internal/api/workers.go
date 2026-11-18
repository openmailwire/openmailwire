package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req protocol.HeartbeatRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if req.PublicIP != "" && net.ParseIP(req.PublicIP) == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "public_ip is not an IP address")
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
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	wk := workerOf(r)
	resp := protocol.LeaseResponse{Jobs: []protocol.Job{}}
	if wk.Draining || req.Max < 1 {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if req.Max > 100 {
		req.Max = 100
	}
	jobs, err := s.Store.Lease(r.Context(), wk.ID, req.Max, s.Cfg.LeaseDuration, s.Cfg.DomainConcurrency)
	if err != nil {
		s.internal(w, "lease", err)
		return
	}
	for _, j := range jobs {
		raw, err := s.Sealer.Open(j.SealedRaw)
		if err != nil {
			// The lease will expire and the sweeper will reschedule it.
			s.Log.Error("unseal message", "delivery", j.DeliveryID, "err", err)
			continue
		}
		job := j.Job
		job.Raw = raw
		job.MailFrom = s.bounceAddress(j.DeliveryID, j.ReturnPath)
		resp.Jobs = append(resp.Jobs, job)
	}
	writeJSON(w, http.StatusOK, resp)
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
	if err := decodeStrict(body, &rep); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if !validID(rep.AttemptID) || !protocol.ValidOutcome(rep.Outcome) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "attempt_id or outcome is invalid")
		return
	}
	status, err := s.Store.Report(r.Context(), workerOf(r).ID, rep)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "attempt not found")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "this attempt was leased to another worker")
	case err != nil:
		s.internal(w, "report", err)
	default:
		writeJSON(w, http.StatusOK, protocol.ReportResponse{Status: status})
	}
}
