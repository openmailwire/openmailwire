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
}

// Server is an inbound SMTP server.
type Server struct {
	cfg  Config
	cp   *Client
	log  *slog.Logger
	smtp *smtp.Server
}

func New(cfg Config, cp *Client, log *slog.Logger) *Server {
	cfg.setDefaults()
	s := &Server{cfg: cfg, cp: cp, log: log}
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
	err := s.smtp.Serve(&limitListener{Listener: l, sem: make(chan struct{}, s.cfg.MaxConns), maxDur: s.cfg.MaxConnDuration, log: s.log})
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
	s     *Server
	conn  *smtp.Conn
	from  string
	rcpts []string
	kinds map[string]string
}

func (ss *session) Reset() {
	ss.from, ss.rcpts, ss.kinds = "", nil, nil
}

func (ss *session) Logout() error { return nil }

func (ss *session) Mail(from string, _ *smtp.MailOptions) error {
	ss.Reset()
	ss.from = from
	return nil
}

var (
	errNoSuchUser = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such recipient here"}
	errTempCheck  = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure checking the recipient; try again later"}
	errTempStore  = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "temporary failure storing the message; try again later"}
	errRejected   = &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "message rejected"}
	errLongLine   = &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "a line of the message is too long"}
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
	ctx, cancel := context.WithTimeout(context.Background(), ss.s.cfg.CheckTimeout)
	defer cancel()
	res, err := ss.s.cp.Check(ctx, addr)
	if err != nil {
		ss.s.log.Warn("inbound recipient check failed", "rcpt_domain", domainOf(addr), "err", err)
		return errTempCheck
	}
	if !res.Accept {
		ss.s.log.Info("inbound recipient refused", "rcpt_domain", domainOf(addr), "remote_ip", ss.remoteIP())
		return errNoSuchUser
	}
	if ss.kinds == nil {
		ss.kinds = map[string]string{}
	}
	ss.kinds[addr] = res.Kind
	ss.rcpts = append(ss.rcpts, addr)
	return nil
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
	_, err = ss.s.cp.Deliver(ctx, protocol.InboundMessage{
		MailFrom: ss.from, Rcpts: ss.rcpts, Raw: raw, RemoteIP: ss.remoteIP(), HELO: ss.conn.Hostname(), TLS: isTLS,
	})
	var he *HTTPError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &he) && he.Status == 413:
		return smtp.ErrDataTooLarge
	case errors.As(err, &he) && he.Status == 422:
		return errRejected
	default:
		ss.s.log.Warn("inbound delivery to the control plane failed", "err", err, "size", len(raw))
		return errTempStore
	}
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

// limitListener caps concurrent connections and the lifetime of each.
type limitListener struct {
	net.Listener
	sem    chan struct{}
	maxDur time.Duration
	log    *slog.Logger
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			bc := &boundedConn{Conn: c, deadline: time.Now().Add(l.maxDur), release: func() { <-l.sem }}
			bc.Conn.SetDeadline(bc.deadline)
			return bc, nil
		default:
			l.log.Warn("inbound connection refused: too many connections")
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			io.WriteString(c, "421 4.3.2 too many connections, try again later\r\n")
			c.Close()
		}
	}
}

// boundedConn never lets a deadline move past the connection's end of life.
type boundedConn struct {
	net.Conn
	deadline time.Time
	once     sync.Once
	release  func()
}

func (c *boundedConn) clamp(t time.Time) time.Time {
	if t.IsZero() || t.After(c.deadline) {
		return c.deadline
	}
	return t
}

func (c *boundedConn) SetDeadline(t time.Time) error      { return c.Conn.SetDeadline(c.clamp(t)) }
func (c *boundedConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(c.clamp(t)) }
func (c *boundedConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(c.clamp(t)) }

func (c *boundedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
