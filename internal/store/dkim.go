package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DKIMKey is a DKIM key of a domain besides its current RSA key (which is
// kept on the domain itself): a new RSA key during a rotation, the previous
// RSA key during the overlap, or an Ed25519 key.
type DKIMKey struct {
	ID          string
	DomainID    string
	Selector    string
	Algorithm   string // "rsa" or "ed25519"
	Public      string
	Private     []byte // sealed PKCS#8
	State       string // pending, active or retiring
	CreatedAt   time.Time
	ActivatedAt *time.Time
	RetireAt    *time.Time
}

const dkimKeyCols = `id::text, domain_id::text, selector, algorithm, public_key, private_key, state, created_at, activated_at, retire_at`

func scanDKIMKey(row pgx.Row) (DKIMKey, error) {
	var k DKIMKey
	err := row.Scan(&k.ID, &k.DomainID, &k.Selector, &k.Algorithm, &k.Public, &k.Private, &k.State, &k.CreatedAt,
		&k.ActivatedAt, &k.RetireAt)
	return k, err
}

// DomainByID returns a domain whatever its project. For the worker API,
// which signs scheduled mail again when it is leased.
func (s *Store) DomainByID(ctx context.Context, id string) (Domain, error) {
	d, err := scanDomain(s.Pool.QueryRow(ctx, `SELECT `+domainCols+` FROM domains WHERE id = $1`, id))
	return d, noRows(err)
}

// DKIMKeys lists a domain's extra DKIM keys, oldest first.
func (s *Store) DKIMKeys(ctx context.Context, domainID string) ([]DKIMKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+dkimKeyCols+` FROM dkim_keys WHERE domain_id = $1 ORDER BY created_at, id`, domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DKIMKey
	for rows.Next() {
		k, err := scanDKIMKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AddDKIMKey stores a new key. ErrConflict when the domain already has a
// key with that selector.
func (s *Store) AddDKIMKey(ctx context.Context, k DKIMKey) (DKIMKey, error) {
	out, err := scanDKIMKey(s.Pool.QueryRow(ctx, `INSERT INTO dkim_keys (domain_id, selector, algorithm, public_key, private_key, state)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+dkimKeyCols, k.DomainID, k.Selector, k.Algorithm, k.Public, k.Private, k.State))
	if isUniqueViolation(err) {
		return DKIMKey{}, ErrConflict
	}
	return out, err
}

// ActivateDKIMKey marks a pending key active.
func (s *Store) ActivateDKIMKey(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE dkim_keys SET state = 'active', activated_at = now() WHERE id = $1 AND state = 'pending'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// DeleteDKIMKey removes one key of a domain.
func (s *Store) DeleteDKIMKey(ctx context.Context, domainID, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM dkim_keys WHERE domain_id = $1 AND id = $2`, domainID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// PromoteDKIMKey ends a rotation: the pending RSA key becomes the domain's
// current key, and the current one is kept as 'retiring' until now +
// overlap, so mail already signed with it still verifies. ErrNotFound when
// the key is not a pending RSA key of the domain.
func (s *Store) PromoteDKIMKey(ctx context.Context, domainID, keyID string, overlap time.Duration) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var oldSel, oldPub string
	var oldPriv []byte
	if err := tx.QueryRow(ctx, `SELECT dkim_selector, dkim_public, dkim_private FROM domains WHERE id = $1 FOR UPDATE`, domainID).
		Scan(&oldSel, &oldPub, &oldPriv); err != nil {
		return noRows(err)
	}
	k, err := scanDKIMKey(tx.QueryRow(ctx, `SELECT `+dkimKeyCols+` FROM dkim_keys
		WHERE id = $1 AND domain_id = $2 AND algorithm = 'rsa' AND state = 'pending' FOR UPDATE`, keyID, domainID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE domains SET dkim_selector = $2, dkim_public = $3, dkim_private = $4 WHERE id = $1`,
		domainID, k.Selector, k.Public, k.Private); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM dkim_keys WHERE id = $1`, k.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dkim_keys (domain_id, selector, algorithm, public_key, private_key, state, activated_at, retire_at)
		VALUES ($1, $2, 'rsa', $3, $4, 'retiring', now(), now() + make_interval(secs => $5))
		ON CONFLICT (domain_id, selector) DO NOTHING`, domainID, oldSel, oldPub, oldPriv, overlap.Seconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
