package api

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/dkimkey"
	"github.com/openmailwire/openmailwire/internal/message"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

const (
	maxRecipients = 50
	maxExpiry     = 7 * 24 * time.Hour
)

// strList accepts "a@b.c" or ["a@b.c", …], like Resend.
type strList []string

func (l *strList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one != "" {
			*l = []string{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("must be a string or an array of strings")
	}
	*l = many
	return nil
}

type tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type sendRequest struct {
	From           string            `json:"from"`
	To             strList           `json:"to"`
	Cc             strList           `json:"cc"`
	Bcc            strList           `json:"bcc"`
	ReplyTo        strList           `json:"reply_to"`
	Subject        string            `json:"subject"`
	HTML           string            `json:"html"`
	Text           string            `json:"text"`
	Headers        map[string]string `json:"headers"`
	Tags           []tag             `json:"tags"`
	ExpiresAt      *time.Time        `json:"expires_at"`
	RetryUncertain *bool             `json:"retry_uncertain"`
}

var tagRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func (s *Server) sendEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid := projectID(r)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
		return
	}
	var req sendRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(idem) > 256 {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "Idempotency-Key is longer than 256 characters")
		return
	}

	from, err := mail.ParseAddress(req.From)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "from: "+err.Error())
		return
	}
	fromDomain := strings.ToLower(domainOf(from.Address))
	switch {
	case len(req.To) == 0:
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "to is required")
		return
	case strings.TrimSpace(req.Subject) == "":
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "subject is required")
		return
	case len(req.Subject) > 998:
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "subject is longer than 998 bytes")
		return
	case req.HTML == "" && req.Text == "":
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "html or text is required")
		return
	}
	recipients, err := envelope(req.To, req.Cc, req.Bcc)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if pe := s.authorizeSend(r, strings.ToLower(from.Address), recipients); pe != nil {
		pe.write(w)
		return
	}
	for _, a := range req.ReplyTo {
		if _, err := mail.ParseAddress(a); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", fmt.Sprintf("reply_to %q: %v", a, err))
			return
		}
	}
	if err := message.ValidateHeaders(req.Headers); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	tags := map[string]string{}
	for _, t := range req.Tags {
		if !tagRE.MatchString(t.Name) || !tagRE.MatchString(t.Value) {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "tag names and values may contain only letters, digits, _ and - (1-256 characters)")
			return
		}
		tags[t.Name] = t.Value
	}
	now := time.Now().UTC()
	expires := now.Add(s.Cfg.DefaultExpiry)
	if req.ExpiresAt != nil {
		if !req.ExpiresAt.After(now) || req.ExpiresAt.After(now.Add(maxExpiry)) {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "expires_at must be in the future and within 7 days")
			return
		}
		expires = req.ExpiresAt.UTC()
	}
	retryUncertain := true
	if req.RetryUncertain != nil {
		retryUncertain = *req.RetryUncertain
	}

	dom, err := s.Store.DomainByName(ctx, pid, fromDomain)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusForbidden, "domain_not_found",
			fmt.Sprintf("the domain %s is not added to this project; add it with POST /domains", fromDomain))
		return
	}
	if err != nil {
		s.internal(w, "domain lookup", err)
		return
	}
	if dom.Status != "verified" && !s.Cfg.AllowUnverified {
		writeError(w, http.StatusForbidden, "domain_not_verified",
			fmt.Sprintf("the domain %s is not verified yet; publish its DNS records and call POST /domains/%s/verify", dom.Name, dom.ID))
		return
	}
	der, err := s.Sealer.Open(dom.DKIMPrivate)
	if err != nil {
		s.internal(w, "unseal dkim key", err)
		return
	}
	key, err := dkimkey.ParsePrivate(der)
	if err != nil {
		s.internal(w, "parse dkim key", err)
		return
	}

	id := secret.UUID()
	raw, err := message.Build(message.Input{
		From: req.From, To: req.To, Cc: req.Cc, ReplyTo: req.ReplyTo,
		Subject: req.Subject, Text: req.Text, HTML: req.HTML, Headers: req.Headers,
		MessageID: id + "@" + dom.Name, Date: now,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	signed, err := dkimkey.Sign(raw, dom.Name, dom.Selector, key)
	if err != nil {
		s.internal(w, "dkim sign", err)
		return
	}
	hash := sha256.Sum256(body)
	gotID, existing, err := s.Store.InsertEmail(ctx, store.NewEmail{
		ID: id, ProjectID: pid, DomainID: dom.ID, From: req.From,
		To: req.To, Cc: req.Cc, Bcc: req.Bcc, Subject: req.Subject,
		MessageID: id + "@" + dom.Name, ReturnPath: dom.ReturnPath,
		IdempotencyKey: idem, RequestHash: hash[:], Raw: s.Sealer.Seal(signed),
		Tags: tags, RetryUncertain: retryUncertain, ExpiresAt: expires, Recipients: recipients,
	})
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency_conflict", "this Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		s.internal(w, "insert email", err)
		return
	}
	if existing {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": gotID})
}

// envelope validates and deduplicates every recipient address.
func envelope(lists ...[]string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, a := range l {
			addr, err := mail.ParseAddress(a)
			if err != nil {
				return nil, fmt.Errorf("recipient %q: %v", a, err)
			}
			e := strings.ToLower(addr.Address)
			if !isASCII(e) {
				return nil, fmt.Errorf("recipient %q: internationalized addresses (SMTPUTF8) are not supported yet", a)
			}
			if domainOf(e) == "" || strings.Count(e, "@") != 1 {
				return nil, fmt.Errorf("recipient %q has no domain", a)
			}
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	if len(out) > maxRecipients {
		return nil, fmt.Errorf("at most %d recipients per email (to, cc and bcc together)", maxRecipients)
	}
	return out, nil
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

type emailResponse struct {
	Object     string                `json:"object"`
	ID         string                `json:"id"`
	From       string                `json:"from"`
	To         []string              `json:"to"`
	Cc         []string              `json:"cc"`
	Bcc        []string              `json:"bcc"`
	Subject    string                `json:"subject"`
	MessageID  string                `json:"message_id"`
	CreatedAt  time.Time             `json:"created_at"`
	ExpiresAt  time.Time             `json:"expires_at"`
	LastEvent  string                `json:"last_event"`
	Tags       map[string]string     `json:"tags"`
	BodyPurged bool                  `json:"body_purged"`
	Recipients []store.RecipientView `json:"recipients"`
	Attempts   []store.AttemptView   `json:"attempts"`
	Events     []store.EventView     `json:"events"`
}

func (s *Server) getEmail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	v, err := s.Store.GetEmail(r.Context(), projectID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "email not found")
		return
	}
	if err != nil {
		s.internal(w, "get email", err)
		return
	}
	resp := emailResponse{
		Object: "email", ID: v.ID, From: v.From, To: v.To, Cc: v.Cc, Bcc: v.Bcc, Subject: v.Subject,
		MessageID: "<" + v.MessageID + ">", CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt, Tags: v.Tags,
		BodyPurged: v.BodyPurged, Recipients: v.Recipients, Attempts: v.Attempts, Events: v.Events,
	}
	if n := len(v.Events); n > 0 {
		resp.LastEvent = v.Events[n-1].Type
	}
	if resp.Attempts == nil {
		resp.Attempts = []store.AttemptView{}
	}
	writeJSON(w, http.StatusOK, resp)
}
