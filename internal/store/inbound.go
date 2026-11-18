package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- inboxes ----

type Inbox struct {
	ID        string
	ProjectID string
	DomainID  string
	Address   string
	CreatedAt time.Time
}

const inboxCols = `id::text, project_id::text, domain_id::text, address, created_at`

func scanInbox(row pgx.Row) (Inbox, error) {
	var b Inbox
	err := row.Scan(&b.ID, &b.ProjectID, &b.DomainID, &b.Address, &b.CreatedAt)
	return b, err
}

func (s *Store) CreateInbox(ctx context.Context, projectID, domainID, address string) (Inbox, error) {
	b, err := scanInbox(s.Pool.QueryRow(ctx, `INSERT INTO inboxes (project_id, domain_id, address)
		VALUES ($1, $2, $3) RETURNING `+inboxCols, projectID, domainID, address))
	if isUniqueViolation(err) {
		return Inbox{}, ErrConflict
	}
	return b, err
}

func (s *Store) ListInboxes(ctx context.Context, projectID string) ([]Inbox, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+inboxCols+` FROM inboxes WHERE project_id = $1 ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Inbox{}
	for rows.Next() {
		b, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetInbox(ctx context.Context, projectID, id string) (Inbox, error) {
	b, err := scanInbox(s.Pool.QueryRow(ctx, `SELECT `+inboxCols+` FROM inboxes WHERE project_id = $1 AND id = $2`, projectID, id))
	return b, noRows(err)
}

// InboxByAddress finds the inbox for a recipient address, in any project.
func (s *Store) InboxByAddress(ctx context.Context, address string) (Inbox, error) {
	b, err := scanInbox(s.Pool.QueryRow(ctx, `SELECT `+inboxCols+` FROM inboxes WHERE address = $1`, address))
	return b, noRows(err)
}

// DeleteInbox removes an inbox and every message it holds.
func (s *Store) DeleteInbox(ctx context.Context, projectID, id string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM inboxes WHERE project_id = $1 AND id = $2`, projectID, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---- bounces and complaints ----

// SentDelivery is one recipient of a sent email, as bounce and complaint
// processing needs it.
type SentDelivery struct {
	DeliveryID string
	EmailID    string
	ProjectID  string
	Rcpt       string
	Status     string
	ReturnPath string
}

// SentDelivery returns the delivery a VERP address names.
func (s *Store) SentDelivery(ctx context.Context, deliveryID string) (SentDelivery, error) {
	var d SentDelivery
	err := s.Pool.QueryRow(ctx, `SELECT d.id::text, d.email_id::text, e.project_id::text, d.rcpt, d.status, e.return_path
		FROM deliveries d JOIN emails e ON e.id = d.email_id WHERE d.id = $1`, deliveryID).
		Scan(&d.DeliveryID, &d.EmailID, &d.ProjectID, &d.Rcpt, &d.Status, &d.ReturnPath)
	return d, noRows(err)
}

// DeliveriesByMessageID returns every recipient of the sent email with
// this Message-ID (without angle brackets).
func (s *Store) DeliveriesByMessageID(ctx context.Context, messageID string) ([]SentDelivery, error) {
	rows, err := s.Pool.Query(ctx, `SELECT d.id::text, d.email_id::text, e.project_id::text, d.rcpt, d.status, e.return_path
		FROM emails e JOIN deliveries d ON d.email_id = e.id WHERE e.message_id = $1 ORDER BY d.rcpt`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SentDelivery
	for rows.Next() {
		var d SentDelivery
		if err := rows.Scan(&d.DeliveryID, &d.EmailID, &d.ProjectID, &d.Rcpt, &d.Status, &d.ReturnPath); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DSNBounce is a failure reported by a delivery status notification that
// arrived after the receiving server had accepted (or seemed to accept) the
// message.
type DSNBounce struct {
	DeliveryID string
	SMTPCode   int
	Status     string // enhanced status code
	Diagnostic string
	Suppress   bool   // add the address to the suppression list (5.1.x)
	Data       []byte // event data
}

// RecordDSNBounce marks a delivery bounced, even one already delivered,
// suppresses the address when asked and records a "bounced" event. A
// delivery that is already bounced (or was never sent) is left alone, so a
// repeated DSN changes nothing; changed reports whether anything happened.
func (s *Store) RecordDSNBounce(ctx context.Context, b DSNBounce) (changed bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var status, emailID, rcpt, projectID string
	err = tx.QueryRow(ctx, `SELECT d.status, d.email_id::text, d.rcpt, e.project_id::text
		FROM deliveries d JOIN emails e ON e.id = d.email_id WHERE d.id = $1 FOR UPDATE OF d`, b.DeliveryID).
		Scan(&status, &emailID, &rcpt, &projectID)
	if err != nil {
		return false, noRows(err)
	}
	if status == "bounced" || status == "suppressed" {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET status = 'bounced', lease_worker = NULL, lease_attempt = NULL,
			lease_expires_at = NULL, last_code = $2, last_response = $3, updated_at = now() WHERE id = $1`,
		b.DeliveryID, nullInt(b.SMTPCode), truncate(b.Diagnostic, 1000)); err != nil {
		return false, err
	}
	if b.Suppress {
		if _, err := tx.Exec(ctx, `INSERT INTO suppressions (project_id, address, reason, detail)
			VALUES ($1, $2, 'hard_bounce', $3) ON CONFLICT DO NOTHING`, projectID, rcpt, truncate(b.Diagnostic, 300)); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, 'bounced', $3)`,
		emailID, b.DeliveryID, b.Data); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Complaint is a recipient's spam report about a sent email.
type Complaint struct {
	EmailID    string
	DeliveryID string // "" when the report does not say which recipient
	ProjectID  string
	Rcpt       string // suppressed with reason "complaint" when set
	Data       []byte // event data
}

// RecordComplaint records a "complained" event and suppresses the address.
// A second report about the same delivery changes nothing.
func (s *Store) RecordComplaint(ctx context.Context, c Complaint) (changed bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if c.DeliveryID != "" {
		// The row lock serializes two reports about the same delivery.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM deliveries WHERE id = $1 FOR UPDATE`, c.DeliveryID); err != nil {
			return false, err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events
				WHERE email_id = $1 AND delivery_id = $2 AND type = 'complained')`,
			c.EmailID, c.DeliveryID).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
	}
	if c.Rcpt != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO suppressions (project_id, address, reason, detail)
			VALUES ($1, $2, 'complaint', 'the recipient reported a message as spam') ON CONFLICT DO NOTHING`,
			c.ProjectID, c.Rcpt); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (email_id, delivery_id, type, data) VALUES ($1, $2, 'complained', $3)`,
		c.EmailID, nullString(c.DeliveryID), c.Data); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ---- inbound messages ----

type NewInboundMessage struct {
	InboxID    string
	ProjectID  string
	MailFrom   string
	From       string
	To, Cc     []string
	ReplyTo    []string
	Subject    string
	MessageID  string   // without angle brackets
	InReplyTo  string   // without angle brackets
	References []string // without angle brackets, oldest first
	Date       string
	Snippet    string
	Size       int
	RemoteIP   string
	HELO       string
	TLS        bool
	Raw        []byte // sealed
}

// InboundMessage is a stored message. Raw is sealed and nil once purged.
type InboundMessage struct {
	ID               string
	InboxID          string
	ProjectID        string
	MailFrom         string
	From             string
	To, Cc           []string
	ReplyTo          []string
	Subject          string
	MessageID        string
	InReplyTo        string
	References       []string
	Date             string
	ThreadID         string
	InReplyToEmailID *string
	Snippet          string
	Size             int
	TLS              bool
	Raw              []byte
	ReceivedAt       time.Time
	PurgedAt         *time.Time
}

const messageCols = `id::text, inbox_id::text, project_id::text, mail_from, from_addr, to_addrs, cc_addrs, reply_to,
	subject, message_id, in_reply_to, refs, date_header, thread_id::text, in_reply_to_email_id::text, snippet, size, tls,
	raw, received_at, purged_at`

func scanMessage(row pgx.Row) (InboundMessage, error) {
	var m InboundMessage
	err := row.Scan(&m.ID, &m.InboxID, &m.ProjectID, &m.MailFrom, &m.From, &m.To, &m.Cc, &m.ReplyTo,
		&m.Subject, &m.MessageID, &m.InReplyTo, &m.References, &m.Date, &m.ThreadID, &m.InReplyToEmailID,
		&m.Snippet, &m.Size, &m.TLS, &m.Raw, &m.ReceivedAt, &m.PurgedAt)
	return m, err
}

// clean makes text from the internet storable: valid UTF-8, no NUL bytes
// (PostgreSQL text cannot hold them), at most n bytes.
func clean(s string, n int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func cleanList(l []string) []string {
	out := make([]string, 0, len(l))
	for i, v := range l {
		if i == 100 {
			break
		}
		out = append(out, clean(v, 1000))
	}
	return out
}

// InsertInboundMessage stores a message for an inbox and records a
// "received" event, in one transaction.
//
// Threading: In-Reply-To first, then References from newest to oldest, are
// matched against this project's sent emails and earlier inbound messages.
// The first match gives the thread; the first sent email matched gives
// in_reply_to_email_id. With no match the message starts its own thread.
//
// A message whose Message-ID this inbox already holds is not stored again
// (existing = true).
func (s *Store) InsertInboundMessage(ctx context.Context, id string, m NewInboundMessage) (out InboundMessage, existing bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx)

	var candidates []string
	seen := map[string]bool{}
	add := func(mid string) {
		if mid != "" && !seen[mid] && len(candidates) < 50 {
			seen[mid] = true
			candidates = append(candidates, mid)
		}
	}
	add(clean(m.InReplyTo, 998))
	for i := len(m.References) - 1; i >= 0; i-- {
		add(clean(m.References[i], 998))
	}

	threadID := id
	var replyTo *string
	if len(candidates) > 0 {
		type hit struct{ thread, email string }
		hits := map[string]hit{}
		rows, err := tx.Query(ctx, `SELECT e.message_id, COALESCE(t.thread_id, e.id)::text, e.id::text
			FROM emails e LEFT JOIN email_threads t ON t.email_id = e.id
			WHERE e.project_id = $1 AND e.message_id = ANY($2)`, m.ProjectID, candidates)
		if err != nil {
			return out, false, err
		}
		for rows.Next() {
			var mid string
			var h hit
			if err := rows.Scan(&mid, &h.thread, &h.email); err != nil {
				rows.Close()
				return out, false, err
			}
			hits[mid] = h
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, false, err
		}
		rows, err = tx.Query(ctx, `SELECT message_id, thread_id::text FROM inbound_messages
			WHERE project_id = $1 AND message_id = ANY($2)`, m.ProjectID, candidates)
		if err != nil {
			return out, false, err
		}
		for rows.Next() {
			var mid, thread string
			if err := rows.Scan(&mid, &thread); err != nil {
				rows.Close()
				return out, false, err
			}
			if _, ok := hits[mid]; !ok {
				hits[mid] = hit{thread: thread}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, false, err
		}
		threadSet := false
		for _, mid := range candidates {
			h, ok := hits[mid]
			if !ok {
				continue
			}
			if !threadSet {
				threadID, threadSet = h.thread, true
			}
			if h.email != "" && replyTo == nil {
				e := h.email
				replyTo = &e
			}
		}
	}

	tag, err := tx.Exec(ctx, `INSERT INTO inbound_messages (id, inbox_id, project_id, mail_from, from_addr, to_addrs,
			cc_addrs, reply_to, subject, message_id, in_reply_to, refs, date_header, thread_id, in_reply_to_email_id,
			snippet, size, remote_ip, helo, tls, raw)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		ON CONFLICT (inbox_id, message_id) WHERE message_id <> '' DO NOTHING`,
		id, m.InboxID, m.ProjectID, clean(m.MailFrom, 320), clean(m.From, 1000), cleanList(m.To), cleanList(m.Cc),
		cleanList(m.ReplyTo), clean(m.Subject, 2000), clean(m.MessageID, 998), clean(m.InReplyTo, 998),
		cleanList(m.References), clean(m.Date, 200), threadID, replyTo, clean(m.Snippet, 1000), m.Size,
		clean(m.RemoteIP, 64), clean(m.HELO, 255), m.TLS, m.Raw)
	if err != nil {
		return out, false, err
	}
	if tag.RowsAffected() == 0 {
		tx.Rollback(ctx)
		out, err = scanMessage(s.Pool.QueryRow(ctx, `SELECT `+messageCols+` FROM inbound_messages
			WHERE inbox_id = $1 AND message_id = $2`, m.InboxID, clean(m.MessageID, 998)))
		return out, true, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (inbound_message_id, type) VALUES ($1, 'received')`, id); err != nil {
		return out, false, err
	}
	out, err = scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM inbound_messages WHERE id = $1`, id))
	if err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

// ListInboundMessages returns an inbox's messages, newest first, up to
// limit, after the message before (exclusive) when it is set, and only
// those of one thread when threadID is set. more says whether older
// messages remain.
func (s *Store) ListInboundMessages(ctx context.Context, projectID, inboxID, before, threadID string, limit int) (msgs []InboundMessage, more bool, err error) {
	q := `SELECT ` + messageCols + ` FROM inbound_messages WHERE project_id = $1 AND inbox_id = $2`
	args := []any{projectID, inboxID}
	if threadID != "" {
		args = append(args, threadID)
		q += fmt.Sprintf(` AND thread_id = $%d`, len(args))
	}
	if before != "" {
		args = append(args, before)
		q += fmt.Sprintf(` AND (received_at, id) < (SELECT received_at, id FROM inbound_messages WHERE id = $%d AND inbox_id = $2)`, len(args))
	}
	args = append(args, limit+1)
	q += fmt.Sprintf(` ORDER BY received_at DESC, id DESC LIMIT $%d`, len(args))
	return s.queryMessages(ctx, q, limit, args...)
}

func (s *Store) queryMessages(ctx context.Context, q string, limit int, args ...any) ([]InboundMessage, bool, error) {
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []InboundMessage{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, false, err
		}
		m.Raw = nil // lists never carry bodies
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func (s *Store) GetInboundMessage(ctx context.Context, projectID, id string) (InboundMessage, error) {
	m, err := scanMessage(s.Pool.QueryRow(ctx, `SELECT `+messageCols+` FROM inbound_messages WHERE project_id = $1 AND id = $2`, projectID, id))
	return m, noRows(err)
}

// RepliesToEmail returns the email's thread and the inbound messages that
// answer it (In-Reply-To or References name it), newest first.
func (s *Store) RepliesToEmail(ctx context.Context, projectID, emailID string, limit int) (threadID string, msgs []InboundMessage, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT COALESCE(t.thread_id, e.id)::text FROM emails e
		LEFT JOIN email_threads t ON t.email_id = e.id WHERE e.project_id = $1 AND e.id = $2`, projectID, emailID).Scan(&threadID)
	if err != nil {
		return "", nil, noRows(err)
	}
	msgs, _, err = s.queryMessages(ctx, `SELECT `+messageCols+` FROM inbound_messages
		WHERE project_id = $1 AND in_reply_to_email_id = $2 ORDER BY received_at DESC, id DESC LIMIT $3`, limit, projectID, emailID, limit)
	return threadID, msgs, err
}

// LinkReply records that a sent email answers an inbound message and
// belongs to its thread.
func (s *Store) LinkReply(ctx context.Context, emailID, threadID, messageID string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO email_threads (email_id, thread_id, in_reply_to_message_id)
		VALUES ($1, $2, $3) ON CONFLICT (email_id) DO NOTHING`, emailID, threadID, messageID)
	return err
}

// SweepInbound purges inbound message bodies older than retention. The
// message stays listed (sender, subject, thread) without its content.
func (s *Store) SweepInbound(ctx context.Context, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE inbound_messages SET raw = NULL, snippet = '', purged_at = now()
		WHERE raw IS NOT NULL AND received_at < now() - make_interval(secs => $1)`, retention.Seconds()); err != nil {
		return fmt.Errorf("purge inbound messages: %w", err)
	}
	return nil
}

// EventData marshals event data, dropping empty values.
func EventData(m map[string]any) []byte {
	for k, v := range m {
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) == "" {
				delete(m, k)
			}
		case int:
			if x == 0 {
				delete(m, k)
			}
		case nil:
			delete(m, k)
		}
	}
	b, _ := json.Marshal(m)
	return b
}
