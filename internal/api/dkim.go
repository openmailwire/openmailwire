package api

import (
	"context"
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

// DKIM key rotation and Ed25519 (lane C).
//
// A domain signs with its current RSA key (domains.dkim_*, the record lane
// A's domain view shows as DKIM) and, when enabled, also with an Ed25519
// key (RFC 8463). Rotation uses two selectors and an overlap:
//
//  1. POST /domains/{id}/dkim/rotate creates a new RSA key under a new
//     selector. Signing goes on with the current key; the answer names the
//     TXT record to publish.
//  2. Once it is published, the same call switches signing to the new key.
//     The previous key stays listed as DKIM_PREVIOUS until its retire_at
//     (dkimOverlap later), because mail queued before the switch was signed
//     with it; then its record can be deleted.
//
// Ed25519 works the same way: POST /domains/{id}/dkim/ed25519
// {"enabled": true} creates the key, and calling it again once the record
// is published turns the second signature on.

// dkimOverlap is how long a previous key stays published after a rotation:
// longer than the longest a queued message can wait (7 days). Scheduled
// mail is signed again when it is sent, so it never needs the old key.
const dkimOverlap = 8 * 24 * time.Hour

// dkimRoutes registers the DKIM endpoints (called from Handler).
func (s *Server) dkimRoutes(mux *http.ServeMux) {
	mux.Handle("GET /domains/{id}/dkim", s.withProject(s.getDKIM))
	mux.Handle("POST /domains/{id}/dkim/rotate", s.withFullAccess(s.rotateDKIM))
	mux.Handle("POST /domains/{id}/dkim/ed25519", s.withFullAccess(s.setEd25519))
}

type dkimKeyView struct {
	Selector  string         `json:"selector"`
	Algorithm string         `json:"algorithm"`
	State     string         `json:"state"` // active, pending or retiring
	Record    domains.Record `json:"record"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	RetireAt  *time.Time     `json:"retire_at,omitempty"`
	Signs     bool           `json:"signs"` // new mail is signed with it
	extra     *store.DKIMKey // nil for the current RSA key
}

// dkimKeysOf lists every DKIM key of the domain: the current RSA key first.
func (s *Server) dkimKeysOf(ctx context.Context, d store.Domain) ([]dkimKeyView, error) {
	extra, err := s.Store.DKIMKeys(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	out := []dkimKeyView{{
		Selector: d.Selector, Algorithm: dkimkey.RSA, State: "active", Signs: true,
		Record: domains.Record{Record: "DKIM", Type: "TXT", Name: d.Selector + "._domainkey." + d.Name,
			Value: dkimkey.TXTRecord(d.DKIMPublic), Required: true, Status: domains.StatusVerified},
	}}
	for i := range extra {
		k := extra[i]
		v := dkimKeyView{Selector: k.Selector, Algorithm: k.Algorithm, State: k.State, CreatedAt: &k.CreatedAt,
			RetireAt: k.RetireAt, extra: &extra[i], Signs: k.State == "active"}
		v.Record = domains.Record{Type: "TXT", Name: k.Selector + "._domainkey." + d.Name,
			Value: dkimkey.TXTRecordFor(k.Algorithm, k.Public)}
		switch {
		case k.Algorithm == dkimkey.Ed25519 && k.State == "pending":
			v.Record.Record, v.Record.Status = "DKIM_ED25519", domains.StatusPending
			v.Record.Detail = "publish this record, then call POST /domains/" + d.ID + "/dkim/ed25519 with {\"enabled\": true} again to start signing with Ed25519"
		case k.Algorithm == dkimkey.Ed25519:
			v.Record.Record, v.Record.Status = "DKIM_ED25519", domains.StatusVerified
			v.Record.Detail = "Ed25519 signature next to the RSA one (RFC 8463)"
		case k.State == "pending":
			v.Record.Record, v.Record.Status = "DKIM_NEXT", domains.StatusPending
			v.Record.Detail = "key rotation: publish this record, then call POST /domains/" + d.ID + "/dkim/rotate again to switch signing to it"
		default:
			v.Record.Record, v.Record.Status = "DKIM_PREVIOUS", domains.StatusVerified
			if k.RetireAt != nil {
				v.Record.Detail = "the previous key after a rotation: keep this record until " + k.RetireAt.UTC().Format(time.RFC3339) +
					", because mail queued before the switch carries its signature; then delete it"
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// dkimRecords are the DKIM records beyond the current key, for the domain
// view (a hook in domains.go).
func (s *Server) dkimRecords(ctx context.Context, d store.Domain) []domains.Record {
	keys, err := s.dkimKeysOf(ctx, d)
	if err != nil {
		s.Log.Warn("dkim keys", "domain", d.Name, "err", err)
		return nil
	}
	var out []domains.Record
	for _, k := range keys[1:] {
		out = append(out, k.Record)
	}
	return out
}

// dkimSigningKeys returns the keys new mail of the domain is signed with:
// the current RSA key, and the active Ed25519 key when there is one.
func (s *Server) dkimSigningKeys(ctx context.Context, d store.Domain) ([]dkimkey.Key, error) {
	der, err := s.Sealer.Open(d.DKIMPrivate)
	if err != nil {
		return nil, fmt.Errorf("unseal dkim key: %w", err)
	}
	rsaKey, err := dkimkey.ParsePrivate(der)
	if err != nil {
		return nil, err
	}
	keys := []dkimkey.Key{{Selector: d.Selector, Signer: rsaKey}}
	ed, err := s.activeEd25519(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if ed != nil {
		keys = append(keys, *ed)
	}
	return keys, nil
}

func (s *Server) activeEd25519(ctx context.Context, domainID string) (*dkimkey.Key, error) {
	extra, err := s.Store.DKIMKeys(ctx, domainID)
	if err != nil {
		return nil, err
	}
	for _, k := range extra {
		if k.Algorithm != dkimkey.Ed25519 || k.State != "active" {
			continue
		}
		der, err := s.Sealer.Open(k.Private)
		if err != nil {
			return nil, fmt.Errorf("unseal ed25519 key: %w", err)
		}
		signer, err := dkimkey.ParseSigner(der)
		if err != nil {
			return nil, err
		}
		return &dkimkey.Key{Selector: k.Selector, Signer: signer}, nil
	}
	return nil, nil
}

// signEd25519 adds the domain's Ed25519 signature, when it has an active
// Ed25519 key, to a message signMessage (send.go) signed with RSA.
func (s *Server) signEd25519(d store.Domain, signed []byte) ([]byte, *policyError) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ed, err := s.activeEd25519(ctx, d.ID)
	if err != nil {
		return nil, s.internalErr("ed25519 key", err)
	}
	if ed == nil {
		return signed, nil
	}
	out, err := dkimkey.SignAll(signed, d.Name, []dkimkey.Key{*ed})
	if err != nil {
		return nil, s.internalErr("ed25519 sign", err)
	}
	return out, nil
}

func (s *Server) getDKIM(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	keys, err := s.dkimKeysOf(r.Context(), d)
	if err != nil {
		s.internal(w, "dkim keys", err)
		return
	}
	issues, err := s.Store.AuthIssues(r.Context(), d.ProjectID, d.Name)
	if err != nil {
		s.internal(w, "auth issues", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "dkim", "domain_id": d.ID, "domain": d.Name, "keys": keys,
		"auth_issues": issues})
}

// newSelector picks a selector the domain does not use yet.
func newSelector(prefix string, taken []dkimKeyView) string {
	for {
		sel := prefix + time.Now().UTC().Format("200601") + secret.RandomHex(2)
		free := true
		for _, k := range taken {
			if k.Selector == sel {
				free = false
			}
		}
		if free {
			return sel
		}
	}
}

// published reports whether name has a DKIM TXT record with public key pub.
func (s *Server) published(ctx context.Context, name, pub string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	txts, err := s.Resolver.LookupTXT(ctx, name)
	if err != nil {
		return false, "no TXT record found at " + name + " yet (" + err.Error() + ")"
	}
	for _, t := range txts {
		for _, part := range strings.Split(t, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && strings.TrimSpace(k) == "p" && strings.Join(strings.Fields(v), "") == pub {
				return true, ""
			}
		}
	}
	return false, "a TXT record exists at " + name + " but its p= key is not the new one"
}

func (s *Server) rotateDKIM(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	keys, err := s.dkimKeysOf(ctx, d)
	if err != nil {
		s.internal(w, "dkim keys", err)
		return
	}
	var pending *dkimKeyView
	for i := range keys {
		if keys[i].extra != nil && keys[i].Algorithm == dkimkey.RSA && keys[i].State == "pending" {
			pending = &keys[i]
		}
	}
	if pending == nil {
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
		if _, err := s.Store.AddDKIMKey(ctx, store.DKIMKey{DomainID: d.ID, Selector: newSelector("omw", keys),
			Algorithm: dkimkey.RSA, Public: pub, Private: s.Sealer.Seal(der), State: "pending"}); err != nil {
			s.internal(w, "add dkim key", err)
			return
		}
		keys, err = s.dkimKeysOf(ctx, d)
		if err != nil {
			s.internal(w, "dkim keys", err)
			return
		}
		next := keys[len(keys)-1]
		out := map[string]any{
			"object": "dkim_rotation", "status": "pending", "record": next.Record, "keys": keys,
			"message": "publish the TXT record " + next.Record.Name + ", then call POST /domains/" + d.ID +
				"/dkim/rotate again to switch signing to the new key. Mail is signed with the current key until then.",
		}
		if auto := s.dnsAutoPublish(r, d, "dkim_rotate", "DKIM_NEXT"); auto != nil { // lane G: a remembered DNS token publishes it
			out["dns_auto"] = auto
		}
		writeJSON(w, http.StatusAccepted, out)
		return
	}
	if ok, why := s.published(ctx, pending.Record.Name, pending.extra.Public); !ok {
		writeError(w, http.StatusConflict, "dkim_record_not_found", "the new DKIM key is not published yet: "+why+
			". Publish "+pending.Record.Name+" TXT \""+pending.Record.Value+"\", wait for DNS, then call this endpoint again.")
		return
	}
	if err := s.Store.PromoteDKIMKey(ctx, d.ID, pending.extra.ID, dkimOverlap); err != nil {
		s.internal(w, "promote dkim key", err)
		return
	}
	d, err = s.Store.GetDomain(ctx, d.ProjectID, d.ID)
	if err != nil {
		s.internal(w, "reload domain", err)
		return
	}
	keys, err = s.dkimKeysOf(ctx, d)
	if err != nil {
		s.internal(w, "dkim keys", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "dkim_rotation", "status": "rotated", "selector": d.Selector, "keys": keys,
		"message": "new mail is signed with selector " + d.Selector + ". Keep the previous record published until its retire_at.",
	})
}

func (s *Server) setEd25519(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeStrict(body, &req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", `send {"enabled": true} or {"enabled": false}`)
		return
	}
	keys, err := s.dkimKeysOf(ctx, d)
	if err != nil {
		s.internal(w, "dkim keys", err)
		return
	}
	var ed *dkimKeyView
	for i := range keys {
		if keys[i].Algorithm == dkimkey.Ed25519 {
			ed = &keys[i]
		}
	}
	var dnsAuto map[string]any // lane G: what a remembered DNS token published
	reply := func(status int, state, msg string) {
		keys, err := s.dkimKeysOf(ctx, d)
		if err != nil {
			s.internal(w, "dkim keys", err)
			return
		}
		out := map[string]any{"object": "dkim_ed25519", "status": state, "keys": keys, "message": msg}
		if dnsAuto != nil {
			out["dns_auto"] = dnsAuto
		}
		writeJSON(w, status, out)
	}
	switch {
	case !*req.Enabled:
		if ed != nil {
			if err := s.Store.DeleteDKIMKey(ctx, d.ID, ed.extra.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.internal(w, "delete dkim key", err)
				return
			}
		}
		reply(http.StatusOK, "disabled", "new mail is signed with RSA only; the Ed25519 TXT record can be deleted")
	case ed == nil:
		key, err := dkimkey.GenerateEd25519()
		if err != nil {
			s.internal(w, "ed25519 keygen", err)
			return
		}
		der, err := dkimkey.MarshalPrivate(key)
		if err != nil {
			s.internal(w, "ed25519 marshal", err)
			return
		}
		sel := newSelector("omwe", keys)
		if _, err := s.Store.AddDKIMKey(ctx, store.DKIMKey{DomainID: d.ID, Selector: sel, Algorithm: dkimkey.Ed25519,
			Public: dkimkey.PublicBase64Ed25519(key), Private: s.Sealer.Seal(der), State: "pending"}); err != nil {
			s.internal(w, "add dkim key", err)
			return
		}
		dnsAuto = s.dnsAutoPublish(r, d, "ed25519", "DKIM_ED25519")
		reply(http.StatusAccepted, "pending", "publish the DKIM_ED25519 TXT record, then call this endpoint with {\"enabled\": true} again to start signing with Ed25519")
	case ed.State == "active":
		reply(http.StatusOK, "active", "new mail carries an Ed25519 signature next to the RSA one")
	default:
		if ok, why := s.published(ctx, ed.Record.Name, ed.extra.Public); !ok {
			writeError(w, http.StatusConflict, "dkim_record_not_found", "the Ed25519 key is not published yet: "+why+
				". Publish "+ed.Record.Name+" TXT \""+ed.Record.Value+"\", wait for DNS, then call this endpoint again.")
			return
		}
		if err := s.Store.ActivateDKIMKey(ctx, ed.extra.ID); err != nil {
			s.internal(w, "activate dkim key", err)
			return
		}
		reply(http.StatusOK, "active", "new mail carries an Ed25519 signature next to the RSA one")
	}
}
