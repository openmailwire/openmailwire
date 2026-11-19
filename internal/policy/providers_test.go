package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderForMX(t *testing.T) {
	r := DefaultRules()
	cases := map[string]string{
		"gmail-smtp-in.l.google.com.":              "google",
		"ASPMX.L.GOOGLE.COM":                       "google",
		"alt1.aspmx.l.google.com":                  "google",
		"aspmx2.googlemail.com":                    "google",
		"acme-com.mail.protection.outlook.com":     "microsoft",
		"hotmail-com.olc.protection.outlook.com.":  "microsoft",
		"mta5.am0.yahoodns.net":                    "yahoo",
		"mx01.mail.icloud.com":                     "apple",
		"mx.zoho.eu":                               "zoho",
		"in1-smtp.messagingengine.com":             "fastmail",
		"mx.example.net":                           "",
		"notgoogle.com":                            "", // a suffix matches whole labels only
		"google.com.evil.test":                     "",
		"":                                         "",
		"mx0a-001b2d01.pphosted.com":               "proofpoint",
		"eu-smtp-inbound-1.mimecast.com":           "mimecast",
		"mx00.gmx.net":                             "gmx",
		"mx1.comcast.net":                          "comcast",
		"mxs.mail.ru":                              "mailru",
		"mail.protonmail.ch":                       "proton",
		"mx.yandex.net":                            "yandex",
		"d123.ess.barracudanetworks.com":           "barracuda",
		"smtp.google.com":                          "google",
		"example-com.mail.eo.outlook.com":          "microsoft",
		"mx-aol.mail.gm0.yahoodns.net":             "yahoo",
		"mx1.mail.me.com":                          "apple",
		"mx.zohomail.com":                          "zoho",
		"mx.yandex.ru":                             "yandex",
		"mx-ha03.web.de":                           "gmx",
		"inbound-smtp.us-east-1.amazonaws.com":     "",
		"route1.mx.cloudflare.net":                 "",
		"mx.zoho.in":                               "zoho",
		"a.mx.mail.yahoo.com":                      "yahoo",
		"mx.hotmail.com":                           "microsoft",
		"mail.aol.com":                             "yahoo",
		"mx1.icloud.com":                           "apple",
		"in2-smtp.messagingengine.com":             "fastmail",
		"mx2.zoho.com":                             "zoho",
		"emx.mail.ru":                              "mailru",
		"mxext1.mailbox.org":                       "",
		"gmail.com":                                "",
		"google.com":                               "google",
		"outlook.com":                              "microsoft",
		"protection.outlook.com":                   "microsoft",
		"example.mail.protection.outlook.com.":     "microsoft",
		"mx-biz.mail.am0.yahoodns.net":             "yahoo",
		"aspmx3.googlemail.com":                    "google",
		"eu-smtp-inbound-2.mimecast.com":           "mimecast",
		"mx0b-001b2d01.pphosted.com":               "proofpoint",
		"mx-ha02.web.de":                           "gmx",
		"mx01.gmx.com":                             "gmx",
		"mx1.mail.icloud.com":                      "apple",
		"mx.example.com.google.com.attacker.test":  "",
		"in-smtp.messagingengine.com.example.test": "",
	}
	for host, want := range cases {
		if got := r.ProviderForMX(host); got != want {
			t.Errorf("ProviderForMX(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestRulesLimitsAndWarmup(t *testing.T) {
	r := DefaultRules()
	if r.Concurrency("google", 10) != 20 || r.Concurrency("example.com", 7) != 7 || r.Concurrency("zoho", 3) != 10 {
		t.Fatal("concurrency")
	}
	if c, ok := r.WarmupCap("google", 1); !ok || c != 50 {
		t.Fatalf("google day 1: %d %v", c, ok)
	}
	if c, ok := r.WarmupCap("google", 3); !ok || c != 200 {
		t.Fatalf("google day 3: %d %v", c, ok)
	}
	if _, ok := r.WarmupCap("google", r.WarmupDays()+1); ok {
		t.Fatal("past the schedule the IP is warm")
	}
	if _, ok := r.WarmupCap("zoho", 1); ok {
		t.Fatal("zoho has no warm-up by default")
	}
	if _, ok := r.WarmupCap("example.com", 1); ok {
		t.Fatal("a domain of no known provider has no warm-up")
	}
}

func TestLoadRules(t *testing.T) {
	if r, err := LoadRules(""); err != nil || r.Provider("google") == nil {
		t.Fatalf("built-in: %v", err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "rules.json")
	os.WriteFile(good, []byte(`{"default_warmup": [10, 20], "providers": [
		{"name": "acme-mail", "mx_suffixes": ["mx.acme.test"], "concurrency": 3, "warmup": "default"},
		{"name": "other", "mx_suffixes": ["other.test"], "warmup": [5, 5, 50]}]}`), 0o600)
	r, err := LoadRules(good)
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderForMX("in.mx.acme.test") != "acme-mail" || r.Concurrency("acme-mail", 10) != 3 || r.Concurrency("other", 10) != 10 {
		t.Fatal("custom rules not applied")
	}
	if c, _ := r.WarmupCap("acme-mail", 2); c != 20 {
		t.Fatalf("default warm-up: %d", c)
	}
	if c, _ := r.WarmupCap("other", 3); c != 50 {
		t.Fatalf("own warm-up: %d", c)
	}
	for name, body := range map[string]string{
		"bad name":        `{"providers": [{"name": "Acme Mail", "mx_suffixes": ["a.test"]}]}`,
		"no suffix":       `{"providers": [{"name": "a", "mx_suffixes": []}]}`,
		"bad suffix":      `{"providers": [{"name": "a", "mx_suffixes": ["not a host"]}]}`,
		"twice":           `{"providers": [{"name": "a", "mx_suffixes": ["a.test"]}, {"name": "a", "mx_suffixes": ["b.test"]}]}`,
		"shrinking":       `{"providers": [{"name": "a", "mx_suffixes": ["a.test"], "warmup": [10, 5]}]}`,
		"unknown field":   `{"providers": [], "speed": "fast"}`,
		"negative":        `{"providers": [{"name": "a", "mx_suffixes": ["a.test"], "concurrency": -1}]}`,
		"warmup word":     `{"providers": [{"name": "a", "mx_suffixes": ["a.test"], "warmup": "fast"}]}`,
		"not json":        `providers: none`,
		"default zero":    `{"default_warmup": [0], "providers": []}`,
		"warmup nonsense": `{"providers": [{"name": "a", "mx_suffixes": ["a.test"], "warmup": [0]}]}`,
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := LoadRules(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadRules(filepath.Join(dir, "missing.json")); err == nil || !strings.Contains(err.Error(), "OMW_PROVIDER_RULES") {
		t.Errorf("missing file: %v", err)
	}
}
