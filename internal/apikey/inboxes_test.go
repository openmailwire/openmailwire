package apikey

import (
	"strings"
	"testing"
)

// Audit R2 M3: sending_access keys read every inbox. They now read only
// the inboxes policy.inboxes lists.
func TestPolicyInboxes(t *testing.T) {
	var none Policy
	if none.AllowsInbox("support@acme.com") {
		t.Fatal("a policy without inboxes allows an inbox")
	}
	p, err := Parse([]byte(`{"inboxes": ["Support@ACME.com", "*@agents.acme.com", "support@acme.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Inboxes) != 2 || p.Inboxes[0] != "support@acme.com" {
		t.Fatalf("normalized: %v", p.Inboxes)
	}
	for addr, want := range map[string]bool{
		"support@acme.com": true, "SUPPORT@acme.com": true, "bot@agents.acme.com": true, "sales@acme.com": false, "x@deep.agents.acme.com": false,
	} {
		if got := p.AllowsInbox(addr); got != want {
			t.Errorf("AllowsInbox(%s) = %v", addr, got)
		}
	}
	if _, err := Parse([]byte(`{"inboxes": []}`)); err == nil || !strings.Contains(err.Error(), "sees no inbox") {
		t.Fatalf("empty inboxes: %v", err)
	}
	if _, err := Parse([]byte(`{"inboxes": ["not an address"]}`)); err == nil {
		t.Fatal("bad pattern accepted")
	}
}
