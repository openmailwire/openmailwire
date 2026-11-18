package api

import (
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/mailparse"
	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

// The internal API for inbound nodes (openmailwire inbound). They
// authenticate with worker tokens, like delivery workers.

var verpRE = regexp.MustCompile(`^b-([0-9a-f]{32})-([0-9a-f]{10})@([^@]+)$`)

// verpDelivery checks a bounce address made by bounceAddress and returns the
// delivery it names. A forged or mistyped address, or one on another return
// path, gives ok = false.
func (s *Server) verpDelivery(ctx context.Context, addr string) (d store.SentDelivery, ok bool, err error) {
	m := verpRE.FindStringSubmatch(strings.ToLower(addr))
	if m == nil || !hmac.Equal([]byte(secret.MAC(s.MACKey, m[1])), []byte(m[2])) {
		return d, false, nil
	}
	c := m[1]
	id := c[0:8] + "-" + c[8:12] + "-" + c[12:16] + "-" + c[16:20] + "-" + c[20:32]
	d, err = s.Store.SentDelivery(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	return d, strings.EqualFold(d.ReturnPath, m[3]), nil
}

// inboundTarget is what one recipient address of inbound mail is for.
type inboundTarget struct {
	kind     string // protocol.Inbound…, or "" for an unknown address
	delivery store.SentDelivery
	inbox    store.Inbox
}

func (s *Server) classify(ctx context.Context, rcpt string) (inboundTarget, error) {
	rcpt = strings.ToLower(strings.TrimSpace(rcpt))
	if rcpt == "" || strings.Count(rcpt, "@") != 1 {
		return inboundTarget{}, nil
	}
	if s.Cfg.FBLAddress != "" && rcpt == s.Cfg.FBLAddress {
		return inboundTarget{kind: protocol.InboundComplaint}, nil
	}
	if strings.HasPrefix(rcpt, "b-") {
		d, ok, err := s.verpDelivery(ctx, rcpt)
		if err != nil || ok {
			return inboundTarget{kind: protocol.InboundBounce, delivery: d}, err
		}
	}
	b, err := s.Store.InboxByAddress(ctx, rcpt)
	if errors.Is(err, store.ErrNotFound) {
		return inboundTarget{}, nil
	}
	if err != nil {
		return inboundTarget{}, err
	}
	return inboundTarget{kind: protocol.InboundInbox, inbox: b}, nil
}

func (s *Server) inboundCheck(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req protocol.InboundCheckRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	t, err := s.classify(r.Context(), req.Rcpt)
	if err != nil {
		s.internal(w, "inbound check", err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.InboundCheckResponse{Accept: t.kind != "", Kind: t.kind})
}

func (s *Server) inboundMaxSize() int64 {
	if s.Cfg.InboundMaxSize > 0 {
		return s.Cfg.InboundMaxSize
	}
	return config.DefaultInboundMaxSize
}

// inboundBodyLimit caps POST /internal/inbound: the largest message, base64
// encoded, plus room for the envelope.
func (s *Server) inboundBodyLimit() int64 { return s.inboundMaxSize()/3*4 + 1<<20 }

func (s *Server) inboundReceive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the message is larger than OMW_INBOUND_MAX_SIZE")
		return
	}
	var msg protocol.InboundMessage
	if err := decodeStrict(body, &msg); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	if len(msg.Raw) == 0 || len(msg.Rcpts) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "raw and rcpts are required")
		return
	}
	if int64(len(msg.Raw)) > s.inboundMaxSize()+64<<10 {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the message is larger than OMW_INBOUND_MAX_SIZE")
		return
	}
	ctx := r.Context()
	resp := protocol.InboundResponse{Results: []protocol.InboundResult{}}
	var report *mailparse.Report
	parseReport := func() mailparse.Report {
		if report == nil {
			rep := mailparse.ParseReport(msg.Raw)
			report = &rep
		}
		return *report
	}
	var parsed *mailparse.Message
	seen := map[string]bool{}
	for _, rcpt := range msg.Rcpts {
		rcpt = strings.ToLower(strings.TrimSpace(rcpt))
		if seen[rcpt] || len(seen) >= 100 {
			continue
		}
		seen[rcpt] = true
		t, err := s.classify(ctx, rcpt)
		if err != nil {
			s.internal(w, "inbound classify", err)
			return
		}
		res := protocol.InboundResult{Rcpt: rcpt, Kind: t.kind}
		switch t.kind {
		case "":
			res.Outcome = "rejected"
		case protocol.InboundBounce:
			rep := parseReport()
			switch rep.Kind {
			case mailparse.KindDSN:
				res.Outcome, err = s.applyDSN(ctx, t.delivery, rep.DSN)
			case mailparse.KindARF:
				res.Outcome, err = s.applyComplaint(ctx, &t.delivery, rep.ARF)
			default:
				res.Outcome = "ignored" // an auto-reply or something else sent to the bounce address
			}
		case protocol.InboundComplaint:
			if rep := parseReport(); rep.Kind == mailparse.KindARF {
				res.Outcome, err = s.applyComplaint(ctx, nil, rep.ARF)
			} else {
				res.Outcome = "ignored"
			}
		case protocol.InboundInbox:
			if parsed == nil {
				parsed = mailparse.Parse(msg.Raw)
			}
			res.Outcome, res.ID, err = s.storeInbound(ctx, t.inbox, msg, parsed)
		}
		if err != nil {
			s.internal(w, "inbound "+t.kind, err)
			return
		}
		s.Log.Info("inbound message", "kind", t.kind, "outcome", res.Outcome, "rcpt_domain", domainOf(rcpt),
			"remote_ip", msg.RemoteIP, "size", len(msg.Raw))
		resp.Results = append(resp.Results, res)
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyDSN records a failure reported by a delivery status notification for
// the delivery its VERP address names.
func (s *Server) applyDSN(ctx context.Context, d store.SentDelivery, dsn *mailparse.DSN) (string, error) {
	failed := dsn.Failed()
	if len(failed) == 0 {
		return "ignored", nil // delayed, delivered, relayed or expanded: nothing to change
	}
	rec := failed[0]
	for _, f := range failed {
		if f.FinalRecipient == d.Rcpt || f.OriginalRecipient == d.Rcpt {
			rec = f
			break
		}
	}
	status := rec.Status
	if status == "" {
		status = "5.0.0"
	}
	format := "rfc3464"
	if dsn.PlainText {
		format = "plain-text"
	}
	diag := rec.Diagnostic
	if diag == "" {
		diag = "the receiving system reported " + status + " after accepting the message"
	}
	changed, err := s.Store.RecordDSNBounce(ctx, store.DSNBounce{
		DeliveryID: d.DeliveryID, SMTPCode: rec.SMTPCode, Status: status, Diagnostic: diag,
		Suppress: policy.IsRecipientFailure(status),
		Data: store.EventData(map[string]any{
			"source": "dsn", "status": status, "diagnostic": truncateRunes(diag, 500), "format": format,
			"reporting_mta": dsn.ReportingMTA, "previous_status": d.Status,
			// The fields webhooks read for any bounce.
			"smtp_code": rec.SMTPCode, "enhanced_code": status, "response": truncateRunes(diag, 300), "mx_host": rec.RemoteMTA,
		}),
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return "duplicate", nil
	}
	return "bounced", nil
}

// applyComplaint records an ARF complaint. via is the delivery named by the
// VERP address the report was sent to, if any; otherwise the report itself
// must identify the email: a VERP envelope sender in it, or the original
// Message-ID.
func (s *Server) applyComplaint(ctx context.Context, via *store.SentDelivery, arf *mailparse.ARF) (string, error) {
	if !arf.IsComplaint() {
		return "ignored", nil
	}
	var target *store.SentDelivery
	if via != nil {
		target = via
	}
	candidates := []string{arf.OriginalMailFrom}
	if arf.Original != nil {
		candidates = append(candidates, arf.Original.ReturnPath)
	}
	for _, c := range candidates {
		if target != nil || c == "" {
			continue
		}
		d, ok, err := s.verpDelivery(ctx, c)
		if err != nil {
			return "", err
		}
		if ok {
			target = &d
		}
	}
	var emailID, projectID string
	if target == nil && arf.Original != nil && arf.Original.MessageID != "" {
		ds, err := s.Store.DeliveriesByMessageID(ctx, arf.Original.MessageID)
		if err != nil {
			return "", err
		}
		rcpts := map[string]bool{}
		for _, r := range arf.OriginalRcptTo {
			rcpts[r] = true
		}
		for _, r := range arf.Original.To {
			rcpts[r] = true
		}
		for i := range ds {
			if rcpts[ds[i].Rcpt] || len(ds) == 1 {
				target = &ds[i]
				break
			}
		}
		if target == nil && len(ds) > 0 {
			// The email is known but the report hides which recipient
			// complained: record it on the email without suppressing anyone.
			emailID, projectID = ds[0].EmailID, ds[0].ProjectID
		}
	}
	c := store.Complaint{EmailID: emailID, ProjectID: projectID}
	if target != nil {
		c = store.Complaint{EmailID: target.EmailID, DeliveryID: target.DeliveryID, ProjectID: target.ProjectID, Rcpt: target.Rcpt}
	}
	if c.EmailID == "" {
		return "ignored", nil
	}
	feedback := arf.FeedbackType
	if feedback == "" {
		feedback = "abuse"
	}
	c.Data = store.EventData(map[string]any{
		"source": "arf", "feedback_type": feedback, "user_agent": truncateRunes(arf.UserAgent, 200),
		"source_ip": arf.SourceIP, "arrival_date": truncateRunes(arf.ArrivalDate, 100),
	})
	changed, err := s.Store.RecordComplaint(ctx, c)
	if err != nil {
		return "", err
	}
	if !changed {
		return "duplicate", nil
	}
	return "complained", nil
}

// storeInbound seals and stores a message for an inbox.
func (s *Server) storeInbound(ctx context.Context, inbox store.Inbox, msg protocol.InboundMessage, p *mailparse.Message) (outcome, id string, err error) {
	from := p.From
	if from == "" {
		from = msg.MailFrom
	}
	m, existing, err := s.Store.InsertInboundMessage(ctx, secret.UUID(), store.NewInboundMessage{
		InboxID: inbox.ID, ProjectID: inbox.ProjectID, MailFrom: msg.MailFrom, From: from,
		To: p.To, Cc: p.Cc, ReplyTo: p.ReplyTo, Subject: p.Subject,
		MessageID: p.MessageID, InReplyTo: p.InReplyTo, References: p.References, Date: p.Date,
		Snippet: p.Snippet(200), Size: len(msg.Raw), RemoteIP: msg.RemoteIP, HELO: msg.HELO, TLS: msg.TLS,
		Raw: s.Sealer.Seal(msg.Raw),
	})
	if err != nil {
		return "", "", err
	}
	if existing {
		return "duplicate", m.ID, nil
	}
	return "stored", m.ID, nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
