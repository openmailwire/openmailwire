package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Webhook struct {
	ID        string
	ProjectID string
	Endpoint  string
	Events    []string
	Secret    []byte // sealed
	CreatedAt time.Time
}

const webhookCols = `id::text, project_id::text, endpoint, events, secret, created_at`

func scanWebhook(row pgx.Row) (Webhook, error) {
	var w Webhook
	err := row.Scan(&w.ID, &w.ProjectID, &w.Endpoint, &w.Events, &w.Secret, &w.CreatedAt)
	return w, err
}

func (s *Store) CreateWebhook(ctx context.Context, w Webhook) (Webhook, error) {
	if w.Events == nil {
		w.Events = []string{}
	}
	return scanWebhook(s.Pool.QueryRow(ctx, `INSERT INTO webhooks (project_id, endpoint, events, secret)
		VALUES ($1, $2, $3, $4) RETURNING `+webhookCols, w.ProjectID, w.Endpoint, w.Events, w.Secret))
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

// FanoutWebhooks reads events past the cursor, asks build for the webhook
// deliveries each one produces, stores them and advances the cursor, all in
// one transaction. The cursor row lock serializes concurrent control planes.
func (s *Store) FanoutWebhooks(ctx context.Context, limit int, build func(EventRow, []Webhook) []NewWebhookDelivery) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var cursor int64
	if err := tx.QueryRow(ctx, `SELECT last_event_id FROM webhook_cursor WHERE id = 1 FOR UPDATE`).Scan(&cursor); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT e.id, e.type, e.created_at, e.data,
			COALESCE(m.id, im.id)::text, COALESCE(m.project_id, im.project_id)::text,
			COALESCE(m.from_addr, im.from_addr), COALESCE(m.to_addrs, im.to_addrs), COALESCE(m.subject, im.subject),
			COALESCE(m.tags, '{}'), COALESCE(m.created_at, im.received_at), COALESCE(d.rcpt, ''),
			COALESCE(im.id::text, ''), COALESCE(im.cc_addrs, '{}'), COALESCE(im.message_id, ''),
			COALESCE(im.inbox_id::text, ''), COALESCE(ib.address, ''), COALESCE(im.thread_id::text, ''),
			COALESCE(im.in_reply_to_email_id::text, '')
		FROM events e
		LEFT JOIN emails m ON m.id = e.email_id
		LEFT JOIN deliveries d ON d.id = e.delivery_id
		LEFT JOIN inbound_messages im ON im.id = e.inbound_message_id
		LEFT JOIN inboxes ib ON ib.id = im.inbox_id
		WHERE e.id > $1 AND (m.id IS NOT NULL OR im.id IS NOT NULL) ORDER BY e.id LIMIT $2`, cursor, limit)
	if err != nil {
		return 0, err
	}
	var events []EventRow
	projects := map[string]bool{}
	for rows.Next() {
		var ev EventRow
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.CreatedAt, &ev.Data, &ev.EmailID, &ev.ProjectID,
			&ev.From, &ev.To, &ev.Subject, &ev.Tags, &ev.EmailCreatedAt, &ev.Recipient,
			&ev.InboundMessageID, &ev.Cc, &ev.MessageID, &ev.InboxID, &ev.InboxAddress, &ev.ThreadID,
			&ev.InReplyToEmailID); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, ev)
		projects[ev.ProjectID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	ids := make([]string, 0, len(projects))
	for p := range projects {
		ids = append(ids, p)
	}
	hookRows, err := tx.Query(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE project_id::text = ANY($1)`, ids)
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

	queued := 0
	for _, ev := range events {
		for _, d := range build(ev, hooks[ev.ProjectID]) {
			if _, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (webhook_id, event_id, type, payload)
				VALUES ($1, $2, $3, $4) ON CONFLICT (webhook_id, event_id) DO NOTHING`,
				d.WebhookID, d.EventID, d.Type, d.Payload); err != nil {
				return 0, err
			}
			queued++
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_cursor SET last_event_id = $1 WHERE id = 1`, events[len(events)-1].ID); err != nil {
		return 0, err
	}
	return queued, tx.Commit(ctx)
}

type DueWebhook struct {
	ID       string
	Endpoint string
	Type     string
	Payload  []byte
	Secret   []byte // sealed
	Attempts int    // including this one
}

// ClaimWebhookDeliveries takes due deliveries and pushes their next attempt
// forward by lease, so a crash mid-send only delays them.
func (s *Store) ClaimWebhookDeliveries(ctx context.Context, limit int, lease time.Duration) ([]DueWebhook, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM webhook_deliveries
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE webhook_deliveries wd SET attempts = wd.attempts + 1,
			next_attempt_at = now() + make_interval(secs => $2)
		FROM due, webhooks w
		WHERE wd.id = due.id AND w.id = wd.webhook_id
		RETURNING wd.id::text, w.endpoint, wd.type, wd.payload, w.secret, wd.attempts`, limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueWebhook
	for rows.Next() {
		var d DueWebhook
		if err := rows.Scan(&d.ID, &d.Endpoint, &d.Type, &d.Payload, &d.Secret, &d.Attempts); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FinishWebhookDelivery records one attempt. next nil with ok false means
// the delivery has run out of retries.
func (s *Store) FinishWebhookDelivery(ctx context.Context, id string, ok bool, status int, errText string, next *time.Time) error {
	var err error
	switch {
	case ok:
		_, err = s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivered', delivered_at = now(),
			last_status = $2, last_error = '' WHERE id = $1`, id, nullInt(status))
	case next != nil:
		_, err = s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = $2, last_status = $3, last_error = $4
			WHERE id = $1`, id, *next, nullInt(status), truncate(errText, 500))
	default:
		_, err = s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_status = $2, last_error = $3
			WHERE id = $1`, id, nullInt(status), truncate(errText, 500))
	}
	return err
}
