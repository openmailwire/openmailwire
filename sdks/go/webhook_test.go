package openmailwire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// The reference vector from Svix's own libraries; the server's signer
// (internal/webhook.Sign) produces the same signature.
const (
	vectorSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vectorID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTS     = "1614265330"
	vectorBody   = `{"test": 2432232314}`
	vectorSig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
	forever      = 100 * 365 * 24 * time.Hour
)

var (
	testSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	testBody   = []byte(`{"type":"email.delivered","created_at":"2026-11-18T10:00:00Z","data":{"email_id":"e1","to":["x@example.test"]}}`)
)

func sign(secret, id string, ts int64, body []byte) string {
	key, _ := base64.StdEncoding.DecodeString(secret[len("whsec_"):])
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%d.", id, ts)
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func svixHeader(ts int64) http.Header {
	h := http.Header{}
	h.Set("svix-id", "msg_1")
	h.Set("svix-timestamp", strconv.FormatInt(ts, 10))
	h.Set("svix-signature", sign(testSecret, "msg_1", ts, testBody))
	return h
}

func TestVerifyWebhookSvixVector(t *testing.T) {
	h := http.Header{}
	h.Set("svix-id", vectorID)
	h.Set("svix-timestamp", vectorTS)
	h.Set("svix-signature", vectorSig)
	if err := VerifyWebhook(vectorSecret, h, []byte(vectorBody), forever); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyWebhookSvixHeaders(t *testing.T) {
	ev, err := ParseWebhook(testSecret, svixHeader(time.Now().Unix()), testBody, 0)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ev.Type, "email.delivered")
	eq(t, ev.Data.EmailID, "e1")
	eq(t, ev.Data.To, []string{"x@example.test"})
}

func TestVerifyWebhookStandardHeaders(t *testing.T) {
	ts := time.Now().Unix()
	h := http.Header{}
	h.Set("Webhook-Id", "msg_1")
	h.Set("Webhook-Timestamp", strconv.FormatInt(ts, 10))
	h.Set("Webhook-Signature", sign(testSecret, "msg_1", ts, testBody))
	if err := VerifyWebhook(testSecret, h, testBody, 0); err != nil {
		t.Fatal(err)
	}
	// A hand-built map with lowercase (non-canonical) keys works too.
	raw := http.Header{"webhook-id": {"msg_1"}, "webhook-timestamp": {strconv.FormatInt(ts, 10)}, "webhook-signature": {h.Get("Webhook-Signature")}}
	if err := VerifyWebhook(testSecret, raw, testBody, 0); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyWebhookSeveralSignatures(t *testing.T) {
	ts := time.Now().Unix()
	h := svixHeader(ts)
	other := sign("whsec_"+base64.StdEncoding.EncodeToString([]byte("rotated out")), "msg_1", ts, testBody)
	h.Set("svix-signature", "v2,ignored "+other+" "+h.Get("svix-signature"))
	if err := VerifyWebhook(testSecret, h, testBody, 0); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyWebhookRefusesForgeries(t *testing.T) {
	ts := time.Now().Unix()
	h := svixHeader(ts)
	if err := VerifyWebhook(testSecret, h, []byte(`{"type":"email.bounced"}`), 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("changed body: %v", err)
	}
	if err := VerifyWebhook("whsec_"+base64.StdEncoding.EncodeToString([]byte("wrong")), h, testBody, 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("wrong secret: %v", err)
	}
	bad := h.Clone()
	bad.Set("svix-signature", "v1,AAAA")
	if err := VerifyWebhook(testSecret, bad, testBody, 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("wrong signature: %v", err)
	}
	bad = h.Clone()
	bad.Set("svix-id", "msg_2")
	if err := VerifyWebhook(testSecret, bad, testBody, 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("other id: %v", err)
	}
}

func TestVerifyWebhookTimestamps(t *testing.T) {
	now := time.Now().Unix()
	for _, c := range []struct {
		ts        int64
		tolerance time.Duration
		ok        bool
	}{
		{now - 301, 0, false},
		{now + 301, 0, false},
		{now - 290, 0, true},
		{now - 20, 10 * time.Second, false},
		{now - 600, 15 * time.Minute, true},
	} {
		err := VerifyWebhook(testSecret, svixHeader(c.ts), testBody, c.tolerance)
		if c.ok && err != nil {
			t.Fatalf("ts %+d: %v", c.ts-now, err)
		}
		if !c.ok && !errors.Is(err, ErrWebhookTimestamp) {
			t.Fatalf("ts %+d: want ErrWebhookTimestamp, got %v", c.ts-now, err)
		}
	}
}

func TestVerifyWebhookBadInput(t *testing.T) {
	if err := VerifyWebhook(testSecret, http.Header{}, testBody, 0); !errors.Is(err, ErrWebhookHeaders) {
		t.Fatal(err)
	}
	h := svixHeader(time.Now().Unix())
	h.Del("svix-signature")
	if err := VerifyWebhook(testSecret, h, testBody, 0); !errors.Is(err, ErrWebhookHeaders) {
		t.Fatal(err)
	}
	h = svixHeader(time.Now().Unix())
	h.Set("svix-timestamp", "soon")
	if err := VerifyWebhook(testSecret, h, testBody, 0); !errors.Is(err, ErrWebhookTimestamp) {
		t.Fatal(err)
	}
	if err := VerifyWebhook("whsec_***", svixHeader(time.Now().Unix()), testBody, 0); !errors.Is(err, ErrWebhookSecret) {
		t.Fatal(err)
	}
}

// Ed25519 ("v1a"). The request vector is pinned on the server side by
// internal/webhook TestSDKVector (seed 00 01 … 1f); every SDK checks it.
const (
	v1aPublicKey = "whpk_A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	v1aID        = "msg_2Lh9KQHvQbLvjGF3s3hQqZ1u"
	v1aTS        = "1700000000"
	v1aBody      = `{"type":"email.delivered","data":{"email_id":"e1"}}`
	v1aSig       = "v1a,De0zbXw3fa+WDUWMYmuFVJsBTTtbEkQJi1dS49Vm48H2mguOCtQ8embnNXP20I+H5cLV4SkJSc/+h7BGhrm2Dw=="
)

func v1aHeader(sig string) http.Header {
	h := http.Header{}
	h.Set("webhook-id", v1aID)
	h.Set("webhook-timestamp", v1aTS)
	h.Set("webhook-signature", sig)
	return h
}

func TestVerifyWebhookV1a(t *testing.T) {
	ev, err := ParseWebhook(v1aPublicKey, v1aHeader(v1aSig), []byte(v1aBody), forever)
	if err != nil || ev.Data.EmailID != "e1" {
		t.Fatalf("v1a vector: %v %+v", err, ev)
	}
	// Next to v1 signatures and during a rotation (two v1a signatures).
	other := "v1a," + base64.StdEncoding.EncodeToString(make([]byte, 64))
	if err := VerifyWebhook(v1aPublicKey, v1aHeader("v1,AAAA "+other+" "+v1aSig), []byte(v1aBody), forever); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		secret, sig, body string
		want              error
	}{
		"changed body":       {v1aPublicKey, v1aSig, `{"type":"email.bounced"}`, ErrWebhookSignature},
		"HMAC secret":        {testSecret, v1aSig, v1aBody, ErrWebhookSignature},
		"labelled v1":        {v1aPublicKey, "v1," + v1aSig[4:], v1aBody, ErrWebhookSignature},
		"short public key":   {"whpk_AAAA", v1aSig, v1aBody, ErrWebhookSecret},
		"invalid public key": {"whpk_***", v1aSig, v1aBody, ErrWebhookSecret},
	} {
		if err := VerifyWebhook(tc.secret, v1aHeader(tc.sig), []byte(tc.body), forever); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	if err := VerifyWebhook(v1aPublicKey, v1aHeader(v1aSig), []byte(v1aBody), 0); !errors.Is(err, ErrWebhookTimestamp) {
		t.Fatalf("old vector within the default tolerance: %v", err)
	}
}

// A timestamp in the year 292277026596 overflowed the age arithmetic and
// passed the 5-minute tolerance.
func TestVerifyWebhookFarFutureTimestamp(t *testing.T) {
	for _, ts := range []string{"9223372036854775807", "999999999999999", "1000000000000000"} {
		unix, _ := strconv.ParseInt(ts, 10, 64)
		h := http.Header{}
		h.Set("webhook-id", "msg_1")
		h.Set("webhook-timestamp", ts)
		h.Set("webhook-signature", sign(testSecret, "msg_1", unix, testBody))
		if err := VerifyWebhook(testSecret, h, testBody, 0); !errors.Is(err, ErrWebhookTimestamp) {
			t.Errorf("timestamp %s: %v", ts, err)
		}
	}
}
