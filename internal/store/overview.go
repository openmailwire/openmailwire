package store

// The project overview (GET /overview): queue depth, recent sending
// outcomes and receiving counts. Every query uses an index and none reads
// message bodies; the sending windows read at most the newest
// InsightsMaxEmails emails of the last 7 days, like insights.

import (
	"context"
	"time"
)

// QueueCounts are a project's deliveries that are not final yet.
type QueueCounts struct {
	Queued    int // due now, waiting for a worker
	Scheduled int // queued for a scheduled_at in the future
	Sending   int
	Deferred  int
	Held      int // recipients waiting for approval
	// HeldEmails is the number of emails with at least one held recipient.
	HeldEmails int
	// OldestQueuedAt is when the oldest delivery that is queued (and due),
	// deferred or sending became due: the email's created_at, or its
	// scheduled_at when that is later.
	OldestQueuedAt *time.Time
}

// OverviewQueue counts a project's deliveries that are not final. Queued
// and deferred rows are found through the deliveries_due partial index,
// sending rows through deliveries_in_flight, and held ones through the
// project's pending holds.
func (s *Store) OverviewQueue(ctx context.Context, projectID string) (QueueCounts, error) {
	var q QueueCounts
	// The status lists match the partial indexes' predicates, so the
	// planner reads only open deliveries.
	err := s.Pool.QueryRow(ctx, `
		WITH open AS (
			SELECT d.status, e.created_at, e.scheduled_at FROM deliveries d JOIN emails e ON e.id = d.email_id
			WHERE d.status IN ('queued', 'deferred') AND e.project_id = $1
			UNION ALL
			SELECT d.status, e.created_at, e.scheduled_at FROM deliveries d JOIN emails e ON e.id = d.email_id
			WHERE d.status = 'sending' AND e.project_id = $1
		)
		SELECT
			count(*) FILTER (WHERE status = 'queued' AND (scheduled_at IS NULL OR scheduled_at <= now())),
			count(*) FILTER (WHERE status = 'queued' AND scheduled_at > now()),
			count(*) FILTER (WHERE status = 'sending'),
			count(*) FILTER (WHERE status = 'deferred'),
			min(GREATEST(created_at, COALESCE(scheduled_at, created_at)))
				FILTER (WHERE status <> 'queued' OR scheduled_at IS NULL OR scheduled_at <= now())
		FROM open`, projectID).Scan(&q.Queued, &q.Scheduled, &q.Sending, &q.Deferred, &q.OldestQueuedAt)
	if err != nil {
		return q, err
	}
	err = s.Pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT d.email_id)
		FROM email_holds h JOIN deliveries d ON d.email_id = h.email_id
		WHERE h.project_id = $1 AND h.decision IS NULL AND d.status = 'held'`, projectID).Scan(&q.Held, &q.HeldEmails)
	return q, err
}

// SendingDay is one group of a project's recent recipients: those of the
// emails created on one UTC day, inside or outside the last 24 hours.
type SendingDay struct {
	Day        time.Time // UTC midnight
	Last24h    bool      // the emails were created in the last 24 hours
	Emails     int
	Recipients int
	Delivered  int
	Bounced    int
	Suppressed int
	Complained int // recipients with a complaint event
}

// OverviewSending groups the recipients of the project's emails of the
// last 7 days (the newest maxEmails of them) by UTC day and by whether they
// are from the last 24 hours. truncated reports that more emails were sent.
func (s *Store) OverviewSending(ctx context.Context, projectID string, maxEmails int) (days []SendingDay, truncated bool, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT count(*) > $2 FROM (
			SELECT 1 FROM emails WHERE project_id = $1 AND created_at > now() - interval '7 days'
			ORDER BY created_at DESC LIMIT $2 + 1) x`, projectID, maxEmails).Scan(&truncated)
	if err != nil {
		return nil, false, err
	}
	rows, err := s.Pool.Query(ctx, `
		WITH recent AS (
			SELECT id, created_at FROM emails
			WHERE project_id = $1 AND created_at > now() - interval '7 days'
			ORDER BY created_at DESC LIMIT $2
		), complained AS (
			SELECT DISTINCT ev.delivery_id FROM recent r JOIN events ev ON ev.email_id = r.id
			WHERE ev.type = 'complained' AND ev.delivery_id IS NOT NULL
		)
		SELECT date_trunc('day', r.created_at AT TIME ZONE 'UTC') AS day,
			r.created_at > now() - interval '24 hours' AS last24,
			count(DISTINCT r.id), count(d.id),
			count(d.id) FILTER (WHERE d.status = 'delivered'),
			count(d.id) FILTER (WHERE d.status = 'bounced'),
			count(d.id) FILTER (WHERE d.status = 'suppressed'),
			count(c.delivery_id)
		FROM recent r
		JOIN deliveries d ON d.email_id = r.id
		LEFT JOIN complained c ON c.delivery_id = d.id
		GROUP BY 1, 2 ORDER BY 1, 2`, projectID, maxEmails)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var d SendingDay
		if err := rows.Scan(&d.Day, &d.Last24h, &d.Emails, &d.Recipients, &d.Delivered, &d.Bounced, &d.Suppressed, &d.Complained); err != nil {
			return nil, false, err
		}
		d.Day = time.Date(d.Day.Year(), d.Day.Month(), d.Day.Day(), 0, 0, 0, 0, time.UTC)
		days = append(days, d)
	}
	return days, truncated, rows.Err()
}

// ReceivingCounts are a project's inbound numbers.
type ReceivingCounts struct {
	Inboxes      int
	Messages24h  int
	HeldMessages int
}

// OverviewReceiving counts the project's inboxes, the messages they
// received in the last 24 hours (inbound_messages_inbox), and the messages
// held from agent keys (inbound_messages_held, a partial index).
func (s *Store) OverviewReceiving(ctx context.Context, projectID string) (ReceivingCounts, error) {
	var c ReceivingCounts
	err := s.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM inboxes WHERE project_id = $1),
			(SELECT count(*) FROM inbound_messages m
				WHERE m.inbox_id IN (SELECT id FROM inboxes WHERE project_id = $1)
				AND m.received_at > now() - interval '24 hours'),
			(SELECT count(*) FROM inbound_messages WHERE project_id = $1 AND held)`,
		projectID).Scan(&c.Inboxes, &c.Messages24h, &c.HeldMessages)
	return c, err
}

// ActivePauses counts the project's pauses in force.
func (s *Store) ActivePauses(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM sending_pauses WHERE project_id = $1 AND resumed_at IS NULL`,
		projectID).Scan(&n)
	return n, err
}

// AuthIssueCounts counts the project's reported authentication problems
// per sending domain.
func (s *Store) AuthIssueCounts(ctx context.Context, projectID string) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT sending_domain, count(*) FROM sender_auth_issues
		WHERE project_id = $1 GROUP BY sending_domain`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}
