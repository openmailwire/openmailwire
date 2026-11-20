package e2e

import (
	"net"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/protocol"
)

// overviewEnv is the env with receiving and warm-up on, as the
// dashboard's home page sees a real installation.
func overviewEnv(t *testing.T) *env {
	return newEnv(t, func(c *config.Control) {
		c.InboundMX = "mx.openmailwire.test"
		c.Warmup = true
	})
}

// addReceivingDomain adds and verifies a domain like addVerifiedDomain,
// and also publishes its return-path MX, which receiving makes required.
// The INBOUND MX is left unpublished.
func (e *env) addReceivingDomain(name string) string {
	e.t.Helper()
	code, dom := e.call("POST", "/domains", "", map[string]string{"name": name})
	if code != 201 {
		e.t.Fatalf("add domain %s: %d %v", name, code, dom)
	}
	if e.dns.mx == nil {
		e.dns.mx = map[string][]*net.MX{}
	}
	for _, r := range dom["records"].([]any) {
		rec := r.(map[string]any)
		n, val := rec["name"].(string), rec["value"].(string)
		switch {
		case rec["type"] == "TXT":
			e.dns.txt[n] = append(e.dns.txt[n], val)
		case rec["record"] == "MX":
			e.dns.mx[n] = append(e.dns.mx[n], &net.MX{Host: val + ".", Pref: 10})
		}
	}
	id := dom["id"].(string)
	if code, v := e.call("POST", "/domains/"+id+"/verify", "", nil); code != 200 || v["status"] != "verified" {
		e.t.Fatalf("verify domain %s: %d %v", name, code, v)
	}
	return id
}

func overviewCall(t *testing.T, h *harness) map[string]any {
	t.Helper()
	code, v := h.call("GET", "/overview", "", nil)
	if code != 200 || v["object"] != "overview" {
		t.Fatalf("GET /overview: %d %v", code, v)
	}
	return v
}

// TestOverview checks GET /overview: queue depth, sending windows and the
// daily series, holds and pauses, domain problems, receiving counts and
// workers.
func TestOverview(t *testing.T) {
	e := overviewEnv(t)
	domID := e.addReceivingDomain("ov.test")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.st.Pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	send := func(h *harness, body map[string]any) map[string]any {
		t.Helper()
		code, v := h.call("POST", "/emails", "", body)
		if code != 200 {
			t.Fatalf("send: %d %v", code, v)
		}
		return v
	}
	mail := func(to string) map[string]any {
		return map[string]any{"from": "a@ov.test", "to": []string{to}, "subject": "s", "text": "t"}
	}

	// An empty project.
	v := overviewCall(t, e.harness)
	q := v["queue"].(map[string]any)
	if q["queued"] != float64(0) || q["oldest_queued_at"] != nil || q["oldest_age_seconds"] != nil {
		t.Errorf("empty queue: %v", q)
	}
	sending := v["sending"].(map[string]any)
	w24 := sending["windows"].([]any)[0].(map[string]any)
	if w24["window"] != "24h" || w24["recipients"] != float64(0) || w24["delivery_rate"] != nil {
		t.Errorf("empty 24h window: %v", w24)
	}

	// Mail in every state: queued, scheduled, held, delivered with a
	// complaint today, and bounced three days ago.
	send(e.harness, mail("q@example.test"))
	scheduled := mail("s@example.test")
	scheduled["scheduled_at"] = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	send(e.harness, scheduled)
	agent, _ := keysNewKey(e, "agent", "sending_access", map[string]any{"require_approval": map[string]any{"always": true}})
	if h := send(agent, mail("h@example.test")); h["held"] != true {
		t.Fatalf("not held: %v", h)
	}
	delivered := send(e.harness, mail("d@example.test"))["id"].(string)
	exec(`UPDATE deliveries SET status = 'delivered' WHERE email_id = $1`, delivered)
	exec(`INSERT INTO events (email_id, delivery_id, type) SELECT email_id, id, 'complained' FROM deliveries WHERE email_id = $1`, delivered)
	old := send(e.harness, mail("b@example.test"))["id"].(string)
	exec(`UPDATE deliveries SET status = 'bounced' WHERE email_id = $1`, old)
	exec(`UPDATE emails SET created_at = now() - interval '3 days' WHERE id = $1`, old)

	// A pause, a pending DKIM rotation, a reported authentication problem,
	// an unverified domain, and an inbox with a held message.
	exec(`INSERT INTO sending_pauses (project_id, scope, scope_id, scope_name, reason, rate, threshold, recipients, bounces, complaints)
		VALUES ($1, 'domain', $2, 'ov.test', 'bounce_rate', 0.08, 0.05, 100, 8, 0)`, e.projectID, domID)
	if code, b := e.call("POST", "/domains/"+domID+"/dkim/rotate", "", nil); code != 202 {
		t.Fatalf("rotate: %d %v", code, b)
	}
	exec(`INSERT INTO sender_auth_issues (project_id, sending_domain, scope, enhanced_code, last_response)
		VALUES ($1, 'ov.test', 'dkim', '5.7.27', '550 5.7.27 sender not authenticated')`, e.projectID)
	if code, b := e.call("POST", "/domains", "", map[string]string{"name": "new.test"}); code != 201 {
		t.Fatalf("add domain: %d %v", code, b)
	}
	inbox := e.createInbox("support@ov.test")
	mx := e.startInbound()
	if err := inboundSend(t, mx, "maria@example.test", []string{"support@ov.test"},
		crlf("From: Maria <maria@example.test>\nTo: support@ov.test\nSubject: Hello\nMessage-ID: <hello-1@example.test>\n\nHi!\n")); err != nil {
		t.Fatal(err)
	}
	e.waitMessages(inbox, 1)

	// Workers: a delivery worker whose self-check failed, and an inbound
	// node, which has no warm-up. Warm-up rows outlive the project, so this
	// test's IP starts and ends clean.
	cleanIP := func() {
		e.st.Pool.Exec(e.ctx, `DELETE FROM warmup_usage WHERE ip = '198.51.100.231'`)
		e.st.Pool.Exec(e.ctx, `DELETE FROM sending_ips WHERE ip = '198.51.100.231'`)
	}
	cleanIP()
	t.Cleanup(cleanIP)
	if _, err := e.st.Heartbeat(e.ctx, e.workerID, protocol.HeartbeatRequest{PublicIP: "198.51.100.231", EHLO: "w1.ov.test", Capacity: 4,
		Version: "test", SelfCheck: &protocol.SelfCheck{OK: false, CheckedAt: time.Now(),
			Checks: []protocol.CheckItem{{Name: "ehlo_fqdn", OK: true}, {Name: "reverse_dns", OK: false, Detail: "no PTR record"}}}}); err != nil {
		t.Fatal(err)
	}
	mxID, _ := e.addWorker("mx1", protocol.RoleInbound)
	e.beat(mxID, "203.0.113.10", "direct")

	v = overviewCall(t, e.harness)
	q = v["queue"].(map[string]any)
	if q["queued"] != float64(1) || q["scheduled"] != float64(1) || q["held"] != float64(1) || q["sending"] != float64(0) || q["deferred"] != float64(0) {
		t.Errorf("queue: %v", q)
	}
	if age, _ := q["oldest_age_seconds"].(float64); q["oldest_queued_at"] == nil || age < 0 || age > 120 {
		t.Errorf("oldest queued: %v", q)
	}
	if v["held_emails"] != float64(1) || v["active_pauses"] != float64(1) {
		t.Errorf("held_emails %v, active_pauses %v", v["held_emails"], v["active_pauses"])
	}

	sending = v["sending"].(map[string]any)
	windows := sending["windows"].([]any)
	w24, w7 := windows[0].(map[string]any), windows[1].(map[string]any)
	want := func(name string, w map[string]any, fields map[string]any) {
		t.Helper()
		for k, x := range fields {
			if w[k] != x {
				t.Errorf("%s window %s = %v, want %v (%v)", name, k, w[k], x, w)
			}
		}
	}
	want("24h", w24, map[string]any{"window": "24h", "emails": float64(4), "recipients": float64(4), "delivered": float64(1),
		"bounced": float64(0), "complained": float64(1), "delivery_rate": 0.25, "bounce_rate": float64(0), "complaint_rate": 0.25})
	want("7d", w7, map[string]any{"window": "7d", "emails": float64(5), "recipients": float64(5), "delivered": float64(1),
		"bounced": float64(1), "complained": float64(1), "delivery_rate": 0.2, "bounce_rate": 0.2, "complaint_rate": 0.2})
	if sending["truncated"] != false {
		t.Errorf("truncated: %v", sending["truncated"])
	}
	daily := sending["daily"].([]any)
	today := time.Now().UTC()
	if len(daily) != 7 {
		t.Fatalf("daily: %v", daily)
	}
	last, threeAgo := daily[6].(map[string]any), daily[3].(map[string]any)
	if last["day"] != today.Format("2006-01-02") || last["recipients"] != float64(4) || last["delivered"] != float64(1) || last["complained"] != float64(1) {
		t.Errorf("today: %v", last)
	}
	if threeAgo["day"] != today.AddDate(0, 0, -3).Format("2006-01-02") || threeAgo["recipients"] != float64(1) || threeAgo["bounced"] != float64(1) {
		t.Errorf("three days ago: %v", threeAgo)
	}
	if first := daily[0].(map[string]any); first["recipients"] != float64(0) {
		t.Errorf("a day without mail: %v", first)
	}

	doms := map[string]map[string]any{}
	for _, x := range v["domains"].([]any) {
		d := x.(map[string]any)
		doms[d["name"].(string)] = d
	}
	ov := doms["ov.test"]
	if ov == nil || ov["status"] != "verified" || ov["dmarc"] != "verified" || ov["dkim_rotation"] != "pending" || ov["ed25519"] != "off" ||
		ov["auth_issues"] != float64(1) || ov["inboxes"] != float64(1) || ov["inbound"] != "missing" || ov["claim_expires_at"] != nil {
		t.Errorf("ov.test: %v", ov)
	}
	problems := func(d map[string]any) []string {
		var out []string
		for _, p := range d["problems"].([]any) {
			out = append(out, p.(string))
		}
		return out
	}
	if p := problems(ov); len(p) != 3 || p[0] != "Inbound MX record not found: its inboxes receive no mail" ||
		p[1] != "DKIM rotation waiting for its DNS record" || p[2] != "1 authentication problem reported by receivers" {
		t.Errorf("ov.test problems: %q", p)
	}
	nd := doms["new.test"]
	if nd == nil || nd["status"] != "pending" || nd["claim_expires_at"] == nil || nd["inbound"] != "pending" {
		t.Errorf("new.test: %v", nd)
	}
	if p := problems(nd); len(p) != 2 || p[0] != "Not verified" || p[1] != "Removed in 6 days unless verified" {
		t.Errorf("new.test problems: %q", p)
	}

	recv := v["receiving"].(map[string]any)
	if recv["enabled"] != true || recv["inbound_mx"] != "mx.openmailwire.test" || recv["inboxes"] != float64(1) ||
		recv["messages_24h"] != float64(1) || recv["held_messages"] != float64(1) {
		t.Errorf("receiving: %v", recv)
	}

	workers := map[string]map[string]any{}
	for _, x := range v["workers"].([]any) {
		w := x.(map[string]any)
		workers[w["name"].(string)] = w
	}
	w1 := workers["w1"]
	if w1 == nil || w1["online"] != true || w1["ip"] != "198.51.100.231" || w1["kind"] != "direct" || w1["draining"] != false || w1["last_seen_at"] == nil {
		t.Fatalf("w1: %v", w1)
	}
	sc := w1["self_check"].(map[string]any)
	if sc["ok"] != false || len(sc["problems"].([]any)) != 1 || sc["problems"].([]any)[0] != "reverse_dns: no PTR record" {
		t.Errorf("w1 self_check: %v", sc)
	}
	wu, _ := w1["warmup"].(map[string]any)
	if wu == nil || wu["enabled"] != true || wu["started"] != false || wu["warm"] != false || wu["remaining"] != nil {
		t.Errorf("w1 warmup (before its first lease): %v", w1["warmup"])
	}
	if mx1 := workers["mx1"]; mx1 == nil || mx1["role"] != "inbound" || mx1["warmup"] != nil || mx1["self_check"] != nil {
		t.Errorf("mx1: %v", mx1)
	}

	// A warming IP reports its remaining capacity for today.
	exec(`INSERT INTO sending_ips (ip, warmup_started) VALUES ('198.51.100.231', (now() AT TIME ZONE 'UTC')::date - 1)`)
	exec(`INSERT INTO warmup_usage (ip, day, provider, sent) VALUES ('198.51.100.231', (now() AT TIME ZONE 'UTC')::date, 'google', 7)`)
	v = overviewCall(t, e.harness)
	for _, x := range v["workers"].([]any) {
		w := x.(map[string]any)
		if w["name"] != "w1" {
			continue
		}
		wu := w["warmup"].(map[string]any)
		capacity, _ := wu["cap"].(float64)
		left, _ := wu["remaining"].(float64)
		if wu["started"] != true || wu["warm"] != false || wu["day"] != float64(2) || capacity <= 0 || left != capacity-7 ||
			len(wu["providers"].([]any)) == 0 {
			t.Errorf("w1 warming: %v", wu)
		}
	}

	// Sending-only keys cannot see it.
	if code, b := agent.call("GET", "/overview", "", nil); code != 403 || b["name"] != "insufficient_permission" {
		t.Errorf("sending key: %d %v", code, b)
	}
}

// TestOverviewThreadFilter checks GET /emails?thread_id: the email that
// started a conversation and the replies sent in it, newest first.
func TestOverviewThreadFilter(t *testing.T) {
	e := overviewEnv(t)
	e.addReceivingDomain("th.test")
	inbox := e.createInbox("support@th.test")
	mx := e.startInbound()

	code, first := e.call("POST", "/emails", "", map[string]any{"from": "support@th.test", "to": []string{"maria@example.test"}, "subject": "Order", "text": "Shipped."})
	if code != 200 {
		t.Fatalf("send: %d %v", code, first)
	}
	e1 := first["id"].(string)
	if code, b := e.call("POST", "/emails", "", map[string]any{"from": "support@th.test", "to": []string{"x@example.test"}, "subject": "Other", "text": "x"}); code != 200 {
		t.Fatalf("send: %d %v", code, b)
	}
	if err := inboundSend(t, mx, "maria@example.test", []string{"support@th.test"},
		customerReply("reply-1@example.test", e1+"@th.test", "<"+e1+"@th.test>", "Re: Order", "Thanks")); err != nil {
		t.Fatal(err)
	}
	m := e.waitMessages(inbox, 1)[0].(map[string]any)
	if m["thread_id"] != e1 {
		t.Fatalf("message thread: %v", m)
	}
	code, reply := e.call("POST", "/messages/"+m["id"].(string)+"/reply", "", map[string]any{"text": "You're welcome."})
	if code != 200 {
		t.Fatalf("reply: %d %v", code, reply)
	}
	r1 := reply["id"].(string)

	code, list := e.call("GET", "/emails?thread_id="+e1, "", nil)
	data, _ := list["data"].([]any)
	if code != 200 || len(data) != 2 || data[0].(map[string]any)["id"] != r1 || data[1].(map[string]any)["id"] != e1 || list["has_more"] != false ||
		list["thread_id"] != e1 {
		t.Fatalf("thread: %d %v", code, list)
	}
	// Paging works inside a thread.
	code, page := e.call("GET", "/emails?limit=1&thread_id="+e1, "", nil)
	if code != 200 || len(page["data"].([]any)) != 1 || page["has_more"] != true || page["next_before"] != r1 {
		t.Fatalf("thread page 1: %d %v", code, page)
	}
	code, page = e.call("GET", "/emails?limit=1&thread_id="+e1+"&before="+r1, "", nil)
	if code != 200 || len(page["data"].([]any)) != 1 || page["data"].([]any)[0].(map[string]any)["id"] != e1 {
		t.Fatalf("thread page 2: %d %v", code, page)
	}
	if code, b := e.call("GET", "/emails?thread_id=not-a-uuid", "", nil); code != 422 || b["name"] != "validation_error" {
		t.Errorf("bad thread_id: %d %v", code, b)
	}
	// A sending-only key sees only its own emails, in threads too.
	agent, _ := keysNewKey(e, "agent", "sending_access", nil)
	if code, b := agent.call("GET", "/emails?thread_id="+e1, "", nil); code != 200 || len(b["data"].([]any)) != 0 {
		t.Errorf("sending key, thread: %d %v", code, b)
	}
}
