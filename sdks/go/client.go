// Package openmailwire is the Go client for OpenMailWire, open-source email
// for AI agents. It uses only the standard library.
//
// Import it with a name, because the import path ends in "go":
//
//	import openmailwire "github.com/openmailwire/openmailwire/sdks/go"
//
//	client, err := openmailwire.New(openmailwire.Config{}) // reads OMW_API_URL and OMW_API_KEY
//	sent, err := client.Emails.Send(ctx, &openmailwire.SendEmailRequest{
//		From:    "Acme <hello@acme.com>",
//		To:      []string{"you@example.org"},
//		Subject: "Hi",
//		Text:    "Hello",
//	}, openmailwire.IdempotencyKey("welcome-42"))
//	email, err := client.WaitForDelivery(ctx, sent.ID, time.Minute)
package openmailwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the SDK version, sent in the User-Agent header.
const Version = "0.1.0"

// DefaultBaseURL is used when neither Config.BaseURL nor OMW_API_URL is set.
const DefaultBaseURL = "http://localhost:8080"

// Config configures a Client. Every field is optional.
type Config struct {
	// APIKey is an "omw_…" key. Default: the OMW_API_KEY environment variable.
	APIKey string
	// BaseURL defaults to the OMW_API_URL environment variable, then
	// DefaultBaseURL.
	BaseURL string
	// HTTPClient defaults to a client with a 30 s timeout. Whichever client
	// is used, redirects are not followed, so the API key never goes to
	// another address: a redirect answers an *Error named "redirect_refused".
	HTTPClient *http.Client
	// PollInterval is how often WaitForDelivery and WaitForReply ask the
	// server. Default 1 s.
	PollInterval time.Duration
}

// Client talks to one OpenMailWire control plane. It is safe for concurrent
// use.
type Client struct {
	// BaseURL is the control plane's URL, without a trailing slash.
	BaseURL string

	Emails   *EmailsService
	Domains  *DomainsService
	Webhooks *WebhooksService
	APIKeys  *APIKeysService
	Inboxes  *InboxesService
	Messages *MessagesService

	apiKey       string
	http         *http.Client
	pollInterval time.Duration
}

// New returns a Client. It fails only when no API key is configured.
func New(cfg Config) (*Client, error) {
	key := cfg.APIKey
	if key == "" {
		key = os.Getenv("OMW_API_KEY")
	}
	if key == "" {
		return nil, errors.New("openmailwire: no API key: set Config.APIKey or OMW_API_KEY")
	}
	base := cfg.BaseURL
	if base == "" {
		base = os.Getenv("OMW_API_URL")
	}
	if base == "" {
		base = DefaultBaseURL
	}
	c := &Client{
		BaseURL:      strings.TrimRight(base, "/"),
		apiKey:       key,
		http:         cfg.HTTPClient,
		pollInterval: cfg.PollInterval,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	// A copy, so the caller's client is left as it was.
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http = &hc
	if c.pollInterval <= 0 {
		c.pollInterval = time.Second
	}
	c.Emails = &EmailsService{c}
	c.Domains = &DomainsService{c}
	c.Webhooks = &WebhooksService{c}
	c.APIKeys = &APIKeysService{c}
	c.Inboxes = &InboxesService{c}
	c.Messages = &MessagesService{c}
	return c, nil
}

// Error is an error answered by the API, in its {statusCode, name, message}
// shape. Use errors.As to inspect it:
//
//	var apiErr *openmailwire.Error
//	if errors.As(err, &apiErr) && apiErr.Name == "domain_not_verified" { … }
type Error struct {
	StatusCode int
	// Name is the machine-readable error name, e.g. "validation_error".
	Name    string
	Message string
	// RetryAfter is the Retry-After header of a 429 answer, or 0.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("openmailwire: %d %s: %s", e.StatusCode, e.Name, e.Message)
}

// RequestOption changes one request.
type RequestOption func(*requestOptions)

type requestOptions struct {
	idempotencyKey string
}

// IdempotencyKey sets the Idempotency-Key header: the same key with the same
// request returns the same id instead of sending twice.
func IdempotencyKey(key string) RequestOption {
	return func(o *requestOptions) { o.idempotencyKey = key }
}

// Ptr returns a pointer to v, for optional fields such as
// SendEmailRequest.RetryUncertain or APIKeyPolicy.MaxPerHour.
func Ptr[T any](v T) *T { return &v }

func seg(s string) string { return url.PathEscape(s) }

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d.Round(time.Second)
		}
	}
	return 0
}

func apiError(resp *http.Response, data []byte) error {
	e := &Error{StatusCode: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		to := resp.Header.Get("Location")
		if to == "" {
			to = "another address"
		}
		e.Name = "redirect_refused"
		e.Message = fmt.Sprintf("%s %s answered %d, a redirect to %s. Redirects are not followed, so the API key never goes "+
			"elsewhere: set BaseURL to the address the API answers at", resp.Request.Method, resp.Request.URL.Redacted(), resp.StatusCode, to)
		return e
	}
	var body struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil && body.Name != "" {
		e.Name, e.Message = body.Name, body.Message
		return e
	}
	e.Name = "http_error"
	e.Message = strings.TrimSpace(string(data))
	if len(e.Message) > 500 {
		e.Message = e.Message[:500]
	}
	if e.Message == "" {
		e.Message = resp.Status
	}
	return e
}

// do sends a request and decodes the JSON answer into out. out may be nil,
// or a *[]byte to receive the raw body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any, opts []RequestOption) error {
	var o requestOptions
	for _, opt := range opts {
		opt(&o)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("openmailwire: encoding request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return fmt.Errorf("openmailwire: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", "openmailwire-go/"+Version)
	if _, raw := out.(*[]byte); raw {
		req.Header.Set("Accept", "*/*")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", o.idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openmailwire: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("openmailwire: %s %s: reading answer: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp, data)
	}
	switch v := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*v = data
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("openmailwire: %s %s: decoding answer: %w", method, path, err)
	}
	return nil
}

func query(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			q.Set(pairs[i], pairs[i+1])
		}
	}
	return q
}

func itoa(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}
