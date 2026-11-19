package policy

import (
	"regexp"
	"strings"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// What a temporary failure says about the receiver, and what it does not.
//
// Receivers answer 4xx for very different reasons. Only some of them mean
// "your IP is sending too fast", and only those may slow down a mailbox
// provider for every project that shares the sending IP:
//
//   - a 421 (the server closes the connection), or 4.7.0, 4.7.28 or 4.7.650
//     at the greeting, EHLO or MAIL FROM, before any recipient is named;
//   - never a mailbox-level code (4.2.x: one recipient's mailbox is busy or
//     receiving too fast), "too many recipients" (4.5.3), an address problem
//     (4.1.x), or an authentication code (4.7.26, 4.7.27, 4.7.29 to 4.7.32).
//
// Free text ("rate", "too many") is not used: it matches per-mailbox replies
// such as Gmail's "450 4.2.1 ... receiving mail at a rate that prevents
// additional messages from being delivered".

// AuthCodes are the enhanced codes receivers (Gmail first) use when the
// sending domain's authentication is missing or wrong: no SPF or DKIM pass,
// DMARC failure, From not aligned, or no TLS where it is required. They are
// about one sending domain of one project, so they are recorded there
// instead of throttling the provider.
var AuthCodes = map[string]bool{
	"4.7.26": true, "5.7.26": true, // unauthenticated (DMARC)
	"4.7.27": true, "5.7.27": true, // SPF did not pass
	"4.7.29": true, "5.7.29": true, // no TLS
	"4.7.30": true, "5.7.30": true, // DKIM did not pass
	"4.7.31": true, "5.7.31": true,
	"4.7.32": true, "5.7.32": true, // From not aligned with SPF or DKIM
}

// IsAuthProblem reports whether a reply is about the sending domain's
// authentication (AuthCodes).
func IsAuthProblem(enhanced string) bool {
	return AuthCodes[strings.TrimSpace(enhanced)]
}

// throttleEnhanced are the enhanced codes that mean "slow down" when they
// come before any recipient is named.
var throttleEnhanced = map[string]bool{
	"4.7.0":   true, // generic, used for connection rate limits
	"4.7.28":  true, // Gmail: unusual rate from your IP
	"4.7.650": true, // Outlook: the IP is rate limited (S775)
}

// IsThrottle reports whether a deferral is a connection- or IP-level signal
// to slow down (see above). stage is the SMTP step that got the reply
// (protocol.Stage*); replies from workers that do not report it count only
// when they are a 421. Only replies from the receiver count (code 4xx); a
// worker's own failures (connect errors, TLS policy) never do.
func IsThrottle(outcome string, code int, enhanced, stage string) bool {
	if outcome != protocol.OutcomeDeferred || code < 400 || code > 499 {
		return false
	}
	enhanced = strings.TrimSpace(enhanced)
	switch {
	case strings.HasPrefix(enhanced, "4.1."), strings.HasPrefix(enhanced, "4.2."),
		enhanced == "4.5.3", AuthCodes[enhanced]:
		return false
	case code == 421:
		return true
	}
	switch stage {
	case protocol.StageGreeting, protocol.StageEHLO, protocol.StageMail:
		return throttleEnhanced[enhanced]
	}
	return false
}

var greylistText = regexp.MustCompile(`(?i)\bgr[ae]y[- ]?list`)

// IsGreylist reports whether a deferral is greylisting: the receiver refuses
// the first attempt from an unknown sender and accepts a retry a few minutes
// later. Greylisting is recognised by its wording ("greylisted",
// "graylisting", "grey-listed"), which is safe here because it only changes
// when the retry comes and keeps the attempt from counting toward the relay
// hand-over.
func IsGreylist(outcome string, code int, response string) bool {
	if outcome != protocol.OutcomeDeferred || code < 400 || code > 499 {
		return false
	}
	return greylistText.MatchString(response)
}
