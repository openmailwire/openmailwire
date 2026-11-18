// Package mcpserver exposes OpenMailWire to AI agents as Model Context
// Protocol tools. Every tool calls the regular HTTP API with the agent's API
// key, so an agent gets exactly the permissions, limits and validation a
// developer gets, and nothing more.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `OpenMailWire sends email from domains that have been verified. Typical flow:
1. Call list_domains to see which sender domains are ready (status "verified").
2. Call send_email from an address on a verified domain. Always pass an idempotency_key and reuse it if you retry, so the email is sent at most once.
3. Call get_email with wait_seconds to learn what happened to each recipient: delivered, deferred (will be retried), bounced (the address is bad), suppressed (it bounced before), expired, or unknown (the receiving server may or may not have it).
To send from a new domain: add_domain, have the DNS records it returns published, then verify_domain.
Only send email the user has asked for. Do not retry a bounced or suppressed recipient.`

// APIClient calls an OpenMailWire control plane.
type APIClient struct {
	Base string
	Key  string
	HTTP *http.Client
}

// APIError is an error answer from the API, passed to the agent verbatim:
// its message names the cause and usually the next step.
type APIError struct {
	Status  int
	Name    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (HTTP %d): %s", e.Name, e.Status, e.Message)
}

func (c *APIClient) do(ctx context.Context, method, path string, body any, header map[string]string, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Name, Message string }
		json.Unmarshal(data, &e)
		if e.Name == "" {
			e.Name, e.Message = "http_error", strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Name: e.Name, Message: e.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// New returns an MCP server whose tools act through c.
func New(c *APIClient, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "openmailwire", Title: "OpenMailWire", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	t := &tools{c: c}

	mcp.AddTool(s, &mcp.Tool{
		Name:  "send_email",
		Title: "Send an email",
		Description: "Queue an email for delivery and return its id. The sender's domain must be verified (see list_domains). " +
			"Pass idempotency_key and reuse it on retries so the same email is never sent twice. " +
			"Delivery happens in the background: call get_email with the returned id to see the result.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, t.sendEmail)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_email",
		Title: "Check an email's delivery",
		Description: "Return the delivery status of every recipient of an email, each delivery attempt with the receiving server's exact reply, and the event timeline. " +
			"Set wait_seconds (up to 60) to wait until every recipient has a final status.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.getEmail)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_domains",
		Title:       "List sender domains",
		Description: "List the domains this API key can send from, with their verification status. Only verified domains can send.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.listDomains)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "add_domain",
		Title: "Add a sender domain",
		Description: "Add a domain to send from and return the DNS records that must be published for it (DKIM, SPF, bounce MX, and DMARC as a recommendation). " +
			"After the records are published, call verify_domain.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
	}, t.addDomain)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "verify_domain",
		Title: "Verify a sender domain's DNS",
		Description: "Check a domain's DNS records now and report which are in place. " +
			"The domain can send once every required record is verified. DNS changes can take minutes to propagate.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
	}, t.verifyDomain)

	return s
}

type tools struct{ c *APIClient }

type SendInput struct {
	From           string            `json:"from" jsonschema:"the sender, like Acme Support <support@acme.com>. Its domain must be verified."`
	To             []string          `json:"to" jsonschema:"recipient addresses. To, cc and bcc together hold at most 50."`
	Cc             []string          `json:"cc,omitempty" jsonschema:"carbon-copy recipients"`
	Bcc            []string          `json:"bcc,omitempty" jsonschema:"blind-copy recipients"`
	ReplyTo        []string          `json:"reply_to,omitempty" jsonschema:"where replies should go"`
	Subject        string            `json:"subject" jsonschema:"the subject line"`
	Text           string            `json:"text,omitempty" jsonschema:"plain-text body. Give text, html or both."`
	HTML           string            `json:"html,omitempty" jsonschema:"HTML body"`
	Tags           map[string]string `json:"tags,omitempty" jsonschema:"labels echoed in webhooks; letters, digits, underscore and hyphen only"`
	IdempotencyKey string            `json:"idempotency_key,omitempty" jsonschema:"a unique key for this email; reuse it when retrying so the email is sent at most once"`
	ExpiresAt      string            `json:"expires_at,omitempty" jsonschema:"RFC 3339 time after which no new delivery attempts are made, useful for one-time codes"`
}

type SendOutput struct {
	ID       string `json:"id"`
	Replayed bool   `json:"replayed"`
	Next     string `json:"next"`
}

func (t *tools) sendEmail(ctx context.Context, _ *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, SendOutput, error) {
	body := map[string]any{"from": in.From, "to": in.To, "subject": in.Subject}
	for k, v := range map[string][]string{"cc": in.Cc, "bcc": in.Bcc, "reply_to": in.ReplyTo} {
		if len(v) > 0 {
			body[k] = v
		}
	}
	if in.Text != "" {
		body["text"] = in.Text
	}
	if in.HTML != "" {
		body["html"] = in.HTML
	}
	if in.ExpiresAt != "" {
		body["expires_at"] = in.ExpiresAt
	}
	if len(in.Tags) > 0 {
		var tags []map[string]string
		for k, v := range in.Tags {
			tags = append(tags, map[string]string{"name": k, "value": v})
		}
		body["tags"] = tags
	}
	header := map[string]string{}
	if in.IdempotencyKey != "" {
		header["Idempotency-Key"] = in.IdempotencyKey
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := t.c.do(ctx, http.MethodPost, "/emails", body, header, &resp); err != nil {
		return nil, SendOutput{}, err
	}
	return nil, SendOutput{ID: resp.ID, Next: "call get_email with this id and wait_seconds to see the delivery result"}, nil
}

type GetEmailInput struct {
	ID          string `json:"id" jsonschema:"the email id returned by send_email"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (max 60) for every recipient to reach a final status"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type emailView struct {
	ID         string `json:"id"`
	Subject    string `json:"subject"`
	Recipients []struct {
		Email         string  `json:"email"`
		Status        string  `json:"status"`
		Attempts      int     `json:"attempts"`
		NextAttemptAt *string `json:"next_attempt_at"`
		LastResponse  string  `json:"last_response"`
	} `json:"recipients"`
}

func pending(status string) bool {
	return status == "queued" || status == "sending" || status == "deferred"
}

func (t *tools) getEmail(ctx context.Context, _ *mcp.CallToolRequest, in GetEmailInput) (*mcp.CallToolResult, any, error) {
	if !uuidRE.MatchString(in.ID) {
		return nil, nil, errors.New("id must be the email id returned by send_email")
	}
	wait := time.Duration(min(max(in.WaitSeconds, 0), 60)) * time.Second
	deadline := time.Now().Add(wait)
	var raw json.RawMessage
	var v emailView
	for {
		if err := t.c.do(ctx, http.MethodGet, "/emails/"+in.ID, nil, nil, &raw); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, nil, err
		}
		done := true
		for _, r := range v.Recipients {
			done = done && !pending(r.Status)
		}
		if done || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Email %s (%q):\n", v.ID, v.Subject)
	for _, r := range v.Recipients {
		fmt.Fprintf(&b, "- %s: %s", r.Email, r.Status)
		if r.Status == "deferred" && r.NextAttemptAt != nil {
			fmt.Fprintf(&b, ", next attempt at %s", *r.NextAttemptAt)
		}
		if r.LastResponse != "" {
			fmt.Fprintf(&b, " (server said: %s)", r.LastResponse)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nFull record:\n")
	var full any
	json.Unmarshal(raw, &full)
	b.WriteString(pretty(full))
	return text(b.String()), nil, nil
}

type NoInput struct{}

type domainView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Records []struct {
		Record   string `json:"record"`
		Type     string `json:"type"`
		Name     string `json:"name"`
		Value    string `json:"value"`
		Priority int    `json:"priority"`
		Required bool   `json:"required"`
		Status   string `json:"status"`
		Detail   string `json:"detail"`
	} `json:"records"`
}

func (t *tools) listDomains(ctx context.Context, _ *mcp.CallToolRequest, _ NoInput) (*mcp.CallToolResult, any, error) {
	var resp struct {
		Data []domainView `json:"data"`
	}
	if err := t.c.do(ctx, http.MethodGet, "/domains", nil, nil, &resp); err != nil {
		return nil, nil, err
	}
	if len(resp.Data) == 0 {
		return text("No domains yet. Use add_domain to add one."), nil, nil
	}
	var b strings.Builder
	for _, d := range resp.Data {
		fmt.Fprintf(&b, "- %s: %s (id %s)\n", d.Name, d.Status, d.ID)
	}
	return text(b.String()), nil, nil
}

type AddDomainInput struct {
	Name string `json:"name" jsonschema:"the domain to send from, like acme.com or mail.acme.com"`
}

func describeRecords(d domainView) string {
	var b strings.Builder
	for _, r := range d.Records {
		need := "required"
		if !r.Required {
			need = "recommended"
		}
		prio := ""
		if r.Priority > 0 {
			prio = fmt.Sprintf(" priority %d", r.Priority)
		}
		fmt.Fprintf(&b, "- %s (%s, %s): %s record at %s%s, value: %s", r.Record, need, r.Status, r.Type, r.Name, prio, r.Value)
		if r.Detail != "" {
			fmt.Fprintf(&b, " [%s]", r.Detail)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (t *tools) addDomain(ctx context.Context, _ *mcp.CallToolRequest, in AddDomainInput) (*mcp.CallToolResult, any, error) {
	var d domainView
	if err := t.c.do(ctx, http.MethodPost, "/domains", map[string]string{"name": in.Name}, nil, &d); err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("Added %s (id %s, status %s). Publish these DNS records at the domain's DNS provider, then call verify_domain:\n%s",
		d.Name, d.ID, d.Status, describeRecords(d))), nil, nil
}

type VerifyDomainInput struct {
	Domain string `json:"domain" jsonschema:"the domain name or its id"`
}

func (t *tools) verifyDomain(ctx context.Context, _ *mcp.CallToolRequest, in VerifyDomainInput) (*mcp.CallToolResult, any, error) {
	id := in.Domain
	if !uuidRE.MatchString(id) {
		var list struct {
			Data []domainView `json:"data"`
		}
		if err := t.c.do(ctx, http.MethodGet, "/domains", nil, nil, &list); err != nil {
			return nil, nil, err
		}
		id = ""
		for _, d := range list.Data {
			if strings.EqualFold(d.Name, strings.TrimSuffix(in.Domain, ".")) {
				id = d.ID
			}
		}
		if id == "" {
			return nil, nil, fmt.Errorf("domain %s is not added; call add_domain first", in.Domain)
		}
	}
	var d domainView
	if err := t.c.do(ctx, http.MethodPost, "/domains/"+id+"/verify", nil, nil, &d); err != nil {
		return nil, nil, err
	}
	head := fmt.Sprintf("%s is %s.", d.Name, d.Status)
	if d.Status == "verified" {
		head += " It can send email now."
	} else {
		head += " Fix the records below that are not verified and try again; DNS changes can take a few minutes to appear."
	}
	return text(head + "\n" + describeRecords(d)), nil, nil
}
