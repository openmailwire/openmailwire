package store

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLimitReached is returned when a project already has as many webhooks
// as it may.
var ErrLimitReached = errors.New("limit reached")

type Webhook struct {
	ID        string
	ProjectID string
	Endpoint  string
	Events    []string
	Secret    []byte // sealed
	CreatedAt time.Time

	// Ed25519 ("v1a") signing, when enabled: the sealed private key and the
	// public key.
	Ed25519Key    []byte
	Ed25519Public []byte

	// After a secret rotation, the previous secret and key keep signing
	// until PreviousExpiresAt.
	SecretPrevious        []byte
	Ed25519PreviousKey    []byte
	Ed25519PreviousPublic []byte
	PreviousExpiresAt     *time.Time
	SecretRotatedAt       *time.Time

	// Circuit breaker: consecutive failed attempts, and until when the
	// endpoint gets none.
	Failures     int
	BreakerUntil *time.Time
}

// InOverlap reports whether the previous secret still signs.
func (w Webhook) InOverlap(now time.Time) bool {
	return w.PreviousExpiresAt != nil && now.Before(*w.PreviousExpiresAt)
}

const webhookCols = `id::text, project_id::text, endpoint, events, secret, created_at,
	ed25519_key, ed25519_public, secret_previous, ed25519_previous_key, ed25519_previous_public,
	previous_expires_at, secret_rotated_at, failures, breaker_until`

func scanWebhook(row pgx.Row) (Webhook, error) {
	var w Webhook
	err := row.Scan(&w.ID, &w.ProjectID, &w.Endpoint, &w.Events, &w.Secret, &w.CreatedAt,
		&w.Ed25519Key, &w.Ed25519Public, &w.SecretPrevious, &w.Ed25519PreviousKey, &w.Ed25519PreviousPublic,
		&w.PreviousExpiresAt, &w.SecretRotatedAt, &w.Failures, &w.BreakerUntil)
	return w, err
}

// CreateWebhook stores a webhook unless the project already has max of
// them (ErrLimitReached). Creations in one project are serialized, so the
// cap holds under concurrent requests.
func (s *Store) CreateWebhook(ctx context.Context, w Webhook, max int) (Webhook, error) {
	if w.Events == nil {
		w.Events = []string{}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Webhook{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(746123011, hashtext($1::text))`, w.ProjectID); err != nil {
		return Webhook{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM webhooks WHERE project_id = $1`, w.ProjectID).Scan(&n); err != nil {
		return Webhook{}, err
	}
	if max > 0 && n >= max {
		return Webhook{}, ErrLimitReached
	}
	out, err := scanWebhook(tx.QueryRow(ctx, `INSERT INTO webhooks (project_id, endpoint, events, secret, ed25519_key, ed25519_public)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+webhookCols,
		w.ProjectID, w.Endpoint, w.Events, w.Secret, w.Ed25519Key, w.Ed25519Public))
	if err != nil {
		return Webhook{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) ListWebhooks(ctx context.Context, projectID string) ([]Webhook, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) GetWebhook(ctx context.Context, projectID, id string) (Webhook, error) {
	w, err := scanWebhook(s.Pool.QueryRow(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE project_id = $1 AND id = $2`, projectID, id))
	return w, noRows(err)
}

func (s *Store) DeleteWebhook(ctx context.Context, projectID, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM webhooks WHERE project_id = $1 AND id = $2`, projectID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// WebhookRotation is a new signing secret (and Ed25519 key, used only when
// the webhook signs with Ed25519), all sealed.
type WebhookRotation struct {
	Secret        []byte
	Ed25519Key    []byte
	Ed25519Public []byte
	// The current secret and key keep signing until then.
	PreviousUntil time.Time
}

// RotateWebhookSecret makes the new secret current and keeps the current
// one as the previous secret until r.PreviousUntil. A secret that was
// still overlapping from an earlier rotation is dropped.
func (s *Store) RotateWebhookSecret(ctx context.Context, projectID, id string, r WebhookRotation) (Webhook, error) {
	w, err := scanWebhook(s.Pool.QueryRow(ctx, `UPDATE webhooks SET
			secret_previous = secret, secret = $3,
			ed25519_previous_key = ed25519_key, ed25519_previous_public = ed25519_public,
			ed25519_key = CASE WHEN ed25519_key IS NULL THEN NULL ELSE $4::bytea END,
			ed25519_public = CASE WHEN ed25519_key IS NULL THEN NULL ELSE $5::bytea END,
			previous_expires_at = $6, secret_rotated_at = now()
		WHERE project_id = $1 AND id = $2 RETURNING `+webhookCols,
		projectID, id, r.Secret, r.Ed25519Key, r.Ed25519Public, r.PreviousUntil))
	return w, noRows(err)
}

type WebhookDeliveryView struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastStatus  *int       `json:"last_status"`
	LastError   string     `json:"last_error"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

func (s *Store) RecentWebhookDeliveries(ctx context.Context, webhookID string, n int) ([]WebhookDeliveryView, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text, type, status, attempts, last_status, last_error, created_at, delivered_at
		FROM webhook_deliveries WHERE webhook_id = $1 ORDER BY created_at DESC, id LIMIT $2`, webhookID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookDeliveryView{}
	for rows.Next() {
		var v WebhookDeliveryView
		if err := rows.Scan(&v.ID, &v.Type, &v.Status, &v.Attempts, &v.LastStatus, &v.LastError, &v.CreatedAt, &v.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// EventRow is one timeline event with the message it belongs to: a sent
// email or, when InboundMessageID is set, an inbound message (EmailID then
// holds the inbound message's id too).
type EventRow struct {
	ID             int64
	Type           string
	CreatedAt      time.Time
	Data           []byte
	EmailID        string
	ProjectID      string
	From           string
	To             []string
	Subject        string
	Tags           map[string]string
	EmailCreatedAt time.Time
	Recipient      string

	// Inbound messages only.
	InboundMessageID string
	Cc               []string
	MessageID        string
	InboxID          string
	InboxAddress     string
	ThreadID         string
	InReplyToEmailID string
}

type NewWebhookDelivery struct {
	WebhookID string
	EventID   int64
	Type      string
	Payload   []byte
}

// FanoutWebhooks takes up to limit events that have not been fanned out,
// asks build for the webhook deliveries each one produces, stores them and
// marks the events fanned out, all in one transaction. It returns how many
// events it handled; 0 means none is waiting.
//
// Events are claimed with FOR UPDATE SKIP LOCKED, so concurrent control
// planes share the work. An event inserted by a transaction that has not
// committed yet is invisible here and is picked up once it commits: unlike
// the id cursor this replaces, nothing is skipped.
func (s *Store) FanoutWebhooks(ctx context.Context, limit int, build func(EventRow, []Webhook) []NewWebhookDelivery) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `SELECT e.id, e.type, e.created_at, e.data,
			COALESCE(COALESCE(m.id, im.id)::text, ''), COALESCE(m.project_id, im.project_id)::text,
			COALESCE(m.from_addr, im.from_addr, ''), COALESCE(m.to_addrs, im.to_addrs, '{}'), COALESCE(m.subject, im.subject, ''),
			COALESCE(m.tags, '{}'), COALESCE(m.created_at, im.received_at, e.created_at), COALESCE(d.rcpt, ''),
			COALESCE(im.id::text, ''), COALESCE(im.cc_addrs, '{}'), COALESCE(im.message_id, ''),
			COALESCE(im.inbox_id::text, ''), COALESCE(ib.address, ''), COALESCE(im.thread_id::text, ''),
			COALESCE(im.in_reply_to_email_id::text, '')
		FROM events e
		LEFT JOIN emails m ON m.id = e.email_id
		LEFT JOIN deliveries d ON d.id = e.delivery_id
		LEFT JOIN inbound_messages im ON im.id = e.inbound_message_id
		LEFT JOIN inboxes ib ON ib.id = im.inbox_id
		WHERE NOT e.fanned_out
		ORDER BY e.id LIMIT $1
		FOR UPDATE OF e SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var events []EventRow
	var ids []int64
	projects := map[string]bool{}
	for rows.Next() {
		var ev EventRow
		var projectID *string
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.CreatedAt, &ev.Data, &ev.EmailID, &projectID,
			&ev.From, &ev.To, &ev.Subject, &ev.Tags, &ev.EmailCreatedAt, &ev.Recipient,
			&ev.InboundMessageID, &ev.Cc, &ev.MessageID, &ev.InboxID, &ev.InboxAddress, &ev.ThreadID,
			&ev.InReplyToEmailID); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, ev.ID)
		if projectID == nil { // the message is gone; nothing to tell anyone
			continue
		}
		ev.ProjectID = *projectID
		events = append(events, ev)
		projects[ev.ProjectID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	var deliveries []NewWebhookDelivery
	if len(projects) > 0 {
		pids := make([]string, 0, len(projects))
		for p := range projects {
			pids = append(pids, p)
		}
		hookRows, err := tx.Query(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE project_id = ANY($1::uuid[])`, pids)
		if err != nil {
			return 0, err
		}
		hooks := map[string][]Webhook{}
		for hookRows.Next() {
			w, err := scanWebhook(hookRows)
			if err != nil {
				hookRows.Close()
				return 0, err
			}
			hooks[w.ProjectID] = append(hooks[w.ProjectID], w)
		}
		hookRows.Close()
		if err := hookRows.Err(); err != nil {
			return 0, err
		}
		for _, ev := range events {
			deliveries = append(deliveries, build(ev, hooks[ev.ProjectID])...)
		}
	}

	if len(deliveries) > 0 {
		touched := map[string]bool{}
		var hookIDs, types []string
		var eventIDs []int64
		var payloads [][]byte
		for _, d := range deliveries {
			touched[d.WebhookID] = true
			hookIDs = append(hookIDs, d.WebhookID)
			eventIDs = append(eventIDs, d.EventID)
			types = append(types, d.Type)
			payloads = append(payloads, d.Payload)
		}
		// Lock the webhooks first, in id order: claims, deletes and other
		// fan-outs then queue behind this transaction instead of
		// deadlocking with it.
		if err := lockWebhooks(ctx, tx, keys(touched)); err != nil {
			return 0, err
		}
		// A trigger (webhook_deliveries_due) moves each webhook's
		// next_due_at up to now, unless its breaker is open.
		if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (webhook_id, event_id, type, payload)
			SELECT * FROM unnest($1::uuid[], $2::bigint[], $3::text[], $4::bytea[])
			ON CONFLICT (webhook_id, event_id) DO NOTHING`, hookIDs, eventIDs, types, payloads); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE events SET fanned_out = true WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lockWebhooks(ctx context.Context, tx pgx.Tx, ids []string) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM webhooks WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE`, ids)
	return err
}

type DueWebhook struct {
	ID        string
	WebhookID string
	ProjectID string
	Endpoint  string
	Type      string
	Payload   []byte
	Attempts  int // including this one

	// Sealed signing material. Secrets holds the current secret, then the
	// previous one while a rotation overlaps; Ed25519Keys likewise, empty
	// unless the webhook signs with Ed25519.
	Secrets     [][]byte
	Ed25519Keys [][]byte
}

// ClaimOptions shapes one claim. The dispatcher passes what it has in
// flight, so the limits hold across claims.
type ClaimOptions struct {
	Max   int           // deliveries to claim at most
	Lease time.Duration // a claimed delivery is not claimed again for this long, so a crash only delays it

	PerWebhook int // deliveries in flight per webhook, at most
	PerProject int // deliveries in flight per project, at most
	// A webhook with this many consecutive failures is half-open: it gets
	// one attempt at a time until one succeeds.
	BreakerThreshold int

	InFlight        map[string]int // per webhook id
	InFlightProject map[string]int // per project id
}

// ClaimWebhookDeliveries takes due deliveries fairly: it picks webhooks
// (oldest due first, skipping endpoints whose breaker is open), gives each
// a share bounded per webhook and per project, and only then takes that
// many of the webhook's own due deliveries. A backlog behind one endpoint
// never delays another endpoint, and one project's endpoints never take
// more than PerProject slots. Every query is bounded and indexed.
func (s *Store) ClaimWebhookDeliveries(ctx context.Context, o ClaimOptions) ([]DueWebhook, error) {
	if o.Max <= 0 {
		return nil, nil
	}
	if o.PerWebhook <= 0 {
		o.PerWebhook = 1
	}
	if o.PerProject <= 0 {
		o.PerProject = o.Max
	}
	var fullHooks, fullProjects []string
	for id, n := range o.InFlight {
		if n >= o.PerWebhook {
			fullHooks = append(fullHooks, id)
		}
	}
	for id, n := range o.InFlightProject {
		if n >= o.PerProject {
			fullProjects = append(fullProjects, id)
		}
	}
	if fullHooks == nil {
		fullHooks = []string{}
	}
	if fullProjects == nil {
		fullProjects = []string{}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Candidate webhooks, locked so concurrent control planes take
	// different ones. Bounded by the LIMIT on an index.
	rows, err := tx.Query(ctx, `SELECT id::text, project_id::text, failures FROM webhooks
		WHERE next_due_at <= now() AND (breaker_until IS NULL OR breaker_until <= now())
		  AND NOT (id = ANY($1::uuid[])) AND NOT (project_id = ANY($2::uuid[]))
		ORDER BY next_due_at LIMIT $3
		FOR NO KEY UPDATE SKIP LOCKED`, fullHooks, fullProjects, 4*o.Max)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id, project string
		allow       int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var failures int
		if err := rows.Scan(&c.id, &c.project, &failures); err != nil {
			rows.Close()
			return nil, err
		}
		c.allow = o.PerWebhook - o.InFlight[c.id]
		if o.BreakerThreshold > 0 && failures >= o.BreakerThreshold {
			c.allow = 1 - o.InFlight[c.id] // half-open: one probe at a time
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// Shares: one delivery per webhook in due order first, then top-ups,
	// within the per-project and overall limits.
	share := make([]int, len(cands))
	projectLeft := map[string]int{}
	for _, c := range cands {
		if _, ok := projectLeft[c.project]; !ok {
			projectLeft[c.project] = o.PerProject - o.InFlightProject[c.project]
		}
	}
	left := o.Max
	for round := 0; left > 0; round++ {
		gave := false
		for i, c := range cands {
			if left == 0 {
				break
			}
			if share[i] < c.allow && projectLeft[c.project] > 0 && (round > 0 || share[i] == 0) {
				share[i]++
				projectLeft[c.project]--
				left--
				gave = true
			}
		}
		if !gave {
			break
		}
	}
	var allocIDs, allIDs []string
	var allocN []int32
	for i, c := range cands {
		allIDs = append(allIDs, c.id)
		if share[i] > 0 {
			allocIDs = append(allocIDs, c.id)
			allocN = append(allocN, int32(share[i]))
		}
	}

	var out []DueWebhook
	if len(allocIDs) > 0 {
		rows, err = tx.Query(ctx, `
			WITH alloc AS (SELECT * FROM unnest($1::uuid[], $2::int[]) AS a(webhook_id, n)),
			due AS (
				SELECT d.id FROM alloc a CROSS JOIN LATERAL (
					SELECT id FROM webhook_deliveries
					WHERE webhook_id = a.webhook_id AND status = 'pending' AND next_attempt_at <= now()
					ORDER BY next_attempt_at LIMIT a.n
					FOR UPDATE SKIP LOCKED) d
			)
			UPDATE webhook_deliveries wd SET attempts = wd.attempts + 1,
				next_attempt_at = now() + make_interval(secs => $3)
			FROM due, webhooks w
			WHERE wd.id = due.id AND w.id = wd.webhook_id
			RETURNING wd.id::text, w.id::text, w.project_id::text, w.endpoint, wd.type, wd.payload, wd.attempts,
				w.secret, CASE WHEN w.previous_expires_at > now() THEN w.secret_previous END,
				w.ed25519_key, CASE WHEN w.previous_expires_at > now() THEN w.ed25519_previous_key END`,
			allocIDs, allocN, o.Lease.Seconds())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d DueWebhook
			var secret, prevSecret, edKey, prevEdKey []byte
			if err := rows.Scan(&d.ID, &d.WebhookID, &d.ProjectID, &d.Endpoint, &d.Type, &d.Payload, &d.Attempts,
				&secret, &prevSecret, &edKey, &prevEdKey); err != nil {
				rows.Close()
				return nil, err
			}
			d.Secrets = nonEmpty(secret, prevSecret)
			d.Ed25519Keys = nonEmpty(edKey, prevEdKey)
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Every candidate's next due time is recomputed from its own pending
	// deliveries (one indexed lookup each), so it moves back in line. The
	// rows are locked by this transaction, so a fan-out that adds a
	// delivery meanwhile updates them after it and its LEAST wins.
	if _, err := tx.Exec(ctx, `UPDATE webhooks w SET next_due_at = (
			SELECT CASE WHEN min(d.next_attempt_at) IS NULL THEN NULL ELSE GREATEST(min(d.next_attempt_at), w.breaker_until) END
			FROM webhook_deliveries d WHERE d.webhook_id = w.id AND d.status = 'pending')
		WHERE w.id = ANY($1::uuid[])`, allIDs); err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}

func nonEmpty(vs ...[]byte) [][]byte {
	var out [][]byte
	for _, v := range vs {
		if len(v) > 0 {
			out = append(out, v)
		}
	}
	return out
}

// WebhookAttempt is the result of one webhook delivery attempt.
type WebhookAttempt struct {
	ID        string
	WebhookID string
	OK        bool
	Status    int        // HTTP status, 0 when there was no answer
	Error     string     // shown to the project: keep it generic
	Next      *time.Time // the next attempt; nil with OK false means no retries are left

	// EndpointFault is false when the attempt failed on our side (a secret
	// that cannot be unsealed): it then doesn't count against the
	// endpoint's breaker.
	EndpointFault bool

	// Circuit breaker: after BreakerThreshold consecutive failures the
	// endpoint rests for BreakerBase, doubling with every further failure
	// up to BreakerMax.
	BreakerThreshold int
	BreakerBase      time.Duration
	BreakerMax       time.Duration
}

// FinishWebhookDelivery records one attempt and updates the endpoint's
// circuit breaker. It returns when the breaker keeps the endpoint closed
// until, or nil.
func (s *Store) FinishWebhookDelivery(ctx context.Context, a WebhookAttempt) (*time.Time, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	switch {
	case a.OK:
		_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivered', delivered_at = now(),
			last_status = $2, last_error = '' WHERE id = $1`, a.ID, nullInt(a.Status))
	case a.Next != nil:
		_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = $2, last_status = $3, last_error = $4
			WHERE id = $1`, a.ID, *a.Next, nullInt(a.Status), truncate(a.Error, 500))
	default:
		_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_status = $2, last_error = $3
			WHERE id = $1`, a.ID, nullInt(a.Status), truncate(a.Error, 500))
	}
	if err != nil {
		return nil, err
	}
	if a.WebhookID == "" {
		return nil, tx.Commit(ctx)
	}
	threshold := a.BreakerThreshold
	if threshold <= 0 || !a.EndpointFault && !a.OK {
		threshold = 1 << 30 // never trips
	}
	var until *time.Time
	err = tx.QueryRow(ctx, `UPDATE webhooks SET
			failures = CASE WHEN $2 THEN 0 WHEN $7 THEN failures + 1 ELSE failures END,
			breaker_until = CASE
				WHEN $2 THEN NULL
				WHEN $7 AND failures + 1 >= $4 THEN now() + make_interval(secs => LEAST($5 * power(2, LEAST(failures + 1 - $4, 30)), $6))
				ELSE breaker_until END,
			next_due_at = GREATEST(
				CASE WHEN $3::timestamptz IS NULL THEN next_due_at ELSE LEAST(next_due_at, $3::timestamptz) END,
				CASE
					WHEN $2 THEN NULL
					WHEN $7 AND failures + 1 >= $4 THEN now() + make_interval(secs => LEAST($5 * power(2, LEAST(failures + 1 - $4, 30)), $6))
					ELSE breaker_until END)
		WHERE id = $1 RETURNING breaker_until`,
		a.WebhookID, a.OK, a.Next, threshold, a.BreakerBase.Seconds(), a.BreakerMax.Seconds(), a.EndpointFault).Scan(&until)
	if errors.Is(err, pgx.ErrNoRows) { // the webhook was deleted meanwhile
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return until, tx.Commit(ctx)
}
