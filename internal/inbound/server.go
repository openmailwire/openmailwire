// Package inbound is the inbound SMTP role (openmailwire inbound): it
// receives mail from the internet for inboxes, bounce (VERP) addresses and
// the feedback-loop address, and hands it to the control plane.
//
// Every recipient is checked with the control plane at RCPT time, so mail
// for an unknown address is refused in the SMTP conversation (550 5.1.1)
// and OpenMailWire never has to send a bounce of its own (no backscatter).
// The node keeps no state: a message is acknowledged with 250 only after
// the control plane has stored it, otherwise the sender gets a temporary
// failure and retries.
package inbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/secret"
	"github.com/openmailwire/openmailwire/internal/smtplimit"
)

// Config tunes the SMTP server. Zero values take the defaults noted.
type Config struct {
	Hostname      string      // announced in the greeting and Received headers
	TLS           *tls.Config // enables STARTTLS
	MaxSize       int64       // largest message in bytes; default 25 MB
	MaxRecipients int         // per message; default 100
	MaxConns      int         // concurrent connections; default 500
	// ReadTimeout bounds each command and the whole DATA transfer;
	// default 10 minutes (RFC 5321 asks for at least 5 per command and 10
	// for DATA). WriteTimeout bounds each reply; default 1 minute.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// MaxConnDuration closes any connection after this long; default 30
	// minutes. It stops a client that trickles bytes from holding a slot.
	MaxConnDuration time.Duration
	// CheckTimeout and DeliverTimeout bound calls to the control plane;
	// defaults 15 seconds and 2 minutes.
	CheckTimeout   time.Duration
	DeliverTimeout time.Duration

	// Abuse limits.
	//
	// MaxConnsPerIP caps concurrent connections from one address (an IPv6
	// /64); default 20. IdleTimeout bounds the wait for each command until
	// the client sends MAIL; default 1 minute.
	MaxConnsPerIP int
	IdleTimeout   time.Duration
	// MaxUnknownRcpts closes a connection after this many refused
	// recipients; default 10. A sender with real mail rarely has more than
	// one or two bad addresses, and an address-guessing client is cut off
	// early.
	MaxUnknownRcpts int
	// RcptChecksPerMinute and RcptCheckBurst limit the recipient checks one
	// address (an IPv6 /64) may cause on the control plane; defaults 120
	// and 60. Cached answers are free.
	RcptChecksPerMinute int
	RcptCheckBurst      int
	// CacheTTL and NegativeCacheTTL keep recipient answers on the node, so
	// repeated mail to one address does not ask the control plane each
	// time; defaults 1 minute and 15 seconds. A negative answer is kept
	// briefly, so a new inbox starts receiving almost at once. CacheSize
	// bounds the entries; default 10000.
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	CacheSize        int
}

func (c *Config) setDefaults() {
	if c.Hostname == "" {
		c.Hostname = "localhost"
	}
	if c.MaxSize <= 0 {
		c.MaxSize = 25 << 20
	}
	if c.MaxRecipients <= 0 {
		c.MaxRecipients = 100
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 500
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 10 * time.Minute
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = time.Minute
	}
	if c.MaxConnDuration <= 0 {
		c.MaxConnDuration = 30 * time.Minute
	}
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = 15 * time.Second
	}
	if c.DeliverTimeout <= 0 {
		c.DeliverTimeout = 2 * time.Minute
	}
	if c.MaxConnsPerIP <= 0 {
		c.MaxConnsPerIP = 20
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = time.Minute
	}
	if c.MaxUnknownRcpts <= 0 {
		c.MaxUnknownRcpts = 10
	}
	if c.RcptChecksPerMinute <= 0 {
		c.RcptChecksPerMinute = 120
	}
	if c.RcptCheckBurst <= 0 {
		c.RcptCheckBurst = 60
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = time.Minute
	}
	if c.NegativeCacheTTL <= 0 {
		c.NegativeCacheTTL = 15 * time.Second
	}
	if c.CacheSize <= 0 {
		c.CacheSize = 10000
	}
}

// Server is an inbound SMTP server.
type Server struct {
	cfg    Config
	cp     *Client
	log    *slog.Logger
	smtp   *smtp.Server
	checks *smtplimit.Limiter // recipient checks per client address
	cache  *rcptCache
}

func New(cfg Config, cp *Client, log *slog.Logger) *Server {
	cfg.setDefaults()
	s := &Server{cfg: cfg, cp: cp, log: log,
		checks: &smtplimit.Limiter{Rate: float64(cfg.RcptChecksPerMinute) / 60, Burst: float64(cfg.RcptCheckBurst)},
		cache:  &rcptCache{max: cfg.CacheSize}}
	srv := smtp.NewServer(s)
	srv.Domain = cfg.Hostname
	srv.TLSConfig = cfg.TLS
	srv.MaxMessageBytes = cfg.MaxSize
	srv.MaxRecipients = cfg.MaxRecipients
	// RFC 5322 caps lines at 998 characters, but some senders write whole
	// HTML bodies on one line; accept them rather than lose the mail.
	srv.MaxLineLength = 64 << 10
	srv.ReadTimeout = cfg.ReadTimeout
	srv.WriteTimeout = cfg.WriteTimeout
	srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	s.smtp = srv
	return s
}

// Serve accepts connections on l until Close or Shutdown.
func (s *Server) Serve(l net.Listener) error {
	err := s.smtp.Serve(smtplimit.NewListener(l, smtplimit.Config{
		MaxConns: s.cfg.MaxConns, MaxPerIP: s.cfg.MaxConnsPerIP, MaxDuration: s.cfg.MaxConnDuration,
		IdleBeforeActive: s.cfg.IdleTimeout, Log: s.log, What: "inbound",
	}))
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting and waits for open conversations to finish.
func (s *Server) Shutdown(ctx context.Context) error { return s.smtp.Shutdown(ctx) }

// Close stops at once.
func (s *Server) Close() error { return s.smtp.Close() }

// NewSession is called by go-smtp after HELO/EHLO (and again after STARTTLS).
func (s *Server) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{s: s, conn: c}, nil
}

type session struct {
	s       *Server
	conn    *smtp.Conn
	from    string
	rcpts   []string
	kinds   map[string]string
	unknown int // recipients refused on this connection
}

func (ss *session) Reset() {
	ss.from, ss.rcpts, ss.kinds = "", nil, nil
}

func (ss *session) Logout() error { return nil }

func (ss *session) Mail(from string, _ *smtp.MailOptions) error {
	ss.Reset()
	ss.from = from
	if c := smtplimit.Of(ss.conn.Conn()); c != nil {
		c.Activate() // a transaction: commands may now take the full ReadTimeout
	}
	return nil
}

var (
	errNoSuchUser = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such recipient here"}
	errTempCheck  = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure checking the recipient; try again later"}
	errTempStore  = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure storing the message; try again later"}
	errRejected   = &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message rejected"}
	errLongLine   = &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "a line of the message is too long"}
	errSlowDown   = &smtp.SMTPError{Code: 450, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "too many recipient checks from your address; try again later"}
	errBusy       = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 2, 1}, Message: "the mailbox is receiving too much mail; try again later"}
)

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

func (ss *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	addr := strings.ToLower(strings.TrimSpace(to))
	if ss.kinds[addr] != "" {
		return nil // a repeated RCPT
	}
	res, cached := ss.s.cache.get(addr)
	if !cached {
		// Each check costs the control plane a request and a query, so
		// one address may not cause them faster than the limit.
		if !ss.s.checks.Allow(smtplimit.KeyOf(ss.remoteIP())) {
			ss.s.log.Warn("inbound recipient checks rate-limited", "remote_ip", ss.remoteIP())
			return ss.refused(errSlowDown)
		}
		ctx, cancel := context.WithTimeout(context.Background(), ss.s.cfg.CheckTimeout)
		defer cancel()
		var err error
		res, err = ss.s.cp.Check(ctx, addr)
		if err != nil {
			ss.s.log.Warn("inbound recipient check failed", "rcpt_domain", domainOf(addr), "err", err)
			return errTempCheck
		}
		ttl := ss.s.cfg.CacheTTL
		if !res.Accept {
			ttl = ss.s.cfg.NegativeCacheTTL
		}
		ss.s.cache.put(addr, res, ttl)
	}
	if !res.Accept {
		ss.s.log.Info("inbound recipient refused", "rcpt_domain", domainOf(addr), "remote_ip", ss.remoteIP())
		return ss.refused(errNoSuchUser)
	}
	if ss.kinds == nil {
		ss.kinds = map[string]string{}
	}
	ss.kinds[addr] = res.Kind
	ss.rcpts = append(ss.rcpts, addr)
	return nil
}

// refused counts a refused recipient. Past MaxUnknownRcpts it sends the
// refusal and a 421 itself and hangs up: the client is guessing addresses.
func (ss *session) refused(e *smtp.SMTPError) error {
	ss.unknown++
	if ss.unknown < ss.s.cfg.MaxUnknownRcpts {
		return e
	}
	ss.s.log.Warn("inbound connection closed: too many refused recipients", "remote_ip", ss.remoteIP(), "refused", ss.unknown)
	c := ss.conn.Conn()
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "%d %d.%d.%d %s\r\n421 4.7.0 too many unknown recipients; closing the connection\r\n",
		e.Code, e.EnhancedCode[0], e.EnhancedCode[1], e.EnhancedCode[2], e.Message)
	ss.conn.Close()
	return e
}

func (ss *session) remoteIP() string {
	host, _, err := net.SplitHostPort(ss.conn.Conn().RemoteAddr().String())
	if err != nil {
		return ss.conn.Conn().RemoteAddr().String()
	}
	return host
}

func (ss *session) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		var se *smtp.SMTPError
		switch {
		case errors.As(err, &se):
			return se // too large
		case errors.Is(err, smtp.ErrTooLongLine):
			return errLongLine
		}
		return errTempStore
	}
	_, isTLS := ss.conn.TLSConnectionState()
	raw := append([]byte(ss.received(isTLS)), body...)
	ctx, cancel := context.WithTimeout(context.Background(), ss.s.cfg.DeliverTimeout)
	defer cancel()
	resp, err := ss.s.cp.Deliver(ctx, protocol.InboundMessage{
		MailFrom: ss.from, Rcpts: ss.rcpts, Raw: raw, RemoteIP: ss.remoteIP(), HELO: ss.conn.Hostname(), TLS: isTLS,
	})
	var he *HTTPError
	switch {
	case err == nil:
		return ss.outcome(resp)
	case errors.As(err, &he) && he.Status == 413:
		return smtp.ErrDataTooLarge
	case errors.As(err, &he) && he.Status == 422:
		return errRejected
	default:
		ss.s.log.Warn("inbound delivery to the control plane failed", "err", err, "size", len(raw))
		return errTempStore
	}
}

// outcome turns the control plane's per-recipient results into the reply
// to DATA. A recipient that must wait (an inbox over its rate) makes the
// sender retry the whole message later; copies already stored are then
// recognized as duplicates. When every recipient was refused (an inbox
// deleted since the check was cached), the message is refused too.
func (ss *session) outcome(resp protocol.InboundResponse) error {
	rejected, deferred := 0, false
	for _, r := range resp.Results {
		switch r.Outcome {
		case "rejected":
			rejected++
			ss.s.cache.drop(r.Rcpt)
		case "deferred":
			deferred = true
		}
	}
	switch {
	case deferred:
		return errBusy
	case rejected > 0 && rejected == len(resp.Results):
		return errNoSuchUser
	}
	return nil
}

// received is the trace header this node adds (RFC 5321 section 4.4).
func (ss *session) received(isTLS bool) string {
	with := "ESMTP"
	if isTLS {
		with = "ESMTPS"
	}
	helo := strings.Map(func(r rune) rune {
		if r <= ' ' || r >= 0x7f || r == '(' || r == ')' || r == ';' {
			return -1
		}
		return r
	}, ss.conn.Hostname())
	h := fmt.Sprintf("Received: from %s ([%s])\r\n\tby %s (OpenMailWire) with %s id %s", helo, ss.remoteIP(), ss.s.cfg.Hostname, with, secret.RandomHex(8))
	if len(ss.rcpts) == 1 {
		h += "\r\n\tfor <" + ss.rcpts[0] + ">"
	}
	return h + ";\r\n\t" + time.Now().UTC().Format(time.RFC1123Z) + "\r\n"
}

// rcptCache keeps the control plane's answers about recipients for a
// short time. It is bounded: when full, expired entries go, and if that is
// not enough, all of them.
type rcptCache struct {
	mu  sync.Mutex
	max int
	m   map[string]cachedRcpt
}

type cachedRcpt struct {
	res   protocol.InboundCheckResponse
	until time.Time
}

func (c *rcptCache) get(addr string) (protocol.InboundCheckResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[addr]
	if !ok || time.Now().After(e.until) {
		return protocol.InboundCheckResponse{}, false
	}
	return e.res, true
}

func (c *rcptCache) put(addr string, res protocol.InboundCheckResponse, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cachedRcpt{}
	}
	if len(c.m) >= c.max {
		now := time.Now()
		for k, e := range c.m {
			if now.After(e.until) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			c.m = map[string]cachedRcpt{}
		}
	}
	c.m[addr] = cachedRcpt{res: res, until: time.Now().Add(ttl)}
}

func (c *rcptCache) drop(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, addr)
}
