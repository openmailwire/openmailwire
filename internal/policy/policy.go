// Package policy decides what happens to a delivery after an attempt:
// done, retry later, give up, or suppress the address.
package policy

import (
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// The first retry comes 5 to 15 minutes after the first attempt (a random
// point in that window, so retries of a burst spread out), which is also long
// enough for greylisting. Later retries follow Backoff.
const (
	FirstRetry       = 5 * time.Minute
	FirstRetrySpread = 10 * time.Minute
)

// Backoff is the wait before attempt n+1, indexed by n-1, for n >= 2. The
// last entry repeats until the message expires.
var Backoff = []time.Duration{
	FirstRetry, // attempt 1: FirstRetry plus up to FirstRetrySpread
	30 * time.Minute,
	time.Hour,
	2 * time.Hour,
	4 * time.Hour,
}

// Delay returns how long to wait after the given number of attempts. jitter
// (0 to 1) places the first retry within its window; later retries do not
// use it.
func Delay(attempts int, jitter float64) time.Duration {
	i := attempts - 1
	if i < 1 {
		return firstRetry(jitter)
	}
	if i >= len(Backoff) {
		i = len(Backoff) - 1
	}
	return Backoff[i]
}

func firstRetry(jitter float64) time.Duration {
	if jitter < 0 || jitter != jitter {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	return FirstRetry + time.Duration(jitter*float64(FirstRetrySpread))
}

type Input struct {
	Outcome        string
	Attempts       int // attempts made so far, including this one
	Now            time.Time
	ExpiresAt      time.Time
	RetryUncertain bool
	SMTPCode       int
	EnhancedCode   string
	// Greylisted: the receiver greylisted this attempt. The retry then comes
	// 5 to 15 minutes later whatever the attempt count, because greylisting
	// lets the next try through.
	Greylisted bool
	// Jitter (0 to 1) places a retry within its window; callers pass a
	// random number.
	Jitter float64
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
	delay := Delay(in.Attempts, in.Jitter)
	if in.Greylisted {
		delay = firstRetry(in.Jitter)
	}
	next := in.Now.Add(delay)
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
