package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/dnsprovider"
	"github.com/openmailwire/openmailwire/internal/store"
)

// Automatic DNS setup (lane G).
//
// POST /domains/{id}/dns/auto publishes every record a domain needs at its
// DNS host (Cloudflare), with an API token from the domain's owner:
//
//   - the same records GET /domains/{id} lists (DKIM, including a key
//     waiting in a rotation and Ed25519; the return path's SPF and MX;
//     DMARC; the INBOUND MX when this server receives mail), plus TLS-RPT;
//   - idempotent: a record that is already right is "unchanged";
//   - never proxied, and never over the owner's own mail setup: an apex
//     with MX records elsewhere keeps them, an existing DMARC record keeps
//     its policy; only the return-path name, which belongs to OpenMailWire,
//     is overwritten (and the change reported);
//   - the zone's settings that matter for mail are reported as warnings.
//
// With remember: true the token is kept, sealed, so that later changes
// publish themselves: a DKIM rotation's new record, an Ed25519 record, and
// the deletion of a rotated-out DKIM record after its overlap. The MCP tool
// setup_domain_dns uses only a remembered token, so an agent never handles
// one.

// dnsAutoTimeout bounds one run: every provider call, and the check after.
const dnsAutoTimeout = 90 * time.Second

// Actions in a run's changes.
const (
	dnsCreate    = "create"
	dnsUpdate    = "update"
	dnsUnchanged = "unchanged"
	dnsSkip      = "skip"
	dnsDelete    = "delete"
)

// dnsRoutes registers the endpoints (called from Handler).
func (s *Server) dnsRoutes(mux *http.ServeMux) {
	mux.Handle("POST /domains/{id}/dns/auto", s.withFullAccess(s.applyDNSAuto))
	mux.Handle("GET /domains/{id}/dns/auto", s.withFullAccess(s.getDNSAuto))
	mux.Handle("DELETE /domains/{id}/dns/auto", s.withFullAccess(s.forgetDNSAuto))
}

// dnsState is the control plane's in-memory state for DNS automation.
type dnsState struct {
	locks [64]sync.Mutex // one run per domain at a time (by hash)
	spf   spfSyncState
}

func (s *Server) dnsLock(domainID string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(domainID))
	return &s.dns.locks[h.Sum32()%uint32(len(s.dns.locks))]
}

// newDNSProvider returns the provider client for a token. Only Cloudflare
// exists so far.
func (s *Server) newDNSProvider(token string) dnsprovider.Provider {
	return dnsprovider.NewCloudflare(s.Cfg.CloudflareAPIBase, token)
}

type dnsZoneView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
	// DNSSEC and EmailRouting are the zone's settings, when the run checked
	// them: "enabled", "disabled" or "unknown" for Email Routing, the
	// provider's status (e.g. "active") for DNSSEC.
	DNSSEC       string `json:"dnssec,omitempty"`
	EmailRouting string `json:"email_routing,omitempty"`
}

// dnsChange is what a run did, or would do, to one record.
type dnsChange struct {
	Record   string `json:"record"` // DKIM, DKIM_NEXT, DKIM_PREVIOUS, DKIM_ED25519, SPF, MX, DMARC, TLSRPT or INBOUND
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Action   string `json:"action"` // create, update, unchanged, skip, delete
	Detail   string `json:"detail,omitempty"`

	id string // the provider's record id, for update and delete
}

// dnsRun is one run's result: the answer of POST /domains/{id}/dns/auto,
// and (without the domain) what GET shows as last_run.
type dnsRun struct {
	Object     string          `json:"object"`
	Provider   string          `json:"provider"`
	Zone       dnsZoneView     `json:"zone"`
	DryRun     bool            `json:"dry_run"`
	Changes    []dnsChange     `json:"changes"`
	Warnings   []string        `json:"warnings"`
	Remembered bool            `json:"remembered"`
	Trigger    string          `json:"trigger"`
	RanAt      time.Time       `json:"ran_at"`
	Domain     *domainResponse `json:"domain,omitempty"`
}

func (run *dnsRun) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !slices.Contains(run.Warnings, msg) {
		run.Warnings = append(run.Warnings, msg)
	}
}

type dnsRunOptions struct {
	ZoneID  string
	DryRun  bool
	Checks  bool            // the zone's settings: status, Email Routing, DNSSEC, proxied names
	Only    map[string]bool // record kinds to publish; nil means every one
	Trigger string          // api, mcp, dkim_rotate, ed25519, dkim_retire
}

type dnsAutoRequest struct {
	Provider string `json:"provider"`
	APIToken string `json:"api_token"`
	ZoneID   string `json:"zone_id"`
	DryRun   bool   `json:"dry_run"`
	Remember bool   `json:"remember"`
}

// validToken accepts what a provider token can look like, without saying
// anything about the value.
func validToken(t string) bool {
	if len(t) < 20 || len(t) > 512 {
		return false
	}
	for _, c := range t {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

func (s *Server) applyDNSAuto(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req dnsAutoRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := decodeStrict(body, &req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error",
				`send {"provider": "cloudflare", "api_token": "…", "zone_id": "…", "dry_run": false, "remember": false} (`+err.Error()+")")
			return
		}
	}
	switch {
	case req.Provider != "" && req.Provider != dnsprovider.CloudflareName:
		writeError(w, http.StatusUnprocessableEntity, "validation_error", `provider must be "cloudflare", the only DNS provider supported so far`)
		return
	case req.APIToken != "" && !validToken(req.APIToken):
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"api_token is not a Cloudflare API token: copy the token itself (20 to 512 printable characters, no spaces) from Cloudflare, My Profile > API Tokens")
		return
	case req.ZoneID != "" && !dnsprovider.ValidCloudflareID(req.ZoneID):
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"zone_id must be a Cloudflare zone id: 32 hexadecimal digits, shown on the zone's Overview page; or leave it out to find the zone by name")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dnsAutoTimeout)
	defer cancel()
	lock := s.dnsLock(d.ID)
	lock.Lock()
	defer lock.Unlock()

	auto, err := s.Store.DNSAutomation(ctx, d.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "dns automation", err)
		return
	}
	token, source := req.APIToken, "request"
	zoneID := req.ZoneID
	if token == "" {
		if auto.Token == nil {
			writeError(w, http.StatusConflict, "dns_provider_not_connected", fmt.Sprintf(
				"no Cloudflare token is remembered for %s. A person must connect Cloudflare once: in the dashboard, or with POST /domains/%s/dns/auto and "+
					`{"provider": "cloudflare", "api_token": "…", "remember": true}`+
					" (a token with Zone > DNS > Edit for the domain's zone). An agent should never ask for or handle the token itself.", d.Name, d.ID))
			return
		}
		raw, err := s.Sealer.Open(auto.Token)
		if err != nil {
			s.internal(w, "open remembered dns token", err)
			return
		}
		token, source = string(raw), "remembered"
		if zoneID == "" {
			zoneID = auto.ZoneID
		}
	}
	trigger := "api"
	if isHop(r.Context()) {
		trigger = "mcp"
	}
	run, err := s.runDNS(ctx, s.newDNSProvider(token), d, dnsRunOptions{ZoneID: zoneID, DryRun: req.DryRun, Checks: true, Trigger: trigger})
	if err != nil {
		s.dnsError(err, d, source == "remembered").write(w)
		return
	}

	var sealed []byte
	remembered := auto.Token != nil
	if req.Remember && source == "request" {
		sealed, remembered = s.Sealer.Seal([]byte(token)), true
	}
	run.Remembered = remembered
	if !req.DryRun {
		checked, pe := s.checkDomain(r, d)
		switch {
		case pe != nil && pe.Name == "domain_verified_elsewhere":
			run.warn("%s", pe.Message)
		case pe != nil:
			pe.write(w)
			return
		default:
			d = checked
		}
	}
	if err := s.saveDNSRun(ctx, d, &run, sealed); err != nil {
		s.internal(w, "save dns run", err)
		return
	}
	if !req.DryRun {
		s.audit(r, "dns.applied", "domain", d.ID, dnsAuditData(run, source))
	}
	if sealed != nil {
		s.audit(r, "dns.token_remembered", "domain", d.ID, map[string]any{
			"name": d.Name, "provider": run.Provider, "zone_id": run.Zone.ID, "zone_name": run.Zone.Name})
	}
	view := s.view(r.Context(), d)
	run.Domain = &view
	writeJSON(w, http.StatusOK, run)
}

// dnsAuditData summarizes a run for the audit log: counts, never values
// or the token.
func dnsAuditData(run dnsRun, source string) map[string]any {
	counts := map[string]int{}
	for _, c := range run.Changes {
		counts[c.Action]++
	}
	return map[string]any{
		"provider": run.Provider, "zone_id": run.Zone.ID, "zone_name": run.Zone.Name, "trigger": run.Trigger, "token": source,
		"created": counts[dnsCreate], "updated": counts[dnsUpdate], "deleted": counts[dnsDelete],
		"unchanged": counts[dnsUnchanged], "skipped": counts[dnsSkip], "warnings": len(run.Warnings),
	}
}

// saveDNSRun stores the run as the domain's last one and remembers the
// sealed token when there is one.
func (s *Server) saveDNSRun(ctx context.Context, d store.Domain, run *dnsRun, sealed []byte) error {
	saved := *run
	saved.Domain = nil
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	var dkim []string
	if !run.DryRun {
		for _, c := range run.Changes {
			if strings.HasPrefix(c.Record, "DKIM") && c.Action != dnsSkip && c.Action != dnsDelete {
				dkim = append(dkim, c.Name)
			}
		}
	}
	return s.Store.SaveDNSRun(ctx, store.DNSRun{DomainID: d.ID, Provider: run.Provider, ZoneID: run.Zone.ID, ZoneName: run.Zone.Name,
		LastRun: data, AddDKIM: dkim, Token: sealed})
}

// dnsError turns a provider error into the API's answer, naming the fix.
func (s *Server) dnsError(err error, d store.Domain, remembered bool) *policyError {
	var pe *dnsprovider.Error
	if !errors.As(err, &pe) {
		return s.internalErr("dns automation", err)
	}
	s.Log.Warn("dns automation", "domain", d.Name, "kind", pe.Kind.String(), "err", pe.Error())
	switch pe.Kind {
	case dnsprovider.KindAuth:
		msg := "Cloudflare refused the API token (" + pe.Msg + "): it is invalid or expired, or it lacks the permission Zone > DNS > Edit for the zone of " +
			d.Name + ". Create a token in Cloudflare (My Profile > API Tokens > Create Token, template \"Edit zone DNS\", Zone Resources: the zone of " +
			d.Name + ") and send it as api_token."
		if remembered {
			msg += " The remembered token no longer works: connect Cloudflare again with a new token and remember: true, or forget it with DELETE /domains/" +
				d.ID + "/dns/auto."
		}
		return &policyError{Status: http.StatusUnprocessableEntity, Name: "dns_provider_auth_failed", Message: msg}
	case dnsprovider.KindZoneNotFound:
		msg := "no Cloudflare zone that this token can see holds " + d.Name
		if pe.Msg != "" && !strings.HasPrefix(pe.Msg, "no zone") {
			msg += " (" + pe.Msg + ")"
		}
		return &policyError{Status: http.StatusUnprocessableEntity, Name: "dns_zone_not_found", Message: msg +
			". Add the domain (or its parent) to Cloudflare, give the token access to that zone (Zone Resources: Include > Specific zone), or pass the zone's zone_id."}
	default:
		wait := pe.RetryAfter
		if wait <= 0 {
			wait = time.Minute
		}
		return &policyError{Status: http.StatusServiceUnavailable, Name: "dns_provider_unavailable", RetryAfter: wait, Message: "Cloudflare could not do it now (" +
			pe.Error() + "). Records changed before this are kept; the call is safe to repeat: try again in a few minutes, after Retry-After."}
	}
}

// wantRecord is one record a domain needs.
type wantRecord struct {
	kind     string // as dnsChange.Record
	typ      string // TXT or MX
	name     string
	value    string
	priority int
	skip     string // set: not published, and why
}

// bounceHost is where this server receives mail for return paths, or "".
func (s *Server) bounceHost() string {
	if s.Cfg.BounceMX != "" {
		return s.Cfg.BounceMX
	}
	return s.Cfg.InboundMX
}

// wantedRecords lists what d needs, from the same source as GET
// /domains/{id}, plus TLS-RPT.
func (s *Server) wantedRecords(ctx context.Context, d store.Domain, only map[string]bool) []wantRecord {
	view := s.view(ctx, d)
	bounce := s.bounceHost()
	ra := view.ReportAddresses
	var out []wantRecord
	add := func(w wantRecord) {
		if only == nil || only[w.kind] {
			w.name = dnsprovider.Canon(w.name)
			out = append(out, w)
		}
	}
	for _, rec := range view.Records {
		switch rec.Record {
		case "DKIM", "DKIM_NEXT", "DKIM_PREVIOUS", "DKIM_ED25519":
			add(wantRecord{kind: rec.Record, typ: "TXT", name: rec.Name, value: rec.Value})
		case "SPF":
			add(wantRecord{kind: "SPF", typ: "TXT", name: rec.Name, value: rec.Value})
		case "MX":
			w := wantRecord{kind: "MX", typ: "MX", name: rec.Name, value: dnsprovider.Canon(rec.Value), priority: max(rec.Priority, 10)}
			if w.value == "" {
				w.skip = "this server has no bounce host (OMW_BOUNCE_MX or OMW_INBOUND_MX), so there is no MX to point the return path at"
			}
			add(w)
		case "DMARC":
			v := "v=DMARC1; p=none;"
			if bounce != "" && ra != nil {
				v = "v=DMARC1; p=none; rua=" + ra.DMARC
			}
			add(wantRecord{kind: "DMARC", typ: "TXT", name: rec.Name, value: v})
		case "INBOUND":
			add(wantRecord{kind: "INBOUND", typ: "MX", name: rec.Name, value: dnsprovider.Canon(rec.Value), priority: max(rec.Priority, 10)})
		}
	}
	if ra != nil {
		w := wantRecord{kind: "TLSRPT", typ: "TXT", name: ra.TLSRecord.Name, value: ra.TLSRecord.Value}
		if bounce == "" {
			w.skip = "this server has no bounce host (OMW_BOUNCE_MX), so TLS reports sent to " + strings.TrimPrefix(ra.TLS, "mailto:") + " would have nowhere to go"
		}
		add(w)
	}
	return out
}

// recordCache lists each name's records once per run.
type recordCache struct {
	p    dnsprovider.Provider
	zone string
	got  map[string][]dnsprovider.Record
}

func (c *recordCache) at(ctx context.Context, name string) ([]dnsprovider.Record, error) {
	name = dnsprovider.Canon(name)
	if r, ok := c.got[name]; ok {
		return r, nil
	}
	r, err := c.p.Records(ctx, c.zone, name)
	if err != nil {
		return nil, err
	}
	c.got[name] = r
	return r, nil
}

func ofType(recs []dnsprovider.Record, typ, tag string) []dnsprovider.Record {
	var out []dnsprovider.Record
	for _, r := range recs {
		if r.Type == typ && (tag == "" || dnsprovider.HasTag(r.Content, tag)) {
			out = append(out, r)
		}
	}
	return out
}

func dkimP(txt string) string {
	for _, part := range strings.Split(dnsprovider.NormalizeTXT(txt), ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == "p" {
			return strings.Join(strings.Fields(v), "")
		}
	}
	return ""
}

// authorizes reports whether an SPF text lists every mechanism of ours.
func authorizes(spf, ours string) bool {
	have := map[string]bool{}
	for _, f := range strings.Fields(strings.ToLower(dnsprovider.NormalizeTXT(spf))) {
		have[strings.TrimPrefix(f, "+")] = true
	}
	for _, f := range strings.Fields(strings.ToLower(ours)) {
		if f == "v=spf1" || f == "-all" {
			continue
		}
		if !have[f] {
			return false
		}
	}
	return true
}

func hosts(recs []dnsprovider.Record) string {
	var hs []string
	for _, r := range recs {
		h := r.Content
		if h == "" {
			h = "." // a null MX
		}
		hs = append(hs, h)
	}
	return strings.Join(hs, ", ")
}

// findZone looks the zone up by id or by name. The token is verified first;
// an account-owned token cannot use Cloudflare's verify endpoint, so a
// refusal there counts only if the zone lookup is refused too.
func findZone(ctx context.Context, p dnsprovider.Provider, d store.Domain, zoneID string) (dnsprovider.Zone, error) {
	verr := p.VerifyToken(ctx)
	if verr != nil && dnsprovider.KindOf(verr) != dnsprovider.KindAuth {
		return dnsprovider.Zone{}, verr
	}
	var zone dnsprovider.Zone
	var err error
	if zoneID != "" {
		zone, err = p.Zone(ctx, zoneID)
		if err == nil && !zone.Contains(d.Name) {
			err = &dnsprovider.Error{Kind: dnsprovider.KindZoneNotFound, Op: "read the zone " + zoneID,
				Msg: "zone " + zoneID + " is " + zone.Name + ", which does not hold " + d.Name}
		}
	} else {
		zone, err = p.FindZone(ctx, d.Name)
	}
	if err != nil && verr != nil {
		return dnsprovider.Zone{}, verr
	}
	return zone, err
}

// runDNS plans the changes, checks the zone's settings, and applies the
// changes unless it is a dry run. A provider error that concerns the whole
// run (the token, the zone, the provider being unavailable) is returned;
// one record the provider refuses is skipped and reported.
func (s *Server) runDNS(ctx context.Context, p dnsprovider.Provider, d store.Domain, o dnsRunOptions) (dnsRun, error) {
	run := dnsRun{Object: "dns_auto_run", Provider: p.Name(), DryRun: o.DryRun, Trigger: o.Trigger, RanAt: time.Now().UTC(),
		Changes: []dnsChange{}, Warnings: []string{}}
	zone, err := findZone(ctx, p, d, o.ZoneID)
	if err != nil {
		return run, err
	}
	run.Zone = dnsZoneView{ID: zone.ID, Name: zone.Name, Status: zone.Status}
	cache := &recordCache{p: p, zone: zone.ID, got: map[string][]dnsprovider.Record{}}

	for _, w := range s.wantedRecords(ctx, d, o.Only) {
		changes, err := s.planRecord(ctx, cache, zone, d, w, &run)
		if err != nil {
			return run, err
		}
		run.Changes = append(run.Changes, changes...)
	}
	if o.Checks {
		if err := s.checkZoneSettings(ctx, p, cache, zone, d, &run); err != nil {
			return run, err
		}
	}
	if o.DryRun {
		if o.Checks {
			run.warn("dry run: nothing was changed. It cannot tell whether the token may edit DNS (Zone > DNS > Edit); a real run can.")
		}
		return run, nil
	}
	applied := 0
	for i := range run.Changes {
		c := &run.Changes[i]
		var err error
		rec := dnsprovider.Record{ID: c.id, Type: c.Type, Name: c.Name, Content: c.Value, Priority: c.Priority}
		switch c.Action {
		case dnsCreate:
			_, err = p.Create(ctx, zone.ID, rec)
			if dnsprovider.IsDuplicate(err) {
				err, c.Action, c.Detail = nil, dnsUnchanged, "already published"
			}
		case dnsUpdate:
			_, err = p.Update(ctx, zone.ID, rec)
		case dnsDelete:
			err = p.Delete(ctx, zone.ID, c.id)
		default:
			continue
		}
		switch dnsprovider.KindOf(err) {
		case 0:
			applied++
		case dnsprovider.KindAuth, dnsprovider.KindUnavailable:
			if applied > 0 {
				var pe *dnsprovider.Error
				errors.As(err, &pe)
				pe.Msg += fmt.Sprintf("; %d change(s) were made before this one", applied)
			}
			return run, err
		default:
			var pe *dnsprovider.Error
			errors.As(err, &pe)
			c.Detail = "Cloudflare refused it: " + pe.Msg
			run.warn("%s %s at %s was not %sd: Cloudflare refused it (%s). Fix it in Cloudflare's dashboard, then run this again.",
				c.Record, c.Type, c.Name, c.Action, pe.Msg)
			c.Action = dnsSkip
		}
	}
	return run, nil
}

// planRecord decides what to do about one record the domain needs.
func (s *Server) planRecord(ctx context.Context, cache *recordCache, zone dnsprovider.Zone, d store.Domain, w wantRecord, run *dnsRun) ([]dnsChange, error) {
	c := dnsChange{Record: w.kind, Type: w.typ, Name: w.name, Value: w.value, Priority: w.priority}
	if w.typ != "MX" {
		c.Priority = 0
	}
	if w.skip != "" {
		c.Action, c.Detail = dnsSkip, w.skip
		return []dnsChange{c}, nil
	}
	if !zone.Contains(w.name) {
		c.Action, c.Detail = dnsSkip, "not in the zone "+zone.Name
		return []dnsChange{c}, nil
	}
	cur, err := cache.at(ctx, w.name)
	if err != nil {
		return nil, err
	}
	if cn := ofType(cur, "CNAME", ""); len(cn) > 0 && (w.kind == "DMARC" || w.kind == "TLSRPT") {
		// The owner delegated the record (to a DMARC reporting service, say).
		c.Action, c.Detail = dnsSkip, "delegated with a CNAME to "+cn[0].Content+"; left as it is"
		return []dnsChange{c}, nil
	} else if len(cn) > 0 {
		c.Action, c.Detail = dnsSkip, "a CNAME record (to "+cn[0].Content+") is at this name, and no other record may sit next to a CNAME"
		run.warn("%s has a CNAME record (to %s), so the %s record cannot be published there. Remove the CNAME in Cloudflare, then run this again.",
			w.name, cn[0].Content, w.kind)
		return []dnsChange{c}, nil
	}
	switch w.kind {
	case "DKIM", "DKIM_NEXT", "DKIM_PREVIOUS", "DKIM_ED25519":
		return planDKIM(c, ofType(cur, "TXT", "v=DKIM1")), nil
	case "SPF":
		return planSPF(c, ofType(cur, "TXT", "v=spf1"), run), nil
	case "MX":
		return planReturnMX(c, ofType(cur, "MX", ""), run), nil
	case "DMARC":
		return s.planDMARC(ctx, cache, zone, d, c, ofType(cur, "TXT", "v=DMARC1"), run)
	case "TLSRPT":
		return planTLSRPT(c, ofType(cur, "TXT", "v=TLSRPTv1"), run), nil
	case "INBOUND":
		return planInbound(c, ofType(cur, "MX", ""), d, run), nil
	}
	return nil, nil
}

// planDKIM: a selector name is OpenMailWire's own, so a DKIM record there
// with another key is replaced.
func planDKIM(c dnsChange, txt []dnsprovider.Record) []dnsChange {
	want := dkimP(c.Value)
	for _, r := range txt {
		if dkimP(r.Content) == want {
			c.Action, c.id = dnsUnchanged, r.ID
			return []dnsChange{c}
		}
	}
	if len(txt) > 0 {
		c.Action, c.id, c.Detail = dnsUpdate, txt[0].ID, "replaces a DKIM record with another key at this selector"
		return []dnsChange{c}
	}
	c.Action = dnsCreate
	return []dnsChange{c}
}

// planSPF: the return path belongs to OpenMailWire. An SPF record there
// that does not authorize OpenMailWire's senders is replaced (and the
// replacement reported); a second SPF record, which makes SPF fail at
// every receiver, is deleted.
func planSPF(c dnsChange, spf []dnsprovider.Record, run *dnsRun) []dnsChange {
	keep := -1
	for i, r := range spf {
		if dnsprovider.SameTXT(r.Content, c.Value) {
			keep = i
			break
		}
	}
	switch {
	case keep >= 0:
		c.Action, c.id = dnsUnchanged, spf[keep].ID
	case len(spf) == 1 && authorizes(spf[0].Content, c.Value):
		keep = 0
		c.Action, c.id, c.Value = dnsUnchanged, spf[0].ID, spf[0].Content
		c.Detail = "the SPF record there already authorizes OpenMailWire's senders, next to others"
	case len(spf) == 0:
		c.Action = dnsCreate
	default:
		keep = 0
		c.Action, c.id = dnsUpdate, spf[0].ID
		c.Detail = fmt.Sprintf("replaces %q, which does not authorize OpenMailWire's senders", spf[0].Content)
		run.warn("The SPF record at %s (%q) was not OpenMailWire's: it is replaced, because the return path %s belongs to OpenMailWire "+
			"and must authorize its senders. Nothing else of the domain's mail setup is touched.", c.Name, spf[0].Content, c.Name)
	}
	out := []dnsChange{c}
	for i, r := range spf {
		if i == keep {
			continue
		}
		out = append(out, dnsChange{Record: "SPF", Type: "TXT", Name: c.Name, Value: r.Content, Action: dnsDelete, id: r.ID,
			Detail: "a second SPF record at one name makes SPF fail at every receiver"})
		run.warn("%s had more than one SPF record; the extra one (%q) is deleted, because receivers treat two as an error.", c.Name, r.Content)
	}
	return out
}

// planReturnMX: the return path's MX points to this server's bounce host.
func planReturnMX(c dnsChange, mx []dnsprovider.Record, run *dnsRun) []dnsChange {
	for _, r := range mx {
		if r.Content == c.Value {
			c.Action, c.id, c.Priority = dnsUnchanged, r.ID, r.Priority
			if len(mx) > 1 {
				run.warn("%s has other MX records besides %s (%s); receivers may send bounces there instead of to OpenMailWire.", c.Name, c.Value, hosts(mx))
			}
			return []dnsChange{c}
		}
	}
	if len(mx) == 0 {
		c.Action = dnsCreate
		return []dnsChange{c}
	}
	c.Action, c.id = dnsUpdate, mx[0].ID
	c.Detail = "replaces MX " + hosts(mx[:1])
	run.warn("The return path %s had MX %s, not OpenMailWire's bounce host %s: it is replaced, because the return path belongs to OpenMailWire.",
		c.Name, hosts(mx), c.Value)
	out := []dnsChange{c}
	for _, r := range mx[1:] {
		out = append(out, dnsChange{Record: "MX", Type: "MX", Name: c.Name, Value: r.Content, Priority: r.Priority, Action: dnsDelete, id: r.ID,
			Detail: "another MX at the return path would take bounces away from OpenMailWire"})
	}
	return out
}

func mentions(txt, address string) bool {
	return address != "" && strings.Contains(strings.ToLower(dnsprovider.NormalizeTXT(txt)), strings.ToLower(address))
}

// planDMARC never rewrites a DMARC policy someone set: an existing record,
// or one the domain inherits from a parent in the zone, is left alone, with
// a warning when it does not send reports here. Only a domain with no
// DMARC at all gets one: p=none, with OpenMailWire's report address.
func (s *Server) planDMARC(ctx context.Context, cache *recordCache, zone dnsprovider.Zone, d store.Domain, c dnsChange,
	dm []dnsprovider.Record, run *dnsRun) ([]dnsChange, error) {
	ra := s.reportAddresses(d)
	rua := ""
	if ra != nil && s.bounceHost() != "" {
		rua = strings.TrimPrefix(ra.DMARC, "mailto:")
	}
	hint := ""
	if rua != "" {
		hint = " To see aggregate reports in GET /domains/" + d.ID + "/reports, add rua=mailto:" + rua + " to it yourself."
	}
	if len(dm) > 0 {
		want := c.Value
		c.Action, c.id, c.Value = dnsUnchanged, dm[0].ID, dm[0].Content
		if len(dm) == 1 && dnsprovider.SameTXT(dm[0].Content, want) {
			return []dnsChange{c}, nil
		}
		c.Detail = "a DMARC record already exists; its policy is left as it is"
		if len(dm) > 1 {
			run.warn("%s has more than one DMARC record, and receivers then ignore them all; keep one. OpenMailWire changed none of them.", c.Name)
		}
		if !mentions(dm[0].Content, rua) && hint != "" {
			run.warn("A DMARC record already exists at %s (%q); OpenMailWire left it as it is.%s", c.Name, dm[0].Content, hint)
		}
		return []dnsChange{c}, nil
	}
	// A parent's record in the same zone covers the domain: a record here
	// would replace that policy for it.
	labels := strings.Split(d.Name, ".")
	for i := 1; i+1 < len(labels); i++ {
		parent := strings.Join(labels[i:], ".")
		if !zone.Contains(parent) {
			break
		}
		recs, err := cache.at(ctx, "_dmarc."+parent)
		if err != nil {
			return nil, err
		}
		if p := ofType(recs, "TXT", "v=DMARC1"); len(p) > 0 {
			c.Action, c.Value = dnsSkip, p[0].Content
			c.Detail = "the domain inherits the DMARC policy at _dmarc." + parent + "; a record here would replace it for " + d.Name
			if !mentions(p[0].Content, rua) && hint != "" {
				run.warn("%s has no DMARC record of its own and inherits _dmarc.%s (%q); OpenMailWire added none, because one here would override that policy.%s",
					d.Name, parent, p[0].Content, strings.Replace(hint, "add rua", "add a record of your own with rua", 1))
			}
			return []dnsChange{c}, nil
		}
	}
	c.Action = dnsCreate
	return []dnsChange{c}, nil
}

// planTLSRPT: like DMARC, an existing TLS-RPT record is the owner's.
func planTLSRPT(c dnsChange, tr []dnsprovider.Record, run *dnsRun) []dnsChange {
	if len(tr) == 0 {
		c.Action = dnsCreate
		return []dnsChange{c}
	}
	want := c.Value
	c.Action, c.id, c.Value = dnsUnchanged, tr[0].ID, tr[0].Content
	if !dnsprovider.SameTXT(tr[0].Content, want) {
		c.Detail = "a TLS-RPT record already exists; it is left as it is"
		if _, addr, ok := strings.Cut(want, "rua="); ok && !mentions(tr[0].Content, strings.TrimPrefix(addr, "mailto:")) {
			run.warn("A TLS-RPT record already exists at %s (%q); OpenMailWire left it as it is. To see TLS reports in GET /domains/{id}/reports, add %s to its rua.",
				c.Name, tr[0].Content, addr)
		}
	}
	return []dnsChange{c}
}

// planInbound never touches an apex that already receives mail elsewhere.
func planInbound(c dnsChange, mx []dnsprovider.Record, d store.Domain, run *dnsRun) []dnsChange {
	for _, r := range mx {
		if r.Content == c.Value {
			c.Action, c.id, c.Priority = dnsUnchanged, r.ID, r.Priority
			return []dnsChange{c}
		}
	}
	if len(mx) > 0 {
		c.Action = dnsSkip
		c.Detail = "the domain already receives mail at " + hosts(mx) + "; its MX records are left as they are"
		run.warn("%s already receives mail at %s, so OpenMailWire did not add or change its MX records: the domain's own mail keeps working. "+
			"To give agents inboxes without touching it, add a subdomain such as agents.%s (POST /domains), set up its DNS, and create the inboxes there.",
			d.Name, hosts(mx), d.Name)
		return []dnsChange{c}
	}
	c.Action = dnsCreate
	return []dnsChange{c}
}

// checkZoneSettings reports the zone's settings that matter for mail.
func (s *Server) checkZoneSettings(ctx context.Context, p dnsprovider.Provider, cache *recordCache, zone dnsprovider.Zone, d store.Domain, run *dnsRun) error {
	if zone.Status != "" && zone.Status != "active" {
		run.warn("The zone %s is %q at Cloudflare, not active: its records are not served until the domain's name servers point to Cloudflare.",
			zone.Name, zone.Status)
	}
	run.Zone.EmailRouting = p.EmailRouting(ctx, zone.ID)
	switch run.Zone.EmailRouting {
	case dnsprovider.StatusEnabled:
		msg := fmt.Sprintf("Cloudflare Email Routing is on for %s. It owns the zone's apex MX records, so mail to %s goes to Email Routing.", zone.Name, zone.Name)
		if s.Cfg.InboundMX != "" && dnsprovider.Canon(d.Name) == zone.Name {
			msg += fmt.Sprintf(" OpenMailWire inboxes on %s cannot receive mail while it is: use a subdomain such as agents.%s for agent inboxes, or turn Email Routing off.",
				d.Name, d.Name)
		}
		run.warn("%s", msg)
	case dnsprovider.StatusUnknown:
		if s.Cfg.InboundMX != "" {
			run.warn("Could not read whether Cloudflare Email Routing is on for %s (the token lacks Zone > Email Routing > Read). If it is, it owns the apex MX records.",
				zone.Name)
		}
	}
	run.Zone.DNSSEC = p.DNSSEC(ctx, zone.ID)
	if st := run.Zone.DNSSEC; st != dnsprovider.StatusUnknown && st != "active" {
		run.warn("DNSSEC is %s for %s. Turning it on (Cloudflare: DNS > Settings > DNSSEC) lets receivers trust these records, and is what DANE builds on.",
			st, zone.Name)
	}
	// Cloudflare's proxy carries only HTTP(S): a proxied mail host, or a
	// proxied record at one of our names, breaks mail.
	names := []string{}
	for _, c := range run.Changes {
		if !slices.Contains(names, c.Name) && zone.Contains(c.Name) {
			names = append(names, c.Name)
		}
	}
	for _, h := range []string{s.Cfg.BounceMX, s.Cfg.InboundMX} {
		if h = dnsprovider.Canon(h); h != "" && zone.Contains(h) && !slices.Contains(names, h) {
			names = append(names, h)
		}
	}
	for _, n := range names {
		recs, err := cache.at(ctx, n)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.Proxied && (r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME") {
				run.warn("%s %s is proxied by Cloudflare (orange cloud). Cloudflare's proxy carries only web traffic, so mail servers cannot reach SMTP there: "+
					"set it to DNS only.", r.Type, n)
			}
		}
	}
	return nil
}

// getDNSAuto shows whether a token is remembered (never the token) and the
// last run.
func (s *Server) getDNSAuto(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	a, err := s.Store.DNSAutomation(r.Context(), d.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "dns automation", err)
		return
	}
	writeJSON(w, http.StatusOK, dnsAutoView(d, a))
}

func dnsAutoView(d store.Domain, a store.DNSAutomation) map[string]any {
	out := map[string]any{"object": "dns_automation", "domain_id": d.ID, "domain": d.Name, "remembered": a.Token != nil,
		"remembered_at": a.RememberedAt, "provider": nil, "zone": nil, "last_run": nil, "last_run_at": a.LastRunAt}
	if a.DomainID != "" {
		out["provider"] = a.Provider
		if a.ZoneID != "" {
			out["zone"] = map[string]string{"id": a.ZoneID, "name": a.ZoneName}
		}
	}
	if len(a.LastRun) > 0 {
		out["last_run"] = json.RawMessage(a.LastRun)
	}
	return out
}

// forgetDNSAuto removes the remembered token. Records already published
// stay; only automatic changes stop.
func (s *Server) forgetDNSAuto(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDomain(w, r)
	if !ok {
		return
	}
	forgot, err := s.Store.ForgetDNSToken(r.Context(), d.ID)
	if err != nil {
		s.internal(w, "forget dns token", err)
		return
	}
	if forgot {
		s.audit(r, "dns.token_forgotten", "domain", d.ID, map[string]any{"name": d.Name})
	}
	a, err := s.Store.DNSAutomation(r.Context(), d.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "dns automation", err)
		return
	}
	out := dnsAutoView(d, a)
	out["forgotten"] = forgot
	writeJSON(w, http.StatusOK, out)
}

// dnsAutoPublish publishes some of d's records (by kind) with the
// remembered token: the hook DKIM rotation and Ed25519 call after making a
// key. It returns nil when no token is remembered. It never fails its
// caller: a problem is in the result.
func (s *Server) dnsAutoPublish(r *http.Request, d store.Domain, trigger string, kinds ...string) map[string]any {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	a, err := s.Store.DNSAutomation(ctx, d.ID)
	if err != nil || a.Token == nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.Log.Error("dns automation", "domain", d.Name, "err", err)
		}
		return nil
	}
	lock := s.dnsLock(d.ID)
	lock.Lock()
	defer lock.Unlock()
	out := map[string]any{"provider": a.Provider, "zone": map[string]string{"id": a.ZoneID, "name": a.ZoneName}}
	raw, err := s.Sealer.Open(a.Token)
	if err != nil {
		s.Log.Error("open remembered dns token", "domain", d.Name, "err", err)
		out["published"], out["error"], out["message"] = false, "internal_error", "the remembered token could not be read; publish the record by hand"
		return out
	}
	only := map[string]bool{}
	for _, k := range kinds {
		only[k] = true
	}
	run, err := s.runDNS(ctx, s.newDNSProvider(string(raw)), d, dnsRunOptions{ZoneID: a.ZoneID, Only: only, Trigger: trigger})
	if err != nil {
		pe := s.dnsError(err, d, true)
		out["published"], out["error"], out["message"] = false, pe.Name, pe.Message+" Publish the record by hand meanwhile."
		return out
	}
	run.Remembered = true
	if err := s.saveDNSRun(ctx, d, &run, nil); err != nil {
		s.Log.Error("save dns run", "domain", d.Name, "err", err)
	}
	s.audit(r, "dns.applied", "domain", d.ID, dnsAuditData(run, "remembered"))
	published := true
	for _, c := range run.Changes {
		published = published && c.Action != dnsSkip
	}
	out["published"], out["changes"], out["warnings"] = published, run.Changes, run.Warnings
	return out
}
