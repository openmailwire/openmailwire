package e2e

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/dashboard"
)

// TestDashboard checks the dashboard as the control plane mounts it, and
// the API behaviour its pages rely on.
func TestDashboard(t *testing.T) {
	e := newEnv(t, nil)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	res, err := noRedirect.Get(e.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/dashboard/" {
		t.Fatalf("/dashboard: %d, Location %q", res.StatusCode, res.Header.Get("Location"))
	}

	// The files need no API key: the page asks for one.
	for path, ctype := range map[string]string{
		"/dashboard/":           "text/html; charset=utf-8",
		"/dashboard/app.js":     "text/javascript; charset=utf-8",
		"/dashboard/styles.css": "text/css; charset=utf-8",
	} {
		res, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != ctype || len(b) == 0 {
			t.Errorf("%s: %d %q (%d bytes)", path, res.StatusCode, res.Header.Get("Content-Type"), len(b))
		}
		if res.Header.Get("Content-Security-Policy") != dashboard.CSP {
			t.Errorf("%s: CSP %q", path, res.Header.Get("Content-Security-Policy"))
		}
	}

	// Sign-in validates the key with GET /domains.
	if code, v := e.call("GET", "/domains", "", nil); code != 200 || v["object"] != "list" {
		t.Errorf("GET /domains: %d %v", code, v)
	}
	bad := &harness{t: t, st: e.st, url: e.ts.URL, apiKey: "omw_not-a-real-key"}
	if code, v := bad.call("GET", "/domains", "", nil); code != 401 || v["name"] != "invalid_api_key" {
		t.Errorf("GET /domains with a bad key: %d %v", code, v)
	}

	// Pages show "Not available on this server version" when an endpoint is
	// missing; they recognise the catch-all answer by this message.
	code, v := e.call("GET", "/no-such-endpoint", "", nil)
	if code != 404 || v["name"] != "not_found" || !strings.Contains(v["message"].(string), "no such endpoint") {
		t.Errorf("unknown endpoint: %d %v", code, v)
	}
}

// TestDashboardV06API checks the fields and error names the v0.6 pages
// rely on: lib.js maps these error codes to its messages, and app.js,
// ops.js and domains.js read these fields.
func TestDashboardV06API(t *testing.T) {
	e := newEnv(t, nil)
	domID, _ := e.addVerifiedDomain("dash.test")
	want := func(what string, code, wantCode int, v map[string]any, name string) {
		t.Helper()
		if code != wantCode || (name != "" && v["name"] != name) {
			t.Fatalf("%s: %d %v, want %d %s", what, code, v, wantCode, name)
		}
	}

	// Domains: claim expiry, report addresses, the DMARC requirement, DKIM.
	code, d := e.call("GET", "/domains/"+domID, "", nil)
	want("GET domain", code, 200, d, "")
	if _, ok := d["claim_expires_at"]; !ok {
		t.Error("domain has no claim_expires_at")
	}
	if ra, _ := d["report_addresses"].(map[string]any); ra == nil || ra["dmarc_record_example"] == "" || ra["tls_rpt_record"] == nil {
		t.Errorf("report_addresses: %v", d["report_addresses"])
	}
	dmarc := false
	for _, r := range d["records"].([]any) {
		if rec := r.(map[string]any); rec["record"] == "DMARC" && rec["required"] == true {
			dmarc = true
		}
	}
	if !dmarc {
		t.Error("the DMARC record is not required")
	}
	code, dk := e.call("GET", "/domains/"+domID+"/dkim", "", nil)
	want("GET dkim", code, 200, dk, "")
	if k := dk["keys"].([]any)[0].(map[string]any); k["algorithm"] != "rsa" || k["state"] != "active" || k["signs"] != true || k["record"] == nil {
		t.Errorf("current DKIM key: %v", k)
	}
	if _, ok := dk["auth_issues"].([]any); !ok {
		t.Errorf("auth_issues: %v", dk["auth_issues"])
	}
	code, rot := e.call("POST", "/domains/"+domID+"/dkim/rotate", "", nil)
	want("rotate, step 1", code, 202, rot, "")
	if rot["status"] != "pending" || len(rot["keys"].([]any)) != 2 || rot["message"] == "" {
		t.Errorf("rotate, step 1: %v", rot)
	}
	code, rot = e.call("POST", "/domains/"+domID+"/dkim/rotate", "", nil)
	want("rotate, step 2 before DNS", code, 409, rot, "dkim_record_not_found")
	code, ed := e.call("POST", "/domains/"+domID+"/dkim/ed25519", "", map[string]any{"enabled": true})
	want("Ed25519 on", code, 202, ed, "")
	code, ed = e.call("POST", "/domains/"+domID+"/dkim/ed25519", "", map[string]any{"enabled": false})
	if code != 200 || ed["status"] != "disabled" {
		t.Errorf("Ed25519 off: %d %v", code, ed)
	}

	// Approval holds.
	agent, agentID := keysNewKey(e, "agent", "sending_access", map[string]any{
		"require_approval": map[string]any{"always": true}, "can_approve": true,
	})
	plain, _ := keysNewKey(e, "plain", "sending_access", nil)
	code, sent := keysSend(agent, "a@dash.test", "jane@example.test")
	if code != 200 || sent["held"] != true {
		t.Fatalf("held send: %d %v", code, sent)
	}
	id := sent["id"].(string)
	code, held := e.call("GET", "/emails/held?limit=50", "", nil)
	want("GET held", code, 200, held, "")
	if _, ok := held["next_before"]; !ok || held["has_more"] != false {
		t.Errorf("held list: %v", held)
	}
	h0 := held["data"].([]any)[0].(map[string]any)
	if h0["id"] != id || h0["api_key_id"] != agentID || h0["expires_at"] == nil || h0["text"] != "t" ||
		len(h0["hold_reasons"].([]any)) != 1 || len(h0["recipients"].([]any)) != 1 {
		t.Errorf("held email: %v", h0)
	}
	code, v := agent.call("POST", "/emails/"+id+"/approve", "", map[string]any{"note": "mine"})
	want("approve own", code, 403, v, "cannot_approve_own_email")
	code, v = plain.call("POST", "/emails/"+id+"/approve", "", nil)
	want("approve without can_approve", code, 403, v, "insufficient_permission")
	code, v = e.call("POST", "/emails/"+id+"/reject", "", map[string]any{"note": "not now"})
	if code != 200 || v["decision"] != "rejected" || len(v["recipients"].([]any)) != 1 || v["suppressed"] == nil {
		t.Errorf("reject: %d %v", code, v)
	}
	code, v = e.call("POST", "/emails/"+id+"/approve", "", nil)
	want("approve a rejected email", code, 409, v, "not_held")

	// Check: a verdict, nothing sent.
	code, chk := agent.call("POST", "/emails/check", "", map[string]any{
		"from": "a@dash.test", "to": []string{"x@example.test"}, "subject": "s", "text": "t", "stream": "transactional",
	})
	if code != 200 || chk["would"] != "hold" || chk["refusals"] == nil || chk["warnings"] == nil || chk["size"] == nil {
		t.Errorf("check: %d %v", code, chk)
	}

	// Sending keys read these pages, and get insufficient_permission for
	// the ones that need full access.
	for _, p := range []string{"/suppressions", "/pauses", "/project", "/emails/held", "/domains/" + domID + "/dkim"} {
		if code, v := plain.call("GET", p, "", nil); code != 200 {
			t.Errorf("sending key GET %s: %d %v", p, code, v)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/insights", nil}, {"GET", "/audit-log", nil}, {"GET", "/audit-log/verify", nil},
		{"PATCH", "/project", map[string]any{"max_per_recipient_per_day": 3}},
		{"POST", "/suppressions", map[string]any{"address": "x@example.test"}},
		{"DELETE", "/suppressions/x@example.test", nil},
		{"POST", "/pauses/" + id + "/resume", nil},
		{"GET", "/domains/" + domID + "/reports", nil},
		{"POST", "/domains/" + domID + "/dkim/rotate", nil},
		{"POST", "/messages/" + id + "/release", nil},
		{"PATCH", "/inboxes/" + id, map[string]any{"hold_unauthenticated": false}},
		{"POST", "/webhooks/" + id + "/rotate-secret", nil},
	} {
		code, v := plain.call(c.method, c.path, "", c.body)
		want("sending key "+c.method+" "+c.path, code, 403, v, "insufficient_permission")
	}

	// Project settings: fractions, a null cap, partial changes.
	code, p := e.call("PATCH", "/project", "", map[string]any{
		"max_per_recipient_per_day": 3, "circuit_breaker": map[string]any{"bounce_rate": 0.03},
	})
	cb, _ := p["circuit_breaker"].(map[string]any)
	if code != 200 || p["max_per_recipient_per_day"] != float64(3) || cb["bounce_rate"] != 0.03 || cb["complaint_rate"] != 0.001 || cb["min_recipients"] != float64(50) {
		t.Errorf("PATCH project: %d %v", code, p)
	}
	code, p = e.call("PATCH", "/project", "", map[string]any{"max_per_recipient_per_day": nil})
	if code != 200 || p["max_per_recipient_per_day"] != nil {
		t.Errorf("PATCH project, no cap: %d %v", code, p)
	}

	// Suppressions: add, filter, export as CSV, remove one stream.
	add := map[string]any{"address": "bob@example.test", "stream": "broadcast", "reason": "customer_request", "detail": "asked"}
	code, v = e.call("POST", "/suppressions", "", add)
	if code != 201 || v["created"] != true {
		t.Errorf("add suppression: %d %v", code, v)
	}
	code, v = e.call("POST", "/suppressions", "", add)
	if code != 200 || v["created"] != false {
		t.Errorf("add it again: %d %v", code, v)
	}
	code, v = e.call("GET", "/suppressions?limit=50&domain=example.test&stream=broadcast&reason=customer_request", "", nil)
	if code != 200 || len(v["data"].([]any)) != 1 || v["data"].([]any)[0].(map[string]any)["address"] != "bob@example.test" {
		t.Errorf("filtered suppressions: %d %v", code, v)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/suppressions/export?domain=example.test", nil)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	csv, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(string(csv), "address,stream,reason,detail,created_at\n") || !strings.Contains(string(csv), "bob@example.test,broadcast") {
		t.Errorf("export: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), csv)
	}
	code, v = e.call("DELETE", "/suppressions/bob@example.test?stream=broadcast", "", nil)
	if code != 200 || len(v["removed"].([]any)) != 1 {
		t.Errorf("remove suppression: %d %v", code, v)
	}
	if code, v := e.call("GET", "/pauses?limit=100", "", nil); code != 200 || v["data"] == nil {
		t.Errorf("pauses: %d %v", code, v)
	}

	// API keys: the edit form's PATCH round-trips.
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	code, k := e.call("PATCH", "/api-keys/"+agentID, "", map[string]any{
		"name": "agent-2",
		"policy": map[string]any{
			"require_approval": map[string]any{"first_contact": true, "recipients_over": 0},
			"can_approve":      true, "inboxes": []string{"support@dash.test"},
		},
		"allowed_ips": []string{"127.0.0.1", "::1"}, "expires_at": exp,
	})
	want("PATCH key", code, 200, k, "")
	code, k = e.call("GET", "/api-keys/"+agentID, "", nil)
	pol, _ := k["policy"].(map[string]any)
	ra, _ := pol["require_approval"].(map[string]any)
	gotExp, _ := time.Parse(time.RFC3339, fmt.Sprint(k["expires_at"]))
	wantExp, _ := time.Parse(time.RFC3339, exp)
	if code != 200 || k["name"] != "agent-2" || len(k["allowed_ips"].([]any)) != 2 || !gotExp.Equal(wantExp) ||
		ra["recipients_over"] != float64(0) || ra["first_contact"] != true || pol["can_approve"] != true || len(pol["inboxes"].([]any)) != 1 {
		t.Errorf("GET key after PATCH: %d %v", code, k)
	}
	code, k = e.call("PATCH", "/api-keys/"+agentID, "", map[string]any{"allowed_ips": nil, "expires_at": nil})
	if code != 200 || k["allowed_ips"] != nil || k["expires_at"] != nil {
		t.Errorf("clear allowed_ips and expires_at: %d %v", code, k)
	}

	// Inboxes: hold_unauthenticated is on by default and toggles.
	code, in := e.call("POST", "/inboxes", "", map[string]any{"address": "support@dash.test"})
	want("POST inbox", code, 201, in, "")
	inboxID := in["id"].(string)
	if code, in := e.call("GET", "/inboxes/"+inboxID, "", nil); code != 200 || in["hold_unauthenticated"] != true {
		t.Errorf("GET inbox: %d %v", code, in)
	}
	if code, in := e.call("PATCH", "/inboxes/"+inboxID, "", map[string]any{"hold_unauthenticated": false}); code != 200 || in["hold_unauthenticated"] != false {
		t.Errorf("PATCH inbox: %d %v", code, in)
	}
	// A missing message is "not found", not a missing endpoint.
	code, v = e.call("POST", "/messages/"+id+"/release", "", nil)
	if code != 404 || v["name"] != "not_found" || strings.Contains(v["message"].(string), "no such endpoint") {
		t.Errorf("release a missing message: %d %v", code, v)
	}

	// Webhooks: Ed25519, the breaker fields, rotation with an overlap.
	code, w := e.call("POST", "/webhooks", "", map[string]any{"endpoint": "http://127.0.0.1:9/hook", "asymmetric_signing": true})
	if code != 201 || !strings.HasPrefix(w["public_key"].(string), "whpk_") || !strings.HasPrefix(w["signing_secret"].(string), "whsec_") || w["asymmetric_signing"] != true {
		t.Fatalf("create webhook: %d %v", code, w)
	}
	code, wl := e.call("GET", "/webhooks", "", nil)
	if w0 := wl["data"].([]any)[0].(map[string]any); code != 200 || w0["consecutive_failures"] != float64(0) || w0["signing_secret"] != nil {
		t.Errorf("list webhooks: %d %v", code, wl)
	}
	code, rw := e.call("POST", "/webhooks/"+w["id"].(string)+"/rotate-secret", "", map[string]any{"overlap_seconds": 3600})
	if code != 200 || rw["signing_secret"] == w["signing_secret"] || rw["previous_secret_expires_at"] == nil || rw["previous_public_key"] != w["public_key"] {
		t.Errorf("rotate secret: %d %v", code, rw)
	}

	// Insights and the audit log.
	code, ins := e.call("GET", "/insights", "", nil)
	if code != 200 || len(ins["windows"].([]any)) != 3 {
		t.Fatalf("insights: %d %v", code, ins)
	}
	for i, name := range []string{"24h", "7d", "30d"} {
		if w := ins["windows"].([]any)[i].(map[string]any); w["window"] != name || w["total"] == nil || w["by_provider"] == nil || w["by_sending_ip"] == nil {
			t.Errorf("insights window %d: %v", i, w)
		}
	}
	code, al := e.call("GET", "/audit-log?limit=50&action=api_key.updated", "", nil)
	if code != 200 || len(al["data"].([]any)) != 2 {
		t.Fatalf("audit log: %d %v", code, al)
	}
	for _, x := range al["data"].([]any) {
		en := x.(map[string]any)
		if en["action"] != "api_key.updated" || en["actor"] != "api_key" || en["hash"] == "" || en["prev_hash"] == "" || en["entry"] == "" {
			t.Errorf("audit entry: %v", en)
		}
	}
	if code, ver := e.call("GET", "/audit-log/verify", "", nil); code != 200 || ver["valid"] != true || ver["head_hash"] == "" {
		t.Errorf("verify: %d %v", code, ver)
	}
}
