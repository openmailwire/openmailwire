package domains

import (
	"context"
	"net"
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
		},
		mx: map[string][]*net.MX{"bounce.acme.test": {{Host: "mx.openmailwire.test.", Pref: 10}}},
	}
	recs, ok := Check(context.Background(), good, spec)
	if !ok {
		t.Fatalf("expected verified: %+v", recs)
	}
	for _, r := range recs {
		if r.Record == "DMARC" && r.Status != StatusMissing {
			t.Fatalf("DMARC should be missing but optional: %+v", r)
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
	}}
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
	dns.mx = map[string][]*net.MX{"acme.test": {{Host: "mx.openmailwire.test.", Pref: 10}}}
	recs, _ = Check(context.Background(), dns, spec)
	for _, r := range recs {
		if r.Record == "INBOUND" && r.Status != StatusVerified {
			t.Fatalf("inbound MX published but %+v", r)
		}
	}
}
