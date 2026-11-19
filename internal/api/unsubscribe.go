package api

// One-click unsubscribe (RFC 8058) for broadcast mail (lane B).
//
// A broadcast email carries List-Unsubscribe: <OMW_PUBLIC_URL/u/{token}>
// and List-Unsubscribe-Post: List-Unsubscribe=One-Click. The token names
// the project and the recipient and is signed, so no lookup table is
// needed. A POST suppresses the address for broadcast mail at once; a GET
// only shows a page with a confirm button, because link scanners and
// previews follow links.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/store"
)

func (s *Server) unsubKey() []byte { return secret.Derive(s.MACKey, "openmailwire/unsubscribe") }

// unsubToken is base64url(project id || address) "." base64url(MAC[:16]).
func (s *Server) unsubToken(projectID, address string) string {
	pid := uuidBytes(projectID)
	payload := append(pid, strings.ToLower(address)...)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.unsubMAC(payload))
}

func (s *Server) unsubMAC(payload []byte) []byte {
	m := hmac.New(sha256.New, s.unsubKey())
	m.Write([]byte("unsubscribe/v1\x00"))
	m.Write(payload)
	return m.Sum(nil)[:16]
}

// parseUnsubToken checks a token and returns the project and address it
// names.
func (s *Server) parseUnsubToken(tok string) (projectID, address string, ok bool) {
	p, sig, found := strings.Cut(tok, ".")
	if !found || len(tok) > 1024 {
		return "", "", false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || len(payload) <= 16 || !hmac.Equal(mac, s.unsubMAC(payload)) {
		return "", "", false
	}
	return uuidString(payload[:16]), string(payload[16:]), true
}

func uuidBytes(id string) []byte {
	b := make([]byte, 16)
	h := strings.ReplaceAll(id, "-", "")
	for i := 0; i < 16 && 2*i+1 < len(h); i++ {
		v, _ := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		b[i] = byte(v)
	}
	return b
}

func uuidString(b []byte) string {
	const hex = "0123456789abcdef"
	var out strings.Builder
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out.WriteByte('-')
		}
		out.WriteByte(hex[c>>4])
		out.WriteByte(hex[c&15])
	}
	return out.String()
}

// maskAddress shows an address without revealing all of it: j***@example.com.
func maskAddress(a string) string {
	local, dom, ok := strings.Cut(a, "@")
	if !ok || local == "" {
		return "your address"
	}
	return local[:1] + "***@" + dom
}

var unsubTmpl = template.Must(template.New("u").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>{{.Title}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#0E1726}
button{font:inherit;padding:.6rem 1.2rem;border:0;border-radius:.4rem;background:#0E1726;color:#fff;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#0E1726;color:#e8edf3}button{background:#19D3A6;color:#0E1726}}</style>
</head><body><h1>{{.Title}}</h1><p>{{.Text}}</p>
{{if .Form}}<form method="post"><input type="hidden" name="List-Unsubscribe" value="One-Click"><button type="submit">Unsubscribe</button></form>{{end}}
</body></html>
`))

func writeUnsubPage(w http.ResponseWriter, status int, title, text string, form bool) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	var b bytes.Buffer
	unsubTmpl.Execute(&b, map[string]any{"Title": title, "Text": text, "Form": form})
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

// unsubAllowed applies the per-IP limit, answering 429 when it is used up.
func (s *Server) unsubAllowed(w http.ResponseWriter, r *http.Request) bool {
	s.safety.init()
	wait := s.safety.unsub.take(clientIP(r), time.Now())
	if wait <= 0 {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	writeUnsubPage(w, http.StatusTooManyRequests, "Too many requests", "Please try again in a minute.", false)
	return false
}

// unsubscribePage answers GET /u/{token}: a confirm button, never an
// unsubscribe.
func (s *Server) unsubscribePage(w http.ResponseWriter, r *http.Request) {
	if !s.unsubAllowed(w, r) {
		return
	}
	_, addr, ok := s.parseUnsubToken(r.PathValue("token"))
	if !ok {
		writeUnsubPage(w, http.StatusNotFound, "Link not valid", "This unsubscribe link is not valid. Copy the whole link from the email, or reply to the sender.", false)
		return
	}
	writeUnsubPage(w, http.StatusOK, "Unsubscribe", "Stop newsletters and other bulk email to "+maskAddress(addr)+" from this sender? Messages you ask for, like receipts, still arrive.", true)
}

// unsubscribeNow answers POST /u/{token}, sent by mailbox providers (RFC
// 8058) or by the confirm page: the address is suppressed for broadcast
// mail at once. Repeating it changes nothing.
func (s *Server) unsubscribeNow(w http.ResponseWriter, r *http.Request) {
	if !s.unsubAllowed(w, r) {
		return
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 4<<10)) // the body is List-Unsubscribe=One-Click; nothing else is needed
	pid, addr, ok := s.parseUnsubToken(r.PathValue("token"))
	if !ok {
		writeUnsubPage(w, http.StatusNotFound, "Link not valid", "This unsubscribe link is not valid. Copy the whole link from the email, or reply to the sender.", false)
		return
	}
	_, created, err := s.Store.AddSuppression(r.Context(), pid, store.Suppression{
		Address: addr, Stream: store.SuppressBroadcast, Reason: "unsubscribe", Detail: "one-click unsubscribe (RFC 8058)",
	})
	if errors.Is(err, store.ErrNotFound) {
		writeUnsubPage(w, http.StatusNotFound, "Link not valid", "This sender no longer exists, so nothing more will be sent.", false)
		return
	}
	if err != nil {
		s.Log.Error("unsubscribe", "err", err)
		writeUnsubPage(w, http.StatusInternalServerError, "Please try again", "Something went wrong; try again in a moment.", false)
		return
	}
	s.Log.Info("unsubscribed", "project", pid, "rcpt_domain", domainOf(addr), "new", created)
	writeUnsubPage(w, http.StatusOK, "You are unsubscribed", maskAddress(addr)+" will not get newsletters or other bulk email from this sender any more.", false)
}

// clientIP is the address the request came from (RemoteAddr: proxies in
// front of the control plane share one budget).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Mailbox providers send one-click POSTs from a few addresses, so the limit
// is generous; it stops floods, not people.
const (
	unsubRate  = 2.0 // tokens per second
	unsubBurst = 120
)

// ipLimiter is a token bucket per client IP.
type ipLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newIPLimiter(rate float64, burst int) *ipLimiter {
	return &ipLimiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// take spends one token for ip and returns 0, or how long until one is
// available.
func (l *ipLimiter) take(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 10000 {
		for k, b := range l.buckets { // forget full buckets
			if now.Sub(b.at).Seconds()*l.rate+b.tokens >= l.burst {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[ip] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}
