package e2e

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestInboxPermissions checks the seam between agent keys (lane A) and
// inboxes (lane B): a sending_access key can read inboxes but not manage
// them, and sees replies only to its own emails.
func TestInboxPermissions(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("inbox.test")
	code, inbox := e.call("POST", "/inboxes", "", map[string]any{"address": "support@inbox.test"})
	if code != 201 {
		t.Fatalf("full key creates an inbox: %d %v", code, inbox)
	}
	code, k := e.call("POST", "/api-keys", "", map[string]any{"name": "agent", "permission": "sending_access"})
	if code != 201 {
		t.Fatalf("create key: %d %v", code, k)
	}
	agent := &harness{t: t, st: e.st, url: e.ts.URL, apiKey: k["token"].(string)}

	if code, b := agent.call("POST", "/inboxes", "", map[string]any{"address": "sales@inbox.test"}); code != 403 || b["name"] != "insufficient_permission" {
		t.Fatalf("sending key created an inbox: %d %v", code, b)
	}
	if code, b := agent.call("DELETE", "/inboxes/"+inbox["id"].(string), "", nil); code != 403 {
		t.Fatalf("sending key deleted an inbox: %d %v", code, b)
	}
	if code, b := agent.call("GET", "/inboxes", "", nil); code != 200 {
		t.Fatalf("sending key lists inboxes: %d %v", code, b)
	}

	// An email sent with the full key: the agent may not read its replies.
	code, sent := e.call("POST", "/emails", "", map[string]any{"from": "support@inbox.test", "to": "c@example.test", "subject": "s", "text": "t"})
	if code != 200 {
		t.Fatalf("send: %d %v", code, sent)
	}
	if code, _ := e.call("GET", "/emails/"+sent["id"].(string)+"/replies", "", nil); code != 200 {
		t.Fatalf("full key reads replies: %d", code)
	}
	if code, _ := agent.call("GET", "/emails/"+sent["id"].(string)+"/replies", "", nil); code != 404 {
		t.Fatalf("sending key read replies to another key's email: %d", code)
	}
}

// TestRawSendFollowsKeyPolicy checks the seam between agent keys (lane A)
// and raw/SMTP sending (lane D): a raw message obeys the key's allowlist
// and is recorded under the key that sent it.
func TestRawSendFollowsKeyPolicy(t *testing.T) {
	e := newEnv(t, nil)
	e.addVerifiedDomain("raw.test")
	code, k := e.call("POST", "/api-keys", "", map[string]any{
		"name": "agent", "permission": "sending_access",
		"policy": map[string]any{"allowed_recipients": []string{"*@example.test"}},
	})
	if code != 201 {
		t.Fatalf("create key: %d %v", code, k)
	}
	agent := &harness{t: t, st: e.st, url: e.ts.URL, apiKey: k["token"].(string)}

	raw := "From: Agent <a@raw.test>\r\nTo: x@elsewhere.test\r\nSubject: s\r\n\r\nhi\r\n"
	code, b := agent.call("POST", "/emails/raw", "", map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(raw))})
	if code != 403 || b["name"] != "recipient_not_allowed" {
		t.Fatalf("raw send outside the allowlist: %d %v", code, b)
	}

	raw = strings.Replace(raw, "elsewhere.test", "example.test", 1)
	code, b = agent.call("POST", "/emails/raw", "", map[string]any{"raw": base64.StdEncoding.EncodeToString([]byte(raw))})
	if code != 200 {
		t.Fatalf("raw send inside the allowlist: %d %v", code, b)
	}
	_, v := agent.call("GET", "/emails/"+b["id"].(string), "", nil)
	if v["api_key_id"] != k["id"] {
		t.Fatalf("raw email not recorded under its key: got %v, want %v", v["api_key_id"], k["id"])
	}
}
