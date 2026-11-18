package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Client calls the control plane's internal inbound API with a worker token.
type Client struct {
	URL   string // the control plane, e.g. https://mail.example.com
	Token string // omwk_…
	HTTP  *http.Client
}

// HTTPError is an error answer from the control plane.
type HTTPError struct {
	Status  int
	Name    string
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("control plane answered %d %s: %s", e.Status, e.Name, e.Message)
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Name, Message string }
		json.Unmarshal(data, &e)
		return &HTTPError{Status: resp.StatusCode, Name: e.Name, Message: e.Message}
	}
	return json.Unmarshal(data, out)
}

// Check asks whether the control plane accepts mail for rcpt.
func (c *Client) Check(ctx context.Context, rcpt string) (protocol.InboundCheckResponse, error) {
	var out protocol.InboundCheckResponse
	err := c.post(ctx, "/internal/inbound/check", protocol.InboundCheckRequest{Rcpt: rcpt}, &out)
	return out, err
}

// Deliver hands an accepted message to the control plane.
func (c *Client) Deliver(ctx context.Context, m protocol.InboundMessage) (protocol.InboundResponse, error) {
	var out protocol.InboundResponse
	err := c.post(ctx, "/internal/inbound", m, &out)
	return out, err
}
