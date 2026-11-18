// Package policy decides what happens to a delivery after an attempt:
// done, retry later, give up, or suppress the address.
package policy

import (
	"regexp"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Backoff is the wait before attempt n+1, indexed by n-1. The last entry
// repeats until the message expires.
var Backoff = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	time.Hour,
	2 * time.Hour,
	4 * time.Hour,
}

// Delay returns how long to wait after the given number of attempts.
func Delay(attempts int) time.Duration {
	i := attempts - 1
	if i < 0 {
		i = 0
	}
	if i >= len(Backoff) {
		i = len(Backoff) - 1
	}
	return Backoff[i]
}

type Input struct {
	Outcome        string
	Attempts       int // attempts made so far, including this one
	Now            time.Time
	ExpiresAt      time.Time
	RetryUncertain bool
	SMTPCode       int
	EnhancedCode   string
}

type Decision struct {
	Status      string // delivered, bounced, deferred, expired or unknown
	NextAttempt time.Time
	Suppress    bool // add the recipient to the project's suppression list
	Event       string
}

// Decide maps one attempt's outcome to the delivery's next state.
func Decide(in Input) Decision {
	switch in.Outcome {
	case protocol.OutcomeDelivered:
		return Decision{Status: "delivered", NextAttempt: in.Now, Event: "delivered"}
	case protocol.OutcomeBounced:
		return Decision{
			Status:      "bounced",
			NextAttempt: in.Now,
			Suppress:    IsRecipientFailure(in.EnhancedCode),
			Event:       "bounced",
		}
	case protocol.OutcomeUnknown:
		if !in.RetryUncertain {
			return Decision{Status: "unknown", NextAttempt: in.Now, Event: "unknown"}
		}
	}
	next := in.Now.Add(Delay(in.Attempts))
	if !next.Before(in.ExpiresAt) {
		return Decision{Status: "expired", NextAttempt: in.Now, Event: "expired"}
	}
	event := "deferred"
	if in.Outcome == protocol.OutcomeUnknown {
		event = "unknown_retry"
	}
	return Decision{Status: "deferred", NextAttempt: next, Event: event}
}

// IsRecipientFailure reports whether an enhanced status code says the
// address itself is bad. Only these justify suppressing the address. A
// policy or authentication rejection (5.7.x) is the sender's problem and must
// never suppress recipients.
func IsRecipientFailure(enhanced string) bool {
	switch strings.TrimSpace(enhanced) {
	case "5.1.1", // bad destination mailbox
		"5.1.2",  // bad destination system (domain)
		"5.1.3",  // bad destination mailbox address syntax
		"5.1.6",  // mailbox has moved
		"5.1.10", // null MX
		"5.2.1":  // mailbox disabled
		return true
	}
	return false
}

var throttleText = regexp.MustCompile(`(?i)\brate|too many`)

// IsThrottle reports whether a deferral says the receiver wants us to slow
// down: a 421, one of the enhanced codes receivers use for rate limits
// (4.7.0, 4.7.28 at Gmail, 4.7.650 at Outlook, 4.3.2), or a reply that talks
// about rates or "too many". Only replies from the receiver count (code
// 4xx); a worker's own failures (connect errors, TLS policy) never do.
func IsThrottle(outcome string, code int, enhanced, response string) bool {
	if outcome != protocol.OutcomeDeferred || code < 400 || code > 499 {
		return false
	}
	if code == 421 {
		return true
	}
	switch strings.TrimSpace(enhanced) {
	case "4.7.0", "4.7.28", "4.7.650", "4.3.2":
		return true
	}
	return throttleText.MatchString(response)
}

// ThrottleBackoff is how long direct workers leave a domain alone after its
// n-th consecutive throttling deferral (n starts at 1): 1, 2, 4, 8, 16
// minutes, then 30 minutes.
func ThrottleBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 6 {
		return 30 * time.Minute
	}
	d := time.Duration(1<<(n-1)) * time.Minute
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}
