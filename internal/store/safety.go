package store

// Agent safety (v0.6, lane B): approval holds, streams and per-stream
// suppressions, contacts, the per-recipient frequency cap, project
// settings and circuit-breaker pauses. The breaker counters for bounces and
// complaints are kept by a trigger (migration 0007), so every path that
// records one counts it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Streams. Transactional mail is what a recipient expects (receipts,
// codes, replies); broadcast mail goes to many and must offer one-click
// unsubscribe.
const (
	StreamTransactional = "transactional"
	StreamBroadcast     = "broadcast"
)

// Suppression streams: what a suppression applies to.
const (
	SuppressAll       = "all"
	SuppressBroadcast = "broadcast"
)

// ErrNotHeld means the email has no recipient waiting for approval.
var ErrNotHeld = errors.New("not held")

// ErrPurged means the email's message is gone, so it cannot be sent.
var ErrPurged = errors.New("message purged")

// Hold marks a NewEmail as held for approval: its unsuppressed recipients
// are stored as 'held' instead of 'queued'.
type Hold struct {
	Reasons []string // the require_approval rules that matched
}

// suppressionStreams lists the suppression streams that stop mail of the
// given stream.
func suppressionStreams(stream string) []string {
	if stream == StreamBroadcast {
		return []string{SuppressAll, SuppressBroadcast}
	}
	return []string{SuppressAll}
}

func streamOf(e NewEmail) string {
	if e.Stream == "" {
		return StreamTransactional
	}
	return e.Stream
}

// insertHold records why a held email waits, inside InsertEmails'
// transaction, once its deliveries exist. An email whose recipients are all
// suppressed has nothing to hold.
func insertHold(ctx context.Context, tx pgx.Tx, e NewEmail) error {
	reasons := e.Hold.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO email_holds (email_id, project_id, api_key_id, reasons, stream)
		SELECT $1, $2, $3, $4, $5
		WHERE EXISTS (SELECT 1 FROM deliveries WHERE email_id = $1 AND status = 'held')`,
		e.ID, e.ProjectID, nullString(e.APIKeyID), reasons, streamOf(e))
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	data, _ := json.Marshal(map[string]any{"reasons": reasons})
	_, err = tx.Exec(ctx, `UPDATE events SET data = $2 WHERE email_id = $1 AND type = 'held'`, e.ID, data)
	return err
}

// ---- approvals ----

// HeldEmail is an email waiting for approval.
type HeldEmail struct {
	ID          string
	APIKeyID    *string
	From        string
	To, Cc, Bcc []string
	Subject     string
	Reasons     []string
	Stream      string
	Recipients  []string // the held ones
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ScheduledAt *time.Time
	Raw         []byte // sealed; nil once purged
}

// ListHeld returns a page of the project's emails waiting for approval,
// newest first: only keyID's when keyID is set. before is an email id from
// the previous page. ErrNotFound when before names no such email.
func (s *Store) ListHeld(ctx context.Context, projectID, keyID, before string, limit int) ([]HeldEmail, bool, error) {
	where := `h.project_id = $1 AND h.decision IS NULL AND e.expires_at > now()`
	args := []any{projectID}
	if keyID != "" {
		args = append(args, keyID)
		where += fmt.Sprintf(` AND h.api_key_id = $%d`, len(args))
	}
	if before != "" {
		var at time.Time
		if err := s.Pool.QueryRow(ctx, `SELECT h.created_at FROM email_holds h JOIN emails e ON e.id = h.email_id
			WHERE `+where+fmt.Sprintf(` AND h.email_id = $%d`, len(args)+1), append(args, before)...).Scan(&at); err != nil {
			return nil, false, noRows(err)
		}
		args = append(args, at, before)
		where += fmt.Sprintf(` AND (h.created_at, h.email_id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := s.Pool.Query(ctx, `SELECT e.id::text, h.api_key_id::text, e.from_addr, e.to_addrs, e.cc_addrs, e.bcc_addrs,
			e.subject, h.reasons, h.stream, h.created_at, e.expires_at, e.scheduled_at, e.raw,
			ARRAY(SELECT d.rcpt FROM deliveries d WHERE d.email_id = e.id AND d.status = 'held' ORDER BY d.rcpt)
		FROM email_holds h JOIN emails e ON e.id = h.email_id
		WHERE `+where+fmt.Sprintf(` ORDER BY h.created_at DESC, h.email_id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []HeldEmail{}
	for rows.Next() {
		var h HeldEmail
		if err := rows.Scan(&h.ID, &h.APIKeyID, &h.From, &h.To, &h.Cc, &h.Bcc, &h.Subject, &h.Reasons, &h.Stream,
			&h.CreatedAt, &h.ExpiresAt, &h.ScheduledAt, &h.Raw, &h.Recipients); err != nil {
			return nil, false, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// HoldInfo is what approving or rejecting an email needs to know first.
type HoldInfo struct {
	EmailID    string
	APIKeyID   *string // the key that sent it
	DomainID   *string
	DomainName string
	Stream     string
	Decision   *string // nil while it waits
	Held       int     // recipients still held
}

// GetHold returns the hold of one email of the project. ErrNotFound when
// the email does not exist; Held is 0 when nothing waits.
func (s *Store) GetHold(ctx context.Context, projectID, emailID string) (HoldInfo, error) {
	var h HoldInfo
	err := s.Pool.QueryRow(ctx, `SELECT e.id::text, e.api_key_id::text, e.domain_id::text, COALESCE(dm.name, ''),
			COALESCE(h.stream, 'transactional'), h.decision,
			(SELECT count(*) FROM deliveries d WHERE d.email_id = e.id AND d.status = 'held')::int
		FROM emails e LEFT JOIN email_holds h ON h.email_id = e.id LEFT JOIN domains dm ON dm.id = e.domain_id
		WHERE e.project_id = $1 AND e.id = $2`, projectID, emailID).
		Scan(&h.EmailID, &h.APIKeyID, &h.DomainID, &h.DomainName, &h.Stream, &h.Decision, &h.Held)
	return h, noRows(err)
}

// Decision is the outcome of approving or rejecting a held email.
type Decision struct {
	Recipients []string // approved (now queued) or rejected
	Suppressed []string // approved, but suppressed since they were held
}

// ApproveEmail releases every held recipient of the email for delivery,
// recording an 'approved' event (data: approved_by, note). A recipient
// suppressed in the meantime becomes 'suppressed' instead. ErrNotHeld when
// nothing waits (decided, expired or never held); ErrPurged when the
// message body is gone.
func (s *Store) ApproveEmail(ctx context.Context, projectID, emailID, approverKeyID, note string) (Decision, error) {
	return s.decide(ctx, projectID, emailID, approverKeyID, note, true)
}

// RejectEmail rejects every held recipient of the email: they become
// 'rejected' with a 'rejected' event (data: rejected_by, note), stop
// counting toward the key's caps and the per-recipient cap. ErrNotHeld when
// nothing waits.
func (s *Store) RejectEmail(ctx context.Context, projectID, emailID, keyID, note string) (Decision, error) {
	return s.decide(ctx, projectID, emailID, keyID, note, false)
}

func (s *Store) decide(ctx context.Context, projectID, emailID, keyID, note string, approve bool) (Decision, error) {
	var out Decision
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	var purged, live bool
	var stream *string
	err = tx.QueryRow(ctx, `SELECT e.raw IS NULL, e.expires_at > now(), h.stream
		FROM emails e LEFT JOIN email_holds h ON h.email_id = e.id
		WHERE e.project_id = $1 AND e.id = $2 FOR UPDATE OF e`, projectID, emailID).Scan(&purged, &live, &stream)
	if err != nil {
		return out, noRows(err)
	}
	// The sweeper takes hold rows with SKIP LOCKED, so it never expires a
	// hold being decided.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM email_holds WHERE email_id = $1 FOR UPDATE`, emailID); err != nil {
		return out, err
	}
	if !live {
		return out, ErrNotHeld
	}
	if approve && purged {
		return out, ErrPurged
	}
	st := StreamTransactional
	if stream != nil {
		st = *stream
	}
	who := "rejected_by"
	if approve {
		who = "approved_by"
	}
	data, _ := json.Marshal(map[string]any{who: keyID, "note": note})

	var rows pgx.Rows
	if approve {
		rows, err = tx.Query(ctx, `
			WITH held AS (
				SELECT d.id, d.rcpt, EXISTS (SELECT 1 FROM suppressions s WHERE s.project_id = $2
					AND s.address = d.rcpt AND s.stream = ANY($3)) AS suppressed
				FROM deliveries d WHERE d.email_id = $1 AND d.status = 'held' FOR UPDATE OF d
			), upd AS (
				UPDATE deliveries d SET status = CASE WHEN held.suppressed THEN 'suppressed' ELSE 'queued' END,
					next_attempt_at = GREATEST(COALESCE((SELECT scheduled_at FROM emails WHERE id = $1), now()), now()),
					updated_at = now()
				FROM held WHERE d.id = held.id
				RETURNING d.id, d.rcpt, d.status
			), ev AS (
				INSERT INTO events (email_id, delivery_id, type, data)
				SELECT $1, id, CASE WHEN status = 'suppressed' THEN 'suppressed' ELSE 'approved' END,
					CASE WHEN status = 'suppressed' THEN '{}'::jsonb ELSE $4::jsonb END FROM upd
			)
			SELECT rcpt, status FROM upd ORDER BY rcpt`, emailID, projectID, suppressionStreams(st), data)
	} else {
		rows, err = tx.Query(ctx, `
			WITH upd AS (
				UPDATE deliveries SET status = 'rejected', updated_at = now()
				WHERE email_id = $1 AND status = 'held'
				RETURNING id, rcpt, status
			), ev AS (
				INSERT INTO events (email_id, delivery_id, type, data) SELECT $1, id, 'rejected', $2::jsonb FROM upd
			)
			SELECT rcpt, status FROM upd ORDER BY rcpt`, emailID, data)
	}
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var rcpt, status string
		if err := rows.Scan(&rcpt, &status); err != nil {
			rows.Close()
			return out, err
		}
		if status == "suppressed" {
			out.Suppressed = append(out.Suppressed, rcpt)
		} else {
			out.Recipients = append(out.Recipients, rcpt)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Recipients)+len(out.Suppressed) == 0 {
		return out, ErrNotHeld
	}
	decision := "rejected"
	if approve {
		decision = "approved"
	}
	if _, err := tx.Exec(ctx, `UPDATE email_holds SET decision = $2, decided_by = $3, decided_at = now(), note = $4
		WHERE email_id = $1`, emailID, decision, nullString(keyID), truncate(note, 1000)); err != nil {
		return out, err
	}
	n := len(out.Suppressed)
	if !approve {
		n += len(out.Recipients)
	}
	if n > 0 {
		// Rejected and newly suppressed recipients no longer count toward
		// the key's caps.
		if _, err := tx.Exec(ctx, `UPDATE emails SET quota_rcpts = GREATEST(quota_rcpts - $2, 0) WHERE id = $1`, emailID, n); err != nil {
			return out, err
		}
	}
	if !approve {
		if _, err := tx.Exec(ctx, `DELETE FROM recipient_sends WHERE email_id = $1`, emailID); err != nil {
			return out, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	if approve && len(out.Recipients) > 0 {
		return out, s.RecordQueued(ctx, []string{emailID})
	}
	return out, nil
}

// SweepSafety expires held emails past their expires_at (event 'expired'
// with {"held": true}) and drops frequency-cap rows and breaker counters
// that no window reads any more. The control plane runs it with Sweep.
func (s *Store) SweepSafety(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, `
		WITH due AS (
			SELECT h.email_id FROM email_holds h JOIN emails e ON e.id = h.email_id
			WHERE h.decision IS NULL AND e.expires_at <= now()
			LIMIT 1000 FOR UPDATE OF h SKIP LOCKED
		), upd AS (
			UPDATE deliveries d SET status = 'expired', updated_at = now()
			FROM due WHERE d.email_id = due.email_id AND d.status = 'held'
			RETURNING d.id, d.email_id
		), ev AS (
			INSERT INTO events (email_id, delivery_id, type, data) SELECT email_id, id, 'expired', '{"held": true}' FROM upd
		)
		UPDATE email_holds SET decision = 'expired', decided_at = now() WHERE email_id IN (SELECT email_id FROM due)`); err != nil {
		return fmt.Errorf("expire held: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM recipient_sends WHERE sent_at < now() - interval '24 hours'`); err != nil {
		return fmt.Errorf("prune recipient sends: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM safety_counters WHERE hour < now() - interval '48 hours'`); err != nil {
		return fmt.Errorf("prune safety counters: %w", err)
	}
	return nil
}

// ---- contacts and breaker counts ----

// NewContacts returns the recipients the project has never queued mail to.
func (s *Store) NewContacts(ctx context.Context, projectID string, rcpts []string) ([]string, error) {
	if len(rcpts) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT a FROM unnest($2::text[]) AS r(a)
		WHERE NOT EXISTS (SELECT 1 FROM contacts c WHERE c.project_id = $1 AND c.address = r.a) ORDER BY a`, projectID, rcpts)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RecordQueued counts the stored emails' recipients that will be sent:
// they become contacts, and count toward the breakers of their key, domain
// and project. It runs after the emails are committed, so the shared
// counters are locked only for one statement.
func (s *Store) RecordQueued(ctx context.Context, emailIDs []string) error {
	if len(emailIDs) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
		WITH q AS (
			SELECT e.project_id, e.api_key_id, e.domain_id, d.rcpt
			FROM deliveries d JOIN emails e ON e.id = d.email_id
			WHERE d.email_id = ANY($1::uuid[]) AND d.status NOT IN ('suppressed', 'held', 'rejected', 'canceled', 'expired')
		), c AS (
			INSERT INTO contacts (project_id, address) SELECT DISTINCT project_id, rcpt FROM q ON CONFLICT DO NOTHING
		), n AS (
			SELECT s.scope, s.id, q.project_id, count(*)::int AS k
			FROM q CROSS JOIN LATERAL (VALUES ('api_key', q.api_key_id), ('domain', q.domain_id), ('project', q.project_id)) s(scope, id)
			WHERE s.id IS NOT NULL GROUP BY s.scope, s.id, q.project_id
		)
		INSERT INTO safety_counters AS c (scope, scope_id, hour, project_id, recipients)
		SELECT scope, id, date_trunc('hour', now()), project_id, k FROM n ORDER BY scope, id
		ON CONFLICT (scope, scope_id, hour) DO UPDATE SET recipients = c.recipients + EXCLUDED.recipients`, emailIDs)
	return err
}

// ---- per-recipient frequency cap ----

// Reservation is one email's claim on the per-recipient cap.
type Reservation struct {
	EmailID    string
	Stream     string
	Recipients []string
}

// RecipientCapError refuses a send that would mail an address more often
// than the project's max_per_recipient_per_day.
type RecipientCapError struct {
	Address    string
	Limit      int
	Used       int // sends to it in the last 24 hours
	Adding     int
	RetryAfter time.Duration // 0 when waiting cannot help
}

func (e *RecipientCapError) Error() string {
	return fmt.Sprintf("max_per_recipient_per_day of %d reached (%d sent, %d more requested)", e.Limit, e.Used, e.Adding)
}

// ReserveRecipients checks the per-recipient cap for the emails about to
// be stored and, with reserve, records their sends, all under a project
// lock so concurrent requests cannot overshoot together. Suppressed
// recipients do not count. A refusal is a *RecipientCapError and records
// nothing. Without reserve it is the early, lock-free check.
func (s *Store) ReserveRecipients(ctx context.Context, projectID string, limit int, rs []Reservation, reserve bool) (*RecipientCapError, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if reserve {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7461232, hashtext($1))`, projectID); err != nil {
			return nil, err
		}
	}
	adding := map[string]int{}
	type row struct{ addr, emailID string }
	var ins []row
	for _, r := range rs {
		sup, err := suppressedAmong(ctx, tx, projectID, r.Recipients, r.Stream)
		if err != nil {
			return nil, err
		}
		for _, a := range r.Recipients {
			if !sup[a] {
				adding[a]++
				ins = append(ins, row{a, r.EmailID})
			}
		}
	}
	if len(adding) == 0 {
		return nil, nil
	}
	addrs := make([]string, 0, len(adding))
	for a := range adding {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	rows, err := tx.Query(ctx, `SELECT address, sent_at FROM recipient_sends
		WHERE project_id = $1 AND address = ANY($2) AND sent_at > now() - interval '24 hours'
		ORDER BY address, sent_at`, projectID, addrs)
	if err != nil {
		return nil, err
	}
	sent := map[string][]time.Time{}
	for rows.Next() {
		var a string
		var at time.Time
		if err := rows.Scan(&a, &at); err != nil {
			rows.Close()
			return nil, err
		}
		sent[a] = append(sent[a], at)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, a := range addrs {
		used, add := len(sent[a]), adding[a]
		if used+add <= limit {
			continue
		}
		ce := &RecipientCapError{Address: a, Limit: limit, Used: used, Adding: add}
		if add <= limit {
			// The sends that must leave the window first, oldest first.
			at := sent[a][used+add-limit-1]
			ce.RetryAfter = max(time.Until(at.Add(24*time.Hour)).Round(time.Second), time.Second)
		}
		return ce, nil
	}
	if !reserve {
		return nil, nil
	}
	for _, r := range ins {
		if _, err := tx.Exec(ctx, `INSERT INTO recipient_sends (project_id, address, email_id) VALUES ($1, $2, $3)`,
			projectID, r.addr, r.emailID); err != nil {
			return nil, err
		}
	}
	return nil, tx.Commit(ctx)
}

// ReleaseRecipients removes the per-recipient sends recorded for emails
// that were not stored after all.
func (s *Store) ReleaseRecipients(ctx context.Context, emailIDs []string) error {
	if len(emailIDs) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM recipient_sends WHERE email_id = ANY($1::uuid[])`, emailIDs)
	return err
}

// ---- project settings ----

// ProjectSettings are the project-wide sending rules.
type ProjectSettings struct {
	ProjectID             string
	Name                  string
	MaxPerRecipientPerDay *int
	BreakerEnabled        bool
	BounceRate            float64
	ComplaintRate         float64
	MinRecipients         int
	UpdatedAt             *time.Time
}

// GetProjectSettings returns the project's settings, with the defaults
// for what was never set.
func (s *Store) GetProjectSettings(ctx context.Context, projectID string) (ProjectSettings, error) {
	p := ProjectSettings{ProjectID: projectID}
	err := s.Pool.QueryRow(ctx, `SELECT p.name, ps.max_per_recipient_per_day, COALESCE(ps.breaker_enabled, true),
			COALESCE(ps.breaker_bounce_rate, 0.05), COALESCE(ps.breaker_complaint_rate, 0.001),
			COALESCE(ps.breaker_min_recipients, 50), ps.updated_at
		FROM projects p LEFT JOIN project_settings ps ON ps.project_id = p.id WHERE p.id = $1`, projectID).
		Scan(&p.Name, &p.MaxPerRecipientPerDay, &p.BreakerEnabled, &p.BounceRate, &p.ComplaintRate, &p.MinRecipients, &p.UpdatedAt)
	return p, noRows(err)
}

// SaveProjectSettings stores every setting of p.
func (s *Store) SaveProjectSettings(ctx context.Context, p ProjectSettings) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO project_settings (project_id, max_per_recipient_per_day, breaker_enabled,
			breaker_bounce_rate, breaker_complaint_rate, breaker_min_recipients)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_id) DO UPDATE SET max_per_recipient_per_day = EXCLUDED.max_per_recipient_per_day,
			breaker_enabled = EXCLUDED.breaker_enabled, breaker_bounce_rate = EXCLUDED.breaker_bounce_rate,
			breaker_complaint_rate = EXCLUDED.breaker_complaint_rate, breaker_min_recipients = EXCLUDED.breaker_min_recipients,
			updated_at = now()`,
		p.ProjectID, p.MaxPerRecipientPerDay, p.BreakerEnabled, p.BounceRate, p.ComplaintRate, p.MinRecipients)
	return err
}

// MaxPerRecipientPerDay returns the project's per-recipient cap, 0 for none.
func (s *Store) MaxPerRecipientPerDay(ctx context.Context, projectID string) (int, error) {
	var n *int
	err := s.Pool.QueryRow(ctx, `SELECT max_per_recipient_per_day FROM project_settings WHERE project_id = $1`, projectID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) || n == nil {
		return 0, nil
	}
	return *n, err
}

// ---- circuit-breaker pauses ----

// Pause is a tripped circuit breaker.
type Pause struct {
	ID         string     `json:"id"`
	Scope      string     `json:"scope"` // api_key, domain or project
	ScopeID    string     `json:"scope_id"`
	ScopeName  string     `json:"scope_name"`
	Reason     string     `json:"reason"` // bounce_rate or complaint_rate
	Rate       float64    `json:"rate"`
	Threshold  float64    `json:"threshold"`
	Recipients int        `json:"recipients"`
	Bounces    int        `json:"bounces"`
	Complaints int        `json:"complaints"`
	CreatedAt  time.Time  `json:"created_at"`
	ResumedAt  *time.Time `json:"resumed_at"`
	ResumedBy  *string    `json:"resumed_by"`
}

const pauseCols = `id::text, scope, scope_id::text, scope_name, reason, rate, threshold, recipients, bounces, complaints,
	created_at, resumed_at, resumed_by::text`

func scanPause(row pgx.Row) (Pause, error) {
	var p Pause
	err := row.Scan(&p.ID, &p.Scope, &p.ScopeID, &p.ScopeName, &p.Reason, &p.Rate, &p.Threshold, &p.Recipients,
		&p.Bounces, &p.Complaints, &p.CreatedAt, &p.ResumedAt, &p.ResumedBy)
	return p, err
}

// ActivePause returns a pause that stops this send: of the project, of the
// key, or of one of the sending domains (by id or name). nil when none.
func (s *Store) ActivePause(ctx context.Context, projectID, keyID string, domainIDs, domainNames []string) (*Pause, error) {
	if domainIDs == nil {
		domainIDs = []string{}
	}
	if domainNames == nil {
		domainNames = []string{}
	}
	p, err := scanPause(s.Pool.QueryRow(ctx, `SELECT `+pauseCols+` FROM sending_pauses
		WHERE project_id = $1 AND resumed_at IS NULL AND (scope = 'project'
			OR (scope = 'api_key' AND scope_id::text = $2)
			OR (scope = 'domain' AND (scope_id::text = ANY($3) OR scope_name = ANY($4))))
		ORDER BY CASE scope WHEN 'project' THEN 0 WHEN 'domain' THEN 1 ELSE 2 END, created_at LIMIT 1`,
		projectID, keyID, domainIDs, domainNames))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPauses returns the project's pauses, newest first: only active ones
// with activeOnly.
func (s *Store) ListPauses(ctx context.Context, projectID string, activeOnly bool, limit int) ([]Pause, error) {
	q := `SELECT ` + pauseCols + ` FROM sending_pauses WHERE project_id = $1`
	if activeOnly {
		q += ` AND resumed_at IS NULL`
	}
	rows, err := s.Pool.Query(ctx, q+` ORDER BY created_at DESC, id LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pause{}
	for rows.Next() {
		p, err := scanPause(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPause returns one pause of the project.
func (s *Store) GetPause(ctx context.Context, projectID, id string) (Pause, error) {
	p, err := scanPause(s.Pool.QueryRow(ctx, `SELECT `+pauseCols+` FROM sending_pauses WHERE project_id = $1 AND id = $2`,
		projectID, id))
	return p, noRows(err)
}

// ResumePause ends an active pause and clears its scope's counters, so its
// rates start over. ErrNotFound when there is no such pause, ErrConflict
// when it was already resumed.
func (s *Store) ResumePause(ctx context.Context, projectID, id, keyID string) (Pause, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Pause{}, err
	}
	defer tx.Rollback(ctx)
	p, err := scanPause(tx.QueryRow(ctx, `UPDATE sending_pauses SET resumed_at = now(), resumed_by = $3
		WHERE project_id = $1 AND id = $2 AND resumed_at IS NULL RETURNING `+pauseCols, projectID, id, nullString(keyID)))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.GetPause(ctx, projectID, id); gerr != nil {
			return Pause{}, gerr
		}
		return Pause{}, ErrConflict
	}
	if err != nil {
		return Pause{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM safety_counters WHERE scope = $1 AND scope_id = $2`, p.Scope, p.ScopeID); err != nil {
		return Pause{}, err
	}
	return p, tx.Commit(ctx)
}

// ---- suppressions ----

// Suppression is one address the project does not send to.
type Suppression struct {
	Address   string    `json:"address"`
	Stream    string    `json:"stream"` // all or broadcast
	Reason    string    `json:"reason"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// SuppressionCursor is the position after the last row of a page.
type SuppressionCursor struct {
	CreatedAt time.Time
	Address   string
	Stream    string
}

// SuppressionQuery selects a page of a project's suppressions, newest
// first. Empty filters match everything.
type SuppressionQuery struct {
	ProjectID string
	Reason    string
	Stream    string
	Address   string // exact
	Domain    string // the address's domain
	After     *SuppressionCursor
	Limit     int // 0: every row
}

func (q SuppressionQuery) where() (string, []any) {
	where := `project_id = $1`
	args := []any{q.ProjectID}
	add := func(cond string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+cond, len(args))
	}
	if q.Reason != "" {
		add(`reason = $%d`, q.Reason)
	}
	if q.Stream != "" {
		add(`stream = $%d`, q.Stream)
	}
	if q.Address != "" {
		add(`address = $%d`, q.Address)
	}
	if q.Domain != "" {
		add(`split_part(address, '@', 2) = $%d`, q.Domain)
	}
	if q.After != nil {
		args = append(args, q.After.CreatedAt, q.After.Address, q.After.Stream)
		where += fmt.Sprintf(` AND (created_at, address, stream) < ($%d, $%d, $%d)`, len(args)-2, len(args)-1, len(args))
	}
	return where, args
}

// ListSuppressions returns one page and whether more follow.
func (s *Store) ListSuppressions(ctx context.Context, q SuppressionQuery) ([]Suppression, bool, error) {
	where, args := q.where()
	args = append(args, q.Limit+1)
	rows, err := s.Pool.Query(ctx, `SELECT address, stream, reason, detail, created_at FROM suppressions WHERE `+where+
		fmt.Sprintf(` ORDER BY created_at DESC, address DESC, stream DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	out, err := pgx.CollectRows(rows, scanSuppression)
	if err != nil {
		return nil, false, err
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

func scanSuppression(row pgx.CollectableRow) (Suppression, error) {
	var v Suppression
	err := row.Scan(&v.Address, &v.Stream, &v.Reason, &v.Detail, &v.CreatedAt)
	return v, err
}

// EachSuppression calls fn for every suppression the query matches, oldest
// first, without holding them all in memory.
func (s *Store) EachSuppression(ctx context.Context, q SuppressionQuery, fn func(Suppression) error) error {
	where, args := q.where()
	rows, err := s.Pool.Query(ctx, `SELECT address, stream, reason, detail, created_at FROM suppressions WHERE `+where+
		` ORDER BY created_at, address, stream`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanSuppression(rows)
		if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// AddSuppression stores a suppression. created is false when the address
// was already suppressed for that stream; the existing row is returned.
// ErrNotFound when the project does not exist.
func (s *Store) AddSuppression(ctx context.Context, projectID string, v Suppression) (out Suppression, created bool, err error) {
	if v.Stream == "" {
		v.Stream = SuppressAll
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO suppressions (project_id, address, reason, detail, stream)
		SELECT id, $2, $3, $4, $5 FROM projects WHERE id = $1 ON CONFLICT DO NOTHING`,
		projectID, v.Address, v.Reason, truncate(v.Detail, 300), v.Stream)
	if err != nil {
		return out, false, err
	}
	err = s.Pool.QueryRow(ctx, `SELECT address, stream, reason, detail, created_at FROM suppressions
		WHERE project_id = $1 AND address = $2 AND stream = $3`, projectID, v.Address, v.Stream).
		Scan(&out.Address, &out.Stream, &out.Reason, &out.Detail, &out.CreatedAt)
	return out, tag.RowsAffected() == 1, noRows(err)
}

// DeleteSuppression removes an address's suppressions: only the one for
// stream when it is set. It returns the rows removed.
func (s *Store) DeleteSuppression(ctx context.Context, projectID, address, stream string) ([]Suppression, error) {
	q := `DELETE FROM suppressions WHERE project_id = $1 AND address = $2`
	args := []any{projectID, address}
	if stream != "" {
		q += ` AND stream = $3`
		args = append(args, stream)
	}
	rows, err := s.Pool.Query(ctx, q+` RETURNING address, stream, reason, detail, created_at`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSuppression)
}
