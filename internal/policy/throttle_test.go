package policy

import (
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

func TestIsThrottle(t *testing.T) {
	d := protocol.OutcomeDeferred
	cases := []struct {
		outcome, enhanced, response string
		code                        int
		want                        bool
	}{
		{d, "", "421 service not available", 421, true},
		{d, "4.7.28", "450 4.7.28 unusual rate of unsolicited mail", 450, true},
		{d, "4.7.650", "451 4.7.650 the mail server has been temporarily rate limited", 451, true},
		{d, "4.7.0", "451 4.7.0 temporary failure", 451, true},
		{d, "4.3.2", "452 4.3.2 system not accepting messages", 452, true},
		{d, "4.2.0", "450 4.2.0 Too many connections from your IP", 450, true},
		{d, "4.2.0", "450 4.2.0 Ratelimit exceeded", 450, true},
		{d, "4.2.0", "450 4.2.0 mailbox busy, try later", 450, false},
		{d, "4.4.1", "451 4.4.1 could not generate a separate queue entry", 451, false},
		{d, "4.7.0", "STARTTLS: handshake failed", 0, false},                     // the worker's own failure
		{d, "4.7.5", "MTA-STS: no matching MX", 0, false},                        // a TLS policy, not the receiver
		{protocol.OutcomeBounced, "5.7.1", "550 5.7.1 rate limited", 550, false}, // permanent
		{protocol.OutcomeUnknown, "", "421 too many", 421, false},
	}
	for _, c := range cases {
		if got := IsThrottle(c.outcome, c.code, c.enhanced, c.response); got != c.want {
			t.Errorf("IsThrottle(%q, %d, %q, %q) = %v", c.outcome, c.code, c.enhanced, c.response, got)
		}
	}
}

func TestThrottleBackoff(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := ThrottleBackoff(i + 1); got != w {
			t.Errorf("ThrottleBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if ThrottleBackoff(0) != time.Minute || ThrottleBackoff(1000) != 30*time.Minute {
		t.Error("out-of-range n")
	}
}
