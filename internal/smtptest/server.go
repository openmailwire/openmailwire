// Package smtptest is a scriptable SMTP server for tests. It is not a mail
// server: it accepts on localhost, records what it receives and answers as
// configured.
package smtptest

import (
	"bytes"
	"net"
	"net/textproto"
	"strings"
	"sync"
)

type Message struct {
	MailFrom string
	Rcpts    []string
	Data     []byte // CRLF line endings, dot-unstuffed
}

type Server struct {
	Addr string
	ln   net.Listener

	mu            sync.Mutex
	rcptReply     map[string]string
	dropAfterData map[string]bool
	messages      []Message
}

// Start listens on a random localhost port.
func Start() (*Server, error) { return StartAt("127.0.0.1:0") }

// StartAt listens on addr.
func StartAt(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{Addr: ln.Addr().String(), ln: ln, rcptReply: map[string]string{}, dropAfterData: map[string]bool{}}
	go s.serve()
	return s, nil
}

func (s *Server) Close() { s.ln.Close() }

// ReplyToRcpt makes RCPT TO:<rcpt> answer with reply, e.g. "550 5.1.1 no such user".
func (s *Server) ReplyToRcpt(rcpt, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rcptReply[strings.ToLower(rcpt)] = reply
}

// DropAfterData makes the server read the whole message for rcpt and then
// close the connection without a final reply.
func (s *Server) DropAfterData(rcpt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAfterData[strings.ToLower(rcpt)] = true
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

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	tp := textproto.NewConn(c)
	tp.PrintfLine("220 smtptest ESMTP")
	var cur Message
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			tp.PrintfLine("250-smtptest")
			tp.PrintfLine("250 8BITMIME")
		case strings.HasPrefix(verb, "MAIL FROM:"):
			cur = Message{MailFrom: addrArg(line)}
			tp.PrintfLine("250 2.1.0 ok")
		case strings.HasPrefix(verb, "RCPT TO:"):
			rcpt := addrArg(line)
			s.mu.Lock()
			reply, ok := s.rcptReply[strings.ToLower(rcpt)]
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
		case verb == "RSET", verb == "NOOP":
			tp.PrintfLine("250 ok")
		case verb == "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("502 5.5.2 not implemented")
		}
	}
}
