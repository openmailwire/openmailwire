package e2e

// v0.6 lane E: webhook fan-out, fairness, circuit breaker, generic errors,
// secret rotation and Ed25519 signatures. The fan-out and fairness tests
// are the v0.5 audit proofs (R1#1, R4#1, R4#4, R4#8) turned into
// regression tests.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/webhook"
)

// whDrain fans out every committed event.
func (e *env) whDrain() {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := e.st.FanoutWebhooks(e.ctx, 500, webhook.Build)
		if err != nil {
			e.t.Fatal(err)
		}
		if n > 0 {
			continue
		}
		// SKIP LOCKED passes over rows that a cancelled fan-out still
		// holds until Postgres has torn its connection down. Wait for
		// those too: an event left unfanned past the deadline still
		// counts as lost.
		var left int
		e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE NOT fanned_out`).Scan(&left)
		if left == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func whJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// whLost counts "queued" events with no email.sent delivery.
func (e *env) whLost() (events, lost int) {
	e.t.Helper()
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE type = 'queued'`).Scan(&events)
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM events ev WHERE ev.type = 'queued'
		AND NOT EXISTS (SELECT 1 FROM webhook_deliveries wd WHERE wd.event_id = ev.id)`).Scan(&lost)
	return
}

func (e *env) whBatch(items []map[string]any) {
	req, _ := http.NewRequest("POST", e.url+"/emails/batch", strings.NewReader(whJSON(items)))
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Error(err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		e.t.Errorf("batch: %d", resp.StatusCode)
	}
}

// R1#1, R4#1 (deterministic): an event whose transaction commits after a
// later event was fanned out used to be skipped forever by the id cursor.
func TestWebhookFanoutLateCommit(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	srv := httptest.NewServer(&receiver{})
	defer srv.Close()
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": srv.URL}); code != 201 {
		t.Fatalf("create webhook: %d %v", code, b)
	}
	id1 := e.send("a@example.test")
	id2 := e.send("b@example.test")
	e.whDrain()

	// Transaction A (a batch, a worker report) has inserted its event but
	// not committed.
	txA, err := e.st.Pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(e.ctx)
	var evA, evB int64
	if err := txA.QueryRow(e.ctx, `INSERT INTO events (email_id, delivery_id, type)
		SELECT email_id, id, 'delivered' FROM deliveries WHERE email_id = $1 RETURNING id`, id1).Scan(&evA); err != nil {
		t.Fatal(err)
	}
	// Transaction B takes the next id and commits first.
	if err := e.st.Pool.QueryRow(e.ctx, `INSERT INTO events (email_id, delivery_id, type)
		SELECT email_id, id, 'delivered' FROM deliveries WHERE email_id = $1 RETURNING id`, id2).Scan(&evB); err != nil {
		t.Fatal(err)
	}
	e.whDrain() // the dispatcher ticks here
	if err := txA.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.whDrain() // and again later

	count := func(ev int64) (n int) {
		e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id = $1`, ev).Scan(&n)
		return
	}
	if evA >= evB {
		t.Fatalf("setup: event A (%d) should have the lower id than B (%d)", evA, evB)
	}
	if count(evA) != 1 || count(evB) != 1 {
		t.Fatalf("event A (committed last) has %d deliveries, event B %d; want 1 each", count(evA), count(evB))
	}
	var left int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE NOT fanned_out`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d events still wait for fan-out", left)
	}
}

// R4#1 (under load): concurrent batch sends while fan-out runs. v0.5 lost
// 900–1,973 of 2,400 email.sent events here.
func TestWebhookFanoutUnderLoad(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	srv := httptest.NewServer(&receiver{})
	defer srv.Close()
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": srv.URL, "events": []string{"email.sent"}}); code != 201 {
		t.Fatalf("create webhook: %d %v", code, b)
	}
	e.whDrain()

	ctx, cancel := context.WithCancel(e.ctx)
	var fanDone sync.WaitGroup
	// Two fan-out loops, as with two control planes.
	for i := 0; i < 2; i++ {
		fanDone.Add(1)
		go func() {
			defer fanDone.Done()
			for ctx.Err() == nil {
				e.st.FanoutWebhooks(ctx, 500, webhook.Build)
				time.Sleep(time.Millisecond)
			}
		}()
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				var batch []map[string]any
				for j := 0; j < 10; j++ {
					var to []string
					for k := 0; k < 5; k++ {
						to = append(to, fmt.Sprintf("r%d-%d-%d-%d@example.test", g, i, j, k))
					}
					batch = append(batch, map[string]any{"from": "hello@acme.test", "to": to, "subject": "s", "text": "t"})
				}
				e.whBatch(batch)
			}
		}(g)
	}
	wg.Wait()
	cancel()
	fanDone.Wait()
	e.whDrain()

	events, lost := e.whLost()
	if events != 8*6*10*5 {
		t.Fatalf("setup: %d queued events", events)
	}
	if lost > 0 {
		t.Fatalf("%d of %d email.sent webhooks were lost", lost, events)
	}
	var dup int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) - count(DISTINCT event_id) FROM webhook_deliveries`).Scan(&dup)
	if dup != 0 {
		t.Fatalf("%d events were fanned out twice", dup)
	}
}

// R4#1 with the real dispatcher at its production tick: every email.sent
// webhook is fanned out and arrives. v0.5 lost 84–173 of 7,200.
func TestWebhookFanoutRealDispatcher(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": srv.URL, "events": []string{"email.sent"}}); code != 201 {
		t.Fatalf("create webhook: %d %v", code, b)
	}
	d := &webhook.Dispatcher{Store: e.st, Sealer: e.sealer, HTTP: webhook.NewHTTPClient(true), Log: e.log, UserAgent: "t"}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go d.Run(ctx, time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				time.Sleep(20 * time.Millisecond)
				var batch []map[string]any
				for j := 0; j < 3; j++ {
					var to []string
					for k := 0; k < 5; k++ {
						to = append(to, fmt.Sprintf("r%d-%d-%d-%d@example.test", g, i, j, k))
					}
					batch = append(batch, map[string]any{"from": "hello@acme.test", "to": to, "subject": "s", "text": "t"})
				}
				e.whBatch(batch)
			}
		}(g)
	}
	wg.Wait()
	const want = 6 * 20 * 3 * 5
	deadline := time.Now().Add(30 * time.Second)
	for rc.count() < want && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	events, lost := e.whLost()
	if lost > 0 || rc.count() != want || events != want {
		t.Fatalf("queued events %d, never fanned out %d, webhooks received %d (want %d)", events, lost, rc.count(), want)
	}
}

// R4#4: one project's slow endpoints held up every other project's webhooks
// (75 s in the audit: 50 claimed, 10 at a time, 15 s each, whole-batch
// waits). Now a project gets a bounded share and nothing waits on a batch.
func TestWebhookFairnessAcrossProjects(t *testing.T) {
	e := newEnv(t, func(c *config.Control) { c.WebhooksPerProject = 50 }) // the audit's 50 webhooks, cap aside
	e.addVerifiedDomain("acme.test")

	release := make(chan struct{})
	var tarpitHits atomic.Int64
	tarpit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tarpitHits.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer tarpit.Close()
	defer close(release)
	for i := 0; i < 50; i++ {
		if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": fmt.Sprintf("%s/%d", tarpit.URL, i), "events": []string{"email.sent"}}); code != 201 {
			t.Fatalf("create webhook %d: %d %v", i, code, b)
		}
	}
	e.send("a@example.test")
	e.send("b@example.test")
	e.whDrain() // 100 deliveries to a tarpit

	B := e.otherProject()
	_, dom := B.call("POST", "/domains", "", map[string]string{"name": "beta.test"})
	for _, r := range dom["records"].([]any) {
		rec := r.(map[string]any)
		if rec["type"] == "TXT" {
			e.dns.txt[rec["name"].(string)] = append(e.dns.txt[rec["name"].(string)], rec["value"].(string))
		}
	}
	if code, v := B.call("POST", "/domains/"+dom["id"].(string)+"/verify", "", nil); code != 200 {
		t.Fatalf("B verify: %d %v", code, v)
	}
	var gotAt atomic.Int64
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAt.CompareAndSwap(0, time.Now().UnixNano())
	}))
	defer fast.Close()
	if code, b := B.call("POST", "/webhooks", "", map[string]any{"endpoint": fast.URL, "events": []string{"email.sent"}}); code != 201 {
		t.Fatalf("B webhook: %d %v", code, b)
	}
	if code, b := B.call("POST", "/emails", "", map[string]any{"from": "hi@beta.test", "to": []string{"x@example.test"}, "subject": "s", "text": "t"}); code != 200 {
		t.Fatalf("B send: %d %v", code, b)
	}
	e.whDrain()

	start := time.Now()
	d := &webhook.Dispatcher{Store: e.st, Sealer: e.sealer, HTTP: webhook.NewHTTPClient(true), Log: e.log, UserAgent: "t"}
	ctx, cancel := context.WithCancel(e.ctx)
	runDone := make(chan struct{})
	go func() { d.Run(ctx, 50*time.Millisecond); close(runDone) }()
	defer func() { cancel(); <-runDone }()
	for gotAt.Load() == 0 && time.Since(start) < 20*time.Second {
		time.Sleep(20 * time.Millisecond)
	}
	if gotAt.Load() == 0 {
		t.Fatal("project B's webhook never arrived")
	}
	lat := time.Unix(0, gotAt.Load()).Sub(start)
	if lat > 3*time.Second {
		t.Fatalf("project B waited %v behind project A's slow endpoints", lat)
	}
	// Project A never holds more than its share of the pool.
	time.Sleep(200 * time.Millisecond)
	if n := tarpitHits.Load(); n > webhook.DefaultPerProject {
		t.Fatalf("project A has %d requests in flight, more than its share of %d", n, webhook.DefaultPerProject)
	}
}

// R4#4: webhooks per project are capped (default 10), also under
// concurrent creation.
func TestWebhookCapPerProject(t *testing.T) {
	e := newEnv(t, nil)
	var wg sync.WaitGroup
	var created, refused atomic.Int64
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": fmt.Sprintf("https://hooks.example.com/%d", i)})
			switch {
			case code == 201:
				created.Add(1)
			case code == 409 && b["name"] == "webhook_limit_reached" && strings.Contains(b["message"].(string), "DELETE /webhooks/{id}"):
				refused.Add(1)
			default:
				t.Errorf("create %d: %d %v", i, code, b)
			}
		}(i)
	}
	wg.Wait()
	if created.Load() != 10 || refused.Load() != 6 {
		t.Fatalf("created %d, refused %d; want 10 and 6", created.Load(), refused.Load())
	}
}

// R4#4: the circuit breaker. After BreakerThreshold consecutive failures the
// endpoint gets nothing until the pause ends, then one probe at a time.
func TestWebhookCircuitBreaker(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	var hits atomic.Int64
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	code, hook := e.call("POST", "/webhooks", "", map[string]any{"endpoint": srv.URL, "events": []string{"email.sent"}})
	if code != 201 {
		t.Fatalf("create webhook: %d %v", code, hook)
	}
	hookID := hook["id"].(string)
	for i := 0; i < 6; i++ {
		e.send(fmt.Sprintf("r%d@example.test", i))
	}
	d := &webhook.Dispatcher{Store: e.st, Sealer: e.sealer, HTTP: webhook.NewHTTPClient(true), Log: e.log, UserAgent: "t",
		PerWebhook: 2, BreakerThreshold: 3, BreakerBase: time.Hour}
	for i := 0; i < 5; i++ {
		d.Step(e.ctx)
	}
	// Two steps of two requests each: the third failure opens the breaker,
	// the fourth was already in flight.
	if n := hits.Load(); n != 4 {
		t.Fatalf("endpoint got %d requests; want 4 before the breaker opened", n)
	}
	_, v := e.call("GET", "/webhooks/"+hookID, "", nil)
	if v["paused_until"] == nil || v["consecutive_failures"].(float64) != 4 {
		t.Fatalf("webhook view: paused_until %v, consecutive_failures %v", v["paused_until"], v["consecutive_failures"])
	}
	// The pause ends: one probe, which succeeds and closes the breaker.
	healthy.Store(true)
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE webhooks SET breaker_until = now() - interval '1 second',
		next_due_at = now() - interval '1 second' WHERE id = $1`, hookID); err != nil {
		t.Fatal(err)
	}
	d.Step(e.ctx)
	if n := hits.Load(); n != 5 {
		t.Fatalf("half-open endpoint got %d requests; want exactly one probe (5 in all)", n)
	}
	_, v = e.call("GET", "/webhooks/"+hookID, "", nil)
	if v["paused_until"] != nil || v["consecutive_failures"].(float64) != 0 {
		t.Fatalf("after a good probe: paused_until %v, consecutive_failures %v", v["paused_until"], v["consecutive_failures"])
	}
	d.Step(e.ctx)
	if n := hits.Load(); n != 6 {
		t.Fatalf("closed breaker: endpoint got %d requests; want the last new delivery too (6)", n)
	}
}

// R4#8: last_error shown to the project said which internal address a
// host name resolved to ("dial tcp [::1]:5432: …").
func TestWebhookLastErrorIsGeneric(t *testing.T) {
	e := newEnv(t, func(c *config.Control) { c.WebhookAllowPrivate = false })
	e.addVerifiedDomain("acme.test")
	code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": "http://localhost:5432/x"})
	if code != 201 {
		t.Fatalf("create webhook: %d %v", code, b)
	}
	hookID := b["id"].(string)
	// An IP literal that is not public is refused at once.
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": "http://[64:ff9b::a9fe:a9fe]/latest"}); code != 422 {
		t.Fatalf("NAT64 metadata address accepted: %d %v", code, b)
	}
	e.send("a@example.test")
	d := &webhook.Dispatcher{Store: e.st, Sealer: e.sealer, HTTP: webhook.NewHTTPClient(false), Log: e.log, UserAgent: "t"}
	d.Step(e.ctx)
	_, v := e.call("GET", "/webhooks/"+hookID, "", nil)
	ds, _ := v["recent_deliveries"].([]any)
	if len(ds) == 0 {
		t.Fatalf("no deliveries: %v", v)
	}
	le := ds[0].(map[string]any)["last_error"].(string)
	if le == "" || strings.Contains(le, "127.0.0.1") || strings.Contains(le, "::1") || strings.Contains(le, "dial") ||
		strings.Contains(le, "5432") {
		t.Fatalf("last_error reveals internal details: %q", le)
	}
	// An unknown name reads the same: no oracle for internal DNS.
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": "http://no-such-host.invalid/x"}); code != 201 {
		t.Fatalf("create webhook: %d %v", code, b)
	} else {
		e.send("b@example.test")
		d.Step(e.ctx)
		_, v := e.call("GET", "/webhooks/"+b["id"].(string), "", nil)
		ds, _ := v["recent_deliveries"].([]any)
		if len(ds) == 0 || ds[0].(map[string]any)["last_error"] != le {
			t.Fatalf("NXDOMAIN and a refused address read differently: %v vs %q", ds, le)
		}
	}
}

// A rotation sends both signatures for the overlap; Ed25519 ("v1a")
// signatures verify with the published public keys.
func TestWebhookSecretRotationAndEd25519(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	code, hook := e.call("POST", "/webhooks", "", map[string]any{"endpoint": srv.URL, "events": []string{"email.sent"}, "asymmetric_signing": true})
	if code != 201 {
		t.Fatalf("create webhook: %d %v", code, hook)
	}
	hookID, oldSecret := hook["id"].(string), hook["signing_secret"].(string)
	oldPub, _ := hook["public_key"].(string)
	if !strings.HasPrefix(oldPub, "whpk_") || hook["asymmetric_signing"] != true {
		t.Fatalf("asymmetric webhook view: %v", hook)
	}
	_, view := e.call("GET", "/webhooks/"+hookID, "", nil)
	if view["public_key"] != oldPub || view["signing_secret"] != nil {
		t.Fatalf("GET must show the public key and never the secret: %v", view)
	}

	if code, b := e.call("POST", "/webhooks/"+hookID+"/rotate-secret", "", map[string]any{"overlap_seconds": 999999999}); code != 422 {
		t.Fatalf("overlap over 7 days accepted: %d %v", code, b)
	}
	code, rot := e.call("POST", "/webhooks/"+hookID+"/rotate-secret", "", map[string]any{"overlap_seconds": 3600})
	if code != 200 {
		t.Fatalf("rotate: %d %v", code, rot)
	}
	newSecret, newPub := rot["signing_secret"].(string), rot["public_key"].(string)
	if newSecret == oldSecret || newPub == oldPub || rot["previous_public_key"] != oldPub || rot["previous_secret_expires_at"] == nil {
		t.Fatalf("rotation answer: %v", rot)
	}

	e.send("a@example.test")
	d := &webhook.Dispatcher{Store: e.st, Sealer: e.sealer, HTTP: webhook.NewHTTPClient(true), Log: e.log, UserAgent: "t"}
	d.Step(e.ctx)
	got, ok := rc.find("email.sent", "a@example.test")
	if !ok {
		t.Fatal("no webhook")
	}
	h := got.header
	for _, s := range []string{oldSecret, newSecret} {
		if !verifySvix(s, h.Get("webhook-id"), h.Get("webhook-timestamp"), got.body, h.Get("webhook-signature")) {
			t.Fatalf("during the overlap the %s secret must verify: %s", s[:10], h.Get("webhook-signature"))
		}
	}
	for _, pk := range []string{oldPub, newPub} {
		if !whVerifyV1a(pk, h.Get("webhook-id"), h.Get("webhook-timestamp"), got.body, h.Get("webhook-signature")) {
			t.Fatalf("during the overlap the v1a signature by %s must verify", pk[:12])
		}
	}
	if h.Get("svix-signature") != h.Get("webhook-signature") {
		t.Fatal("svix-signature and webhook-signature differ")
	}

	// Without an overlap only the newest secret and key sign.
	code, rot = e.call("POST", "/webhooks/"+hookID+"/rotate-secret", "", map[string]any{"overlap_seconds": 0})
	if code != 200 || rot["previous_public_key"] != nil {
		t.Fatalf("rotate without overlap: %d %v", code, rot)
	}
	newest, newestPub := rot["signing_secret"].(string), rot["public_key"].(string)
	e.send("b@example.test")
	d.Step(e.ctx)
	got, ok = rc.find("email.sent", "b@example.test")
	if !ok {
		t.Fatal("no webhook")
	}
	h = got.header
	if n := len(strings.Fields(h.Get("webhook-signature"))); n != 2 {
		t.Fatalf("want one v1 and one v1a signature, got %q", h.Get("webhook-signature"))
	}
	if !verifySvix(newest, h.Get("webhook-id"), h.Get("webhook-timestamp"), got.body, h.Get("webhook-signature")) ||
		verifySvix(newSecret, h.Get("webhook-id"), h.Get("webhook-timestamp"), got.body, h.Get("webhook-signature")) ||
		!whVerifyV1a(newestPub, h.Get("webhook-id"), h.Get("webhook-timestamp"), got.body, h.Get("webhook-signature")) {
		t.Fatal("after an overlap of 0 only the newest secret and key may sign")
	}
	if code, b := e.call("POST", "/webhooks/00000000-0000-4000-8000-000000000000/rotate-secret", "", nil); code != 404 {
		t.Fatalf("rotating an unknown webhook: %d %v", code, b)
	}
}

func whVerifyV1a(whpk, id, ts string, body []byte, header string) bool {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(whpk, "whpk_"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	msg := []byte(id + "." + ts + "." + string(body))
	for _, part := range strings.Fields(header) {
		if v, ok := strings.CutPrefix(part, "v1a,"); ok {
			sig, err := base64.StdEncoding.DecodeString(v)
			if err == nil && ed25519.Verify(pub, msg, sig) {
				return true
			}
		}
	}
	return false
}
