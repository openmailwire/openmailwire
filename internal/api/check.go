package api

// POST /emails/check (lane B): a pre-send lint. It takes the body of
// POST /emails, sends and stores nothing, and reports what a send would do
// (send, hold or refuse), every refusal that would apply, and warnings
// about deliverability.

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openmailwire/openmailwire/internal/message"
	"github.com/openmailwire/openmailwire/internal/store"
)

const (
	gmailClipBytes  = 102 * 1024 // Gmail clips a message whose HTML is larger
	largeMessage    = 10 << 20   // many receivers refuse larger messages
	dkimHeaderBytes = 500        // about what the DKIM-Signature adds
)

type checkWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type checkRefusal struct {
	StatusCode int    `json:"statusCode"`
	Name       string `json:"name"`
	Message    string `json:"message"`
}

type checkResult struct {
	Object      string         `json:"object"`
	OK          bool           `json:"ok"`    // no refusal and no warning
	Would       string         `json:"would"` // send, hold or refuse
	Stream      string         `json:"stream"`
	Size        int            `json:"size"` // bytes as sent, DKIM signature included (estimated)
	HoldReasons []string       `json:"hold_reasons"`
	Refusals    []checkRefusal `json:"refusals"`
	Warnings    []checkWarning `json:"warnings"`
}

func (s *Server) checkEmail(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req sendRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.check(r, req))
}

func (s *Server) check(r *http.Request, req sendRequest) (res checkResult) {
	ctx, key := r.Context(), apiKeyOf(r)
	res = checkResult{Object: "email_check", HoldReasons: []string{}, Refusals: []checkRefusal{}, Warnings: []checkWarning{}}
	refuse := func(pe *policyError) {
		res.Refusals = append(res.Refusals, checkRefusal{StatusCode: pe.Status, Name: pe.Name, Message: pe.Message})
	}
	warn := func(code, format string, args ...any) {
		res.Warnings = append(res.Warnings, checkWarning{Code: code, Message: fmt.Sprintf(format, args...)})
	}
	defer func() {
		res.OK = len(res.Refusals) == 0 && len(res.Warnings) == 0
		switch {
		case len(res.Refusals) > 0:
			res.Would = "refuse"
		case len(res.HoldReasons) > 0:
			res.Would = "hold"
		default:
			res.Would = "send"
		}
	}()
	res.Stream, _ = parseStream(req.Stream)

	plan, pe := validateSend(req, time.Now().UTC(), s.Cfg.DefaultExpiry)
	if pe != nil {
		refuse(pe)
		return res
	}
	if pe := s.planSafety(r, plan); pe != nil {
		refuse(pe)
	} else if h := plan.hold(); h != nil {
		res.HoldReasons = h.Reasons
	}
	if pe := allowedBy(key.Policy, plan.from, plan.recipients); pe != nil {
		refuse(pe)
	}
	if pe := s.checkCaps(ctx, key, plan.recipients); pe != nil {
		refuse(pe)
	}
	var domainIDs []string
	dom, dpe := s.senderDomain(ctx, key.ProjectID, plan.fromDomain)
	if dpe != nil {
		refuse(dpe)
		if dpe.Name == "domain_not_found" || dpe.Name == "domain_not_verified" {
			warn("domain_not_verified", "%s", dpe.Message)
		}
	} else {
		domainIDs = []string{dom.ID}
	}
	if pe := s.checkPause(ctx, key, domainIDs, []string{plan.fromDomain}); pe != nil {
		refuse(pe)
	}
	if limit, err := s.Store.MaxPerRecipientPerDay(ctx, key.ProjectID); err != nil {
		refuse(s.internalErr("project settings", err))
	} else if limit > 0 {
		ce, err := s.Store.ReserveRecipients(ctx, key.ProjectID, limit,
			[]store.Reservation{{Stream: plan.stream(), Recipients: plan.recipients}}, false)
		switch {
		case err != nil:
			refuse(s.internalErr("check recipients", err))
		case ce != nil:
			refuse(recipientRefusal(ce))
		}
	}

	raw, err := message.Build(message.Input{
		From: req.From, To: req.To, Cc: req.Cc, ReplyTo: req.ReplyTo, Subject: req.Subject, Text: req.Text, HTML: req.HTML,
		Headers: plan.messageHeaders(), MessageID: "check@" + plan.fromDomain, Date: time.Now(), Attachments: plan.attachments,
	})
	if err != nil {
		refuse(invalid("%v", err))
		return res
	}
	res.Size = len(raw) + dkimHeaderBytes

	if !bytes.Contains(raw, []byte("Content-Type: text/plain")) {
		warn("no_text_part", "the email has HTML but no plain-text part; spam filters score HTML-only mail worse and some readers show nothing. Pass text as well")
	}
	if n := len(req.HTML); n > gmailClipBytes {
		warn("html_too_large", "the HTML is %d KB; Gmail clips HTML over 102 KB and hides the rest (often the unsubscribe link) behind \"View entire message\". Shorten it or link to a web page", (n+1023)/1024)
	}
	hosts, _ := linkHosts(req.Text, req.HTML)
	if foreign := foreignHosts(hosts, plan.fromDomain); len(foreign) > 0 {
		if len(foreign) > 10 {
			foreign = append(foreign[:10], fmt.Sprintf("and %d more", len(foreign)-10))
		}
		warn("link_domain_mismatch", "links point to %s, not to the sender's domain %s; links to other domains make mail look like phishing to spam filters. Link to your own domain where you can",
			strings.Join(foreign, ", "), siteOf(plan.fromDomain))
	}
	if res.Stream == store.StreamBroadcast {
		switch {
		case s.Cfg.PublicURL == "":
			warn("missing_unsubscribe", "broadcast email needs a one-click unsubscribe link, and OMW_PUBLIC_URL is not set on this server, so it cannot be sent")
		case !mentionsUnsubscribe(req.Text) && !mentionsUnsubscribe(req.HTML):
			warn("missing_unsubscribe", "the List-Unsubscribe header is added for you, but the body has no visible unsubscribe link; bulk-mail rules (CAN-SPAM, Gmail and Yahoo sender requirements) expect one in the message too")
		}
	}
	if res.Size > largeMessage {
		warn("large_message", "the message is %.1f MB; many mail servers refuse messages over 10 MB. Link to large files instead of attaching them", float64(res.Size)/(1<<20))
	}
	return res
}

func mentionsUnsubscribe(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "unsubscribe") || strings.Contains(l, "opt out") || strings.Contains(l, "opt-out")
}
