package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/dnsprovider/cloudflaretest"
)

// Lane G: automatic DNS setup with Cloudflare, against a local fake of
// Cloudflare's API that also serves as the control plane's resolver.

const cfToken = "cfTEST_token_1234567890abcdefghijklmnopqrstu"

// lockedBuffer is a log destination safe for concurrent handlers.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type dnsEnv struct {
	*env
	cf   *cloudflaretest.Server
	logs *lockedBuffer
	seen []string // every API response body, to check for the token
}

// newDNSEnv is newEnv with the fake Cloudflare as API base and resolver, a
// bounce host, and a logger that keeps everything logged.
func newDNSEnv(t *testing.T, tweak func(*config.Control)) *dnsEnv {
	t.Helper()
	cf := cloudflaretest.New(cfToken)
	t.Cleanup(cf.Close)
	e := newEnv(t, func(c *config.Control) {
		c.CloudflareAPIBase = cf.URL
		c.BounceMX = "bounces.mail.openmailwire.test"
		if tweak != nil {
			tweak(c)
		}
	})
	logs := &lockedBuffer{}
	e.srv.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.srv.Resolver = cf
	d := &dnsEnv{env: e, cf: cf, logs: logs}
	t.Cleanup(d.checkNoToken)
	return d
}

// call is e.call that also keeps the response for checkNoToken.
func (d *dnsEnv) call(h *harness, method, path string, body any) (int, map[string]any) {
	d.t.Helper()
	code, out := h.call(method, path, "", body)
	b, _ := json.Marshal(out)
	d.seen = append(d.seen, string(b))
	return code, out
}

// checkNoToken: the token is in no response, audit entry, log line or
// stored column in plain text.
func (d *dnsEnv) checkNoToken() {
	t := d.t
	for _, s := range d.seen {
		if strings.Contains(s, cfToken) {
			t.Errorf("the token is in a response: %.300s", s)
		}
	}
	if strings.Contains(d.logs.String(), cfToken) {
		t.Errorf("the token is in the logs")
	}
	rows, err := d.st.Pool.Query(context.Background(), `SELECT entry FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var entry string
		rows.Scan(&entry)
		if strings.Contains(entry, cfToken) {
			t.Errorf("the token is in the audit log: %s", entry)
		}
	}
	rows.Close()
	var leaked bool
	if err := d.st.Pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM dns_automation
		WHERE position($1::bytea IN COALESCE(api_token, '')) > 0 OR last_run::text LIKE '%' || $2 || '%')`,
		[]byte(cfToken), cfToken).Scan(&leaked); err != nil || leaked {
		t.Errorf("the token is stored in plain text (%v)", err)
	}
	for auth := range d.cf.Authorizations() {
		if auth != "Bearer "+cfToken && !strings.HasPrefix(auth, "Bearer wrong") {
			t.Errorf("Cloudflare got Authorization %q", auth)
		}
	}
}

func (d *dnsEnv) addDomain(name string) string {
	d.t.Helper()
	code, dom := d.env.call("POST", "/domains", "", map[string]string{"name": name})
	if code != 201 {
		d.t.Fatalf("add domain %s: %d %v", name, code, dom)
	}
	return dom["id"].(string)
}

type change struct{ Record, Type, Name, Value, Action, Detail string }

func changesOf(t *testing.T, run map[string]any) map[string]change {
	t.Helper()
	out := map[string]change{}
	list, ok := run["changes"].([]any)
	if !ok {
		t.Fatalf("no changes in %v", run)
	}
	for _, c := range list {
		m := c.(map[string]any)
		ch := change{Record: m["record"].(string), Type: m["type"].(string), Name: m["name"].(string), Value: m["value"].(string),
			Action: m["action"].(string)}
		ch.Detail, _ = m["detail"].(string)
		key := ch.Record
		if _, dup := out[key]; dup {
			key += "#" + ch.Action
		}
		out[key] = ch
	}
	return out
}

func warningsOf(run map[string]any) string {
	var ws []string
	for _, w := range run["warnings"].([]any) {
		ws = append(ws, w.(string))
	}
	return strings.Join(ws, "\n")
}

func (d *dnsEnv) writes() int {
	return d.cf.Count("POST ") + d.cf.Count("PATCH ") + d.cf.Count("DELETE ")
}

func TestDNSAutoApply(t *testing.T) {
	d := newDNSEnv(t, nil)
	d.cf.AddZone("acme.test")
	d.cf.AddRecord(cloudflaretest.Record{Type: "TXT", Name: "acme.test", Content: "google-site-verification=keep-me"})
	id := d.addDomain("acme.test")
	path := "/domains/" + id + "/dns/auto"

	code, run := d.call(d.harness, "POST", path, map[string]any{"provider": "cloudflare", "api_token": cfToken})
	if code != 200 {
		t.Fatalf("apply: %d %v", code, run)
	}
	ch := changesOf(t, run)
	for _, rec := range []string{"DKIM", "SPF", "MX", "DMARC", "TLSRPT"} {
		if ch[rec].Action != "create" {
			t.Errorf("%s: %+v, want create", rec, ch[rec])
		}
	}
	if _, ok := ch["INBOUND"]; ok {
		t.Error("an INBOUND MX without OMW_INBOUND_MX")
	}
	if ch["DMARC"].Value != "v=DMARC1; p=none; rua=mailto:dmarc@bounce.acme.test" ||
		ch["TLSRPT"].Value != "v=TLSRPTv1; rua=mailto:tls-rpt@bounce.acme.test" ||
		ch["SPF"].Value != "v=spf1 include:_spf.openmailwire.test -all" ||
		ch["MX"].Value != "bounces.mail.openmailwire.test" || ch["MX"].Name != "bounce.acme.test" {
		t.Fatalf("values: %+v", ch)
	}
	zone := run["zone"].(map[string]any)
	if zone["name"] != "acme.test" || zone["dnssec"] != "disabled" || zone["email_routing"] != "disabled" || run["dry_run"] != false ||
		run["provider"] != "cloudflare" || run["remembered"] != false {
		t.Fatalf("run: %v", run)
	}
	if !strings.Contains(warningsOf(run), "DNSSEC is disabled for acme.test") {
		t.Errorf("no DNSSEC note: %s", warningsOf(run))
	}
	if dom := run["domain"].(map[string]any); dom["status"] != "verified" {
		t.Fatalf("after apply the domain is %v: %v", dom["status"], dom["records"])
	}
	for _, r := range d.cf.Records("") {
		if r.Proxied {
			t.Errorf("proxied record %+v", r)
		}
		if r.Content != "google-site-verification=keep-me" && r.Comment != "Managed by OpenMailWire" {
			t.Errorf("no comment on %+v", r)
		}
	}
	if got := d.cf.TXT("acme.test"); len(got) != 1 {
		t.Fatalf("the apex TXT changed: %q", got)
	}

	// Again: everything is unchanged and nothing is written.
	writes := d.writes()
	code, run = d.call(d.harness, "POST", path, map[string]any{"provider": "cloudflare", "api_token": cfToken})
	if code != 200 {
		t.Fatalf("re-apply: %d %v", code, run)
	}
	for rec, c := range changesOf(t, run) {
		if c.Action != "unchanged" {
			t.Errorf("re-apply %s: %+v", rec, c)
		}
	}
	if d.writes() != writes {
		t.Fatalf("re-apply wrote %d times", d.writes()-writes)
	}

	// A dry run of a subdomain in the same zone writes nothing, and keeps
	// the DMARC policy it inherits from acme.test.
	sub := d.addDomain("agents.acme.test")
	code, run = d.call(d.harness, "POST", "/domains/"+sub+"/dns/auto", map[string]any{"api_token": cfToken, "dry_run": true})
	if code != 200 || run["dry_run"] != true {
		t.Fatalf("dry run: %d %v", code, run)
	}
	ch = changesOf(t, run)
	if ch["DKIM"].Action != "create" || ch["SPF"].Action != "create" || ch["DMARC"].Action != "skip" ||
		!strings.Contains(ch["DMARC"].Detail, "inherits the DMARC policy at _dmarc.acme.test") {
		t.Fatalf("dry run changes: %+v", ch)
	}
	if !strings.Contains(warningsOf(run), "dry run: nothing was changed") {
		t.Errorf("warnings: %s", warningsOf(run))
	}
	if d.writes() != writes || len(d.cf.Records("bounce.agents.acme.test")) != 0 {
		t.Fatal("a dry run wrote records")
	}
	if dom := run["domain"].(map[string]any); dom["status"] != "pending" {
		t.Fatalf("a dry run verified the domain: %v", dom["status"])
	}

	// GET shows the last run, and that no token is remembered.
	code, got := d.call(d.harness, "GET", path, nil)
	if code != 200 || got["remembered"] != false || got["last_run"] == nil || got["zone"].(map[string]any)["name"] != "acme.test" {
		t.Fatalf("GET: %d %v", code, got)
	}

	// The audit log has the two applies (not the dry run), without values.
	code, log := d.call(d.harness, "GET", "/audit-log?action=dns.applied", nil)
	if entries := log["data"].([]any); code != 200 || len(entries) != 2 {
		t.Fatalf("audit: %d %v", code, log)
	}
	data := log["data"].([]any)[1].(map[string]any)["data"].(map[string]any)
	if data["created"].(float64) != 5 || data["trigger"] != "api" || data["token"] != "request" || data["zone_name"] != "acme.test" {
		t.Fatalf("audit data %v", data)
	}
}

func TestDNSAutoProtectsOwnerMail(t *testing.T) {
	d := newDNSEnv(t, func(c *config.Control) {
		c.InboundMX = "mx.inbound.openmailwire.test"
		c.BounceMX = "bounces.owner.test" // in the zone, and proxied below
	})
	d.cf.AddZone("owner.test")
	d.cf.SetZone("owner.test", func(z *cloudflaretest.Zone) { z.EmailRouting, z.DNSSEC = "enabled", "active" })
	d.cf.AddRecord(cloudflaretest.Record{Type: "MX", Name: "owner.test", Content: "aspmx.l.google.com", Priority: 1})
	ownDMARC := "v=DMARC1; p=reject; rua=mailto:dmarc@owner.test"
	d.cf.AddRecord(cloudflaretest.Record{Type: "TXT", Name: "_dmarc.owner.test", Content: `"` + ownDMARC + `"`})
	d.cf.AddRecord(cloudflaretest.Record{Type: "TXT", Name: "bounce.owner.test", Content: "v=spf1 include:_spf.google.com ~all"})
	d.cf.AddRecord(cloudflaretest.Record{Type: "A", Name: "bounces.owner.test", Content: "203.0.113.25", Proxied: true})
	id := d.addDomain("owner.test")

	code, run := d.call(d.harness, "POST", "/domains/"+id+"/dns/auto", map[string]any{"api_token": cfToken})
	if code != 200 {
		t.Fatalf("apply: %d %v", code, run)
	}
	ch, warn := changesOf(t, run), warningsOf(run)
	if ch["INBOUND"].Action != "skip" || !strings.Contains(warn, "owner.test already receives mail at aspmx.l.google.com") ||
		!strings.Contains(warn, "agents.owner.test") {
		t.Fatalf("apex MX: %+v\n%s", ch["INBOUND"], warn)
	}
	if mx := d.cf.Records("owner.test"); len(mx) != 1 || mx[0].Content != "aspmx.l.google.com" {
		t.Fatalf("the apex MX changed: %+v", mx)
	}
	if ch["DMARC"].Action != "unchanged" || !strings.Contains(warn, "A DMARC record already exists at _dmarc.owner.test") ||
		!strings.Contains(warn, "rua=mailto:dmarc@bounce.owner.test") {
		t.Fatalf("DMARC: %+v\n%s", ch["DMARC"], warn)
	}
	if got := d.cf.TXT("_dmarc.owner.test"); len(got) != 1 || got[0] != ownDMARC {
		t.Fatalf("the DMARC record changed: %q", got)
	}
	// The return path is OpenMailWire's: a foreign SPF there is replaced,
	// and the replacement reported.
	if ch["SPF"].Action != "update" || !strings.Contains(warn, "The SPF record at bounce.owner.test") {
		t.Fatalf("SPF: %+v\n%s", ch["SPF"], warn)
	}
	if got := d.cf.TXT("bounce.owner.test"); len(got) != 1 || got[0] != "v=spf1 include:_spf.openmailwire.test -all" {
		t.Fatalf("SPF now %q", got)
	}
	for _, want := range []string{"Cloudflare Email Routing is on for owner.test", "use a subdomain such as agents.owner.test",
		"A bounces.owner.test is proxied by Cloudflare"} {
		if !strings.Contains(warn, want) {
			t.Errorf("no warning %q in:\n%s", want, warn)
		}
	}
	if strings.Contains(warn, "DNSSEC") {
		t.Errorf("a DNSSEC warning with DNSSEC active:\n%s", warn)
	}
	// Email Routing that the token may not read is unknown, not an error.
	d.cf.SetZone("owner.test", func(z *cloudflaretest.Zone) { z.EmailRouting = "forbidden" })
	code, run = d.call(d.harness, "POST", "/domains/"+id+"/dns/auto", map[string]any{"api_token": cfToken, "dry_run": true})
	if code != 200 || run["zone"].(map[string]any)["email_routing"] != "unknown" ||
		!strings.Contains(warningsOf(run), "Could not read whether Cloudflare Email Routing is on") {
		t.Fatalf("routing unknown: %d %v", code, run)
	}
}

func TestDNSAutoErrors(t *testing.T) {
	d := newDNSEnv(t, nil)
	d.cf.AddZone("acme.test")
	id := d.addDomain("acme.test")
	path := "/domains/" + id + "/dns/auto"

	expect := func(body map[string]any, status int, name, contains string) map[string]any {
		t.Helper()
		code, b := d.call(d.harness, "POST", path, body)
		if code != status || b["name"] != name || !strings.Contains(b["message"].(string), contains) {
			t.Fatalf("%v: %d %v, want %d %s (%q)", body, code, b, status, name, contains)
		}
		return b
	}
	// Nothing remembered and no token: the person must connect Cloudflare.
	expect(map[string]any{}, 409, "dns_provider_not_connected", "connect Cloudflare once")
	// Bad requests; the token is never echoed.
	expect(map[string]any{"provider": "route53", "api_token": cfToken}, 422, "validation_error", "cloudflare")
	expect(map[string]any{"api_token": "has spaces " + cfToken}, 422, "validation_error", "not a Cloudflare API token")
	expect(map[string]any{"api_token": cfToken, "zone_id": "../../user"}, 422, "validation_error", "32 hexadecimal digits")
	expect(map[string]any{"api_token": cfToken, "colour": "orange"}, 422, "validation_error", "colour")
	// A wrong token, and one that may read but not edit DNS.
	expect(map[string]any{"api_token": "wrong-" + cfToken}, 422, "dns_provider_auth_failed", "Zone > DNS > Edit")
	d.cf.ReadOnly = true
	expect(map[string]any{"api_token": cfToken}, 422, "dns_provider_auth_failed", "Zone > DNS > Edit")
	d.cf.ReadOnly = false
	if n := len(d.cf.Records("")); n != 0 {
		t.Fatalf("%d records after failed runs", n)
	}
	// No zone holds the domain; a zone id of another zone.
	other := d.addDomain("elsewhere.test")
	code, b := d.call(d.harness, "POST", "/domains/"+other+"/dns/auto", map[string]any{"api_token": cfToken})
	if code != 422 || b["name"] != "dns_zone_not_found" || !strings.Contains(b["message"].(string), "elsewhere.test") {
		t.Fatalf("no zone: %d %v", code, b)
	}
	z2 := d.cf.AddZone("unrelated.test")
	expect(map[string]any{"api_token": cfToken, "zone_id": z2.ID}, 422, "dns_zone_not_found", "does not hold acme.test")
	// Cloudflare rate-limits for longer than we wait: 503 with Retry-After.
	d.cf.Inject(cloudflaretest.Fault{Status: 429, RetryAfter: "300", Times: 5})
	req, _ := http.NewRequest("POST", d.ts.URL+path, strings.NewReader(`{"api_token":"`+cfToken+`"}`))
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable map[string]any
	json.NewDecoder(resp.Body).Decode(&unavailable)
	resp.Body.Close()
	if resp.StatusCode != 503 || unavailable["name"] != "dns_provider_unavailable" || resp.Header.Get("Retry-After") != "300" {
		t.Fatalf("429: %d %v %q", resp.StatusCode, unavailable, resp.Header.Get("Retry-After"))
	}
	d.cf.ClearFaults()
	// A 5xx on a create is not repeated; the run stops and says so.
	d.cf.Inject(cloudflaretest.Fault{Method: "POST", Path: "/dns_records", Status: 500})
	before := d.cf.Count("POST ")
	expect(map[string]any{"api_token": cfToken}, 503, "dns_provider_unavailable", "safe to repeat")
	if d.cf.Count("POST ")-before != 1 {
		t.Fatal("a failed create was repeated")
	}
	// Then it works, and finishes what the failed run started.
	if code, run := d.call(d.harness, "POST", path, map[string]any{"api_token": cfToken}); code != 200 {
		t.Fatalf("after the failures: %d %v", code, run)
	}

	// Sending-only keys cannot use any of it.
	agent, _ := keysNewKey(d.env, "agent", "sending_access", nil)
	for _, m := range []string{"POST", "GET", "DELETE"} {
		if code, b := d.call(agent, m, path, map[string]any{}); code != 403 || b["name"] != "insufficient_permission" {
			t.Fatalf("%s with a sending key: %d %v", m, code, b)
		}
	}
	for _, s := range d.seen {
		if strings.Contains(s, "wrong-") {
			t.Fatalf("a response echoes the token: %s", s)
		}
	}
}

func TestDNSAutoRememberRotateForget(t *testing.T) {
	d := newDNSEnv(t, nil)
	d.cf.AddZone("acme.test")
	id := d.addDomain("acme.test")
	path := "/domains/" + id + "/dns/auto"

	code, run := d.call(d.harness, "POST", path, map[string]any{"provider": "cloudflare", "api_token": cfToken, "remember": true})
	if code != 200 || run["remembered"] != true || run["domain"].(map[string]any)["status"] != "verified" {
		t.Fatalf("apply and remember: %d %v", code, run)
	}
	oldDKIM := changesOf(t, run)["DKIM"].Name
	code, got := d.call(d.harness, "GET", path, nil)
	if code != 200 || got["remembered"] != true || got["remembered_at"] == nil || got["provider"] != "cloudflare" {
		t.Fatalf("GET: %d %v", code, got)
	}

	// Rotation step 1 publishes the new key's record by itself.
	code, rot := d.call(d.harness, "POST", "/domains/"+id+"/dkim/rotate", nil)
	if code != 202 {
		t.Fatalf("rotate: %d %v", code, rot)
	}
	auto, _ := rot["dns_auto"].(map[string]any)
	if auto == nil || auto["published"] != true {
		t.Fatalf("rotate did not publish: %v", rot)
	}
	next := rot["record"].(map[string]any)
	if got := d.cf.TXT(next["name"].(string)); len(got) != 1 || got[0] != next["value"] {
		t.Fatalf("new DKIM record %q", got)
	}
	// So step 2 finds it right away.
	if code, rot = d.call(d.harness, "POST", "/domains/"+id+"/dkim/rotate", nil); code != 200 || rot["status"] != "rotated" {
		t.Fatalf("rotate step 2: %d %v", code, rot)
	}
	// Nothing is retired during the overlap…
	if err := d.srv.RetireDKIMRecords(d.ctx); err != nil {
		t.Fatal(err)
	}
	if len(d.cf.TXT(oldDKIM)) != 1 {
		t.Fatal("the previous DKIM record was deleted during its overlap")
	}
	// …and the previous record goes once the overlap is over.
	if _, err := d.st.Pool.Exec(d.ctx, `UPDATE dkim_keys SET retire_at = now() - interval '1 minute' WHERE state = 'retiring'`); err != nil {
		t.Fatal(err)
	}
	if err := d.srv.RetireDKIMRecords(d.ctx); err != nil {
		t.Fatal(err)
	}
	if got := d.cf.TXT(oldDKIM); len(got) != 0 {
		t.Fatalf("the previous DKIM record is still there: %q", got)
	}
	if got := d.cf.TXT(next["name"].(string)); len(got) != 1 {
		t.Fatal("the current DKIM record was deleted")
	}
	writes := d.writes()
	if err := d.srv.RetireDKIMRecords(d.ctx); err != nil || d.writes() != writes {
		t.Fatalf("a second retire pass: %v, %d writes", err, d.writes()-writes)
	}

	// Ed25519: its record is published when the key is made.
	code, ed := d.call(d.harness, "POST", "/domains/"+id+"/dkim/ed25519", map[string]any{"enabled": true})
	if auto, _ := ed["dns_auto"].(map[string]any); code != 202 || auto == nil || auto["published"] != true {
		t.Fatalf("ed25519: %d %v", code, ed)
	}
	if code, ed = d.call(d.harness, "POST", "/domains/"+id+"/dkim/ed25519", map[string]any{"enabled": true}); code != 200 || ed["status"] != "active" {
		t.Fatalf("ed25519 step 2: %d %v", code, ed)
	}

	// The MCP tool, with the remembered token.
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(d.ctx,
		&mcp.StreamableClientTransport{Endpoint: d.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{d.apiKey}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tl, err := sess.ListTools(d.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range tl.Tools {
		if x.Name == "setup_domain_dns" {
			tool = x
		}
	}
	if tool == nil || tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
		t.Fatalf("setup_domain_dns missing or not destructive: %+v", tool)
	}
	if props, _ := json.Marshal(tool.InputSchema); strings.Contains(string(props), "token") {
		t.Fatalf("the tool takes a token: %s", props)
	}
	txt := textOf(callTool(t, sess, "setup_domain_dns", map[string]any{"domain": "acme.test", "dry_run": true}))
	if !strings.Contains(txt, "Dry run in Cloudflare zone acme.test") || !strings.Contains(txt, "unchanged: DKIM_ED25519 TXT") {
		t.Fatalf("dry run text:\n%s", txt)
	}
	txt = textOf(callTool(t, sess, "setup_domain_dns", map[string]any{"domain": id}))
	if !strings.Contains(txt, "Applied in Cloudflare zone acme.test") || !strings.Contains(txt, "acme.test is verified") {
		t.Fatalf("apply text:\n%s", txt)
	}
	d.seen = append(d.seen, txt)

	// Forget: automatic changes stop, the records stay.
	code, f := d.call(d.harness, "DELETE", path, nil)
	if code != 200 || f["forgotten"] != true || f["remembered"] != false {
		t.Fatalf("forget: %d %v", code, f)
	}
	if code, f = d.call(d.harness, "DELETE", path, nil); code != 200 || f["forgotten"] != false {
		t.Fatalf("forget again: %d %v", code, f)
	}
	res, err := sess.CallTool(d.ctx, &mcp.CallToolParams{Name: "setup_domain_dns", Arguments: map[string]any{"domain": "acme.test"}})
	if err != nil || !res.IsError || !strings.Contains(textOf(res), "dns_provider_not_connected") || !strings.Contains(textOf(res), "dashboard") {
		t.Fatalf("tool without a token: %v %v", err, textOf(res))
	}
	code, rot = d.call(d.harness, "POST", "/domains/"+id+"/dkim/rotate", nil)
	if code != 202 || rot["dns_auto"] != nil {
		t.Fatalf("rotate after forget: %d %v", code, rot)
	}
	if len(d.cf.TXT(rot["record"].(map[string]any)["name"].(string))) != 0 {
		t.Fatal("published with a forgotten token")
	}

	// Every apply, remember and forget is in the audit log.
	code, log := d.call(d.harness, "GET", "/audit-log?limit=200", nil)
	if code != 200 {
		t.Fatal(code)
	}
	count := map[string]int{}
	triggers := map[string]bool{}
	for _, e := range log["data"].([]any) {
		m := e.(map[string]any)
		count[m["action"].(string)]++
		if m["action"] == "dns.applied" {
			data := m["data"].(map[string]any)
			triggers[data["trigger"].(string)] = true
			if data["trigger"] == "dkim_retire" && (m["actor"] != "system" || data["deleted"].(float64) != 1) {
				t.Errorf("retire entry %v", m)
			}
		}
	}
	if count["dns.token_remembered"] != 1 || count["dns.token_forgotten"] != 1 || count["dns.applied"] != 5 {
		t.Fatalf("audit actions %v", count)
	}
	for _, tr := range []string{"api", "dkim_rotate", "dkim_retire", "ed25519", "mcp"} {
		if !triggers[tr] {
			t.Errorf("no dns.applied with trigger %s: %v", tr, triggers)
		}
	}
	if code, v := d.call(d.harness, "GET", "/audit-log/verify", nil); code != 200 || v["valid"] != true {
		t.Fatalf("audit chain: %v", v)
	}
}

func TestOperatorSPFSync(t *testing.T) {
	d := newDNSEnv(t, func(c *config.Control) {
		c.SPFInclude = "_spf.mail.optest.net"
		c.CloudflareToken = cfToken
	})
	d.cf.AddZone("optest.net")
	name := "_spf.mail.optest.net"

	// No worker has announced an IP: nothing is published.
	res, err := d.srv.SyncOperatorSPF(d.ctx, false)
	if err != nil || res.Action != "skip" || len(d.cf.Records(name)) != 0 {
		t.Fatalf("no workers: %+v %v", res, err)
	}
	heartbeat := func(token, ip string) {
		t.Helper()
		if code, n := d.postWorker(token, "/internal/worker/heartbeat", `{"public_ip":"`+ip+`","ehlo":"w.optest.net","capacity":4}`); code != 200 {
			t.Fatalf("heartbeat: %d %s", code, n)
		}
	}
	heartbeat(d.workerToken, "203.0.113.7")
	// Inbound nodes, relays and workers not seen for 7 days do not count.
	inbound, _ := d.addWorker("in-1", "inbound")
	stale, _ := d.addWorker("old-1", "delivery")
	if _, err := d.st.Pool.Exec(d.ctx, `UPDATE workers SET ip = '192.0.2.50', last_seen_at = now() WHERE id = $1`, inbound); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.Pool.Exec(d.ctx, `UPDATE workers SET ip = '192.0.2.60', last_seen_at = now() - interval '8 days' WHERE id = $1`, stale); err != nil {
		t.Fatal(err)
	}

	// The control plane's loop publishes it.
	ctx, cancel := context.WithCancel(d.ctx)
	t.Cleanup(cancel)
	go d.srv.RunDNSMaintenance(ctx, time.Hour)
	deadline := time.Now().Add(10 * time.Second)
	for len(d.cf.TXT(name)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.cf.TXT(name); len(got) != 1 || got[0] != "v=spf1 ip4:203.0.113.7 -all" {
		t.Fatalf("published %q", got)
	}

	// Nothing changed: Cloudflare is not even asked.
	requests := len(d.cf.Requests())
	if res, err = d.srv.SyncOperatorSPF(d.ctx, false); err != nil || res.Action != "unchanged" || len(d.cf.Requests()) != requests {
		t.Fatalf("unchanged: %+v %v (%d requests)", res, err, len(d.cf.Requests())-requests)
	}

	// A new worker's heartbeat with a new IP: the record follows.
	_, tok := d.addWorker("w2", "delivery")
	heartbeat(tok, "198.51.100.9")
	res, err = d.srv.SyncOperatorSPF(d.ctx, true)
	if err != nil || res.Action != "update" || len(d.cf.TXT(name)) != 1 || d.cf.TXT(name)[0] != "v=spf1 ip4:203.0.113.7 -all" {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	res, err = d.srv.SyncOperatorSPF(d.ctx, false)
	want := "v=spf1 ip4:198.51.100.9 ip4:203.0.113.7 -all"
	if err != nil || res.Action != "update" || res.New != want || len(res.IPs) != 2 {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if got := d.cf.TXT(name); len(got) != 1 || got[0] != want {
		t.Fatalf("record %q", got)
	}
	if !strings.Contains(d.logs.String(), "operator SPF include updated") {
		t.Fatal("the change was not logged")
	}

	// Cloudflare refusing the operator token is an error the loop logs.
	d.cf.Inject(cloudflaretest.Fault{Status: 403, Times: 5})
	heartbeat(tok, "198.51.100.10")
	if _, err := d.srv.SyncOperatorSPF(d.ctx, false); err == nil {
		t.Fatal("no error when Cloudflare refuses the token")
	}
}
