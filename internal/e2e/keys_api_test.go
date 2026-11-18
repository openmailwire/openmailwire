package e2e

// End-to-end tests for API keys for agents (permissions, policies, caps)
// and the email API (attachments, scheduling, cancel, batch, list, MCP).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/mcpserver"
)

// keysCall sends a request with h's key. body is JSON-encoded unless it is
// already []byte. It returns the status, the headers and the decoded body.
func keysCall(h *harness, method, path string, header map[string]string, body any) (int, http.Header, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, h.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, resp.Header, out
}

// keysNewKey creates an API key through the API with the env's full key
// and returns a harness that uses it, and its id.
func keysNewKey(e *env, name, permission string, policy map[string]any) (*harness, string) {
	e.t.Helper()
	body := map[string]any{"name": name}
	if permission != "" {
		body["permission"] = permission
	}
	if policy != nil {
		body["policy"] = policy
	}
	code, k := e.call("POST", "/api-keys", "", body)
	if code != 201 {
		e.t.Fatalf("create key %s: %d %v", name, code, k)
	}
	return &harness{t: e.t, st: e.st, url: e.ts.URL, apiKey: k["token"].(string)}, k["id"].(string)
}

func keysSend(h *harness, from string, to ...string) (int, map[string]any) {
	h.t.Helper()
	return h.call("POST", "/emails", "", map[string]any{"from": from, "to": to, "subject": "s", "text": "t"})
}

func keysEmailCount(e *env) int {
	e.t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM emails`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func keysRecipient(t *testing.T, view map[string]any, rcpt string) map[string]any {
	t.Helper()
	for _, r := range view["recipients"].([]any) {
		if m := r.(map[string]any); m["email"] == rcpt {
			return m
		}
	}
	t.Fatalf("no recipient %s in %v", rcpt, view)
	return nil
}

func keysEventTypes(view map[string]any) []string {
	var out []string
	for _, ev := range view["events"].([]any) {
		out = append(out, ev.(map[string]any)["type"].(string))
	}
	return out
}

func TestKeyPolicies(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	e.addVerifiedDomain("support.acme.test")

	// Create a key for an agent; the token is shown once, the policy normalized.
	code, k := e.call("POST", "/api-keys", "", map[string]any{
		"name": "support-agent", "permission": "sending_access",
		"policy": map[string]any{
			"max_per_hour":       3,
			"allowed_recipients": []string{"*@Example.test", "ops@other.test"},
			"allowed_from":       []string{"agent@acme.test", "*@support.acme.test"},
		},
	})
	if code != 201 || !strings.HasPrefix(fmt.Sprint(k["token"]), "omw_") || k["permission"] != "sending_access" {
		t.Fatalf("create key: %d %v", code, k)
	}
	keyID, token := k["id"].(string), k["token"].(string)
	if k["prefix"] != token[:10] || k["last_used_at"] != nil {
		t.Fatalf("key view: %v", k)
	}
	if pol := k["policy"].(map[string]any); pol["allowed_recipients"].([]any)[0] != "*@example.test" || pol["max_per_hour"] != float64(3) {
		t.Fatalf("policy not normalized: %v", pol)
	}
	_, list := e.call("GET", "/api-keys", "", nil)
	if data := list["data"].([]any); len(data) != 2 || data[1].(map[string]any)["token"] != nil {
		t.Fatalf("list keys: %v", list)
	}
	agent := &harness{t: t, st: e.st, url: e.ts.URL, apiKey: token}

	// Bad policies and names are refused with the field named.
	for _, bad := range []map[string]any{
		{"name": "x", "policy": map[string]any{"allowed_recipients": []string{"acme.com"}}},
		{"name": "x", "policy": map[string]any{"max_per_hour": 0}},
		{"name": "x", "policy": map[string]any{"max_per_minute": 5}},
		{"name": "x", "permission": "admin"},
		{"name": ""},
	} {
		if code, b := e.call("POST", "/api-keys", "", bad); code != 422 || b["name"] != "validation_error" {
			t.Fatalf("bad key %v accepted: %d %v", bad, code, b)
		}
	}

	// sender_not_allowed and recipient_not_allowed name the cause.
	code, b := keysSend(agent, "other@acme.test", "a@example.test")
	if code != 403 || b["name"] != "sender_not_allowed" || !strings.Contains(b["message"].(string), "other@acme.test") {
		t.Fatalf("sender: %d %v", code, b)
	}
	code, b = keysSend(agent, "Agent <agent@acme.test>", "a@example.test", "bad@evil.test", "worse@evil.test")
	if code != 403 || b["name"] != "recipient_not_allowed" || !strings.Contains(b["message"].(string), "bad@evil.test") ||
		strings.Contains(b["message"].(string), "worse@evil.test") {
		t.Fatalf("recipient: %d %v", code, b)
	}
	if code, b = keysSend(agent, "x@support.acme.test", "ops@other.test"); code != 200 {
		t.Fatalf("wildcard sender: %d %v", code, b)
	}

	// max_per_hour 3: one used above, two more fit. A suppressed recipient
	// is not sent to and does not count.
	if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO suppressions (project_id, address, reason) VALUES ($1, 'gone@example.test', 'hard_bounce')`,
		e.projectID); err != nil {
		t.Fatal(err)
	}
	if code, b = keysSend(agent, "agent@acme.test", "a@example.test", "gone@example.test"); code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	if code, b = keysSend(agent, "agent@acme.test", "b@example.test"); code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	if code, b = keysSend(agent, "agent@acme.test", "gone@example.test"); code != 200 {
		t.Fatalf("a send only to suppressed recipients counts nothing: %d %v", code, b)
	}
	code, hdr, b := keysCall(agent, "POST", "/emails", nil, map[string]any{"from": "agent@acme.test", "to": "c@example.test", "subject": "s", "text": "t"})
	retry, _ := strconv.Atoi(hdr.Get("Retry-After"))
	if code != 429 || b["name"] != "rate_limit_exceeded" || retry < 3500 || retry > 3600 || !strings.Contains(b["message"].(string), "max_per_hour") {
		t.Fatalf("hourly cap: %d Retry-After=%q %v", code, hdr.Get("Retry-After"), b)
	}

	// The key's use is recorded.
	if _, v := e.call("GET", "/api-keys/"+keyID, "", nil); v["last_used_at"] == nil {
		t.Fatalf("last_used_at not set: %v", v)
	}

	// PATCH renames and replaces the policy; {} lifts every limit. The
	// permission cannot change.
	if code, b := e.call("PATCH", "/api-keys/"+keyID, "", map[string]any{"permission": "full_access"}); code != 422 {
		t.Fatalf("permission change: %d %v", code, b)
	}
	code, v := e.call("PATCH", "/api-keys/"+keyID, "", map[string]any{"name": "renamed", "policy": map[string]any{}})
	if code != 200 || v["name"] != "renamed" || len(v["policy"].(map[string]any)) != 0 || v["permission"] != "sending_access" {
		t.Fatalf("patch: %d %v", code, v)
	}
	if code, b = keysSend(agent, "anyone@acme.test", "c@example.test", "d@evil.test"); code != 200 {
		t.Fatalf("send after lifting the policy: %d %v", code, b)
	}

	// max_per_day, and a request that can never fit.
	daily, _ := keysNewKey(e, "daily", "", map[string]any{"max_per_day": 2})
	if code, b = keysSend(daily, "a@acme.test", "1@example.test", "2@example.test"); code != 200 {
		t.Fatalf("daily send: %d %v", code, b)
	}
	code, hdr, b = keysCall(daily, "POST", "/emails", nil, map[string]any{"from": "a@acme.test", "to": "3@example.test", "subject": "s", "text": "t"})
	if retry, _ := strconv.Atoi(hdr.Get("Retry-After")); code != 429 || retry < 86000 || retry > 86400 || !strings.Contains(b["message"].(string), "max_per_day") {
		t.Fatalf("daily cap: %d %q %v", code, hdr.Get("Retry-After"), b)
	}
	tiny, _ := keysNewKey(e, "tiny", "", map[string]any{"max_per_hour": 1})
	code, hdr, b = keysCall(tiny, "POST", "/emails", nil, map[string]any{"from": "a@acme.test", "to": []string{"1@example.test", "2@example.test"}, "subject": "s", "text": "t"})
	if code != 429 || hdr.Get("Retry-After") == "" || !strings.Contains(b["message"].(string), "alone") {
		t.Fatalf("oversized request: %d %v", code, b)
	}

	// Revoking a key stops it at once.
	if code, b := e.call("DELETE", "/api-keys/"+keyID, "", nil); code != 200 || b["deleted"] != true {
		t.Fatalf("revoke: %d %v", code, b)
	}
	if code, b := keysSend(agent, "a@acme.test", "a@example.test"); code != 401 {
		t.Fatalf("revoked key still works: %d %v", code, b)
	}
	if code, _ := e.call("GET", "/api-keys/"+keyID, "", nil); code != 404 {
		t.Fatalf("revoked key still listed: %d", code)
	}
	if code, _ := e.call("DELETE", "/api-keys/"+keyID, "", nil); code != 404 {
		t.Fatalf("second revoke: %d", code)
	}
	// Another project cannot see this project's keys.
	if code, _ := e.otherProject().call("GET", "/api-keys/"+keyID, "", nil); code != 404 {
		t.Fatalf("cross-project key read: %d", code)
	}
}

// TestCapsHoldUnderConcurrency: parallel requests cannot overshoot a cap
// together, because the insert re-checks it under a per-key lock.
func TestCapsHoldUnderConcurrency(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	agent, _ := keysNewKey(e, "burst", "sending_access", map[string]any{"max_per_hour": 5})
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(map[string]any{"from": "a@acme.test", "to": fmt.Sprintf("r%d@example.test", i), "subject": "s", "text": "t"})
			req, _ := http.NewRequest("POST", e.ts.URL+"/emails", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+agent.apiKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[200] != 5 || codes[429] != 7 {
		t.Fatalf("want 5 accepted and 7 refused, got %v", codes)
	}
}

func TestSendingAccess(t *testing.T) {
	e := newEnv(t, nil)
	domID, _ := e.addVerifiedDomain("acme.test")
	agent, agentID := keysNewKey(e, "agent", "sending_access", nil)
	peer, peerID := keysNewKey(e, "peer", "sending_access", nil)

	for _, c := range []struct{ method, path string }{
		{"POST", "/domains"}, {"POST", "/domains/" + domID + "/verify"}, {"DELETE", "/domains/" + domID},
		{"POST", "/webhooks"}, {"GET", "/webhooks"}, {"GET", "/webhooks/" + domID}, {"DELETE", "/webhooks/" + domID},
		{"GET", "/api-keys"}, {"POST", "/api-keys"}, {"GET", "/api-keys/" + agentID},
		{"PATCH", "/api-keys/" + agentID}, {"DELETE", "/api-keys/" + agentID},
	} {
		code, b := agent.call(c.method, c.path, "", map[string]any{})
		if code != 403 || b["name"] != "insufficient_permission" || !strings.Contains(b["message"].(string), "full_access") {
			t.Fatalf("%s %s with a sending key: %d %v", c.method, c.path, code, b)
		}
	}
	if code, _ := agent.call("GET", "/domains", "", nil); code != 200 {
		t.Fatalf("list domains: %d", code)
	}
	if code, _ := agent.call("GET", "/domains/"+domID, "", nil); code != 200 {
		t.Fatalf("get domain: %d", code)
	}

	_, mine := keysSend(agent, "a@acme.test", "x@example.test")
	_, theirs := keysSend(peer, "a@acme.test", "y@example.test")
	_, full := keysSend(e.harness, "a@acme.test", "z@example.test")
	mineID, theirsID := mine["id"].(string), theirs["id"].(string)

	// The key that sent each email is recorded; a sending key sees only its own.
	if _, v := e.call("GET", "/emails/"+mineID, "", nil); v["api_key_id"] != agentID {
		t.Fatalf("api_key_id: %v", v["api_key_id"])
	}
	if code, v := agent.call("GET", "/emails/"+mineID, "", nil); code != 200 || v["api_key_id"] != agentID {
		t.Fatalf("own email: %d %v", code, v)
	}
	if code, _ := agent.call("GET", "/emails/"+theirsID, "", nil); code != 404 {
		t.Fatalf("a sending key read another key's email: %d", code)
	}
	if code, _ := agent.call("POST", "/emails/"+theirsID+"/cancel", "", nil); code != 404 {
		t.Fatalf("a sending key canceled another key's email: %d", code)
	}
	_, l := agent.call("GET", "/emails", "", nil)
	if data := l["data"].([]any); len(data) != 1 || data[0].(map[string]any)["id"] != mineID {
		t.Fatalf("sending key list: %v", l)
	}
	if code, b := agent.call("GET", "/emails?api_key_id="+peerID, "", nil); code != 403 || b["name"] != "insufficient_permission" {
		t.Fatalf("sending key listing another key: %d %v", code, b)
	}
	if code, l := agent.call("GET", "/emails?api_key_id="+agentID, "", nil); code != 200 || len(l["data"].([]any)) != 1 {
		t.Fatalf("sending key listing itself: %d %v", code, l)
	}
	if _, l := e.call("GET", "/emails", "", nil); len(l["data"].([]any)) != 3 {
		t.Fatalf("full key list: %v", l)
	}
	if _, l := e.call("GET", "/emails?api_key_id="+peerID, "", nil); len(l["data"].([]any)) != 1 || l["data"].([]any)[0].(map[string]any)["id"] != theirsID {
		t.Fatalf("filter by key: %v", l)
	}
	if code, _ := e.call("GET", "/emails/"+full["id"].(string), "", nil); code != 200 {
		t.Fatalf("full key reads its email: %d", code)
	}

	// Over remote MCP, a sending key has the same limits.
	remote, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(e.ctx,
		&mcp.StreamableClientTransport{Endpoint: e.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{agent.apiKey}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	res, err := remote.CallTool(e.ctx, &mcp.CallToolParams{Name: "add_domain", Arguments: map[string]any{"name": "new.test"}})
	if err != nil || !res.IsError || !strings.Contains(textOf(res), "insufficient_permission") {
		t.Fatalf("add_domain with a sending key over MCP: %v %s", err, textOf(res))
	}
	callTool(t, remote, "send_email", map[string]any{"from": "a@acme.test", "to": []string{"w@example.test"}, "subject": "via mcp", "text": "t"})
	got := textOf(callTool(t, remote, "list_emails", map[string]any{}))
	if !strings.Contains(got, "via mcp") || !strings.Contains(got, mineID) || strings.Contains(got, theirsID) {
		t.Fatalf("list_emails with a sending key: %s", got)
	}
}

// keysMIME parses a message into its structure ("multipart/mixed[...]")
// and its decoded leaf parts.
type keysPart struct {
	header map[string][]string
	body   []byte
}

func keysMIME(t *testing.T, raw []byte) (string, []keysPart) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var parts []keysPart
	var walk func(ct string, body io.Reader, header map[string][]string) string
	walk = func(ct string, body io.Reader, header map[string][]string) string {
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil {
			t.Fatalf("content-type %q: %v", ct, err)
		}
		if !strings.HasPrefix(mt, "multipart/") {
			data, _ := io.ReadAll(body) // multipart.Part decodes quoted-printable itself
			if strings.EqualFold(http.Header(header).Get("Content-Transfer-Encoding"), "base64") {
				if data, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(string(data), "\r\n", "")); err != nil {
					t.Fatal(err)
				}
			}
			parts = append(parts, keysPart{header, data})
			return mt
		}
		mr := multipart.NewReader(body, params["boundary"])
		var kids []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			kids = append(kids, walk(p.Header.Get("Content-Type"), p, p.Header))
		}
		return mt + "[" + strings.Join(kids, ",") + "]"
	}
	return walk(msg.Header.Get("Content-Type"), msg.Body, msg.Header), parts
}

func TestAttachmentsDelivered(t *testing.T) {
	e := newEnv(t, nil)
	_, dkimTXT := e.addVerifiedDomain("acme.test")
	e.startWorker()

	pdf := make([]byte, 300<<10)
	rand.Read(pdf)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 1, 2, 3}, 2000)...)
	b64 := base64.StdEncoding.EncodeToString
	code, b := e.call("POST", "/emails", "", map[string]any{
		"from": "Acme <reports@acme.test>", "to": "boss@example.test", "subject": "Monthly report",
		"text": "The report is attached.", "html": `<p>The report is attached.</p><img src="cid:logo">`,
		"attachments": []map[string]any{
			{"filename": "report.pdf", "content": b64(pdf)},
			{"filename": "logo.png", "content": b64(png), "content_type": "image/png", "content_id": "logo"},
		},
	})
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "boss@example.test", "delivered")
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	verifyDKIM(t, msgs[0].Data, dkimTXT)
	shape, parts := keysMIME(t, msgs[0].Data)
	if shape != "multipart/mixed[multipart/related[multipart/alternative[text/plain,text/html],image/png],application/pdf]" {
		t.Fatalf("structure: %s", shape)
	}
	if string(parts[0].body) != "The report is attached." {
		t.Fatalf("text part: %q", parts[0].body)
	}
	logo, report := http.Header(parts[2].header), http.Header(parts[3].header)
	if !bytes.Equal(parts[2].body, png) || logo.Get("Content-ID") != "<logo>" || !strings.HasPrefix(logo.Get("Content-Disposition"), "inline") {
		t.Fatalf("inline part: %v", logo)
	}
	if !bytes.Equal(parts[3].body, pdf) || !strings.HasPrefix(report.Get("Content-Disposition"), "attachment") ||
		!strings.HasPrefix(report.Get("Content-Type"), "application/pdf") {
		t.Fatalf("attachment part: %v", report)
	}

	// Limits.
	one := map[string]any{"filename": "a.txt", "content": b64([]byte("x"))}
	many := make([]map[string]any, 21)
	for i := range many {
		many[i] = one
	}
	big := b64(make([]byte, 5<<20+1))
	for name, c := range map[string]struct {
		atts any
		want string
	}{
		"count":   {many, "at most 20"},
		"size":    {[]map[string]any{{"filename": "a.bin", "content": big}, {"filename": "b.bin", "content": big}}, "10 MB"},
		"base64":  {[]map[string]any{{"filename": "a.bin", "content": "not base64!"}}, "base64"},
		"missing": {[]map[string]any{{"filename": "a.bin"}}, "content is required"},
		"cid":     {[]map[string]any{{"filename": "a.png", "content": b64(png), "content_id": "a b"}}, "content_id"},
	} {
		code, b := e.call("POST", "/emails", "", map[string]any{"from": "a@acme.test", "to": "x@example.test", "subject": "s", "text": "t", "attachments": c.atts})
		if code != 422 || !strings.Contains(fmt.Sprint(b["message"]), c.want) {
			t.Fatalf("%s: %d %v", name, code, b)
		}
	}
	// The request body may be up to 20 MB, no more.
	huge, _ := json.Marshal(map[string]any{"from": "a@acme.test", "to": "x@example.test", "subject": "s", "text": "t",
		"attachments": []map[string]any{{"filename": "a.bin", "content": b64(make([]byte, 16<<20))}}})
	if code, _, b := keysCall(e.harness, "POST", "/emails", nil, huge); code != 413 || b["name"] != "payload_too_large" {
		t.Fatalf("21 MB body: %d %v", code, b)
	}
	if len(e.smtp.Messages()) != 1 {
		t.Fatal("a refused email was sent")
	}
}

func TestScheduledSendAndCancel(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	hooks := &receiver{}
	hookSrv := httptest.NewServer(hooks)
	defer hookSrv.Close()
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": hookSrv.URL, "events": []string{"email.canceled"}}); code != 201 {
		t.Fatalf("webhook: %d %v", code, b)
	}
	e.startWebhooks()
	e.startWorker()

	// Validation.
	now := time.Now().UTC()
	for _, bad := range []map[string]any{
		{"scheduled_at": now.Add(-time.Minute).Format(time.RFC3339)},
		{"scheduled_at": now.Add(31 * 24 * time.Hour).Format(time.RFC3339)},
		{"scheduled_at": "tomorrow"},
		{"scheduled_at": now.Add(time.Hour).Format(time.RFC3339), "expires_at": now.Add(30 * time.Minute).Format(time.RFC3339)},
	} {
		req := map[string]any{"from": "a@acme.test", "to": "x@example.test", "subject": "s", "text": "t"}
		for k, v := range bad {
			req[k] = v
		}
		if code, b := e.call("POST", "/emails", "", req); code != 422 {
			t.Fatalf("%v accepted: %d %v", bad, code, b)
		}
	}

	// A send scheduled 2 s ahead is not delivered before then.
	at := time.Now().Add(2 * time.Second).UTC().Truncate(time.Millisecond)
	code, b := e.call("POST", "/emails", "", map[string]any{"from": "a@acme.test", "to": "later@example.test", "subject": "later", "text": "t",
		"scheduled_at": at.Format(time.RFC3339Nano)})
	if code != 200 {
		t.Fatalf("schedule: %d %v", code, b)
	}
	id := b["id"].(string)
	_, v := e.call("GET", "/emails/"+id, "", nil)
	if v["scheduled_at"] == nil || v["last_event"] != "scheduled" || strings.Join(keysEventTypes(v), ",") != "queued,scheduled" {
		t.Fatalf("scheduled view: %v", v)
	}
	if exp, _ := time.Parse(time.RFC3339Nano, v["expires_at"].(string)); !exp.Equal(at.Add(time.Hour)) {
		t.Fatalf("expires_at %v, want scheduled_at + default expiry", exp)
	}
	if next := keysRecipient(t, v, "later@example.test")["next_attempt_at"]; next == nil {
		t.Fatal("no next_attempt_at")
	}
	time.Sleep(time.Until(at) - 400*time.Millisecond)
	if n := len(e.smtp.Messages()); n != 0 {
		t.Fatalf("delivered %d messages before scheduled_at", n)
	}
	v = e.waitStatus(id, "later@example.test", "delivered")
	started, _ := time.Parse(time.RFC3339Nano, v["attempts"].([]any)[0].(map[string]any)["started_at"].(string))
	if started.Before(at) {
		t.Fatalf("attempt started at %v, before scheduled_at %v", started, at)
	}

	// Cancel a scheduled send: every recipient not yet attempted.
	code, b = e.call("POST", "/emails", "", map[string]any{"from": "a@acme.test", "to": []string{"c1@example.test", "c2@example.test"},
		"subject": "cancel me", "text": "t", "scheduled_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if code != 200 {
		t.Fatalf("schedule: %d %v", code, b)
	}
	cid := b["id"].(string)
	code, b = e.call("POST", "/emails/"+cid+"/cancel", "", nil)
	if code != 200 || fmt.Sprint(b["canceled"]) != "[c1@example.test c2@example.test]" {
		t.Fatalf("cancel: %d %v", code, b)
	}
	_, v = e.call("GET", "/emails/"+cid, "", nil)
	if keysRecipient(t, v, "c1@example.test")["status"] != "canceled" || v["last_event"] != "canceled" {
		t.Fatalf("canceled view: %v", v)
	}
	if code, b = e.call("POST", "/emails/"+cid+"/cancel", "", nil); code != 409 || b["name"] != "cannot_cancel" {
		t.Fatalf("second cancel: %d %v", code, b)
	}
	if code, b = e.call("POST", "/emails/"+id+"/cancel", "", nil); code != 409 || b["name"] != "cannot_cancel" {
		t.Fatalf("cancel a delivered email: %d %v", code, b)
	}
	if code, _ = e.call("POST", "/emails/00000000-0000-0000-0000-000000000000/cancel", "", nil); code != 404 {
		t.Fatalf("cancel unknown: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, rcpt := range []string{"c1@example.test", "c2@example.test"} {
		for {
			if got, ok := hooks.find("email.canceled", rcpt); ok {
				if got.payload.Data.EmailID != cid {
					t.Fatalf("webhook email id %s", got.payload.Data.EmailID)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no email.canceled webhook for %s", rcpt)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Canceled recipients do not count toward a key's cap.
	capped, _ := keysNewKey(e, "capped", "", map[string]any{"max_per_hour": 2})
	_, b = capped.call("POST", "/emails", "", map[string]any{"from": "a@acme.test", "to": []string{"1@example.test", "2@example.test"},
		"subject": "s", "text": "t", "scheduled_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if code, _ := capped.call("POST", "/emails/"+b["id"].(string)+"/cancel", "", nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	if code, b := keysSend(capped, "a@acme.test", "3@example.test", "4@example.test"); code != 200 {
		t.Fatalf("send after cancel: %d %v", code, b)
	}
}

func TestBatch(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	e.addVerifiedDomain("other.test")
	item := func(from, to, subject string) map[string]any {
		return map[string]any{"from": from, "to": to, "subject": subject, "text": "t"}
	}

	// Every item is checked before anything is stored.
	before := keysEmailCount(e)
	for name, c := range map[string]struct {
		items          any
		code           int
		errName, inMsg string
	}{
		"empty":       {[]any{}, 422, "validation_error", "1 to 100"},
		"not array":   {map[string]any{"from": "a@acme.test"}, 422, "validation_error", "JSON array"},
		"too many":    {make([]map[string]any, 101), 422, "validation_error", "1 to 100"},
		"invalid":     {[]any{item("a@acme.test", "x@example.test", "ok"), item("a@acme.test", "y@example.test", "")}, 422, "validation_error", "emails[1]: subject is required"},
		"domain":      {[]any{item("a@acme.test", "x@example.test", "ok"), item("a@acme.test", "x@example.test", "ok"), item("a@nope.test", "z@example.test", "bad")}, 403, "domain_not_found", "emails[2]:"},
		"attachments": {[]any{map[string]any{"from": "a@acme.test", "to": "x@example.test", "subject": "s", "text": "t", "attachments": []any{}}}, 422, "validation_error", "attachments are not supported"},
	} {
		code, _, b := keysCall(e.harness, "POST", "/emails/batch", nil, c.items)
		if code != c.code || b["name"] != c.errName || !strings.Contains(fmt.Sprint(b["message"]), c.inMsg) {
			t.Fatalf("%s: %d %v", name, code, b)
		}
	}
	scoped, _ := keysNewKey(e, "scoped", "sending_access", map[string]any{"allowed_recipients": []string{"*@example.test"}})
	code, _, b := keysCall(scoped, "POST", "/emails/batch", nil, []any{item("a@acme.test", "x@example.test", "ok"), item("a@acme.test", "x@evil.test", "no")})
	if code != 403 || b["name"] != "recipient_not_allowed" || !strings.HasPrefix(b["message"].(string), "emails[1]:") {
		t.Fatalf("policy in batch: %d %v", code, b)
	}
	// The caps apply to the batch as a whole.
	capped, _ := keysNewKey(e, "capped", "", map[string]any{"max_per_hour": 2})
	code, hdr, b := keysCall(capped, "POST", "/emails/batch", nil, []any{item("a@acme.test", "1@example.test", "1"), item("a@acme.test", "2@example.test", "2"), item("a@acme.test", "3@example.test", "3")})
	if code != 429 || b["name"] != "rate_limit_exceeded" || hdr.Get("Retry-After") == "" {
		t.Fatalf("batch over cap: %d %v", code, b)
	}
	if n := keysEmailCount(e); n != before {
		t.Fatalf("a refused batch stored %d emails", n-before)
	}

	// A good batch is stored at once, from several domains, and delivered.
	e.startWorker()
	items := []any{item("a@acme.test", "b1@example.test", "one"), item("a@other.test", "b2@example.test", "two"), item("a@acme.test", "b3@example.test", "three")}
	code, hdr, b = keysCall(e.harness, "POST", "/emails/batch", map[string]string{"Idempotency-Key": "batch-1"}, items)
	if code != 200 || len(b["data"].([]any)) != 3 || hdr.Get("Idempotent-Replayed") != "" {
		t.Fatalf("batch: %d %v", code, b)
	}
	var ids []string
	for i, d := range b["data"].([]any) {
		ids = append(ids, d.(map[string]any)["id"].(string))
		e.waitStatus(ids[i], fmt.Sprintf("b%d@example.test", i+1), "delivered")
	}
	// The same Idempotency-Key and items replay; a changed item conflicts.
	code, hdr, b = keysCall(e.harness, "POST", "/emails/batch", map[string]string{"Idempotency-Key": "batch-1"}, items)
	if code != 200 || hdr.Get("Idempotent-Replayed") != "true" || b["data"].([]any)[2].(map[string]any)["id"] != ids[2] {
		t.Fatalf("batch replay: %d %v", code, b)
	}
	items[1] = item("a@other.test", "b2@example.test", "changed")
	if code, _, b = keysCall(e.harness, "POST", "/emails/batch", map[string]string{"Idempotency-Key": "batch-1"}, items); code != 409 {
		t.Fatalf("batch conflict: %d %v", code, b)
	}
	// Each item is stored under <key>:<index>.
	if _, again := e.call("POST", "/emails", "batch-1:0", item("a@acme.test", "b1@example.test", "one")); again["id"] != ids[0] {
		t.Fatalf("item idempotency key: %v, want %s", again["id"], ids[0])
	}
	if n := len(e.smtp.Messages()); n != 3 {
		t.Fatalf("sent %d messages, want 3", n)
	}
}

func TestListEmails(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	other, otherID := keysNewKey(e, "other", "", nil)
	var ids []string
	for i := 0; i < 5; i++ {
		_, b := e.call("POST", "/emails", "", map[string]any{"from": "Acme <a@acme.test>", "to": fmt.Sprintf("r%d@example.test", i), "subject": fmt.Sprintf("s%d", i), "text": "t"})
		ids = append(ids, b["id"].(string))
	}
	_, ob := keysSend(other, "a@acme.test", "o@example.test")

	// Newest first, in pages, until has_more is false.
	var got []string
	path := "/emails?limit=2"
	for pages := 0; ; pages++ {
		code, l := e.call("GET", path, "", nil)
		if code != 200 || l["object"] != "list" || pages > 5 {
			t.Fatalf("list: %d %v", code, l)
		}
		for _, d := range l["data"].([]any) {
			got = append(got, d.(map[string]any)["id"].(string))
		}
		if l["has_more"] != true {
			if l["next_before"] != nil {
				t.Fatalf("next_before on the last page: %v", l)
			}
			break
		}
		path = "/emails?limit=2&before=" + l["next_before"].(string)
	}
	want := []string{ob["id"].(string), ids[4], ids[3], ids[2], ids[1], ids[0]}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order:\n got %v\nwant %v", got, want)
	}

	_, l := e.call("GET", "/emails?limit=1&api_key_id="+otherID, "", nil)
	row := l["data"].([]any)[0].(map[string]any)
	if row["id"] != ob["id"] || row["api_key_id"] != otherID || row["from"] != "a@acme.test" || row["subject"] != "s" ||
		fmt.Sprint(row["to"]) != "[o@example.test]" || row["last_event"] != "queued" || row["scheduled_at"] != nil || row["created_at"] == nil {
		t.Fatalf("row: %v", row)
	}
	if l["has_more"] != false {
		t.Fatalf("has_more: %v", l)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "before=nope", "api_key_id=nope", "before=00000000-0000-0000-0000-000000000000"} {
		if code, b := e.call("GET", "/emails?"+q, "", nil); code != 422 {
			t.Fatalf("%s: %d %v", q, code, b)
		}
	}
	// Another project sees none of these.
	if _, l := e.otherProject().call("GET", "/emails", "", nil); len(l["data"].([]any)) != 0 {
		t.Fatalf("cross-project list: %v", l)
	}
}

func TestMCPEmailTools(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("acme.test")
	srv := mcpserver.New(&mcpserver.APIClient{Base: e.ts.URL, Key: e.apiKey, HTTP: http.DefaultClient}, "test")
	cT, sT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(e.ctx, sT, nil); err != nil {
		t.Fatal(err)
	}
	agent, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(e.ctx, cT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	tl, err := agent.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range tl.Tools {
		tools[tool.Name] = tool
	}
	if tools["list_emails"] == nil || tools["cancel_email"] == nil {
		t.Fatalf("tools: %v", tools)
	}
	if a := tools["cancel_email"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Fatal("cancel_email must be marked destructive")
	}
	if !tools["list_emails"].Annotations.ReadOnlyHint {
		t.Fatal("list_emails must be read-only")
	}

	sent := callTool(t, agent, "send_email", map[string]any{
		"from": "a@acme.test", "to": []string{"later@example.test"}, "subject": "scheduled by an agent", "text": "t",
		"scheduled_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "idempotency_key": "agent-scheduled",
	})
	var out struct{ ID, Next string }
	j, _ := json.Marshal(sent.StructuredContent)
	json.Unmarshal(j, &out)
	if out.ID == "" || !strings.Contains(out.Next, "cancel_email") {
		t.Fatalf("send_email: %s", j)
	}
	listed := textOf(callTool(t, agent, "list_emails", map[string]any{"limit": 5}))
	if !strings.Contains(listed, out.ID) || !strings.Contains(listed, "scheduled for") || !strings.Contains(listed, "last event scheduled") {
		t.Fatalf("list_emails: %s", listed)
	}
	if got := textOf(callTool(t, agent, "cancel_email", map[string]any{"id": out.ID})); !strings.Contains(got, "later@example.test") {
		t.Fatalf("cancel_email: %s", got)
	}
	res, err := agent.CallTool(context.Background(), &mcp.CallToolParams{Name: "cancel_email", Arguments: map[string]any{"id": out.ID}})
	if err != nil || !res.IsError || !strings.Contains(textOf(res), "cannot_cancel") {
		t.Fatalf("second cancel_email: %v %s", err, textOf(res))
	}

	// Paging through list_emails.
	for i := 0; i < 3; i++ {
		keysSend(e.harness, "a@acme.test", fmt.Sprintf("p%d@example.test", i))
	}
	page := textOf(callTool(t, agent, "list_emails", map[string]any{"limit": 2}))
	i := strings.Index(page, "before=")
	if i < 0 {
		t.Fatalf("no next page hint: %s", page)
	}
	before := strings.TrimSuffix(strings.TrimSpace(page[i+len("before="):]), ".")
	if rest := textOf(callTool(t, agent, "list_emails", map[string]any{"before": before})); !strings.Contains(rest, out.ID) {
		t.Fatalf("second page: %s", rest)
	}
}
