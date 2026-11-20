package dnsprovider

import (
	"context"
	"net/netip"
	"sort"
	"strings"
)

// SenderSPF builds the SPF record that authorizes exactly ips: "v=spf1
// ip4:… ip6:… -all", IPv4 first, each address once. Strings that are not
// IP addresses are left out. It also returns the addresses it used.
func SenderSPF(ips []string) (string, []string) {
	seen := map[netip.Addr]bool{}
	var v4, v6 []netip.Addr
	for _, s := range ips {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil || !a.IsValid() {
			continue
		}
		a = a.Unmap().WithZone("")
		if seen[a] {
			continue
		}
		seen[a] = true
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	sort.Slice(v4, func(i, j int) bool { return v4[i].Less(v4[j]) })
	sort.Slice(v6, func(i, j int) bool { return v6[i].Less(v6[j]) })
	parts := []string{"v=spf1"}
	var used []string
	for _, a := range v4 {
		parts = append(parts, "ip4:"+a.String())
		used = append(used, a.String())
	}
	for _, a := range v6 {
		parts = append(parts, "ip6:"+a.String())
		used = append(used, a.String())
	}
	return strings.Join(append(parts, "-all"), " "), used
}

// SPFChange is what SyncSPF did, or would do in a dry run.
type SPFChange struct {
	Name string
	Zone Zone
	// Old are the SPF texts that were at Name.
	Old []string
	New string
	// Action is "create", "update" or "unchanged".
	Action string
	// Deleted counts extra SPF records removed: a name with two SPF
	// records fails SPF at every receiver.
	Deleted int
}

// SyncSPF makes the SPF record at name read value: it creates it, updates
// the one that is there, and removes any second SPF record. It writes
// nothing when the record already reads value, or when dryRun is set.
func SyncSPF(ctx context.Context, p Provider, zone Zone, name, value string, dryRun bool) (SPFChange, error) {
	ch := SPFChange{Name: canon(name), Zone: zone, New: value}
	recs, err := p.Records(ctx, zone.ID, ch.Name)
	if err != nil {
		return ch, err
	}
	var spf []Record
	keep := -1
	for _, r := range recs {
		if r.Type != "TXT" || !HasTag(r.Content, "v=spf1") {
			continue
		}
		ch.Old = append(ch.Old, r.Content)
		if keep < 0 && SameTXT(r.Content, value) {
			keep = len(spf)
		}
		spf = append(spf, r)
	}
	switch {
	case len(spf) == 0:
		ch.Action = "create"
	case keep >= 0:
		ch.Action = "unchanged"
	default:
		ch.Action, keep = "update", 0
	}
	for i := range spf {
		if i != keep {
			ch.Deleted++
		}
	}
	if dryRun {
		return ch, nil
	}
	switch ch.Action {
	case "create":
		if _, err := p.Create(ctx, zone.ID, Record{Type: "TXT", Name: ch.Name, Content: value}); err != nil && !IsDuplicate(err) {
			return ch, err
		}
	case "update":
		if _, err := p.Update(ctx, zone.ID, Record{ID: spf[keep].ID, Type: "TXT", Name: ch.Name, Content: value}); err != nil {
			return ch, err
		}
	}
	for i, r := range spf {
		if i == keep {
			continue
		}
		if err := p.Delete(ctx, zone.ID, r.ID); err != nil {
			return ch, err
		}
	}
	return ch, nil
}
