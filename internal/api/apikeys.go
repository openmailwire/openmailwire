package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/apikey"
	"github.com/openmailwire/openmailwire/internal/message"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

type apiKeyResponse struct {
	Object     string        `json:"object"`
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Prefix     string        `json:"prefix"`
	Permission string        `json:"permission"`
	Policy     apikey.Policy `json:"policy"`
	AllowedIPs []string      `json:"allowed_ips"` // null: any address
	ExpiresAt  *time.Time    `json:"expires_at"`  // null: never
	CreatedAt  time.Time     `json:"created_at"`
	LastUsedAt *time.Time    `json:"last_used_at"`
	Token      string        `json:"token,omitempty"`
}

func apiKeyView(k store.APIKey) apiKeyResponse {
	v := apiKeyResponse{
		Object: "api_key", ID: k.ID, Name: k.Name, Prefix: k.Prefix, Permission: k.Permission,
		Policy: k.Policy, ExpiresAt: k.ExpiresAt, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
	}
	if k.AllowedIPs != nil {
		v.AllowedIPs = make([]string, len(k.AllowedIPs))
		for i, p := range k.AllowedIPs {
			v.AllowedIPs[i] = p.String()
		}
	}
	return v
}

func validKeyName(name string) *policyError {
	if strings.TrimSpace(name) == "" || len(name) > 100 {
		return invalid("name is required (1-100 characters), e.g. the agent that will use the key")
	}
	if !message.Storable(name) || strings.ContainsFunc(name, isControl) {
		return invalid("name contains a NUL or other control character; remove it")
	}
	return nil
}

// keyAccess holds a key's allowed_ips and expires_at, as given in a create
// or update request.
type keyAccess struct {
	AllowedIPs jsonSpan `json:"allowed_ips"`
	ExpiresAt  jsonSpan `json:"expires_at"`
}

// parse validates the fields; set… is false for a field left out.
func (a keyAccess) parse(now time.Time) (ips []netip.Prefix, setIPs bool, expires *time.Time, setExpires bool, pe *policyError) {
	if a.AllowedIPs != nil {
		setIPs = true
		if !a.AllowedIPs.isNull() {
			var list []string
			if err := json.Unmarshal(a.AllowedIPs, &list); err != nil {
				return nil, false, nil, false, invalid(`allowed_ips must be a list of addresses or CIDR prefixes, like ["203.0.113.7", "10.0.0.0/8"], or null`)
			}
			var err error
			if ips, err = apikey.ParseAllowedIPs(list); err != nil {
				return nil, false, nil, false, invalid("%v", err)
			}
		}
	}
	if a.ExpiresAt != nil {
		setExpires = true
		if !a.ExpiresAt.isNull() {
			var t time.Time
			if err := json.Unmarshal(a.ExpiresAt, &t); err != nil {
				return nil, false, nil, false, invalid("expires_at must be an RFC 3339 time, like %s, or null for never",
					now.Add(30*24*time.Hour).Format(time.RFC3339))
			}
			if !t.After(now) {
				return nil, false, nil, false, invalid("expires_at must be in the future")
			}
			t = t.UTC()
			expires = &t
		}
	}
	return ips, setIPs, expires, setExpires, nil
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Name       string        `json:"name"`
		Permission string        `json:"permission"`
		Policy     apikey.Policy `json:"policy"`
		keyAccess
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if pe := validKeyName(req.Name); pe != nil {
		pe.write(w)
		return
	}
	ips, _, expires, _, pe := req.keyAccess.parse(time.Now())
	if pe != nil {
		pe.write(w)
		return
	}
	if req.Permission == "" {
		req.Permission = apikey.FullAccess
	}
	if !apikey.ValidPermission(req.Permission) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "permission must be full_access or sending_access")
		return
	}
	if err := req.Policy.Normalize(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	token, hash := secret.NewToken("omw_")
	k, err := s.Store.NewAPIKey(r.Context(), store.APIKey{
		ProjectID: projectID(r), Name: strings.TrimSpace(req.Name), Prefix: token[:10],
		Permission: req.Permission, Policy: req.Policy, AllowedIPs: ips, ExpiresAt: expires,
	}, hash)
	if err != nil {
		s.internal(w, "create api key", err)
		return
	}
	s.audit(r, "api_key.created", "api_key", k.ID, keyAuditData(k))
	resp := apiKeyView(k)
	resp.Token = token // shown once
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.ListAPIKeys(r.Context(), projectID(r))
	if err != nil {
		s.internal(w, "list api keys", err)
		return
	}
	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func keyNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "API key not found (it may have been revoked)")
}

func (s *Server) getAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		keyNotFound(w)
		return
	}
	k, err := s.Store.GetAPIKey(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		keyNotFound(w)
		return
	}
	if err != nil {
		s.internal(w, "get api key", err)
		return
	}
	writeJSON(w, http.StatusOK, apiKeyView(k))
}

// keyAuditData describes a key for the audit log: its settings, never its
// token.
func keyAuditData(k store.APIKey) map[string]any {
	v := apiKeyView(k)
	return map[string]any{"name": v.Name, "prefix": v.Prefix, "permission": v.Permission, "policy": v.Policy,
		"allowed_ips": v.AllowedIPs, "expires_at": v.ExpiresAt}
}

// updateAPIKey renames a key, replaces its whole policy ({} removes every
// limit), its allowed_ips or its expires_at (null removes either). The
// permission cannot change: create a new key instead.
func (s *Server) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		keyNotFound(w)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req struct {
		Name       *string        `json:"name"`
		Policy     *apikey.Policy `json:"policy"`
		Permission *string        `json:"permission"`
		keyAccess
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if req.Permission != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"permission cannot be changed; create a key with the permission you need and revoke this one")
		return
	}
	if req.Name != nil {
		if pe := validKeyName(*req.Name); pe != nil {
			pe.write(w)
			return
		}
		*req.Name = strings.TrimSpace(*req.Name)
	}
	if req.Policy != nil {
		if err := req.Policy.Normalize(); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
			return
		}
	}
	u := store.KeyUpdate{Name: req.Name, Policy: req.Policy}
	var pe *policyError
	if u.AllowedIPs, u.SetAllowedIPs, u.ExpiresAt, u.SetExpiresAt, pe = req.keyAccess.parse(time.Now()); pe != nil {
		pe.write(w)
		return
	}
	k, err := s.Store.UpdateAPIKey(r.Context(), projectID(r), id, u)
	if errors.Is(err, store.ErrNotFound) {
		keyNotFound(w)
		return
	}
	if err != nil {
		s.internal(w, "update api key", err)
		return
	}
	s.audit(r, "api_key.updated", "api_key", k.ID, keyAuditData(k))
	writeJSON(w, http.StatusOK, apiKeyView(k))
}

func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		keyNotFound(w)
		return
	}
	err := s.Store.RevokeAPIKey(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		keyNotFound(w)
		return
	}
	if err != nil {
		s.internal(w, "revoke api key", err)
		return
	}
	s.audit(r, "api_key.revoked", "api_key", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"object": "api_key", "id": id, "deleted": true})
}
