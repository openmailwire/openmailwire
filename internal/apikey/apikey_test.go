package apikey

import (
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, addr string
		want          bool
	}{
		{"ops@example.org", "ops@example.org", true},
		{"ops@example.org", "ops2@example.org", false},
		{"*@acme.com", "anyone@acme.com", true},
		{"*@acme.com", "anyone@sub.acme.com", false},
		{"*@acme.com", "anyone@acme.com.evil.test", false},
		{"*@acme.com", "anyone@evilacme.com", false},
		{"*@*.acme.com", "a@sub.acme.com", true},
		{"*@*.acme.com", "a@x.y.acme.com", true},
		{"*@*.acme.com", "a@acme.com", false},
		{"agent-*@acme.com", "agent-7@acme.com", true},
		{"agent-*@acme.com", "agent-@acme.com", true},
		{"agent-*@acme.com", "agent@acme.com", false},
		{"*bot*@acme.com", "my-bot-1@acme.com", true},
		{"a*b*c@x.test", "aXbYc@x.test", true},
		{"a*b*c@x.test", "aXbY@x.test", false},
		// "*" never crosses the "@".
		{"*@*", "a@b", true},
		{"a*@x.test", "a@y@x.test", false},
		{"*", "a@b", false},
		{"*@acme.com", "noat", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.addr); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.addr, got, c.want)
		}
	}
}

func TestGlobDoesNotBlowUp(t *testing.T) {
	p := strings.Repeat("*a", 50) + "b"
	s := strings.Repeat("a", 200)
	if glob(p, s) {
		t.Fatal("matched")
	}
}

func TestParseAndNormalize(t *testing.T) {
	p, err := Parse([]byte(`{"max_per_hour": 100, "allowed_recipients": [" *@ACME.com ", "*@acme.com", "ops@example.org"], "allowed_from": ["agent@acme.com"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if *p.MaxPerHour != 100 || p.MaxPerDay != nil {
		t.Fatalf("caps: %+v", p)
	}
	if strings.Join(p.AllowedRecipients, ",") != "*@acme.com,ops@example.org" {
		t.Fatalf("recipients not normalized: %v", p.AllowedRecipients)
	}
	if !p.AllowsSender("Agent@Acme.com") || p.AllowsSender("other@acme.com") {
		t.Fatal("allowed_from")
	}
	if r, bad := p.FirstDisallowedRecipient([]string{"a@acme.com", "x@evil.test", "y@evil.test"}); !bad || r != "x@evil.test" {
		t.Fatalf("first disallowed: %q %v", r, bad)
	}
	if _, bad := p.FirstDisallowedRecipient([]string{"a@acme.com", "ops@example.org"}); bad {
		t.Fatal("allowed recipients refused")
	}

	empty, err := Parse(nil)
	if err != nil || empty.HasCaps() || !empty.AllowsSender("x@y.z") {
		t.Fatalf("empty policy: %+v %v", empty, err)
	}
	if _, bad := empty.FirstDisallowedRecipient([]string{"x@y.z"}); bad {
		t.Fatal("empty policy refused a recipient")
	}

	for _, bad := range []string{
		`{"max_per_hour": 0}`,
		`{"max_per_day": -5}`,
		`{"max_per_day": 1.5}`,
		`{"max_per_minute": 5}`,
		`{"allowed_recipients": []}`,
		`{"allowed_recipients": ["acme.com"]}`,
		`{"allowed_from": ["a b@acme.com"]}`,
		`{"allowed_from": ["<a@acme.com>"]}`,
		`{"allowed_from": ["a@acme.com@x"]}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestDescribe(t *testing.T) {
	var many []string
	for i := 0; i < 12; i++ {
		many = append(many, "a@b")
	}
	if got := Describe(many); !strings.HasSuffix(got, "and 2 more") {
		t.Fatalf("describe: %q", got)
	}
}
