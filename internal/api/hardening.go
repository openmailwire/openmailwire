package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
)

// ---- response headers ----

// secureHeaders marks every response nosniff and, unless the handler says
// otherwise (the docs and the dashboard set their own), never cacheable:
// API answers carry project data.
func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// ---- in-process hops ----

// Remote MCP tools and inbox replies call the API in process (inProcess,
// mcp.go). Such a hop already passed the outer request's checks: it skips
// the rate limit and the large-body slots, and is judged by the outer
// client's address.
type hopKey struct{}

// clientIPKey holds the netip.Addr of the client that made the outer
// request.
type clientIPKey struct{}

func isHop(ctx context.Context) bool { return ctx.Value(hopKey{}) != nil }

// hopTransport is inProcess for requests made on behalf of a client at ip.
type hopTransport struct {
	inner http.RoundTripper
	ip    netip.Addr
}

func (t hopTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.inner.RoundTrip(req.WithContext(context.WithValue(req.Context(), clientIPKey{}, t.ip)))
}

// clientIP is the address the request came from: the outer client's for an
// in-process hop; otherwise the connection's peer, or, when the peer is a
// trusted proxy (OMW_TRUSTED_PROXIES), the right-most address in
// X-Forwarded-For that is not one. The zero Addr when unknown.
func (s *Server) clientIP(r *http.Request) netip.Addr {
	if isHop(r.Context()) {
		ip, _ := r.Context().Value(clientIPKey{}).(netip.Addr)
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	ip = ip.Unmap().WithZone("")
	if !trusted(s.Cfg.TrustedProxies, ip) {
		return ip
	}
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return ip
	}
	hops := strings.Split(strings.Join(xff, ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{} // a garbled chain: trust none of it
		}
		a = a.Unmap().WithZone("")
		if !trusted(s.Cfg.TrustedProxies, a) {
			return a
		}
		ip = a
	}
	return ip
}

func trusted(proxies []netip.Prefix, ip netip.Addr) bool {
	for _, p := range proxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ---- large bodies ----

// largeBody is the size over which a request body needs a slot: the
// handlers that take bodies this large decode, render, sign and seal them,
// and each step holds a copy.
const largeBody = 1 << 20

// slotWait is how long a large request waits for a slot before 503.
var slotWait = 30 * time.Second

// errServerBusy is what a body reader returns when no slot came free.
var errServerBusy = errors.New("server busy")

type slots chan struct{}

func newSlots(n int) slots {
	if n <= 0 {
		n = config.DefaultLargeRequests
	}
	return make(slots, n)
}

// acquire waits for a slot, at most slotWait. release must be called once
// when ok.
func (s slots) acquire(ctx context.Context) (release func(), ok bool) {
	select {
	case s <- struct{}{}:
		return func() { <-s }, true
	default:
	}
	t := time.NewTimer(slotWait)
	defer t.Stop()
	select {
	case s <- struct{}{}:
		return func() { <-s }, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

// lazySlot takes a slot once a body of unknown length (chunked) passes
// largeBody.
type lazySlot struct {
	io.ReadCloser
	slots   slots
	ctx     context.Context
	n       int64
	release func()
	failed  bool
}

func (l *lazySlot) Read(p []byte) (int, error) {
	if l.failed {
		return 0, errServerBusy
	}
	n, err := l.ReadCloser.Read(p)
	l.n += int64(n)
	if l.release == nil && l.n > largeBody {
		release, ok := l.slots.acquire(l.ctx)
		if !ok {
			l.failed = true
			return n, errServerBusy
		}
		l.release = release
	}
	return n, err
}

func (l *lazySlot) done() {
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

func writeBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	writeError(w, http.StatusServiceUnavailable, "server_busy", fmt.Sprintf(
		"the server is already handling as many requests over %d MB as it allows (OMW_MAX_LARGE_REQUESTS); retry after 10 seconds with the same Idempotency-Key",
		largeBody>>20))
}

// ---- per-key rate limit ----

// keyLimiter is a token bucket per API key: rate tokens a second, at most
// burst saved up. One request takes one token.
type keyLimiter struct {
	rate  float64
	burst float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newKeyLimiter(l config.APILimits) *keyLimiter {
	if l.KeyRate <= 0 {
		return nil
	}
	burst := float64(l.KeyBurst)
	if burst < 1 {
		burst = math.Max(1, 2*l.KeyRate)
	}
	return &keyLimiter{rate: l.KeyRate, burst: burst, buckets: map[string]*bucket{}}
}

// allow takes a token for key, or says how long until one is free.
func (l *keyLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		// Forget keys whose bucket has filled up again: they are idle.
		full := time.Duration(l.burst / l.rate * float64(time.Second))
		for k, b := range l.buckets {
			if now.Sub(b.at) > full {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

func (l *keyLimiter) refusal(wait time.Duration) *policyError {
	secs := max(int(math.Ceil(wait.Seconds())), 1)
	return &policyError{Status: http.StatusTooManyRequests, Name: "rate_limit_exceeded", RetryAfter: time.Duration(secs) * time.Second,
		Message: fmt.Sprintf("this API key made more than %s requests per second (bursts of %d allowed); retry after %d s (see the Retry-After header), with the same Idempotency-Key for sends",
			strconv.FormatFloat(l.rate, 'f', -1, 64), int(l.burst), secs)}
}
