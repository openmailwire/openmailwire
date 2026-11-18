package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/apikey"
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
	CreatedAt  time.Time     `json:"created_at"`
	LastUsedAt *time.Time    `json:"last_used_at"`
	Token      string        `json:"token,omitempty"`
}

func apiKeyView(k store.APIKey) apiKeyResponse {
	return apiKeyResponse{
		Object: "api_key", ID: k.ID, Name: k.Name, Prefix: k.Prefix, Permission: k.Permission,
		Policy: k.Policy, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
	}
}

func validKeyName(name string) *policyError {
	if strings.TrimSpace(name) == "" || len(name) > 100 {
		return invalid("name is required (1-100 characters), e.g. the agent that will use the key")
	}
	return nil
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
	}
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if pe := validKeyName(req.Name); pe != nil {
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
		Permission: req.Permission, Policy: req.Policy,
	}, hash)
	if err != nil {
		s.internal(w, "create api key", err)
		return
	}
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

// updateAPIKey renames a key and/or replaces its whole policy ({} removes
// every limit). The permission cannot change: create a new key instead.
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
	k, err := s.Store.UpdateAPIKey(r.Context(), projectID(r), id, req.Name, req.Policy)
	if errors.Is(err, store.ErrNotFound) {
		keyNotFound(w)
		return
	}
	if err != nil {
		s.internal(w, "update api key", err)
		return
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"object": "api_key", "id": id, "deleted": true})
}
