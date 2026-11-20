package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/dnsprovider"
	"github.com/openmailwire/openmailwire/internal/store"
)

// DNS maintenance (lane G), run by the control plane every few minutes:
//
//   - the operator's SPF include (OMW_SPF_INCLUDE, with OMW_CLOUDFLARE_TOKEN)
//     is kept listing the live delivery workers' IPs;
//   - DKIM records published with a remembered token are deleted at the
//     provider once their key is retired (after a rotation's overlap, or
//     when Ed25519 is turned off).
//
// Cloudflare is asked only when there is something to check: the SPF
// value is compared with the last one published first, and a domain is
// touched only when one of its DKIM records is due for deletion. Failures
// back off, up to an hour.

// DNSMaintenanceInterval is how often the control plane runs it.
const DNSMaintenanceInterval = 5 * time.Minute

// senderWindow is how recent a worker's heartbeat must be for its IP to
// stay in the operator's SPF record.
const senderWindow = 7 * 24 * time.Hour

// spfRecheck is how long an SPF value known to be published is trusted
// before Cloudflare is asked again (to repair a record changed by hand).
const spfRecheck = time.Hour

type spfSyncState struct {
	mu        sync.Mutex
	zone      dnsprovider.Zone
	published string    // the value last seen published
	checkedAt time.Time // when it was read or written at Cloudflare
	failures  int
	notBefore time.Time
}

// OperatorSPF is what one operator SPF sync did.
type OperatorSPF struct {
	Name   string   // OMW_SPF_INCLUDE
	Zone   string   // the Cloudflare zone holding it
	Action string   // create, update, unchanged or skip
	Old    []string // the SPF records that were there
	New    string
	IPs    []string
	// Deleted counts duplicate SPF records removed.
	Deleted int
	// Reason says why nothing was written, for skip (and for an unchanged
	// value that was not read again).
	Reason string
}

// ErrOperatorSPFOff means OMW_CLOUDFLARE_TOKEN or OMW_SPF_INCLUDE is unset.
var ErrOperatorSPFOff = errors.New("operator SPF sync needs OMW_CLOUDFLARE_TOKEN and OMW_SPF_INCLUDE")

// SyncOperatorSPF makes the TXT record at OMW_SPF_INCLUDE authorize exactly
// the IPs that direct delivery workers announced within 7 days. It writes
// only when the value changed, and never an SPF record that authorizes no
// one: with no live worker it changes nothing. A dry run only reads.
func (s *Server) SyncOperatorSPF(ctx context.Context, dryRun bool) (OperatorSPF, error) {
	out := OperatorSPF{Name: dnsprovider.Canon(s.Cfg.SPFInclude)}
	if s.Cfg.CloudflareToken == "" || out.Name == "" {
		return out, ErrOperatorSPFOff
	}
	ips, err := s.Store.LiveSenderIPs(ctx, senderWindow)
	if err != nil {
		return out, err
	}
	out.New, out.IPs = dnsprovider.SenderSPF(ips)
	if len(out.IPs) == 0 {
		out.Action, out.Reason = dnsSkip, "no delivery worker announced a public IP (OMW_PUBLIC_IP) in the last 7 days; an SPF record that authorizes no one would stop all mail"
		return out, nil
	}
	st := &s.dns.spf
	st.mu.Lock()
	defer st.mu.Unlock()
	if !dryRun && out.New == st.published && time.Since(st.checkedAt) < spfRecheck {
		out.Action, out.Zone, out.Old = dnsUnchanged, st.zone.Name, []string{st.published}
		out.Reason = "published already"
		return out, nil
	}
	p := s.newDNSProvider(s.Cfg.CloudflareToken)
	if st.zone.ID == "" {
		z, err := p.FindZone(ctx, out.Name)
		if err != nil {
			return out, err
		}
		st.zone = z
	}
	out.Zone = st.zone.Name
	ch, err := dnsprovider.SyncSPF(ctx, p, st.zone, out.Name, out.New, dryRun)
	out.Action, out.Old, out.Deleted = ch.Action, ch.Old, ch.Deleted
	if err != nil {
		if dnsprovider.KindOf(err) == dnsprovider.KindNotFound {
			st.zone = dnsprovider.Zone{} // the zone may be gone; look it up again next time
		}
		return out, err
	}
	if !dryRun {
		st.published, st.checkedAt = out.New, time.Now()
		if out.Action != dnsUnchanged || out.Deleted > 0 {
			s.Log.Info("operator SPF include updated", "name", out.Name, "action", out.Action, "old", out.Old, "new", out.New)
		}
	}
	return out, nil
}

// RunDNSMaintenance runs the DNS maintenance every interval until ctx ends.
func (s *Server) RunDNSMaintenance(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.dnsMaintenance(ctx, every)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) dnsMaintenance(ctx context.Context, every time.Duration) {
	if s.Cfg.CloudflareToken != "" && s.Cfg.SPFInclude != "" {
		st := &s.dns.spf
		st.mu.Lock()
		due := !time.Now().Before(st.notBefore)
		st.mu.Unlock()
		if due {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_, err := s.SyncOperatorSPF(cctx, false)
			cancel()
			st.mu.Lock()
			if err != nil && ctx.Err() == nil {
				st.failures++
				wait := min(every<<min(st.failures, 4), time.Hour)
				st.notBefore = time.Now().Add(wait)
				s.Log.Warn("operator SPF sync failed; trying again later", "name", s.Cfg.SPFInclude, "retry_in", wait.String(), "err", err)
			} else {
				st.failures, st.notBefore = 0, time.Time{}
			}
			st.mu.Unlock()
		}
	}
	if err := s.RetireDKIMRecords(ctx); err != nil && ctx.Err() == nil {
		s.Log.Warn("retire dkim records", "err", err)
	}
}

// RetireDKIMRecords deletes, at the provider, the DKIM records published
// with a remembered token whose key no longer signs or verifies anything:
// a rotated-out key past its retire_at, or an Ed25519 key turned off. A
// rotated-out key is retired whether or not it was published through the
// provider, since its selector is OpenMailWire's own.
func (s *Server) RetireDKIMRecords(ctx context.Context) error {
	autos, err := s.Store.RememberedDNS(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range autos {
		if err := s.retireDomainDKIM(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) retireDomainDKIM(ctx context.Context, a store.DNSAutomation) error {
	d, err := s.Store.DomainByID(ctx, a.DomainID)
	if err != nil {
		return err
	}
	keys, err := s.Store.DKIMKeys(ctx, d.ID)
	if err != nil {
		return err
	}
	name := func(sel string) string { return dnsprovider.Canon(sel + "._domainkey." + d.Name) }
	needed := []string{name(d.Selector)}
	var retiring []string
	now := time.Now()
	for _, k := range keys {
		n := name(k.Selector)
		if k.State == "retiring" {
			retiring = append(retiring, n)
			if k.RetireAt == nil || k.RetireAt.After(now) {
				needed = append(needed, n)
			}
			continue
		}
		needed = append(needed, n)
	}
	var add, gone []string
	for _, n := range retiring {
		if !slices.Contains(a.PublishedDKIM, n) {
			add = append(add, n)
		}
	}
	for _, n := range append(slices.Clone(a.PublishedDKIM), add...) {
		if !slices.Contains(needed, n) && !slices.Contains(gone, n) {
			gone = append(gone, n)
		}
	}
	if len(gone) == 0 {
		if len(add) > 0 {
			return s.Store.TrackPublishedDKIM(ctx, d.ID, add, nil)
		}
		return nil
	}
	lock := s.dnsLock(d.ID)
	lock.Lock()
	defer lock.Unlock()
	raw, err := s.Sealer.Open(a.Token)
	if err != nil {
		return err
	}
	p := s.newDNSProvider(string(raw))
	run := dnsRun{Object: "dns_auto_run", Provider: a.Provider, Zone: dnsZoneView{ID: a.ZoneID, Name: a.ZoneName},
		Trigger: "dkim_retire", RanAt: now.UTC(), Remembered: true, Changes: []dnsChange{}, Warnings: []string{}}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var removed []string
	var failed error
	for _, n := range gone {
		recs, err := p.Records(cctx, a.ZoneID, n)
		if err != nil {
			failed = err
			break
		}
		for _, r := range ofType(recs, "TXT", "v=DKIM1") {
			if err = p.Delete(cctx, a.ZoneID, r.ID); err != nil {
				break
			}
			run.Changes = append(run.Changes, dnsChange{Record: "DKIM_RETIRED", Type: "TXT", Name: n, Value: r.Content, Action: dnsDelete,
				Detail: "the key no longer signs, and mail signed with it is past its overlap"})
		}
		if err != nil {
			failed = err
			break
		}
		removed = append(removed, n)
	}
	if err := s.Store.TrackPublishedDKIM(ctx, d.ID, add, removed); err != nil {
		return err
	}
	if failed != nil {
		pe := s.dnsError(failed, d, true)
		run.warn("Retiring old DKIM records stopped: %s", pe.Message)
	}
	if len(run.Changes) > 0 || failed != nil {
		if err := s.saveDNSRun(ctx, d, &run, nil); err != nil {
			return err
		}
	}
	if len(run.Changes) > 0 {
		if err := s.Store.AppendAudit(ctx, store.AuditEntry{ProjectID: d.ProjectID, Actor: store.ActorSystem, Action: "dns.applied",
			TargetType: "domain", TargetID: d.ID, Data: dnsAuditData(run, "remembered")}); err != nil {
			s.Log.Error("audit log", "action", "dns.applied", "err", err)
		}
	}
	if failed != nil {
		return errors.New("domain " + d.Name + ": " + strings.TrimSpace(failed.Error()))
	}
	return nil
}
