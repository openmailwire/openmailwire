package webhook

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
)

// Audit R2 L4: email.received's data.to was the sender's To header, which
// can name another inbox (or anyone). It is the inbox that received the
// message now; the header is in header_to. The sender verdict comes along.
func TestReceivedPayloadToIsTheInbox(t *testing.T) {
	ev := store.EventRow{
		ID: 9, Type: "received", CreatedAt: time.Now(), EmailID: "m1", InboundMessageID: "m1",
		From: "x@attacker.test", To: []string{"tenant-b@acme.test"}, InboxID: "i1", InboxAddress: "tenant-a@acme.test",
		Data: []byte(`{"trust":"failed","held":true,"suspicious_instructions":false,"authentication":{"spf":"fail","dkim":"none","dmarc":"fail"}}`),
	}
	b, err := Payload(ev, EmailReceived)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Data struct {
			To                     []string          `json:"to"`
			HeaderTo               []string          `json:"header_to"`
			Trust                  string            `json:"trust"`
			Held                   *bool             `json:"held"`
			SuspiciousInstructions *bool             `json:"suspicious_instructions"`
			Authentication         map[string]string `json:"authentication"`
		} `json:"data"`
	}
	json.Unmarshal(b, &p)
	d := p.Data
	if len(d.To) != 1 || d.To[0] != "tenant-a@acme.test" || len(d.HeaderTo) != 1 || d.HeaderTo[0] != "tenant-b@acme.test" ||
		d.Trust != "failed" || d.Held == nil || !*d.Held || d.SuspiciousInstructions == nil || *d.SuspiciousInstructions ||
		d.Authentication["dmarc"] != "fail" {
		t.Fatalf("received payload: %s", b)
	}
}

// Audit R2 H2: a complaint that names no recipient listed every recipient
// of the email in data.to, so handlers that suppress data.to suppressed
// them all.
func TestComplaintWithoutRecipient(t *testing.T) {
	ev := store.EventRow{ID: 10, Type: "complained", CreatedAt: time.Now(), EmailID: "e2", From: "a@acme.test",
		To: []string{"x@example.test", "y@example.test"}, Data: []byte(`{"source":"arf","feedback_type":"abuse"}`)}
	b, _ := Payload(ev, EmailComplained)
	var p struct {
		Data struct {
			To []string `json:"to"`
		} `json:"data"`
	}
	json.Unmarshal(b, &p)
	if p.Data.To == nil || len(p.Data.To) != 0 {
		t.Fatalf("complaint payload: %s", b)
	}
}
