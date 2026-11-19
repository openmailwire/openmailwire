package worker

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// smtpClient is the SMTP client the worker delivers with. Unlike net/smtp
// it
//   - keeps the reply to EHLO: a 421 there is a receiver asking us to slow
//     down, and only 500/502 ("command not recognised") fall back to HELO;
//   - says which step each reply came from (protocol.Stage*);
//   - sets a deadline per command, per block of the message and for the
//     final reply (RFC 5321 section 4.5.3.2), never past the job's own
//     deadline;
//   - can carry several messages over one connection (RSET in between).
type smtpClient struct {
	raw        *deadlineConn
	text       *textproto.Conn
	host       string            // the server's name
	ext        map[string]string // EHLO keywords, upper case
	tlsState   *tls.ConnectionState
	mode       string // what the TLS of this session amounts to (Result.TLS)
	cmdTimeout time.Duration
	sent       int       // messages handed over on this connection
	idleSince  time.Time // when it went back to the pool
}

// replyError is a reply a step did not want.
type replyError struct {
	Stage string
	Code  int
	Msg   string
}

func (e *replyError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Msg) }

// deadlineConn caps every deadline at the job's deadline, and in body mode
// gives each write its own deadline: a slow but steady transfer of a large
// message is fine, a stalled one is not.
type deadlineConn struct {
	net.Conn
	mu    sync.Mutex
	limit time.Time     // the job's deadline; zero means none
	block time.Duration // when > 0, every Write gets a deadline this far ahead
}

func (c *deadlineConn) at(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := time.Now().Add(d)
	if !c.limit.IsZero() && c.limit.Before(t) {
		return c.limit
	}
	return t
}

func (c *deadlineConn) setBlock(d time.Duration) {
	c.mu.Lock()
	c.block = d
	c.mu.Unlock()
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	block := c.block
	c.mu.Unlock()
	if block > 0 {
		c.Conn.SetWriteDeadline(c.at(block))
	}
	return c.Conn.Write(b)
}

func newSMTPClient(conn net.Conn, host string, cmdTimeout time.Duration) *smtpClient {
	raw := &deadlineConn{Conn: conn}
	return &smtpClient{raw: raw, text: textproto.NewConn(raw), host: host, cmdTimeout: cmdTimeout, mode: "none"}
}

// bind makes ctx's deadline the limit of every later deadline.
func (c *smtpClient) bind(ctx context.Context) {
	dl, _ := ctx.Deadline()
	c.raw.mu.Lock()
	c.raw.limit = dl
	c.raw.mu.Unlock()
}

func (c *smtpClient) deadline(d time.Duration) { c.raw.Conn.SetDeadline(c.raw.at(d)) }

// greet reads the server's greeting.
func (c *smtpClient) greet() error {
	c.deadline(c.cmdTimeout)
	code, msg, err := c.text.ReadResponse(0)
	if err != nil {
		return err
	}
	if code != 220 {
		return &replyError{protocol.StageGreeting, code, msg}
	}
	return nil
}

// cmd sends one command and reads its whole reply. err is only an I/O or
// protocol error; the caller judges the code.
func (c *smtpClient) cmd(timeout time.Duration, format string, args ...any) (int, string, error) {
	c.deadline(timeout)
	id, err := c.text.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}
	c.text.StartResponse(id)
	defer c.text.EndResponse(id)
	return c.text.ReadResponse(0)
}

// hello sends EHLO, and HELO only when the server does not know EHLO.
func (c *smtpClient) hello(name string) error {
	code, msg, err := c.cmd(c.cmdTimeout, "EHLO %s", name)
	if err != nil {
		return err
	}
	switch code {
	case 250:
		c.ext = map[string]string{}
		lines := strings.Split(msg, "\n")
		for _, l := range lines[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
			c.ext[strings.ToUpper(k)] = v
		}
		return nil
	case 500, 502:
		code, msg, err = c.cmd(c.cmdTimeout, "HELO %s", name)
		if err != nil {
			return err
		}
		if code != 250 {
			return &replyError{protocol.StageEHLO, code, msg}
		}
		c.ext = map[string]string{}
		return nil
	}
	return &replyError{protocol.StageEHLO, code, msg}
}

func (c *smtpClient) has(ext string) bool {
	_, ok := c.ext[ext]
	return ok
}

// startTLS upgrades the connection. The caller sends EHLO again.
func (c *smtpClient) startTLS(ctx context.Context, cfg *tls.Config) error {
	code, msg, err := c.cmd(c.cmdTimeout, "STARTTLS")
	if err != nil {
		return err
	}
	if code != 220 {
		return &replyError{protocol.StageSTARTTLS, code, msg}
	}
	tc := tls.Client(c.raw, cfg)
	c.deadline(c.cmdTimeout)
	if err := tc.HandshakeContext(ctx); err != nil {
		return err
	}
	st := tc.ConnectionState()
	c.tlsState = &st
	c.text = textproto.NewConn(tc)
	return nil
}

// authPlain authenticates with AUTH PLAIN (RFC 4954), on an encrypted
// session only.
func (c *smtpClient) authPlain(user, pass string) error {
	resp := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
	code, msg, err := c.cmd(c.cmdTimeout, "AUTH PLAIN %s", resp)
	if err != nil {
		return err
	}
	if code != 235 {
		return &replyError{protocol.StageAuth, code, msg}
	}
	return nil
}

// reset ends any transaction (RSET) before the connection carries another
// message. An error means the connection cannot be used.
func (c *smtpClient) reset() error {
	code, msg, err := c.cmd(c.cmdTimeout, "RSET")
	if err != nil {
		return err
	}
	if code != 250 {
		return &replyError{"rset", code, msg}
	}
	return nil
}

// quit says goodbye and closes the connection.
func (c *smtpClient) quit() {
	c.cmd(10*time.Second, "QUIT")
	c.close()
}

func (c *smtpClient) close() { c.text.Close() }

// sessionPool keeps connections that finished a transaction cleanly, so the
// next message to the same server (with the same TLS requirements) reuses
// them instead of a new TCP and TLS handshake.
type sessionPool struct {
	mu          sync.Mutex
	idle        map[string][]*smtpClient
	maxIdle     int           // per key
	idleTimeout time.Duration // an idle connection older than this is closed
}

func (p *sessionPool) get(key string) *smtpClient {
	p.mu.Lock()
	stale := p.expireLocked()
	var c *smtpClient
	if l := p.idle[key]; len(l) > 0 {
		c = l[len(l)-1]
		p.idle[key] = l[:len(l)-1]
	}
	p.mu.Unlock()
	closeAll(stale)
	return c
}

func (p *sessionPool) put(key string, c *smtpClient) {
	c.idleSince = time.Now()
	p.mu.Lock()
	stale := p.expireLocked()
	if p.idle == nil {
		p.idle = map[string][]*smtpClient{}
	}
	if len(p.idle[key]) >= p.maxIdle {
		stale = append(stale, c)
	} else {
		p.idle[key] = append(p.idle[key], c)
	}
	p.mu.Unlock()
	closeAll(stale)
}

// expireLocked removes connections idle for too long and returns them.
func (p *sessionPool) expireLocked() []*smtpClient {
	var stale []*smtpClient
	for k, l := range p.idle {
		keep := l[:0]
		for _, c := range l {
			if time.Since(c.idleSince) > p.idleTimeout {
				stale = append(stale, c)
			} else {
				keep = append(keep, c)
			}
		}
		if len(keep) == 0 {
			delete(p.idle, k)
		} else {
			p.idle[k] = keep
		}
	}
	return stale
}

// closeIdle closes every pooled connection.
func (p *sessionPool) closeIdle() {
	p.mu.Lock()
	var all []*smtpClient
	for _, l := range p.idle {
		all = append(all, l...)
	}
	p.idle = nil
	p.mu.Unlock()
	closeAll(all)
}

func closeAll(cs []*smtpClient) {
	for _, c := range cs {
		c.bind(context.Background())
		c.quit()
	}
}
