package e2e

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtptest"
	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/worker"
)

// newDeliveryEnv is newEnv plus a clean receiver_state, which newEnv's
// TRUNCATE does not reach (it references neither projects nor workers).
// The table is emptied again when the test ends, so no other test meets a
// domain in backoff.
func newDeliveryEnv(t *testing.T, tweak func(*config.Control)) *env {
	e := newEnv(t, tweak)
	reset := func() {
		if _, err := e.st.Pool.Exec(context.Background(), `TRUNCATE receiver_state`); err != nil {
			t.Error(err)
		}
	}
	reset()
	t.Cleanup(reset)
	return e
}

// send queues one email from acme.test and returns its id.
func (e *env) send(to ...string) string {
	e.t.Helper()
	code, b := e.call("POST", "/emails", "", map[string]any{"from": "Acme <hello@acme.test>", "to": to, "subject": "s", "text": "t"})
	if code != 200 {
		e.t.Fatalf("send to %v: %d %v", to, code, b)
	}
	return b["id"].(string)
}

// delivery returns a recipient's status and attempt count, from the database.
func (e *env) delivery(emailID, rcpt string) (status string, attempts int) {
	e.t.Helper()
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT status, attempts FROM deliveries WHERE email_id = $1 AND rcpt = $2`,
		emailID, rcpt).Scan(&status, &attempts); err != nil {
		e.t.Fatal(err)
	}
	return status, attempts
}

// waitDelivery waits until the recipient has had n attempts and is in status.
func (e *env) waitDelivery(emailID, rcpt string, n int, status string) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s, a := e.delivery(emailID, rcpt)
		if s == status && a == n {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s for %s: %s after %d attempts, want %s after %d", emailID, rcpt, s, a, status, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// retryNow makes the email's waiting deliveries due at once, instead of
// after the retry backoff.
func (e *env) retryNow(emailID string) {
	e.t.Helper()
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE deliveries SET next_attempt_at = now()
		WHERE email_id = $1 AND status IN ('queued', 'deferred')`, emailID); err != nil {
		e.t.Fatal(err)
	}
}

// endBackoff ends a scope's throttling backoff now (a recipient domain of
// no known provider is its own scope).
func (e *env) endBackoff(domain string) {
	e.t.Helper()
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE receiver_state SET backoff_until = now() - interval '1 second'
		WHERE scope = $1`, domain); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) receiver(domain string) store.ReceiverState {
	e.t.Helper()
	rs, err := e.st.GetReceiverState(e.ctx, domain)
	if err != nil {
		e.t.Fatalf("receiver state of %s: %v", domain, err)
	}
	return rs
}

// event returns the data of the email's first event of type typ.
func (e *env) event(emailID, typ string) map[string]any {
	e.t.Helper()
	_, v := e.call("GET", "/emails/"+emailID, "", nil)
	evs, _ := v["events"].([]any)
	for _, ev := range evs {
		m := ev.(map[string]any)
		if m["type"] == typ {
			data, _ := m["data"].(map[string]any)
			return data
		}
	}
	b, _ := json.MarshalIndent(evs, "", "  ")
	e.t.Fatalf("email %s has no %q event; events:\n%s", emailID, typ, b)
	return nil
}

// backoffLeft is how long the domain's backoff still runs, by the
// database's clock.
func (e *env) backoffLeft(domain string) time.Duration {
	e.t.Helper()
	var secs float64
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT EXTRACT(EPOCH FROM backoff_until - now())::float8
		FROM receiver_state WHERE scope = $1`, domain).Scan(&secs); err != nil {
		e.t.Fatal(err)
	}
	return time.Duration(secs * float64(time.Second))
}

func near(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	if got < want-15*time.Second || got > want+time.Second {
		t.Fatalf("%s: %v left, want about %v", what, got.Round(time.Second), want)
	}
}

// TestThrottling: a receiver that says "slow down" to two recipients halves
// the limit of its scope (here the domain itself: its MX belongs to no known
// provider) for the sending IP and starts a backoff that direct workers
// with that IP respect; deliveries raise the limit again.
func TestThrottling(t *testing.T) {
	e := newDeliveryEnv(t, func(c *config.Control) { c.DomainConcurrency = 4 })
	e.addVerifiedDomain("acme.test")
	e.smtp.ReplyToDomain("throttle.test", "421 4.7.28 too many messages from your IP, slow down")
	stop := e.startWorker()

	// First throttle, from two recipients: limit 4 -> 2, backoff 1 minute.
	id := e.send("a@throttle.test", "b@throttle.test")
	e.waitDelivery(id, "a@throttle.test", 1, "deferred")
	e.waitDelivery(id, "b@throttle.test", 1, "deferred")
	rs := e.receiver("throttle.test")
	if rs.Limit != 2 || rs.Throttles != 1 || rs.SourceIP != "203.0.113.7" {
		t.Fatalf("after one throttle: %+v", rs)
	}
	near(t, "first backoff", e.backoffLeft("throttle.test"), time.Minute)
	if ev := e.event(id, "throttled"); ev["scope"] != "throttle.test" || ev["limit"] != 2.0 || ev["previous_limit"] != 4.0 ||
		ev["source_ip"] != "203.0.113.7" {
		t.Fatalf("throttled event: %v", ev)
	}
	if _, v := e.call("GET", "/emails/"+id, "", nil); v["last_event"] != "deferred" {
		t.Fatalf("last_event = %v; the delivery's own event must stay last", v["last_event"])
	}
	stop()

	// While the domain backs off, a lease skips it and serves other domains.
	e.retryNow(id)
	other := e.send("b@other.test")
	jobs, err := e.st.Lease(e.ctx, e.workerID, 10, time.Minute, 4)
	if err != nil || len(jobs) != 1 || jobs[0].Rcpt != "b@other.test" {
		t.Fatalf("lease during backoff: %v %+v", err, jobs)
	}
	reportDelivered(t, e, jobs[0])
	e.waitStatus(other, "b@other.test", "delivered")

	// The lease honours a lowered limit even within one request.
	if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO receiver_state (scope, source_ip, "limit")
		SELECT 'narrow.test', ip, 1 FROM workers WHERE id = $1`, e.workerID); err != nil {
		t.Fatal(err)
	}
	narrow := e.send("n1@narrow.test", "n2@narrow.test", "n3@narrow.test")
	jobs, _ = e.st.Lease(e.ctx, e.workerID, 10, time.Minute, 4)
	if len(jobs) != 1 || jobs[0].RcptDomain != "narrow.test" {
		t.Fatalf("limit 1 leased %d jobs: %+v", len(jobs), jobs)
	}
	if more, _ := e.st.Lease(e.ctx, e.workerID, 10, time.Minute, 4); len(more) != 0 {
		t.Fatalf("leased past the limit: %+v", more)
	}
	// This report also carries a note from an MTA-STS policy in testing mode,
	// which the attempt's event keeps.
	if _, err := e.st.ReportAttempt(e.ctx, e.workerID, protocol.Report{
		AttemptID: jobs[0].AttemptID, Outcome: protocol.OutcomeDelivered, SMTPCode: 250, Response: "250 ok",
		TLS: "opportunistic", TLSPolicy: "MTA-STS policy of narrow.test (testing): enforce mode would defer",
	}, store.ReportOptions{DomainConcurrency: 4}); err != nil { // limit 1 -> 2
		t.Fatal(err)
	}
	if ev := e.event(narrow, "delivered"); !strings.Contains(fmt.Sprint(ev["tls_policy"]), "(testing)") {
		t.Fatalf("delivered event lost the TLS policy note: %v", ev)
	}
	jobs, _ = e.st.Lease(e.ctx, e.workerID, 10, time.Minute, 4)
	if len(jobs) != 2 {
		t.Fatalf("after one delivery the limit is 2, leased %d", len(jobs))
	}
	for _, j := range jobs {
		reportDelivered(t, e, j)
	}
	if ev := e.event(narrow, "throttle_relaxed"); ev["limit"] != 2.0 || ev["previous_limit"] != 1.0 {
		t.Fatalf("throttle_relaxed event: %v", ev)
	}
	if rs := e.receiver("narrow.test"); rs.Limit != 4 {
		t.Fatalf("narrow.test limit after three deliveries: %d, want 4 (the cap)", rs.Limit)
	}

	// After the backoff, another throttle halves again and doubles the wait.
	e.endBackoff("throttle.test")
	e.startWorker()
	e.waitDelivery(id, "a@throttle.test", 2, "deferred")
	e.waitDelivery(id, "b@throttle.test", 2, "deferred")
	rs = e.receiver("throttle.test")
	if rs.Limit != 1 || rs.Throttles != 2 {
		t.Fatalf("after two throttles: %+v", rs)
	}
	near(t, "second backoff", e.backoffLeft("throttle.test"), 2*time.Minute)

	// Recovery: the receiver accepts again, and each delivery raises the
	// limit by one, up to OMW_DOMAIN_CONCURRENCY.
	e.smtp.ReplyToDomain("throttle.test", "")
	e.endBackoff("throttle.test")
	e.retryNow(id)
	e.waitStatus(id, "a@throttle.test", "delivered")
	e.waitStatus(id, "b@throttle.test", "delivered")
	rs = e.receiver("throttle.test")
	if rs.Limit != 3 || rs.Throttles != 0 {
		t.Fatalf("after recovery: %+v", rs)
	}
	if ev := e.event(id, "throttle_relaxed"); ev["limit"] != 2.0 || ev["previous_limit"] != 1.0 {
		t.Fatalf("throttle_relaxed event: %v", ev)
	}
	more := e.send("c1@throttle.test", "c2@throttle.test", "c3@throttle.test", "c4@throttle.test")
	for _, r := range []string{"c1", "c2", "c3", "c4"} {
		e.waitStatus(more, r+"@throttle.test", "delivered")
	}
	if rs := e.receiver("throttle.test"); rs.Limit != 4 {
		t.Fatalf("limit after recovery: %d, want 4", rs.Limit)
	}
	// Throttle events are timeline entries only, never webhooks.
	var hooks int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM webhook_deliveries WHERE type LIKE '%throttle%'`).Scan(&hooks)
	if hooks != 0 {
		t.Fatalf("%d throttle webhooks", hooks)
	}
}

func reportDelivered(t *testing.T, e *env, j store.LeasedJob) {
	t.Helper()
	if _, err := e.st.ReportAttempt(e.ctx, e.workerID, protocol.Report{
		AttemptID: j.AttemptID, Outcome: protocol.OutcomeDelivered, SMTPCode: 250, EnhancedCode: "2.0.0", Response: "250 2.0.0 ok",
	}, store.ReportOptions{DomainConcurrency: 4}); err != nil {
		t.Fatal(err)
	}
}

// startRelay registers a relay worker and runs it against smarthost.
func (e *env) startRelay(smarthost *smtptest.Server, user, pass string, roots *x509.CertPool) (stop func()) {
	e.t.Helper()
	token, hash := secret.NewToken("omwk_")
	if _, err := e.st.CreateWorker(e.ctx, "relay1", hash); err != nil {
		e.t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(smarthost.Addr)
	port, _ := strconv.Atoi(portStr)
	r := worker.NewRunner(config.Worker{
		ControlURL: e.ts.URL, Token: token, EHLO: "relay1.openmailwire.test", PublicIP: "203.0.113.9",
		StateDir: e.t.TempDir(), Concurrency: 4, PollInterval: 50 * time.Millisecond, JobTimeout: 30 * time.Second,
		RelayHost: host, RelayPort: port, RelayUsername: user, RelayPassword: pass, RelayTLS: "starttls", TLSRoots: roots,
	}, e.log, "test")
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	e.t.Cleanup(stop)

	// Wait for its first heartbeat: from then on it counts as live.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var kind string
		var seen *time.Time
		e.st.Pool.QueryRow(e.ctx, `SELECT kind, last_seen_at FROM workers WHERE name = 'relay1'`).Scan(&kind, &seen)
		if kind == protocol.KindRelay && seen != nil {
			return stop
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("relay worker never announced itself (kind %q)", kind)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func attemptWorkers(v map[string]any) []string {
	var out []string
	for _, a := range v["attempts"].([]any) {
		out = append(out, a.(map[string]any)["worker"].(string))
	}
	return out
}

// TestRelayFallback: deliveries that direct workers could not finish go to
// a relay worker, which authenticates to its smarthost over TLS and passes
// the message on unchanged.
func TestRelayFallback(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	_, dkimTXT := e.addVerifiedDomain("acme.test")
	e.smtp.ReplyToDomain("slow.test", "450 4.2.0 mailbox busy, try again later") // not a throttle
	e.smtp.ReplyToDomain("busy.test", "421 4.7.0 too many connections, slow down")

	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	smarthost, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(smarthost.Close)
	smarthost.EnableSTARTTLS(smtptest.TLSConfig(cert))
	smarthost.RequireTLS()
	smarthost.RequireAuth("relay-user", "relay-pass")

	stopRelay := e.startRelay(smarthost, "relay-user", "relay-pass", ca.Pool)
	stopDirect := e.startWorker()

	// Two failed direct attempts, then the relay.
	id := e.send("x@slow.test")
	e.waitDelivery(id, "x@slow.test", 1, "deferred")
	e.retryNow(id)
	e.waitDelivery(id, "x@slow.test", 2, "deferred")
	e.retryNow(id)
	v := e.waitStatus(id, "x@slow.test", "delivered")
	if w := attemptWorkers(v); len(w) != 3 || w[0] != "w1" || w[1] != "w1" || w[2] != "relay1" {
		t.Fatalf("attempt workers %v, want w1, w1, relay1", w)
	}
	last := v["attempts"].([]any)[2].(map[string]any)
	if last["tls"] != "verified" || last["source_ip"] != "203.0.113.9" || last["mx_host"] != "127.0.0.1" {
		t.Fatalf("relay attempt: %v", last)
	}
	msgs := smarthost.Messages()
	if len(msgs) != 1 || !msgs[0].TLS || msgs[0].AuthUser != "relay-user" || len(msgs[0].Rcpts) != 1 || msgs[0].Rcpts[0] != "x@slow.test" {
		t.Fatalf("smarthost got %+v", msgs)
	}
	if !strings.HasPrefix(msgs[0].MailFrom, "b-") || !strings.HasSuffix(msgs[0].MailFrom, "@bounce.acme.test") {
		t.Fatalf("envelope sender through the relay: %q", msgs[0].MailFrom)
	}
	verifyDKIM(t, msgs[0].Data, dkimTXT)
	if n := e.smtp.RcptCount("slow.test"); n != 2 {
		t.Fatalf("the direct receiver saw %d attempts, want 2", n)
	}

	// A throttled domain goes to the relay at once, without waiting for two
	// attempts; the relay's deliveries do not raise the direct limit.
	id2 := e.send("y1@busy.test", "y2@busy.test")
	e.waitDelivery(id2, "y1@busy.test", 1, "deferred")
	e.waitDelivery(id2, "y2@busy.test", 1, "deferred")
	if rs := e.receiver("busy.test"); rs.Limit != 2 || rs.BackoffUntil == nil {
		t.Fatalf("busy.test after a throttle: %+v", rs)
	}
	e.retryNow(id2)
	for _, rcpt := range []string{"y1@busy.test", "y2@busy.test"} {
		e.waitStatus(id2, rcpt, "delivered")
	}
	_, v = e.call("GET", "/emails/"+id2, "", nil)
	if w := attemptWorkers(v); len(w) != 4 || w[2] != "relay1" || w[3] != "relay1" {
		t.Fatalf("attempt workers %v, want w1, w1, relay1, relay1", w)
	}
	if rs := e.receiver("busy.test"); rs.Limit != 2 {
		t.Fatalf("a relay delivery changed the direct limit: %+v", rs)
	}
	if n := e.smtp.RcptCount("busy.test"); n != 2 {
		t.Fatalf("the throttling receiver saw %d attempts, want only the two first ones", n)
	}
	verifyDKIM(t, smarthost.Messages()[1].Data, dkimTXT)

	// Without a live relay worker, direct workers take every delivery.
	stopRelay()
	stopDirect()
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE workers SET last_seen_at = now() - interval '3 minutes' WHERE name = 'relay1'`); err != nil {
		t.Fatal(err)
	}
	e.smtp.ReplyToDomain("slow.test", "")
	id3 := e.send("z@slow.test")
	// As if two direct attempts had already failed.
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE deliveries SET attempts = 2 WHERE email_id = $1`, id3); err != nil {
		t.Fatal(err)
	}
	e.startWorker()
	v = e.waitStatus(id3, "z@slow.test", "delivered")
	if w := attemptWorkers(v); len(w) != 1 || w[0] != "w1" {
		t.Fatalf("attempt workers %v, want w1", w)
	}

	// The worker API refuses an unknown kind.
	req, _ := http.NewRequest("POST", e.ts.URL+"/internal/worker/heartbeat", bytes.NewReader([]byte(`{"kind":"carrier-pigeon"}`)))
	req.Header.Set("Authorization", "Bearer "+e.workerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 422 {
		t.Fatalf("unknown kind: %v %v", err, resp.StatusCode)
	}
	resp.Body.Close()
}
