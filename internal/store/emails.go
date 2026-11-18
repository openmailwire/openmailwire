package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// querier is what both the pool and a transaction offer.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ---- send caps ----

// SendCaps are an API key's limits on recipients queued in a rolling hour
// and a rolling 24 hours. Zero means no cap.
type SendCaps struct {
	ProjectID string
	KeyID     string
	PerHour   int
	PerDay    int
}

func (c SendCaps) active() bool { return c.KeyID != "" && (c.PerHour > 0 || c.PerDay > 0) }

// CapError refuses a send that would take a key over one of its caps.
type CapError struct {
	Field      string // "max_per_hour" or "max_per_day"
	Window     time.Duration
	Limit      int
	Used       int // recipients already counted in the window
	Adding     int // recipients this request would add
	RetryAfter time.Duration
}

func (e *CapError) Error() string {
	return fmt.Sprintf("%s of %d reached (%d used, %d more requested)", e.Field, e.Limit, e.Used, e.Adding)
}

// CheckCaps reports whether adding more recipients would take the key over
// a cap. It takes no lock: it is the early check before a message is built.
// InsertEmails checks again under a lock.
func (s *Store) CheckCaps(ctx context.Context, c SendCaps, adding int) (*CapError, error) {
	if !c.active() || adding <= 0 {
		return nil, nil
	}
	return checkCaps(ctx, s.Pool, c, adding)
}

// checkCaps sums the recipients the key queued in each window with one
// range scan of emails_project_key_created. When a cap is exceeded it also
// works out when enough of them leave the window.
func checkCaps(ctx context.Context, q querier, c SendCaps, adding int) (*CapError, error) {
	longest := time.Hour
	if c.PerDay > 0 {
		longest = 24 * time.Hour
	}
	var hour, day int
	if err := q.QueryRow(ctx, `SELECT
			COALESCE(sum(quota_rcpts) FILTER (WHERE created_at > now() - interval '1 hour'), 0),
			COALESCE(sum(quota_rcpts), 0)
		FROM emails
		WHERE project_id = $1 AND api_key_id = $2 AND created_at > now() - make_interval(secs => $3)`,
		c.ProjectID, c.KeyID, longest.Seconds()).Scan(&hour, &day); err != nil {
		return nil, err
	}
	var worst *CapError
	for _, w := range []struct {
		field       string
		window      time.Duration
		limit, used int
	}{
		{"max_per_hour", time.Hour, c.PerHour, hour},
		{"max_per_day", 24 * time.Hour, c.PerDay, day},
	} {
		if w.limit <= 0 || w.used+adding <= w.limit {
			continue
		}
		ce := &CapError{Field: w.field, Window: w.window, Limit: w.limit, Used: w.used, Adding: adding, RetryAfter: w.window}
		if adding <= w.limit {
			wait, err := capFreesAt(ctx, q, c, w.window, w.used+adding-w.limit)
			if err != nil {
				return nil, err
			}
			if wait > 0 {
				ce.RetryAfter = wait
			}
		}
		if worst == nil || ce.RetryAfter > worst.RetryAfter {
			worst = ce
		}
	}
	return worst, nil
}

// capFreesAt returns how long until the oldest recipients totalling at
// least n leave the window, or 0 when that cannot be worked out.
func capFreesAt(ctx context.Context, q querier, c SendCaps, window time.Duration, n int) (time.Duration, error) {
	var secs float64
	err := q.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM created_at + make_interval(secs => $3) - clock_timestamp())::float8
		FROM (
			SELECT created_at, sum(quota_rcpts) OVER (ORDER BY created_at, id) AS running
			FROM emails
			WHERE project_id = $1 AND api_key_id = $2 AND created_at > now() - make_interval(secs => $3)
		) w
		WHERE running >= $4 ORDER BY created_at LIMIT 1`, c.ProjectID, c.KeyID, window.Seconds(), n).Scan(&secs)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return time.Duration(math.Max(1, math.Ceil(secs))) * time.Second, nil
}

// CountUnsuppressed counts the recipients that are not on the project's
// suppression list. Duplicates count each time.
func (s *Store) CountUnsuppressed(ctx context.Context, projectID string, rcpts []string) (int, error) {
	if len(rcpts) == 0 {
		return 0, nil
	}
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM unnest($2::text[]) AS r(a)
		WHERE NOT EXISTS (SELECT 1 FROM suppressions s WHERE s.project_id = $1 AND s.address = r.a)`,
		projectID, rcpts).Scan(&n)
	return n, err
}

// ---- cancel ----

// CancelEmail cancels every delivery of the email that is still queued and
// has never been attempted, records a canceled event for each, and returns
// those recipients (none when nothing could be canceled). With keyID set,
// only that key's emails are found. ErrNotFound when there is no such
// email.
func (s *Store) CancelEmail(ctx context.Context, projectID, id, keyID string) ([]string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	q := `SELECT 1 FROM emails WHERE project_id = $1 AND id = $2`
	args := []any{projectID, id}
	if keyID != "" {
		q += ` AND api_key_id = $3`
		args = append(args, keyID)
	}
	var one int
	if err := tx.QueryRow(ctx, q+` FOR UPDATE`, args...).Scan(&one); err != nil {
		return nil, noRows(err)
	}
	rows, err := tx.Query(ctx, `
		WITH c AS (
			UPDATE deliveries SET status = 'canceled', updated_at = now()
			WHERE email_id = $1 AND status = 'queued' AND attempts = 0
			RETURNING id, rcpt
		), ev AS (
			INSERT INTO events (email_id, delivery_id, type) SELECT $1, id, 'canceled' FROM c
		)
		SELECT rcpt FROM c ORDER BY rcpt`, id)
	if err != nil {
		return nil, err
	}
	canceled := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, err
		}
		canceled = append(canceled, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(canceled) > 0 {
		// Canceled recipients no longer count toward the key's caps.
		if _, err := tx.Exec(ctx, `UPDATE emails SET quota_rcpts = GREATEST(quota_rcpts - $2, 0) WHERE id = $1`,
			id, len(canceled)); err != nil {
			return nil, err
		}
	}
	return canceled, tx.Commit(ctx)
}

// ---- list ----

// EmailSummary is one row of the email list.
type EmailSummary struct {
	ID          string     `json:"id"`
	From        string     `json:"from"`
	To          []string   `json:"to"`
	Subject     string     `json:"subject"`
	CreatedAt   time.Time  `json:"created_at"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	LastEvent   string     `json:"last_event"`
	APIKeyID    *string    `json:"api_key_id"`
}

// ListEmailsQuery selects a page of a project's emails, newest first.
type ListEmailsQuery struct {
	ProjectID string
	APIKeyID  string // only emails queued by this key, when set
	Before    string // an email id: only emails older than it
	Limit     int
}

// ListEmails returns one page and whether more follow. ErrNotFound when
// Before names no email that the query could return.
func (s *Store) ListEmails(ctx context.Context, q ListEmailsQuery) ([]EmailSummary, bool, error) {
	where := `e.project_id = $1`
	args := []any{q.ProjectID}
	if q.APIKeyID != "" {
		args = append(args, q.APIKeyID)
		where += fmt.Sprintf(` AND e.api_key_id = $%d`, len(args))
	}
	if q.Before != "" {
		var at time.Time
		if err := s.Pool.QueryRow(ctx, `SELECT e.created_at FROM emails e WHERE `+where+
			fmt.Sprintf(` AND e.id = $%d`, len(args)+1), append(args, q.Before)...).Scan(&at); err != nil {
			return nil, false, noRows(err)
		}
		args = append(args, at, q.Before)
		where += fmt.Sprintf(` AND (e.created_at, e.id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, q.Limit+1)
	rows, err := s.Pool.Query(ctx, `SELECT e.id::text, e.from_addr, e.to_addrs, e.subject, e.created_at, e.scheduled_at,
			COALESCE((SELECT ev.type FROM events ev WHERE ev.email_id = e.id ORDER BY ev.id DESC LIMIT 1), ''),
			e.api_key_id::text
		FROM emails e WHERE `+where+fmt.Sprintf(` ORDER BY e.created_at DESC, e.id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []EmailSummary{}
	for rows.Next() {
		var v EmailSummary
		if err := rows.Scan(&v.ID, &v.From, &v.To, &v.Subject, &v.CreatedAt, &v.ScheduledAt, &v.LastEvent, &v.APIKeyID); err != nil {
			return nil, false, err
		}
		out = append(out, v)
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
