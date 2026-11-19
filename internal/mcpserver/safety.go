package mcpserver

// Agent safety tools (v0.6, lane B): the pre-send check, approval holds,
// suppressions and circuit-breaker pauses. Like every tool, they call the
// HTTP API with the agent's key, so the server decides what is allowed.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const safetyInstructions = `
Safety: the server may hold an email for approval (send_email then says "held for approval"; a person or a reviewing key decides with approve_email or reject_email), refuse a recipient mailed too often today (recipient_rate_limited), or pause a key, domain or project whose mail bounces or draws complaints (sending_paused). Never work around a hold, a pause, a suppression or an unsubscribe: tell the user instead. Call check_email before a send you are unsure about; it sends nothing. Newsletters and other bulk mail go with stream "broadcast", one recipient per email, and get a one-click unsubscribe link.`

// KeyAccess is what the API key may do, when the caller knows it (the
// remote /mcp endpoint does). Tools the key cannot use are then not
// offered. Nil means unknown: every tool is offered and the API decides.
type KeyAccess struct {
	FullAccess bool
	CanApprove bool
}

func addSafetyTools(s *mcp.Server, t *tools) {
	acc := t.c.Access
	mcp.AddTool(s, &mcp.Tool{
		Name:  "check_email",
		Title: "Check an email before sending",
		Description: "Check an email without sending it: whether it would be sent, held for approval or refused (and why), " +
			"its size, and deliverability warnings (no plain-text part, HTML over Gmail's 102 KB clip, links to other domains, " +
			"a broadcast without an unsubscribe link, an unverified domain). Takes the same fields as send_email.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.checkEmail)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_held",
		Title: "List emails waiting for approval",
		Description: "List emails held for approval, newest first, with why they were held and the start of their text. " +
			"A key that may approve sees the whole project's; other keys see their own. Pass before (next_before) for older ones.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.listHeld)

	if acc == nil || acc.CanApprove {
		mcp.AddTool(s, &mcp.Tool{
			Name:  "approve_email",
			Title: "Approve a held email",
			Description: "Approve an email held for approval: it is delivered to every held recipient. Only for keys allowed to approve, " +
				"and never for an email sent with the same key. Read it first (list_held) and approve only what the user wants sent.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
		}, t.approveEmail)
		mcp.AddTool(s, &mcp.Tool{
			Name:        "reject_email",
			Title:       "Reject a held email",
			Description: "Reject an email held for approval: it is never sent. Only for keys allowed to approve. This cannot be undone.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)},
		}, t.rejectEmail)
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_suppressions",
		Title: "List suppressed addresses",
		Description: "List addresses this project does not send to, newest first, with the reason: hard_bounce, complaint, " +
			"unsubscribe (broadcast mail only) or one set by hand. Filter by address, reason or stream.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.listSuppressions)

	if acc == nil || acc.FullAccess {
		mcp.AddTool(s, &mcp.Tool{
			Name:  "add_suppression",
			Title: "Stop sending to an address",
			Description: "Add an address to the suppression list, for example when someone asks not to be emailed. " +
				"With stream \"broadcast\" only bulk mail stops; the default stops all mail. Needs a full_access key.",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
		}, t.addSuppression)
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_pauses",
		Title: "List sending pauses",
		Description: "List circuit-breaker pauses: a key, sending domain or project whose bounce or complaint rate went over the limit, " +
			"and which sends nothing until a person resumes it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)},
	}, t.listPauses)
}

// sendBody is the POST /emails (and /emails/check) body for the fields an
// agent passes.
func sendBody(from string, to, cc, bcc, replyTo []string, subject, text, html, stream, expiresAt, scheduledAt string, tags map[string]string) map[string]any {
	body := map[string]any{"from": from, "to": to, "subject": subject}
	for k, v := range map[string][]string{"cc": cc, "bcc": bcc, "reply_to": replyTo} {
		if len(v) > 0 {
			body[k] = v
		}
	}
	for k, v := range map[string]string{"text": text, "html": html, "stream": stream, "expires_at": expiresAt, "scheduled_at": scheduledAt} {
		if v != "" {
			body[k] = v
		}
	}
	if len(tags) > 0 {
		var l []map[string]string
		for k, v := range tags {
			l = append(l, map[string]string{"name": k, "value": v})
		}
		body["tags"] = l
	}
	return body
}

func heldNext(reasons []string) string {
	return fmt.Sprintf("the email is held for approval (%s): nothing is sent until a person or an approver key approves it. "+
		"Do not send it again. Tell the user it waits for approval; get_email shows when it was approved or rejected",
		strings.Join(reasons, ", "))
}

type CheckInput struct {
	From        string            `json:"from" jsonschema:"the sender, like Acme Support <support@acme.com>"`
	To          []string          `json:"to" jsonschema:"recipient addresses"`
	Cc          []string          `json:"cc,omitempty" jsonschema:"carbon-copy recipients"`
	Bcc         []string          `json:"bcc,omitempty" jsonschema:"blind-copy recipients"`
	ReplyTo     []string          `json:"reply_to,omitempty" jsonschema:"where replies should go"`
	Subject     string            `json:"subject" jsonschema:"the subject line"`
	Text        string            `json:"text,omitempty" jsonschema:"plain-text body"`
	HTML        string            `json:"html,omitempty" jsonschema:"HTML body"`
	Stream      string            `json:"stream,omitempty" jsonschema:"transactional (default) or broadcast (bulk mail, one recipient, gets an unsubscribe link)"`
	Tags        map[string]string `json:"tags,omitempty" jsonschema:"labels echoed in webhooks"`
	ScheduledAt string            `json:"scheduled_at,omitempty" jsonschema:"RFC 3339 time to send at"`
}

type checkResult struct {
	Would       string   `json:"would"`
	Stream      string   `json:"stream"`
	Size        int      `json:"size"`
	HoldReasons []string `json:"hold_reasons"`
	Refusals    []struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"refusals"`
	Warnings []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
}

func (t *tools) checkEmail(ctx context.Context, _ *mcp.CallToolRequest, in CheckInput) (*mcp.CallToolResult, any, error) {
	body := sendBody(in.From, in.To, in.Cc, in.Bcc, in.ReplyTo, in.Subject, in.Text, in.HTML, in.Stream, "", in.ScheduledAt, in.Tags)
	var r checkResult
	if err := t.c.do(ctx, http.MethodPost, "/emails/check", body, nil, &r); err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	switch r.Would {
	case "refuse":
		b.WriteString("This email would be refused:\n")
		for _, x := range r.Refusals {
			fmt.Fprintf(&b, "- %s: %s\n", x.Name, x.Message)
		}
	case "hold":
		fmt.Fprintf(&b, "This email would be held for approval (%s) before anything is sent.\n", strings.Join(r.HoldReasons, ", "))
	default:
		b.WriteString("This email would be sent.\n")
	}
	fmt.Fprintf(&b, "Stream %s, about %d KB.\n", r.Stream, (r.Size+1023)/1024)
	if len(r.Warnings) == 0 {
		b.WriteString("No deliverability warnings.\n")
	} else {
		b.WriteString("Warnings:\n")
		for _, x := range r.Warnings {
			fmt.Fprintf(&b, "- %s: %s\n", x.Code, x.Message)
		}
	}
	return text(b.String()), nil, nil
}

type ListHeldInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"how many to return, 1 to 50 (default 20)"`
	Before string `json:"before,omitempty" jsonschema:"an email id: only emails held before it (the next_before of the previous call)"`
}

func (t *tools) listHeld(ctx context.Context, _ *mcp.CallToolRequest, in ListHeldInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	if in.Limit != 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	if in.Before != "" {
		if !uuidRE.MatchString(in.Before) {
			return nil, nil, errors.New("before must be an email id, as returned in next_before")
		}
		q.Set("before", in.Before)
	}
	path := "/emails/held"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Data []struct {
			ID          string   `json:"id"`
			APIKeyID    *string  `json:"api_key_id"`
			From        string   `json:"from"`
			Subject     string   `json:"subject"`
			Stream      string   `json:"stream"`
			HoldReasons []string `json:"hold_reasons"`
			Recipients  []string `json:"recipients"`
			CreatedAt   string   `json:"created_at"`
			ExpiresAt   string   `json:"expires_at"`
			Text        string   `json:"text"`
			Truncated   bool     `json:"text_truncated"`
		} `json:"data"`
		HasMore    bool    `json:"has_more"`
		NextBefore *string `json:"next_before"`
	}
	if err := t.c.do(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, nil, err
	}
	if len(resp.Data) == 0 {
		return text("No emails are waiting for approval."), nil, nil
	}
	var b strings.Builder
	for _, h := range resp.Data {
		key := "unknown key"
		if h.APIKeyID != nil {
			key = "key " + *h.APIKeyID
		}
		fmt.Fprintf(&b, "- %s: %q from %s to %s (%s, %s), held because of %s; expires %s\n",
			h.ID, h.Subject, h.From, strings.Join(h.Recipients, ", "), h.Stream, key, strings.Join(h.HoldReasons, ", "), h.ExpiresAt)
		if h.Text != "" {
			more := ""
			if h.Truncated {
				more = " […]"
			}
			fmt.Fprintf(&b, "  Text: %s%s\n", strings.ReplaceAll(strings.TrimSpace(h.Text), "\n", "\n  "), more)
		}
	}
	if resp.HasMore && resp.NextBefore != nil {
		fmt.Fprintf(&b, "More are older: call list_held with before=%s.\n", *resp.NextBefore)
	}
	return text(b.String()), nil, nil
}

type DecideInput struct {
	ID   string `json:"id" jsonschema:"the held email's id (list_held)"`
	Note string `json:"note,omitempty" jsonschema:"why, kept with the decision"`
}

func (t *tools) decide(ctx context.Context, in DecideInput, verb string) (*mcp.CallToolResult, any, error) {
	if !uuidRE.MatchString(in.ID) {
		return nil, nil, errors.New("id must be a held email's id, as list_held returns")
	}
	body := map[string]string{}
	if in.Note != "" {
		body["note"] = in.Note
	}
	var resp struct {
		Recipients []string `json:"recipients"`
		Suppressed []string `json:"suppressed"`
	}
	if err := t.c.do(ctx, http.MethodPost, "/emails/"+in.ID+"/"+verb, body, nil, &resp); err != nil {
		return nil, nil, err
	}
	var msg string
	if verb == "approve" {
		msg = fmt.Sprintf("Approved email %s: it is being delivered to %s.", in.ID, strings.Join(resp.Recipients, ", "))
		if len(resp.Suppressed) > 0 {
			msg += fmt.Sprintf(" Not sent to %s: suppressed since it was held.", strings.Join(resp.Suppressed, ", "))
		}
	} else {
		msg = fmt.Sprintf("Rejected email %s: nothing will be sent to %s.", in.ID, strings.Join(resp.Recipients, ", "))
	}
	return text(msg), nil, nil
}

func (t *tools) approveEmail(ctx context.Context, _ *mcp.CallToolRequest, in DecideInput) (*mcp.CallToolResult, any, error) {
	return t.decide(ctx, in, "approve")
}

func (t *tools) rejectEmail(ctx context.Context, _ *mcp.CallToolRequest, in DecideInput) (*mcp.CallToolResult, any, error) {
	return t.decide(ctx, in, "reject")
}

type ListSuppressionsInput struct {
	Address string `json:"address,omitempty" jsonschema:"only this address"`
	Reason  string `json:"reason,omitempty" jsonschema:"only this reason: hard_bounce, complaint, unsubscribe, manual…"`
	Stream  string `json:"stream,omitempty" jsonschema:"only all (no mail at all) or broadcast (no bulk mail)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"how many to return, 1 to 100 (default 20)"`
	Before  string `json:"before,omitempty" jsonschema:"the next_before of the previous call"`
}

type suppressionView struct {
	Address   string `json:"address"`
	Stream    string `json:"stream"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

func (t *tools) listSuppressions(ctx context.Context, _ *mcp.CallToolRequest, in ListSuppressionsInput) (*mcp.CallToolResult, any, error) {
	q := url.Values{}
	for k, v := range map[string]string{"address": in.Address, "reason": in.Reason, "stream": in.Stream, "before": in.Before} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if in.Limit != 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	path := "/suppressions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp struct {
		Data       []suppressionView `json:"data"`
		HasMore    bool              `json:"has_more"`
		NextBefore *string           `json:"next_before"`
	}
	if err := t.c.do(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, nil, err
	}
	if len(resp.Data) == 0 {
		return text("No suppressed addresses match."), nil, nil
	}
	var b strings.Builder
	for _, v := range resp.Data {
		fmt.Fprintf(&b, "- %s: %s (%s mail), since %s", v.Address, v.Reason, v.Stream, v.CreatedAt)
		if v.Detail != "" {
			fmt.Fprintf(&b, " [%s]", v.Detail)
		}
		b.WriteString("\n")
	}
	if resp.HasMore && resp.NextBefore != nil {
		fmt.Fprintf(&b, "More: call list_suppressions with before=%s.\n", *resp.NextBefore)
	}
	return text(b.String()), nil, nil
}

type AddSuppressionInput struct {
	Address string `json:"address" jsonschema:"the address to stop sending to"`
	Reason  string `json:"reason,omitempty" jsonschema:"a short reason name, like customer_request (default manual)"`
	Detail  string `json:"detail,omitempty" jsonschema:"free text, kept with the suppression"`
	Stream  string `json:"stream,omitempty" jsonschema:"all (default: no mail at all) or broadcast (no bulk mail)"`
}

func (t *tools) addSuppression(ctx context.Context, _ *mcp.CallToolRequest, in AddSuppressionInput) (*mcp.CallToolResult, any, error) {
	body := map[string]string{"address": in.Address}
	for k, v := range map[string]string{"reason": in.Reason, "detail": in.Detail, "stream": in.Stream} {
		if v != "" {
			body[k] = v
		}
	}
	var v suppressionView
	if err := t.c.do(ctx, http.MethodPost, "/suppressions", body, nil, &v); err != nil {
		return nil, nil, err
	}
	return text(fmt.Sprintf("%s is suppressed (%s, %s mail): nothing of that kind will be sent to it.", v.Address, v.Reason, v.Stream)), nil, nil
}

type ListPausesInput struct {
	ActiveOnly bool `json:"active_only,omitempty" jsonschema:"only pauses still in force"`
}

func (t *tools) listPauses(ctx context.Context, _ *mcp.CallToolRequest, in ListPausesInput) (*mcp.CallToolResult, any, error) {
	path := "/pauses"
	if in.ActiveOnly {
		path += "?active=true"
	}
	var resp struct {
		Data []struct {
			ID         string  `json:"id"`
			Scope      string  `json:"scope"`
			ScopeName  string  `json:"scope_name"`
			Reason     string  `json:"reason"`
			Rate       float64 `json:"rate"`
			Threshold  float64 `json:"threshold"`
			Recipients int     `json:"recipients"`
			CreatedAt  string  `json:"created_at"`
			Active     bool    `json:"active"`
		} `json:"data"`
	}
	if err := t.c.do(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, nil, err
	}
	if len(resp.Data) == 0 {
		return text("Nothing is paused."), nil, nil
	}
	var b strings.Builder
	for _, p := range resp.Data {
		state := "resumed"
		if p.Active {
			state = "ACTIVE: sends nothing until a person resumes it"
		}
		fmt.Fprintf(&b, "- %s %q: %s %.2f%% over %d recipients (limit %.2f%%), since %s; %s (pause %s)\n",
			p.Scope, p.ScopeName, p.Reason, p.Rate*100, p.Recipients, p.Threshold*100, p.CreatedAt, state, p.ID)
	}
	return text(b.String()), nil, nil
}
