package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/openmailwire/openmailwire/internal/dkimkey"
	"github.com/openmailwire/openmailwire/internal/store"
)

// The send path, shared by every endpoint that queues mail (POST /emails,
// POST /emails/batch, POST /emails/raw, inbox replies):
//
//	replayed       answer a repeated Idempotency-Key early
//	authorizeSend  the key's allowlists and caps (policy.go)
//	senderDomain   the From domain must be added and verified
//	signMessage    DKIM-sign the rendered message
//	queue          store it for the key, all or nothing
//
// Each step answers with a *policyError in the API's error shape.

const (
	maxBody     = 4 << 20  // most endpoints
	maxSendBody = 20 << 20 // endpoints that take attachments or many emails
)

// bodyLimit is the request-body limit for r.
func bodyLimit(r *http.Request) int64 {
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/emails", "/emails/batch", "/emails/raw":
			return maxSendBody
		}
	}
	return maxBody
}

// readBody reads the request body, answering 413 when it is over the
// limit.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				fmt.Sprintf("request body is larger than %d MB", tooBig.Limit>>20))
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		}
		return nil, false
	}
	return body, true
}

// idempotencyKey reads the Idempotency-Key header.
func idempotencyKey(r *http.Request) (string, *policyError) {
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idem) > 256 {
		return "", invalid("Idempotency-Key is longer than 256 characters")
	}
	return idem, nil
}

// replayed answers the request and returns true when idem was already used
// in this project: with the first email's id for the same body, or 409 for
// a different one. Checking before anything else means a retry of a send
// that succeeded is never refused by a cap it has since reached.
func (s *Server) replayed(w http.ResponseWriter, r *http.Request, idem string, hash []byte) bool {
	if idem == "" {
		return false
	}
	id, err := s.Store.FindIdempotent(r.Context(), projectID(r), idem, hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "this Idempotency-Key was already used with a different request")
	case err != nil:
		s.internal(w, "find idempotent", err)
	default:
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	}
	return true
}

// senderDomain returns the project's domain called name (lowercase),
// refusing one that is not added or not verified.
func (s *Server) senderDomain(ctx context.Context, projectID, name string) (store.Domain, *policyError) {
	dom, err := s.Store.DomainByName(ctx, projectID, name)
	if errors.Is(err, store.ErrNotFound) {
		return dom, &policyError{Status: http.StatusForbidden, Name: "domain_not_found", Message: fmt.Sprintf(
			"the domain %s is not added to this project; add it with POST /domains", name)}
	}
	if err != nil {
		return dom, s.internalErr("domain lookup", err)
	}
	if dom.Status != "verified" && !s.Cfg.AllowUnverified {
		return dom, &policyError{Status: http.StatusForbidden, Name: "domain_not_verified", Message: fmt.Sprintf(
			"the domain %s is not verified yet; publish its DNS records and call POST /domains/%s/verify", dom.Name, dom.ID)}
	}
	return dom, nil
}

// signMessage DKIM-signs a rendered message with the domain's key.
func (s *Server) signMessage(dom store.Domain, raw []byte) ([]byte, *policyError) {
	der, err := s.Sealer.Open(dom.DKIMPrivate)
	if err != nil {
		return nil, s.internalErr("unseal dkim key", err)
	}
	key, err := dkimkey.ParsePrivate(der)
	if err != nil {
		return nil, s.internalErr("parse dkim key", err)
	}
	signed, err := dkimkey.Sign(raw, dom.Name, dom.Selector, key)
	if err != nil {
		return nil, s.internalErr("dkim sign", err)
	}
	return signed, nil
}

// queue stores signed emails for the request's API key, all of them or
// none, in one transaction. It sets each email's ProjectID and APIKeyID, and
// re-checks the key's caps under a per-key lock. Fill in everything else,
// with Raw already sealed (s.Sealer.Seal).
func (s *Server) queue(r *http.Request, es []store.NewEmail) ([]store.Inserted, *policyError) {
	key := apiKeyOf(r)
	for i := range es {
		es[i].ProjectID = key.ProjectID
		es[i].APIKeyID = key.ID
	}
	out, err := s.Store.InsertEmails(r.Context(), es, capsOf(key))
	var ce *store.CapError
	switch {
	case errors.As(err, &ce):
		return nil, capRefusal(ce)
	case errors.Is(err, store.ErrConflict):
		return nil, &policyError{Status: http.StatusConflict, Name: "idempotency_conflict",
			Message: "this Idempotency-Key was already used with a different request"}
	case err != nil:
		return nil, s.internalErr("insert email", err)
	}
	return out, nil
}
