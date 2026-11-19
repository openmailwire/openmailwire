// Package smtplimit holds the connection and rate limits shared by the
// SMTP servers (inbound and submission): a listener that caps connections
// in total and per client address and bounds each one's lifetime and idle
// time, and small per-address counters and token buckets.
package smtplimit

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Config tunes a Listener.
type Config struct {
	MaxConns    int           // concurrent connections in total
	MaxPerIP    int           // concurrent connections from one address (an IPv6 /64); 0 = no cap
	MaxDuration time.Duration // a connection is closed this long after it was accepted
	// IdleBeforeActive, when set, bounds the wait for each command until
	// the session calls Activate (after MAIL, or after AUTH): a client that
	// connects and says nothing, or only EHLO, is dropped sooner than one
	// that is sending mail.
	IdleBeforeActive time.Duration
	Log              *slog.Logger
	What             string // "inbound" or "submission", for log lines
}

// Limits holds the connection counts shared by the listeners it wraps, so
// a server on two ports (587 and 465) has one budget.
type Limits struct {
	cfg   Config
	sem   chan struct{}
	mu    sync.Mutex
	perIP map[string]int
}

// NewLimits fills in defaults: 500 connections, 30 minutes each.
func NewLimits(cfg Config) *Limits {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 500
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = 30 * time.Minute
	}
	return &Limits{cfg: cfg, sem: make(chan struct{}, cfg.MaxConns), perIP: map[string]int{}}
}

// Wrap returns a listener that enforces the limits on l.
func (l *Limits) Wrap(ln net.Listener) net.Listener { return &Listener{Listener: ln, Limits: l} }

// Listener enforces its Limits on the connections it accepts.
type Listener struct {
	net.Listener
	*Limits
}

// NewListener wraps l with limits of its own.
func NewListener(l net.Listener, cfg Config) net.Listener { return NewLimits(cfg).Wrap(l) }

// Accept returns the next connection within the limits. Connections over
// a limit get "421 4.7.0" (per address) or "421 4.3.2" (in total) and are
// closed without reaching the SMTP server; the reply is written in the
// background, so a client that does not read cannot stall the listener.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key := Key(c.RemoteAddr())
		if !l.takeIP(key) {
			l.cfg.Log.Warn(l.cfg.What+" connection refused: too many connections from one address", "remote", key)
			go refuse(c, "421 4.7.0 too many connections from your address, try again later")
			continue
		}
		select {
		case l.sem <- struct{}{}:
		default:
			l.releaseIP(key)
			l.cfg.Log.Warn(l.cfg.What + " connection refused: too many connections")
			go refuse(c, "421 4.3.2 too many connections, try again later")
			continue
		}
		bc := &Conn{Conn: c, deadline: time.Now().Add(l.cfg.MaxDuration), idle: l.cfg.IdleBeforeActive,
			release: func() { <-l.sem; l.releaseIP(key) }}
		if l.cfg.IdleBeforeActive <= 0 {
			bc.active.Store(true)
		}
		bc.Conn.SetDeadline(bc.deadline)
		return bc, nil
	}
}

func refuse(c net.Conn, line string) {
	c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, line+"\r\n")
	c.Close()
}

func (l *Listener) takeIP(key string) bool {
	if l.cfg.MaxPerIP <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perIP[key] >= l.cfg.MaxPerIP {
		return false
	}
	l.perIP[key]++
	return true
}

func (l *Listener) releaseIP(key string) {
	if l.cfg.MaxPerIP <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perIP[key]--; l.perIP[key] <= 0 {
		delete(l.perIP, key)
	}
}

// Conn never lets a deadline move past the connection's end of life, nor,
// until Activate, a read deadline past the idle limit.
type Conn struct {
	net.Conn
	deadline time.Time
	idle     time.Duration
	active   atomic.Bool
	once     sync.Once
	release  func()
}

// Activate lifts the short idle limit: the session has started real work.
func (c *Conn) Activate() { c.active.Store(true) }

func (c *Conn) clamp(t time.Time) time.Time {
	if t.IsZero() || t.After(c.deadline) {
		return c.deadline
	}
	return t
}

func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	if !c.active.Load() {
		if limit := time.Now().Add(c.idle); t.IsZero() || t.After(limit) {
			t = limit
		}
	}
	return c.Conn.SetReadDeadline(c.clamp(t))
}

func (c *Conn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(c.clamp(t)) }

func (c *Conn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}

// Of finds the *Conn under a connection the SMTP server holds, which is a
// *tls.Conn after STARTTLS. It returns nil for any other connection.
func Of(c net.Conn) *Conn {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	bc, _ := c.(*Conn)
	return bc
}

// Key is the address limits are counted by: the IP, or for IPv6 its /64,
// since one host usually owns a whole /64.
func Key(a net.Addr) string {
	host := a.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return KeyOf(host)
}

// KeyOf is Key for an address written as text.
func KeyOf(host string) string {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.String()
	}
	p, _ := ip.Prefix(64)
	return p.String()
}

// maxKeys bounds the per-address tables; past it the oldest state is
// forgotten. A flood from many addresses then costs memory proportional to
// maxKeys, not to the number of addresses.
const maxKeys = 50000

// Window counts events per key within a sliding window.
type Window struct {
	Span time.Duration
	mu   sync.Mutex
	m    map[string][]time.Time
}

// Add records n events for key now and returns how many fall within the
// window.
func (w *Window) Add(key string, n int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if w.m == nil {
		w.m = map[string][]time.Time{}
	}
	if len(w.m) >= maxKeys {
		w.prune(now)
	}
	ts := w.trim(w.m[key], now)
	for i := 0; i < n && len(ts) < 1000; i++ {
		ts = append(ts, now)
	}
	w.m[key] = ts
	return len(ts)
}

// Count returns the events for key within the window.
func (w *Window) Count(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	ts := w.trim(w.m[key], time.Now())
	if len(ts) == 0 {
		delete(w.m, key)
	} else {
		w.m[key] = ts
	}
	return len(ts)
}

func (w *Window) trim(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > w.Span {
		i++
	}
	return ts[i:]
}

func (w *Window) prune(now time.Time) {
	for k, ts := range w.m {
		if ts = w.trim(ts, now); len(ts) == 0 {
			delete(w.m, k)
		}
	}
	if len(w.m) >= maxKeys {
		w.m = map[string][]time.Time{}
	}
}

// Limiter is a token bucket per key: Burst tokens, refilled at Rate per
// second.
type Limiter struct {
	Rate  float64
	Burst float64
	mu    sync.Mutex
	m     map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Allow takes a token for key, or reports that none is left.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.m == nil {
		l.m = map[string]*bucket{}
	}
	b := l.m[key]
	if b == nil {
		if len(l.m) >= maxKeys {
			for k, v := range l.m {
				if l.refill(v, now) >= l.Burst {
					delete(l.m, k)
				}
			}
			if len(l.m) >= maxKeys {
				l.m = map[string]*bucket{}
			}
		}
		b = &bucket{tokens: l.Burst, last: now}
		l.m[key] = b
	}
	b.tokens = l.refill(b, now)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *Limiter) refill(b *bucket, now time.Time) float64 {
	return min(l.Burst, b.tokens+now.Sub(b.last).Seconds()*l.Rate)
}
