// Package store is the control plane's PostgreSQL layer: projects, keys,
// domains, the durable delivery queue, attempts and events.
package store

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrForbidden = errors.New("forbidden")
)

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate applies every embedded migration that has not run yet. An
// advisory lock keeps two control planes from migrating at once.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7461230001)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7461230001)`)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- projects and API keys ----

func (s *Store) CreateProject(ctx context.Context, name string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `INSERT INTO projects (name) VALUES ($1) RETURNING id::text`, name).Scan(&id)
	return id, err
}

func (s *Store) CreateAPIKey(ctx context.Context, projectID, name, prefix string, hash []byte) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO api_keys (project_id, name, prefix, token_hash) VALUES ($1, $2, $3, $4)`,
		projectID, name, prefix, hash)
	return err
}

// ProjectForKey returns the project an API key belongs to.
func (s *Store) ProjectForKey(ctx context.Context, hash []byte) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT project_id::text FROM api_keys WHERE token_hash = $1 AND revoked_at IS NULL`, hash).Scan(&id)
	return id, noRows(err)
}

// ---- workers ----

type Worker struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	IP         string     `json:"ip"`
	EHLO       string     `json:"ehlo"`
	Capacity   int        `json:"capacity"`
	Version    string     `json:"version"`
	Draining   bool       `json:"draining"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

const workerCols = `id::text, name, ip, ehlo, capacity, version, draining, last_seen_at`

func scanWorker(row pgx.Row) (Worker, error) {
	var w Worker
	err := row.Scan(&w.ID, &w.Name, &w.IP, &w.EHLO, &w.Capacity, &w.Version, &w.Draining, &w.LastSeenAt)
	return w, err
}

func (s *Store) CreateWorker(ctx context.Context, name string, hash []byte) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `INSERT INTO workers (name, token_hash) VALUES ($1, $2) RETURNING id::text`, name, hash).Scan(&id)
	if isUniqueViolation(err) {
		return "", ErrConflict
	}
	return id, err
}

func (s *Store) WorkerForToken(ctx context.Context, hash []byte) (Worker, error) {
	w, err := scanWorker(s.Pool.QueryRow(ctx, `SELECT `+workerCols+` FROM workers WHERE token_hash = $1`, hash))
	return w, noRows(err)
}

func (s *Store) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+workerCols+` FROM workers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) SetDraining(ctx context.Context, name string, draining bool) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE workers SET draining = $2 WHERE name = $1`, name, draining)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) Heartbeat(ctx context.Context, workerID string, hb protocol.HeartbeatRequest) (draining bool, err error) {
	err = s.Pool.QueryRow(ctx, `UPDATE workers SET ip = $2, ehlo = $3, capacity = $4, version = $5, last_seen_at = now()
		WHERE id = $1 RETURNING draining`, workerID, hb.PublicIP, hb.EHLO, hb.Capacity, hb.Version).Scan(&draining)
	return draining, noRows(err)
}

// WorkerIPs lists the sending IPs workers have advertised.
func (s *Store) WorkerIPs(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT ip FROM workers WHERE ip <> '' ORDER BY ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ips []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		ips = append(ips, ip)
	}
	return ips, rows.Err()
}

// ---- domains ----

type Domain struct {
	ID          string
	ProjectID   string
	Name        string
	Status      string
	Selector    string
	DKIMPublic  string
	DKIMPrivate []byte // sealed
	ReturnPath  string
	Checks      []byte
	CreatedAt   time.Time
	VerifiedAt  *time.Time
	CheckedAt   *time.Time
}

const domainCols = `id::text, project_id::text, name, status, dkim_selector, dkim_public, dkim_private,
	return_path, checks, created_at, verified_at, checked_at`

func scanDomain(row pgx.Row) (Domain, error) {
	var d Domain
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Status, &d.Selector, &d.DKIMPublic, &d.DKIMPrivate,
		&d.ReturnPath, &d.Checks, &d.CreatedAt, &d.VerifiedAt, &d.CheckedAt)
	return d, err
}

func (s *Store) CreateDomain(ctx context.Context, d Domain) (Domain, error) {
	out, err := scanDomain(s.Pool.QueryRow(ctx, `INSERT INTO domains
		(project_id, name, dkim_selector, dkim_public, dkim_private, return_path)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+domainCols,
		d.ProjectID, d.Name, d.Selector, d.DKIMPublic, d.DKIMPrivate, d.ReturnPath))
	if isUniqueViolation(err) {
		return Domain{}, ErrConflict
	}
	return out, err
}

func (s *Store) ListDomains(ctx context.Context, projectID string) ([]Domain, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+domainCols+` FROM domains WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDomain(ctx context.Context, projectID, id string) (Domain, error) {
	d, err := scanDomain(s.Pool.QueryRow(ctx, `SELECT `+domainCols+` FROM domains WHERE project_id = $1 AND id = $2`, projectID, id))
	return d, noRows(err)
}

func (s *Store) DomainByName(ctx context.Context, projectID, name string) (Domain, error) {
	d, err := scanDomain(s.Pool.QueryRow(ctx, `SELECT `+domainCols+` FROM domains WHERE project_id = $1 AND name = $2`, projectID, name))
	return d, noRows(err)
}

func (s *Store) SaveDomainCheck(ctx context.Context, id, status string, checks []byte) error {
	_, err := s.Pool.Exec(ctx, `UPDATE domains SET status = $2, checks = $3, checked_at = now(),
		verified_at = CASE WHEN $2 = 'verified' THEN COALESCE(verified_at, now()) ELSE NULL END
		WHERE id = $1`, id, status, checks)
	return err
}

// ForceVerify marks a domain verified without DNS checks. For development
// and tests only; the CLI exposes it behind an explicit flag.
func (s *Store) ForceVerify(ctx context.Context, name string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE domains SET status = 'verified', verified_at = now() WHERE name = $1`, name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) DeleteDomain(ctx context.Context, projectID, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM domains WHERE project_id = $1 AND id = $2`, projectID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---- emails ----

type NewEmail struct {
	ID             string
	ProjectID      string
	DomainID       string
	From           string
	To, Cc, Bcc    []string
	Subject        string
	MessageID      string
	ReturnPath     string
	IdempotencyKey string
	RequestHash    []byte
	Raw            []byte // sealed, signed message
	Tags           map[string]string
	RetryUncertain bool
	ExpiresAt      time.Time
	Recipients     []string // envelope recipients, lowercased and deduplicated
}

// InsertEmail commits the message and one delivery per recipient in one
// transaction. With an idempotency key, a repeat of the same request returns
// the first ID (existing = true); a different request with the same key
// returns ErrConflict.
func (s *Store) InsertEmail(ctx context.Context, e NewEmail) (id string, existing bool, err error) {
	if e.IdempotencyKey != "" {
		id, err := s.findIdempotent(ctx, e.ProjectID, e.IdempotencyKey, e.RequestHash)
		if err == nil {
			return id, true, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", false, err
		}
	}
	if e.Tags == nil {
		e.Tags = map[string]string{}
	}
	tags, err := json.Marshal(e.Tags)
	if err != nil {
		return "", false, err
	}
	nonNil := func(l []string) []string {
		if l == nil {
			return []string{}
		}
		return l
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `INSERT INTO emails (id, project_id, domain_id, from_addr, to_addrs, cc_addrs, bcc_addrs,
			subject, message_id, return_path, idempotency_key, request_hash, raw, tags, retry_uncertain, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (project_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
		e.ID, e.ProjectID, nullString(e.DomainID), e.From, nonNil(e.To), nonNil(e.Cc), nonNil(e.Bcc),
		e.Subject, e.MessageID, e.ReturnPath, nullString(e.IdempotencyKey), e.RequestHash, e.Raw, tags,
		e.RetryUncertain, e.ExpiresAt)
	if err != nil {
		return "", false, err
	}
	if tag.RowsAffected() == 0 {
		// A concurrent request with the same key won the race.
		tx.Rollback(ctx)
		id, err := s.findIdempotent(ctx, e.ProjectID, e.IdempotencyKey, e.RequestHash)
		return id, err == nil, err
	}

	suppressed := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT address FROM suppressions WHERE project_id = $1 AND address = ANY($2)`,
		e.ProjectID, e.Recipients)
	if err != nil {
		return "", false, err
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return "", false, err
		}
		suppressed[a] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}

	for _, rcpt := range e.Recipients {
		status := "queued"
		if suppressed[rcpt] {
			status = "suppressed"
		}
		var deliveryID string
		if err := tx.QueryRow(ctx, `INSERT INTO deliveries (email_id, rcpt, rcpt_domain, status)
			VALUES ($1, $2, $3, $4) RETURNING id::text`,
			e.ID, rcpt, domainOf(rcpt), status).Scan(&deliveryID); err != nil {
			return "", false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type) VALUES ($1, $2, $3)`,
			e.ID, deliveryID, status); err != nil {
			return "", false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return e.ID, false, nil
}

func (s *Store) findIdempotent(ctx context.Context, projectID, key string, hash []byte) (string, error) {
	var id string
	var h []byte
	err := s.Pool.QueryRow(ctx, `SELECT id::text, request_hash FROM emails WHERE project_id = $1 AND idempotency_key = $2`,
		projectID, key).Scan(&id, &h)
	if err != nil {
		return "", noRows(err)
	}
	if !bytes.Equal(h, hash) {
		return "", ErrConflict
	}
	return id, nil
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

type EmailView struct {
	ID         string
	From       string
	To, Cc     []string
	Bcc        []string
	Subject    string
	MessageID  string
	Tags       map[string]string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	BodyPurged bool
	Recipients []RecipientView
	Attempts   []AttemptView
	Events     []EventView
}

type RecipientView struct {
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	LastCode      *int       `json:"last_code"`
	LastResponse  string     `json:"last_response"`
}

type AttemptView struct {
	ID           string     `json:"id"`
	Recipient    string     `json:"recipient"`
	Worker       string     `json:"worker"`
	SourceIP     string     `json:"source_ip"`
	MXHost       string     `json:"mx_host"`
	Outcome      string     `json:"outcome"`
	SMTPCode     *int       `json:"smtp_code"`
	EnhancedCode string     `json:"enhanced_code"`
	Response     string     `json:"response"`
	TLS          string     `json:"tls"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

type EventView struct {
	Type      string          `json:"type"`
	Recipient string          `json:"recipient,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// GetEmail returns one message with its per-recipient state, every attempt
// and the event timeline.
func (s *Store) GetEmail(ctx context.Context, projectID, id string) (EmailView, error) {
	var v EmailView
	err := s.Pool.QueryRow(ctx, `SELECT id::text, from_addr, to_addrs, cc_addrs, bcc_addrs, subject, message_id,
			tags, created_at, expires_at, raw IS NULL
		FROM emails WHERE project_id = $1 AND id = $2`, projectID, id).
		Scan(&v.ID, &v.From, &v.To, &v.Cc, &v.Bcc, &v.Subject, &v.MessageID, &v.Tags, &v.CreatedAt, &v.ExpiresAt, &v.BodyPurged)
	if err != nil {
		return v, noRows(err)
	}

	rows, err := s.Pool.Query(ctx, `SELECT rcpt, status, attempts,
			CASE WHEN status IN ('queued', 'deferred') THEN next_attempt_at END, last_code, last_response
		FROM deliveries WHERE email_id = $1 ORDER BY rcpt`, id)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var r RecipientView
		if err := rows.Scan(&r.Email, &r.Status, &r.Attempts, &r.NextAttemptAt, &r.LastCode, &r.LastResponse); err != nil {
			rows.Close()
			return v, err
		}
		v.Recipients = append(v.Recipients, r)
	}
	rows.Close()

	rows, err = s.Pool.Query(ctx, `SELECT a.id::text, d.rcpt, COALESCE(w.name, ''), a.source_ip, a.mx_host,
			COALESCE(a.outcome, ''), a.smtp_code, a.enhanced_code, a.response, a.tls, a.started_at, a.finished_at
		FROM attempts a JOIN deliveries d ON d.id = a.delivery_id LEFT JOIN workers w ON w.id = a.worker_id
		WHERE d.email_id = $1 ORDER BY a.started_at, a.id`, id)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var a AttemptView
		if err := rows.Scan(&a.ID, &a.Recipient, &a.Worker, &a.SourceIP, &a.MXHost, &a.Outcome, &a.SMTPCode,
			&a.EnhancedCode, &a.Response, &a.TLS, &a.StartedAt, &a.FinishedAt); err != nil {
			rows.Close()
			return v, err
		}
		v.Attempts = append(v.Attempts, a)
	}
	rows.Close()

	rows, err = s.Pool.Query(ctx, `SELECT e.type, COALESCE(d.rcpt, ''), e.data, e.created_at
		FROM events e LEFT JOIN deliveries d ON d.id = e.delivery_id
		WHERE e.email_id = $1 ORDER BY e.id`, id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var e EventView
		var data []byte
		if err := rows.Scan(&e.Type, &e.Recipient, &data, &e.CreatedAt); err != nil {
			return v, err
		}
		if len(data) > 0 && string(data) != "{}" {
			e.Data = data
		}
		v.Events = append(v.Events, e)
	}
	return v, rows.Err()
}

// ---- the delivery queue ----

// LeasedJob is a job whose message is still sealed.
type LeasedJob struct {
	protocol.Job
	SealedRaw  []byte
	ReturnPath string
}

// Lease hands up to max due deliveries to a worker. Each gets a fresh attempt
// ID and an expiring lease. perDomain caps in-flight deliveries per
// recipient domain across all workers, so adding a worker never multiplies
// the rate a receiver sees.
func (s *Store) Lease(ctx context.Context, workerID string, max int, lease time.Duration, perDomain int) ([]LeasedJob, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH picked AS (
			SELECT d.id
			FROM deliveries d JOIN emails e ON e.id = d.email_id
			WHERE d.status IN ('queued', 'deferred')
			  AND d.next_attempt_at <= now()
			  AND e.expires_at > now()
			  AND e.raw IS NOT NULL
			  AND (SELECT count(*) FROM deliveries x WHERE x.status = 'sending' AND x.rcpt_domain = d.rcpt_domain) < $3
			ORDER BY d.next_attempt_at
			LIMIT $1
			FOR UPDATE OF d SKIP LOCKED
		), leased AS (
			UPDATE deliveries d SET status = 'sending', attempts = d.attempts + 1, lease_worker = $2,
				lease_attempt = gen_random_uuid(), lease_expires_at = now() + make_interval(secs => $4),
				updated_at = now()
			FROM picked p WHERE d.id = p.id
			RETURNING d.id, d.email_id, d.rcpt, d.rcpt_domain, d.lease_attempt, d.lease_expires_at
		), started AS (
			INSERT INTO attempts (id, delivery_id, worker_id)
			SELECT lease_attempt, id, $2 FROM leased
		)
		SELECT l.id::text, l.rcpt, l.rcpt_domain, l.lease_attempt::text, l.lease_expires_at, e.raw, e.return_path
		FROM leased l JOIN emails e ON e.id = l.email_id`,
		max, workerID, perDomain, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []LeasedJob
	for rows.Next() {
		var j LeasedJob
		if err := rows.Scan(&j.DeliveryID, &j.Rcpt, &j.RcptDomain, &j.AttemptID, &j.LeaseExpiresAt, &j.SealedRaw, &j.ReturnPath); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Report records a worker's result for one attempt and moves the delivery
// to its next state. A replayed report is a no-op. A late report for a lease
// the sweeper already reclaimed is recorded; if it says delivered, the
// delivery is marked delivered, because the receiver did accept it.
func (s *Store) Report(ctx context.Context, workerID string, r protocol.Report) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	var deliveryID, attemptWorker string
	var reportedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT delivery_id::text, COALESCE(worker_id::text, ''), reported_at
		FROM attempts WHERE id = $1 FOR UPDATE`, r.AttemptID).Scan(&deliveryID, &attemptWorker, &reportedAt)
	if err != nil {
		return "", noRows(err)
	}
	if attemptWorker != workerID {
		return "", ErrForbidden
	}

	var status, emailID, rcpt, projectID string
	var leaseAttempt *string
	var attempts int
	var expiresAt time.Time
	var retryUncertain bool
	err = tx.QueryRow(ctx, `SELECT d.status, d.email_id::text, d.rcpt, e.project_id::text, d.lease_attempt::text,
			d.attempts, e.expires_at, e.retry_uncertain
		FROM deliveries d JOIN emails e ON e.id = d.email_id WHERE d.id = $1 FOR UPDATE OF d`, deliveryID).
		Scan(&status, &emailID, &rcpt, &projectID, &leaseAttempt, &attempts, &expiresAt, &retryUncertain)
	if err != nil {
		return "", err
	}
	if reportedAt != nil {
		return status, nil
	}

	if _, err := tx.Exec(ctx, `UPDATE attempts SET reported_at = now(), finished_at = COALESCE(finished_at, now()),
			outcome = $2, smtp_code = $3, enhanced_code = $4, response = $5, mx_host = $6, source_ip = $7, tls = $8
		WHERE id = $1`, r.AttemptID, r.Outcome, nullInt(r.SMTPCode), truncate(r.EnhancedCode, 16),
		truncate(r.Response, 1000), truncate(r.MXHost, 255), truncate(r.SourceIP, 64), truncate(r.TLS, 16)); err != nil {
		return "", err
	}

	eventData, _ := json.Marshal(map[string]any{
		"attempt_id": r.AttemptID, "smtp_code": r.SMTPCode, "enhanced_code": r.EnhancedCode,
		"response": truncate(r.Response, 300), "mx_host": r.MXHost,
	})

	if leaseAttempt == nil || *leaseAttempt != r.AttemptID {
		if r.Outcome == protocol.OutcomeDelivered && status != "delivered" {
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = 'delivered', lease_worker = NULL, lease_attempt = NULL,
					lease_expires_at = NULL, last_code = $2, last_response = $3, updated_at = now() WHERE id = $1`,
				deliveryID, nullInt(r.SMTPCode), truncate(r.Response, 1000)); err != nil {
				return "", err
			}
			status = "delivered"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, $3, $4)`,
			emailID, deliveryID, "late_report_"+r.Outcome, eventData); err != nil {
			return "", err
		}
		return status, tx.Commit(ctx)
	}

	dec := policy.Decide(policy.Input{
		Outcome: r.Outcome, Attempts: attempts, Now: time.Now(), ExpiresAt: expiresAt,
		RetryUncertain: retryUncertain, SMTPCode: r.SMTPCode, EnhancedCode: r.EnhancedCode,
	})
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = $2, next_attempt_at = $3, lease_worker = NULL,
			lease_attempt = NULL, lease_expires_at = NULL, last_code = $4, last_response = $5, updated_at = now()
		WHERE id = $1`, deliveryID, dec.Status, dec.NextAttempt, nullInt(r.SMTPCode), truncate(r.Response, 1000)); err != nil {
		return "", err
	}
	if dec.Suppress {
		if _, err := tx.Exec(ctx, `INSERT INTO suppressions (project_id, address, reason, detail)
			VALUES ($1, $2, 'hard_bounce', $3) ON CONFLICT DO NOTHING`,
			projectID, rcpt, truncate(r.Response, 300)); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, $3, $4)`,
		emailID, deliveryID, dec.Event, eventData); err != nil {
		return "", err
	}
	return dec.Status, tx.Commit(ctx)
}

// Sweep reclaims expired leases, expires overdue deliveries and purges
// message bodies past retention. The control plane runs it periodically.
func (s *Store) Sweep(ctx context.Context, retention time.Duration) error {
	if err := s.reclaimLeases(ctx); err != nil {
		return fmt.Errorf("reclaim leases: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `
		WITH expired AS (
			UPDATE deliveries d SET status = 'expired', updated_at = now()
			FROM emails e
			WHERE e.id = d.email_id AND d.status IN ('queued', 'deferred') AND e.expires_at <= now()
			RETURNING d.id, d.email_id
		)
		INSERT INTO events (email_id, delivery_id, type) SELECT email_id, id, 'expired' FROM expired`); err != nil {
		return fmt.Errorf("expire: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE emails SET raw = NULL, purged_at = now()
		WHERE raw IS NOT NULL AND created_at < now() - make_interval(secs => $1)
		  AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.email_id = emails.id
		                  AND d.status IN ('queued', 'sending', 'deferred'))`, retention.Seconds()); err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	// Finished webhook deliveries hold addresses and subjects; keep them only
	// as long as message bodies.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM webhook_deliveries
		WHERE status <> 'pending' AND created_at < now() - make_interval(secs => $1)`, retention.Seconds()); err != nil {
		return fmt.Errorf("purge webhook deliveries: %w", err)
	}
	return nil
}

// reclaimLeases handles workers that never reported. The receiver may or
// may not have the message, so the attempt is recorded as unknown and the
// message's retry_uncertain setting decides whether it is sent again.
func (s *Store) reclaimLeases(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT d.id::text, d.email_id::text, d.lease_attempt::text, d.attempts,
			e.expires_at, e.retry_uncertain
		FROM deliveries d JOIN emails e ON e.id = d.email_id
		WHERE d.status = 'sending' AND d.lease_expires_at < now()
		FOR UPDATE OF d SKIP LOCKED`)
	if err != nil {
		return err
	}
	type lost struct {
		id, emailID, attempt string
		attempts             int
		expires              time.Time
		retry                bool
	}
	var all []lost
	for rows.Next() {
		var l lost
		var attempt *string
		if err := rows.Scan(&l.id, &l.emailID, &attempt, &l.attempts, &l.expires, &l.retry); err != nil {
			rows.Close()
			return err
		}
		if attempt != nil {
			l.attempt = *attempt
		}
		all = append(all, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range all {
		dec := policy.Decide(policy.Input{
			Outcome: protocol.OutcomeUnknown, Attempts: l.attempts, Now: time.Now(),
			ExpiresAt: l.expires, RetryUncertain: l.retry,
		})
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = $2, next_attempt_at = $3, lease_worker = NULL,
				lease_attempt = NULL, lease_expires_at = NULL,
				last_response = 'lease expired before the worker reported; outcome unknown', updated_at = now()
			WHERE id = $1`, l.id, dec.Status, dec.NextAttempt); err != nil {
			return err
		}
		if l.attempt != "" {
			if _, err := tx.Exec(ctx, `UPDATE attempts SET outcome = 'unknown', finished_at = now(),
					response = 'lease expired before the worker reported'
				WHERE id = $1 AND reported_at IS NULL`, l.attempt); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type) VALUES ($1, $2, 'lease_expired')`,
			l.emailID, l.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
