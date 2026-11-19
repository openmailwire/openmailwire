package apidocs

import (
	"strings"
	"testing"

	"github.com/openmailwire/openmailwire/internal/webhook"
)

// mcpTools are the MCP tools llms.txt teaches: today's and the contract's.
var mcpTools = []string{
	"send_email", "get_email", "list_emails", "cancel_email",
	"list_domains", "add_domain", "verify_domain",
	"list_inboxes", "list_messages", "read_message", "reply_to_message", "wait_for_reply",
	"check_email", "list_held", "approve_email", "reject_email", "list_suppressions", "add_suppression", "list_pauses",
}

var deliveryStatuses = []string{"queued", "sending", "deferred", "delivered", "bounced", "suppressed", "expired", "unknown", "canceled",
	"held", "rejected"}

func TestLLMsTxt(t *testing.T) {
	text := string(llmsTxt)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	// The llms.txt convention: an H1, then a one-line blockquote summary,
	// then sections.
	if !strings.HasPrefix(lines[0], "# ") {
		t.Errorf("the first line must be a Markdown H1, got %q", lines[0])
	}
	quote := -1
	for i, l := range lines[1:] {
		if strings.HasPrefix(l, "> ") {
			quote = i + 1
			break
		}
		if strings.TrimSpace(l) != "" {
			t.Errorf("line %d: want the blockquote summary right after the title, got %q", i+2, l)
			break
		}
	}
	if quote < 0 {
		t.Error("no blockquote summary")
	}
	if !strings.Contains(text, "\n## ") {
		t.Error("no sections")
	}
	if len(lines) > 250 {
		t.Errorf("%d lines; keep it under about 250", len(lines))
	}

	for _, r := range publicRoutes {
		if !strings.Contains(text, "`"+r.method+" "+r.path+"`") {
			t.Errorf("llms.txt does not list `%s %s`", r.method, r.path)
		}
	}
	for name := range requiredErrors {
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("llms.txt does not explain the error %s", name)
		}
	}
	for _, s := range deliveryStatuses {
		if !strings.Contains(text, "- `"+s+"`:") {
			t.Errorf("llms.txt does not explain the status %s", s)
		}
	}
	for _, typ := range webhook.Types {
		if !strings.Contains(text, "`"+typ+"`") {
			t.Errorf("llms.txt does not list the webhook event %s", typ)
		}
	}
	for _, tool := range mcpTools {
		if !strings.Contains(text, "`"+tool+"`") {
			t.Errorf("llms.txt does not list the MCP tool %s", tool)
		}
	}
	for _, want := range []string{"Authorization: Bearer omw_", "Idempotency-Key", "openmailwire mcp", "/mcp", "svix-signature", "webhook-signature", "Retry-After"} {
		if !strings.Contains(text, want) {
			t.Errorf("llms.txt does not mention %s", want)
		}
	}
}
