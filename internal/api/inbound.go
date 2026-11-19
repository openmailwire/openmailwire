package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/mailauth"
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
	domain   store.Domain // reports: the domain they are about
	report   string       // reports: "dmarc" or "tls"
}

// Report addresses: these local parts on a verified domain's return-path
// domain (bounce.<domain>) receive its aggregate reports. The return-path
// domain shares the domain's organizational domain, so DMARC reporters
// need no external-destination record (RFC 7489 section 7.1).
const (
	dmarcReportLocal = "dmarc"
	tlsReportLocal   = "tls-rpt"
)

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
	local, dom, _ := strings.Cut(rcpt, "@")
	if local == dmarcReportLocal || local == tlsReportLocal {
		d, err := s.Store.DomainByReturnPath(ctx, dom)
		if err == nil {
			kind := "dmarc"
			if local == tlsReportLocal {
				kind = "tls"
			}
			return inboundTarget{kind: protocol.InboundReport, domain: d, report: kind}, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return inboundTarget{}, err
		}
	}
	b, err := s.Store.InboxByAddress(ctx, rcpt)
	if err == nil {
		return inboundTarget{kind: protocol.InboundInbox, inbox: b}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return inboundTarget{}, err
	}
	if local == "postmaster" || local == "abuse" {
		ok, err := s.servesDomain(ctx, dom)
		if err != nil || ok {
			return inboundTarget{kind: protocol.InboundPostmaster}, err
		}
	}
	return inboundTarget{}, nil
}

// servesDomain reports whether the inbound nodes are the MX of dom: it
// holds inboxes, or it is a return-path domain, or the feedback-loop or
// postmaster address is on it. RFC 5321 section 4.5.1 requires
// postmaster@ there; abuse@ is RFC 2142's.
func (s *Server) servesDomain(ctx context.Context, dom string) (bool, error) {
	if dom == domainOf(s.Cfg.FBLAddress) || dom == domainOf(s.Cfg.PostmasterInbox) {
		return true, nil
	}
	if ok, err := s.Store.ReturnPathKnown(ctx, dom); err != nil || ok {
		return ok, err
	}
	return s.Store.InboxDomainKnown(ctx, dom)
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

// inboundSlots bounds, per server, the inbound messages processed at
// once: parsing, sender authentication (DNS) and storing a large message
// cost memory and time, and the inbound nodes may forward many in
// parallel. It maps *Server to its semaphore.
var inboundSlots sync.Map

func (s *Server) inboundSlot(ctx context.Context) (release func(), ok bool) {
	n := s.Cfg.InboundConcurrency
	if n <= 0 {
		n = 8
	}
	v, _ := inboundSlots.LoadOrStore(s, make(chan struct{}, n))
	ch := v.(chan struct{})
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

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
	body = nil
	if len(msg.Raw) == 0 || len(msg.Rcpts) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_error", "raw and rcpts are required")
		return
	}
	if int64(len(msg.Raw)) > s.inboundMaxSize()+64<<10 {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the message is larger than OMW_INBOUND_MAX_SIZE")
		return
	}
	ctx := r.Context()
	release, ok := s.inboundSlot(ctx)
	if !ok {
		// The node answers 451 and the sender retries.
		writeError(w, http.StatusServiceUnavailable, "busy", "the control plane is processing too many inbound messages; try again later")
		return
	}
	defer release()
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
	parse := func() *mailparse.Message {
		if parsed == nil {
			parsed = mailparse.Parse(msg.Raw)
		}
		return parsed
	}
	var auth *mailauth.Results
	authenticate := func() mailauth.Results {
		if auth == nil {
			a := s.authenticate(ctx, msg, parse())
			auth = &a
		}
		return *auth
	}
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
				res.Outcome, err = s.applyComplaint(ctx, &t.delivery, rep.ARF, msg.Raw)
			default:
				res.Outcome = "ignored" // an auto-reply or something else sent to the bounce address
			}
		case protocol.InboundComplaint:
			if rep := parseReport(); rep.Kind == mailparse.KindARF {
				res.Outcome, err = s.applyComplaint(ctx, nil, rep.ARF, msg.Raw)
			} else {
				res.Outcome = "ignored"
			}
		case protocol.InboundReport:
			res.Outcome, err = s.storeReport(ctx, t, msg.Raw)
		case protocol.InboundInbox:
			res.Outcome, res.ID, err = s.storeInbound(ctx, t.inbox, msg, parse(), authenticate)
		case protocol.InboundPostmaster:
			res.Outcome, res.ID, err = s.postmaster(ctx, rcpt, msg, parse, authenticate)
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

// authenticate evaluates SPF, DKIM and DMARC for a received message.
func (s *Server) authenticate(ctx context.Context, msg protocol.InboundMessage, p *mailparse.Message) mailauth.Results {
	return mailauth.Evaluate(ctx, s.authResolver(), mailauth.Input{
		IP: net.ParseIP(msg.RemoteIP), HELO: msg.HELO, MailFrom: msg.MailFrom, Raw: msg.Raw, From: p.FromAddr, FromCount: p.FromCount,
	}, 10*time.Second, "openmailwire")
}

// authResolver is the DNS used to authenticate senders.
func (s *Server) authResolver() mailauth.Resolver {
	if s.Resolver == nil {
		return mailauth.Complete(noDNS{})
	}
	return mailauth.Complete(s.Resolver)
}

// noDNS finds nothing; it stands in when the server has no resolver.
type noDNS struct{}

func (noDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	return nil, &net.DNSError{Err: "no resolver", Name: name, IsNotFound: true}
}

func (noDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, &net.DNSError{Err: "no resolver", Name: name, IsNotFound: true}
}

// postmaster handles mail for postmaster@ and abuse@: it goes to the
// operator's inbox when OMW_POSTMASTER_INBOX names one, and is otherwise
// logged (sender domain, size, never content) and dropped.
func (s *Server) postmaster(ctx context.Context, rcpt string, msg protocol.InboundMessage, parse func() *mailparse.Message,
	authenticate func() mailauth.Results) (outcome, id string, err error) {
	if s.Cfg.PostmasterInbox != "" {
		ib, err := s.Store.InboxByAddress(ctx, s.Cfg.PostmasterInbox)
		if err == nil {
			return s.storeInbound(ctx, ib, msg, parse(), authenticate)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", "", err
		}
		s.Log.Warn("OMW_POSTMASTER_INBOX is not an inbox; postmaster mail is only logged")
	}
	s.Log.Warn("postmaster mail received", "rcpt", strings.SplitN(rcpt, "@", 2)[0]+"@"+domainOf(rcpt),
		"sender_domain", domainOf(msg.MailFrom), "remote_ip", msg.RemoteIP, "size", len(msg.Raw))
	return "logged", "", nil
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

// applyComplaint records an ARF complaint.
//
// Anyone can send a report, so what it may change depends on how it
// identifies the email:
//   - through a VERP address (the one it was sent to, its
//     Original-Mail-From, or the Return-Path of the original it quotes):
//     the address carries an HMAC, so it names exactly one delivery, which
//     is recorded and its recipient suppressed;
//   - through the original Message-ID only: the report counts only when a
//     feedback loop in OMW_FBL_TRUSTED_DOMAINS signed it (DKIM). It is
//     looked up only among emails on the return path or sending domain the
//     report names, records at most one complaint per email, names no
//     recipient and suppresses nobody, because the recipient fields of a
//     report cannot be checked.
func (s *Server) applyComplaint(ctx context.Context, via *store.SentDelivery, arf *mailparse.ARF, raw []byte) (string, error) {
	if !arf.IsComplaint() {
		return "ignored", nil
	}
	target, how := via, "verp_recipient"
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
			target, how = &d, "verp"
		}
	}
	data := map[string]any{
		"source": "arf", "feedback_type": orAbuse(arf.FeedbackType), "user_agent": truncateRunes(arf.UserAgent, 200),
		"source_ip": truncateRunes(arf.SourceIP, 64), "arrival_date": truncateRunes(arf.ArrivalDate, 100), "identified_by": how,
	}
	if target != nil {
		return s.recordComplaint(ctx, store.Complaint{EmailID: target.EmailID, DeliveryID: target.DeliveryID,
			ProjectID: target.ProjectID, Rcpt: target.Rcpt, Data: store.EventData(data)})
	}

	if arf.Original == nil || arf.Original.MessageID == "" {
		return "ignored", nil
	}
	signer := mailauth.SignedBy(mailauth.VerifyDKIM(ctx, s.authResolver(), raw, 10*time.Second), s.Cfg.FBLTrustedDomains)
	if signer == "" {
		s.Log.Info("complaint report ignored: it names the email only by Message-ID and no trusted feedback loop signed it")
		return "ignored", nil
	}
	var returnPaths, fromDomains []string
	for _, a := range []string{arf.OriginalMailFrom, arf.Original.ReturnPath} {
		if d := domainOf(a); d != "" {
			returnPaths = append(returnPaths, d)
		}
	}
	// The quoted original's From domain; Reported-Domain only when the
	// original has no From.
	if d := domainOf(arf.Original.From); d != "" {
		fromDomains = append(fromDomains, d)
	} else if d := strings.TrimSpace(arf.ReportedDomain); d != "" {
		fromDomains = append(fromDomains, d)
	}
	ds, err := s.Store.DeliveriesByMessageID(ctx, arf.Original.MessageID, returnPaths, fromDomains)
	if err != nil {
		return "", err
	}
	emails := map[string]store.SentDelivery{}
	for _, d := range ds {
		emails[d.EmailID] = d
	}
	if len(emails) != 1 {
		return "ignored", nil // unknown, or several emails share the Message-ID: nothing safe to record
	}
	data["identified_by"], data["reporter"] = "message_id", signer
	return s.recordComplaint(ctx, store.Complaint{EmailID: ds[0].EmailID, ProjectID: ds[0].ProjectID, Data: store.EventData(data)})
}

func (s *Server) recordComplaint(ctx context.Context, c store.Complaint) (string, error) {
	changed, err := s.Store.RecordComplaint(ctx, c)
	if err != nil {
		return "", err
	}
	if !changed {
		return "duplicate", nil
	}
	return "complained", nil
}

func orAbuse(feedback string) string {
	if feedback == "" {
		return "abuse"
	}
	return feedback
}

// storeInbound seals and stores a message for an inbox, with its sender
// authentication. It is held (hidden from sending_access keys) when the
// inbox holds unauthenticated mail and the sender is not authenticated.
func (s *Server) storeInbound(ctx context.Context, inbox store.Inbox, msg protocol.InboundMessage, p *mailparse.Message,
	authenticate func() mailauth.Results) (outcome, id string, err error) {
	if max := s.Cfg.InboxMaxPerHour; max > 0 {
		n, err := s.Store.RecentInboundCount(ctx, inbox.ID, time.Hour, max)
		if err != nil {
			return "", "", err
		}
		if n >= max {
			s.Log.Warn("inbox over its hourly limit; message deferred", "inbox_domain", domainOf(inbox.Address), "limit", max)
			return "deferred", "", nil
		}
	}
	auth := authenticate() // after the rate check: it costs DNS lookups
	from := p.From
	if from == "" {
		from = msg.MailFrom
	}
	held := inbox.HoldUnauthenticated && auth.Trust != mailauth.Authenticated
	rules := mailparse.SuspiciousInstructions(p.SuspiciousText()...)
	authJSON, _ := json.Marshal(auth)
	var snippet []byte
	if sn := p.Snippet(200); sn != "" {
		snippet = s.Sealer.Seal([]byte(sn))
	}
	m, existing, err := s.Store.InsertInboundMessage(ctx, secret.UUID(), store.NewInboundMessage{
		InboxID: inbox.ID, ProjectID: inbox.ProjectID, MailFrom: msg.MailFrom, From: from,
		To: p.To, Cc: p.Cc, ReplyTo: p.ReplyTo, Subject: p.Subject,
		MessageID: p.MessageID, InReplyTo: p.InReplyTo, References: p.References, Date: p.Date,
		Snippet: snippet, Size: len(msg.Raw), RemoteIP: msg.RemoteIP, HELO: msg.HELO, TLS: msg.TLS,
		Raw: s.Sealer.Seal(msg.Raw), Trust: auth.Trust, AuthResults: authJSON, Held: held, SuspiciousRules: rules,
		ContentHash: contentHash(msg.MailFrom, auth.Trust, p),
		EventData: store.EventData(map[string]any{
			"trust": auth.Trust, "held": held, "suspicious_instructions": len(rules) > 0,
			"authentication": authSummary(auth),
		}),
	})
	if err != nil {
		return "", "", err
	}
	if existing {
		return "duplicate", m.ID, nil
	}
	return "stored", m.ID, nil
}

// authSummary is the short form of the results webhooks and lists show.
func authSummary(a mailauth.Results) map[string]string {
	dkim := mailauth.None
	for _, d := range a.DKIM {
		if d.Result == mailauth.Pass && d.Aligned {
			dkim = mailauth.Pass
			break
		}
		if dkim == mailauth.None || d.Result == mailauth.Pass {
			dkim = d.Result
		}
	}
	return map[string]string{"spf": a.SPF.Result, "dkim": dkim, "dmarc": a.DMARC.Result}
}

// contentHash identifies a message's content for deduplication: the
// envelope sender, the sender verdict, the headers a reader sees and the
// body with its attachments. Trace headers (Received, which differ on
// every retry) are not part of it, so a retried transaction is still a
// duplicate, while a different message that reuses a Message-ID is not.
func contentHash(mailFrom, trust string, p *mailparse.Message) string {
	h := sha256.New()
	field := func(h hash.Hash, b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, v := range []string{strings.ToLower(mailFrom), trust, p.From, strings.Join(p.To, ","), strings.Join(p.Cc, ","),
		strings.Join(p.ReplyTo, ","), p.Subject, p.Date, p.MessageID, p.InReplyTo, strings.Join(p.References, " "), p.Text, p.HTML} {
		field(h, []byte(v))
	}
	for _, a := range p.Attachments {
		field(h, []byte(a.Filename+"\x00"+a.ContentType+"\x00"+a.ContentID))
		field(h, a.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// storeReport parses and stores a DMARC or TLS-RPT aggregate report for
// the domain its address belongs to. Anything unreadable, or about another
// domain, is ignored (and still acknowledged, so the reporter does not
// retry it).
func (s *Server) storeReport(ctx context.Context, t inboundTarget, raw []byte) (string, error) {
	d := t.domain
	about := func(name string) bool {
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		return name == d.Name || strings.HasSuffix(name, "."+d.Name)
	}
	r := store.NewDomainReport{ProjectID: d.ProjectID, DomainID: d.ID, Kind: t.report}
	switch t.report {
	case "dmarc":
		rep, err := mailparse.ParseDMARCReport(raw)
		if err != nil {
			s.Log.Info("DMARC report ignored", "domain", d.Name, "err", err)
			return "ignored", nil
		}
		if !about(rep.Policy.Domain) {
			s.Log.Info("DMARC report ignored: it is about another domain", "domain", d.Name, "report_domain", rep.Policy.Domain)
			return "ignored", nil
		}
		r.OrgName, r.ReportID, r.Begin, r.End, r.Truncated = rep.OrgName, rep.ReportID, rep.Begin, rep.End, rep.Truncated
		r.Policy, _ = json.Marshal(rep.Policy)
		for _, x := range rep.Records {
			auth, _ := json.Marshal(map[string]any{"dkim": x.DKIMResults, "spf": x.SPFResults, "reasons": x.Reasons})
			r.DMARC = append(r.DMARC, store.DMARCRow{SourceIP: x.SourceIP, Count: x.Count, Disposition: x.Disposition,
				DKIM: x.DKIM, SPF: x.SPF, HeaderFrom: x.HeaderFrom, EnvelopeFrom: x.EnvelopeFrom, Auth: auth})
			r.Messages += x.Count
			if x.DKIM != "pass" && x.SPF != "pass" {
				r.Failed += x.Count
			}
		}
	case "tls":
		rep, err := mailparse.ParseTLSReport(raw)
		if err != nil {
			s.Log.Info("TLS report ignored", "domain", d.Name, "err", err)
			return "ignored", nil
		}
		r.OrgName, r.ReportID, r.Begin, r.End, r.Truncated = rep.OrgName, rep.ReportID, rep.Begin, rep.End, rep.Truncated
		for _, p := range rep.Policies {
			if !about(p.PolicyDomain) {
				continue
			}
			if len(p.Failures) > 100 {
				p.Failures = p.Failures[:100]
			}
			failures, _ := json.Marshal(p.Failures)
			r.TLS = append(r.TLS, store.TLSRow{PolicyType: p.PolicyType, PolicyDomain: p.PolicyDomain, MXHosts: p.MXHosts,
				Successful: p.Successful, Failed: p.Failed, Failures: failures})
			r.Messages += p.Successful + p.Failed
			r.Failed += p.Failed
		}
		if len(r.TLS) == 0 {
			s.Log.Info("TLS report ignored: no policy for this domain", "domain", d.Name)
			return "ignored", nil
		}
	}
	_, dup, err := s.Store.InsertDomainReport(ctx, r)
	if err != nil {
		return "", err
	}
	if dup {
		return "duplicate", nil
	}
	return "stored", nil
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
