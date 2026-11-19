package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The audit log records who changed what: keys, domains, webhooks, inboxes,
// approvals, secret rotations. It is append-only (a trigger refuses UPDATE
// and DELETE) and hash-chained per project: each entry's hash is
//
//	SHA-256(prev_hash || entry)
//
// where entry is the canonical JSON text stored with the row and prev_hash
// is the previous entry's hash in the same project (32 zero bytes for the
// first). Editing or removing a row in the database breaks the chain, which
// VerifyAudit (GET /audit-log/verify) reports.

// Who acted.
const (
	ActorAPIKey = "api_key" // ActorKeyID says which
	ActorCLI    = "cli"     // an operator, with the openmailwire command
	ActorSystem = "system"  // the control plane itself (expiry, sweeps)
)

// AuditEntry is one thing to record.
type AuditEntry struct {
	ProjectID  string // "" for a server-wide entry (e.g. a secret rotation)
	Actor      string // ActorAPIKey, ActorCLI or ActorSystem; default ActorAPIKey with ActorKeyID, else ActorSystem
	ActorKeyID string
	// Action is "<thing>.<verb>": api_key.created, domain.verified,
	// webhook.deleted, email.approved, secret.rotated, …
	Action     string
	TargetType string // api_key, domain, webhook, inbox, email, …
	TargetID   string
	// Data holds details. Never put secrets, tokens or message bodies here.
	Data map[string]any
}

// auditJSON is the canonical, hashed form of an entry. Field order is fixed
// and encoding/json sorts map keys, so the same entry always gives the same
// text.
type auditJSON struct {
	ID         int64          `json:"id"`
	ProjectID  *string        `json:"project_id"`
	CreatedAt  string         `json:"created_at"`
	Actor      string         `json:"actor"`
	ActorKeyID *string        `json:"actor_key_id"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	Data       map[string]any `json:"data"`
}

// AuditRecord is a stored entry.
type AuditRecord struct {
	ID         int64          `json:"id"`
	ProjectID  *string        `json:"project_id"`
	CreatedAt  time.Time      `json:"created_at"`
	Actor      string         `json:"actor"`
	ActorKeyID *string        `json:"actor_key_id"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	Data       map[string]any `json:"data"`
	// Entry is the exact text that was hashed.
	Entry    string `json:"entry"`
	PrevHash []byte `json:"-"`
	Hash     []byte `json:"-"`
}

var zeroHash = make([]byte, sha256.Size)

func auditHash(prev []byte, entry string) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write([]byte(entry))
	return h.Sum(nil)
}

// AppendAudit records an entry in its own transaction.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := AppendAuditTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AppendAuditTx records an entry inside tx, so that it commits (or rolls
// back) with the change it describes. It holds a per-project lock until tx
// ends, which keeps the chain linear.
func AppendAuditTx(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	if e.Action == "" {
		return fmt.Errorf("audit: action is required")
	}
	if e.Actor == "" {
		e.Actor = ActorSystem
		if e.ActorKeyID != "" {
			e.Actor = ActorAPIKey
		}
	}
	chain := e.ProjectID
	if chain == "" {
		chain = "server"
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7461232, hashtext($1))`, chain); err != nil {
		return err
	}
	prev := zeroHash
	q := `SELECT hash FROM audit_log WHERE project_id = $1 ORDER BY id DESC LIMIT 1`
	args := []any{e.ProjectID}
	if e.ProjectID == "" {
		q, args = `SELECT hash FROM audit_log WHERE project_id IS NULL ORDER BY id DESC LIMIT 1`, nil
	}
	if err := tx.QueryRow(ctx, q, args...).Scan(&prev); err != nil && err != pgx.ErrNoRows {
		return err
	}
	var id int64
	if err := tx.QueryRow(ctx, `SELECT nextval('audit_log_id_seq')`).Scan(&id); err != nil {
		return err
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	ent := auditJSON{
		ID: id, CreatedAt: at.Format("2006-01-02T15:04:05.000000Z07:00"), Actor: e.Actor,
		Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID, Data: data,
	}
	if e.ProjectID != "" {
		ent.ProjectID = &e.ProjectID
	}
	if e.ActorKeyID != "" {
		ent.ActorKeyID = &e.ActorKeyID
	}
	text, err := json.Marshal(ent)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (id, project_id, created_at, actor, actor_key_id, action, target_type,
			target_id, entry, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, nullString(e.ProjectID), at, e.Actor, nullString(e.ActorKeyID), e.Action, e.TargetType, e.TargetID,
		string(text), prev, auditHash(prev, string(text)))
	return err
}

// AuditQuery selects a page of a project's audit log, newest first.
type AuditQuery struct {
	ProjectID string
	Before    int64  // only entries with a smaller id
	Action    string // only this action, when set
	Limit     int
}

// ListAudit returns one page and whether more follow.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditRecord, bool, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, entry, prev_hash, hash FROM audit_log
		WHERE project_id = $1 AND ($2::bigint = 0 OR id < $2::bigint) AND ($3::text = '' OR action = $3::text)
		ORDER BY id DESC LIMIT $4`, q.ProjectID, q.Before, q.Action, q.Limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []AuditRecord{}
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.ID, &r.Entry, &r.PrevHash, &r.Hash); err != nil {
			return nil, false, err
		}
		var ent auditJSON
		if err := json.Unmarshal([]byte(r.Entry), &ent); err != nil {
			return nil, false, fmt.Errorf("audit entry %d: %w", r.ID, err)
		}
		r.ProjectID, r.Actor, r.ActorKeyID, r.Action = ent.ProjectID, ent.Actor, ent.ActorKeyID, ent.Action
		r.TargetType, r.TargetID, r.Data = ent.TargetType, ent.TargetID, ent.Data
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, ent.CreatedAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

// AuditVerification is the result of checking a project's chain.
type AuditVerification struct {
	Valid   bool   `json:"valid"`
	Entries int    `json:"entries"`
	Head    []byte `json:"-"` // the last entry's hash: keep it to detect truncation later
	// FirstInvalidID is the first entry that does not check out, and
	// Problem says why.
	FirstInvalidID *int64 `json:"first_invalid_id"`
	Problem        string `json:"problem,omitempty"`
}

// VerifyAudit recomputes the project's whole chain, oldest first.
func (s *Store) VerifyAudit(ctx context.Context, projectID string) (AuditVerification, error) {
	v := AuditVerification{Valid: true, Head: zeroHash}
	rows, err := s.Pool.Query(ctx, `SELECT id, project_id::text, entry, prev_hash, hash FROM audit_log
		WHERE project_id = $1 ORDER BY id`, projectID)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	prev := zeroHash
	for rows.Next() {
		var id int64
		var pid, entry string
		var prevHash, hash []byte
		if err := rows.Scan(&id, &pid, &entry, &prevHash, &hash); err != nil {
			return v, err
		}
		v.Entries++
		if v.Valid {
			var ent auditJSON
			problem := ""
			switch {
			case json.Unmarshal([]byte(entry), &ent) != nil:
				problem = "the entry is not valid JSON"
			case ent.ID != id || ent.ProjectID == nil || *ent.ProjectID != pid:
				problem = "the entry's id or project does not match its row"
			case !bytes.Equal(prevHash, prev):
				problem = "prev_hash is not the previous entry's hash: an entry was removed or reordered"
			case !bytes.Equal(hash, auditHash(prevHash, entry)):
				problem = "the hash does not match the entry: the entry was changed"
			}
			if problem != "" {
				v.Valid, v.Problem = false, problem
				v.FirstInvalidID = &id
			}
		}
		prev = hash
	}
	v.Head = prev
	return v, rows.Err()
}
