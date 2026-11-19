package policy

import (
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// TestIsThrottle covers R3#1: only connection- or IP-level replies slow a
// provider down for everyone. Per-mailbox limits (Gmail's 4.2.1 "receiving
// mail at a rate"), authentication codes (4.7.26 to 4.7.32 but 4.7.28) and
// free text never do.
func TestIsThrottle(t *testing.T) {
	d := protocol.OutcomeDeferred
	g, e, m, r, dt := protocol.StageGreeting, protocol.StageEHLO, protocol.StageMail, protocol.StageRcpt, protocol.StageMessage
	cases := []struct {
		outcome, enhanced, stage string
		code                     int
		want                     bool
	}{
		{d, "", g, 421, true},         // service not available
		{d, "4.7.0", e, 421, true},    // too many connections, at EHLO
		{d, "4.7.28", dt, 421, true},  // Gmail: unusual rate from your IP
		{d, "", "", 421, true},        // an older worker without stages
		{d, "4.7.28", m, 450, true},   // a rate code before any recipient
		{d, "4.7.650", m, 451, true},  // Outlook S775: the IP is rate limited
		{d, "4.7.0", g, 451, true},    // generic, at the greeting
		{d, "4.7.0", r, 451, false},   // generic, about a recipient
		{d, "4.7.650", r, 451, false}, // not before any recipient: stays per recipient
		{d, "4.7.0", "", 451, false},  // no stage known
		{d, "4.3.2", m, 452, false},   // no longer a throttle
		{d, "4.2.1", r, 450, false},   // Gmail: this mailbox receives too fast
		{d, "4.2.1", dt, 421, false},  // even as a 421
		{d, "4.2.2", r, 452, false},   // mailbox full
		{d, "4.5.3", r, 452, false},   // too many recipients
		{d, "4.1.8", m, 421, false},   // a sender address problem
		{d, "4.7.27", dt, 421, false}, // Gmail: SPF did not pass
		{d, "4.7.30", dt, 421, false}, // Gmail: DKIM did not pass
		{d, "4.7.32", dt, 421, false}, // Gmail: From not aligned
		{d, "4.7.26", dt, 421, false}, // Gmail: unauthenticated
		{d, "4.2.0", r, 450, false},   // "Too many connections" as free text no longer counts
		{d, "4.4.1", dt, 451, false},  //
		{d, "4.7.0", "", 0, false},    // the worker's own failure (STARTTLS)
		{d, "4.7.5", "", 0, false},    // a TLS policy, not the receiver
		{protocol.OutcomeBounced, "5.7.1", r, 550, false},
		{protocol.OutcomeUnknown, "", dt, 421, false},
	}
	for _, c := range cases {
		if got := IsThrottle(c.outcome, c.code, c.enhanced, c.stage); got != c.want {
			t.Errorf("IsThrottle(%q, %d, %q, %q) = %v", c.outcome, c.code, c.enhanced, c.stage, got)
		}
	}
}

func TestIsAuthProblem(t *testing.T) {
	for _, c := range []string{"4.7.26", "4.7.27", "4.7.30", "4.7.32", "5.7.26", "5.7.27", "5.7.30", "5.7.32", " 4.7.29 "} {
		if !IsAuthProblem(c) {
			t.Errorf("%s is an authentication problem", c)
		}
	}
	for _, c := range []string{"4.7.28", "4.7.0", "5.7.1", "4.2.1", ""} {
		if IsAuthProblem(c) {
			t.Errorf("%s is not an authentication problem", c)
		}
	}
}

func TestIsGreylist(t *testing.T) {
	d := protocol.OutcomeDeferred
	cases := []struct {
		code     int
		response string
		want     bool
	}{
		{451, "451 4.7.1 Greylisting in action, please come back later", true},
		{450, "450 4.2.0 <a@b.test>: Recipient address rejected: Greylisted, see http://postgrey.schweikert.ch/help/b.test.html", true},
		{451, "451 Temporarily rejected: graylisted", true},
		{451, "451 4.7.1 grey-listed, try again in 5 minutes", true},
		{451, "451 4.3.0 temporary failure", false},
		{550, "550 greylisted forever?", false},
		{0, "greylist", false},
	}
	for _, c := range cases {
		if got := IsGreylist(d, c.code, c.response); got != c.want {
			t.Errorf("IsGreylist(%d, %q) = %v", c.code, c.response, got)
		}
	}
	if IsGreylist(protocol.OutcomeBounced, 451, "greylisted") {
		t.Error("only deferrals")
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
