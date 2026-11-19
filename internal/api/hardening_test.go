package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
)

// Audit R1#10: a subject that already starts with "Re:" was never
// shortened, so a reply to it was refused (subject over 998 bytes).
func TestAuditR1ReplySubjectTooLong(t *testing.T) {
	for _, in := range []string{"Re: " + strings.Repeat("x", 1200), strings.Repeat("y", 1200), "RE: " + strings.Repeat("ب", 700)} {
		out := replySubject(in)
		if _, pe := validateSend(sendRequest{From: "a@b.test", To: strList{"c@d.test"}, Subject: out, Text: "t"}, time.Now(), time.Hour); pe != nil {
			t.Errorf("replySubject gave %d bytes and the reply is refused: %s", len(out), pe.Message)
		}
		if !strings.HasPrefix(strings.ToLower(out), "re: ") {
			t.Errorf("no Re: prefix: %.20q", out)
		}
	}
	if got := replySubject("Re: hi"); got != "Re: hi" {
		t.Fatalf("got %q", got)
	}
}

func TestFieldName(t *testing.T) {
	for line, want := range map[string]string{
		"From: a\r\n":     "From",
		"X-Custom:\r\n":   "X-Custom",
		"no colon\r\n":    "",
		": empty\r\n":     "",
		"Bad Name: x\r\n": "",
	} {
		got, err := fieldName([]byte(line))
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", line, got, err, want)
		}
	}
	for _, line := range []string{"From : a\r\n", "From\t: a\r\n", "To  : b\r\n"} {
		if _, err := fieldName([]byte(line)); err == nil || !strings.Contains(err.Error(), "whitespace before its colon") {
			t.Errorf("%q: %v", line, err)
		}
	}
}

func TestDecodeStrict(t *testing.T) {
	var req sendRequest
	for body, want := range map[string]string{
		`{"from":"a@b.c","to":"x@y.z"}`:                         "",
		`{"FROM":"a@b.c"}`:                                      "", // encoding/json matches names ignoring case
		`{"from":"a@b.c","bogus":1}`:                            `unknown field "bogus"`,
		`{"attachments":[{"filename":"a","content":"eA=="}]}`:   "",
		`{"attachments":[{"filename":"a","contents":"eA=="}]}`:  `unknown field "contents"`,
		`{"tags":[{"name":"a","value":"b","x":{"y":[1,"}"]}}]}`: `unknown field "x"`,
		`{"headers":{"X-Anything":"ok"}}`:                       "",
		`{"to":["a@b.c"]} trailing`:                             "invalid character",
		`{"subject":"\"quoted\" \\ \u0041"}`:                    "",
		`{"b\u006fgus":1}`:                                      `unknown field "bogus"`,
	} {
		req = sendRequest{}
		err := decodeStrict([]byte(body), &req)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}
	// Embedded fields (keyAccess) count as the outer struct's.
	var key struct {
		Name string `json:"name"`
		keyAccess
	}
	if err := decodeStrict([]byte(`{"name":"x","allowed_ips":null,"expires_at":"2030-01-01T00:00:00Z"}`), &key); err != nil {
		t.Fatal(err)
	}
	if !key.AllowedIPs.isNull() || key.AllowedIPs == nil || string(key.ExpiresAt) != `"2030-01-01T00:00:00Z"` {
		t.Fatalf("%+v", key)
	}
}

func TestDecodeContent(t *testing.T) {
	for in, want := range map[string]string{
		`"aGVsbG8="`:      "hello",
		`"aGVsbG8"`:       "hello",
		`"aGVs\nbG8="`:    "hello",
		`"aGVs bG8="`:     "hello",
		`"aGVs\u0062G8="`: "hello",
	} {
		got, err := decodeContent(jsonSpan(in))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{`null`, `""`, ``} {
		if _, err := decodeContent(jsonSpan(in)); err != errNoContent {
			t.Errorf("%s: %v", in, err)
		}
	}
	if _, err := decodeContent(jsonSpan(`123`)); err == nil {
		t.Error("a number was accepted")
	}
}

func TestToCRLFAndRenderDoNotCopy(t *testing.T) {
	msg := []byte("From: a@acme.test\r\nTo: b@example.test\r\nDate: Tue, 17 Nov 2026 09:00:00 +0100\r\nMessage-ID: <x@acme.test>\r\n\r\nhi\r\n")
	if got := toCRLF(msg); &got[0] != &msg[0] {
		t.Fatal("toCRLF copied a CRLF message")
	}
	m, err := parseRaw(msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.render("unused", time.Now()); &got[0] != &msg[0] {
		t.Fatal("render copied a message it does not change")
	}
	if got := string(toCRLF([]byte("a\nb\rc"))); got != "a\r\nb\r\nc\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestKeyLimiter(t *testing.T) {
	l := newKeyLimiter(config.APILimits{KeyRate: 2, KeyBurst: 3})
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("k", now); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	ok, wait := l.allow("k", now)
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("4th: %v %s", ok, wait)
	}
	if ok, _ := l.allow("other", now); !ok {
		t.Fatal("another key shares the bucket")
	}
	if ok, _ := l.allow("k", now.Add(500*time.Millisecond)); !ok {
		t.Fatal("not refilled after 0.5 s")
	}
	pe := l.refusal(300 * time.Millisecond)
	if pe.Status != 429 || pe.Name != "rate_limit_exceeded" || pe.RetryAfter != time.Second {
		t.Fatalf("%+v", pe)
	}
	// Idle keys are forgotten.
	l.allow("k", now.Add(time.Hour))
	if len(l.buckets) != 1 {
		t.Fatalf("%d buckets", len(l.buckets))
	}
	if newKeyLimiter(config.APILimits{}) != nil {
		t.Fatal("a zero rate must turn the limit off")
	}
}

func TestClientIP(t *testing.T) {
	s := &Server{Cfg: config.Control{APILimits: config.APILimits{TrustedProxies: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}}}}
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.7:5000", "", "203.0.113.7"},
		{"203.0.113.7:5000", "198.51.100.1", "203.0.113.7"}, // not a trusted proxy: its header is ignored
		{"10.1.2.3:5000", "", "10.1.2.3"},
		{"10.1.2.3:5000", "198.51.100.1", "198.51.100.1"},
		{"10.1.2.3:5000", "6.6.6.6, 198.51.100.1, 10.9.9.9", "198.51.100.1"},
		{"[::1]:5000", "2001:db8::5", "2001:db8::5"},
		{"[::ffff:203.0.113.7]:5000", "", "203.0.113.7"},
		{"10.1.2.3:5000", "nonsense", "invalid IP"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientIP(r).String(); got != c.want {
			t.Errorf("%s via %q: %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	// An in-process hop takes the outer client's address, whatever its own.
	r := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(context.WithValue(r.Context(), hopKey{}, true), clientIPKey{}, netip.MustParseAddr("192.0.2.9"))
	if got := s.clientIP(r.WithContext(ctx)); got.String() != "192.0.2.9" {
		t.Fatalf("hop: %s", got)
	}
}

// Requests with bodies over 1 MB need a slot; one that cannot get one in
// time answers 503 server_busy, and in-process hops never wait.
func TestLargeBodySlots(t *testing.T) {
	defer func(d time.Duration) { slotWait = d }(slotWait)
	slotWait = 200 * time.Millisecond
	s := &Server{Log: slog.New(slog.DiscardHandler), large: newSlots(1)}
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	h := s.limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		if _, ok := readBody(w, r); !ok {
			return
		}
		if r.Header.Get("X-Hold") != "" {
			<-release
		}
		w.WriteHeader(200)
	}))
	big := strings.Repeat("x", largeBody+1)

	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r := httptest.NewRequest("POST", "/emails", strings.NewReader(big))
		r.Header.Set("X-Hold", "1")
		h.ServeHTTP(first, r)
		close(done)
	}()
	<-entered

	busy := httptest.NewRecorder()
	h.ServeHTTP(busy, httptest.NewRequest("POST", "/emails", strings.NewReader(big)))
	if busy.Code != 503 || !strings.Contains(busy.Body.String(), "server_busy") || busy.Header().Get("Retry-After") == "" {
		t.Fatalf("second large request: %d %s", busy.Code, busy.Body)
	}
	// A chunked body takes its slot once it passes 1 MB.
	chunked := httptest.NewRequest("POST", "/emails", io.MultiReader(strings.NewReader(big)))
	chunked.ContentLength = -1
	busy = httptest.NewRecorder()
	h.ServeHTTP(busy, chunked)
	if busy.Code != 503 {
		t.Fatalf("chunked large request: %d %s", busy.Code, busy.Body)
	}
	<-entered
	// Small bodies and in-process hops go straight through.
	small := httptest.NewRecorder()
	h.ServeHTTP(small, httptest.NewRequest("POST", "/emails", strings.NewReader("{}")))
	<-entered
	if small.Code != 200 {
		t.Fatalf("small request: %d", small.Code)
	}
	hop := httptest.NewRequest("POST", "/emails", strings.NewReader(big))
	hop = hop.WithContext(context.WithValue(hop.Context(), hopKey{}, true))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hop)
	<-entered
	if rec.Code != 200 {
		t.Fatalf("hop: %d", rec.Code)
	}
	close(release)
	<-done
	if first.Code != 200 {
		t.Fatalf("first: %d", first.Code)
	}
}
