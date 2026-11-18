package openmailwire

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testID  = "ed01a077-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	testKey = "omw_test_key"
)

type recorded struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

func (r recorded) json(t *testing.T) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("request body is not JSON: %v (%q)", err, r.Body)
	}
	return v
}

type reply struct {
	status  int
	body    any // JSON-encoded unless []byte or string
	headers map[string]string
}

type mock struct {
	mu       sync.Mutex
	requests []recorded
}

func (m *mock) all() []recorded {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recorded(nil), m.requests...)
}

func (m *mock) last() recorded {
	all := m.all()
	return all[len(all)-1]
}

// serve starts a local server answering from handler (nil means 404) and
// returns a client pointed at it.
func serve(t *testing.T, handler func(r recorded) *reply) (*Client, *mock) {
	t.Helper()
	m := &mock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec := recorded{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Header: req.Header.Clone(), Body: body}
		m.mu.Lock()
		m.requests = append(m.requests, rec)
		m.mu.Unlock()
		out := handler(rec)
		if out == nil {
			out = &reply{status: 404, body: map[string]any{"statusCode": 404, "name": "not_found", "message": "no such endpoint"}}
		}
		var data []byte
		switch b := out.body.(type) {
		case []byte:
			data = b
			w.Header().Set("Content-Type", "application/octet-stream")
		case string:
			data = []byte(b)
			w.Header().Set("Content-Type", "text/plain")
		default:
			data, _ = json.Marshal(b)
			w.Header().Set("Content-Type", "application/json")
		}
		for k, v := range out.headers {
			w.Header().Set(k, v)
		}
		if out.status == 0 {
			out.status = 200
		}
		w.WriteHeader(out.status)
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{APIKey: testKey, BaseURL: srv.URL + "/", PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c, m
}

func ok(body any) *reply { return &reply{status: 200, body: body} }

func emailWith(statuses ...string) map[string]any {
	rcpts := []map[string]any{}
	for i, s := range statuses {
		rcpts = append(rcpts, map[string]any{"email": "r" + string(rune('0'+i)) + "@example.test", "status": s, "attempts": 0})
	}
	return map[string]any{
		"object": "email", "id": testID, "from": "a@acme.test", "to": []string{"x@example.test"}, "subject": "hi",
		"created_at": "2026-11-18T10:00:00Z", "expires_at": "2026-11-20T10:00:00Z",
		"recipients": rcpts, "attempts": []any{}, "events": []any{},
	}
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

var ctx = context.Background()

// ------------------------------------------------------------------ configuration

func TestNewReadsEnvironment(t *testing.T) {
	t.Setenv("OMW_API_URL", "https://mail.example.test/")
	t.Setenv("OMW_API_KEY", "omw_from_env")
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, c.BaseURL, "https://mail.example.test")
	eq(t, c.apiKey, "omw_from_env")
}

func TestNewDefaultsAndNeedsKey(t *testing.T) {
	t.Setenv("OMW_API_URL", "")
	t.Setenv("OMW_API_KEY", "")
	c, err := New(Config{APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, c.BaseURL, "http://localhost:8080")
	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "OMW_API_KEY") {
		t.Fatalf("want a missing-key error, got %v", err)
	}
}

// ------------------------------------------------------------------ emails

func TestSend(t *testing.T) {
	c, m := serve(t, func(recorded) *reply { return ok(map[string]string{"id": testID}) })
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	res, err := c.Emails.Send(ctx, &SendEmailRequest{
		From:    "Acme <hello@acme.test>",
		To:      []string{"you@example.test"},
		ReplyTo: []string{"support@acme.test"},
		Subject: "Your code",
		Text:    "482913",
		Tags:    []Tag{{Name: "kind", Value: "otp"}},
		Attachments: []Attachment{
			{Filename: "a.txt", Content: []byte("hello"), ContentType: "text/plain"},
			{Filename: "logo.png", Content: []byte{0x89, 'P', 'N', 'G'}, ContentID: "logo"},
		},
		ScheduledAt:    &at,
		RetryUncertain: Ptr(false),
	}, IdempotencyKey("signup-42"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.ID, testID)
	r := m.last()
	eq(t, r.Method+" "+r.Path, "POST /emails")
	eq(t, r.Header.Get("Authorization"), "Bearer "+testKey)
	eq(t, r.Header.Get("Content-Type"), "application/json")
	eq(t, r.Header.Get("Idempotency-Key"), "signup-42")
	if !strings.HasPrefix(r.Header.Get("User-Agent"), "openmailwire-go/") {
		t.Fatalf("User-Agent %q", r.Header.Get("User-Agent"))
	}
	eq(t, r.json(t), map[string]any{
		"from":     "Acme <hello@acme.test>",
		"to":       []any{"you@example.test"},
		"reply_to": []any{"support@acme.test"},
		"subject":  "Your code",
		"text":     "482913",
		"tags":     []any{map[string]any{"name": "kind", "value": "otp"}},
		"attachments": []any{
			map[string]any{"filename": "a.txt", "content": "aGVsbG8=", "content_type": "text/plain"},
			map[string]any{"filename": "logo.png", "content": base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'}), "content_id": "logo"},
		},
		"scheduled_at":    "2030-01-02T03:04:05Z",
		"retry_uncertain": false,
	})
}

func TestSendWithoutIdempotencyKey(t *testing.T) {
	c, m := serve(t, func(recorded) *reply { return ok(map[string]string{"id": testID}) })
	if _, err := c.Emails.Send(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, set := m.last().Header["Idempotency-Key"]; set {
		t.Fatal("Idempotency-Key sent without being asked for")
	}
}

func TestGetListCancel(t *testing.T) {
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "GET" && r.Path == "/emails/"+testID:
			return ok(emailWith("delivered"))
		case r.Method == "GET" && r.Path == "/emails":
			return ok(map[string]any{"object": "list", "data": []any{map[string]any{"id": testID, "created_at": "2026-11-18T10:00:00Z", "api_key_id": nil}}, "has_more": true, "next_before": testID})
		case r.Method == "POST" && r.Path == "/emails/"+testID+"/cancel":
			return ok(map[string]any{"object": "email", "id": testID, "canceled": 2})
		}
		return nil
	})
	e, err := c.Emails.Get(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, e.Recipients[0].Status, StatusDelivered)
	page, err := c.Emails.List(ctx, &ListEmailsParams{Limit: 10, Before: testID, APIKeyID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, page.HasMore, true)
	eq(t, page.NextBefore, testID)
	eq(t, m.last().Query, "api_key_id=k1&before="+testID+"&limit=10")
	if _, err := c.Emails.List(ctx, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().Query, "")
	res, err := c.Emails.Cancel(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.ID, testID)
}

func TestBatch(t *testing.T) {
	c, m := serve(t, func(recorded) *reply {
		return ok(map[string]any{"data": []any{map[string]string{"id": "1"}, map[string]string{"id": "2"}}})
	})
	res, err := c.Emails.Batch(ctx, []SendEmailRequest{
		{From: "a@acme.test", To: []string{"x@example.test"}, Subject: "1", Text: "one"},
		{From: "a@acme.test", To: []string{"y@example.test"}, Subject: "2", Text: "two"},
	}, IdempotencyKey("nightly"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, []string{res.Data[0].ID, res.Data[1].ID}, []string{"1", "2"})
	r := m.last()
	eq(t, r.Path, "/emails/batch")
	eq(t, r.Header.Get("Idempotency-Key"), "nightly")
	items := r.json(t).([]any)
	eq(t, len(items), 2)
	eq(t, items[1].(map[string]any)["subject"], "2")
	if _, has := items[0].(map[string]any)["attachments"]; has {
		t.Fatal("empty attachments must be left out of a batch item")
	}
}

func TestSendRaw(t *testing.T) {
	c, m := serve(t, func(recorded) *reply { return ok(map[string]string{"id": testID}) })
	raw := []byte("From: a@acme.test\r\nTo: x@example.test\r\nSubject: hi\r\n\r\nbody\r\n")
	if _, err := c.Emails.SendRaw(ctx, &SendRawRequest{Raw: raw, MailFrom: "a@acme.test", Rcpts: []string{"x@example.test"}}, IdempotencyKey("r1")); err != nil {
		t.Fatal(err)
	}
	r := m.last()
	eq(t, r.Path, "/emails/raw")
	eq(t, r.Header.Get("Idempotency-Key"), "r1")
	eq(t, r.json(t), map[string]any{
		"raw":       base64.StdEncoding.EncodeToString(raw),
		"mail_from": "a@acme.test",
		"rcpts":     []any{"x@example.test"},
	})
}

// ------------------------------------------------------------------ domains, webhooks, keys

func TestDomains(t *testing.T) {
	dom := map[string]any{"object": "domain", "id": testID, "name": "acme.test", "status": "pending", "created_at": "2026-11-18T10:00:00Z", "verified_at": nil, "records": []any{map[string]any{"record": "DKIM", "type": "TXT", "required": true}}}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "POST" && r.Path == "/domains":
			return &reply{status: 201, body: dom}
		case r.Path == "/domains":
			return ok(map[string]any{"object": "list", "data": []any{dom}})
		case r.Method == "POST" && r.Path == "/domains/"+testID+"/verify":
			v := map[string]any{}
			for k, x := range dom {
				v[k] = x
			}
			v["status"] = "verified"
			return ok(v)
		case r.Method == "DELETE" && r.Path == "/domains/"+testID:
			return ok(map[string]any{"object": "domain", "id": testID, "deleted": true})
		case r.Path == "/domains/"+testID:
			return ok(dom)
		}
		return nil
	})
	d, err := c.Domains.Create(ctx, "acme.test")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, d.Records[0].Record, "DKIM")
	eq(t, m.last().json(t), map[string]any{"name": "acme.test"})
	list, err := c.Domains.List(ctx)
	if err != nil || len(list.Data) != 1 {
		t.Fatal(list, err)
	}
	if d, err = c.Domains.Get(ctx, testID); err != nil || d.ID != testID {
		t.Fatal(d, err)
	}
	if d, err = c.Domains.Verify(ctx, testID); err != nil || d.Status != "verified" {
		t.Fatal(d, err)
	}
	del, err := c.Domains.Delete(ctx, testID)
	if err != nil || !del.Deleted {
		t.Fatal(del, err)
	}
}

func TestWebhooks(t *testing.T) {
	hook := map[string]any{"object": "webhook", "id": testID, "endpoint": "https://h.test/x", "events": []string{"email.bounced"}, "created_at": "2026-11-18T10:00:00Z"}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "POST" && r.Path == "/webhooks":
			return &reply{status: 201, body: map[string]any{"id": testID, "signing_secret": "whsec_abc", "created_at": "2026-11-18T10:00:00Z"}}
		case r.Path == "/webhooks":
			return ok(map[string]any{"object": "list", "data": []any{hook}})
		case r.Method == "DELETE" && r.Path == "/webhooks/"+testID:
			return ok(map[string]any{"object": "webhook", "id": testID, "deleted": true})
		case r.Path == "/webhooks/"+testID:
			return ok(map[string]any{"id": testID, "created_at": "2026-11-18T10:00:00Z", "recent_deliveries": []any{map[string]any{"id": "d1", "status": "delivered", "created_at": "2026-11-18T10:00:00Z", "last_status": 200}}})
		}
		return nil
	})
	w, err := c.Webhooks.Create(ctx, &CreateWebhookRequest{Endpoint: "https://h.test/x", Events: []string{"email.bounced"}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, w.SigningSecret, "whsec_abc")
	eq(t, m.last().json(t), map[string]any{"endpoint": "https://h.test/x", "events": []any{"email.bounced"}})
	if _, err := c.Webhooks.Create(ctx, &CreateWebhookRequest{Endpoint: "https://h.test/all"}); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"endpoint": "https://h.test/all"})
	list, err := c.Webhooks.List(ctx)
	if err != nil || list.Data[0].Events[0] != "email.bounced" {
		t.Fatal(list, err)
	}
	got, err := c.Webhooks.Get(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, *got.RecentDeliveries[0].LastStatus, 200)
	del, err := c.Webhooks.Delete(ctx, testID)
	if err != nil || !del.Deleted {
		t.Fatal(del, err)
	}
}

func TestAPIKeys(t *testing.T) {
	key := map[string]any{"id": testID, "name": "agent", "prefix": "omw_ab", "permission": "sending_access", "policy": map[string]any{"max_per_hour": 10}, "created_at": "2026-11-18T10:00:00Z", "last_used_at": nil}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "POST" && r.Path == "/api-keys":
			k := map[string]any{"token": "omw_secret"}
			for x, v := range key {
				k[x] = v
			}
			return &reply{status: 201, body: k}
		case r.Path == "/api-keys":
			return ok(map[string]any{"object": "list", "data": []any{key}})
		case r.Method == "PATCH" && r.Path == "/api-keys/"+testID:
			return ok(map[string]any{"id": testID, "name": "renamed", "created_at": "2026-11-18T10:00:00Z"})
		case r.Method == "DELETE" && r.Path == "/api-keys/"+testID:
			return ok(map[string]any{"object": "api_key", "id": testID, "deleted": true})
		case r.Path == "/api-keys/"+testID:
			return ok(key)
		}
		return nil
	})
	k, err := c.APIKeys.Create(ctx, &CreateAPIKeyRequest{
		Name: "agent", Permission: SendingAccess,
		Policy: &APIKeyPolicy{MaxPerHour: Ptr(10), AllowedRecipients: []string{"*@acme.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, k.Token, "omw_secret")
	eq(t, *k.Policy.MaxPerHour, 10)
	eq(t, m.last().json(t), map[string]any{
		"name": "agent", "permission": "sending_access",
		"policy": map[string]any{"max_per_hour": float64(10), "allowed_recipients": []any{"*@acme.test"}},
	})
	list, err := c.APIKeys.List(ctx)
	if err != nil || list.Data[0].Prefix != "omw_ab" {
		t.Fatal(list, err)
	}
	if k, err = c.APIKeys.Get(ctx, testID); err != nil || k.Name != "agent" || k.LastUsedAt != nil {
		t.Fatal(k, err)
	}
	if k, err = c.APIKeys.Update(ctx, testID, &UpdateAPIKeyRequest{Name: "renamed"}); err != nil || k.Name != "renamed" {
		t.Fatal(k, err)
	}
	eq(t, m.last().Method, "PATCH")
	eq(t, m.last().json(t), map[string]any{"name": "renamed"})
	if _, err := c.APIKeys.Update(ctx, testID, &UpdateAPIKeyRequest{Policy: &APIKeyPolicy{}}); err != nil {
		t.Fatal(err)
	}
	eq(t, m.last().json(t), map[string]any{"policy": map[string]any{}})
	del, err := c.APIKeys.Delete(ctx, testID)
	if err != nil || !del.Deleted {
		t.Fatal(del, err)
	}
}

// ------------------------------------------------------------------ inboxes and messages

func TestInboxes(t *testing.T) {
	inbox := map[string]any{"id": "i1", "address": "agent@acme.test", "created_at": "2026-11-18T10:00:00Z"}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Method == "POST" && r.Path == "/inboxes":
			return &reply{status: 201, body: inbox}
		case r.Path == "/inboxes":
			return ok(map[string]any{"object": "list", "data": []any{inbox}})
		case r.Method == "DELETE" && r.Path == "/inboxes/i1":
			return ok(map[string]any{"object": "inbox", "id": "i1", "deleted": true})
		case r.Path == "/inboxes/i1/messages":
			return ok(map[string]any{"data": []any{map[string]any{"id": "m1", "to": "agent@acme.test", "received_at": "2026-11-18T10:00:00Z", "thread_id": nil}}, "has_more": false, "next_before": nil})
		}
		return nil
	})
	in, err := c.Inboxes.Create(ctx, "agent@acme.test")
	if err != nil || in.Address != "agent@acme.test" {
		t.Fatal(in, err)
	}
	eq(t, m.last().json(t), map[string]any{"address": "agent@acme.test"})
	list, err := c.Inboxes.List(ctx)
	if err != nil || len(list.Data) != 1 {
		t.Fatal(list, err)
	}
	page, err := c.Inboxes.ListMessages(ctx, "i1", &ListMessagesParams{Limit: 5, ThreadID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, page.Data[0].ID, "m1")
	eq(t, page.Data[0].To, AddressList{"agent@acme.test"})
	eq(t, m.last().Query, "limit=5&thread_id=t1")
	del, err := c.Inboxes.Delete(ctx, "i1")
	if err != nil || !del.Deleted {
		t.Fatal(del, err)
	}
}

func TestMessages(t *testing.T) {
	blob := []byte{0, 1, 2, 250, 255}
	c, m := serve(t, func(r recorded) *reply {
		switch {
		case r.Path == "/messages/m1":
			return ok(map[string]any{"id": "m1", "subject": "Re: hi", "to": []string{"a@acme.test"}, "cc": nil, "headers": map[string]string{"message-id": "<x@y>"}, "attachments": []any{map[string]any{"index": 0, "filename": "f.bin", "size": 5}}, "received_at": "2026-11-18T10:00:00Z"})
		case r.Path == "/messages/m1/attachments/0":
			return ok(blob)
		case r.Method == "POST" && r.Path == "/messages/m1/reply":
			return ok(map[string]string{"id": testID})
		}
		return nil
	})
	msg, err := c.Messages.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, msg.Subject, "Re: hi")
	eq(t, msg.Headers["message-id"], "<x@y>")
	eq(t, msg.Attachments[0].Size, int64(5))
	data, err := c.Messages.GetAttachment(ctx, "m1", 0)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, data, blob)
	res, err := c.Messages.Reply(ctx, "m1", &ReplyRequest{Text: "thanks"})
	if err != nil || res.ID != testID {
		t.Fatal(res, err)
	}
	eq(t, m.last().json(t), map[string]any{"text": "thanks"})
}

// ------------------------------------------------------------------ errors

func TestAPIError(t *testing.T) {
	c, _ := serve(t, func(recorded) *reply {
		return &reply{status: 422, body: map[string]any{"statusCode": 422, "name": "validation_error", "message": "subject is required"}}
	})
	_, err := c.Emails.Send(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Text: "x"})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	eq(t, *apiErr, Error{StatusCode: 422, Name: "validation_error", Message: "subject is required"})
	eq(t, err.Error(), "openmailwire: 422 validation_error: subject is required")
}

func TestRateLimitRetryAfter(t *testing.T) {
	c, _ := serve(t, func(recorded) *reply {
		return &reply{status: 429, headers: map[string]string{"Retry-After": "120"}, body: map[string]any{"statusCode": 429, "name": "rate_limit_exceeded", "message": "hourly cap reached"}}
	})
	_, err := c.Emails.Send(ctx, &SendEmailRequest{From: "a@acme.test", To: []string{"b@example.test"}, Subject: "s", Text: "x"})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatal(err)
	}
	eq(t, apiErr.StatusCode, 429)
	eq(t, apiErr.Name, "rate_limit_exceeded")
	eq(t, apiErr.RetryAfter, 120*time.Second)
}

func TestNonJSONError(t *testing.T) {
	c, _ := serve(t, func(recorded) *reply { return &reply{status: 502, body: "Bad Gateway"} })
	_, err := c.Domains.List(ctx)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatal(err)
	}
	eq(t, *apiErr, Error{StatusCode: 502, Name: "http_error", Message: "Bad Gateway"})
}

func TestConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := New(Config{APIKey: testKey, BaseURL: url})
	_, err := c.Domains.List(ctx)
	var apiErr *Error
	if err == nil || errors.As(err, &apiErr) {
		t.Fatalf("want a network error, got %v", err)
	}
}

// ------------------------------------------------------------------ waiting

func TestWaitForDelivery(t *testing.T) {
	states := [][]string{{"queued", "queued"}, {"sending", "deferred"}, {"delivered", "bounced"}}
	n := 0
	var mu sync.Mutex
	c, m := serve(t, func(recorded) *reply {
		mu.Lock()
		defer mu.Unlock()
		s := states[min(n, len(states)-1)]
		n++
		return ok(emailWith(s...))
	})
	e, err := c.WaitForDelivery(ctx, testID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, []string{e.Recipients[0].Status, e.Recipients[1].Status}, []string{"delivered", "bounced"})
	eq(t, len(m.all()), 3)
}

func TestWaitForDeliveryTimeout(t *testing.T) {
	c, _ := serve(t, func(recorded) *reply { return ok(emailWith("deferred")) })
	e, err := c.WaitForDelivery(ctx, testID, 50*time.Millisecond)
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("want ErrWaitTimeout, got %v", err)
	}
	eq(t, e.Recipients[0].Status, "deferred")
}

func TestWaitForDeliveryContext(t *testing.T) {
	c, _ := serve(t, func(recorded) *reply { return ok(emailWith("queued")) })
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := c.WaitForDelivery(cctx, testID, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

func TestWaitForReply(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	c, _ := serve(t, func(r recorded) *reply {
		switch r.Path {
		case "/inboxes":
			return ok(map[string]any{"object": "list", "data": []any{map[string]any{"id": "i1"}, map[string]any{"id": "i2"}}})
		case "/inboxes/i1/messages":
			return ok(map[string]any{"data": []any{map[string]any{"id": "m0", "in_reply_to_email_id": "other", "received_at": "2026-11-18T10:00:00Z"}}})
		case "/inboxes/i2/messages":
			if r.Query != "limit=50" {
				t.Errorf("query %q", r.Query)
			}
			mu.Lock()
			polls++
			p := polls
			mu.Unlock()
			data := []any{}
			if p >= 2 {
				data = append(data,
					map[string]any{"id": "m1", "in_reply_to_email_id": testID, "received_at": "2026-11-18T10:00:00Z"},
					map[string]any{"id": "m2", "in_reply_to_email_id": testID, "received_at": "2026-11-18T11:00:00Z"})
			}
			return ok(map[string]any{"data": data})
		}
		return nil
	})
	msgs, err := c.WaitForReply(ctx, testID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, []string{msgs[0].ID, msgs[1].ID}, []string{"m2", "m1"})
}

func TestWaitForReplyTimeoutOneInbox(t *testing.T) {
	c, m := serve(t, func(r recorded) *reply {
		if r.Path == "/inboxes/i9/messages" {
			return ok(map[string]any{"data": []any{}})
		}
		return nil
	})
	msgs, err := c.WaitForReply(ctx, testID, 50*time.Millisecond, "i9")
	if err != nil || len(msgs) != 0 {
		t.Fatal(msgs, err)
	}
	for _, r := range m.all() {
		if r.Path != "/inboxes/i9/messages" {
			t.Fatalf("unexpected request %s", r.Path)
		}
	}
}
