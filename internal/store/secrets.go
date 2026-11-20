package store

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openmailwire/openmailwire/internal/secret"
)

// SealedColumn is a column whose values are sealed with OMW_SECRET_KEYS.
// Key is the table's primary key column.
type SealedColumn struct{ Table, Key, Column string }

func (c SealedColumn) String() string { return c.Table + "." + c.Column }

// SealedColumns lists every column that holds sealed values: the start-up
// key check samples them, and `openmailwire rotate-secrets` reseals them.
//
// A table that adds a sealed column must be added here, or rotation
// would leave values sealed with a retired key.
var SealedColumns = []SealedColumn{
	{"domains", "id", "dkim_private"},
	{"webhooks", "id", "secret"},
	{"webhooks", "id", "secret_previous"},
	{"webhooks", "id", "ed25519_key"},
	{"webhooks", "id", "ed25519_previous_key"},
	{"secret_values", "name", "value"},
	{"emails", "id", "raw"},
	{"inbound_messages", "id", "raw"},
	{"dkim_keys", "id", "private_key"},           // extra and rotated DKIM keys (0008)
	{"inbound_messages", "id", "snippet_sealed"}, // list snippets (0009)
	{"dns_automation", "domain_id", "api_token"}, // remembered DNS provider tokens (0012)
}

// macRootName names the secret_values row holding the root of the keys
// that sign bounce addresses.
const macRootName = "mac_root"

// KeyCheckError is a start-up refusal: the configured keys don't match the
// data in the database.
type KeyCheckError struct{ Msg string }

func (e *KeyCheckError) Error() string { return e.Msg }

// VerifySecretKeys checks keys (OMW_SECRET_KEYS, the sealing key first)
// against the database before the control plane starts:
//
//   - every key that still seals data here must be configured;
//   - a configured key must match the check value stored under its id.
//
// On the first start with key ids (a new database, or an upgrade from
// v0.5) it samples the sealed data instead, and refuses to start if a
// sample opens with none of the keys. It registers the sealing key, and
// returns the MAC root: the key from which bounce-address keys derive. The
// root is the key the existing data was sealed with (on an upgrade, the old
// OMW_SECRET_KEY, so addresses already sent keep verifying) and is kept
// sealed in the database, so it survives key rotations.
func (s *Store) VerifySecretKeys(ctx context.Context, keys [][]byte) (macRoot []byte, err error) {
	sealer, err := secret.NewKeyring(keys)
	if err != nil {
		return nil, err
	}
	byID := map[string][]byte{}
	for _, k := range keys {
		byID[secret.KeyID(k)] = k
	}
	primary := sealer.PrimaryID()

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7461230010)`); err != nil {
		return nil, err
	}
	type row struct {
		id      string
		check   []byte
		retired bool
	}
	rows, err := tx.Query(ctx, `SELECT key_id, check_value, retired_at IS NOT NULL FROM secret_keys ORDER BY created_at, key_id`)
	if err != nil {
		return nil, err
	}
	var known []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.check, &r.retired); err != nil {
			rows.Close()
			return nil, err
		}
		known = append(known, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(known) == 0 {
		opener, err := probeSealed(ctx, tx, sealer)
		if err != nil {
			return nil, err
		}
		root := keys[0]
		if opener != "" {
			root = byID[opener]
		}
		for _, id := range uniq(primary, opener) {
			if _, err := tx.Exec(ctx, `INSERT INTO secret_keys (key_id, check_value) VALUES ($1, $2)`,
				id, secret.KeyCheck(byID[id])); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO secret_values (name, value) VALUES ($1, $2)`,
			macRootName, sealer.Seal(root)); err != nil {
			return nil, err
		}
		return root, tx.Commit(ctx)
	}

	var missing []string
	primaryKnown, primaryRetired := false, false
	for _, r := range known {
		k, configured := byID[r.id]
		if configured && !hmac.Equal(secret.KeyCheck(k), r.check) {
			return nil, &KeyCheckError{fmt.Sprintf("OMW_SECRET_KEYS: the key with id %s is not the key this database registered under that id; "+
				"set the original key", r.id)}
		}
		if !configured && !r.retired {
			missing = append(missing, r.id)
		}
		if r.id == primary {
			primaryKnown, primaryRetired = true, r.retired
		}
	}
	if len(missing) > 0 {
		return nil, &KeyCheckError{fmt.Sprintf("data in this database is sealed with key %s, which is not configured "+
			"(configured: %s). Set OMW_SECRET_KEYS to the new key followed by the old one(s), run `openmailwire rotate-secrets`, "+
			"and remove an old key only once it reports that nothing is sealed with it",
			strings.Join(missing, ", "), strings.Join(sealer.IDs(), ", "))}
	}
	switch {
	case !primaryKnown:
		if _, err := tx.Exec(ctx, `INSERT INTO secret_keys (key_id, check_value) VALUES ($1, $2)`,
			primary, secret.KeyCheck(keys[0])); err != nil {
			return nil, err
		}
	case primaryRetired:
		if _, err := tx.Exec(ctx, `UPDATE secret_keys SET retired_at = NULL WHERE key_id = $1`, primary); err != nil {
			return nil, err
		}
	}
	var sealedRoot []byte
	err = tx.QueryRow(ctx, `SELECT value FROM secret_values WHERE name = $1`, macRootName).Scan(&sealedRoot)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		macRoot = keys[0]
		if _, err := tx.Exec(ctx, `INSERT INTO secret_values (name, value) VALUES ($1, $2)`, macRootName, sealer.Seal(macRoot)); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if macRoot, err = sealer.Open(sealedRoot); err != nil {
			return nil, &KeyCheckError{"none of OMW_SECRET_KEYS opens the stored bounce-address key: " + err.Error()}
		}
	}
	return macRoot, tx.Commit(ctx)
}

func uniq(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == id
		}
		if !dup {
			out = append(out, id)
		}
	}
	return out
}

// probeSealed opens a few values of every sealed column and returns the id
// of the key that opened the first, or "" when there is no sealed data.
func probeSealed(ctx context.Context, tx pgx.Tx, sealer *secret.Sealer) (string, error) {
	opener := ""
	for _, c := range SealedColumns {
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT 3`, c.Column, c.Table, c.Column))
		if err != nil {
			return "", fmt.Errorf("sample %s: %w", c, err)
		}
		var vals [][]byte
		for rows.Next() {
			var v []byte
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return "", err
			}
			vals = append(vals, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		for _, v := range vals {
			id := sealer.OpenedBy(v)
			if id == "" {
				return "", &KeyCheckError{fmt.Sprintf("OMW_SECRET_KEY does not open the data already in this database (%s). "+
					"Set it to the key this database was created with; a different key cannot read the stored DKIM keys, "+
					"webhook secrets and messages", c)}
			}
			if opener == "" {
				opener = id
			}
		}
	}
	return opener, nil
}

// ResealStats counts what a reseal pass did to one column.
type ResealStats struct {
	Column     SealedColumn
	Resealed   int
	Unreadable int // values no configured key opens; left as they are
}

// ResealOptions paces a reseal pass so it runs next to a live control
// plane.
type ResealOptions struct {
	Batch int           // rows per transaction
	Pause time.Duration // between transactions
	// Progress, when set, is called after every batch.
	Progress func(ResealStats)
}

// ResealColumn rewrites every value of c that is not sealed with the
// sealing key of sealer, in small transactions that lock only the rows
// they rewrite (SKIP LOCKED), in primary key order.
func (s *Store) ResealColumn(ctx context.Context, sealer *secret.Sealer, c SealedColumn, o ResealOptions) (ResealStats, error) {
	if o.Batch <= 0 {
		o.Batch = 100
	}
	st := ResealStats{Column: c}
	header := sealer.PrimaryHeader()
	after := ""
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		n, last, err := s.resealBatch(ctx, sealer, c, header, after, first, o.Batch, &st)
		if err != nil {
			return st, fmt.Errorf("reseal %s: %w", c, err)
		}
		if o.Progress != nil {
			o.Progress(st)
		}
		if n < o.Batch {
			return st, nil
		}
		after, first = last, false
		if o.Pause > 0 {
			select {
			case <-ctx.Done():
				return st, ctx.Err()
			case <-time.After(o.Pause):
			}
		}
	}
}

func (s *Store) resealBatch(ctx context.Context, sealer *secret.Sealer, c SealedColumn, header []byte,
	after string, first bool, batch int, st *ResealStats) (int, string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)
	cond := ""
	args := []any{header, batch}
	if !first {
		cond = fmt.Sprintf(" AND %s > $3", c.Key)
		args = append(args, after)
	}
	// substring() reads only the first bytes of a large value.
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %[1]s::text, %[2]s FROM %[3]s
		WHERE %[2]s IS NOT NULL AND substring(%[2]s from 1 for 8) <> $1%[4]s
		ORDER BY %[1]s LIMIT $2 FOR UPDATE SKIP LOCKED`, c.Key, c.Column, c.Table, cond), args...)
	if err != nil {
		return 0, "", err
	}
	type item struct {
		key string
		val []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.key, &it.val); err != nil {
			rows.Close()
			return 0, "", err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	for _, it := range items {
		v, err := sealer.Reseal(it.val)
		if err != nil {
			st.Unreadable++
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s = $1 WHERE %s = $2`, c.Table, c.Column, c.Key), v, it.key); err != nil {
			return 0, "", err
		}
		st.Resealed++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	last := ""
	if len(items) > 0 {
		last = items[len(items)-1].key
	}
	return len(items), last, nil
}

// CountUnsealedByPrimary counts the values of c not sealed with the sealing
// key, at most limit of them.
func (s *Store) CountUnsealedByPrimary(ctx context.Context, sealer *secret.Sealer, c SealedColumn, limit int) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM %[2]s
		WHERE %[1]s IS NOT NULL AND substring(%[1]s from 1 for 8) <> $1 LIMIT $2) x`, c.Column, c.Table),
		sealer.PrimaryHeader(), limit).Scan(&n)
	return n, err
}

// RetireSecretKeys marks every key but the sealing key retired: nothing is
// sealed with them any more, so they may be removed from OMW_SECRET_KEYS.
// It returns the ids it retired.
func (s *Store) RetireSecretKeys(ctx context.Context, primaryID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE secret_keys SET retired_at = now()
		WHERE key_id <> $1 AND retired_at IS NULL RETURNING key_id`, primaryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
