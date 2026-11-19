package e2e

// Regression tests for the v0.5.0 audit findings on delivery (R1#2 and
// R3#1 to R3#13). Each one failed before its fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtptest"
	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/worker"
)

// queueRaw stores one email with one recipient for project pid, straight
// through the store (no API, no network).
func (e *env) queueRaw(pid, rcpt string) string {
	e.t.Helper()
	id := secret.UUID()
	if _, _, err := e.st.InsertEmail(e.ctx, store.NewEmail{
		ID: id, ProjectID: pid, From: "a@x.test", To: []string{rcpt}, Subject: "s",
		MessageID: id + "@x.test", ReturnPath: "bounce.x.test", Raw: []byte("sealed"),
		RetryUncertain: true, ExpiresAt: time.Now().Add(time.Hour), Recipients: []string{rcpt},
	}); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// deliverAndReport leases one job for rcpt, runs it against srv and stores
// the report, as a worker would.
func (e *env) deliverAndReport(srv *smtptest.Server, o store.LeaseOptions) worker.Result {
	e.t.Helper()
	jobs, err := e.st.LeaseJobs(e.ctx, o)
	if err != nil || len(jobs) != 1 {
		e.t.Fatalf("lease: %v, %d jobs", err, len(jobs))
	}
	j := jobs[0].Job
	j.MailFrom, j.Raw = "b@bounce.x.test", []byte("Subject: s\r\n\r\nx\r\n")
	res := (&worker.Deliverer{Override: srv.Addr, EHLO: "w.test", NoReuse: true}).Deliver(e.ctx, j)
	if _, err := e.st.ReportAttempt(e.ctx, e.workerID, protocol.Report{
		AttemptID: j.AttemptID, Outcome: res.Outcome, SMTPCode: res.Code, EnhancedCode: res.Enhanced, Response: res.Response,
		Stage: res.Stage,
	}, store.ReportOptions{DomainConcurrency: 10}); err != nil {
		e.t.Fatal(err)
	}
	return res
}

// TestThrottleNotPoisonedByOneMailbox covers R3#1. A per-mailbox deferral
// (Gmail's "450 4.2.1 ... receiving mail at a rate") and sender
// authentication codes (421 4.7.30) never slow a receiver down for every
// project; a real throttle needs two recipients.
func TestThrottleNotPoisonedByOneMailbox(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	pidB, err := e.st.CreateProject(e.ctx, "victim")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	o := store.LeaseOptions{WorkerID: e.workerID, Max: 10, Lease: time.Minute, DomainConcurrency: 10}

	// Project A's busy mailbox.
	srv.ReplyToRcpt("ann@gmail.com", "450 4.2.1 The user you are trying to contact is receiving mail at a rate that prevents additional messages from being delivered.")
	e.queueRaw(e.projectID, "ann@gmail.com")
	if res := e.deliverAndReport(srv, o); res.Code != 450 || res.Stage != protocol.StageRcpt {
		t.Fatalf("worker result: %+v", res)
	}
	e.queueRaw(pidB, "bob@gmail.com")
	jobs, err := e.st.LeaseJobs(e.ctx, o)
	if err != nil || len(jobs) != 1 || jobs[0].Rcpt != "bob@gmail.com" {
		t.Fatalf("project B's mail is held back by project A's mailbox: %v %+v", err, jobs)
	}
	if _, err := e.st.GetReceiverState(e.ctx, "gmail.com"); err != store.ErrNotFound {
		t.Fatalf("a per-mailbox reply changed the receiver state: %v", err)
	}
	reportDelivered(t, e, jobs[0])

	// Authentication problems: recorded for the project and its sending
	// domain, never a throttle, even as a 421 from several recipients.
	srv.ReplyToDomain("auth.test", "421 4.7.30 DKIM did not pass, this message is rate limited")
	var authID string
	for _, r := range []string{"a1@auth.test", "a2@auth.test"} {
		authID = e.queueRaw(e.projectID, r)
		e.deliverAndReport(srv, o)
	}
	if rs, err := e.st.GetReceiverState(e.ctx, "auth.test"); err != store.ErrNotFound {
		t.Fatalf("authentication codes throttled the receiver: %+v %v", rs, err)
	}
	issues, err := e.st.AuthIssues(e.ctx, e.projectID, "x.test")
	if err != nil || len(issues) != 1 || issues[0].EnhancedCode != "4.7.30" || issues[0].Count != 2 || issues[0].Scope != "auth.test" {
		t.Fatalf("auth issues: %v %+v", err, issues)
	}
	if ev := e.event(authID, "auth_problem"); ev["sending_domain"] != "x.test" || ev["enhanced_code"] != "4.7.30" {
		t.Fatalf("auth_problem event: %v", ev)
	}

	// A connection-level throttle from one recipient only waits; a second
	// recipient confirms it.
	srv.ReplyToDomain("rate.test", "421 4.7.0 too many connections from your IP")
	e.queueRaw(e.projectID, "r1@rate.test")
	e.deliverAndReport(srv, o)
	if rs, err := e.st.GetReceiverState(e.ctx, "rate.test"); err != nil || rs.BackoffUntil != nil || rs.Limit != 10 {
		t.Fatalf("one recipient throttled the receiver: %+v %v", rs, err)
	}
	e.queueRaw(pidB, "r2@rate.test")
	e.deliverAndReport(srv, o)
	if rs, err := e.st.GetReceiverState(e.ctx, "rate.test"); err != nil || rs.BackoffUntil == nil || rs.Limit != 5 {
		t.Fatalf("two recipients did not throttle the receiver: %+v %v", rs, err)
	}
}

// TestCancelAndLeaseAgree covers R1#2 end to end: whatever the timing, a
// recipient the cancel API reports canceled is never handed to a worker.
// (internal/store's TestLeaseRechecksCancel hits the exact window.)
func TestCancelAndLeaseAgree(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	e.addVerifiedDomain("race.test")
	opts := store.LeaseOptions{WorkerID: e.workerID, Max: 10, Lease: time.Minute, DomainConcurrency: 5}
	for try := 0; try < 10; try++ {
		rcpt := fmt.Sprintf("victim@t%d.test", try)
		code, sent := e.call("POST", "/emails", "", map[string]any{"from": "a@race.test", "to": rcpt, "subject": "s", "text": "t"})
		if code != 200 {
			t.Fatal(code, sent)
		}
		id := sent["id"].(string)
		var jobs []store.LeasedJob
		var lerr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); jobs, lerr = e.st.LeaseJobs(e.ctx, opts) }()
		ccode, cres := e.call("POST", "/emails/"+id+"/cancel", "", nil)
		wg.Wait()
		if lerr != nil {
			t.Fatal(lerr)
		}
		leased := false
		for _, j := range jobs {
			leased = leased || j.Rcpt == rcpt
		}
		canceled := ccode == 200 && fmt.Sprint(cres["canceled"]) == "["+rcpt+"]"
		if canceled && leased {
			t.Fatalf("try %d: the API answered canceled, but the delivery was leased", try)
		}
		if !canceled && !leased {
			t.Fatalf("try %d: neither canceled (%d %v) nor leased", try, ccode, cres)
		}
		for _, j := range jobs {
			reportDelivered(t, e, j)
		}
	}
}

// TestLeaseCostIndependentOfBacklog covers R3#13: a big backlog on a domain
// at its limit used to be read on every lease, inside the global lease lock
// (about 0.5 µs a row). Now a lease reads at most a fixed head of the queue
// and then goes domain by domain.
func TestLeaseCostIndependentOfBacklog(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	bulk := secret.UUID()
	if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO emails (id, project_id, from_addr, subject, message_id, return_path, raw, expires_at)
		VALUES ($1, $2, 'a@x.test', 's', 'm@x.test', 'bounce.x.test', 'x', now() + interval '1 hour')`, bulk, e.projectID); err != nil {
		t.Fatal(err)
	}
	// full.test is at its limit: 10 in flight.
	if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO deliveries (email_id, rcpt, rcpt_domain, status, attempts, lease_worker, lease_attempt, lease_expires_at)
		SELECT $1, 's' || g || '@full.test', 'full.test', 'sending', 1, $2, gen_random_uuid(), now() + interval '1 hour'
		FROM generate_series(1, 10) g`, bulk, e.workerID); err != nil {
		t.Fatal(err)
	}
	o := store.LeaseOptions{WorkerID: e.workerID, Max: 20, Lease: time.Minute, DomainConcurrency: 10}
	measure := func(backlog int) time.Duration {
		if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO deliveries (email_id, rcpt, rcpt_domain, status, next_attempt_at)
			SELECT $1, 'u' || g || '-' || $2::text || '@full.test', 'full.test', 'queued', now() - interval '2 hours'
			FROM generate_series(1, $2::int) g`, bulk, backlog); err != nil {
			t.Fatal(err)
		}
		e.st.Pool.Exec(e.ctx, `ANALYZE deliveries`)
		best := time.Hour
		for i := 0; i < 3; i++ {
			id := e.queueRaw(e.projectID, "x@other.test")
			start := time.Now()
			jobs, err := e.st.LeaseJobs(context.Background(), o)
			took := time.Since(start)
			if err != nil || len(jobs) != 1 || jobs[0].Rcpt != "x@other.test" {
				t.Fatalf("lease behind a backlog of %d: %v %+v", backlog, err, jobs)
			}
			reportDelivered(t, e, jobs[0])
			e.st.Pool.Exec(e.ctx, `DELETE FROM emails WHERE id = $1`, id)
			best = min(best, took)
		}
		return best
	}
	small := measure(5000)
	large := measure(95000) // 100,000 in all
	t.Logf("lease behind 5,000 held-up deliveries: %v; behind 100,000: %v", small, large)
	if large > 3*small+5*time.Millisecond {
		t.Fatalf("lease time grows with the backlog: %v for 5,000, %v for 100,000", small, large)
	}
}

// TestScheduledMailRedatedAtLease covers R3#3: a scheduled message is
// leased with the Date of its sending and a fresh DKIM signature, not those
// of the request (up to 30 days earlier). The rest is byte for byte.
func TestScheduledMailRedatedAtLease(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	_, dkimTXT := e.addVerifiedDomain("acme.test")
	at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	code, b := e.call("POST", "/emails", "", map[string]any{"from": "hello@acme.test", "to": "x@example.test",
		"subject": "s", "text": "t", "scheduled_at": at.Format(time.RFC3339)})
	if code != 200 {
		t.Fatalf("%d %v", code, b)
	}
	id := b["id"].(string)
	var sealed []byte
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT raw FROM emails WHERE id = $1`, id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	stored, _ := e.sealer.Open(sealed)
	// Three days pass.
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE deliveries SET next_attempt_at = now() WHERE email_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	c := &worker.Client{Base: e.ts.URL, Token: e.workerToken, HTTP: http.DefaultClient}
	var resp protocol.LeaseResponse
	if err := c.Post(e.ctx, "/internal/worker/lease", protocol.LeaseRequest{Max: 5}, &resp); err != nil || len(resp.Jobs) != 1 {
		t.Fatalf("lease: %v %+v", err, resp)
	}
	raw := resp.Jobs[0].Raw
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	date, err := msg.Header.Date()
	if err != nil || time.Since(date) > time.Minute || time.Since(date) < -time.Minute {
		t.Fatalf("leased Date %q, want about now (%v)", msg.Header.Get("Date"), err)
	}
	verifyDKIM(t, raw, dkimTXT)
	body := func(m []byte) string { return string(m[bytes.Index(m, []byte("\r\n\r\n")):]) }
	fields := func(m []byte) string {
		var keep []string
		h := string(m[:bytes.Index(m, []byte("\r\n\r\n"))])
		for _, f := range strings.Split(strings.ReplaceAll(h, "\r\n ", " "), "\r\n") {
			if !strings.HasPrefix(f, "DKIM-Signature:") && !strings.HasPrefix(f, "Date:") && !strings.HasPrefix(f, "\t") {
				keep = append(keep, f)
			}
		}
		return strings.Join(keep, "\n")
	}
	if body(raw) != body(stored) || fields(raw) != fields(stored) {
		t.Fatalf("the message changed beyond Date and DKIM-Signature:\n%s\n---\n%s", raw, stored)
	}
}

// TestUnsealFailureReleasesLease covers R3#7: a job the control plane
// cannot unseal is given back at once (no attempt, an unseal_failed
// event), instead of staying leased until it ends "unknown".
func TestUnsealFailureReleasesLease(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	id := secret.UUID()
	if _, _, err := e.st.InsertEmail(e.ctx, store.NewEmail{
		ID: id, ProjectID: e.projectID, From: "a@x.test", To: []string{"r@example.test"}, Subject: "s",
		MessageID: id + "@x.test", ReturnPath: "bounce.x.test", Raw: []byte("not-sealed-with-this-key"),
		RetryUncertain: false, ExpiresAt: time.Now().Add(time.Hour), Recipients: []string{"r@example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	c := &worker.Client{Base: e.ts.URL, Token: e.workerToken, HTTP: http.DefaultClient}
	var resp protocol.LeaseResponse
	if err := c.Post(e.ctx, "/internal/worker/lease", protocol.LeaseRequest{Max: 5}, &resp); err != nil {
		t.Fatal(err)
	}
	st, att := e.delivery(id, "r@example.test")
	if len(resp.Jobs) != 0 || st != "queued" || att != 0 {
		t.Fatalf("after the lease: %d jobs, delivery %s with %d attempts", len(resp.Jobs), st, att)
	}
	var due time.Duration
	e.st.Pool.QueryRow(e.ctx, `SELECT next_attempt_at - now() FROM deliveries WHERE email_id = $1`, id).Scan(&due)
	if due < 4*time.Minute {
		t.Fatalf("released delivery is due again in %v; it would loop", due)
	}
	if ev := e.event(id, "unseal_failed"); !strings.Contains(fmt.Sprint(ev["reason"]), "OMW_SECRET_KEY") {
		t.Fatalf("unseal_failed event: %v", ev)
	}
	var attempts int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM attempts a JOIN deliveries d ON d.id = a.delivery_id WHERE d.email_id = $1`, id).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("%d attempt rows for a job that was never handed out", attempts)
	}
	if err := e.st.Sweep(e.ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.delivery(id, "r@example.test"); st == "unknown" {
		t.Fatal("never handed to a worker, yet the delivery ended unknown")
	}
}

// TestNonASCIIReplyIsStored covers R3#4: a long reply with multi-byte
// characters (and a NUL) is cut on character boundaries, so the report is
// stored instead of failing forever.
func TestNonASCIIReplyIsStored(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	text := strings.Repeat("x", 987) + strings.Repeat("é", 60) // 1107 bytes of reply text
	srv.ReplyToRcpt("r@example.test", "550 5.1.1 "+text)
	id := e.queueRaw(e.projectID, "r@example.test")
	jobs, err := e.st.LeaseJobs(e.ctx, store.LeaseOptions{WorkerID: e.workerID, Max: 1, Lease: time.Minute})
	if err != nil || len(jobs) != 1 {
		t.Fatal(err, len(jobs))
	}
	j := jobs[0].Job
	j.MailFrom, j.Raw = "b@bounce.x.test", []byte("Subject: s\r\n\r\nx\r\n")
	res := (&worker.Deliverer{Override: srv.Addr, EHLO: "w.test"}).Deliver(e.ctx, j)
	// What the worker's Client sends and the API decodes.
	b, _ := json.Marshal(protocol.Report{AttemptID: j.AttemptID, Outcome: res.Outcome, SMTPCode: res.Code, EnhancedCode: res.Enhanced,
		Response: res.Response, MXHost: "mx\x00.example.test", TLSPolicy: strings.Repeat("ü", 400)})
	var rep protocol.Report
	json.Unmarshal(b, &rep)
	if _, err := e.st.ReportAttempt(e.ctx, e.workerID, rep, store.ReportOptions{DomainConcurrency: 10}); err != nil {
		t.Fatalf("the report cannot be stored, so the worker would retry it forever: %v", err)
	}
	if st, _ := e.delivery(id, "r@example.test"); st != "bounced" {
		t.Fatalf("status %s", st)
	}
}

// TestReportAndSweepDoNotDeadlock covers R3#8: ReportAttempt and the
// sweep's reclaim lock deliveries and attempts in the same order.
func TestReportAndSweepDoNotDeadlock(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	e.queueRaw(e.projectID, "r@example.test")
	jobs, err := e.st.LeaseJobs(e.ctx, store.LeaseOptions{WorkerID: e.workerID, Max: 1, Lease: time.Minute})
	if err != nil || len(jobs) != 1 {
		t.Fatal(err, len(jobs))
	}
	att := jobs[0].AttemptID
	e.st.Pool.Exec(e.ctx, `UPDATE deliveries SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, jobs[0].DeliveryID)

	// Hold the attempt row so the report queues for it after taking the
	// delivery; then the sweep comes.
	hold, err := e.st.Pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(e.ctx)
	if _, err := hold.Exec(e.ctx, `SELECT 1 FROM attempts WHERE id = $1 FOR UPDATE`, att); err != nil {
		t.Fatal(err)
	}
	waiters := func() (n int) {
		e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()
			AND datname = current_database()`).Scan(&n)
		return n
	}
	errs := make(chan string, 2)
	go func() {
		_, err := e.st.ReportAttempt(e.ctx, e.workerID, protocol.Report{AttemptID: att, Outcome: "delivered", SMTPCode: 250}, store.ReportOptions{DomainConcurrency: 10})
		errs <- fmt.Sprintf("report: %v", err)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for waiters() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the report never waited for the attempt")
		}
		time.Sleep(5 * time.Millisecond)
	}
	go func() { errs <- fmt.Sprintf("sweep: %v", e.st.Sweep(e.ctx, time.Hour)) }()
	time.Sleep(300 * time.Millisecond) // the sweep skips the locked delivery, or waits
	hold.Commit(e.ctx)
	a, b := <-errs, <-errs
	if strings.Contains(a+b, "deadlock") || !strings.Contains(a+b, "report: <nil>") || !strings.Contains(a+b, "sweep: <nil>") {
		t.Fatalf("%s | %s", a, b)
	}
	var status string
	e.st.Pool.QueryRow(e.ctx, `SELECT status FROM deliveries WHERE id = $1`, jobs[0].DeliveryID).Scan(&status)
	if status != "delivered" {
		t.Fatalf("status %s; the report said delivered", status)
	}
}
