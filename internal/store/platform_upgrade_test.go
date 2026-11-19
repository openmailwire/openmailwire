package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/openmailwire/openmailwire/internal/secret"
)

// An upgrade from v0.5 with a fan-out backlog: events the old cursor had
// passed are marked fanned out, the backlog is not, and webhooks with
// pending deliveries get a due time. Runs the migrations in a scratch
// schema of the test database.
func TestPlatformMigrationUpgrade(t *testing.T) {
	url := os.Getenv("OMW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OMW_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	schema := "upgrade_" + secret.RandomHex(6)
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+schema+`; SET search_path = `+schema); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)

	run := func(name string) {
		t.Helper()
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, m := range []string{"0001_init.sql", "0002_webhooks.sql", "0003_keys_and_email_api.sql", "0004_inbound.sql", "0005_delivery.sql"} {
		run(m)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO projects (id, name) VALUES ('00000000-0000-4000-8000-000000000001', 'p');
		INSERT INTO emails (id, project_id, from_addr, message_id, return_path, expires_at)
			VALUES ('00000000-0000-4000-8000-0000000000e1', '00000000-0000-4000-8000-000000000001', 'a@x.test', 'm1', 'b.x.test', now() + interval '1 day');
		INSERT INTO events (email_id, type) SELECT '00000000-0000-4000-8000-0000000000e1', 'queued' FROM generate_series(1, 10);
		UPDATE webhook_cursor SET last_event_id = (SELECT min(id) + 5 FROM events);
		INSERT INTO webhooks (id, project_id, endpoint, secret)
			VALUES ('00000000-0000-4000-8000-0000000000a1', '00000000-0000-4000-8000-000000000001', 'https://h.test', '\x00'),
			       ('00000000-0000-4000-8000-0000000000a2', '00000000-0000-4000-8000-000000000001', 'https://h.test/2', '\x00');
		INSERT INTO webhook_deliveries (webhook_id, event_id, type, payload, next_attempt_at)
			VALUES ('00000000-0000-4000-8000-0000000000a1', 5, 'email.sent', '\x7b7d', now() - interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	run("0010_platform.sql")

	var done, backlog int
	if err := conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE fanned_out), count(*) FILTER (WHERE NOT fanned_out) FROM events`).
		Scan(&done, &backlog); err != nil {
		t.Fatal(err)
	}
	if done != 6 || backlog != 4 {
		t.Fatalf("after the upgrade %d events are fanned out and %d wait; want 6 and 4 (the backlog past the cursor)", done, backlog)
	}
	var pendingDue, idleDue bool
	conn.QueryRow(ctx, `SELECT
		(SELECT next_due_at < now() FROM webhooks WHERE id = '00000000-0000-4000-8000-0000000000a1'),
		(SELECT next_due_at IS NULL FROM webhooks WHERE id = '00000000-0000-4000-8000-0000000000a2')`).Scan(&pendingDue, &idleDue)
	if !pendingDue || !idleDue {
		t.Fatalf("next_due_at after the upgrade: webhook with a pending delivery due %v, idle webhook unset %v", pendingDue, idleDue)
	}

	// New events wait for fan-out; a delivery inserted anywhere makes its
	// webhook due (trigger), but not while its breaker is open.
	var fanned bool
	if err := conn.QueryRow(ctx, `INSERT INTO events (email_id, type) VALUES ('00000000-0000-4000-8000-0000000000e1', 'delivered')
		RETURNING fanned_out`).Scan(&fanned); err != nil || fanned {
		t.Fatalf("a new event must default to not fanned out: %v %v", fanned, err)
	}
	if _, err := conn.Exec(ctx, `
		UPDATE webhooks SET breaker_until = now() + interval '1 hour' WHERE id = '00000000-0000-4000-8000-0000000000a1';
		INSERT INTO webhook_deliveries (webhook_id, event_id, type, payload) VALUES
			('00000000-0000-4000-8000-0000000000a1', 11, 'email.delivered', '\x7b7d'),
			('00000000-0000-4000-8000-0000000000a2', 11, 'email.delivered', '\x7b7d')`); err != nil {
		t.Fatal(err)
	}
	var paused, due bool
	conn.QueryRow(ctx, `SELECT
		(SELECT next_due_at > now() + interval '59 minutes' FROM webhooks WHERE id = '00000000-0000-4000-8000-0000000000a1'),
		(SELECT next_due_at <= now() FROM webhooks WHERE id = '00000000-0000-4000-8000-0000000000a2')`).Scan(&paused, &due)
	if !paused || !due {
		t.Fatalf("after inserting deliveries: paused webhook stays paused %v, other webhook due %v", paused, due)
	}
}
