package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/dkimkey"
	"github.com/openmailwire/openmailwire/internal/domains"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

type domainResponse struct {
	Object     string     `json:"object"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	VerifiedAt *time.Time `json:"verified_at"`
	CheckedAt  *time.Time `json:"checked_at"`
	// ClaimExpiresAt is when an unverified domain is removed; null once
	// verified.
	ClaimExpiresAt *time.Time       `json:"claim_expires_at"`
	Records        []domains.Record `json:"records"`
}

func (s *Server) spec(ctx context.Context, d store.Domain) domains.Spec {
	var mechanisms []string
	if s.Cfg.SPFInclude != "" {
		mechanisms = []string{"include:" + s.Cfg.SPFInclude}
	} else {
		ips, err := s.Store.WorkerIPs(ctx)
		if err != nil {
			s.Log.Warn("worker ips", "err", err)
		}
		for _, ip := range ips {
			if strings.Contains(ip, ":") {
				mechanisms = append(mechanisms, "ip6:"+ip)
			} else {
				mechanisms = append(mechanisms, "ip4:"+ip)
			}
		}
	}
	return domains.Spec{
		Domain: d.Name, Selector: d.Selector, DKIMPublic: d.DKIMPublic,
		ReturnPath: d.ReturnPath, BounceMX: s.Cfg.BounceMX, InboundMX: s.Cfg.InboundMX, SPFMechanisms: mechanisms,
	}
}

// view shows the records to publish, with the statuses from the last check
// where the expected value has not changed since.
func (s *Server) view(ctx context.Context, d store.Domain) domainResponse {
	recs := domains.Records(s.spec(ctx, d))
	var checked []domains.Record
	if json.Unmarshal(d.Checks, &checked) == nil {
		for i := range recs {
			for _, c := range checked {
				if c.Record == recs[i].Record && c.Name == recs[i].Name && c.Value == recs[i].Value {
					recs[i].Status, recs[i].Detail = c.Status, c.Detail
				}
			}
		}
	}
	return domainResponse{
		Object: "domain", ID: d.ID, Name: d.Name, Status: d.Status, CreatedAt: d.CreatedAt,
		VerifiedAt: d.VerifiedAt, CheckedAt: d.CheckedAt, ClaimExpiresAt: d.ClaimExpiresAt, Records: recs,
	}
}

func (s *Server) createDomain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	name, err := domains.Normalize(req.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	key, err := dkimkey.Generate()
	if err != nil {
		s.internal(w, "dkim keygen", err)
		return
	}
	der, err := dkimkey.MarshalPrivate(key)
	if err != nil {
		s.internal(w, "dkim marshal", err)
		return
	}
	pub, err := dkimkey.PublicBase64(key)
	if err != nil {
		s.internal(w, "dkim public", err)
		return
	}
	d, err := s.Store.CreateDomain(ctx, store.Domain{
		ProjectID:   projectID(r),
		Name:        name,
		Selector:    "omw" + time.Now().UTC().Format("200601") + secret.RandomHex(2),
		DKIMPublic:  pub,
		DKIMPrivate: s.Sealer.Seal(der),
		ReturnPath:  s.Cfg.ReturnPathLabel + "." + name,
	})
	if errors.Is(err, store.ErrConflict) {
		// Only this project's own domains conflict: other projects' names
		// are never revealed.
		writeError(w, http.StatusConflict, "domain_exists", "this project already has this domain; find it with GET /domains")
		return
	}
	if err != nil {
		s.internal(w, "create domain", err)
		return
	}
	s.audit(r, "domain.created", "domain", d.ID, map[string]any{"name": d.Name})
	writeJSON(w, http.StatusCreated, s.view(ctx, d))
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Store.ListDomains(r.Context(), projectID(r))
	if err != nil {
		s.internal(w, "list domains", err)
		return
	}
	out := make([]domainResponse, 0, len(ds))
	for _, d := range ds {
		out = append(out, s.view(r.Context(), d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) loadDomain(w http.ResponseWriter, r *http.Request) (store.Domain, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "domain not found")
		return store.Domain{}, false
	}
	d, err := s.Store.GetDomain(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "domain not found")
		return d, false
	}
	if err != nil {
		s.internal(w, "get domain", err)
		return d, false
	}
	return d, true
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request) {
	if d, ok := s.loadDomain(w, r); ok {
		writeJSON(w, http.StatusOK, s.view(r.Context(), d))
	}
}

func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	recs, verified := domains.Check(ctx, s.Resolver, s.spec(ctx, d))
	status := "pending"
	if verified {
		status = "verified"
	} else {
		for _, rec := range recs {
			if rec.Required && rec.Status == domains.StatusFailed {
				status = "failed"
			}
		}
	}
	checks, _ := json.Marshal(recs)
	err := s.Store.SaveDomainCheck(r.Context(), d.ID, status, checks)
	if errors.Is(err, store.ErrConflict) {
		// The DNS records are in place, so the caller controls the domain,
		// but another project verified the same name first.
		if err := s.Store.SaveDomainCheck(r.Context(), d.ID, "pending", checks); err != nil {
			s.internal(w, "save domain check", err)
			return
		}
		writeError(w, http.StatusConflict, "domain_verified_elsewhere", fmt.Sprintf(
			"another project has already verified %s, and a domain can be verified in only one project at a time; delete it from that project (or ask your OpenMailWire operator), then verify again", d.Name))
		return
	}
	if err != nil {
		s.internal(w, "save domain check", err)
		return
	}
	was := d.Status
	d, err = s.Store.GetDomain(r.Context(), d.ProjectID, d.ID)
	if err != nil {
		s.internal(w, "reload domain", err)
		return
	}
	if was != d.Status && (was == "verified" || d.Status == "verified") {
		action := "domain.verified"
		if was == "verified" {
			action = "domain.unverified"
		}
		s.audit(r, action, "domain", d.ID, map[string]any{"name": d.Name, "status": d.Status})
	}
	writeJSON(w, http.StatusOK, s.view(r.Context(), d))
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteDomain(r.Context(), d.ProjectID, d.ID); err != nil {
		s.internal(w, "delete domain", err)
		return
	}
	s.audit(r, "domain.deleted", "domain", d.ID, map[string]any{"name": d.Name})
	writeJSON(w, http.StatusOK, map[string]any{"object": "domain", "id": d.ID, "deleted": true})
}
