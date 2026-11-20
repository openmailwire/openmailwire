package mcpserver

// Automatic DNS setup (lane G): publish a domain's records at Cloudflare
// with the token a person connected for it. The agent never sees or
// handles a DNS provider token: the tool has no field for one.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const dnsInstructions = `
DNS: setup_domain_dns publishes a domain's DNS records at Cloudflare, with a token a person connected for that domain earlier (it is never shown to you). Run it with dry_run first and tell the user what it would change, then without. If no token is connected, ask the user to connect Cloudflare in the dashboard; never ask for a Cloudflare token in the conversation.`

func addDNSTools(s *mcp.Server, t *tools) {
	if acc := t.c.Access; acc != nil && !acc.FullAccess {
		return
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:  "setup_domain_dns",
		Title: "Publish a domain's DNS records at Cloudflare",
		Description: "Create or update every DNS record a domain needs (DKIM, SPF and MX for the return path, DMARC, TLS reporting, and the inbox MX " +
			"when the server receives mail) at Cloudflare, then verify the domain. It uses the Cloudflare token a person connected for the domain; " +
			"you never pass or see a token. It never replaces the domain's own mail setup: existing MX records and DMARC policies are kept and reported. " +
			"Set dry_run to only see what would change. Needs a full_access key.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, t.setupDomainDNS)
}

type SetupDomainDNSInput struct {
	Domain string `json:"domain" jsonschema:"the domain name or its id"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"only report what would change; publish nothing"`
}

type dnsRunView struct {
	Zone struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		DNSSEC       string `json:"dnssec"`
		EmailRouting string `json:"email_routing"`
	} `json:"zone"`
	DryRun  bool `json:"dry_run"`
	Changes []struct {
		Record   string `json:"record"`
		Type     string `json:"type"`
		Name     string `json:"name"`
		Value    string `json:"value"`
		Priority int    `json:"priority"`
		Action   string `json:"action"`
		Detail   string `json:"detail"`
	} `json:"changes"`
	Warnings []string    `json:"warnings"`
	Domain   *domainView `json:"domain"`
}

// domainID resolves a domain name to its id (an id is returned as it is).
func (t *tools) domainID(ctx context.Context, domain string) (string, error) {
	if uuidRE.MatchString(domain) {
		return domain, nil
	}
	var list struct {
		Data []domainView `json:"data"`
	}
	if err := t.c.do(ctx, http.MethodGet, "/domains", nil, nil, &list); err != nil {
		return "", err
	}
	for _, d := range list.Data {
		if strings.EqualFold(d.Name, strings.TrimSuffix(domain, ".")) {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("domain %s is not added; call add_domain first", domain)
}

func (t *tools) setupDomainDNS(ctx context.Context, _ *mcp.CallToolRequest, in SetupDomainDNSInput) (*mcp.CallToolResult, any, error) {
	id, err := t.domainID(ctx, in.Domain)
	if err != nil {
		return nil, nil, err
	}
	var run dnsRunView
	if err := t.c.do(ctx, http.MethodPost, "/domains/"+id+"/dns/auto", map[string]any{"dry_run": in.DryRun}, nil, &run); err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	if run.DryRun {
		fmt.Fprintf(&b, "Dry run in Cloudflare zone %s: nothing was changed. These changes would be made:\n", run.Zone.Name)
	} else {
		fmt.Fprintf(&b, "Applied in Cloudflare zone %s:\n", run.Zone.Name)
	}
	for _, c := range run.Changes {
		fmt.Fprintf(&b, "- %s: %s %s at %s", c.Action, c.Record, c.Type, c.Name)
		if c.Value != "" && c.Action != "unchanged" {
			v := c.Value
			if len(v) > 80 {
				v = v[:77] + "…"
			}
			fmt.Fprintf(&b, " = %s", v)
		}
		if c.Detail != "" {
			fmt.Fprintf(&b, " (%s)", c.Detail)
		}
		b.WriteString("\n")
	}
	if len(run.Warnings) > 0 {
		b.WriteString("Warnings, for the user:\n")
		for _, w := range run.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	if d := run.Domain; d != nil {
		switch {
		case run.DryRun:
			fmt.Fprintf(&b, "%s is %s now. Call setup_domain_dns without dry_run to apply.\n", d.Name, d.Status)
		case d.Status == "verified":
			fmt.Fprintf(&b, "%s is verified and can send email.\n", d.Name)
		default:
			fmt.Fprintf(&b, "%s is %s: DNS changes can take a few minutes to be visible; call verify_domain later, not in a loop.\n%s",
				d.Name, d.Status, describeRecords(*d))
		}
	}
	return text(b.String()), nil, nil
}
