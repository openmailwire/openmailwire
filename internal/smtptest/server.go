// Package smtptest is a scriptable SMTP server for tests. It is not a mail
// server: it accepts on localhost, records what it receives and answers as
// configured.
//
// Optional behaviour, all off by default:
//   - STARTTLS with a given certificate (EnableSTARTTLS), or TLS from the
//     first byte (StartImplicitTLS), and refusing mail sent without it
//     (RequireTLS);
//   - AUTH PLAIN with one user (RequireAuth);
//   - scripted replies per recipient (ReplyToRcpt) or per recipient domain
//     (ReplyToDomain), e.g. a 421 that tells the sender to slow down;
//   - the extensions it announces besides STARTTLS and AUTH (Extensions;
//     8BITMIME by default).
//
// Connections and RSETs are counted, to test connection reuse.
package smtptest

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/textproto"
	"strings"
	"sync"
)

type Message struct {
	MailFrom   string
	MailParams string // what followed the address on MAIL FROM, e.g. "BODY=8BITMIME"
	Rcpts      []string
	Data       []byte // CRLF line endings, dot-unstuffed
	TLS        bool   // the message arrived over TLS (STARTTLS or implicit)
	AuthUser   string // the user that authenticated with AUTH PLAIN, if any
}

type Server struct {
	Addr string
	ln   net.Listener

	mu            sync.Mutex
	rcptReply     map[string]string
	domainReply   map[string]string
	dropAfterData map[string]bool
	rcptCount     map[string]int
	messages      []Message
	tlsConfig     *tls.Config // STARTTLS is offered when set
	implicitTLS   bool        // set at start, never changed
	requireTLS    bool
	authUser      string
	authPass      string
	extensions    []string
	connections   int
	resets        int
}

// Start listens on a random localhost port.
func Start() (*Server, error) { return StartAt("127.0.0.1:0") }

// StartAt listens on addr.
func StartAt(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return start(ln, false), nil
}

// StartImplicitTLS listens on a random localhost port and speaks TLS from
// the first byte, like a submission server on port 465.
func StartImplicitTLS(cfg *tls.Config) (*Server, error) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		return nil, err
	}
	return start(ln, true), nil
}

func start(ln net.Listener, implicitTLS bool) *Server {
	s := &Server{
		Addr: ln.Addr().String(), ln: ln, implicitTLS: implicitTLS,
		rcptReply: map[string]string{}, domainReply: map[string]string{},
		dropAfterData: map[string]bool{}, rcptCount: map[string]int{}, extensions: []string{"8BITMIME"},
	}
	go s.serve()
	return s
}

func (s *Server) Close() { s.ln.Close() }

// ReplyToRcpt makes RCPT TO:<rcpt> answer with reply, e.g. "550 5.1.1 no
// such user". An empty reply removes the rule.
func (s *Server) ReplyToRcpt(rcpt, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	setOrDelete(s.rcptReply, strings.ToLower(rcpt), reply)
}

// ReplyToDomain makes RCPT TO for every address at domain answer with
// reply, e.g. "421 4.7.28 too many messages, slow down". A rule for the
// exact recipient wins. An empty reply removes the rule.
func (s *Server) ReplyToDomain(domain, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	setOrDelete(s.domainReply, strings.ToLower(domain), reply)
}

func setOrDelete(m map[string]string, k, v string) {
	if v == "" {
		delete(m, k)
		return
	}
	m[k] = v
}

// DropAfterData makes the server read the whole message for rcpt and then
// close the connection without a final reply.
func (s *Server) DropAfterData(rcpt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAfterData[strings.ToLower(rcpt)] = true
}

// EnableSTARTTLS offers STARTTLS with cfg (which holds the certificate).
func (s *Server) EnableSTARTTLS(cfg *tls.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tlsConfig = cfg
}

// RequireTLS refuses MAIL FROM with 530 until the session uses TLS.
func (s *Server) RequireTLS() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireTLS = true
}

// RequireAuth offers AUTH PLAIN for one user and refuses MAIL FROM with 530
// until the client has authenticated. When STARTTLS is enabled, AUTH is
// offered only after it, as real servers do.
func (s *Server) RequireAuth(user, pass string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authUser, s.authPass = user, pass
}

// Extensions sets the EHLO keywords announced besides STARTTLS and AUTH,
// e.g. Extensions("8BITMIME", "SMTPUTF8", "SIZE 1000000"), or none.
func (s *Server) Extensions(ext ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extensions = append([]string(nil), ext...)
}

// Connections returns how many connections the server accepted.
func (s *Server) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

// Resets returns how many RSET commands the server got.
func (s *Server) Resets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resets
}

// RcptCount returns how many RCPT TO commands named an address at domain.
func (s *Server) RcptCount(domain string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rcptCount[strings.ToLower(domain)]
}

func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func addrArg(line string) string {
	i := strings.IndexByte(line, '<')
	j := strings.IndexByte(line, '>')
	if i < 0 || j < i {
		return ""
	}
	return line[i+1 : j]
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

type settings struct {
	tlsConfig          *tls.Config
	requireTLS         bool
	authUser, authPass string
	extensions         []string
}

func (s *Server) settings() settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return settings{tlsConfig: s.tlsConfig, requireTLS: s.requireTLS, authUser: s.authUser, authPass: s.authPass,
		extensions: s.extensions}
}

func (s *Server) handle(c net.Conn) {
	defer func() { c.Close() }()
	s.mu.Lock()
	s.connections++
	s.mu.Unlock()
	tp := textproto.NewConn(c)
	tp.PrintfLine("220 smtptest ESMTP")
	inTLS := s.implicitTLS
	authed := ""
	var cur Message
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(line)
		cfg := s.settings()
		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			ext := append([]string{"smtptest"}, cfg.extensions...)
			if cfg.tlsConfig != nil && !inTLS {
				ext = append(ext, "STARTTLS")
			}
			if cfg.authUser != "" && (inTLS || cfg.tlsConfig == nil) {
				ext = append(ext, "AUTH PLAIN")
			}
			for i, e := range ext {
				sep := "-"
				if i == len(ext)-1 {
					sep = " "
				}
				tp.PrintfLine("250%s%s", sep, e)
			}
		case verb == "STARTTLS":
			if cfg.tlsConfig == nil || inTLS {
				tp.PrintfLine("502 5.5.1 STARTTLS not available")
				continue
			}
			tp.PrintfLine("220 2.0.0 ready to start TLS")
			tc := tls.Server(c, cfg.tlsConfig)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, tp, inTLS, authed, cur = tc, textproto.NewConn(tc), true, "", Message{}
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			if cfg.authUser == "" {
				tp.PrintfLine("502 5.5.1 AUTH not available")
				continue
			}
			if cfg.tlsConfig != nil && !inTLS {
				tp.PrintfLine("538 5.7.11 encryption required for AUTH")
				continue
			}
			arg := strings.TrimSpace(line[len("AUTH PLAIN"):])
			if arg == "" {
				tp.PrintfLine("334 ")
				if arg, err = tp.ReadLine(); err != nil {
					return
				}
			}
			b, _ := base64.StdEncoding.DecodeString(arg)
			parts := strings.Split(string(b), "\x00")
			if len(parts) == 3 && parts[1] == cfg.authUser && parts[2] == cfg.authPass {
				authed = parts[1]
				tp.PrintfLine("235 2.7.0 authentication successful")
			} else {
				tp.PrintfLine("535 5.7.8 authentication credentials invalid")
			}
		case strings.HasPrefix(verb, "MAIL FROM:"):
			if cfg.requireTLS && !inTLS {
				tp.PrintfLine("530 5.7.0 must issue a STARTTLS command first")
				continue
			}
			if cfg.authUser != "" && authed == "" {
				tp.PrintfLine("530 5.7.0 authentication required")
				continue
			}
			cur = Message{MailFrom: addrArg(line), TLS: inTLS, AuthUser: authed}
			if j := strings.IndexByte(line, '>'); j >= 0 {
				cur.MailParams = strings.TrimSpace(line[j+1:])
			}
			tp.PrintfLine("250 2.1.0 ok")
		case strings.HasPrefix(verb, "RCPT TO:"):
			rcpt := addrArg(line)
			s.mu.Lock()
			s.rcptCount[domainOf(rcpt)]++
			reply, ok := s.rcptReply[strings.ToLower(rcpt)]
			if !ok {
				reply, ok = s.domainReply[domainOf(rcpt)]
			}
			s.mu.Unlock()
			if ok {
				tp.PrintfLine("%s", reply)
				continue
			}
			cur.Rcpts = append(cur.Rcpts, rcpt)
			tp.PrintfLine("250 2.1.5 ok")
		case verb == "DATA":
			tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			cur.Data = bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
			drop := false
			s.mu.Lock()
			for _, r := range cur.Rcpts {
				drop = drop || s.dropAfterData[strings.ToLower(r)]
			}
			if !drop {
				s.messages = append(s.messages, cur)
			}
			s.mu.Unlock()
			if drop {
				return
			}
			tp.PrintfLine("250 2.0.0 ok queued as smtptest-%d", len(cur.Data))
		case verb == "RSET":
			s.mu.Lock()
			s.resets++
			s.mu.Unlock()
			cur = Message{}
			tp.PrintfLine("250 ok")
		case verb == "NOOP":
			tp.PrintfLine("250 ok")
		case verb == "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("502 5.5.2 not implemented")
		}
	}
}
