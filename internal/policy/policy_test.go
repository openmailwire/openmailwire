package policy

import (
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	later := now.Add(48 * time.Hour)
	cases := []struct {
		name     string
		in       Input
		status   string
		suppress bool
		event    string
	}{
		{"delivered", Input{Outcome: protocol.OutcomeDelivered, Attempts: 1, Now: now, ExpiresAt: later}, "delivered", false, "delivered"},
		{"mailbox gone suppresses", Input{Outcome: protocol.OutcomeBounced, Now: now, ExpiresAt: later, EnhancedCode: "5.1.1"}, "bounced", true, "bounced"},
		{"policy block does not suppress", Input{Outcome: protocol.OutcomeBounced, Now: now, ExpiresAt: later, EnhancedCode: "5.7.26"}, "bounced", false, "bounced"},
		{"temporary failure retries", Input{Outcome: protocol.OutcomeDeferred, Attempts: 1, Now: now, ExpiresAt: later}, "deferred", false, "deferred"},
		{"retry past expiry expires", Input{Outcome: protocol.OutcomeDeferred, Attempts: 7, Now: now, ExpiresAt: now.Add(time.Hour)}, "expired", false, "expired"},
		{"unknown retries by default", Input{Outcome: protocol.OutcomeUnknown, Attempts: 1, Now: now, ExpiresAt: later, RetryUncertain: true}, "deferred", false, "unknown_retry"},
		{"unknown can stop", Input{Outcome: protocol.OutcomeUnknown, Attempts: 1, Now: now, ExpiresAt: later}, "unknown", false, "unknown"},
	}
	for _, c := range cases {
		d := Decide(c.in)
		if d.Status != c.status || d.Suppress != c.suppress || d.Event != c.event {
			t.Errorf("%s: got %+v", c.name, d)
		}
	}
}

func TestDelay(t *testing.T) {
	if Delay(0) != time.Minute || Delay(1) != time.Minute || Delay(2) != 5*time.Minute || Delay(99) != 4*time.Hour {
		t.Fatal("backoff schedule changed")
	}
}
