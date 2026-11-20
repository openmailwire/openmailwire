package dnsprovider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/dnsprovider"
	"github.com/openmailwire/openmailwire/internal/dnsprovider/cloudflaretest"
)

const token = "cf-test-token-0123456789abcdefghijklmnopqrstuv"

// client returns a client for the fake that records its waits instead of
// sleeping.
func client(t *testing.T, cf *cloudflaretest.Server, tok string) (*dnsprovider.Cloudflare, *[]time.Duration) {
	t.Helper()
	c := dnsprovider.NewCloudflare(cf.URL, tok)
	var mu sync.Mutex
	waits := &[]time.Duration{}
	c.SetSleep(func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*waits = append(*waits, d)
		mu.Unlock()
		return ctx.Err()
	})
	return c, waits
}

func fake(t *testing.T) *cloudflaretest.Server {
	t.Helper()
	cf := cloudflaretest.New(token)
	t.Cleanup(cf.Close)
	return cf
}

func TestVerifyToken(t *testing.T) {
	ctx := context.Background()
	cf := fake(t)
	c, _ := client(t, cf, token)
	if err := c.VerifyToken(ctx); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	bad, _ := client(t, cf, "wrong-token-wrong-token-wrong-token-0000")
	if err := bad.VerifyToken(ctx); !errors.Is(err, dnsprovider.ErrAuth) {
		t.Fatalf("wrong token: %v, want ErrAuth", err)
	}
	cf.TokenStatus = "disabled"
	if err := c.VerifyToken(ctx); !errors.Is(err, dnsprovider.ErrAuth) || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled token: %v", err)
	}
}

func TestFindZoneWalksUp(t *testing.T) {
	ctx := context.Background()
	cf := fake(t)
	zone := cf.AddZone("example.com")
	cf.AddZone("other.example.org")
	c, _ := client(t, cf, token)

	z, err := c.FindZone(ctx, "Agents.Mail.Example.COM.")
	if err != nil || z.ID != zone.ID || z.Name != "example.com" || z.Status != "active" {
		t.Fatalf("FindZone: %+v %v", z, err)
	}
	var asked []string
	for _, r := range cf.Requests() {
		if strings.HasPrefix(r, "GET /zones?") {
			asked = append(asked, r)
		}
	}
	if len(asked) != 3 || !strings.Contains(asked[0], "name=agents.mail.example.com") ||
		!strings.Contains(asked[1], "name=mail.example.com") || !strings.Contains(asked[2], "name=example.com") {
		t.Fatalf("zone lookups: %v", asked)
	}
	if !z.Contains("bounce.agents.example.com") || z.Contains("example.com.evil.test") || z.Contains("notexample.com") {
		t.Fatal("Zone.Contains")
	}

	// The apex itself, and a name in no zone (the walk stops before "org").
	if z, err := c.FindZone(ctx, "example.com"); err != nil || z.ID != zone.ID {
		t.Fatalf("apex: %+v %v", z, err)
	}
	if _, err := c.FindZone(ctx, "mail.example.org"); !errors.Is(err, dnsprovider.ErrZoneNotFound) {
		t.Fatalf("no zone: %v", err)
	}
	if n := cf.Count("GET /zones?name=org"); n != 0 {
		t.Fatalf("looked up a top-level name %d times", n)
	}
	// A zone the token cannot see is not found.
	cf.SetZone("other.example.org", func(z *cloudflaretest.Zone) { z.Hidden = true })
	if _, err := c.FindZone(ctx, "x.other.example.org"); !errors.Is(err, dnsprovider.ErrZoneNotFound) {
		t.Fatalf("hidden zone: %v", err)
	}

	// By id.
	if z, err := c.Zone(ctx, zone.ID); err != nil || z.Name != "example.com" {
		t.Fatalf("Zone(id): %+v %v", z, err)
	}
	if _, err := c.Zone(ctx, strings.Repeat("0", 32)); !errors.Is(err, dnsprovider.ErrZoneNotFound) {
		t.Fatalf("unknown zone id: %v", err)
	}
	before := len(cf.Requests())
	if _, err := c.Zone(ctx, "../user/tokens/verify"); !errors.Is(err, dnsprovider.ErrZoneNotFound) {
		t.Fatalf("malformed zone id: %v", err)
	}
	if len(cf.Requests()) != before {
		t.Fatal("a malformed zone id reached the API")
	}
}

func TestRecords(t *testing.T) {
	ctx := context.Background()
	cf := fake(t)
	zone := cf.AddZone("example.com")
	c, _ := client(t, cf, token)

	// A 2048-bit DKIM record is longer than one TXT string.
	dkim := "v=DKIM1; k=rsa; p=" + strings.Repeat("A", 392)
	rec, err := c.Create(ctx, zone.ID, dnsprovider.Record{Type: "TXT", Name: "sel._domainkey.example.com", Content: dkim})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" || rec.Content != dkim {
		t.Fatalf("created %+v", rec)
	}
	stored := cf.Records("sel._domainkey.example.com")
	if len(stored) != 1 || !strings.HasPrefix(stored[0].Content, `"v=DKIM1; k=rsa; p=`) || !strings.Contains(stored[0].Content, `" "`) ||
		stored[0].Proxied || stored[0].Comment != dnsprovider.RecordComment {
		t.Fatalf("stored %+v", stored)
	}
	if got := cf.TXT("sel._domainkey.example.com"); len(got) != 1 || got[0] != dkim {
		t.Fatalf("resolved %q", got)
	}

	mx, err := c.Create(ctx, zone.ID, dnsprovider.Record{Type: "MX", Name: "bounce.example.com", Content: "MX.Mail.Example.NET.", Priority: 10})
	if err != nil || mx.Content != "mx.mail.example.net" || mx.Priority != 10 {
		t.Fatalf("MX: %+v %v", mx, err)
	}
	cf.AddRecord(cloudflaretest.Record{Type: "A", Name: "bounce.example.com", Content: "203.0.113.9", Proxied: true})
	got, err := c.Records(ctx, zone.ID, "Bounce.Example.com.")
	if err != nil || len(got) != 2 {
		t.Fatalf("records: %+v %v", got, err)
	}
	var sawProxied bool
	for _, r := range got {
		sawProxied = sawProxied || (r.Type == "A" && r.Proxied)
	}
	if !sawProxied {
		t.Fatalf("proxied flag lost: %+v", got)
	}

	mx.Content, mx.Priority = "mx2.mail.example.net", 20
	if up, err := c.Update(ctx, zone.ID, mx); err != nil || up.Content != "mx2.mail.example.net" || up.Priority != 20 {
		t.Fatalf("update: %+v %v", up, err)
	}
	if err := c.Delete(ctx, zone.ID, mx.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, zone.ID, mx.ID); err != nil {
		t.Fatalf("deleting a record that is gone: %v", err)
	}

	// The same record again is a duplicate, which callers can recognise.
	if _, err := c.Create(ctx, zone.ID, dnsprovider.Record{Type: "TXT", Name: "sel._domainkey.example.com", Content: dkim}); !dnsprovider.IsDuplicate(err) {
		t.Fatalf("duplicate create: %v", err)
	}

	// More records at one name than fit on a page.
	for i := range 130 {
		cf.AddRecord(cloudflaretest.Record{Type: "TXT", Name: "many.example.com", Content: fmt.Sprintf("n=%d", i)})
	}
	if many, err := c.Records(ctx, zone.ID, "many.example.com"); err != nil || len(many) != 130 {
		t.Fatalf("paged records: %d %v", len(many), err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	cf := fake(t)
	zone := cf.AddZone("example.com")
	c, _ := client(t, cf, token)
	for _, tc := range []struct{ routing, want string }{
		{"enabled", dnsprovider.StatusEnabled},
		{"disabled", dnsprovider.StatusDisabled},
		{"forbidden", dnsprovider.StatusUnknown}, // 403: the token lacks Email Routing Read
	} {
		cf.SetZone("example.com", func(z *cloudflaretest.Zone) { z.EmailRouting = tc.routing })
		if got := c.EmailRouting(ctx, zone.ID); got != tc.want {
			t.Errorf("email routing %s: got %s, want %s", tc.routing, got, tc.want)
		}
	}
	for _, tc := range []struct{ dnssec, want string }{{"active", "active"}, {"disabled", "disabled"}, {"forbidden", dnsprovider.StatusUnknown}} {
		cf.SetZone("example.com", func(z *cloudflaretest.Zone) { z.DNSSEC = tc.dnssec })
		if got := c.DNSSEC(ctx, zone.ID); got != tc.want {
			t.Errorf("dnssec %s: got %s, want %s", tc.dnssec, got, tc.want)
		}
	}
}

func TestErrorsAndRetries(t *testing.T) {
	ctx := context.Background()
	cf := fake(t)
	zone := cf.AddZone("example.com")
	c, waits := client(t, cf, token)

	// 401 and 403.
	cf.Inject(cloudflaretest.Fault{Path: "/dns_records", Status: 401})
	if _, err := c.Records(ctx, zone.ID, "example.com"); !errors.Is(err, dnsprovider.ErrAuth) {
		t.Fatalf("401: %v", err)
	}
	cf.ReadOnly = true
	if _, err := c.Create(ctx, zone.ID, dnsprovider.Record{Type: "TXT", Name: "x.example.com", Content: "x"}); !errors.Is(err, dnsprovider.ErrAuth) {
		t.Fatalf("403 on a write: %v", err)
	}
	cf.ReadOnly = false

	// 429, then success: one wait of Retry-After.
	cf.Inject(cloudflaretest.Fault{Path: "/dns_records", Status: 429, RetryAfter: "2"})
	if _, err := c.Records(ctx, zone.ID, "example.com"); err != nil {
		t.Fatalf("after one 429: %v", err)
	}
	if len(*waits) != 1 || (*waits)[0] != 2*time.Second {
		t.Fatalf("waits %v, want [2s]", *waits)
	}
	// A 429 retries a create too: Cloudflare did not process it.
	*waits = nil
	cf.Inject(cloudflaretest.Fault{Method: "POST", Status: 429, RetryAfter: "1"})
	if _, err := c.Create(ctx, zone.ID, dnsprovider.Record{Type: "TXT", Name: "y.example.com", Content: "y"}); err != nil {
		t.Fatalf("create after a 429: %v", err)
	}
	if len(*waits) != 1 {
		t.Fatalf("waits %v", *waits)
	}

	// A Retry-After longer than MaxWait gives up at once, saying how long.
	*waits = nil
	cf.Inject(cloudflaretest.Fault{Status: 429, RetryAfter: "300"})
	_, err := c.Records(ctx, zone.ID, "example.com")
	var pe *dnsprovider.Error
	if !errors.As(err, &pe) || pe.Kind != dnsprovider.KindUnavailable || pe.RetryAfter != 300*time.Second || len(*waits) != 0 {
		t.Fatalf("long Retry-After: %v (waits %v)", err, *waits)
	}
	// 429 every time: a bounded number of attempts, with growing waits.
	*waits = nil
	before := len(cf.Requests())
	cf.Inject(cloudflaretest.Fault{Status: 429, Times: 10})
	if _, err := c.Records(ctx, zone.ID, "example.com"); !errors.Is(err, dnsprovider.ErrUnavailable) {
		t.Fatalf("429 forever: %v", err)
	}
	if n := len(cf.Requests()) - before; n != c.Retries+1 {
		t.Fatalf("%d attempts, want %d", n, c.Retries+1)
	}
	if len(*waits) != c.Retries || (*waits)[1] <= (*waits)[0] {
		t.Fatalf("waits %v", *waits)
	}
	cf.ClearFaults()

	// 5xx: reads are retried, creates are not (one may have happened).
	*waits = nil
	cf.Inject(cloudflaretest.Fault{Path: "/dns_records", Method: "GET", Status: 502})
	if _, err := c.Records(ctx, zone.ID, "example.com"); err != nil {
		t.Fatalf("GET after one 502: %v", err)
	}
	before = len(cf.Requests())
	cf.Inject(cloudflaretest.Fault{Method: "POST", Status: 500})
	if _, err := c.Create(ctx, zone.ID, dnsprovider.Record{Type: "TXT", Name: "z.example.com", Content: "z"}); !errors.Is(err, dnsprovider.ErrUnavailable) {
		t.Fatalf("POST 500: %v", err)
	}
	if n := len(cf.Requests()) - before; n != 1 {
		t.Fatalf("a failed create was sent %d times", n)
	}
	cf.Inject(cloudflaretest.Fault{Status: 503, Times: 10})
	if _, err := c.Records(ctx, zone.ID, "example.com"); !errors.Is(err, dnsprovider.ErrUnavailable) {
		t.Fatalf("503 forever: %v", err)
	}
}

func TestTimeoutsAndTokenNeverShown(t *testing.T) {
	// A server that never answers: the caller's deadline wins.
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer stuck.Close()
	c := dnsprovider.NewCloudflare(stuck.URL, token)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.VerifyToken(ctx); !errors.Is(err, dnsprovider.ErrUnavailable) || time.Since(start) > 5*time.Second {
		t.Fatalf("stuck server: %v after %s", err, time.Since(start))
	}

	// A server (or a proxy in between) that echoes the token.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{
			{"code": 1234, "message": "bad header " + r.Header.Get("Authorization")}}})
	}))
	defer echo.Close()
	c = dnsprovider.NewCloudflare(echo.URL, token)
	err := c.VerifyToken(context.Background())
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[token]") {
		t.Fatalf("error %q", err)
	}
	// Redirects are not followed, so the token goes nowhere else.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect with %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	redirect := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	if err := dnsprovider.NewCloudflare(redirect.URL, token).VerifyToken(context.Background()); err == nil {
		t.Fatal("a redirect counted as success")
	}

	// Printing or logging the client never shows the token.
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("client", "c", c)
	js, _ := json.Marshal(c)
	for _, s := range []string{fmt.Sprintf("%v %+v %#v %s", c, c, c, c), b.String(), string(js)} {
		if strings.Contains(s, token) {
			t.Fatalf("token shown in %q", s)
		}
	}
}

func TestTXTHelpers(t *testing.T) {
	for in, want := range map[string]string{
		`v=spf1 -all`:                       "v=spf1 -all",
		`"v=spf1 -all"`:                     "v=spf1 -all",
		`"v=DKIM1; p=AAA" "BBB"`:            "v=DKIM1; p=AAABBB",
		`"a\"b" "c\\d"`:                     `a"bc\d`,
		`"unterminated`:                     `"unterminated`,
		`  plain text with "quotes" inside `: `plain text with "quotes" inside`,
	} {
		if got := dnsprovider.NormalizeTXT(in); got != want {
			t.Errorf("NormalizeTXT(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 600)
	q := dnsprovider.QuoteTXT(long)
	if strings.Count(q, `"`) != 6 || dnsprovider.NormalizeTXT(q) != long {
		t.Fatalf("QuoteTXT split %q", q)
	}
	if dnsprovider.QuoteTXT(`a"b`) != `"a\"b"` || dnsprovider.NormalizeTXT(dnsprovider.QuoteTXT(`a"b\`)) != `a"b\` {
		t.Fatal("QuoteTXT escaping")
	}
	for _, tc := range []struct {
		text, tag string
		want      bool
	}{
		{"v=spf1 include:x -all", "v=spf1", true},
		{"V=SPF1 -all", "v=spf1", true},
		{"v=spf10 -all", "v=spf1", false},
		{"v=DMARC1; p=none", "v=DMARC1", true},
		{"v = DMARC1 ; p=none", "v=DMARC1", true},
		{`"v=TLSRPTv1; rua=mailto:x@example.com"`, "v=TLSRPTv1", true},
		{"google-site-verification=abc", "v=spf1", false},
	} {
		if got := dnsprovider.HasTag(tc.text, tc.tag); got != tc.want {
			t.Errorf("HasTag(%q, %q) = %v", tc.text, tc.tag, got)
		}
	}
	if !dnsprovider.SameTXT(`"v=spf1  ip4:1.2.3.4 -all"`, "V=SPF1 ip4:1.2.3.4 -all") || dnsprovider.SameTXT("v=spf1 -all", "v=spf1 ~all") {
		t.Fatal("SameTXT")
	}
}

func TestSenderSPF(t *testing.T) {
	v, used := dnsprovider.SenderSPF([]string{"198.51.100.9", "203.0.113.7", "198.51.100.9", "2001:db8::25", "::ffff:192.0.2.1", "not-an-ip", ""})
	if v != "v=spf1 ip4:192.0.2.1 ip4:198.51.100.9 ip4:203.0.113.7 ip6:2001:db8::25 -all" || len(used) != 4 {
		t.Fatalf("%q %v", v, used)
	}
	if v, _ := dnsprovider.SenderSPF(nil); v != "v=spf1 -all" {
		t.Fatalf("%q", v)
	}
}

func TestSyncSPF(t *testing.T) {
	ctx := context.Background()
	cf := fake(t)
	cf.AddZone("example.net")
	c, _ := client(t, cf, token)
	zone, err := c.FindZone(ctx, "_spf.mail.example.net")
	if err != nil {
		t.Fatal(err)
	}
	name := "_spf.mail.example.net"
	v1 := "v=spf1 ip4:203.0.113.7 -all"
	ch, err := dnsprovider.SyncSPF(ctx, c, zone, name, v1, true)
	if err != nil || ch.Action != "create" || len(cf.Records(name)) != 0 {
		t.Fatalf("dry run: %+v %v", ch, err)
	}
	if ch, err = dnsprovider.SyncSPF(ctx, c, zone, name, v1, false); err != nil || ch.Action != "create" {
		t.Fatalf("create: %+v %v", ch, err)
	}
	writes := cf.Count("POST") + cf.Count("PATCH") + cf.Count("DELETE")
	if ch, err = dnsprovider.SyncSPF(ctx, c, zone, name, v1, false); err != nil || ch.Action != "unchanged" {
		t.Fatalf("again: %+v %v", ch, err)
	}
	if cf.Count("POST")+cf.Count("PATCH")+cf.Count("DELETE") != writes {
		t.Fatal("an unchanged record was written")
	}
	// Someone added a second SPF record and an unrelated TXT.
	cf.AddRecord(cloudflaretest.Record{Type: "TXT", Name: name, Content: `"v=spf1 ip4:192.0.2.66 -all"`})
	cf.AddRecord(cloudflaretest.Record{Type: "TXT", Name: name, Content: "note=keep me"})
	v2 := "v=spf1 ip4:198.51.100.9 ip4:203.0.113.7 -all"
	if ch, err = dnsprovider.SyncSPF(ctx, c, zone, name, v2, false); err != nil || ch.Action != "update" || ch.Deleted != 1 || len(ch.Old) != 2 {
		t.Fatalf("update: %+v %v", ch, err)
	}
	if got := cf.TXT(name); len(got) != 2 || got[0] != v2 || got[1] != "note=keep me" {
		t.Fatalf("after update: %q", got)
	}
}
