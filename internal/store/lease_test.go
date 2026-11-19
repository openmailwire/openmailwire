package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testStore returns a store on a schema of its own in the test database,
// dropped when the test ends, so these tests never meet the end-to-end
// tests' data (they share the database and may run at the same time).
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("OMW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("OMW_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	schema := "omw_store_" + hex.EncodeToString(b)
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := &Store{Pool: pool}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

type raceFixture struct {
	st                           *Store
	projectID, workerID, emailID string
}

func newRaceFixture(t *testing.T) *raceFixture {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	pid, err := st.CreateProject(ctx, "race")
	if err != nil {
		t.Fatal(err)
	}
	wid, err := st.CreateWorker(ctx, "w1", []byte("hash-w1"))
	if err != nil {
		t.Fatal(err)
	}
	id := "6f0c3a52-1b7e-4c2a-9d1e-0a1b2c3d4e5f"
	if _, _, err := st.InsertEmail(ctx, NewEmail{
		ID: id, ProjectID: pid, From: "a@x.test", To: []string{"r@example.test"}, Subject: "s",
		MessageID: id + "@x.test", ReturnPath: "bounce.x.test", Raw: []byte("sealed"),
		RetryUncertain: true, ExpiresAt: time.Now().Add(time.Hour), Recipients: []string{"r@example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	return &raceFixture{st: st, projectID: pid, workerID: wid, emailID: id}
}

func (f *raceFixture) status(t *testing.T) string {
	t.Helper()
	var s string
	if err := f.st.Pool.QueryRow(context.Background(), `SELECT status FROM deliveries WHERE email_id = $1`, f.emailID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestLeaseRechecksCancel covers R1#2 and R3#2: a cancel that commits after
// a lease has chosen a delivery, but before it locks it, wins. The lease
// used to overwrite canceled with sending, and the API had already told the
// caller the recipient was canceled.
func TestLeaseRechecksCancel(t *testing.T) {
	f := newRaceFixture(t)
	ctx := context.Background()
	var canceled []string
	leaseHook = func() {
		var err error
		canceled, err = f.st.CancelEmail(ctx, f.projectID, f.emailID, "")
		if err != nil {
			t.Error(err)
		}
	}
	defer func() { leaseHook = nil }()
	jobs, err := f.st.LeaseJobs(ctx, LeaseOptions{WorkerID: f.workerID, Max: 10, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(canceled) != 1 {
		t.Fatalf("the cancel did not run in the window: %v", canceled)
	}
	if len(jobs) != 0 {
		t.Fatalf("the canceled delivery was leased: %+v", jobs)
	}
	if s := f.status(t); s != "canceled" {
		t.Fatalf("status %s, want canceled", s)
	}
	var attempts int
	f.st.Pool.QueryRow(ctx, `SELECT count(*) FROM attempts`).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("%d attempts recorded for a canceled delivery", attempts)
	}
}

// TestLeaseRechecksExpiry is the sweep's side of R3#2: a delivery the sweep
// expires in that window is not leased either.
func TestLeaseRechecksExpiry(t *testing.T) {
	f := newRaceFixture(t)
	ctx := context.Background()
	leaseHook = func() {
		if _, err := f.st.Pool.Exec(ctx, `UPDATE emails SET expires_at = now() - interval '1 second' WHERE id = $1`, f.emailID); err != nil {
			t.Error(err)
		}
		if err := f.st.Sweep(ctx, time.Hour); err != nil {
			t.Error(err)
		}
	}
	defer func() { leaseHook = nil }()
	jobs, err := f.st.LeaseJobs(ctx, LeaseOptions{WorkerID: f.workerID, Max: 10, Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("the expired delivery was leased: %+v", jobs)
	}
	if s := f.status(t); s != "expired" {
		t.Fatalf("status %s, want expired", s)
	}
}
