package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/store"
)

// verifySvix is an independent implementation of the receiver side, written
// the way Resend's own docs describe it.
func verifySvix(whsec, id, ts string, body []byte, header string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(whsec, "whsec_"))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "." + string(body)))
	want := base64.StdEncoding.EncodeToString(m.Sum(nil))
	for _, part := range strings.Fields(header) {
		if v, ok := strings.CutPrefix(part, "v1,"); ok && hmac.Equal([]byte(v), []byte(want)) {
			return true
		}
	}
	return false
}

func TestSignMatchesSvixReceiver(t *testing.T) {
	whsec := NewSecret()
	body := []byte(`{"type":"email.delivered"}`)
	sig, err := Sign(whsec, "msg_1", 1700000000, body)
	if err != nil {
		t.Fatal(err)
	}
	if !verifySvix(whsec, "msg_1", "1700000000", body, sig) {
		t.Fatal("signature does not verify")
	}
	if verifySvix(whsec, "msg_1", "1700000000", []byte(`{"type":"email.bounced"}`), sig) {
		t.Fatal("signature verified a different body")
	}
}

func TestPayloadIsPerRecipient(t *testing.T) {
	ev := store.EventRow{
		ID: 7, Type: "bounced", CreatedAt: time.Now(), EmailID: "e1", From: "a@acme.test",
		To: []string{"x@example.test", "y@example.test"}, Subject: "hi", Recipient: "y@example.test",
		Data: []byte(`{"smtp_code":550,"enhanced_code":"5.1.1","response":"550 5.1.1 no such user"}`),
	}
	if ExternalType(ev.Type) != EmailBounced {
		t.Fatal("bounced should map to email.bounced")
	}
	b, err := Payload(ev, EmailBounced)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Data struct {
			To     []string `json:"to"`
			Bounce struct{ Type, SubType, Message string }
		} `json:"data"`
	}
	json.Unmarshal(b, &p)
	if len(p.Data.To) != 1 || p.Data.To[0] != "y@example.test" {
		t.Fatalf("to should hold only the bounced recipient: %v", p.Data.To)
	}
	if p.Data.Bounce.Type != "Permanent" || p.Data.Bounce.SubType != "NoEmail" || !strings.Contains(p.Data.Bounce.Message, "no such user") {
		t.Fatalf("bounce: %+v", p.Data.Bounce)
	}
}

func TestBuildRespectsSubscriptionAndCreationTime(t *testing.T) {
	now := time.Now()
	ev := store.EventRow{ID: 1, Type: "delivered", CreatedAt: now, EmailID: "e1"}
	hooks := []store.Webhook{
		{ID: "all", CreatedAt: now.Add(-time.Hour)},
		{ID: "bounces-only", Events: []string{EmailBounced}, CreatedAt: now.Add(-time.Hour)},
		{ID: "too-new", CreatedAt: now.Add(time.Minute)},
	}
	out := Build(ev, hooks)
	if len(out) != 1 || out[0].WebhookID != "all" || out[0].Type != EmailDelivered {
		t.Fatalf("got %+v", out)
	}
	if Build(store.EventRow{Type: "late_report_bounced"}, hooks) != nil {
		t.Fatal("events without a webhook type must produce nothing")
	}
}

func TestEndpointRules(t *testing.T) {
	for _, bad := range []string{"http://example.com/h", "ftp://example.com", "https://user:pw@example.com/h", "/relative"} {
		if ValidateEndpoint(bad, false) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if ValidateEndpoint("https://example.com/hooks/omw", false) != nil || ValidateEndpoint("http://localhost:3000/h", true) != nil {
		t.Fatal("rejected a valid endpoint")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if _, err := NewHTTPClient(false).Get(srv.URL); !errors.Is(err, errBlocked) {
		t.Fatalf("loopback endpoint was not blocked: %v", err)
	}
	if _, err := NewHTTPClient(true).Get(srv.URL); err != nil {
		t.Fatalf("allowPrivate should permit loopback: %v", err)
	}
}

func TestInboundPayloads(t *testing.T) {
	ev := store.EventRow{
		ID: 9, Type: "received", CreatedAt: time.Now(), EmailID: "m1", InboundMessageID: "m1",
		From: "María <maria@example.test>", Subject: "Re: hi", Cc: []string{"ops@example.test"},
		MessageID: "r1@example.test", InboxID: "i1", InboxAddress: "support@acme.test", ThreadID: "e1", InReplyToEmailID: "e1",
	}
	if ExternalType(ev.Type) != EmailReceived || !ValidType(EmailReceived) || !ValidType(EmailComplained) {
		t.Fatal("received must map to email.received")
	}
	b, err := Payload(ev, EmailReceived)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	json.Unmarshal(b, &p)
	data := p["data"].(map[string]any)
	// With no To header (the inbox was a Bcc), "to" is the inbox address.
	if data["email_id"] != "m1" || data["to"].([]any)[0] != "support@acme.test" || data["inbox"] != "support@acme.test" ||
		data["thread_id"] != "e1" || data["in_reply_to_email_id"] != "e1" || data["message_id"] != "<r1@example.test>" ||
		data["delivery"] != nil || data["bounce"] != nil {
		t.Fatalf("received payload: %s", b)
	}

	ev = store.EventRow{
		ID: 10, Type: "complained", CreatedAt: time.Now(), EmailID: "e2", From: "a@acme.test",
		To: []string{"x@example.test", "y@example.test"}, Recipient: "y@example.test",
		Data: []byte(`{"source":"arf","feedback_type":"abuse","user_agent":"Yahoo!-Mail-Feedback/2.0"}`),
	}
	if ExternalType(ev.Type) != EmailComplained {
		t.Fatal("complained must map to email.complained")
	}
	b, _ = Payload(ev, EmailComplained)
	p = nil
	json.Unmarshal(b, &p)
	data = p["data"].(map[string]any)
	c := data["complaint"].(map[string]any)
	if len(data["to"].([]any)) != 1 || data["to"].([]any)[0] != "y@example.test" || c["feedback_type"] != "abuse" ||
		c["user_agent"] != "Yahoo!-Mail-Feedback/2.0" || data["inbox"] != nil {
		t.Fatalf("complained payload: %s", b)
	}
}
