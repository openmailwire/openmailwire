package e2e

// End-to-end tests for v0.6 lane A: API correctness and hardening. Each
// "Audit R…" test reproduces a finding of the v0.5.0 audit and failed
// before its fix.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/config"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// storedRaw returns the signed message stored for an email.
func (e *env) storedRaw(id string) []byte {
	e.t.Helper()
	var sealed []byte
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT raw FROM emails WHERE id = $1`, id).Scan(&sealed); err != nil {
		e.t.Fatal(err)
	}
	raw, err := e.sealer.Open(sealed)
	if err != nil {
		e.t.Fatal(err)
	}
	return raw
}

// publish adds the TXT records of a domain view to the fake DNS, once each.
func (e *env) publish(dom map[string]any) {
	for _, r := range dom["records"].([]any) {
		rec := r.(map[string]any)
		n, v := rec["name"].(string), rec["value"].(string)
		if rec["type"] != "TXT" || strings.Contains(strings.Join(e.dns.txt[n], "\n"), v) {
			continue
		}
		e.dns.txt[n] = append(e.dns.txt[n], v)
	}
}

func longest(raw []byte) int {
	n := 0
	for _, l := range bytes.Split(raw, []byte("\r\n")) {
		n = max(n, len(l))
	}
	return n
}

// Audit R1#3: a second From written "From :" (whitespace before the colon)
// was invisible to the raw path's checks, so a key limited to one sender
// queued and DKIM-signed mail from another.
func TestAuditR1RawSmuggledFrom(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("raw.test")
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{"allowed_from": []string{"agent@raw.test"}})
	for name, raw := range map[string]string{
		"From with a space before the colon":     "From: agent@raw.test\r\nTo: x@example.test\r\nSubject: s\r\nFrom : CEO <ceo@bank.example>\r\n\r\nbody\r\n",
		"From with a tab before the colon":       "From: agent@raw.test\r\nTo: x@example.test\r\nFrom\t: ceo@raw.test\r\n\r\nbody\r\n",
		"a second from in lowercase":             "From: agent@raw.test\r\nfrom: ceo@raw.test\r\nTo: x@example.test\r\n\r\nbody\r\n",
		"a second FROM in capitals":              "From: agent@raw.test\r\nTo: x@example.test\r\nFROM: ceo@raw.test\r\n\r\nbody\r\n",
		"a second Subject":                       "From: agent@raw.test\r\nTo: x@example.test\r\nSubject: a\r\nSubject: b\r\n\r\nbody\r\n",
		"a To with a space before the colon":     "From: agent@raw.test\r\nTo : x@example.test\r\n\r\nbody\r\n",
		"a header name with a control character": "From: agent@raw.test\r\nX-\x01: y\r\nTo: x@example.test\r\n\r\nbody\r\n",
	} {
		code, _, b := keysCall(agent, "POST", "/emails/raw", nil, map[string]any{"raw": b64(raw)})
		if code != 422 || b["name"] != "validation_error" {
			t.Errorf("%s: %d %v", name, code, b)
		}
	}
	// A second To is a recipient too, checked like the first.
	limited, _ := keysNewKey(e, "limited", "sending_access", map[string]any{"allowed_recipients": []string{"*@example.test"}})
	code, _, b := keysCall(limited, "POST", "/emails/raw", nil, map[string]any{
		"raw": b64("From: a@raw.test\r\nTo: x@example.test\r\nto: boss@elsewhere.test\r\n\r\nbody\r\n")})
	if code != 403 || b["name"] != "recipient_not_allowed" {
		t.Fatalf("second to: %d %v", code, b)
	}
	// The plain message still goes.
	code, _, b = keysCall(agent, "POST", "/emails/raw", nil, map[string]any{
		"raw": b64("From: agent@raw.test\r\nTo: x@example.test\r\nSubject: s\r\n\r\nbody\r\n")})
	if code != 200 {
		t.Fatalf("plain: %d %v", code, b)
	}
}

// Audit R1#4: domain names were unique across all projects, so any project
// could squat a name forever, and the 409 told it which names exist.
func TestAuditR1DomainNamesPerProject(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("victim.test")
	other := e.otherProject()

	// No oracle: another project adds the name like any other.
	code, b := other.call("POST", "/domains", "", map[string]any{"name": "victim.test"})
	if code != 201 || b["status"] != "pending" || b["claim_expires_at"] == nil {
		t.Fatalf("other project adds victim.test: %d %v", code, b)
	}
	exp, _ := time.Parse(time.RFC3339, b["claim_expires_at"].(string))
	if d := time.Until(exp); d < 6*24*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("claim expires in %s", d)
	}
	// It cannot verify a name another project has verified, even with DNS.
	e.publish(b)
	victimClaim := b["id"].(string)
	code, b = other.call("POST", "/domains/"+victimClaim+"/verify", "", nil)
	if code != 409 || b["name"] != "domain_verified_elsewhere" {
		t.Fatalf("verify a name verified elsewhere: %d %v", code, b)
	}
	// Squatting: another project claims a name first; the real owner adds
	// and verifies it anyway.
	code, squat := other.call("POST", "/domains", "", map[string]any{"name": "newco.test"})
	if code != 201 {
		t.Fatal(code, squat)
	}
	id, _ := e.addVerifiedDomain("newco.test")
	code, b = e.call("GET", "/domains/"+id, "", nil)
	if code != 200 || b["status"] != "verified" || b["claim_expires_at"] != nil {
		t.Fatalf("owner's newco.test: %d %v", code, b)
	}
	// Within a project, a name is still added once.
	if code, b = e.call("POST", "/domains", "", map[string]any{"name": "newco.test"}); code != 409 || b["name"] != "domain_exists" {
		t.Fatalf("same project twice: %d %v", code, b)
	}
	// The squatter can never send from it.
	if code, b = other.call("POST", "/emails", "", map[string]any{"from": "ceo@newco.test", "to": "x@example.test", "subject": "s", "text": "t"}); code != 403 {
		t.Fatalf("squatter sends: %d %v", code, b)
	}

	// Claims expire: an unverified claim past its time, and a failing claim
	// on a name another project has verified.
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE domains SET claim_expires_at = now() - interval '1 minute' WHERE id = $1`, squat["id"]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE domains SET claim_expires_at = NULL, checked_at = now() - interval '8 days' WHERE id = $1`, victimClaim); err != nil {
		t.Fatal(err)
	}
	expired, err := e.st.ExpireDomainClaims(e.ctx)
	if err != nil || len(expired) != 2 {
		t.Fatalf("expired %v %v", expired, err)
	}
	if _, list := other.call("GET", "/domains", "", nil); len(list["data"].([]any)) != 0 {
		t.Fatalf("other project still lists %v", list["data"])
	}
	if expired, _ := e.st.ExpireDomainClaims(e.ctx); len(expired) != 0 {
		t.Fatalf("verified domains expired: %v", expired)
	}

	// Development force-verify needs the project when several have the name.
	other.call("POST", "/domains", "", map[string]any{"name": "shared.test"})
	e.call("POST", "/domains", "", map[string]any{"name": "shared.test"})
	if err := e.st.ForceVerify(e.ctx, "shared.test", ""); err == nil {
		t.Fatal("force-verify of a name two projects have, without a project")
	}
	if err := e.st.ForceVerify(e.ctx, "shared.test", e.projectID); err != nil {
		t.Fatal(err)
	}
}

// Audit R1#6: idempotency keys were per project and checked before the
// key's policy: one key could probe, replay and pre-claim another's.
func TestAuditR1IdempotencyPerKey(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("idem.test")
	a, _ := keysNewKey(e, "a", "sending_access", nil)
	b, _ := keysNewKey(e, "b", "sending_access", map[string]any{"allowed_recipients": []string{"*@b-only.test"}})
	body := map[string]any{"from": "billing@idem.test", "to": "customer@example.test", "subject": "Invoice 42", "text": "t"}
	code, _, sent := keysCall(a, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-42"}, body)
	if code != 200 {
		t.Fatal(code, sent)
	}
	// No oracle: a key a used and one nobody used answer alike.
	c1, _, o1 := keysCall(b, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-42"}, []byte(`{}`))
	c2, _, o2 := keysCall(b, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-43"}, []byte(`{}`))
	if c1 != c2 || o1["name"] != o2["name"] {
		t.Fatalf("used key %d %v, unused key %d %v", c1, o1["name"], c2, o2["name"])
	}
	// No replay of a's email: b's policy applies to b's request.
	c3, h3, o3 := keysCall(b, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-42"}, body)
	if c3 != 403 || o3["name"] != "recipient_not_allowed" || h3.Get("Idempotent-Replayed") != "" {
		t.Fatalf("b replays a's request: %d %v", c3, o3)
	}
	// No pre-claim: b's use of a key does not block a.
	if code, _, o := keysCall(b, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-44"},
		map[string]any{"from": "billing@idem.test", "to": "x@b-only.test", "subject": "s", "text": "t"}); code != 200 {
		t.Fatal(code, o)
	}
	c4, h4, o4 := keysCall(a, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-44"},
		map[string]any{"from": "billing@idem.test", "to": "customer@example.test", "subject": "Invoice 44", "text": "t"})
	if c4 != 200 || h4.Get("Idempotent-Replayed") != "" {
		t.Fatalf("a's send after b used its key: %d %v", c4, o4)
	}
	// A key's own retry still replays.
	code, h, again := keysCall(a, "POST", "/emails", map[string]string{"Idempotency-Key": "invoice-42"}, body)
	if code != 200 || again["id"] != sent["id"] || h.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("a's retry: %d %v", code, again)
	}
	// Keys are printable ASCII.
	if code, _, o := keysCall(a, "POST", "/emails", map[string]string{"Idempotency-Key": "caf\u00e9"}, body); code != 422 {
		t.Fatalf("non-ASCII key: %d %v", code, o)
	}
}

// Audit R1#7: a batch item's key "<key>:<index>" collided with a single send
// using that literal key.
func TestAuditR1BatchIdempotencyNamespace(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("batch.test")
	item := map[string]any{"from": "a@batch.test", "to": "x@example.test", "subject": "s", "text": "t"}
	code, _, out := keysCall(e.harness, "POST", "/emails/batch", map[string]string{"Idempotency-Key": "order-7"}, []any{item, item})
	if code != 200 {
		t.Fatal(code, out)
	}
	code, h, o := keysCall(e.harness, "POST", "/emails", map[string]string{"Idempotency-Key": "order-7:0"},
		map[string]any{"from": "a@batch.test", "to": "y@example.test", "subject": "other", "text": "t"})
	if code != 200 || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("single send with key order-7:0 after batch order-7: %d %v", code, o)
	}
	code, h, again := keysCall(e.harness, "POST", "/emails/batch", map[string]string{"Idempotency-Key": "order-7"}, []any{item, item})
	if code != 200 || h.Get("Idempotent-Replayed") != "true" || again["data"].([]any)[1].(map[string]any)["id"] != out["data"].([]any)[1].(map[string]any)["id"] {
		t.Fatalf("batch replay: %d %v", code, again)
	}
}

// Audit R1#8: text PostgreSQL cannot store answered 500 (and 451 over SMTP,
// so clients retried forever). It must be 422 on every path.
func TestAuditR1UnstorableTextIs422(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("nul.test")
	for _, c := range []struct {
		name string
		path string
		body any
	}{
		{"json subject with NUL", "/emails", map[string]any{"from": "a@nul.test", "to": "x@example.test", "subject": "hi\x00", "text": "t"}},
		{"json from name with NUL", "/emails", map[string]any{"from": "\"A\x00B\" <a@nul.test>", "to": "x@example.test", "subject": "s", "text": "t"}},
		{"json to name with NUL", "/emails", map[string]any{"from": "a@nul.test", "to": "\"X\x00\" <x@example.test>", "subject": "s", "text": "t"}},
		{"json header value with NUL", "/emails", map[string]any{"from": "a@nul.test", "to": "x@example.test", "subject": "s", "text": "t", "headers": map[string]string{"X-A": "a\x00"}}},
		{"batch subject with NUL", "/emails/batch", []any{map[string]any{"from": "a@nul.test", "to": "x@example.test", "subject": "\x00", "text": "t"}}},
		{"raw encoded-word -> invalid UTF-8", "/emails/raw", map[string]any{"raw": b64("From: a@nul.test\r\nTo: x@example.test\r\nSubject: =?utf-8?q?caf=E9?=\r\n\r\nb\r\n")}},
		{"raw 8-bit Latin-1 subject", "/emails/raw", map[string]any{"raw": b64("From: a@nul.test\r\nTo: x@example.test\r\nSubject: caf\xe9\r\n\r\nb\r\n")}},
		{"raw encoded-word -> NUL", "/emails/raw", map[string]any{"raw": b64("From: a@nul.test\r\nTo: x@example.test\r\nSubject: =?utf-8?q?a=00b?=\r\n\r\nb\r\n")}},
		{"raw from name -> invalid UTF-8", "/emails/raw", map[string]any{"raw": b64("From: =?utf-8?q?A=FF?= <a@nul.test>\r\nTo: x@example.test\r\n\r\nb\r\n")}},
		{"raw Latin-1 Message-ID", "/emails/raw", map[string]any{"raw": b64("From: a@nul.test\r\nTo: x@example.test\r\nMessage-ID: <caf\xe9@nul.test>\r\n\r\nb\r\n")}},
		{"api key name with NUL", "/api-keys", map[string]any{"name": "agent\x00"}},
	} {
		code, _, o := keysCall(e.harness, "POST", c.path, nil, c.body)
		if code != 422 || o["name"] != "validation_error" {
			t.Errorf("%s: %d %v", c.name, code, o)
		}
	}
	// The safety net: whatever the database refuses as text is the caller's
	// 422, from any handler.
	var n int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM emails`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d emails stored", n)
	}
}

// Audit R1#9: the JSON path wrote header lines over 998 bytes (a 998-byte
// subject made a 1,007-byte line; a 2,000-character display name, more),
// which the raw path itself refuses.
func TestAuditR1LongHeaderLines(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("long.test")
	subject := strings.Repeat("a", 998)
	code, out := e.call("POST", "/emails", "", map[string]any{"from": strings.Repeat("N", 256) + " <a@long.test>",
		"to": []string{strings.Repeat("ب", 250) + " <x@example.test>", "y@example.test"}, "subject": subject, "text": "t"})
	if code != 200 {
		t.Fatal(code, out)
	}
	raw := e.storedRaw(out["id"].(string))
	if n := longest(raw); n > 998 {
		t.Fatalf("a %d-byte line in\n%s", n, raw)
	}
	// The stored message passes the raw path's own checks.
	if code, _, o := keysCall(e.harness, "POST", "/emails/raw", nil, map[string]any{"raw": base64.StdEncoding.EncodeToString(raw)}); code != 200 {
		t.Fatalf("resubmitting the stored message: %d %v", code, o)
	}
	_, v := e.call("GET", "/emails/"+out["id"].(string), "", nil)
	if v["subject"] != subject {
		t.Fatalf("subject stored as %q", v["subject"])
	}
	// What cannot fit is refused with the reason.
	for _, c := range []struct{ name, want string }{
		{strings.Repeat("N", 2000) + " <a@long.test>", "display name"},
		{"a@" + strings.Repeat("d", 250) + ".test", "longer than 254"},
	} {
		code, o := e.call("POST", "/emails", "", map[string]any{"from": c.name, "to": "x@example.test", "subject": "s", "text": "t"})
		if code != 422 || !strings.Contains(o["message"].(string), c.want) {
			t.Errorf("from %.20q…: %d %v", c.name, code, o)
		}
	}
	code, o := e.call("POST", "/emails", "", map[string]any{"from": "a@long.test", "to": "x@example.test", "subject": "s", "text": "t",
		"attachments": []map[string]string{{"filename": "a.pdf", "content": b64("x"), "content_type": "application/pdf; x=\"" + strings.Repeat("n", 1100) + "\""}}})
	if code != 422 || !strings.Contains(o["message"].(string), "content_type") {
		t.Fatalf("long content_type: %d %v", code, o)
	}
}

// Audit R1#5: one 13 MB send allocated about 363 MB, with no bound on how
// many ran at once. Copies are cut (one buffer per step), large bodies
// need a slot, and each key has a request rate.
func TestAuditR1SendMemory(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("mem.test")
	data := make([]byte, 10<<20-1024)
	for i := range data {
		data[i] = byte(i * 7)
	}
	body, _ := json.Marshal(map[string]any{"from": "a@mem.test", "to": "x@example.test", "subject": "s", "text": "t",
		"attachments": []map[string]string{{"filename": "a.bin", "content": base64.StdEncoding.EncodeToString(data)}}})
	data = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	code, _, o := keysCall(e.harness, "POST", "/emails", nil, body)
	runtime.ReadMemStats(&after)
	if code != 200 {
		t.Fatal(code, o)
	}
	mb := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
	t.Logf("a %.1f MB send allocated %.0f MB in total", float64(len(body))/(1<<20), mb)
	// Before: about 363 MB. The body, the decoded file, the message, its
	// DKIM signing (two buffers) and the sealed copy are about 90 MB; the
	// database driver's copy and the test client's own buffers add more.
	if mb > 200 {
		t.Fatalf("allocated %.0f MB for one send", mb)
	}
}

func TestPerKeyRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Control) { c.KeyRate, c.KeyBurst = 1, 5 })
	other, _ := keysNewKey(e, "other", "", nil) // the first of e's 5
	for i := 0; i < 4; i++ {
		if code, _ := e.call("GET", "/domains", "", nil); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	code, h, o := keysCall(e.harness, "GET", "/domains", nil, nil)
	if code != 429 || o["name"] != "rate_limit_exceeded" || h.Get("Retry-After") != "1" {
		t.Fatalf("over the rate: %d %v Retry-After %q", code, o, h.Get("Retry-After"))
	}
	// Another key has its own bucket.
	if code, _ := other.call("GET", "/domains", "", nil); code != 200 {
		t.Fatalf("other key: %d", code)
	}
	time.Sleep(1100 * time.Millisecond)
	if code, _ := e.call("GET", "/domains", "", nil); code != 200 {
		t.Fatalf("after a second: %d", code)
	}
	// Remote MCP works under the limit: its in-process API calls are not
	// counted again.
	time.Sleep(5 * time.Second)
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(e.ctx,
		&mcp.StreamableClientTransport{Endpoint: e.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{other.apiKey}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	callTool(t, sess, "list_domains", map[string]any{})
}

// API keys gain allowed_ips (CIDR) and expires_at.
func TestAPIKeyIPsAndExpiry(t *testing.T) {
	e := newEnv(t, func(c *config.Control) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")} })
	e.addVerifiedDomain("ip.test")
	code, k := e.call("POST", "/api-keys", "", map[string]any{"name": "office", "permission": "sending_access",
		"allowed_ips": []string{"203.0.113.9/24", "2001:db8::1"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if code != 201 || strings.Join(toStrings(k["allowed_ips"]), ",") != "203.0.113.0/24,2001:db8::1/128" || k["expires_at"] == nil {
		t.Fatalf("create: %d %v", code, k)
	}
	office := &harness{t: t, st: e.st, url: e.ts.URL, apiKey: k["token"].(string)}
	id := k["id"].(string)

	// The test client connects from 127.0.0.1, a trusted proxy here, so the
	// client is the right-most untrusted X-Forwarded-For address.
	code, _, o := keysCall(office, "GET", "/domains", nil, nil)
	if code != 403 || o["name"] != "ip_not_allowed" || !strings.Contains(o["message"].(string), "127.0.0.1") {
		t.Fatalf("from 127.0.0.1: %d %v", code, o)
	}
	for xff, want := range map[string]int{
		"203.0.113.50":               200,
		"198.51.100.1, 203.0.113.50": 200, // the left-most is the client's claim; the proxy appended .50
		"203.0.113.50, 198.51.100.1": 403,
		"2001:db8::1":                200,
		"203.0.113.50, 127.0.0.1":    200, // trusted hops are skipped
		"garbage":                    403,
	} {
		if code, _, o := keysCall(office, "GET", "/domains", map[string]string{"X-Forwarded-For": xff}, nil); code != want {
			t.Errorf("X-Forwarded-For %q: %d %v", xff, code, o)
		}
	}
	// Remote MCP tools act from the caller's address too.
	tr := &http.Client{Transport: headerTransport{key: office.apiKey, xff: "203.0.113.50"}}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(e.ctx,
		&mcp.StreamableClientTransport{Endpoint: e.ts.URL + "/mcp", HTTPClient: tr}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := textOf(callTool(t, sess, "list_domains", map[string]any{})); !strings.Contains(got, "ip.test") {
		t.Fatalf("list_domains: %s", got)
	}
	sess.Close()

	// Validation.
	for _, bad := range []map[string]any{
		{"name": "x", "allowed_ips": []string{}},
		{"name": "x", "allowed_ips": []string{"not-an-ip"}},
		{"name": "x", "allowed_ips": "10.0.0.0/8"},
		{"name": "x", "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		{"name": "x", "expires_at": "tomorrow"},
	} {
		if code, o := e.call("POST", "/api-keys", "", bad); code != 422 {
			t.Errorf("%v: %d %v", bad, code, o)
		}
	}
	// PATCH null removes the restriction.
	code, k = e.call("PATCH", "/api-keys/"+id, "", map[string]any{"allowed_ips": nil})
	if code != 200 || k["allowed_ips"] != nil || k["expires_at"] == nil {
		t.Fatalf("patch: %d %v", code, k)
	}
	if code, _ := office.call("GET", "/domains", "", nil); code != 200 {
		t.Fatalf("unrestricted: %d", code)
	}
	// An expired key is refused with the reason.
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	code, _, o = keysCall(office, "GET", "/domains", nil, nil)
	if code != 401 || o["name"] != "api_key_expired" {
		t.Fatalf("expired: %d %v", code, o)
	}
	code, k = e.call("PATCH", "/api-keys/"+id, "", map[string]any{"expires_at": nil})
	if code != 200 || k["expires_at"] != nil {
		t.Fatalf("patch expiry: %d %v", code, k)
	}
	if code, _ := office.call("GET", "/domains", "", nil); code != 200 {
		t.Fatalf("no longer expires: %d", code)
	}
}

type headerTransport struct{ key, xff string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.key)
	r.Header.Set("X-Forwarded-For", h.xff)
	return http.DefaultTransport.RoundTrip(r)
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, s := range l {
		out = append(out, s.(string))
	}
	return out
}

// The audit log records who changed what, append-only and hash-chained.
func TestAuditLog(t *testing.T) {
	e := newEnv(t, nil)
	domID, _ := e.addVerifiedDomain("audit.test")
	agent, agentID := keysNewKey(e, "agent", "sending_access", nil)
	e.call("PATCH", "/api-keys/"+agentID, "", map[string]any{"name": "agent-2"})
	code, hook := e.call("POST", "/webhooks", "", map[string]any{"endpoint": "http://127.0.0.1:9/hook"})
	if code != 201 {
		t.Fatal(code, hook)
	}
	e.call("DELETE", "/webhooks/"+hook["id"].(string), "", nil)
	inbox := e.createInbox("help@audit.test")
	e.call("DELETE", "/inboxes/"+inbox, "", nil)
	e.call("DELETE", "/api-keys/"+agentID, "", nil)
	e.call("DELETE", "/domains/"+domID, "", nil)

	code, list := e.call("GET", "/audit-log", "", nil)
	if code != 200 {
		t.Fatal(code, list)
	}
	var actions []string
	entries := list["data"].([]any)
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i].(map[string]any)
		actions = append(actions, en["action"].(string))
		if en["actor"] != "api_key" || en["actor_key_id"] == nil {
			t.Errorf("entry %v has no acting key", en)
		}
		if strings.Contains(en["entry"].(string), "token") || strings.Contains(en["entry"].(string), "whsec_") {
			t.Errorf("entry holds a secret: %s", en["entry"])
		}
	}
	want := "domain.created domain.verified api_key.created api_key.updated webhook.created webhook.deleted inbox.created inbox.deleted api_key.revoked domain.deleted"
	if strings.Join(actions, " ") != want {
		t.Fatalf("actions:\n%s\nwant\n%s", strings.Join(actions, " "), want)
	}
	// The chain checks out by hand: hash = SHA-256(prev_hash || entry).
	prev := strings.Repeat("0", 64)
	for i := len(entries) - 1; i >= 0; i-- {
		en := entries[i].(map[string]any)
		p, _ := hex.DecodeString(en["prev_hash"].(string))
		sum := sha256.Sum256(append(p, en["entry"].(string)...))
		if en["prev_hash"] != prev || hex.EncodeToString(sum[:]) != en["hash"] {
			t.Fatalf("entry %v does not chain", en["id"])
		}
		prev = en["hash"].(string)
	}
	// Paging and filtering.
	code, page := e.call("GET", "/audit-log?limit=3", "", nil)
	if code != 200 || len(page["data"].([]any)) != 3 || page["has_more"] != true {
		t.Fatalf("page: %d %v", code, page)
	}
	code, page = e.call("GET", "/audit-log?limit=50&before="+page["next_before"].(string), "", nil)
	if code != 200 || len(page["data"].([]any)) != len(entries)-3 {
		t.Fatalf("page 2: %d %v", code, page)
	}
	code, page = e.call("GET", "/audit-log?action=webhook.created", "", nil)
	if code != 200 || len(page["data"].([]any)) != 1 {
		t.Fatalf("filter: %d %v", code, page)
	}
	// Full access only; other projects see nothing of it.
	if code, _ := agent.call("GET", "/audit-log", "", nil); code != 401 { // revoked above
		t.Fatalf("revoked key: %d", code)
	}
	sender, _ := keysNewKey(e, "sender", "sending_access", nil)
	if code, o := sender.call("GET", "/audit-log", "", nil); code != 403 {
		t.Fatalf("sending_access: %d %v", code, o)
	}
	if code, o := e.otherProject().call("GET", "/audit-log", "", nil); code != 200 || len(o["data"].([]any)) != 0 {
		t.Fatalf("other project: %d %v", code, o)
	}

	// Verification, then tampering.
	code, v := e.call("GET", "/audit-log/verify", "", nil)
	if code != 200 || v["valid"] != true || int(v["entries"].(float64)) != len(entries)+1 || v["head_hash"] == "" {
		t.Fatalf("verify: %d %v", code, v)
	}
	if _, err := e.st.Pool.Exec(e.ctx, `UPDATE audit_log SET action = 'x'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update allowed: %v", err)
	}
	if _, err := e.st.Pool.Exec(e.ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("delete allowed")
	}
	victim := entries[len(entries)-4].(map[string]any)["id"].(float64) // api_key.updated, name agent-2
	tamper := `ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only;
		UPDATE audit_log SET entry = replace(entry, 'agent-2', 'someone') WHERE id = ` + strconv.Itoa(int(victim)) + `;
		ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only;`
	if _, err := e.st.Pool.Exec(e.ctx, tamper); err != nil {
		t.Fatal(err)
	}
	code, v = e.call("GET", "/audit-log/verify", "", nil)
	if code != 200 || v["valid"] != false || v["first_invalid_id"] != victim || !strings.Contains(v["problem"].(string), "changed") {
		t.Fatalf("after tampering: %d %v", code, v)
	}
}

// Responses are nosniff and no-store; /health shows the version only to a
// caller with a key.
func TestResponseHeadersAndHealth(t *testing.T) {
	e := newEnv(t, nil)
	code, h, _ := keysCall(e.harness, "GET", "/domains", nil, nil)
	if code != 200 || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", code, h)
	}
	resp, err := http.Get(e.ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var anon map[string]any
	json.NewDecoder(resp.Body).Decode(&anon)
	resp.Body.Close()
	if anon["status"] != "ok" || anon["version"] != nil || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("anonymous health: %v", anon)
	}
	if _, _, o := keysCall(e.harness, "GET", "/health", nil, nil); o["version"] != "test" {
		t.Fatalf("health with a key: %v", o)
	}
	if _, _, o := keysCall(&harness{t: t, url: e.ts.URL, apiKey: e.workerToken}, "GET", "/health", nil, nil); o["version"] != "test" {
		t.Fatalf("health with a worker token: %v", o)
	}
	if _, _, o := keysCall(&harness{t: t, url: e.ts.URL, apiKey: "omw_wrong"}, "GET", "/health", nil, nil); o["version"] != nil {
		t.Fatalf("health with a wrong key: %v", o)
	}
}

// HTML-only mail gets a generated text/plain part.
func TestHTMLOnlySendHasTextPart(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("html.test")
	code, out := e.call("POST", "/emails", "", map[string]any{"from": "a@html.test", "to": "x@example.test", "subject": "s",
		"html": "<p>Your code is <b>123456</b>.</p>"})
	if code != 200 {
		t.Fatal(code, out)
	}
	raw := string(e.storedRaw(out["id"].(string)))
	if !strings.Contains(raw, "multipart/alternative") || !strings.Contains(raw, "text/plain") || !strings.Contains(raw, "Your code is 123456.") {
		t.Fatalf("message:\n%s", raw)
	}
}
