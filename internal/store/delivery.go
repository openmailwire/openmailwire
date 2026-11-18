package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
)

const (
	// DefaultDomainConcurrency is OMW_DOMAIN_CONCURRENCY's default.
	DefaultDomainConcurrency = 10
	// DefaultRelayAfterAttempts is OMW_RELAY_AFTER_ATTEMPTS's default.
	DefaultRelayAfterAttempts = 2
	// RelayLiveWindow: a relay worker counts as live while its last
	// heartbeat is this recent.
	RelayLiveWindow = 2 * time.Minute

	leaseLockKey = 7461230005 // serializes leases so per-domain limits hold across workers
	leaseScan    = 1000       // due deliveries considered per lease
)

// LeaseOptions describes one lease request.
type LeaseOptions struct {
	WorkerID string
	Kind     string // protocol.KindDirect (or empty) or protocol.KindRelay
	Max      int
	Lease    time.Duration
	// DomainConcurrency caps in-flight deliveries per recipient domain
	// (OMW_DOMAIN_CONCURRENCY). For direct workers, a domain that throttled
	// us has a lower limit in receiver_state.
	DomainConcurrency int
	// RelayAfterAttempts routes a delivery to relay workers once it has had
	// this many attempts (OMW_RELAY_AFTER_ATTEMPTS).
	RelayAfterAttempts int
}

// LeaseJobs hands up to o.Max due deliveries to a worker. Each gets a fresh
// attempt ID and an expiring lease.
//
// Direct workers get deliveries whose domain is not in backoff, up to the
// domain's limit of in-flight deliveries from direct workers. While a relay
// worker is live, they leave alone the deliveries routed to it: those with
// at least o.RelayAfterAttempts attempts.
//
// Relay workers get only deliveries with at least o.RelayAfterAttempts
// attempts or whose domain is in backoff, up to o.DomainConcurrency in
// flight per domain through relays. The receiver's limit learned from our
// own IPs does not apply to them.
//
// Leases are serialized with an advisory lock, so two workers leasing at once
// cannot both fill the same domain's last slot.
func (s *Store) LeaseJobs(ctx context.Context, o LeaseOptions) ([]LeasedJob, error) {
	kind := o.Kind
	if kind == "" {
		kind = protocol.KindDirect
	}
	if o.DomainConcurrency < 1 {
		o.DomainConcurrency = DefaultDomainConcurrency
	}
	if o.RelayAfterAttempts < 1 {
		o.RelayAfterAttempts = DefaultRelayAfterAttempts
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(leaseLockKey)); err != nil {
		return nil, err
	}
	// The kind a worker leases as is the kind its reports count as, even if
	// its heartbeat has not landed yet.
	if _, err := tx.Exec(ctx, `UPDATE workers SET kind = $2 WHERE id = $1 AND kind <> $2`, o.WorkerID, kind); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		WITH relay AS (
			SELECT EXISTS (SELECT 1 FROM workers WHERE kind = 'relay' AND NOT draining
				AND last_seen_at > now() - make_interval(secs => $8)) AS live
		), due AS (
			SELECT d.id, d.rcpt_domain, d.next_attempt_at,
				CASE WHEN $5 = 'relay' THEN $3 ELSE LEAST(COALESCE(rs."limit", $3), $3) END - inflight.n AS room
			FROM deliveries d
			JOIN emails e ON e.id = d.email_id
			LEFT JOIN receiver_state rs ON rs.rcpt_domain = d.rcpt_domain
			CROSS JOIN relay
			CROSS JOIN LATERAL (
				SELECT count(*) AS n FROM deliveries x LEFT JOIN workers w ON w.id = x.lease_worker
				WHERE x.status = 'sending' AND x.rcpt_domain = d.rcpt_domain AND COALESCE(w.kind, 'direct') = $5
			) inflight
			WHERE d.status IN ('queued', 'deferred')
			  AND d.next_attempt_at <= now()
			  AND e.expires_at > now()
			  AND e.raw IS NOT NULL
			  AND CASE WHEN $5 = 'relay'
				THEN d.attempts >= $6 OR rs.backoff_until > now()
				ELSE (rs.backoff_until IS NULL OR rs.backoff_until <= now()) AND NOT (relay.live AND d.attempts >= $6)
			  END
			  AND inflight.n < CASE WHEN $5 = 'relay' THEN $3 ELSE LEAST(COALESCE(rs."limit", $3), $3) END
			ORDER BY d.next_attempt_at
			LIMIT $7
		), ranked AS (
			SELECT id, room, row_number() OVER (PARTITION BY rcpt_domain ORDER BY next_attempt_at, id) AS rn
			FROM due
		), picked AS (
			SELECT d.id
			FROM deliveries d JOIN ranked r ON r.id = d.id
			WHERE r.rn <= r.room
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
		o.Max, o.WorkerID, o.DomainConcurrency, o.Lease.Seconds(), kind, o.RelayAfterAttempts, leaseScan,
		RelayLiveWindow.Seconds())
	if err != nil {
		return nil, err
	}
	var jobs []LeasedJob
	for rows.Next() {
		var j LeasedJob
		if err := rows.Scan(&j.DeliveryID, &j.Rcpt, &j.RcptDomain, &j.AttemptID, &j.LeaseExpiresAt, &j.SealedRaw, &j.ReturnPath); err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, tx.Commit(ctx)
}

// ReceiverState is what the control plane has learned about one recipient
// domain from throttling deferrals.
type ReceiverState struct {
	Domain       string     `json:"rcpt_domain"`
	Limit        int        `json:"limit"`
	BackoffUntil *time.Time `json:"backoff_until"`
	Throttles    int        `json:"throttles"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// GetReceiverState returns the learned state of a recipient domain, or
// ErrNotFound when the domain never throttled us.
func (s *Store) GetReceiverState(ctx context.Context, domain string) (ReceiverState, error) {
	var r ReceiverState
	err := s.Pool.QueryRow(ctx, `SELECT rcpt_domain, "limit", backoff_until, throttles, updated_at
		FROM receiver_state WHERE rcpt_domain = $1`, domain).Scan(&r.Domain, &r.Limit, &r.BackoffUntil, &r.Throttles, &r.UpdatedAt)
	return r, noRows(err)
}

// attemptRef names the attempt whose report changes a receiver's state.
type attemptRef struct {
	emailID, deliveryID, domain string
	startedAt                   time.Time
}

// throttle handles a throttling deferral from a direct worker: the domain's
// limit halves (minimum 1) and direct workers leave it alone for
// policy.ThrottleBackoff(n), n being the consecutive throttles so far.
//
// Deferrals from attempts that were already in flight when the domain last
// throttled us are the same burst, so they do not halve the limit again.
func throttle(ctx context.Context, tx pgx.Tx, at attemptRef, ceiling int) error {
	if _, err := tx.Exec(ctx, `INSERT INTO receiver_state (rcpt_domain, "limit") VALUES ($1, $2)
		ON CONFLICT (rcpt_domain) DO NOTHING`, at.domain, ceiling); err != nil {
		return err
	}
	var limit, throttles int
	var last *time.Time
	if err := tx.QueryRow(ctx, `SELECT "limit", throttles, last_throttled_at FROM receiver_state
		WHERE rcpt_domain = $1 FOR UPDATE`, at.domain).Scan(&limit, &throttles, &last); err != nil {
		return err
	}
	if last != nil && at.startedAt.Before(*last) {
		return nil
	}
	prev := min(limit, ceiling)
	next := max1(prev / 2)
	n := throttles + 1
	backoff := policy.ThrottleBackoff(n)
	var until time.Time
	if err := tx.QueryRow(ctx, `UPDATE receiver_state SET "limit" = $2, throttles = $3,
			backoff_until = now() + make_interval(secs => $4), last_throttled_at = now(), updated_at = now()
		WHERE rcpt_domain = $1 RETURNING backoff_until`, at.domain, next, n, backoff.Seconds()).Scan(&until); err != nil {
		return err
	}
	return receiverEvent(ctx, tx, at, "throttled", map[string]any{
		"rcpt_domain": at.domain, "limit": next, "previous_limit": prev,
		"backoff_until": until, "backoff_seconds": int(backoff.Seconds()),
	})
}

// relax handles a delivery by a direct worker to a domain that throttled us
// before: its limit grows by 1, up to ceiling, and the run of consecutive
// throttles ends. Deliveries already in flight when the domain last
// throttled us do not count; they were not sent under the lower limit.
func relax(ctx context.Context, tx pgx.Tx, at attemptRef, ceiling int) error {
	var limit, throttles int
	var last *time.Time
	err := tx.QueryRow(ctx, `SELECT "limit", throttles, last_throttled_at FROM receiver_state
		WHERE rcpt_domain = $1 FOR UPDATE`, at.domain).Scan(&limit, &throttles, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if last != nil && at.startedAt.Before(*last) {
		return nil
	}
	next := max1(min(limit+1, ceiling))
	if next == limit && throttles == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE receiver_state SET "limit" = $2, throttles = 0, updated_at = now()
		WHERE rcpt_domain = $1`, at.domain, next); err != nil {
		return err
	}
	if next <= limit {
		return nil
	}
	return receiverEvent(ctx, tx, at, "throttle_relaxed", map[string]any{
		"rcpt_domain": at.domain, "limit": next, "previous_limit": limit,
	})
}

func receiverEvent(ctx context.Context, tx pgx.Tx, at attemptRef, typ string, data map[string]any) error {
	b, _ := json.Marshal(data)
	_, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, $3, $4)`,
		at.emailID, at.deliveryID, typ, b)
	return err
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
