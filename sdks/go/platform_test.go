package openmailwire

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests of the v0.6.0 API: approvals, checks, suppressions, pauses, the
// project, the audit log, insights, DKIM, trust fields and retries.

func TestSendStreamAndHeld(t *testing.T) {
	c, m := serve(t, func(recorded) *reply {
		return ok(map[string]any{"id": testID, "held": true, "hold_reasons": []string{HoldFirstContact}})
	})
	res, err := c.Emails.Send(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", Text: "x", Stream: StreamBroadcast})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, *res, SendResponse{ID: testID, Held: true, HoldReasons: []string{"first_contact"}})
	eq(t, m.last().json(t).(map[string]any)["stream"], "broadcast")
}

func TestCheck(t *testing.T) {
	c, m := serve(t, func(r recorded) *reply {
		if r.Method == "POST" && r.Path == "/emails/check" {
			return ok(map[string]any{
				"object": "email_check", "ok": false, "would": "refuse", "stream": "transactional", "size": 812,
				"hold_reasons": []string{}, "refusals": []any{map[string]any{"statusCode": 403, "name": "sending_paused", "message": "m"}},
				"warnings": []any{map[string]any{"code": "no_text_part", "message": "add text"}},
			})
		}
		return nil
	})
	res, err := c.Emails.Check(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", HTML: "<p>x</p>"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Would, "refuse")
	eq(t, res.Size, int64(812))
	eq(t, res.Refusals[0].Name, ErrorNameSendingPaused)
	eq(t, res.Warnings[0].Code, "no_text_part")
	eq(t, m.last().json(t), map[string]any{"from": "a@acme.test", "to": []any{"b@example.test"}, "subject": "s", "html": "<p>x</p>"})
}

func TestHeldApproveRejectReplies(t *testing.T) {
	decision := func(d string) *reply {
		return ok(map[string]any{"object": "email", "id": testID, "decision": d, "recipients": []string{"b@example.test"}, "suppressed": []string{}})
	}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/emails/held":
			return ok(map[string]any{"object": "list", "data": []any{map[string]any{
				"object": "held_email", "id": testID, "api_key_id": nil, "hold_reasons": []string{"has_links"},
				"created_at": "2026-11-18T10:00:00Z", "expires_at": "2026-11-20T10:00:00Z", "scheduled_at": nil, "text": "hello",
			}}, "has_more": false, "next_before": nil})
		case r.Method == "POST" && r.Path == "/emails/"+testID+"/approve":
			return decision("approved")
		case r.Method == "POST" && r.Path == "/emails/"+testID+"/reject":
			return decision("rejected")
		case r.Method == "GET" && r.Path == "/emails/"+testID+"/replies":
			return ok(map[string]any{"object": "list", "email_id": testID, "thread_id": "t1", "data": []any{
				map[string]any{"id": "m1", "received_at": "2026-11-18T11:00:00Z", "trust": "authenticated", "in_reply_to_email_id": testID},
			}})
		}
		return nil
	})
	held, err := c.Emails.ListHeld(ctx, &ListHeldParams{Limit: 5, Before: "e0"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, held.Data[0].HoldReasons, []string{HoldHasLinks})
	eq(t, held.Data[0].Text, "hello")
	eq(t, m.last().Query, "before=e0&limit=5")
	if _, err := c.Emails.ListHeld(ctx, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().Query, "")

	d, err := c.Emails.Approve(ctx, testID, &DecisionRequest{Note: "looks fine"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, d.Decision, "approved")
	eq(t, m.last().json(t), map[string]any{"note": "looks fine"})
	if _, err := c.Emails.Approve(ctx, testID, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, string(m.last().Body), "{}")
	if d, err = c.Emails.Reject(ctx, testID, &DecisionRequest{Note: "wrong customer"}); err != nil || d.Decision != "rejected" {
		t.Fatal(d, err)
	}
	eq(t, m.last().json(t), map[string]any{"note": "wrong customer"})

	replies, err := c.Emails.Replies(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, replies.ThreadID, "t1")
	eq(t, replies.Data[0].Trust, TrustAuthenticated)
}

func TestHeldIsNotFinal(t *testing.T) {
	eq(t, Recipient{Status: StatusHeld}.Final(), false)
	eq(t, Recipient{Status: StatusRejected}.Final(), true)
}

// ------------------------------------------------------------------ suppressions, pauses, project

func TestSuppressions(t *testing.T) {
	row := map[string]any{"address": "x@example.test", "stream": "all", "reason": "manual", "detail": "", "created_at": "2026-01-01T00:00:00Z"}
	csv := "address,stream,reason,detail,created_at\nx@example.test,all,manual,,2026-01-01T00:00:00Z\n"
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/suppressions":
			return ok(map[string]any{"object": "list", "data": []any{row}, "has_more": true, "next_before": "c1"})
		case r.Method == "POST" && r.Path == "/suppressions":
			created := map[string]any{"object": "suppression", "created": true}
			for k, v := range row {
				created[k] = v
			}
			return &reply{status: 201, body: created}
		case r.Method == "DELETE" && r.Path == "/suppressions/x@example.test":
			return ok(map[string]any{"object": "suppression", "address": "x@example.test", "deleted": true, "removed": []any{row}})
		case r.Method == "GET" && r.Path == "/suppressions/export":
			return &reply{status: 200, body: csv, headers: map[string]string{"Content-Type": "text/csv"}}
		}
		return nil
	})
	page, err := c.Suppressions.List(ctx, &ListSuppressionsParams{Limit: 2, Before: "c0", SuppressionFilter: SuppressionFilter{
		Reason: "hard_bounce", Stream: SuppressBroadcast, Address: "x@example.test", Domain: "example.test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, page.NextBefore, "c1")
	eq(t, page.Data[0].Address, "x@example.test")
	eq(t, m.last().Query, "address=x%40example.test&before=c0&domain=example.test&limit=2&reason=hard_bounce&stream=broadcast")

	created, err := c.Suppressions.Create(ctx, &CreateSuppressionRequest{Address: "x@example.test", Reason: "customer_request", Detail: "by phone", Stream: SuppressAll})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, created.Created, true)
	eq(t, created.Address, "x@example.test")
	eq(t, m.last().json(t), map[string]any{"address": "x@example.test", "reason": "customer_request", "detail": "by phone", "stream": "all"})
	if _, err := c.Suppressions.Create(ctx, &CreateSuppressionRequest{Address: "x@example.test"}); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"address": "x@example.test"})

	del, err := c.Suppressions.Delete(ctx, "x@example.test", SuppressBroadcast)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(del.Removed), 1)
	eq(t, m.last().Query, "stream=broadcast")
	if _, err := c.Suppressions.Delete(ctx, "x@example.test", ""); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().Query, "")

	got, err := c.Suppressions.Export(ctx, &SuppressionFilter{Reason: "complaint"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got, csv)
	eq(t, m.last().Query, "reason=complaint")
	if !strings.Contains(m.last().Header.Get("Accept"), "text/csv") {
		t.Fatalf("Accept %q", m.last().Header.Get("Accept"))
	}
	if got, err = c.Suppressions.Export(ctx, nil); err != nil || got != csv {
		t.Fatal(got, err)
	}
}

func TestPauses(t *testing.T) {
	pause := map[string]any{"object": "pause", "id": "p1", "scope": "domain", "scope_id": "d1", "reason": "bounce_rate", "rate": 0.09, "threshold": 0.05, "created_at": "2026-11-18T10:00:00Z", "active": true}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/pauses":
			return ok(map[string]any{"object": "list", "data": []any{pause}})
		case r.Method == "POST" && r.Path == "/pauses/p1/resume":
			resumed := map[string]any{}
			for k, v := range pause {
				resumed[k] = v
			}
			resumed["active"], resumed["resumed_by"] = false, "k1"
			return ok(resumed)
		}
		return nil
	})
	list, err := c.Pauses.List(ctx, &ListPausesParams{Active: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, list.Data[0].Scope, "domain")
	eq(t, list.Data[0].Rate, 0.09)
	eq(t, m.last().Query, "active=true&limit=10")
	if _, err := c.Pauses.List(ctx, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().Query, "")
	p, err := c.Pauses.Resume(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, p.Active, false)
	eq(t, p.ResumedBy, "k1")
	eq(t, m.last().Method, "POST")
	eq(t, len(m.last().Body), 0)
}

func TestProject(t *testing.T) {
	project := map[string]any{"object": "project", "id": "pr1", "name": "acme", "max_per_recipient_per_day": 5, "circuit_breaker": map[string]any{"enabled": true, "bounce_rate": 0.05}}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/project":
			return ok(project)
		case r.Method == "PATCH" && r.Path == "/project":
			return ok(project)
		}
		return nil
	})
	p, err := c.Project.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, *p.MaxPerRecipientPerDay, 5)
	eq(t, *p.CircuitBreaker.BounceRate, 0.05)

	if _, err := c.Project.Update(ctx, &UpdateProjectRequest{
		MaxPerRecipientPerDay: Ptr(3),
		CircuitBreaker:        &CircuitBreaker{BounceRate: Ptr(0.02), MinRecipients: Ptr(100)},
	}); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"max_per_recipient_per_day": float64(3), "circuit_breaker": map[string]any{"bounce_rate": 0.02, "min_recipients": float64(100)}})
	if _, err := c.Project.Update(ctx, &UpdateProjectRequest{ClearMaxPerRecipientPerDay: true}); err != nil {
		t.Fatal(err)
	}
	eq(t, string(m.last().Body), `{"max_per_recipient_per_day":null}`)
	if _, err := c.Project.Update(ctx, &UpdateProjectRequest{CircuitBreaker: &CircuitBreaker{Enabled: Ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	eq(t, string(m.last().Body), `{"circuit_breaker":{"enabled":false}}`)
}

// ------------------------------------------------------------------ audit log, insights

func TestAuditLog(t *testing.T) {
	c, m := serve(t, func(r recorded) *reply {
		switch r.Path {
		case "/audit-log":
			return ok(map[string]any{"object": "list", "data": []any{map[string]any{
				"object": "audit_entry", "id": 7, "project_id": "pr1", "created_at": "2026-11-18T10:00:00Z", "actor": "api_key",
				"actor_key_id": nil, "action": "api_key.created", "data": map[string]any{"name": "agent"}, "entry": "{}", "prev_hash": "00", "hash": "ab",
			}}, "has_more": true, "next_before": "7"})
		case "/audit-log/verify":
			return ok(map[string]any{"object": "audit_verification", "valid": true, "entries": 7, "head_hash": "ab", "first_invalid_id": nil})
		}
		return nil
	})
	page, err := c.AuditLog.List(ctx, &ListAuditLogParams{Limit: 1, Before: "9", Action: "api_key.created"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, page.Data[0].ID, int64(7))
	eq(t, string(page.Data[0].Data), `{"name":"agent"}`)
	eq(t, page.NextBefore, "7")
	eq(t, m.last().Query, "action=api_key.created&before=9&limit=1")
	v, err := c.AuditLog.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, v.Valid, true)
	eq(t, v.HeadHash, "ab")
	if v.FirstInvalidID != nil {
		t.Fatal(v.FirstInvalidID)
	}
}

func TestInsights(t *testing.T) {
	window := func(name string) map[string]any {
		return map[string]any{"window": name, "total": map[string]any{"attempts": 2, "accepted": 1, "acceptance_rate": 0.5}, "by_provider": []any{map[string]any{"provider": "google", "attempts": 2}}, "by_sending_ip": []any{}}
	}
	c, m := serve(t, func(r recorded) *reply {
		if r.Path == "/insights" {
			return ok(map[string]any{"object": "insights", "generated_at": "2026-11-18T10:00:00Z", "emails_considered": 2, "truncated": false, "windows": []any{window("24h"), window("7d"), window("30d")}})
		}
		return nil
	})
	all, err := c.Insights.Get(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(all.Windows), 3)
	eq(t, *all.Windows[0].Total.AcceptanceRate, 0.5)
	eq(t, all.Windows[0].ByProvider[0].Provider, "google")
	week, err := c.Insights.Get(ctx, &InsightsParams{Window: Window7d})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(week.Windows), 1)
	eq(t, week.Windows[0].Window, "7d")
	eq(t, m.last().Query, "")
}

// ------------------------------------------------------------------ domains, webhooks, keys, inboxes

func TestDomainDKIMAndReports(t *testing.T) {
	state := func(object, status string) map[string]any {
		return map[string]any{"object": object, "status": status, "keys": []any{map[string]any{"selector": "omw2", "algorithm": "rsa", "state": "pending", "signs": false, "record": map[string]any{"record": "DKIM_NEXT", "type": "TXT"}}}}
	}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/domains/"+testID+"/dkim":
			return ok(state("dkim", ""))
		case r.Method == "POST" && r.Path == "/domains/"+testID+"/dkim/rotate":
			return &reply{status: 202, body: state("dkim_rotation", "pending")}
		case r.Method == "POST" && r.Path == "/domains/"+testID+"/dkim/ed25519":
			if strings.Contains(string(r.Body), "true") {
				return ok(state("dkim_ed25519", "active"))
			}
			return ok(state("dkim_ed25519", "disabled"))
		case r.Method == "GET" && r.Path == "/domains/"+testID+"/reports":
			return ok(map[string]any{"object": "domain_reports", "domain_id": testID, "domain": "acme.test", "days": 7, "dmarc": map[string]any{"pass": 3}, "tls": map[string]any{}, "reports": []any{}})
		case r.Method == "GET" && r.Path == "/domains/"+testID:
			return ok(map[string]any{"object": "domain", "id": testID, "created_at": "2026-11-18T10:00:00Z", "claim_expires_at": "2026-11-25T10:00:00Z", "report_addresses": map[string]any{"dmarc_rua": "mailto:d@rp.acme.test", "tls_rpt_record": map[string]any{"name": "_smtp._tls.acme.test", "value": "v=TLSRPTv1"}}})
		}
		return nil
	})
	st, err := c.Domains.DKIM(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, st.Keys[0].Selector, "omw2")
	eq(t, st.Keys[0].Record.Record, "DKIM_NEXT")
	if st, err = c.Domains.RotateDKIM(ctx, testID); err != nil || st.Status != "pending" {
		t.Fatal(st, err)
	}
	eq(t, m.last().Method, "POST")
	if st, err = c.Domains.SetEd25519(ctx, testID, true); err != nil || st.Status != "active" {
		t.Fatal(st, err)
	}
	eq(t, string(m.last().Body), `{"enabled":true}`)
	if st, err = c.Domains.SetEd25519(ctx, testID, false); err != nil || st.Status != "disabled" {
		t.Fatal(st, err)
	}
	eq(t, string(m.last().Body), `{"enabled":false}`)
	rep, err := c.Domains.Reports(ctx, testID, &DomainReportsParams{Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, rep.DMARC["pass"], float64(3))
	eq(t, m.last().Query, "days=7")
	if _, err := c.Domains.Reports(ctx, testID, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().Query, "")
	d, err := c.Domains.Get(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, d.ClaimExpiresAt.UTC(), time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	eq(t, d.ReportAddresses.DMARCRUA, "mailto:d@rp.acme.test")
	eq(t, d.ReportAddresses.TLSRPTRecord.Value, "v=TLSRPTv1")
}

func TestWebhookEd25519AndRotation(t *testing.T) {
	hook := map[string]any{"object": "webhook", "id": testID, "endpoint": "https://h.test/x", "events": nil, "created_at": "2026-11-18T10:00:00Z", "asymmetric_signing": true, "public_key": "whpk_abc"}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "POST" && r.Path == "/webhooks":
			return &reply{status: 201, body: hook}
		case r.Method == "GET" && r.Path == "/webhooks/"+testID:
			return ok(hook)
		case r.Method == "POST" && r.Path == "/webhooks/"+testID+"/rotate-secret":
			return ok(map[string]any{"object": "webhook", "id": testID, "signing_secret": "whsec_2", "previous_public_key": "whpk_abc", "previous_secret_expires_at": "2026-11-19T10:00:00Z"})
		}
		return nil
	})
	if _, err := c.Webhooks.Create(ctx, &CreateWebhookRequest{Endpoint: "https://h.test/x", Events: []string{EventEmailHeld, EventSendingPaused}, AsymmetricSigning: true}); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"endpoint": "https://h.test/x", "events": []any{"email.held", "sending.paused"}, "asymmetric_signing": true})
	w, err := c.Webhooks.Get(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, w.PublicKey, "whpk_abc")
	eq(t, w.AsymmetricSigning, true)
	w, err = c.Webhooks.RotateSecret(ctx, testID, &RotateWebhookSecretRequest{OverlapSeconds: Ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, w.SigningSecret, "whsec_2")
	eq(t, w.PreviousPublicKey, "whpk_abc")
	eq(t, string(m.last().Body), `{"overlap_seconds":0}`)
	if _, err := c.Webhooks.RotateSecret(ctx, testID, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, string(m.last().Body), `{}`)
}

func TestAPIKeyIPsExpiryAndApprovalPolicy(t *testing.T) {
	c, m := serve(t, func(r recorded) *reply {
		return ok(map[string]any{"id": testID, "allowed_ips": []string{"10.0.0.0/8"}, "expires_at": "2031-01-01T00:00:00Z", "created_at": "2026-11-18T10:00:00Z"})
	})
	exp := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	key, err := c.APIKeys.Create(ctx, &CreateAPIKeyRequest{
		Name:       "agent",
		Permission: SendingAccess,
		Policy: &APIKeyPolicy{
			RequireApproval: &RequireApproval{FirstContact: true, RecipientsOver: Ptr(5)},
			Inboxes:         []string{"support@acme.test"},
		},
		AllowedIPs: []string{"10.0.0.0/8"},
		ExpiresAt:  &exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, key.AllowedIPs, []string{"10.0.0.0/8"})
	eq(t, key.ExpiresAt.UTC(), exp)
	eq(t, m.last().json(t), map[string]any{
		"name":        "agent",
		"permission":  "sending_access",
		"policy":      map[string]any{"require_approval": map[string]any{"first_contact": true, "recipients_over": float64(5)}, "inboxes": []any{"support@acme.test"}},
		"allowed_ips": []any{"10.0.0.0/8"},
		"expires_at":  "2031-01-01T00:00:00Z",
	})
	if _, err := c.APIKeys.Update(ctx, testID, &UpdateAPIKeyRequest{ClearAllowedIPs: true, ClearExpiresAt: true}); err != nil {
		t.Fatal(err)
	}
	eq(t, string(m.last().Body), `{"allowed_ips":null,"expires_at":null}`)
	if _, err := c.APIKeys.Update(ctx, testID, &UpdateAPIKeyRequest{Policy: &APIKeyPolicy{CanApprove: true}, AllowedIPs: []string{"203.0.113.7"}, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"policy": map[string]any{"can_approve": true}, "allowed_ips": []any{"203.0.113.7"}, "expires_at": "2031-01-01T00:00:00Z"})
	if _, err := c.APIKeys.Update(ctx, testID, &UpdateAPIKeyRequest{Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	eq(t, string(m.last().Body), `{"name":"renamed"}`)
}

func TestInboxGetUpdateAndHold(t *testing.T) {
	inbox := map[string]any{"object": "inbox", "id": "i1", "address": "agent@acme.test", "created_at": "2026-11-18T10:00:00Z", "domain_id": "d1", "hold_unauthenticated": true}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "POST" && r.Path == "/inboxes":
			return &reply{status: 201, body: inbox}
		case r.Method == "GET" && r.Path == "/inboxes/i1":
			return ok(inbox)
		case r.Method == "PATCH" && r.Path == "/inboxes/i1":
			return ok(map[string]any{"object": "inbox", "id": "i1", "address": "agent@acme.test", "created_at": "2026-11-18T10:00:00Z", "hold_unauthenticated": false})
		}
		return nil
	})
	if _, err := c.Inboxes.Create(ctx, "agent@acme.test", HoldUnauthenticated(false)); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"address": "agent@acme.test", "hold_unauthenticated": false})
	if _, err := c.Inboxes.Create(ctx, "agent@acme.test"); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"address": "agent@acme.test"})
	in, err := c.Inboxes.Get(ctx, "i1")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, in.HoldUnauthenticated, true)
	eq(t, in.DomainID, "d1")
	if in, err = c.Inboxes.Update(ctx, "i1", &UpdateInboxRequest{HoldUnauthenticated: false}); err != nil || in.HoldUnauthenticated {
		t.Fatal(in, err)
	}
	eq(t, string(m.last().Body), `{"hold_unauthenticated":false}`)
}

func TestMessageTrustReleaseAndReplyTo(t *testing.T) {
	msg := map[string]any{
		"id": "m1", "received_at": "2026-11-18T10:00:00Z", "trust": "unauthenticated", "held": true, "suspicious_instructions": true,
		"suspicious_rules": []string{"override_instructions"}, "authentication_results": map[string]any{"dmarc": map[string]any{"result": "none"}},
	}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/messages/m1":
			return ok(msg)
		case r.Method == "POST" && r.Path == "/messages/m1/release":
			return ok(map[string]any{"id": "m1", "received_at": "2026-11-18T10:00:00Z", "held": false, "released_at": "2026-11-18T10:05:00Z"})
		case r.Method == "POST" && r.Path == "/messages/m1/reply":
			return ok(map[string]any{"id": testID, "thread_id": "t1", "to": []string{"a@example.test", "b@example.test"}, "warnings": []string{"other domain"}})
		}
		return nil
	})
	got, err := c.Messages.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, got.Trust, TrustUnauthenticated)
	eq(t, got.Held, true)
	eq(t, got.SuspiciousInstructions, true)
	eq(t, got.SuspiciousRules, []string{"override_instructions"})
	eq(t, string(got.AuthenticationResults), `{"dmarc":{"result":"none"}}`)
	rel, err := c.Messages.Release(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, rel.Held, false)
	eq(t, rel.ReleasedAt != nil, true)
	eq(t, m.last().Method, "POST")
	sent, err := c.Messages.Reply(ctx, "m1", &ReplyRequest{Text: "thanks", To: []string{"a@example.test", "b@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, sent.ID, testID)
	eq(t, sent.ThreadID, "t1")
	eq(t, sent.To, []string{"a@example.test", "b@example.test"})
	eq(t, sent.Warnings, []string{"other domain"})
	eq(t, m.last().json(t), map[string]any{"text": "thanks", "to": []any{"a@example.test", "b@example.test"}})
}

func TestParseWebhookV06Events(t *testing.T) {
	for _, body := range []string{
		`{"type":"email.held","created_at":"2026-11-18T10:00:00Z","data":{"email_id":"e1","to":["b@example.test"],"approval":{"reasons":["first_contact"]}}}`,
		`{"type":"sending.paused","created_at":"2026-11-18T10:00:00Z","data":{"pause":{"id":"p1","scope":"domain","reason":"bounce_rate","rate":0.09}}}`,
		`{"type":"email.received","created_at":"2026-11-18T10:00:00Z","data":{"email_id":"m1","to":["agent@acme.test"],"header_to":["team@acme.test"],"trust":"failed","authentication":{"spf":"fail","dkim":"none","dmarc":"fail"},"held":true,"suspicious_instructions":true}}`,
	} {
		ts := time.Now().Unix()
		h := http.Header{}
		h.Set("webhook-id", "msg_1")
		h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		h.Set("webhook-signature", sign(testSecret, "msg_1", ts, []byte(body)))
		ev, err := ParseWebhook(testSecret, h, []byte(body), 0)
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case EventEmailHeld:
			eq(t, ev.Data.Approval.Reasons, []string{HoldFirstContact})
		case EventSendingPaused:
			eq(t, ev.Data.Pause.Scope, "domain")
			eq(t, ev.Data.Pause.Rate, 0.09)
		case EventEmailReceived:
			eq(t, ev.Data.HeaderTo, []string{"team@acme.test"})
			eq(t, ev.Data.Trust, TrustFailed)
			eq(t, ev.Data.Authentication.DMARC, "fail")
			eq(t, ev.Data.Held, true)
			eq(t, ev.Data.SuspiciousInstructions, true)
		default:
			t.Fatalf("unexpected type %q", ev.Type)
		}
	}
}

// ------------------------------------------------------------------ errors and retries

func TestRetryAfterOnRecipientCapAndServerBusy(t *testing.T) {
	for _, tc := range []struct {
		status int
		name   string
	}{{429, ErrorNameRecipientRateLimited}, {503, ErrorNameServerBusy}} {
		c, m := serve(t, func(recorded) *reply {
			return &reply{status: tc.status, headers: map[string]string{"Retry-After": "10"}, body: map[string]any{"statusCode": tc.status, "name": tc.name, "message": "wait"}}
		})
		_, err := c.Emails.Send(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", Text: "x"})
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatal(err)
		}
		eq(t, apiErr.StatusCode, tc.status)
		eq(t, apiErr.RetryAfter, 10*time.Second)
		eq(t, apiErr.Retryable(), true)
		eq(t, IsError(err, tc.name), true)
		eq(t, IsError(fmt.Errorf("wrapped: %w", err), tc.name), true)
		eq(t, len(m.all()), 1) // no retry by default
	}
	eq(t, (&Error{StatusCode: 422}).Retryable(), false)
	eq(t, IsError(errors.New("x"), ErrorNameSendingPaused), false)
}

func TestErrorNames(t *testing.T) {
	for got, want := range map[string]string{
		ErrorNameSendingPaused:           "sending_paused",
		ErrorNameInsufficientPermission:  "insufficient_permission",
		ErrorNameCannotApproveOwnEmail:   "cannot_approve_own_email",
		ErrorNameServerBusy:              "server_busy",
		ErrorNameDomainVerifiedElsewhere: "domain_verified_elsewhere",
		ErrorNameIPNotAllowed:            "ip_not_allowed",
		ErrorNameAPIKeyExpired:           "api_key_expired",
		ErrorNameWebhookLimitReached:     "webhook_limit_reached",
		ErrorNameRecipientRateLimited:    "recipient_rate_limited",
	} {
		eq(t, got, want)
	}
}

func TestMaxRetries(t *testing.T) {
	answers := []*reply{
		{status: 503, headers: map[string]string{"Retry-After": "0"}, body: map[string]any{"statusCode": 503, "name": "server_busy", "message": "busy"}},
		{status: 429, headers: map[string]string{"Retry-After": "0"}, body: map[string]any{"statusCode": 429, "name": "rate_limit_exceeded", "message": "slow"}},
		ok(map[string]string{"id": testID}),
	}
	var m *mock
	var base *Client
	base, m = serve(t, func(recorded) *reply { return answers[len(m.all())-1] })
	c, err := New(Config{APIKey: testKey, BaseURL: base.BaseURL, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Emails.Send(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", Text: "x"}, IdempotencyKey("k1"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.ID, testID)
	all := m.all()
	eq(t, len(all), 3)
	for _, r := range all {
		eq(t, r.Header.Get("Idempotency-Key"), "k1")
		eq(t, r.json(t).(map[string]any)["subject"], "s")
	}
}

func TestMaxRetriesGivesUp(t *testing.T) {
	busy := map[string]any{"statusCode": 503, "name": "server_busy", "message": "busy"}
	base, m := serve(t, func(recorded) *reply {
		return &reply{status: 503, headers: map[string]string{"Retry-After": "0"}, body: busy}
	})
	c, _ := New(Config{APIKey: testKey, BaseURL: base.BaseURL, MaxRetries: 1})
	if _, err := c.Domains.List(ctx); !IsError(err, ErrorNameServerBusy) {
		t.Fatal(err)
	}
	eq(t, len(m.all()), 2)

	base, m = serve(t, func(recorded) *reply {
		return &reply{status: 503, headers: map[string]string{"Retry-After": "3600"}, body: busy}
	})
	c, _ = New(Config{APIKey: testKey, BaseURL: base.BaseURL, MaxRetries: 3, MaxRetryWait: 5 * time.Second})
	_, err := c.Domains.List(ctx)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Hour {
		t.Fatal(err)
	}
	eq(t, len(m.all()), 1)
}
