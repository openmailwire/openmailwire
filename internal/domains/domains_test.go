package domains

import (
	"context"
	"net"
	"strings"
	"testing"
)

type fakeDNS struct {
	txt map[string][]string
	mx  map[string][]*net.MX
}

func (f fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f fakeDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if v, ok := f.mx[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func TestCheck(t *testing.T) {
	spec := Spec{
		Domain: "acme.test", Selector: "pk1", DKIMPublic: "MIIBkey",
		ReturnPath: "bounce.acme.test", BounceMX: "mx.openmailwire.test",
		SPFMechanisms: []string{"ip4:203.0.113.7", "ip4:198.51.100.9"},
	}
	good := fakeDNS{
		txt: map[string][]string{
			"pk1._domainkey.acme.test": {"v=DKIM1; k=rsa; p=MIIB key"},
			"bounce.acme.test":         {"v=spf1 ip4:198.51.100.9 ip4:203.0.113.7 -all"},
			"_dmarc.acme.test":         {"v=DMARC1; p=reject; rua=mailto:d@acme.test"},
		},
		mx: map[string][]*net.MX{"bounce.acme.test": {{Host: "mx.openmailwire.test.", Pref: 10}}},
	}
	recs, ok := Check(context.Background(), good, spec)
	if !ok {
		t.Fatalf("expected verified: %+v", recs)
	}
	for _, r := range recs {
		if r.Status != StatusVerified || r.Detail != "" {
			t.Fatalf("%s: %s %q", r.Record, r.Status, r.Detail)
		}
	}

	bad := fakeDNS{txt: map[string][]string{
		"pk1._domainkey.acme.test": {"v=DKIM1; k=rsa; p=OTHER"},
		"bounce.acme.test":         {"v=spf1 ip4:203.0.113.7 -all", "v=spf1 -all"},
	}}
	recs, ok = Check(context.Background(), bad, spec)
	if ok {
		t.Fatal("expected not verified")
	}
	want := map[string]string{"DKIM": StatusFailed, "SPF": StatusFailed, "MX": StatusMissing}
	for _, r := range recs {
		if w, has := want[r.Record]; has && r.Status != w {
			t.Errorf("%s: got %s (%s), want %s", r.Record, r.Status, r.Detail, w)
		}
	}
}

func TestNormalize(t *testing.T) {
	if n, err := Normalize(" Mail.Example.COM. "); err != nil || n != "mail.example.com" {
		t.Fatalf("got %q %v", n, err)
	}
	for _, bad := range []string{"localhost", "-bad.com", "a..b", "exa mple.com", ""} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestInboundMX(t *testing.T) {
	spec := Spec{
		Domain: "acme.test", Selector: "pk1", DKIMPublic: "MIIBkey",
		ReturnPath: "bounce.acme.test", SPFMechanisms: []string{"ip4:203.0.113.7"},
	}
	for _, r := range Records(spec) {
		if r.Record == "INBOUND" {
			t.Fatal("INBOUND is shown only when OMW_INBOUND_MX is set")
		}
	}
	spec.InboundMX = "mx.openmailwire.test"
	dns := fakeDNS{txt: map[string][]string{
		"pk1._domainkey.acme.test": {"v=DKIM1; k=rsa; p=MIIBkey"},
		"bounce.acme.test":         {"v=spf1 ip4:203.0.113.7 -all"},
		"_dmarc.acme.test":         {"v=DMARC1; p=none"},
	}, mx: map[string][]*net.MX{"bounce.acme.test": {{Host: "mx.openmailwire.test.", Pref: 10}}}}
	// Without the MX the domain still verifies: inboxes are optional.
	recs, ok := Check(context.Background(), dns, spec)
	var inbound *Record
	for i := range recs {
		if recs[i].Record == "INBOUND" {
			inbound = &recs[i]
		}
	}
	if !ok || inbound == nil || inbound.Required || inbound.Type != "MX" || inbound.Name != "acme.test" ||
		inbound.Value != "mx.openmailwire.test" || inbound.Status != StatusMissing {
		t.Fatalf("verified=%v inbound=%+v", ok, inbound)
	}
	dns.mx["acme.test"] = []*net.MX{{Host: "mx.openmailwire.test.", Pref: 10}}
	recs, _ = Check(context.Background(), dns, spec)
	for _, r := range recs {
		if r.Record == "INBOUND" && r.Status != StatusVerified {
			t.Fatalf("inbound MX published but %+v", r)
		}
	}
}

func recordOf(t *testing.T, recs []Record, kind string) Record {
	t.Helper()
	for _, r := range recs {
		if r.Record == kind {
			return r
		}
	}
	t.Fatalf("no %s record in %+v", kind, recs)
	return Record{}
}

// v0.6: DMARC (at least p=none) is required, and its detail explains the
// Gmail, Yahoo and Microsoft rules until it is in place.
func TestDMARCRequired(t *testing.T) {
	spec := Spec{Domain: "mail.acme.test", Selector: "pk1", DKIMPublic: "K", ReturnPath: "bounce.mail.acme.test",
		SPFMechanisms: []string{"ip4:203.0.113.7"}}
	base := map[string][]string{
		"pk1._domainkey.mail.acme.test": {"v=DKIM1; k=rsa; p=K"},
		"bounce.mail.acme.test":         {"v=spf1 ip4:203.0.113.7 -all"},
	}
	with := func(extra map[string][]string) fakeDNS {
		txt := map[string][]string{}
		for k, v := range base {
			txt[k] = v
		}
		for k, v := range extra {
			txt[k] = v
		}
		return fakeDNS{txt: txt}
	}
	if !recordOf(t, Records(spec), "DMARC").Required {
		t.Fatal("DMARC must be required")
	}
	for _, c := range []struct {
		name     string
		dns      fakeDNS
		verified bool
		status   string
		detail   string
	}{
		{"missing", with(nil), false, StatusMissing, "Microsoft"},
		{"own record", with(map[string][]string{"_dmarc.mail.acme.test": {"v=DMARC1; p=quarantine"}}), true, StatusVerified, ""},
		{"p=none", with(map[string][]string{"_dmarc.mail.acme.test": {"v=DMARC1; p=none"}}), true, StatusVerified, "only monitors"},
		{"inherited from the parent", with(map[string][]string{"_dmarc.acme.test": {"v=DMARC1; p=reject"}}), true, StatusVerified, "inherited from _dmarc.acme.test"},
		{"parent sp= wins", with(map[string][]string{"_dmarc.acme.test": {"v=DMARC1; p=reject; sp=bogus"}}), false, StatusFailed, "no valid policy"},
		{"two records", with(map[string][]string{"_dmarc.mail.acme.test": {"v=DMARC1; p=none", "v=DMARC1; p=reject"}}), false, StatusFailed, "more than one"},
		{"no policy", with(map[string][]string{"_dmarc.mail.acme.test": {"v=DMARC1; rua=mailto:x@acme.test"}}), false, StatusFailed, "no valid policy"},
		{"other TXT only", with(map[string][]string{"_dmarc.mail.acme.test": {"google-site-verification=x"}}), false, StatusMissing, "no DMARC record"},
	} {
		t.Run(c.name, func(t *testing.T) {
			recs, ok := Check(context.Background(), c.dns, spec)
			d := recordOf(t, recs, "DMARC")
			if ok != c.verified || d.Status != c.status || !strings.Contains(d.Detail, c.detail) {
				t.Fatalf("verified=%v %+v", ok, d)
			}
		})
	}
}

// v0.6: the return path's MX is always shown; it is required when this
// server receives bounces (OMW_BOUNCE_MX, or else the inbound host).
func TestReturnPathMX(t *testing.T) {
	spec := Spec{Domain: "acme.test", Selector: "pk1", DKIMPublic: "K", ReturnPath: "bounce.acme.test",
		SPFMechanisms: []string{"ip4:203.0.113.7"}}
	txt := map[string][]string{
		"pk1._domainkey.acme.test": {"v=DKIM1; k=rsa; p=K"},
		"bounce.acme.test":         {"v=spf1 ip4:203.0.113.7 -all"},
		"_dmarc.acme.test":         {"v=DMARC1; p=none"},
	}
	// No bounce host: shown, optional, and checked for any MX.
	mx := recordOf(t, Records(spec), "MX")
	if mx.Required || mx.Name != "bounce.acme.test" || mx.Value != "" || !strings.Contains(mx.Detail, "OMW_BOUNCE_MX") {
		t.Fatalf("%+v", mx)
	}
	recs, ok := Check(context.Background(), fakeDNS{txt: txt}, spec)
	if !ok || recordOf(t, recs, "MX").Status != StatusMissing {
		t.Fatalf("verified=%v %+v", ok, recordOf(t, recs, "MX"))
	}
	recs, _ = Check(context.Background(), fakeDNS{txt: txt, mx: map[string][]*net.MX{"bounce.acme.test": {{Host: "mx.acme.test."}}}}, spec)
	if r := recordOf(t, recs, "MX"); r.Status != StatusVerified {
		t.Fatalf("%+v", r)
	}
	// An inbound host receives the bounces: the MX points there and is
	// required.
	spec.InboundMX = "in.openmailwire.test"
	mx = recordOf(t, Records(spec), "MX")
	if !mx.Required || mx.Value != "in.openmailwire.test" {
		t.Fatalf("%+v", mx)
	}
	if _, ok := Check(context.Background(), fakeDNS{txt: txt}, spec); ok {
		t.Fatal("verified without the return path MX")
	}
	// OMW_BOUNCE_MX wins over the inbound host.
	spec.BounceMX = "bounce.openmailwire.test"
	if mx = recordOf(t, Records(spec), "MX"); mx.Value != "bounce.openmailwire.test" {
		t.Fatalf("%+v", mx)
	}
}
