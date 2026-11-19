package e2e

// End-to-end tests of the v0.6 deliverability features: worker token
// roles, version skew on the worker API, throttling per mailbox provider
// and sending IP, IP warm-up with relay overflow, greylisting, DKIM
// rotation and Ed25519, and the worker self-check in the heartbeat.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtptest"
	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/worker"
)

// postWorker calls the worker API with a token and returns the status and
// the error name, if any.
func (e *env) postWorker(token, path, body string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Name
}

// addWorker registers a worker with a role and returns its id and token.
func (e *env) addWorker(name, role string) (id, token string) {
	e.t.Helper()
	token, hash := secret.NewToken("omwk_")
	id, err := e.st.CreateWorkerWithRole(e.ctx, name, role, hash)
	if err != nil {
		e.t.Fatal(err)
	}
	return id, token
}

// beat records a heartbeat for a worker, as if it ran with this IP.
func (e *env) beat(workerID, ip, kind string) {
	e.t.Helper()
	if _, err := e.st.Heartbeat(e.ctx, workerID, protocol.HeartbeatRequest{PublicIP: ip, EHLO: "w.test", Capacity: 10, Kind: kind}); err != nil {
		e.t.Fatal(err)
	}
}

// TestWorkerTokenRoles: inbound endpoints refuse delivery and relay
// tokens, the delivery endpoints refuse inbound tokens, and a delivery
// token cannot lease as a relay (or the reverse). Tokens from before roles
// ("any") keep working everywhere.
func TestWorkerTokenRoles(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	_, delivery := e.addWorker("d1", protocol.RoleDelivery)
	_, inbound := e.addWorker("mx1", protocol.RoleInbound)
	_, relay := e.addWorker("r1", protocol.RoleRelay)
	check := `{"rcpt":"nobody@example.test"}`
	cases := []struct {
		token, path, body string
		status            int
	}{
		{delivery, "/internal/inbound/check", check, 403},
		{relay, "/internal/inbound/check", check, 403},
		{inbound, "/internal/inbound/check", check, 200},
		{e.workerToken, "/internal/inbound/check", check, 200},
		{inbound, "/internal/worker/lease", `{"max":1}`, 403},
		{inbound, "/internal/worker/heartbeat", `{}`, 403},
		{delivery, "/internal/worker/lease", `{"max":1}`, 200},
		{delivery, "/internal/worker/lease", `{"max":1,"kind":"relay"}`, 403},
		{relay, "/internal/worker/lease", `{"max":1}`, 403},
		{relay, "/internal/worker/lease", `{"max":1,"kind":"relay"}`, 200},
		{relay, "/internal/worker/heartbeat", `{"kind":"relay"}`, 200},
		{e.workerToken, "/internal/worker/lease", `{"max":1,"kind":"relay"}`, 200},
	}
	for _, c := range cases {
		code, name := e.postWorker(c.token, c.path, c.body)
		if code != c.status || (code == 403 && name != "wrong_worker_role") {
			who := map[string]string{delivery: "delivery", inbound: "inbound", relay: "relay", e.workerToken: "any"}[c.token]
			t.Errorf("%s token, %s %s: %d %s, want %d", who, c.path, c.body, code, name, c.status)
		}
	}
}

// TestWorkerAPIToleratesUnknownFields (R3, unproven): a worker newer than
// the control plane sends fields the control plane does not know; they are
// ignored instead of refused, so reports are never lost to version skew.
func TestWorkerAPIToleratesUnknownFields(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	if code, name := e.postWorker(e.workerToken, "/internal/worker/heartbeat",
		`{"public_ip":"203.0.113.7","ehlo":"w.test","capacity":1,"version":"0.9","from_the_future":{"x":1}}`); code != 200 {
		t.Fatalf("heartbeat: %d %s", code, name)
	}
	e.queueRaw(e.projectID, "r@example.test")
	jobs, err := e.st.LeaseJobs(e.ctx, store.LeaseOptions{WorkerID: e.workerID, Max: 1, Lease: time.Minute})
	if err != nil || len(jobs) != 1 {
		t.Fatal(err, len(jobs))
	}
	body := `{"attempt_id":"` + jobs[0].AttemptID + `","outcome":"delivered","smtp_code":250,"response":"250 ok","stage":"message","new_field":true}`
	if code, name := e.postWorker(e.workerToken, "/internal/worker/report", body); code != 200 {
		t.Fatalf("report with an unknown field: %d %s", code, name)
	}
	// An attempt of another worker is a conflict (dropped by the worker),
	// not a 403 (kept for the operator).
	_, other := e.addWorker("w2", protocol.RoleDelivery)
	if code, name := e.postWorker(other, "/internal/worker/report", body); code != 409 || name != "attempt_of_another_worker" {
		t.Fatalf("another worker's attempt: %d %s", code, name)
	}
}

// TestProviderThrottlingPerIP: recipient domains whose MX belongs to one
// provider share its limits; a throttle slows that provider down for the
// sending IP that was throttled, not for other IPs.
func TestProviderThrottlingPerIP(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	w2, _ := e.addWorker("w2", protocol.RoleDelivery)
	e.beat(e.workerID, "203.0.113.7", "direct")
	e.beat(w2, "198.51.100.9", "direct")
	o := store.LeaseOptions{WorkerID: e.workerID, Max: 10, Lease: time.Minute, DomainConcurrency: 10}

	// Two Google-hosted domains defer at MAIL FROM with a rate code.
	for _, r := range []string{"a@one.test", "b@two.test"} {
		e.queueRaw(e.projectID, r)
	}
	jobs, err := e.st.LeaseJobs(e.ctx, o)
	if err != nil || len(jobs) != 2 {
		t.Fatal(err, len(jobs))
	}
	var throttledEmail string
	for _, j := range jobs {
		if _, err := e.st.ReportAttempt(e.ctx, e.workerID, protocol.Report{
			AttemptID: j.AttemptID, Outcome: protocol.OutcomeDeferred, SMTPCode: 450, EnhancedCode: "4.7.28",
			Response: "450 4.7.28 unusual rate of unsolicited mail from your IP", MXHost: "aspmx.l.google.com", Stage: protocol.StageMail,
		}, store.ReportOptions{DomainConcurrency: 10}); err != nil {
			t.Fatal(err)
		}
		var emailID string
		e.st.Pool.QueryRow(e.ctx, `SELECT email_id::text FROM deliveries WHERE id = $1`, j.DeliveryID).Scan(&emailID)
		throttledEmail = emailID
	}
	rs := e.receiver("google")
	if rs.SourceIP != "203.0.113.7" || rs.BackoffUntil == nil || rs.Limit != 10 { // google's per-IP cap is 20, halved to 10
		t.Fatalf("google for 203.0.113.7: %+v", rs)
	}
	if ev := e.event(throttledEmail, "throttled"); ev["scope"] != "google" || ev["source_ip"] != "203.0.113.7" {
		t.Fatalf("throttled event: %v", ev)
	}

	// New mail for both Google domains and another one.
	for _, r := range []string{"c@one.test", "d@two.test", "f@other.test"} {
		e.queueRaw(e.projectID, r)
	}
	jobs, err = e.st.LeaseJobs(e.ctx, o)
	if err != nil || len(jobs) != 1 || jobs[0].Rcpt != "f@other.test" {
		t.Fatalf("the throttled IP leased %v %+v; want only other.test", err, jobs)
	}
	o2 := o
	o2.WorkerID = w2
	jobs, err = e.st.LeaseJobs(e.ctx, o2)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("another IP leased %v %+v; want both Google domains", err, jobs)
	}
}

// TestWarmupAndRelayOverflow: a new sending IP gets at most the day's
// warm-up cap per provider; the rest waits, or goes to a live relay.
func TestWarmupAndRelayOverflow(t *testing.T) {
	rules, err := policy.ParseRules([]byte(`{"providers": [{"name": "google", "mx_suffixes": ["google.com"], "warmup": [2, 4]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := newDeliveryEnv(t, func(c *config.Control) { c.Warmup, c.ProviderRules = true, rules })
	e.st.Pool.Exec(e.ctx, `INSERT INTO rcpt_domain_providers (rcpt_domain, provider) VALUES ('one.test', 'google')`)
	e.beat(e.workerID, "192.0.2.10", "direct")
	for i := 0; i < 3; i++ {
		e.queueRaw(e.projectID, "u"+string(rune('a'+i))+"@one.test")
	}
	e.queueRaw(e.projectID, "x@other.test")
	// Sealed for real: the lease handler opens them.
	e.st.Pool.Exec(e.ctx, `UPDATE emails SET raw = $1`, e.sealer.Seal([]byte("Subject: s\r\n\r\nx\r\n")))

	// Through the API, which passes OMW_WARMUP and the rules on.
	c := &worker.Client{Base: e.ts.URL, Token: e.workerToken, HTTP: http.DefaultClient}
	var resp protocol.LeaseResponse
	if err := c.Post(e.ctx, "/internal/worker/lease", protocol.LeaseRequest{Max: 10}, &resp); err != nil {
		t.Fatal(err)
	}
	google := 0
	for _, j := range resp.Jobs {
		if j.RcptDomain == "one.test" {
			google++
		}
	}
	if google != 2 || len(resp.Jobs) != 3 {
		t.Fatalf("day 1 leased %d jobs, %d for google; want 3 and 2", len(resp.Jobs), google)
	}
	st, err := e.st.IPWarmup(e.ctx, rules, "192.0.2.10")
	if err != nil || st.Warm || st.Day != 1 || len(st.Left) != 1 || st.Left[0].Cap != 2 || st.Left[0].Used != 2 {
		t.Fatalf("warm-up status: %+v %v", st, err)
	}
	o := store.LeaseOptions{WorkerID: e.workerID, Max: 10, Lease: time.Minute, Rules: rules, Warmup: true}
	if jobs, _ := e.st.LeaseJobs(e.ctx, o); len(jobs) != 0 {
		t.Fatalf("leased past the warm-up cap: %+v", jobs)
	}

	// A live relay takes the overflow.
	relayID, _ := e.addWorker("relay1", protocol.RoleRelay)
	e.beat(relayID, "", "relay")
	ro := o
	ro.WorkerID, ro.Kind = relayID, protocol.KindRelay
	if jobs, err := e.st.LeaseJobs(e.ctx, ro); err != nil || len(jobs) != 1 || jobs[0].RcptDomain != "one.test" {
		t.Fatalf("relay overflow: %v %+v", err, jobs)
	}

	// Marked warm, the IP has no cap any more.
	e.queueRaw(e.projectID, "ud@one.test")
	if err := e.st.SetIPWarm(e.ctx, "192.0.2.10", true); err != nil {
		t.Fatal(err)
	}
	if jobs, _ := e.st.LeaseJobs(e.ctx, o); len(jobs) != 1 {
		t.Fatalf("a warm IP is still capped: %+v", jobs)
	}
}

// TestGreylisting: a greylisted attempt is retried 5 to 15 minutes later
// and does not count toward the hand-over to a relay.
func TestGreylisting(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.ReplyToDomain("grey.test", "451 4.7.1 Greylisted, please try again later")
	relayID, _ := e.addWorker("relay1", protocol.RoleRelay)
	e.beat(relayID, "", "relay")
	e.beat(e.workerID, "203.0.113.7", "direct")
	id := e.queueRaw(e.projectID, "g@grey.test")
	o := store.LeaseOptions{WorkerID: e.workerID, Max: 10, Lease: time.Minute, RelayAfterAttempts: 2}
	for i := 0; i < 2; i++ {
		e.retryNow(id)
		e.deliverAndReport(srv, o)
		var greylists int
		var wait time.Duration
		e.st.Pool.QueryRow(e.ctx, `SELECT greylists, next_attempt_at - now() FROM deliveries WHERE email_id = $1`, id).Scan(&greylists, &wait)
		if greylists != i+1 || wait < 4*time.Minute || wait > 15*time.Minute {
			t.Fatalf("attempt %d: %d greylists, next attempt in %v", i+1, greylists, wait)
		}
	}
	if ev := e.event(id, "deferred"); ev["greylisted"] != true {
		t.Fatalf("deferred event: %v", ev)
	}
	e.retryNow(id)
	ro := o
	ro.WorkerID, ro.Kind = relayID, protocol.KindRelay
	if jobs, _ := e.st.LeaseJobs(e.ctx, ro); len(jobs) != 0 {
		t.Fatalf("greylisting sent the delivery to the relay: %+v", jobs)
	}
	if jobs, _ := e.st.LeaseJobs(e.ctx, o); len(jobs) != 1 {
		t.Fatalf("direct workers must keep a greylisted delivery: %+v", jobs)
	}
}

// verifyAll checks every DKIM signature of msg against records (TXT
// name -> value) and returns the selectors that verified.
func verifyAll(t *testing.T, msg []byte, records map[string]string) []string {
	t.Helper()
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(msg), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			if v, ok := records[name]; ok {
				return []string{v}, nil
			}
			return nil, errors.New("not found")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ok []string
	for _, v := range vs {
		if v.Err != nil {
			t.Fatalf("signature of %s: %v", v.Domain, v.Err)
		}
		ok = append(ok, v.Domain)
	}
	return ok
}

func recordsOf(v map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range v["records"].([]any) {
		rec := r.(map[string]any)
		out[rec["record"].(string)] = rec
	}
	return out
}

// TestDKIMRotationAndEd25519: rotation with two selectors and an overlap,
// and an Ed25519 signature next to RSA, each in two steps (create, then
// switch once the record is published).
func TestDKIMRotationAndEd25519(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	id, oldTXT := e.addVerifiedDomain("acme.test")
	e.startWorker()
	_, before := e.call("GET", "/domains/"+id, "", nil)
	oldDKIM := recordsOf(before)["DKIM"]

	code, v := e.call("POST", "/domains/"+id+"/dkim/rotate", "", map[string]any{})
	if code != 202 || v["status"] != "pending" {
		t.Fatalf("start rotation: %d %v", code, v)
	}
	next := v["record"].(map[string]any)
	if next["record"] != "DKIM_NEXT" || next["name"] == oldDKIM["name"] {
		t.Fatalf("new record: %v", next)
	}
	// Mail is still signed with the current key.
	sent := e.send("x@example.test")
	e.waitStatus(sent, "x@example.test", "delivered")
	verifyDKIM(t, e.smtp.Messages()[0].Data, oldTXT)

	code, v = e.call("POST", "/domains/"+id+"/dkim/rotate", "", map[string]any{})
	if code != 409 || v["name"] != "dkim_record_not_found" || !strings.Contains(v["message"].(string), next["name"].(string)) {
		t.Fatalf("switch before publishing: %d %v", code, v)
	}
	e.dns.txt[next["name"].(string)] = []string{next["value"].(string)}
	code, v = e.call("POST", "/domains/"+id+"/dkim/rotate", "", map[string]any{})
	if code != 200 || v["status"] != "rotated" {
		t.Fatalf("switch: %d %v", code, v)
	}
	_, after := e.call("GET", "/domains/"+id, "", nil)
	recs := recordsOf(after)
	if recs["DKIM"]["name"] != next["name"] || recs["DKIM_PREVIOUS"]["name"] != oldDKIM["name"] ||
		!strings.Contains(recs["DKIM_PREVIOUS"]["detail"].(string), "keep this record until") {
		t.Fatalf("records after the rotation: %v", after["records"])
	}
	sent = e.send("y@example.test")
	e.waitStatus(sent, "y@example.test", "delivered")
	if got := verifyAll(t, e.smtp.Messages()[1].Data, map[string]string{next["name"].(string): next["value"].(string)}); len(got) != 1 {
		t.Fatalf("signatures with the new key: %v", got)
	}

	// Ed25519.
	sending := e.sendingKey()
	if code, _ := sending.call("POST", "/domains/"+id+"/dkim/ed25519", "", map[string]any{"enabled": true}); code != 403 {
		t.Fatalf("a sending key turned Ed25519 on: %d", code)
	}
	code, v = e.call("POST", "/domains/"+id+"/dkim/ed25519", "", map[string]any{"enabled": true})
	if code != 202 || v["status"] != "pending" {
		t.Fatalf("enable Ed25519: %d %v", code, v)
	}
	var ed map[string]any
	for _, k := range v["keys"].([]any) {
		if k := k.(map[string]any); k["algorithm"] == "ed25519" {
			ed = k["record"].(map[string]any)
		}
	}
	if ed == nil || !strings.Contains(ed["value"].(string), "k=ed25519") {
		t.Fatalf("Ed25519 record: %v", v["keys"])
	}
	if code, v = e.call("POST", "/domains/"+id+"/dkim/ed25519", "", map[string]any{"enabled": true}); code != 409 {
		t.Fatalf("activate before publishing: %d %v", code, v)
	}
	e.dns.txt[ed["name"].(string)] = []string{ed["value"].(string)}
	if code, v = e.call("POST", "/domains/"+id+"/dkim/ed25519", "", map[string]any{"enabled": true}); code != 200 || v["status"] != "active" {
		t.Fatalf("activate: %d %v", code, v)
	}
	sent = e.send("z@example.test")
	e.waitStatus(sent, "z@example.test", "delivered")
	got := verifyAll(t, e.smtp.Messages()[2].Data, map[string]string{
		next["name"].(string): next["value"].(string), ed["name"].(string): ed["value"].(string)})
	if len(got) != 2 {
		t.Fatalf("want an RSA and an Ed25519 signature, got %v", got)
	}
	if code, v = e.call("GET", "/domains/"+id+"/dkim", "", nil); code != 200 || len(v["keys"].([]any)) != 3 {
		t.Fatalf("GET dkim: %d %v", code, v)
	}
	if code, v = e.call("POST", "/domains/"+id+"/dkim/ed25519", "", map[string]any{"enabled": false}); code != 200 || v["status"] != "disabled" {
		t.Fatalf("disable: %d %v", code, v)
	}
}

// sendingKey returns a harness with a sending_access key of the project.
func (e *env) sendingKey() *harness {
	e.t.Helper()
	code, v := e.call("POST", "/api-keys", "", map[string]any{"name": "agent", "permission": "sending_access"})
	if code != 201 {
		e.t.Fatalf("create sending key: %d %v", code, v)
	}
	return &harness{t: e.t, st: e.st, url: e.ts.URL, apiKey: v["token"].(string)}
}

// TestSelfCheckReportedInHeartbeat: a worker's self-check result reaches
// the control plane with its heartbeat (here skipped: the worker sends to
// an SMTP override).
func TestSelfCheckReportedInHeartbeat(t *testing.T) {
	e := newDeliveryEnv(t, nil)
	e.startWorker()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var raw []byte
		e.st.Pool.QueryRow(e.ctx, `SELECT self_check FROM workers WHERE id = $1`, e.workerID).Scan(&raw)
		var c protocol.SelfCheck
		if json.Unmarshal(raw, &c) == nil && c.Skipped != "" {
			if !strings.Contains(c.Skipped, "OMW_SMTP_OVERRIDE") {
				t.Fatalf("self-check: %+v", c)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no self-check in the heartbeat: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
