package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openmailwire/openmailwire/internal/apikey"
)

// APIKey is the authenticated caller of the public API.
type APIKey struct {
	ID         string
	ProjectID  string
	Name       string
	Prefix     string
	Permission string // apikey.FullAccess or apikey.SendingAccess
	Policy     apikey.Policy
	CreatedAt  time.Time
	LastUsedAt *time.Time
	// AllowedIPs are the networks the key may be used from; nil means
	// anywhere.
	AllowedIPs []netip.Prefix
	// ExpiresAt is when the key stops working; nil means never.
	ExpiresAt *time.Time
}

// FullAccess reports whether the key may call every endpoint.
func (k APIKey) FullAccess() bool { return k.Permission != apikey.SendingAccess }

// Expired reports whether the key has expired at now.
func (k APIKey) Expired(now time.Time) bool { return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) }

const keyCols = `id::text, project_id::text, name, prefix, permission, policy, created_at, last_used_at, allowed_ips, expires_at`

func scanKey(row pgx.Row) (APIKey, error) {
	var k APIKey
	var policy []byte
	var ips []string
	if err := row.Scan(&k.ID, &k.ProjectID, &k.Name, &k.Prefix, &k.Permission, &policy, &k.CreatedAt, &k.LastUsedAt,
		&ips, &k.ExpiresAt); err != nil {
		return k, err
	}
	if err := json.Unmarshal(policy, &k.Policy); err != nil {
		return k, err
	}
	if ips != nil {
		k.AllowedIPs = make([]netip.Prefix, 0, len(ips))
		for _, s := range ips {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return k, fmt.Errorf("api key %s: allowed_ips: %w", k.ID, err)
			}
			k.AllowedIPs = append(k.AllowedIPs, p)
		}
	}
	return k, nil
}

// prefixStrings is how allowed_ips are stored: nil for no restriction.
func prefixStrings(ps []netip.Prefix) []string {
	if ps == nil {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

// CreateAPIKey stores a full-access key without a policy.
func (s *Store) CreateAPIKey(ctx context.Context, projectID, name, prefix string, hash []byte) error {
	_, err := s.NewAPIKey(ctx, APIKey{ProjectID: projectID, Name: name, Prefix: prefix}, hash)
	return err
}

// NewAPIKey stores a key with its permission (full access when empty),
// policy, allowed networks and expiry. Only the token's hash is stored.
func (s *Store) NewAPIKey(ctx context.Context, k APIKey, hash []byte) (APIKey, error) {
	if k.Permission == "" {
		k.Permission = apikey.FullAccess
	}
	policy, err := json.Marshal(k.Policy)
	if err != nil {
		return APIKey{}, err
	}
	return scanKey(s.Pool.QueryRow(ctx, `INSERT INTO api_keys (project_id, name, prefix, token_hash, permission, policy,
			allowed_ips, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+keyCols,
		k.ProjectID, k.Name, k.Prefix, hash, k.Permission, policy, prefixStrings(k.AllowedIPs), k.ExpiresAt))
}

// KeyForToken returns the unrevoked API key with this token hash, expired
// or not (the caller checks ExpiresAt, to answer with the reason), and
// records its use (at most once a minute, so a busy key does not write on
// every request; never for an expired key).
func (s *Store) KeyForToken(ctx context.Context, hash []byte) (APIKey, error) {
	k, err := scanKey(s.Pool.QueryRow(ctx, `
		WITH k AS (
			SELECT * FROM api_keys WHERE token_hash = $1 AND revoked_at IS NULL
		), touch AS (
			UPDATE api_keys a SET last_used_at = now() FROM k
			WHERE a.id = k.id AND (k.last_used_at IS NULL OR k.last_used_at < now() - interval '1 minute')
				AND (k.expires_at IS NULL OR k.expires_at > now())
		)
		SELECT `+keyCols+` FROM k`, hash))
	return k, noRows(err)
}

// ListAPIKeys returns a project's live keys, oldest first.
func (s *Store) ListAPIKeys(ctx context.Context, projectID string) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+keyCols+` FROM api_keys
		WHERE project_id = $1 AND revoked_at IS NULL ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetAPIKey returns one live key of the project.
func (s *Store) GetAPIKey(ctx context.Context, projectID, id string) (APIKey, error) {
	k, err := scanKey(s.Pool.QueryRow(ctx, `SELECT `+keyCols+` FROM api_keys
		WHERE project_id = $1 AND id = $2 AND revoked_at IS NULL`, projectID, id))
	return k, noRows(err)
}

// KeyUpdate changes some of a key's settings; a nil field (or false Set…)
// leaves that setting unchanged.
type KeyUpdate struct {
	Name   *string
	Policy *apikey.Policy
	// SetAllowedIPs replaces the allowed networks with AllowedIPs (nil
	// removes the restriction).
	SetAllowedIPs bool
	AllowedIPs    []netip.Prefix
	// SetExpiresAt replaces the expiry with ExpiresAt (nil: never).
	SetExpiresAt bool
	ExpiresAt    *time.Time
}

// UpdateAPIKey changes a live key's settings.
func (s *Store) UpdateAPIKey(ctx context.Context, projectID, id string, u KeyUpdate) (APIKey, error) {
	var pj []byte
	if u.Policy != nil {
		var err error
		if pj, err = json.Marshal(u.Policy); err != nil {
			return APIKey{}, err
		}
	}
	k, err := scanKey(s.Pool.QueryRow(ctx, `UPDATE api_keys SET name = COALESCE($3, name), policy = COALESCE($4, policy),
			allowed_ips = CASE WHEN $5 THEN $6 ELSE allowed_ips END,
			expires_at = CASE WHEN $7 THEN $8 ELSE expires_at END
		WHERE project_id = $1 AND id = $2 AND revoked_at IS NULL RETURNING `+keyCols,
		projectID, id, u.Name, pj, u.SetAllowedIPs, prefixStrings(u.AllowedIPs), u.SetExpiresAt, u.ExpiresAt))
	return k, noRows(err)
}

// RevokeAPIKey revokes a live key. Its emails keep their api_key_id.
func (s *Store) RevokeAPIKey(ctx context.Context, projectID, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now()
		WHERE project_id = $1 AND id = $2 AND revoked_at IS NULL`, projectID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
