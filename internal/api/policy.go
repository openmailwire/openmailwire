package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/apikey"
	"github.com/openmailwire/openmailwire/internal/store"
)

// policyError is a refusal, in the API's error shape. Every step of the send
// path answers with one.
type policyError struct {
	Status     int
	Name       string
	Message    string
	RetryAfter time.Duration // sent as Retry-After when set
}

func (p *policyError) write(w http.ResponseWriter) {
	if p.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(p.RetryAfter.Seconds()))))
	}
	writeError(w, p.Status, p.Name, p.Message)
}

// prefixed returns a copy whose message starts with prefix (e.g. which
// batch item it is about).
func (p *policyError) prefixed(prefix string) *policyError {
	c := *p
	c.Message = prefix + c.Message
	return &c
}

func invalid(format string, args ...any) *policyError {
	return &policyError{Status: http.StatusUnprocessableEntity, Name: "validation_error", Message: fmt.Sprintf(format, args...)}
}

// internalErr logs err and returns the generic 500 answer.
func (s *Server) internalErr(what string, err error) *policyError {
	s.Log.Error(what, "err", err)
	return &policyError{Status: http.StatusInternalServerError, Name: "internal_error", Message: "internal error"}
}

// authorizeSend is the one place where an API key's sending rules are
// enforced. Every path that queues mail (the JSON API, batches, raw MIME,
// inbox replies, SMTP submission, MCP) must call it with the envelope
// sender address and the deduplicated recipients, before anything is
// stored. It returns nil to allow the send.
//
// It checks the key's allowed_from and allowed_recipients patterns, then
// its max_per_hour and max_per_day caps, then whether a circuit breaker has
// paused the key, the sending domain or the project. Storing through queue
// checks the caps and pauses again (the caps under a per-key lock), and
// claims the project's per-recipient cap (max_per_recipient_per_day) under
// a project lock, so concurrent requests cannot overshoot them together.
//
// Holds are not refusals: planSafety and rawSafety apply the key's
// require_approval rules.
func (s *Server) authorizeSend(r *http.Request, from string, recipients []string) *policyError {
	key := apiKeyOf(r)
	if pe := allowedBy(key.Policy, from, recipients); pe != nil {
		return pe
	}
	if pe := s.checkCaps(r.Context(), key, recipients); pe != nil {
		return pe
	}
	return s.checkPause(r.Context(), key, nil, []string{domainOf(strings.ToLower(from))})
}

// allowedBy checks the sender and recipients against a key's allowlists.
func allowedBy(p apikey.Policy, from string, recipients []string) *policyError {
	from = strings.ToLower(from)
	if !p.AllowsSender(from) {
		return &policyError{Status: http.StatusForbidden, Name: "sender_not_allowed", Message: fmt.Sprintf(
			"this API key may send only from addresses matching %s, so %s is not allowed; use one of those addresses, or ask the key's owner to change its allowed_from policy",
			apikey.Describe(p.AllowedFrom), from)}
	}
	if rcpt, bad := p.FirstDisallowedRecipient(recipients); bad {
		return &policyError{Status: http.StatusForbidden, Name: "recipient_not_allowed", Message: fmt.Sprintf(
			"this API key may send only to addresses matching %s, so %s is not allowed; remove that recipient, or ask the key's owner to change its allowed_recipients policy",
			apikey.Describe(p.AllowedRecipients), rcpt)}
	}
	return nil
}

// capsOf returns the key's send caps, or nil when it has none.
func capsOf(k store.APIKey) *store.SendCaps {
	if !k.Policy.HasCaps() {
		return nil
	}
	c := &store.SendCaps{ProjectID: k.ProjectID, KeyID: k.ID}
	if k.Policy.MaxPerHour != nil {
		c.PerHour = *k.Policy.MaxPerHour
	}
	if k.Policy.MaxPerDay != nil {
		c.PerDay = *k.Policy.MaxPerDay
	}
	return c
}

// checkCaps refuses a send that would take the key over a cap. Suppressed
// recipients are not sent to, so they do not count.
func (s *Server) checkCaps(ctx context.Context, key store.APIKey, recipients []string) *policyError {
	caps := capsOf(key)
	if caps == nil {
		return nil
	}
	n, err := s.Store.CountUnsuppressed(ctx, key.ProjectID, recipients)
	if err != nil {
		return s.internalErr("count unsuppressed", err)
	}
	ce, err := s.Store.CheckCaps(ctx, *caps, n)
	if err != nil {
		return s.internalErr("check caps", err)
	}
	if ce != nil {
		return capRefusal(ce)
	}
	return nil
}

func capRefusal(ce *store.CapError) *policyError {
	window := "hour"
	if ce.Window > time.Hour {
		window = "24 hours"
	}
	pe := &policyError{Status: http.StatusTooManyRequests, Name: "rate_limit_exceeded", RetryAfter: ce.RetryAfter}
	if ce.Adding > ce.Limit {
		pe.Message = fmt.Sprintf("this API key may send to at most %d recipients per %s (%s), and this request alone has %d; send it in smaller parts",
			ce.Limit, window, ce.Field, ce.Adding)
		return pe
	}
	pe.Message = fmt.Sprintf("this API key may send to at most %d recipients per %s (%s); %d were queued in the last %s and this request adds %d. Retry after %s (see the Retry-After header), or ask the key's owner to raise %s",
		ce.Limit, window, ce.Field, ce.Used, window, ce.Adding, ce.RetryAfter.Round(time.Second), ce.Field)
	return pe
}

// withFullAccess authenticates like withProject and also requires a
// full_access key.
func (s *Server) withFullAccess(next http.HandlerFunc) http.Handler {
	return s.withProject(func(w http.ResponseWriter, r *http.Request) {
		if requireFull(w, r) {
			next(w, r)
		}
	})
}

// requireFull answers 403 unless the request's key has full access.
func requireFull(w http.ResponseWriter, r *http.Request) bool {
	if apiKeyOf(r).FullAccess() {
		return true
	}
	writeError(w, http.StatusForbidden, "insufficient_permission", fmt.Sprintf(
		"this API key has sending_access: it can send email, read and cancel its own emails, and read domains. %s %s needs a full_access key",
		r.Method, r.URL.Path))
	return false
}

// ownKeyOnly returns the key whose emails the request may see: its own for
// a sending_access key, "" (every key) for full access.
func ownKeyOnly(r *http.Request) string {
	if k := apiKeyOf(r); !k.FullAccess() {
		return k.ID
	}
	return ""
}
