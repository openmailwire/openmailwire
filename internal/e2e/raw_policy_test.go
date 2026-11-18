package e2e

import (
	"encoding/base64"
	"strings"
	"testing"
)

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
