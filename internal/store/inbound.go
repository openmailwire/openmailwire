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
	// HoldUnauthenticated keeps mail whose sender is not authenticated
	// from sending_access keys until a full-access key releases it.
	HoldUnauthenticated bool
	CreatedAt           time.Time
}

const inboxCols = `id::text, project_id::text, domain_id::text, address, hold_unauthenticated, created_at`

func scanInbox(row pgx.Row) (Inbox, error) {
	var b Inbox
	err := row.Scan(&b.ID, &b.ProjectID, &b.DomainID, &b.Address, &b.HoldUnauthenticated, &b.CreatedAt)
	return b, err
}

func (s *Store) CreateInbox(ctx context.Context, projectID, domainID, address string, holdUnauthenticated bool) (Inbox, error) {
	b, err := scanInbox(s.Pool.QueryRow(ctx, `INSERT INTO inboxes (project_id, domain_id, address, hold_unauthenticated)
		VALUES ($1, $2, $3, $4) RETURNING `+inboxCols, projectID, domainID, address, holdUnauthenticated))
	if isUniqueViolation(err) {
		return Inbox{}, ErrConflict
	}
	return b, err
}

// SetInboxHold changes an inbox's hold_unauthenticated setting.
func (s *Store) SetInboxHold(ctx context.Context, projectID, id string, hold bool) (Inbox, error) {
	b, err := scanInbox(s.Pool.QueryRow(ctx, `UPDATE inboxes SET hold_unauthenticated = $3
		WHERE project_id = $1 AND id = $2 RETURNING `+inboxCols, projectID, id, hold))
	return b, noRows(err)
}

// InboxDomainKnown reports whether any inbox is on domain: the inbound
// node is then its MX, so postmaster@ and abuse@ there must be accepted.
func (s *Store) InboxDomainKnown(ctx context.Context, domain string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inboxes i JOIN domains d ON d.id = i.domain_id
		WHERE d.name = $1 AND d.status = 'verified')`, domain).Scan(&ok)
	return ok, err
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

// DeliveriesByMessageID returns the recipients of the sent email with this
// Message-ID (without angle brackets), only among emails whose return path
// is one of returnPaths or whose sending domain is one of fromDomains.
// Message-IDs are chosen by clients on POST /emails/raw, so an unscoped
// lookup could find another project's email.
func (s *Store) DeliveriesByMessageID(ctx context.Context, messageID string, returnPaths, fromDomains []string) ([]SentDelivery, error) {
	if len(returnPaths) == 0 && len(fromDomains) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT d.id::text, d.email_id::text, e.project_id::text, d.rcpt, d.status, e.return_path
		FROM emails e JOIN deliveries d ON d.email_id = e.id LEFT JOIN domains dm ON dm.id = e.domain_id
		WHERE e.message_id = $1 AND (e.return_path = ANY($2) OR dm.name = ANY($3))
		ORDER BY e.created_at DESC, d.rcpt LIMIT 1000`, messageID, returnPaths, fromDomains)
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
	} else {
		// A report that does not say which recipient complained counts
		// once per email, however often it is sent.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('complaint:' || $1, 0))`, c.EmailID); err != nil {
			return false, err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events
				WHERE email_id = $1 AND delivery_id IS NULL AND type = 'complained')`, c.EmailID).Scan(&exists); err != nil {
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
	Snippet    []byte // sealed
	Size       int
	RemoteIP   string
	HELO       string
	TLS        bool
	Raw        []byte // sealed

	Trust           string // authenticated, unauthenticated or failed
	AuthResults     []byte // JSON
	Held            bool
	SuspiciousRules []string
	// ContentHash identifies the content: a second copy with the same
	// Message-ID and hash is a duplicate; with another hash it is kept and
	// both are flagged (message_id_conflict).
	ContentHash string
	// EventData is the "received" event's data, which webhooks read.
	EventData []byte
}

// InboundMessage is a stored message. Raw is sealed and nil once purged.
type InboundMessage struct {
	ID                string
	InboxID           string
	ProjectID         string
	MailFrom          string
	From              string
	To, Cc            []string
	ReplyTo           []string
	Subject           string
	MessageID         string
	InReplyTo         string
	References        []string
	Date              string
	ThreadID          string
	InReplyToEmailID  *string
	Snippet           []byte // sealed; nil once purged, or for messages stored before v0.6
	Size              int
	RemoteIP          string
	TLS               bool
	Raw               []byte
	ReceivedAt        time.Time
	PurgedAt          *time.Time
	Trust             string
	AuthResults       []byte
	Held              bool
	ReleasedAt        *time.Time
	SuspiciousRules   []string
	MessageIDConflict bool
}

const messageCols = `id::text, inbox_id::text, project_id::text, mail_from, from_addr, to_addrs, cc_addrs, reply_to,
	subject, message_id, in_reply_to, refs, date_header, thread_id::text, in_reply_to_email_id::text, snippet_sealed, size,
	remote_ip, tls, raw, received_at, purged_at, trust, authentication_results, held, released_at, suspicious_rules,
	message_id_conflict`

func scanMessage(row pgx.Row) (InboundMessage, error) {
	var m InboundMessage
	err := row.Scan(&m.ID, &m.InboxID, &m.ProjectID, &m.MailFrom, &m.From, &m.To, &m.Cc, &m.ReplyTo,
		&m.Subject, &m.MessageID, &m.InReplyTo, &m.References, &m.Date, &m.ThreadID, &m.InReplyToEmailID,
		&m.Snippet, &m.Size, &m.RemoteIP, &m.TLS, &m.Raw, &m.ReceivedAt, &m.PurgedAt, &m.Trust, &m.AuthResults, &m.Held,
		&m.ReleasedAt, &m.SuspiciousRules, &m.MessageIDConflict)
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

	msgID := clean(m.MessageID, 998)
	if msgID != "" {
		// Serialize copies of one Message-ID, so the conflict check below
		// sees every other copy.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ' ' || $2, 0))`, m.InboxID, msgID); err != nil {
			return out, false, err
		}
	}
	trust := m.Trust
	if trust == "" {
		trust = "unauthenticated"
	}
	auth := m.AuthResults
	if len(auth) == 0 {
		auth = []byte("{}")
	}
	rules := m.SuspiciousRules
	if rules == nil {
		rules = []string{}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO inbound_messages (id, inbox_id, project_id, mail_from, from_addr, to_addrs,
			cc_addrs, reply_to, subject, message_id, in_reply_to, refs, date_header, thread_id, in_reply_to_email_id,
			snippet_sealed, size, remote_ip, helo, tls, raw, trust, authentication_results, held, suspicious_rules, content_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23,
			$24, $25, $26)
		ON CONFLICT (inbox_id, message_id, content_hash) WHERE message_id <> '' DO NOTHING`,
		id, m.InboxID, m.ProjectID, clean(m.MailFrom, 320), clean(m.From, 1000), cleanList(m.To), cleanList(m.Cc),
		cleanList(m.ReplyTo), clean(m.Subject, 2000), msgID, clean(m.InReplyTo, 998),
		cleanList(m.References), clean(m.Date, 200), threadID, replyTo, m.Snippet, m.Size,
		clean(m.RemoteIP, 64), clean(m.HELO, 255), m.TLS, m.Raw, trust, auth, m.Held, rules, m.ContentHash)
	if err != nil {
		return out, false, err
	}
	if tag.RowsAffected() == 0 {
		tx.Rollback(ctx)
		out, err = scanMessage(s.Pool.QueryRow(ctx, `SELECT `+messageCols+` FROM inbound_messages
			WHERE inbox_id = $1 AND message_id = $2 AND content_hash = $3`, m.InboxID, msgID, m.ContentHash))
		return out, true, err
	}
	if msgID != "" {
		// Another copy with this Message-ID but different content: one of
		// them may be forged. Keep both and flag them.
		if _, err := tx.Exec(ctx, `UPDATE inbound_messages SET message_id_conflict = true
			WHERE inbox_id = $1 AND message_id = $2 AND NOT message_id_conflict
				AND EXISTS (SELECT 1 FROM inbound_messages o WHERE o.inbox_id = $1 AND o.message_id = $2 AND o.id <> $3)`,
			m.InboxID, msgID, id); err != nil {
			return out, false, err
		}
	}
	data := m.EventData
	if len(data) == 0 {
		data = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events (inbound_message_id, type, data) VALUES ($1, 'received', $2)`, id, data); err != nil {
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
//
// Held messages are left out unless includeHeld is set.
func (s *Store) ListInboundMessages(ctx context.Context, projectID, inboxID, before, threadID string, limit int, includeHeld bool) (msgs []InboundMessage, more bool, err error) {
	q := `SELECT ` + messageCols + ` FROM inbound_messages WHERE project_id = $1 AND inbox_id = $2`
	args := []any{projectID, inboxID}
	if !includeHeld {
		q += ` AND NOT held`
	}
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
// answer it (In-Reply-To or References name it), newest first. Held
// messages are left out unless includeHeld is set.
func (s *Store) RepliesToEmail(ctx context.Context, projectID, emailID string, limit int, includeHeld bool) (threadID string, msgs []InboundMessage, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT COALESCE(t.thread_id, e.id)::text FROM emails e
		LEFT JOIN email_threads t ON t.email_id = e.id WHERE e.project_id = $1 AND e.id = $2`, projectID, emailID).Scan(&threadID)
	if err != nil {
		return "", nil, noRows(err)
	}
	msgs, _, err = s.queryMessages(ctx, `SELECT `+messageCols+` FROM inbound_messages
		WHERE project_id = $1 AND in_reply_to_email_id = $2 AND (NOT held OR $4) ORDER BY received_at DESC, id DESC LIMIT $3`,
		limit, projectID, emailID, limit, includeHeld)
	return threadID, msgs, err
}

// ReleaseInboundMessage makes a held message visible to every key and
// records a "released" event. changed is false when it was not held.
func (s *Store) ReleaseInboundMessage(ctx context.Context, projectID, id, keyID string) (m InboundMessage, changed bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return m, false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE inbound_messages SET held = false, released_at = now()
		WHERE project_id = $1 AND id = $2 AND held`, projectID, id)
	if err != nil {
		return m, false, err
	}
	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO events (inbound_message_id, type, data) VALUES ($1, 'released', $2)`,
			id, EventData(map[string]any{"api_key_id": keyID})); err != nil {
			return m, false, err
		}
	}
	m, err = scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM inbound_messages WHERE project_id = $1 AND id = $2`, projectID, id))
	if err != nil {
		return m, false, noRows(err)
	}
	return m, tag.RowsAffected() > 0, tx.Commit(ctx)
}

// SetSnippet stores a sealed snippet derived for a message stored before
// snippets were sealed.
func (s *Store) SetSnippet(ctx context.Context, id string, sealed []byte) error {
	_, err := s.Pool.Exec(ctx, `UPDATE inbound_messages SET snippet_sealed = $2 WHERE id = $1 AND raw IS NOT NULL AND snippet_sealed IS NULL`, id, sealed)
	return err
}

// RecentInboundCount counts an inbox's messages received in the last
// window, stopping at limit.
func (s *Store) RecentInboundCount(ctx context.Context, inboxID string, window time.Duration, limit int) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM inbound_messages
		WHERE inbox_id = $1 AND received_at > now() - make_interval(secs => $2) LIMIT $3) x`, inboxID, window.Seconds(), limit).Scan(&n)
	return n, err
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
	if _, err := s.Pool.Exec(ctx, `UPDATE inbound_messages SET raw = NULL, snippet = '', snippet_sealed = NULL, purged_at = now()
		WHERE raw IS NOT NULL AND received_at < now() - make_interval(secs => $1)`, retention.Seconds()); err != nil {
		return fmt.Errorf("purge inbound messages: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM domain_reports WHERE received_at < now() - make_interval(secs => $1)`,
		ReportRetention.Seconds()); err != nil {
		return fmt.Errorf("purge domain reports: %w", err)
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
