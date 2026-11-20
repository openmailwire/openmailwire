package api

// GET /overview: the state of a project at a glance, for the dashboard's
// home page. Read-only and cheap: every count uses an index (store
// overview.go), and the sending numbers read at most the newest
// store.InsightsMaxEmails emails of the last 7 days.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/openmailwire/openmailwire/internal/dkimkey"
	"github.com/openmailwire/openmailwire/internal/policy"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/store"
)

// workerOnline is how recently a worker must have been seen to count as
// online: delivery workers send a heartbeat every 20 seconds, so this is
// six missed heartbeats.
const workerOnline = 2 * time.Minute

type queueView struct {
	Queued           int        `json:"queued"`
	Scheduled        int        `json:"scheduled"`
	Sending          int        `json:"sending"`
	Deferred         int        `json:"deferred"`
	Held             int        `json:"held"`
	OldestQueuedAt   *time.Time `json:"oldest_queued_at"`
	OldestAgeSeconds *float64   `json:"oldest_age_seconds"`
}

type sendingWindow struct {
	Window        string   `json:"window"`
	Emails        int      `json:"emails"`
	Recipients    int      `json:"recipients"`
	Delivered     int      `json:"delivered"`
	Bounced       int      `json:"bounced"`
	Complained    int      `json:"complained"`
	Suppressed    int      `json:"suppressed"`
	DeliveryRate  *float64 `json:"delivery_rate"`
	BounceRate    *float64 `json:"bounce_rate"`
	ComplaintRate *float64 `json:"complaint_rate"`
}

func (w *sendingWindow) add(d store.SendingDay) {
	w.Emails += d.Emails
	w.Recipients += d.Recipients
	w.Delivered += d.Delivered
	w.Bounced += d.Bounced
	w.Complained += d.Complained
	w.Suppressed += d.Suppressed
}

// rates divides by every recipient of the window's emails.
func (w *sendingWindow) rates() {
	if w.Recipients == 0 {
		return
	}
	f := func(n int) *float64 {
		v := math.Round(float64(n)/float64(w.Recipients)*10000) / 10000
		return &v
	}
	w.DeliveryRate, w.BounceRate, w.ComplaintRate = f(w.Delivered), f(w.Bounced), f(w.Complained)
}

type sendingDayView struct {
	Day        string `json:"day"`
	Recipients int    `json:"recipients"`
	Delivered  int    `json:"delivered"`
	Bounced    int    `json:"bounced"`
	Complained int    `json:"complained"`
}

type domainHealth struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	ClaimExpiresAt *time.Time `json:"claim_expires_at"`
	DMARC          string     `json:"dmarc"`
	Inbound        *string    `json:"inbound"`
	Inboxes        int        `json:"inboxes"`
	DKIMRotation   string     `json:"dkim_rotation"`
	Ed25519        string     `json:"ed25519"`
	AuthIssues     int        `json:"auth_issues"`
	Problems       []string   `json:"problems"`
}

type selfCheckView struct {
	OK        bool       `json:"ok"`
	Skipped   string     `json:"skipped,omitempty"`
	Problems  []string   `json:"problems"`
	CheckedAt *time.Time `json:"checked_at"`
}

type providerCapView struct {
	Provider string `json:"provider"`
	Cap      int    `json:"cap"`
	Used     int    `json:"used"`
}

type warmupView struct {
	Enabled   bool              `json:"enabled"`
	Started   bool              `json:"started"`
	Warm      bool              `json:"warm"`
	Day       int               `json:"day"`
	Remaining *int              `json:"remaining"`
	Cap       *int              `json:"cap"`
	Providers []providerCapView `json:"providers"`
}

type workerView struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Role       string         `json:"role"`
	IP         string         `json:"ip"`
	Version    string         `json:"version"`
	Draining   bool           `json:"draining"`
	LastSeenAt *time.Time     `json:"last_seen_at"`
	Online     bool           `json:"online"`
	SelfCheck  *selfCheckView `json:"self_check"`
	Warmup     *warmupView    `json:"warmup"`
}

type receivingView struct {
	Enabled      bool    `json:"enabled"`
	InboundMX    *string `json:"inbound_mx"`
	Inboxes      int     `json:"inboxes"`
	Messages24h  int     `json:"messages_24h"`
	HeldMessages int     `json:"held_messages"`
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid := projectID(r)
	now := time.Now().UTC()

	q, err := s.Store.OverviewQueue(ctx, pid)
	if err != nil {
		s.internal(w, "overview queue", err)
		return
	}
	queue := queueView{Queued: q.Queued, Scheduled: q.Scheduled, Sending: q.Sending, Deferred: q.Deferred, Held: q.Held,
		OldestQueuedAt: q.OldestQueuedAt}
	if q.OldestQueuedAt != nil {
		age := math.Max(0, math.Round(now.Sub(*q.OldestQueuedAt).Seconds()))
		queue.OldestAgeSeconds = &age
	}

	days, truncated, err := s.Store.OverviewSending(ctx, pid, store.InsightsMaxEmails)
	if err != nil {
		s.internal(w, "overview sending", err)
		return
	}
	w24, w7 := sendingWindow{Window: "24h"}, sendingWindow{Window: "7d"}
	daily := make([]sendingDayView, 7)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for i := range daily {
		daily[i].Day = today.AddDate(0, 0, i-6).Format("2006-01-02")
	}
	for _, d := range days {
		w7.add(d)
		if d.Last24h {
			w24.add(d)
		}
		if i := 6 - int(today.Sub(d.Day).Hours()/24); i >= 0 && i < 7 {
			daily[i].Recipients += d.Recipients
			daily[i].Delivered += d.Delivered
			daily[i].Bounced += d.Bounced
			daily[i].Complained += d.Complained
		}
	}
	w24.rates()
	w7.rates()

	pauses, err := s.Store.ActivePauses(ctx, pid)
	if err != nil {
		s.internal(w, "overview pauses", err)
		return
	}
	health, ok := s.domainHealth(w, r)
	if !ok {
		return
	}
	rc, err := s.Store.OverviewReceiving(ctx, pid)
	if err != nil {
		s.internal(w, "overview receiving", err)
		return
	}
	recv := receivingView{Enabled: s.Cfg.InboundMX != "", Inboxes: rc.Inboxes, Messages24h: rc.Messages24h, HeldMessages: rc.HeldMessages}
	if s.Cfg.InboundMX != "" {
		mx := s.Cfg.InboundMX
		recv.InboundMX = &mx
	}
	workers, err := s.workersView(r, now)
	if err == nil {
		var operator bool
		if operator, err = s.isOperatorProject(r); err == nil && !operator {
			redactWorkers(workers)
		}
	}
	if err != nil {
		s.internal(w, "overview workers", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":       "overview",
		"generated_at": now,
		"queue":        queue,
		"sending": map[string]any{
			"truncated": truncated,
			"windows":   []sendingWindow{w24, w7},
			"daily":     daily,
		},
		"held_emails":   q.HeldEmails,
		"active_pauses": pauses,
		"domains":       health,
		"receiving":     recv,
		"workers":       workers,
	})
}

// domainHealth lists the project's domains with what needs attention.
func (s *Server) domainHealth(w http.ResponseWriter, r *http.Request) ([]domainHealth, bool) {
	ctx := r.Context()
	pid := projectID(r)
	doms, err := s.Store.ListDomains(ctx, pid)
	if err != nil {
		s.internal(w, "overview domains", err)
		return nil, false
	}
	issues, err := s.Store.AuthIssueCounts(ctx, pid)
	if err != nil {
		s.internal(w, "overview auth issues", err)
		return nil, false
	}
	boxes, err := s.Store.ListInboxes(ctx, pid)
	if err != nil {
		s.internal(w, "overview inboxes", err)
		return nil, false
	}
	inboxes := map[string]int{}
	for _, b := range boxes {
		inboxes[b.DomainID]++
	}
	out := []domainHealth{}
	for _, d := range doms {
		v := s.view(ctx, d)
		h := domainHealth{ID: d.ID, Name: d.Name, Status: d.Status, ClaimExpiresAt: d.ClaimExpiresAt,
			DMARC: "missing", DKIMRotation: "idle", Ed25519: "off", AuthIssues: issues[d.Name], Inboxes: inboxes[d.ID],
			Problems: []string{}}
		for _, rec := range v.Records {
			switch rec.Record {
			case "DMARC":
				h.DMARC = rec.Status
			case "INBOUND":
				st := rec.Status
				h.Inbound = &st
			}
		}
		keys, err := s.Store.DKIMKeys(ctx, d.ID)
		if err != nil {
			s.internal(w, "overview dkim keys", err)
			return nil, false
		}
		for _, k := range keys {
			switch {
			case k.Algorithm == dkimkey.Ed25519 && k.State == "pending":
				h.Ed25519 = "pending"
			case k.Algorithm == dkimkey.Ed25519:
				h.Ed25519 = "active"
			case k.State == "pending":
				h.DKIMRotation = "pending"
			}
		}
		if d.Status != "verified" {
			h.Problems = append(h.Problems, "Not verified")
			if d.ClaimExpiresAt != nil {
				h.Problems = append(h.Problems, "Removed "+untilText(time.Until(*d.ClaimExpiresAt))+" unless verified")
			}
		}
		switch h.DMARC {
		case "verified":
		case "failed":
			h.Problems = append(h.Problems, "DMARC record invalid")
		case "missing":
			h.Problems = append(h.Problems, "DMARC record missing")
		default:
			if d.Status == "verified" {
				h.Problems = append(h.Problems, "DMARC record not checked yet")
			}
		}
		if h.Inbound != nil && *h.Inbound != "verified" && h.Inboxes > 0 {
			h.Problems = append(h.Problems, "Inbound MX record not found: its inboxes receive no mail")
		}
		if h.DKIMRotation == "pending" {
			h.Problems = append(h.Problems, "DKIM rotation waiting for its DNS record")
		}
		if h.Ed25519 == "pending" {
			h.Problems = append(h.Problems, "Ed25519 DKIM key waiting for its DNS record")
		}
		if h.AuthIssues > 0 {
			h.Problems = append(h.Problems, plural(h.AuthIssues, "authentication problem", "authentication problems")+" reported by receivers")
		}
		out = append(out, h)
	}
	return out, true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// untilText says when, roughly: "in 2 days", "in 5 hours", "within the hour".
func untilText(d time.Duration) string {
	switch {
	case d <= 0:
		return "at the next sweep"
	case d < time.Hour:
		return "within the hour"
	case d < 48*time.Hour:
		return "in " + plural(int(d.Hours()), "hour", "hours")
	default:
		return "in " + plural(int(d.Hours()/24), "day", "days")
	}
}

// workersView lists every worker of the installation (workers are shared by
// all projects) with its self-check and its IP's warm-up.
func (s *Server) workersView(r *http.Request, now time.Time) ([]workerView, error) {
	ws, err := s.Store.ListWorkers(r.Context())
	if err != nil {
		return nil, err
	}
	rules := s.Cfg.ProviderRules
	if rules == nil {
		rules = policy.DefaultRules()
	}
	out := []workerView{}
	for _, wk := range ws {
		v := workerView{Name: wk.Name, Kind: wk.Kind, Role: wk.Role, IP: wk.IP, Version: wk.Version, Draining: wk.Draining,
			LastSeenAt: wk.LastSeenAt, Online: wk.LastSeenAt != nil && now.Sub(*wk.LastSeenAt) < workerOnline}
		if len(wk.SelfCheck) > 0 {
			var c protocol.SelfCheck
			if json.Unmarshal(wk.SelfCheck, &c) == nil {
				sc := &selfCheckView{OK: c.OK || c.Skipped != "", Skipped: c.Skipped, Problems: []string{}}
				if !c.CheckedAt.IsZero() {
					at := c.CheckedAt
					sc.CheckedAt = &at
				}
				for _, it := range c.Checks {
					if !it.OK {
						p := it.Name
						if it.Detail != "" {
							p += ": " + it.Detail
						}
						sc.Problems = append(sc.Problems, p)
					}
				}
				v.SelfCheck = sc
			}
		}
		// Warm-up applies to delivery workers that send from their own IP.
		if wk.IP != "" && wk.Role != protocol.RoleInbound && wk.Kind != protocol.KindRelay {
			wv := &warmupView{Enabled: s.Cfg.Warmup, Providers: []providerCapView{}}
			if !s.Cfg.Warmup {
				wv.Warm = true
			} else {
				st, err := s.Store.IPWarmup(r.Context(), rules, wk.IP)
				if err != nil {
					return nil, err
				}
				wv.Started, wv.Warm, wv.Day = st.Started, st.Warm, st.Day
				if st.Started && !st.Warm {
					capSum, left := 0, 0
					for _, p := range st.Left {
						wv.Providers = append(wv.Providers, providerCapView{Provider: p.Provider, Cap: p.Cap, Used: p.Used})
						capSum += p.Cap
						left += max(0, p.Cap-p.Used)
					}
					wv.Cap, wv.Remaining = &capSum, &left
				}
			}
			v.Warmup = wv
		}
		out = append(out, v)
	}
	return out, nil
}

// isOperatorProject reports whether the request's project may see the
// installation's workers in detail: a project listed in
// OMW_OPERATOR_PROJECTS, or the only project of a single-tenant install.
// Workers are shared by every project, so on a multi-tenant install their
// names, addresses and versions are the operator's business.
func (s *Server) isOperatorProject(r *http.Request) (bool, error) {
	pid := projectID(r)
	if len(s.Cfg.OperatorProjects) > 0 {
		return slices.Contains(s.Cfg.OperatorProjects, pid), nil
	}
	var n int
	if err := s.Store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM (SELECT 1 FROM projects LIMIT 2) p`).Scan(&n); err != nil {
		return false, err
	}
	return n <= 1, nil
}

// redactWorkers keeps what a tenant needs to read the installation's
// health (online, draining, self-check passed, warm-up progress) and drops
// what identifies the operator's machines.
func redactWorkers(ws []workerView) {
	for i := range ws {
		ws[i].Name = fmt.Sprintf("worker %d", i+1)
		ws[i].IP = ""
		ws[i].Version = ""
		if ws[i].SelfCheck != nil {
			c := *ws[i].SelfCheck
			if len(c.Problems) > 0 {
				c.Problems = []string{"the worker's self-check reported problems; the operator can see them"}
			}
			ws[i].SelfCheck = &c
		}
	}
}
