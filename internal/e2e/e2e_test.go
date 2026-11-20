// Package e2e runs the control plane and a worker together against a real
// PostgreSQL and a scripted SMTP server.
//
//	OMW_TEST_DATABASE_URL=postgres://… go test ./internal/e2e/
//
// The test empties every OpenMailWire table in that database. Point it at a
// throwaway database only.
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/api"
	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/mcpserver"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtptest"
	"github.com/openmailwire/openmailwire/internal/store"
	"github.com/openmailwire/openmailwire/internal/webhook"
	"github.com/openmailwire/openmailwire/internal/worker"
)

// dns answers with whatever the test publishes.
type dns struct {
	txt map[string][]string
	mx  map[string][]*net.MX // none unless a test publishes some
}

func (d *dns) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := d.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (d *dns) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if v, ok := d.mx[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

type received struct {
	header  http.Header
	body    []byte
	payload struct {
		Type string `json:"type"`
		Data struct {
			EmailID string   `json:"email_id"`
			To      []string `json:"to"`
			Bounce  struct {
				Type string `json:"type"`
			} `json:"bounce"`
		} `json:"data"`
	}
}

// receiver records every webhook it is sent.
type receiver struct {
	mu  sync.Mutex
	got []received
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rec received
	rec.header = r.Header.Clone()
	rec.body, _ = io.ReadAll(r.Body)
	json.Unmarshal(rec.body, &rec.payload)
	rc.mu.Lock()
	rc.got = append(rc.got, rec)
	rc.mu.Unlock()
}

func (rc *receiver) find(typ, rcpt string) (received, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, r := range rc.got {
		if r.payload.Type == typ && len(r.payload.Data.To) == 1 && r.payload.Data.To[0] == rcpt {
			return r, true
		}
	}
	return received{}, false
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

func verifySvix(whsec, id, ts string, body []byte, header string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(whsec, "whsec_"))
	if err != nil || id == "" || ts == "" {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "." + string(body)))
	want := base64.StdEncoding.EncodeToString(m.Sum(nil))
	for _, part := range strings.Fields(header) {
		if v, ok := strings.CutPrefix(part, "v1,"); ok && hmac.Equal([]byte(v), []byte(want)) {
			return true
		}
	}
	return false
}

type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return http.DefaultTransport.RoundTrip(r)
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error: %s", name, textOf(res))
	}
	return res
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type harness struct {
	t      *testing.T
	st     *store.Store
	url    string
	apiKey string
}

func (h *harness) call(method, path, idem string, body any) (int, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (h *harness) waitStatus(id, rcpt, want string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		_, last = h.call("GET", "/emails/"+id, "", nil)
		recips, _ := last["recipients"].([]any)
		for _, r := range recips {
			m := r.(map[string]any)
			if m["email"] == rcpt && m["status"] == want {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(last, "", "  ")
	h.t.Fatalf("%s for %s never reached %q; last view:\n%s", id, rcpt, want, b)
	return nil
}

func TestEndToEnd(t *testing.T) {
	dbURL := os.Getenv("OMW_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("OMW_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil { // second run must be a no-op
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `TRUNCATE projects, workers CASCADE`); err != nil {
		t.Fatal(err)
	}

	pid, err := st.CreateProject(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	apiKey, keyHash := secret.NewToken("omw_")
	if err := st.CreateAPIKey(ctx, pid, "e2e", apiKey[:10], keyHash); err != nil {
		t.Fatal(err)
	}
	workerToken, workerHash := secret.NewToken("omwk_")
	workerID, err := st.CreateWorker(ctx, "w1", workerHash)
	if err != nil {
		t.Fatal(err)
	}

	key := []byte(strings.Repeat("k", 32))
	sealer, _ := secret.NewSealer(key)
	resolver := &dns{txt: map[string][]string{}}
	srv := &api.Server{
		Store: st, Sealer: sealer, MACKey: secret.Derive(key, "openmailwire/verp"), Resolver: resolver, Log: log, Version: "test",
		Cfg: config.Control{
			ReturnPathLabel: "bounce", SPFInclude: "_spf.openmailwire.test",
			DefaultExpiry: time.Hour, LeaseDuration: time.Minute, DomainConcurrency: 5, Retention: time.Hour,
			WebhookAllowHTTP: true, WebhookAllowPrivate: true,
		},
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	h := &harness{t: t, st: st, url: ts.URL, apiKey: apiKey}

	// Add a domain: it starts pending and sending from it is refused.
	code, dom := h.call("POST", "/domains", "", map[string]string{"name": "Acme.test"})
	if code != 201 || dom["status"] != "pending" || dom["name"] != "acme.test" {
		t.Fatalf("create domain: %d %v", code, dom)
	}
	domID := dom["id"].(string)
	send := map[string]any{
		"from": "Acme <login@acme.test>", "to": "ok@example.test",
		"subject": "کد ورود: 482913", "text": "Your code is 482913.", "html": "<p>Your code is <b>482913</b>.</p>",
	}
	if code, body := h.call("POST", "/emails", "", send); code != 403 || body["name"] != "domain_not_verified" {
		t.Fatalf("unverified domain sent: %d %v", code, body)
	}

	// Publish the records it asked for, then verify.
	var dkimPub string
	for _, r := range dom["records"].([]any) {
		rec := r.(map[string]any)
		resolver.txt[rec["name"].(string)] = append(resolver.txt[rec["name"].(string)], rec["value"].(string))
		if rec["record"] == "DKIM" {
			dkimPub = rec["value"].(string)
		}
	}
	code, dom = h.call("POST", "/domains/"+domID+"/verify", "", nil)
	if code != 200 || dom["status"] != "verified" {
		t.Fatalf("verify: %d %v", code, dom)
	}

	// A webhook receiver, registered before anything is sent.
	hooks := &receiver{}
	hookSrv := httptest.NewServer(hooks)
	defer hookSrv.Close()
	code, hook := h.call("POST", "/webhooks", "", map[string]any{"endpoint": hookSrv.URL + "/omw"})
	if code != 201 || !strings.HasPrefix(fmt.Sprint(hook["signing_secret"]), "whsec_") {
		t.Fatalf("create webhook: %d %v", code, hook)
	}
	whsec := hook["signing_secret"].(string)
	if code, body := h.call("POST", "/webhooks", "", map[string]any{"endpoint": "https://x.test", "events": []string{"email.nope"}}); code != 422 {
		t.Fatalf("unknown event type accepted: %d %v", code, body)
	}
	dispatcher := &webhook.Dispatcher{Store: st, Sealer: sealer, HTTP: webhook.NewHTTPClient(true), Log: log, UserAgent: "test"}
	go dispatcher.Run(ctx, 50*time.Millisecond)

	smtp, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer smtp.Close()
	smtp.ReplyToRcpt("gone@example.test", "550 5.1.1 no such user")
	smtp.DropAfterData("lost@example.test")

	// Same idempotency key and body: one message. Different body: conflict.
	code, first := h.call("POST", "/emails", "login-42", send)
	if code != 200 {
		t.Fatalf("send: %d %v", code, first)
	}
	id := first["id"].(string)
	if _, again := h.call("POST", "/emails", "login-42", send); again["id"] != id {
		t.Fatalf("idempotent replay returned %v, want %s", again["id"], id)
	}
	changed := map[string]any{}
	for k, v := range send {
		changed[k] = v
	}
	changed["text"] = "different"
	if code, body := h.call("POST", "/emails", "login-42", changed); code != 409 {
		t.Fatalf("idempotency conflict: %d %v", code, body)
	}

	// Start a worker that delivers everything to the scripted server.
	runner := worker.NewRunner(config.Worker{
		ControlURL: ts.URL, Token: workerToken, EHLO: "w1.openmailwire.test", PublicIP: "203.0.113.7",
		SMTPOverride: smtp.Addr, StateDir: t.TempDir(), Concurrency: 4,
		PollInterval: 50 * time.Millisecond, JobTimeout: 30 * time.Second,
	}, log, "test")
	wctx, stopWorker := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(wctx) }()

	view := h.waitStatus(id, "ok@example.test", "delivered")
	attempts := view["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("want 1 attempt, got %d", len(attempts))
	}
	a := attempts[0].(map[string]any)
	if a["worker"] != "w1" || a["source_ip"] != "203.0.113.7" || a["outcome"] != "delivered" || !strings.Contains(a["response"].(string), "queued as") {
		t.Fatalf("attempt record: %v", a)
	}

	// The receiver got one signed message whose DKIM signature verifies
	// against the DNS record the API published.
	msgs := smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("receiver got %d messages", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].MailFrom, "b-") || !strings.HasSuffix(msgs[0].MailFrom, "@bounce.acme.test") {
		t.Fatalf("envelope sender %q", msgs[0].MailFrom)
	}
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(msgs[0].Data), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if strings.HasSuffix(domain, "._domainkey.acme.test") {
				return []string{dkimPub}, nil
			}
			return nil, errors.New("not found")
		},
	})
	if err != nil || len(vs) != 1 || vs[0].Err != nil {
		t.Fatalf("dkim: %v %+v", err, vs)
	}

	// A mailbox that does not exist bounces and is suppressed next time.
	_, b := h.call("POST", "/emails", "", map[string]any{"from": "login@acme.test", "to": "gone@example.test", "subject": "x", "text": "x"})
	h.waitStatus(b["id"].(string), "gone@example.test", "bounced")
	_, b = h.call("POST", "/emails", "", map[string]any{"from": "login@acme.test", "to": "gone@example.test", "subject": "x", "text": "x"})
	h.waitStatus(b["id"].(string), "gone@example.test", "suppressed")

	// The connection drops after the body: the outcome is unknown and, by
	// default, the message is retried later rather than lost.
	_, b = h.call("POST", "/emails", "", map[string]any{"from": "login@acme.test", "to": "lost@example.test", "subject": "x", "text": "x"})
	lost := h.waitStatus(b["id"].(string), "lost@example.test", "deferred")
	la := lost["attempts"].([]any)[0].(map[string]any)
	if la["outcome"] != protocol.OutcomeUnknown {
		t.Fatalf("lost reply attempt: %v", la)
	}

	// An AI agent does the same work through MCP, first in process (as with
	// `openmailwire mcp` on stdio), then over the remote /mcp endpoint.
	agentSrv := mcpserver.New(&mcpserver.APIClient{Base: ts.URL, Key: apiKey, HTTP: http.DefaultClient}, "test")
	cT, sT := mcp.NewInMemoryTransports()
	if _, err := agentSrv.Connect(ctx, sT, nil); err != nil {
		t.Fatal(err)
	}
	agent, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-agent", Version: "1"}, nil).Connect(ctx, cT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	tl, err := agent.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	toolNames := map[string]*mcp.Tool{}
	for _, tool := range tl.Tools {
		toolNames[tool.Name] = tool
	}
	for _, n := range []string{"send_email", "get_email", "list_domains", "add_domain", "verify_domain"} {
		if toolNames[n] == nil {
			t.Fatalf("tool %s missing; have %v", n, toolNames)
		}
	}
	if a := toolNames["send_email"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Fatal("send_email must be marked destructive so clients ask before sending")
	}
	sent := callTool(t, agent, "send_email", map[string]any{
		"from": "Agent <agent@acme.test>", "to": []string{"agent-ok@example.test"},
		"subject": "Sent by an agent", "text": "hello from an agent", "idempotency_key": "agent-1",
	})
	var sentOut struct {
		ID string `json:"id"`
	}
	sentJSON, _ := json.Marshal(sent.StructuredContent)
	json.Unmarshal(sentJSON, &sentOut)
	if sentOut.ID == "" {
		t.Fatalf("send_email returned no id: %s", sentJSON)
	}
	if got := textOf(callTool(t, agent, "get_email", map[string]any{"id": sentOut.ID, "wait_seconds": 10})); !strings.Contains(got, "agent-ok@example.test: delivered") {
		t.Fatalf("get_email: %s", got)
	}
	bad, err := agent.CallTool(ctx, &mcp.CallToolParams{Name: "send_email", Arguments: map[string]any{
		"from": "x@not-added.test", "to": []string{"a@example.test"}, "subject": "x", "text": "x",
	}})
	if err != nil || !bad.IsError || !strings.Contains(textOf(bad), "domain_not_found") {
		t.Fatalf("an API error must reach the agent as a tool error: %v %+v", err, bad)
	}
	if got := textOf(callTool(t, agent, "verify_domain", map[string]any{"domain": "acme.test"})); !strings.Contains(got, "acme.test is verified") {
		t.Fatalf("verify_domain: %s", got)
	}

	remote, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-remote", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{apiKey}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if got := textOf(callTool(t, remote, "list_domains", map[string]any{})); !strings.Contains(got, "acme.test: verified") {
		t.Fatalf("remote list_domains: %s", got)
	}
	if resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != 401 {
		t.Fatalf("remote MCP without a key must be refused: %v %v", err, resp.StatusCode)
	}

	stopWorker()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A worker that vanishes: its lease expires, the sweeper records an
	// unknown outcome and reschedules; a late "delivered" report still wins.
	_, b = h.call("POST", "/emails", "", map[string]any{"from": "login@acme.test", "to": "slow@example.test", "subject": "x", "text": "x"})
	slowID := b["id"].(string)
	jobs, err := st.Lease(ctx, workerID, 10, time.Millisecond, 5)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("lease: %v %d", err, len(jobs))
	}
	var slow store.LeasedJob
	for _, j := range jobs {
		if j.Rcpt == "slow@example.test" {
			slow = j
		}
	}
	time.Sleep(20 * time.Millisecond)
	if err := st.Sweep(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(slowID, "slow@example.test", "deferred")
	if _, err := st.Report(ctx, workerID, protocol.Report{AttemptID: slow.AttemptID, Outcome: protocol.OutcomeDelivered, SMTPCode: 250, Response: "250 ok"}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(slowID, "slow@example.test", "delivered")
	// Replaying the same report changes nothing.
	if s, err := st.Report(ctx, workerID, protocol.Report{AttemptID: slow.AttemptID, Outcome: protocol.OutcomeBounced, SMTPCode: 550}); err != nil || s != "delivered" {
		t.Fatalf("replayed report: %q %v", s, err)
	}

	// Every webhook arrives signed, per recipient, with Resend's field names.
	want := []struct{ typ, rcpt string }{
		{"email.sent", "ok@example.test"},
		{"email.delivered", "ok@example.test"},
		{"email.bounced", "gone@example.test"},
		{"email.suppressed", "gone@example.test"},
		{"email.delivery_delayed", "lost@example.test"},
		{"email.delivered", "slow@example.test"},
	}
	deadline := time.Now().Add(15 * time.Second)
	for _, w := range want {
		for {
			got, ok := hooks.find(w.typ, w.rcpt)
			if ok {
				if !verifySvix(whsec, got.header.Get("svix-id"), got.header.Get("svix-timestamp"), got.body, got.header.Get("svix-signature")) {
					t.Fatalf("%s for %s: bad signature", w.typ, w.rcpt)
				}
				if got.header.Get("webhook-signature") != got.header.Get("svix-signature") {
					t.Fatal("standard-webhooks headers missing")
				}
				if w.typ == "email.bounced" && got.payload.Data.Bounce.Type != "Permanent" {
					t.Fatalf("bounce payload: %s", got.body)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s webhook for %s; got %d webhooks", w.typ, w.rcpt, hooks.count())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if _, view := h.call("GET", "/webhooks/"+hook["id"].(string), "", nil); len(view["recent_deliveries"].([]any)) == 0 {
		t.Fatalf("webhook view has no deliveries: %v", view)
	}

	// Another project cannot read this project's email.
	otherPID, _ := st.CreateProject(ctx, "other")
	otherKey, otherHash := secret.NewToken("omw_")
	st.CreateAPIKey(ctx, otherPID, "", otherKey[:10], otherHash)
	other := &harness{t: t, st: st, url: ts.URL, apiKey: otherKey}
	if code, _ := other.call("GET", "/emails/"+id, "", nil); code != 404 {
		t.Fatalf("cross-project read: %d", code)
	}
}
