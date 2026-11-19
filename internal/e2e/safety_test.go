package e2e

// End-to-end tests for agent safety and compliance (v0.6, lane B):
// approval holds, the suppressions API, streams with one-click
// unsubscribe, Feedback-ID, the per-recipient frequency cap, circuit
// breakers, the pre-send check, project settings and their MCP tools.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/mcpserver"
	"github.com/openmailwire/openmailwire/internal/message"
	"github.com/openmailwire/openmailwire/internal/store"
)

// sfSend sends a plain email with h's key.
func sfSend(h *harness, from string, to []string, extra map[string]any) (int, map[string]any) {
	h.t.Helper()
	body := map[string]any{"from": from, "to": to, "subject": "s", "text": "plain words"}
	for k, v := range extra {
		body[k] = v
	}
	return h.call("POST", "/emails", "", body)
}

func sfStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, x.(string))
	}
	return out
}

// sfHook starts a webhook endpoint subscribed to events and returns its
// receiver.
func sfHook(e *env, events ...string) *receiver {
	e.t.Helper()
	rc := &receiver{}
	hs := httptest.NewServer(rc)
	e.t.Cleanup(hs.Close)
	if code, b := e.call("POST", "/webhooks", "", map[string]any{"endpoint": hs.URL, "events": events}); code != 201 {
		e.t.Fatalf("create webhook: %d %v", code, b)
	}
	e.startWebhooks()
	return rc
}

// sfWaitHook waits for a webhook of type typ about emailID ("" for any) and
// returns its decoded body.
func sfWaitHook(t *testing.T, rc *receiver, typ, emailID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rc.mu.Lock()
		for _, r := range rc.got {
			if r.payload.Type == typ && (emailID == "" || r.payload.Data.EmailID == emailID) {
				var m map[string]any
				json.Unmarshal(r.body, &m)
				rc.mu.Unlock()
				return m
			}
		}
		rc.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %s webhook for %q", typ, emailID)
	return nil
}

func sfStatusOf(h *harness, id, rcpt string) string {
	h.t.Helper()
	_, v := h.call("GET", "/emails/"+id, "", nil)
	for _, r := range v["recipients"].([]any) {
		if m := r.(map[string]any); m["email"] == rcpt {
			return m["status"].(string)
		}
	}
	return ""
}

func sfRaw(h *harness, raw string, extra map[string]any) (int, map[string]any) {
	h.t.Helper()
	body := map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(raw))}
	for k, v := range extra {
		body[k] = v
	}
	return h.call("POST", "/emails/raw", "", body)
}

// sfMessageTo returns the one delivered message for rcpt.
func sfMessageTo(t *testing.T, e *env, rcpt string) []byte {
	t.Helper()
	var found [][]byte
	for _, m := range e.smtp.Messages() {
		if slices.Contains(m.Rcpts, rcpt) {
			found = append(found, m.Data)
		}
	}
	if len(found) == 0 {
		t.Fatalf("nothing delivered to %s", rcpt)
	}
	return found[len(found)-1]
}

func sfHeader(t *testing.T, raw []byte) mail.Header {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m.Header
}

func sfKeyID(e *env, name string) string {
	e.t.Helper()
	_, l := e.call("GET", "/api-keys", "", nil)
	for _, k := range l["data"].([]any) {
		if m := k.(map[string]any); m["name"] == name {
			return m["id"].(string)
		}
	}
	e.t.Fatalf("no key named %s", name)
	return ""
}

func TestSafetyApprovalHolds(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("hold.test")
	e.startWorker()
	rc := sfHook(e, "email.held", "email.approved", "email.rejected")

	agent, agentID := keysNewKey(e, "agent", "sending_access", map[string]any{"require_approval": map[string]any{
		"first_contact": true, "has_links": true, "has_attachments": true, "recipients_over": 2,
	}})
	reviewer, reviewerID := keysNewKey(e, "reviewer", "sending_access", map[string]any{"can_approve": true})

	// A first contact is held, not sent.
	code, b := sfSend(agent, "a@hold.test", []string{"new@example.test"}, nil)
	if code != 200 || b["held"] != true || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"first_contact"}) {
		t.Fatalf("first contact: %d %v", code, b)
	}
	first := b["id"].(string)
	e.waitStatus(first, "new@example.test", "held")
	time.Sleep(300 * time.Millisecond)
	if n := len(e.smtp.Messages()); n != 0 {
		t.Fatalf("a held email was delivered (%d messages)", n)
	}

	// The agent sees its held email but cannot approve it.
	code, l := agent.call("GET", "/emails/held", "", nil)
	data, _ := l["data"].([]any)
	if code != 200 || len(data) != 1 {
		t.Fatalf("agent lists held: %d %v", code, l)
	}
	h := data[0].(map[string]any)
	if h["id"] != first || h["text"] != "plain words" || h["api_key_id"] != agentID ||
		!slices.Equal(sfStrings(h["recipients"]), []string{"new@example.test"}) {
		t.Fatalf("held email view: %v", h)
	}
	if code, b := agent.call("POST", "/emails/"+first+"/approve", "", nil); code != 403 || b["name"] != "insufficient_permission" {
		t.Fatalf("agent approved: %d %v", code, b)
	}
	if code, b := agent.call("POST", "/emails/"+first+"/reject", "", nil); code != 403 {
		t.Fatalf("agent rejected: %d %v", code, b)
	}

	// The reviewer approves; the email goes out.
	code, b = reviewer.call("POST", "/emails/"+first+"/approve", "", map[string]any{"note": "fine"})
	if code != 200 || b["decision"] != "approved" || !slices.Equal(sfStrings(b["recipients"]), []string{"new@example.test"}) {
		t.Fatalf("approve: %d %v", code, b)
	}
	v := e.waitStatus(first, "new@example.test", "delivered")
	var types []string
	for _, ev := range v["events"].([]any) {
		types = append(types, ev.(map[string]any)["type"].(string))
	}
	if !slices.Contains(types, "held") || !slices.Contains(types, "approved") {
		t.Fatalf("timeline lacks held/approved: %v", types)
	}
	if code, b := reviewer.call("POST", "/emails/"+first+"/approve", "", nil); code != 409 || b["name"] != "not_held" {
		t.Fatalf("approve twice: %d %v", code, b)
	}
	approved := sfWaitHook(t, rc, "email.approved", first)
	if a := approved["data"].(map[string]any)["approval"].(map[string]any); a["approved_by"] != reviewerID || a["note"] != "fine" {
		t.Fatalf("email.approved payload: %v", approved)
	}
	held := sfWaitHook(t, rc, "email.held", first)
	if a := held["data"].(map[string]any)["approval"].(map[string]any); !slices.Equal(sfStrings(a["reasons"]), []string{"first_contact"}) {
		t.Fatalf("email.held payload: %v", held)
	}

	// Now a contact: plain mail to it is sent at once.
	code, b = sfSend(agent, "a@hold.test", []string{"new@example.test"}, nil)
	if code != 200 || b["held"] != nil {
		t.Fatalf("known contact: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "new@example.test", "delivered")

	// Links, attachments and many recipients are held.
	code, b = sfSend(agent, "a@hold.test", []string{"new@example.test"}, map[string]any{"text": "see www.hold.test/offer"})
	if code != 200 || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"has_links"}) {
		t.Fatalf("link: %d %v", code, b)
	}
	linked := b["id"].(string)
	code, b = sfSend(agent, "a@hold.test", []string{"new@example.test"}, map[string]any{"attachments": []map[string]any{
		{"filename": "a.txt", "content": base64.StdEncoding.EncodeToString([]byte("x"))}}})
	if code != 200 || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"has_attachments"}) {
		t.Fatalf("attachment: %d %v", code, b)
	}
	code, b = sfSend(agent, "a@hold.test", []string{"new@example.test", "x1@example.test", "x2@example.test"}, nil)
	if code != 200 || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"first_contact", "recipients_over"}) {
		t.Fatalf("three recipients: %d %v", code, b)
	}

	// The reviewer sees all of them; rejecting one means it is never sent.
	_, l = reviewer.call("GET", "/emails/held?limit=2", "", nil)
	if len(l["data"].([]any)) != 2 || l["has_more"] != true {
		t.Fatalf("reviewer page: %v", l)
	}
	_, l2 := reviewer.call("GET", "/emails/held?limit=2&before="+l["next_before"].(string), "", nil)
	if len(l2["data"].([]any)) != 1 {
		t.Fatalf("reviewer second page: %v", l2)
	}
	code, b = reviewer.call("POST", "/emails/"+linked+"/reject", "", map[string]any{"note": "no offers"})
	if code != 200 || b["decision"] != "rejected" {
		t.Fatalf("reject: %d %v", code, b)
	}
	if s := sfStatusOf(e.harness, linked, "new@example.test"); s != "rejected" {
		t.Fatalf("rejected email status %q", s)
	}
	if r := sfWaitHook(t, rc, "email.rejected", linked); r["data"].(map[string]any)["approval"].(map[string]any)["note"] != "no offers" {
		t.Fatalf("email.rejected payload: %v", r)
	}
	if code, b := e.call("POST", "/emails/"+linked+"/approve", "", nil); code != 409 {
		t.Fatalf("approve a rejected email: %d %v", code, b)
	}

	// A key never approves its own email, even with can_approve.
	self, _ := keysNewKey(e, "self", "sending_access", map[string]any{"require_approval": map[string]any{"always": true}, "can_approve": true})
	code, b = sfSend(self, "a@hold.test", []string{"new@example.test"}, nil)
	if code != 200 || b["held"] != true {
		t.Fatalf("self send: %d %v", code, b)
	}
	own := b["id"].(string)
	if code, b := self.call("POST", "/emails/"+own+"/approve", "", nil); code != 403 || b["name"] != "cannot_approve_own_email" {
		t.Fatalf("self-approval: %d %v", code, b)
	}
	if code, b := e.call("POST", "/emails/"+own+"/approve", "", nil); code != 200 {
		t.Fatalf("full key approves: %d %v", code, b)
	}
	e.waitStatus(own, "new@example.test", "delivered")

	// Policy validation.
	for _, pol := range []map[string]any{
		{"require_approval": map[string]any{}},
		{"require_approval": map[string]any{"recipients_over": -1}},
		{"require_approval": map[string]any{"sometimes": true}},
	} {
		if code, b := e.call("POST", "/api-keys", "", map[string]any{"name": "bad", "policy": pol}); code != 422 {
			t.Fatalf("policy %v accepted: %d %v", pol, code, b)
		}
	}
}

// TestSafetyHoldsOnRawAndBatch: the raw path (and so SMTP submission) and
// batches follow the same approval rules.
func TestSafetyHoldsOnRawAndBatch(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("rawhold.test")
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{"require_approval": map[string]any{
		"has_links": true, "has_attachments": true}})

	// The link hides in a base64 HTML part.
	html := base64.StdEncoding.EncodeToString([]byte(`<p>Click <a href="https://evil.example/x">here</a></p>`))
	linked := "From: a@rawhold.test\r\nTo: r@example.test\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + html + "\r\n"
	code, b := sfRaw(agent, linked, nil)
	if code != 200 || b["held"] != true || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"has_links"}) {
		t.Fatalf("raw with a link: %d %v", code, b)
	}
	if s := sfStatusOf(agent, b["id"].(string), "r@example.test"); s != "held" {
		t.Fatalf("raw held status %q", s)
	}

	attached := "From: a@rawhold.test\r\nTo: r@example.test\r\nSubject: s\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=a.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n--b--\r\n"
	code, b = sfRaw(agent, attached, nil)
	if code != 200 || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"has_attachments"}) {
		t.Fatalf("raw with an attachment: %d %v", code, b)
	}

	code, b = sfRaw(agent, "From: a@rawhold.test\r\nTo: r@example.test\r\nSubject: s\r\n\r\nplain\r\n", nil)
	if code != 200 || b["held"] != nil {
		t.Fatalf("plain raw: %d %v", code, b)
	}

	// Nothing is held when every recipient is suppressed: nothing waits.
	e.call("POST", "/suppressions", "", map[string]any{"address": "gone@example.test"})
	code, b = sfSend(agent, "a@rawhold.test", []string{"gone@example.test"}, map[string]any{"text": "https://rawhold.test/x"})
	if code != 200 || b["held"] != nil || sfStatusOf(agent, b["id"].(string), "gone@example.test") != "suppressed" {
		t.Fatalf("held but suppressed: %d %v", code, b)
	}

	code, b = agent.call("POST", "/emails/batch", "", []map[string]any{
		{"from": "a@rawhold.test", "to": "r@example.test", "subject": "s", "text": "https://rawhold.test/x"},
		{"from": "a@rawhold.test", "to": "r@example.test", "subject": "s", "text": "nothing to click"},
	})
	items, _ := b["data"].([]any)
	if code != 200 || len(items) != 2 || items[0].(map[string]any)["held"] != true || items[1].(map[string]any)["held"] != nil {
		t.Fatalf("batch: %d %v", code, b)
	}
}

// TestSafetyHeldMailExpires: undecided held mail expires at expires_at, and
// its body survives the purge until then.
func TestSafetyHeldMailExpires(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("expire.test")
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{"require_approval": map[string]any{"always": true}})

	code, b := sfSend(agent, "a@expire.test", []string{"late@example.test"}, map[string]any{
		"expires_at": time.Now().Add(1500 * time.Millisecond).UTC().Format(time.RFC3339Nano)})
	if code != 200 || b["held"] != true {
		t.Fatalf("send: %d %v", code, b)
	}
	short := b["id"].(string)
	code, b = sfSend(agent, "a@expire.test", []string{"kept@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	kept := b["id"].(string)

	// Purging bodies older than "now" keeps held ones.
	if err := e.st.Sweep(e.ctx, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if _, v := e.call("GET", "/emails/"+kept, "", nil); v["body_purged"] != false {
		t.Fatalf("held body purged: %v", v["body_purged"])
	}

	time.Sleep(1600 * time.Millisecond)
	if err := e.st.SweepSafety(e.ctx); err != nil {
		t.Fatal(err)
	}
	_, v := e.call("GET", "/emails/"+short, "", nil)
	evs := v["events"].([]any)
	last := evs[len(evs)-1].(map[string]any)
	if sfStatusOf(e.harness, short, "late@example.test") != "expired" || last["type"] != "expired" ||
		last["data"].(map[string]any)["held"] != true {
		t.Fatalf("held mail did not expire: %v", v)
	}
	if code, b := e.call("POST", "/emails/"+short+"/approve", "", nil); code != 409 || b["name"] != "not_held" {
		t.Fatalf("approve expired: %d %v", code, b)
	}
	_, l := e.call("GET", "/emails/held", "", nil)
	if d := l["data"].([]any); len(d) != 1 || d[0].(map[string]any)["id"] != kept {
		t.Fatalf("held list after expiry: %v", l)
	}

	// The kept one still goes out when approved.
	e.startWorker()
	if code, b := e.call("POST", "/emails/"+kept+"/approve", "", nil); code != 200 {
		t.Fatalf("approve: %d %v", code, b)
	}
	e.waitStatus(kept, "kept@example.test", "delivered")
}

func TestSafetySuppressionsAPI(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("sup.test")
	e.startWorker()
	agent, _ := keysNewKey(e, "agent", "sending_access", nil)

	code, b := e.call("POST", "/suppressions", "", map[string]any{"address": "Gone@Example.test", "reason": "customer_request", "detail": "=asked by phone"})
	if code != 201 || b["address"] != "gone@example.test" || b["stream"] != "all" || b["created"] != true {
		t.Fatalf("add: %d %v", code, b)
	}
	if code, b := e.call("POST", "/suppressions", "", map[string]any{"address": "gone@example.test"}); code != 200 || b["created"] != false || b["reason"] != "customer_request" {
		t.Fatalf("add again: %d %v", code, b)
	}
	for _, bad := range []map[string]any{
		{"address": "not an address"}, {"address": "x@example.test", "reason": "Bad Reason"},
		{"address": "x@example.test", "stream": "transactional"}, {"address": "x@example.test", "extra": 1},
	} {
		if code, b := e.call("POST", "/suppressions", "", bad); code != 422 {
			t.Fatalf("add %v: %d %v", bad, code, b)
		}
	}
	e.call("POST", "/suppressions", "", map[string]any{"address": "news@example.test", "stream": "broadcast", "reason": "unsubscribe"})
	e.call("POST", "/suppressions", "", map[string]any{"address": "third@other.test"})

	// Sending keys read but never change.
	if code, b := agent.call("POST", "/suppressions", "", map[string]any{"address": "y@example.test"}); code != 403 || b["name"] != "insufficient_permission" {
		t.Fatalf("agent adds: %d %v", code, b)
	}
	if code, _ := agent.call("DELETE", "/suppressions/gone@example.test", "", nil); code != 403 {
		t.Fatalf("agent deletes: %d", code)
	}
	code, l := agent.call("GET", "/suppressions", "", nil)
	if code != 200 || len(l["data"].([]any)) != 3 {
		t.Fatalf("agent lists: %d %v", code, l)
	}

	// Pagination and filters.
	var seen []string
	next := ""
	for page := 0; page < 5; page++ {
		path := "/suppressions?limit=1"
		if next != "" {
			path += "&before=" + url.QueryEscape(next)
		}
		_, p := e.call("GET", path, "", nil)
		for _, d := range p["data"].([]any) {
			seen = append(seen, d.(map[string]any)["address"].(string))
		}
		if p["has_more"] != true {
			break
		}
		next = p["next_before"].(string)
	}
	sort.Strings(seen)
	if !slices.Equal(seen, []string{"gone@example.test", "news@example.test", "third@other.test"}) {
		t.Fatalf("paged through %v", seen)
	}
	for q, want := range map[string]int{"reason=customer_request": 1, "stream=broadcast": 1, "address=GONE@example.test": 1, "domain=other.test": 1, "reason=complaint": 0} {
		if _, p := e.call("GET", "/suppressions?"+q, "", nil); len(p["data"].([]any)) != want {
			t.Fatalf("filter %s: %v", q, p)
		}
	}
	if code, _ := e.call("GET", "/suppressions?stream=nope", "", nil); code != 422 {
		t.Fatalf("bad stream filter: %d", code)
	}

	// CSV export, readable by a sending key, safe to open in a spreadsheet.
	req, _ := http.NewRequest("GET", e.ts.URL+"/suppressions/export", nil)
	req.Header.Set("Authorization", "Bearer "+agent.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export: %d %v %v", resp.StatusCode, err, rows)
	}
	if len(rows) != 4 || !slices.Equal(rows[0], []string{"address", "stream", "reason", "detail", "created_at"}) {
		t.Fatalf("export rows: %v", rows)
	}
	if rows[1][0] != "gone@example.test" || rows[1][3] != "'=asked by phone" {
		t.Fatalf("export row: %v", rows[1])
	}

	// Suppressed addresses are not sent to; broadcast-only ones still get
	// transactional mail.
	code, b = sfSend(agent, "a@sup.test", []string{"gone@example.test", "news@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	id := b["id"].(string)
	e.waitStatus(id, "gone@example.test", "suppressed")
	e.waitStatus(id, "news@example.test", "delivered")

	// Deleting one lets mail through again.
	code, b = e.call("DELETE", "/suppressions/Gone@example.test", "", nil)
	if code != 200 || b["deleted"] != true {
		t.Fatalf("delete: %d %v", code, b)
	}
	if code, b := e.call("DELETE", "/suppressions/gone@example.test", "", nil); code != 404 || b["name"] != "not_found" {
		t.Fatalf("delete again: %d %v", code, b)
	}
	if code, _ := e.call("DELETE", "/suppressions/news@example.test?stream=all", "", nil); code != 404 {
		t.Fatalf("delete the wrong stream: %d", code)
	}
	code, b = sfSend(agent, "a@sup.test", []string{"gone@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "gone@example.test", "delivered")
}

// TestSafetyBroadcastNeedsPublicURL: without OMW_PUBLIC_URL there is no
// unsubscribe link, so broadcast mail is refused.
func TestSafetyBroadcastNeedsPublicURL(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("nourl.test")
	code, b := sfSend(e.harness, "a@nourl.test", []string{"r@example.test"}, map[string]any{"stream": "broadcast"})
	if code != 422 || b["name"] != "unsubscribe_unavailable" || !strings.Contains(b["message"].(string), "OMW_PUBLIC_URL") {
		t.Fatalf("broadcast without a public URL: %d %v", code, b)
	}
	code, b = sfRaw(e.harness, "From: a@nourl.test\r\nTo: r@example.test\r\nSubject: s\r\nX-OMW-Stream: broadcast\r\n\r\nhi\r\n", nil)
	if code != 422 || b["name"] != "unsubscribe_unavailable" {
		t.Fatalf("raw broadcast without a public URL: %d %v", code, b)
	}
	if code, b := sfSend(e.harness, "a@nourl.test", []string{"r@example.test"}, nil); code != 200 {
		t.Fatalf("transactional still works: %d %v", code, b)
	}
	if _, err := config.PublicURL("ftp://x"); err == nil {
		t.Fatal("OMW_PUBLIC_URL accepted ftp://")
	}
	if u, err := config.PublicURL("https://mail.example.com/"); err != nil || u != "https://mail.example.com" {
		t.Fatalf("OMW_PUBLIC_URL: %q %v", u, err)
	}
}

func TestSafetyBroadcastUnsubscribe(t *testing.T) {
	e := newEnv(t, func(c *config.Control) { c.PublicURL = "https://omw.example.test" })
	_, dkimTXT := e.addVerifiedDomain("news.test")
	e.startWorker()
	keyID := sfKeyID(e, "e2e")

	for name, extra := range map[string]map[string]any{
		"two recipients":    {"stream": "broadcast", "to": []string{"a@example.test", "b@example.test"}},
		"a cc":              {"stream": "broadcast", "cc": []string{"b@example.test"}},
		"own unsubscribe":   {"stream": "broadcast", "headers": map[string]string{"List-Unsubscribe": "<mailto:x@news.test>"}},
		"own Feedback-ID":   {"headers": map[string]string{"feedback-id": "a:b:c:d"}},
		"an unknown stream": {"stream": "marketing"},
	} {
		body := map[string]any{"from": "n@news.test", "to": []string{"reader@example.test"}, "subject": "s", "text": "t"}
		for k, v := range extra {
			body[k] = v
		}
		if code, b := e.call("POST", "/emails", "", body); code != 422 {
			t.Fatalf("%s: %d %v", name, code, b)
		}
	}

	code, b := sfSend(e.harness, "n@news.test", []string{"reader@example.test"}, map[string]any{"stream": "broadcast", "text": "news. unsubscribe below"})
	if code != 200 {
		t.Fatalf("broadcast: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "reader@example.test", "delivered")
	raw := sfMessageTo(t, e, "reader@example.test")
	verifyDKIM(t, raw, dkimTXT)
	hdr := sfHeader(t, raw)
	lu := hdr.Get("List-Unsubscribe")
	if !strings.HasPrefix(lu, "<https://omw.example.test/u/") || !strings.HasSuffix(lu, ">") {
		t.Fatalf("List-Unsubscribe: %q", lu)
	}
	if got := hdr.Get("List-Unsubscribe-Post"); got != "List-Unsubscribe=One-Click" {
		t.Fatalf("List-Unsubscribe-Post: %q", got)
	}
	if got, want := hdr.Get("Feedback-ID"), "broadcast:"+keyID+":"+e.projectID+":omw"; got != want {
		t.Fatalf("Feedback-ID %q, want %q", got, want)
	}
	sig := strings.ToLower(strings.Join(strings.Fields(hdr.Get("DKIM-Signature")), ""))
	for _, h := range []string{"list-unsubscribe", "list-unsubscribe-post", "feedback-id"} {
		if !strings.Contains(sig, h) {
			t.Fatalf("DKIM does not sign %s: %s", h, sig)
		}
	}
	token := strings.TrimSuffix(strings.TrimPrefix(lu, "<https://omw.example.test/u/"), ">")

	// Transactional mail gets a Feedback-ID and no unsubscribe header.
	code, b = sfSend(e.harness, "n@news.test", []string{"receipt@example.test"}, nil)
	if code != 200 {
		t.Fatalf("transactional: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "receipt@example.test", "delivered")
	th := sfHeader(t, sfMessageTo(t, e, "receipt@example.test"))
	if th.Get("List-Unsubscribe") != "" || th.Get("Feedback-ID") != "transactional:"+keyID+":"+e.projectID+":omw" {
		t.Fatalf("transactional headers: %v", th)
	}

	// GET only shows a confirm page: link scanners must not unsubscribe.
	resp, err := http.Get(e.ts.URL + "/u/" + token)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!bytes.Contains(page, []byte(`<form method="post">`)) || !bytes.Contains(page, []byte("r***@example.test")) ||
		bytes.Contains(page, []byte("reader@example.test")) {
		t.Fatalf("confirm page: %d %s", resp.StatusCode, page)
	}
	if _, l := e.call("GET", "/suppressions", "", nil); len(l["data"].([]any)) != 0 {
		t.Fatalf("GET unsubscribed: %v", l)
	}

	// Forged and tampered tokens do nothing.
	tampered := []byte(token)
	tampered[len(tampered)-2] ^= 1
	for _, bad := range []string{"garbage", string(tampered), "AAAA.AAAA"} {
		resp, err := http.Post(e.ts.URL+"/u/"+bad, "application/x-www-form-urlencoded", strings.NewReader("List-Unsubscribe=One-Click"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("POST /u/%s: %d", bad, resp.StatusCode)
		}
	}

	// The RFC 8058 POST unsubscribes at once, for broadcast mail only.
	for i := 0; i < 2; i++ {
		resp, err := http.Post(e.ts.URL+"/u/"+token, "application/x-www-form-urlencoded", strings.NewReader("List-Unsubscribe=One-Click"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("one-click POST %d: %d", i, resp.StatusCode)
		}
	}
	_, l := e.call("GET", "/suppressions?address=reader@example.test", "", nil)
	d := l["data"].([]any)
	if len(d) != 1 || d[0].(map[string]any)["reason"] != "unsubscribe" || d[0].(map[string]any)["stream"] != "broadcast" {
		t.Fatalf("suppression after unsubscribe: %v", l)
	}
	code, b = sfSend(e.harness, "n@news.test", []string{"reader@example.test"}, map[string]any{"stream": "broadcast"})
	if code != 200 {
		t.Fatalf("broadcast again: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "reader@example.test", "suppressed")
	code, b = sfSend(e.harness, "n@news.test", []string{"reader@example.test"}, nil)
	if code != 200 {
		t.Fatalf("transactional after unsubscribe: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "reader@example.test", "delivered")

	// Raw messages pick the stream with X-OMW-Stream; their own unsubscribe
	// headers and Feedback-ID are replaced.
	rawMsg := "From: n@news.test\r\nTo: rawreader@example.test\r\nSubject: s\r\nX-OMW-Stream: broadcast\r\n" +
		"List-Unsubscribe: <mailto:old@news.test>\r\nFeedback-ID: fake:1:2:3\r\n\r\nhi\r\n"
	code, b = sfRaw(e.harness, rawMsg, nil)
	if code != 200 {
		t.Fatalf("raw broadcast: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "rawreader@example.test", "delivered")
	rawSent := sfMessageTo(t, e, "rawreader@example.test")
	verifyDKIM(t, rawSent, dkimTXT)
	rh := sfHeader(t, rawSent)
	if len(rh["List-Unsubscribe"]) != 1 || !strings.Contains(rh.Get("List-Unsubscribe"), "https://omw.example.test/u/") ||
		len(rh["Feedback-Id"]) != 1 || !strings.HasPrefix(rh.Get("Feedback-ID"), "broadcast:") || rh.Get("X-OMW-Stream") != "" {
		t.Fatalf("raw broadcast headers: %v", rh)
	}
	code, b = sfRaw(e.harness, "From: n@news.test\r\nTo: a@example.test, b@example.test\r\nSubject: s\r\n\r\nhi\r\n", map[string]any{"stream": "broadcast"})
	if code != 422 {
		t.Fatalf("raw broadcast to two: %d %v", code, b)
	}

	// /u/ is rate-limited per client IP.
	limited := false
	for i := 0; i < 200 && !limited; i++ {
		resp, err := http.Get(e.ts.URL + "/u/garbage")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		limited = resp.StatusCode == 429 && resp.Header.Get("Retry-After") != ""
	}
	if !limited {
		t.Fatal("200 requests to /u/ from one IP were never limited")
	}
}

func TestSafetyRecipientCap(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("cap.test")
	agent, _ := keysNewKey(e, "agent", "sending_access", nil)

	if code, b := agent.call("PATCH", "/project", "", map[string]any{"max_per_recipient_per_day": 2}); code != 403 {
		t.Fatalf("agent changes settings: %d %v", code, b)
	}
	code, p := e.call("PATCH", "/project", "", map[string]any{"max_per_recipient_per_day": 2})
	if code != 200 || p["max_per_recipient_per_day"] != float64(2) {
		t.Fatalf("set cap: %d %v", code, p)
	}
	if _, p := agent.call("GET", "/project", "", nil); p["max_per_recipient_per_day"] != float64(2) {
		t.Fatalf("agent reads settings: %v", p)
	}

	// Two emails per recipient per day, across every key of the project.
	if code, b := sfSend(e.harness, "a@cap.test", []string{"busy@example.test"}, nil); code != 200 {
		t.Fatalf("first: %d %v", code, b)
	}
	if code, b := sfSend(agent, "a@cap.test", []string{"busy@example.test"}, nil); code != 200 {
		t.Fatalf("second: %d %v", code, b)
	}
	code, hdr, b := keysCall(agent, "POST", "/emails", nil, map[string]any{"from": "a@cap.test", "to": "busy@example.test", "subject": "s", "text": "t"})
	if code != 429 || b["name"] != "recipient_rate_limited" || hdr.Get("Retry-After") == "" ||
		!strings.Contains(b["message"].(string), "busy@example.test") {
		t.Fatalf("third: %d %v %v", code, hdr, b)
	}
	if ra, _ := strconv.Atoi(hdr.Get("Retry-After")); ra < 86000 || ra > 86400 {
		t.Fatalf("Retry-After %d; the first send leaves the window in about a day", ra)
	}
	if code, b := sfSend(agent, "a@cap.test", []string{"other@example.test"}, nil); code != 200 {
		t.Fatalf("another recipient: %d %v", code, b)
	}
	if code, _ := sfRaw(agent, "From: a@cap.test\r\nTo: busy@example.test\r\nSubject: s\r\n\r\nhi\r\n", nil); code != 429 {
		t.Fatalf("raw path ignores the cap: %d", code)
	}

	// A batch is checked as a whole.
	item := map[string]any{"from": "a@cap.test", "to": "pair@example.test", "subject": "s", "text": "t"}
	if code, b := agent.call("POST", "/emails/batch", "", []map[string]any{item, item, item}); code != 429 || b["name"] != "recipient_rate_limited" {
		t.Fatalf("batch of three: %d %v", code, b)
	}
	if code, b := agent.call("POST", "/emails/batch", "", []map[string]any{item, item}); code != 200 {
		t.Fatalf("batch of two after a refused batch: %d %v", code, b)
	}

	// An idempotent replay is one send.
	for i := 0; i < 2; i++ {
		if code, b := agent.call("POST", "/emails", "same-key", map[string]any{"from": "a@cap.test", "to": "rep@example.test", "subject": "s", "text": "t"}); code != 200 {
			t.Fatalf("idempotent send %d: %d %v", i, code, b)
		}
	}
	if code, b := sfSend(agent, "a@cap.test", []string{"rep@example.test"}, nil); code != 200 {
		t.Fatalf("second real send after a replay: %d %v", code, b)
	}

	// Suppressed recipients are not sent to, so they do not count.
	e.call("POST", "/suppressions", "", map[string]any{"address": "sup@example.test"})
	for i := 0; i < 3; i++ {
		if code, b := sfSend(agent, "a@cap.test", []string{"sup@example.test"}, nil); code != 200 {
			t.Fatalf("suppressed recipient %d: %d %v", i, code, b)
		}
	}

	// Rejected held mail gives its place back.
	holder, _ := keysNewKey(e, "holder", "sending_access", map[string]any{"require_approval": map[string]any{"always": true}})
	var held []string
	for i := 0; i < 2; i++ {
		code, b := sfSend(holder, "a@cap.test", []string{"rel@example.test"}, nil)
		if code != 200 || b["held"] != true {
			t.Fatalf("held %d: %d %v", i, code, b)
		}
		held = append(held, b["id"].(string))
	}
	if code, _ := sfSend(holder, "a@cap.test", []string{"rel@example.test"}, nil); code != 429 {
		t.Fatalf("held mail does not count: %d", code)
	}
	if code, b := e.call("POST", "/emails/"+held[0]+"/reject", "", nil); code != 200 {
		t.Fatalf("reject: %d %v", code, b)
	}
	if code, b := sfSend(holder, "a@cap.test", []string{"rel@example.test"}, nil); code != 200 {
		t.Fatalf("after a rejection: %d %v", code, b)
	}

	// null turns the cap off.
	if code, p := e.call("PATCH", "/project", "", map[string]any{"max_per_recipient_per_day": nil}); code != 200 || p["max_per_recipient_per_day"] != nil {
		t.Fatalf("cap off: %d %v", code, p)
	}
	if code, b := sfSend(agent, "a@cap.test", []string{"busy@example.test"}, nil); code != 200 {
		t.Fatalf("no cap: %d %v", code, b)
	}
}

func TestSafetyProjectSettings(t *testing.T) {
	e := newEnv(t, nil)
	code, p := e.call("GET", "/project", "", nil)
	cb, _ := p["circuit_breaker"].(map[string]any)
	if code != 200 || p["id"] != e.projectID || p["max_per_recipient_per_day"] != nil || cb["enabled"] != true ||
		cb["bounce_rate"] != 0.05 || cb["complaint_rate"] != 0.001 || cb["min_recipients"] != float64(50) {
		t.Fatalf("defaults: %d %v", code, p)
	}
	for _, bad := range []map[string]any{
		{"max_per_recipient_per_day": 0}, {"max_per_recipient_per_day": "3"},
		{"circuit_breaker": map[string]any{"bounce_rate": 0}}, {"circuit_breaker": map[string]any{"complaint_rate": 1.5}},
		{"circuit_breaker": map[string]any{"min_recipients": 0}}, {"circuit_breaker": map[string]any{"other": 1}},
		{"name": "x"},
	} {
		if code, b := e.call("PATCH", "/project", "", bad); code != 422 {
			t.Fatalf("PATCH %v: %d %v", bad, code, b)
		}
	}
	code, p = e.call("PATCH", "/project", "", map[string]any{"circuit_breaker": map[string]any{"bounce_rate": 0.1}})
	cb = p["circuit_breaker"].(map[string]any)
	if code != 200 || cb["bounce_rate"] != 0.1 || cb["complaint_rate"] != 0.001 || p["updated_at"] == nil {
		t.Fatalf("partial update: %d %v", code, p)
	}
}

func TestSafetyCircuitBreaker(t *testing.T) {
	e := newEnv(t, nil)
	domID, _ := e.addVerifiedDomain("cb.test")
	rc := sfHook(e, "sending.paused")
	agent, agentID := keysNewKey(e, "agent", "sending_access", nil)
	if code, p := e.call("PATCH", "/project", "", map[string]any{"circuit_breaker": map[string]any{"min_recipients": 4, "bounce_rate": 0.25}}); code != 200 {
		t.Fatalf("thresholds: %d %v", code, p)
	}
	e.smtp.ReplyToRcpt("bad1@example.test", "550 5.1.1 no such user")
	e.smtp.ReplyToRcpt("bad2@example.test", "550 5.1.1 no such user")
	e.smtp.ReplyToRcpt("bad3@example.test", "550 5.7.1 rejected")

	// Two bounces among four recipients: 50%, over the 25% limit.
	code, b := sfSend(agent, "a@cb.test", []string{"ok1@example.test", "ok2@example.test", "bad1@example.test", "bad2@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	id := b["id"].(string)
	e.startWorker() // after the send is counted, so the bounces always come second
	e.waitStatus(id, "bad1@example.test", "bounced")
	e.waitStatus(id, "bad2@example.test", "bounced")
	e.waitStatus(id, "ok2@example.test", "delivered")

	_, l := agent.call("GET", "/pauses?active=true", "", nil)
	pauses := map[string]map[string]any{}
	for _, p := range l["data"].([]any) {
		m := p.(map[string]any)
		pauses[m["scope"].(string)] = m
	}
	if len(pauses) != 3 || pauses["api_key"]["scope_id"] != agentID || pauses["domain"]["scope_id"] != domID ||
		pauses["domain"]["scope_name"] != "cb.test" || pauses["project"]["reason"] != "bounce_rate" ||
		pauses["project"]["rate"] != 0.5 || pauses["project"]["recipients"] != float64(4) {
		t.Fatalf("pauses: %v", l)
	}
	hook := sfWaitHook(t, rc, "sending.paused", id)
	if p := hook["data"].(map[string]any)["pause"].(map[string]any); p["reason"] != "bounce_rate" || p["threshold"] != 0.25 {
		t.Fatalf("sending.paused payload: %v", hook)
	}

	// Every send from a paused scope is refused, naming the rate and the way out.
	code, b = sfSend(agent, "a@cb.test", []string{"ok1@example.test"}, nil)
	msg, _ := b["message"].(string)
	if code != 403 || b["name"] != "sending_paused" || !strings.Contains(msg, "50%") || !strings.Contains(msg, "25%") ||
		!strings.Contains(msg, "POST /pauses/") {
		t.Fatalf("paused send: %d %v", code, b)
	}
	if code, _ := sfRaw(e.harness, "From: a@cb.test\r\nTo: ok1@example.test\r\nSubject: s\r\n\r\nhi\r\n", nil); code != 403 {
		t.Fatalf("raw send while the project is paused: %d", code)
	}
	if code, _ := e.call("POST", "/emails/batch", "", []map[string]any{{"from": "a@cb.test", "to": "ok1@example.test", "subject": "s", "text": "t"}}); code != 403 {
		t.Fatalf("batch while paused: %d", code)
	}

	// Only a full_access key resumes, one scope at a time.
	if code, b := agent.call("POST", "/pauses/"+pauses["api_key"]["id"].(string)+"/resume", "", nil); code != 403 {
		t.Fatalf("agent resumes: %d %v", code, b)
	}
	if code, b := e.call("POST", "/pauses/"+pauses["project"]["id"].(string)+"/resume", "", nil); code != 200 || b["active"] != false || b["resumed_by"] == nil {
		t.Fatalf("resume project: %d %v", code, b)
	}
	code, b = sfSend(e.harness, "a@cb.test", []string{"ok1@example.test"}, nil)
	if code != 403 || !strings.Contains(b["message"].(string), "cb.test") {
		t.Fatalf("domain still paused: %d %v", code, b)
	}
	for _, s := range []string{"domain", "api_key"} {
		if code, b := e.call("POST", "/pauses/"+pauses[s]["id"].(string)+"/resume", "", nil); code != 200 {
			t.Fatalf("resume %s: %d %v", s, code, b)
		}
	}
	if code, b := e.call("POST", "/pauses/"+pauses["domain"]["id"].(string)+"/resume", "", nil); code != 409 || b["name"] != "not_paused" {
		t.Fatalf("resume twice: %d %v", code, b)
	}
	if code, _ := e.call("POST", "/pauses/00000000-0000-4000-8000-000000000000/resume", "", nil); code != 404 {
		t.Fatalf("resume unknown: %d", code)
	}

	// Resuming starts the counts over: one more bounce does not pause again.
	code, b = sfSend(agent, "a@cb.test", []string{"bad3@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send after resume: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "bad3@example.test", "bounced")
	if _, l := e.call("GET", "/pauses?active=true", "", nil); len(l["data"].([]any)) != 0 {
		t.Fatalf("paused again: %v", l)
	}

	// Complaints trip it too, wherever they are recorded.
	e.call("PATCH", "/project", "", map[string]any{"circuit_breaker": map[string]any{"min_recipients": 1, "bounce_rate": 1, "complaint_rate": 0.4}})
	code, b = sfSend(agent, "a@cb.test", []string{"c1@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	cid := b["id"].(string)
	e.waitStatus(cid, "c1@example.test", "delivered")
	var deliveryID string
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT id::text FROM deliveries WHERE email_id = $1`, cid).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.RecordComplaint(e.ctx, store.Complaint{EmailID: cid, DeliveryID: deliveryID, ProjectID: e.projectID,
		Rcpt: "c1@example.test", Data: []byte(`{"source":"arf"}`)}); err != nil {
		t.Fatal(err)
	}
	_, l = e.call("GET", "/pauses?active=true", "", nil)
	if d := l["data"].([]any); len(d) != 3 || d[0].(map[string]any)["reason"] != "complaint_rate" {
		t.Fatalf("complaint pauses: %v", l)
	}
	if code, b := sfSend(agent, "a@cb.test", []string{"ok1@example.test"}, nil); code != 403 || !strings.Contains(b["message"].(string), "complaint") {
		t.Fatalf("send after complaints: %d %v", code, b)
	}

	// With the breaker off, nothing more pauses.
	for _, p := range l["data"].([]any) {
		e.call("POST", "/pauses/"+p.(map[string]any)["id"].(string)+"/resume", "", nil)
	}
	e.call("PATCH", "/project", "", map[string]any{"circuit_breaker": map[string]any{"enabled": false, "bounce_rate": 0.01}})
	code, b = sfSend(agent, "a@cb.test", []string{"bad3@example.test"}, nil)
	if code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	e.waitStatus(b["id"].(string), "bad3@example.test", "bounced")
	if _, l := e.call("GET", "/pauses?active=true", "", nil); len(l["data"].([]any)) != 0 {
		t.Fatalf("a disabled breaker paused: %v", l)
	}
	if _, l := e.call("GET", "/pauses", "", nil); len(l["data"].([]any)) != 6 {
		t.Fatalf("pause history: %v", l)
	}
}

func TestSafetyCheckEmail(t *testing.T) {
	e := newEnv(t, func(c *config.Control) { c.PublicURL = "https://omw.example.test" })
	e.addVerifiedDomain("check.test")

	code, r := e.call("POST", "/emails/check", "", map[string]any{"from": "a@check.test", "to": "x@example.test", "subject": "s",
		"text": "hi", "html": `<p>hi <a href="https://www.check.test/x">x</a></p>`})
	if code != 200 || r["ok"] != true || r["would"] != "send" || r["stream"] != "transactional" || r["size"].(float64) < 500 {
		t.Fatalf("clean email: %d %v", code, r)
	}

	html := "<p>" + strings.Repeat("a", 110*1024) + ` <a href="https://evil.example/login">x</a> see www.other.test/page</p>`
	code, r = e.call("POST", "/emails/check", "", map[string]any{"from": "a@check.test", "to": "x@example.test", "subject": "s",
		"html": html, "stream": "broadcast"})
	codes := map[string]string{}
	for _, w := range r["warnings"].([]any) {
		m := w.(map[string]any)
		codes[m["code"].(string)] = m["message"].(string)
	}
	if code != 200 || r["ok"] != false || r["would"] != "send" || r["stream"] != "broadcast" {
		t.Fatalf("lint email: %d %v", code, r)
	}
	for _, c := range []string{"html_too_large", "link_domain_mismatch", "missing_unsubscribe"} {
		if codes[c] == "" {
			t.Fatalf("no %s warning: %v", c, r)
		}
	}
	if !strings.Contains(codes["link_domain_mismatch"], "evil.example") || !strings.Contains(codes["link_domain_mismatch"], "other.test") {
		t.Fatalf("link warning: %s", codes["link_domain_mismatch"])
	}
	// HTML alone: warned about unless the server adds a text part itself.
	built, err := message.Build(message.Input{From: "a@check.test", To: []string{"x@example.test"}, Subject: "s", HTML: "<p>x</p>", MessageID: "m@check.test"})
	if err != nil {
		t.Fatal(err)
	}
	if hasText := bytes.Contains(built, []byte("text/plain")); hasText == (codes["no_text_part"] != "") {
		t.Fatalf("no_text_part warning %q, but the built message has a text part: %v", codes["no_text_part"], hasText)
	}

	// Refusals that would apply are all reported, holds too.
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{
		"allowed_recipients": []string{"*@example.test"}, "require_approval": map[string]any{"always": true}})
	code, r = agent.call("POST", "/emails/check", "", map[string]any{"from": "a@nope.test", "to": "y@elsewhere.test", "subject": "s", "text": "t"})
	var names []string
	for _, x := range r["refusals"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if code != 200 || r["would"] != "refuse" || !slices.Contains(names, "recipient_not_allowed") || !slices.Contains(names, "domain_not_found") ||
		!slices.Equal(sfStrings(r["hold_reasons"]), []string{"always"}) {
		t.Fatalf("refused check: %d %v", code, r)
	}
	if !strings.Contains(string(mustJSON(r["warnings"])), "domain_not_verified") {
		t.Fatalf("no domain_not_verified warning: %v", r)
	}
	code, r = agent.call("POST", "/emails/check", "", map[string]any{"from": "a@check.test", "to": "y@example.test", "subject": "s", "text": "t"})
	if code != 200 || r["would"] != "hold" {
		t.Fatalf("held check: %d %v", code, r)
	}
	if code, r := e.call("POST", "/emails/check", "", map[string]any{"from": "a@check.test", "to": "y@example.test", "text": "t"}); code != 200 ||
		r["would"] != "refuse" || r["refusals"].([]any)[0].(map[string]any)["name"] != "validation_error" {
		t.Fatalf("invalid email: %d %v", code, r)
	}
	if code, _ := e.call("POST", "/emails/check", "", map[string]any{"from": "a@check.test", "bogus": 1}); code != 422 {
		t.Fatalf("unknown field: %d", code)
	}

	// The per-recipient cap shows up too.
	e.call("PATCH", "/project", "", map[string]any{"max_per_recipient_per_day": 1})
	if code, b := sfSend(e.harness, "a@check.test", []string{"once@example.test"}, nil); code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	_, r = e.call("POST", "/emails/check", "", map[string]any{"from": "a@check.test", "to": "once@example.test", "subject": "s", "text": "t"})
	if r["would"] != "refuse" || r["refusals"].([]any)[0].(map[string]any)["name"] != "recipient_rate_limited" {
		t.Fatalf("cap check: %v", r)
	}

	// Checking stored nothing.
	if _, l := e.call("GET", "/emails", "", nil); len(l["data"].([]any)) != 1 {
		t.Fatalf("check stored emails: %v", l)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func sfConnect(t *testing.T, e *env, token string) *mcp.ClientSession {
	t.Helper()
	s, err := mcp.NewClient(&mcp.Implementation{Name: "safety", Version: "1"}, nil).Connect(e.ctx,
		&mcp.StreamableClientTransport{Endpoint: e.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sfTools(t *testing.T, s *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	l, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range l.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestSafetyMCP(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("mcp.test")
	e.startWorker()
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{"require_approval": map[string]any{"first_contact": true}})
	reviewer, _ := keysNewKey(e, "reviewer", "sending_access", map[string]any{"can_approve": true})

	// A sending key without can_approve is not offered the approval tools.
	as := sfConnect(t, e, agent.apiKey)
	tools := sfTools(t, as)
	for _, name := range []string{"check_email", "list_held", "list_suppressions", "list_pauses"} {
		if tools[name] == nil {
			t.Fatalf("agent lacks %s", name)
		}
	}
	for _, name := range []string{"approve_email", "reject_email", "add_suppression"} {
		if tools[name] != nil {
			t.Fatalf("agent is offered %s", name)
		}
	}

	res := callTool(t, as, "check_email", map[string]any{"from": "a@mcp.test", "to": []string{"n1@example.test"}, "subject": "s", "text": "t"})
	if !strings.Contains(textOf(res), "held for approval (first_contact)") {
		t.Fatalf("check_email: %s", textOf(res))
	}
	res = callTool(t, as, "send_email", map[string]any{"from": "a@mcp.test", "to": []string{"n1@example.test"}, "subject": "hello", "text": "body text"})
	out, _ := json.Marshal(res.StructuredContent)
	var sent struct {
		ID   string `json:"id"`
		Held bool   `json:"held"`
	}
	json.Unmarshal(out, &sent)
	if !sent.Held || !strings.Contains(textOf(res), "held for approval") {
		t.Fatalf("send_email: %s %s", textOf(res), out)
	}
	if txt := textOf(callTool(t, as, "get_email", map[string]any{"id": sent.ID})); !strings.Contains(txt, "waiting for approval") {
		t.Fatalf("get_email: %s", txt)
	}

	// The reviewer reads and approves over remote MCP.
	rs := sfConnect(t, e, reviewer.apiKey)
	rtools := sfTools(t, rs)
	if rtools["approve_email"] == nil || rtools["reject_email"] == nil || rtools["add_suppression"] != nil {
		t.Fatalf("reviewer tools: %v", rtools)
	}
	if a := rtools["approve_email"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Fatalf("approve_email is not marked destructive: %+v", a)
	}
	if txt := textOf(callTool(t, rs, "list_held", nil)); !strings.Contains(txt, sent.ID) || !strings.Contains(txt, "body text") || !strings.Contains(txt, "first_contact") {
		t.Fatalf("list_held: %s", txt)
	}
	if txt := textOf(callTool(t, rs, "approve_email", map[string]any{"id": sent.ID, "note": "ok"})); !strings.Contains(txt, "Approved") {
		t.Fatalf("approve_email: %s", txt)
	}
	e.waitStatus(sent.ID, "n1@example.test", "delivered")

	res = callTool(t, as, "send_email", map[string]any{"from": "a@mcp.test", "to": []string{"n2@example.test"}, "subject": "s", "text": "t"})
	json.Unmarshal(mustJSON(res.StructuredContent), &sent)
	if txt := textOf(callTool(t, rs, "reject_email", map[string]any{"id": sent.ID})); !strings.Contains(txt, "Rejected") {
		t.Fatalf("reject_email: %s", txt)
	}
	if s := sfStatusOf(e.harness, sent.ID, "n2@example.test"); s != "rejected" {
		t.Fatalf("status after reject_email: %s", s)
	}
	bad, err := rs.CallTool(e.ctx, &mcp.CallToolParams{Name: "approve_email", Arguments: map[string]any{"id": sent.ID}})
	if err != nil || !bad.IsError || !strings.Contains(textOf(bad), "not_held") {
		t.Fatalf("approving a rejected email: %v %v", err, bad)
	}

	// A full-access agent (stdio MCP: access unknown, every tool offered).
	full := mcpserver.New(&mcpserver.APIClient{Base: e.ts.URL, Key: e.apiKey, HTTP: http.DefaultClient}, "test")
	cT, sT := mcp.NewInMemoryTransports()
	go full.Run(e.ctx, sT)
	fs, err := mcp.NewClient(&mcp.Implementation{Name: "full", Version: "1"}, nil).Connect(e.ctx, cT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if ft := sfTools(t, fs); ft["approve_email"] == nil || ft["add_suppression"] == nil {
		t.Fatalf("stdio tools: %v", ft)
	}
	if txt := textOf(callTool(t, fs, "add_suppression", map[string]any{"address": "stop@example.test", "reason": "customer_request"})); !strings.Contains(txt, "stop@example.test is suppressed") {
		t.Fatalf("add_suppression: %s", txt)
	}
	if txt := textOf(callTool(t, as, "list_suppressions", map[string]any{"reason": "customer_request"})); !strings.Contains(txt, "stop@example.test") {
		t.Fatalf("list_suppressions: %s", txt)
	}
	if txt := textOf(callTool(t, as, "list_pauses", map[string]any{"active_only": true})); !strings.Contains(txt, "Nothing is paused") {
		t.Fatalf("list_pauses: %s", txt)
	}
	// The stream reaches the API: without OMW_PUBLIC_URL broadcast is refused.
	bad, err = fs.CallTool(e.ctx, &mcp.CallToolParams{Name: "send_email", Arguments: map[string]any{
		"from": "a@mcp.test", "to": []string{"news@example.test"}, "subject": "s", "text": "t", "stream": "broadcast"}})
	if err != nil || !bad.IsError || !strings.Contains(textOf(bad), "unsubscribe_unavailable") {
		t.Fatalf("broadcast over MCP: %v %v", err, textOf(bad))
	}
}

// TestSafetyHeldInboxReply: a reply from an inbox follows the key's
// approval rules too, and says so.
func TestSafetyHeldInboxReply(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("desk.test")
	mx := e.startInbound()
	inbox := e.createInbox("help@desk.test")
	msg := "From: Customer <customer@example.test>\r\nTo: help@desk.test\r\nSubject: Question\r\n" +
		"Message-ID: <q1@example.test>\r\n\r\nCan you help?\r\n"
	if err := inboundSend(t, mx, "customer@example.test", []string{"help@desk.test"}, msg); err != nil {
		t.Fatal(err)
	}
	m := e.waitMessages(inbox, 1)[0].(map[string]any)
	// The test sender is not authenticated (no SPF/DKIM in the fake DNS),
	// so the message is held from agent keys until a full-access key
	// releases it; and a sending_access key reads only the inboxes its
	// policy lists.
	if code, b := e.call("POST", "/messages/"+m["id"].(string)+"/release", "", nil); code != 200 {
		t.Fatalf("release: %d %v", code, b)
	}
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{
		"require_approval": map[string]any{"always": true},
		"inboxes":          []string{"help@desk.test"},
	})
	code, b := agent.call("POST", "/messages/"+m["id"].(string)+"/reply", "", map[string]any{"text": "Sure."})
	if code != 200 || b["held"] != true || !slices.Equal(sfStrings(b["hold_reasons"]), []string{"always"}) || b["thread_id"] == nil {
		t.Fatalf("held reply: %d %v", code, b)
	}
	if s := sfStatusOf(e.harness, b["id"].(string), "customer@example.test"); s != "held" {
		t.Fatalf("reply status %q", s)
	}
}
