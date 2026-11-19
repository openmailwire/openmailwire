// Package submission is the SMTP submission server (RFC 6409) for apps that
// only speak SMTP: WordPress plugins, monitoring tools, printers, legacy
// code.
//
// Clients log in with any username and an API key (omw_…) as the password.
// Each message goes to the control plane's POST /emails/raw with that key,
// so it gets exactly the checks, key policies and DKIM signing of the HTTP
// API. The server holds no state, no DKIM keys and no database connection.
package submission

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/openmailwire/openmailwire/internal/smtplimit"
)

// maxRecipients matches the API's limit per email. Further RCPTs get a
// temporary 452, so clients send them in another transaction.
const maxRecipients = 50

type Config struct {
	Hostname string      // announced in the greeting
	APIURL   string      // the control plane, e.g. http://localhost:8080
	TLS      *tls.Config // required: STARTTLS, and implicit TLS for ServeTLS
	MaxSize  int64       // largest message in bytes

	// MaxAuthFailures is how many failed logins a connection may make
	// before it is closed. Default 3.
	MaxAuthFailures int
	// MaxAuthFailuresPerIP is how many failed logins one address (an IPv6
	// /64) may make in AuthFailureWindow, over all its connections, before
	// its logins are refused until the window has passed. Defaults 10 and
	// 15 minutes. It stops password guessing spread over many connections.
	MaxAuthFailuresPerIP int
	AuthFailureWindow    time.Duration
	// MaxConns caps concurrent connections over both ports (default 200),
	// MaxConnsPerIP those from one address (default 20), and
	// MaxConnDuration the lifetime of each (default 30 minutes).
	// IdleTimeout bounds the wait for each command until the client has
	// logged in (default 1 minute).
	MaxConns        int
	MaxConnsPerIP   int
	MaxConnDuration time.Duration
	IdleTimeout     time.Duration
	// ReadTimeout bounds each command and the whole DATA transfer.
	// Default 5 minutes. WriteTimeout bounds each reply. Default 1 minute.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	HTTP         *http.Client // default: a client with a 60 s timeout
	Log          *slog.Logger
	Version      string
}

type Server struct {
	cfg       Config
	smtp      *smtp.Server
	limits    *smtplimit.Limits
	authFails *smtplimit.Window // failed logins per client address
}

// New checks cfg and fills in defaults. Start it with Serve and ServeTLS.
func New(cfg Config) (*Server, error) {
	if cfg.TLS == nil {
		return nil, errors.New("submission: a TLS configuration is required")
	}
	if cfg.APIURL == "" {
		return nil, errors.New("submission: the control plane URL is required")
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 14 << 20
	}
	if cfg.MaxAuthFailures <= 0 {
		cfg.MaxAuthFailures = 3
	}
	if cfg.MaxAuthFailuresPerIP <= 0 {
		cfg.MaxAuthFailuresPerIP = 10
	}
	if cfg.AuthFailureWindow <= 0 {
		cfg.AuthFailureWindow = 15 * time.Minute
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 200
	}
	if cfg.MaxConnsPerIP <= 0 {
		cfg.MaxConnsPerIP = 20
	}
	if cfg.MaxConnDuration <= 0 {
		cfg.MaxConnDuration = 30 * time.Minute
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = time.Minute
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 5 * time.Minute
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = time.Minute
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "localhost"
	}
	s := &Server{cfg: cfg, authFails: &smtplimit.Window{Span: cfg.AuthFailureWindow},
		limits: smtplimit.NewLimits(smtplimit.Config{
			MaxConns: cfg.MaxConns, MaxPerIP: cfg.MaxConnsPerIP, MaxDuration: cfg.MaxConnDuration,
			IdleBeforeActive: cfg.IdleTimeout, Log: cfg.Log, What: "submission",
		})}
	ss := smtp.NewServer(s)
	ss.Domain = cfg.Hostname
	ss.TLSConfig = cfg.TLS
	ss.AllowInsecureAuth = false // AUTH is refused until STARTTLS
	ss.MaxMessageBytes = cfg.MaxSize
	ss.MaxRecipients = maxRecipients
	ss.ReadTimeout = cfg.ReadTimeout
	ss.WriteTimeout = cfg.WriteTimeout
	ss.ErrorLog = errorLog{cfg.Log}
	s.smtp = ss
	return s, nil
}

// Serve accepts SMTP on l (port 587). Clients must STARTTLS before AUTH.
// Both ports share the connection limits.
func (s *Server) Serve(l net.Listener) error { return s.smtp.Serve(s.limits.Wrap(l)) }

// ServeTLS accepts SMTP over implicit TLS on l (port 465).
func (s *Server) ServeTLS(l net.Listener) error {
	return s.smtp.Serve(tls.NewListener(s.limits.Wrap(l), s.cfg.TLS))
}

// Close closes the listeners and every connection now.
func (s *Server) Close() error { return s.smtp.Close() }

// Shutdown closes the listeners and waits for connections to finish.
func (s *Server) Shutdown(ctx context.Context) error { return s.smtp.Shutdown(ctx) }

// CheckAPI tells whether the control plane answers its health check.
func (s *Server) CheckAPI(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.APIURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /health answered %d", resp.StatusCode)
	}
	return nil
}

// NewSession implements smtp.Backend.
func (s *Server) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{srv: s, conn: c}, nil
}

var (
	errNeedTLS = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0},
		Message: "Must issue a STARTTLS command first"}
	errNeedAuth = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0},
		Message: "Authentication required: log in with AUTH PLAIN or LOGIN, any username, and an OpenMailWire API key (omw_...) as the password"}
	errAuthFailed = &smtp.SMTPError{Code: 535, EnhancedCode: smtp.EnhancedCode{5, 7, 8},
		Message: "Authentication failed: the password must be a valid OpenMailWire API key (omw_...)"}
	errAuthTemp = &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0},
		Message: "Temporary authentication failure: the control plane is unreachable; try again later"}
	errTemp = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0},
		Message: "Temporary failure: the message was not queued; try again later"}
	errAuthBlocked = &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0},
		Message: "Too many failed logins from your address; try again later"}
)

type session struct {
	srv      *Server
	conn     *smtp.Conn
	key      string
	failures int
	from     string
	rcpts    []string
}

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }

func (s *session) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(_, _, password string) error { return s.login(password) }), nil
	case sasl.Login:
		return &loginServer{auth: func(_, password string) error { return s.login(password) }}, nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

// login accepts any username; the password must be a live API key. The key
// is checked against the control plane now, so a wrong one fails here
// rather than after the client has sent a whole message.
func (s *session) login(password string) error {
	log := s.srv.cfg.Log.With("remote", s.remoteIP())
	ipKey := smtplimit.KeyOf(s.remoteIP())
	if s.srv.authFails.Count(ipKey) >= s.srv.cfg.MaxAuthFailuresPerIP {
		// The address has guessed too often: refuse without checking, so a
		// correct guess is not confirmed either.
		log.Warn("submission: login refused: too many failed logins from this address")
		s.hangUp(errAuthBlocked, "421 4.7.0 Too many failed logins from your address; try again later")
		return errAuthBlocked
	}
	if strings.HasPrefix(password, "omw_") {
		ok, err := s.srv.checkKey(password)
		if err != nil {
			log.Error("submission: cannot check an API key", "err", err)
			return errAuthTemp
		}
		if ok {
			s.key = password
			if c := smtplimit.Of(s.conn.Conn()); c != nil {
				c.Activate()
			}
			log.Info("submission: logged in", "key", keyPrefix(password))
			return nil
		}
	}
	s.failures++
	perIP := s.srv.authFails.Add(ipKey, 1)
	log.Warn("submission: authentication failed", "failures", s.failures, "failures_from_address", perIP)
	if s.failures >= s.srv.cfg.MaxAuthFailures || perIP >= s.srv.cfg.MaxAuthFailuresPerIP {
		// Answer, then hang up: further guesses need a new connection.
		s.hangUp(errAuthFailed, "421 4.7.0 Too many failed authentication attempts; closing the connection")
	}
	return errAuthFailed
}

// hangUp writes the reply to the current command and a 421 itself, then
// closes the connection.
func (s *session) hangUp(e *smtp.SMTPError, line string) {
	c := s.conn.Conn()
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, fmt.Sprintf("%d %d.%d.%d %s\r\n%s\r\n", e.Code, e.EnhancedCode[0], e.EnhancedCode[1], e.EnhancedCode[2], e.Message, line))
	s.conn.Close()
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.key == "" {
		if _, isTLS := s.conn.TLSConnectionState(); !isTLS {
			return errNeedTLS
		}
		return errNeedAuth
	}
	s.from, s.rcpts = from, nil
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.key == "" {
		return errNeedAuth
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err // smtp.ErrDataTooLarge becomes 552 5.3.4
	}
	return s.srv.submit(s.key, s.from, s.rcpts, raw, s.remoteIP())
}

func (s *session) Reset() { s.from, s.rcpts = "", nil }

func (s *session) Logout() error { return nil }

func (s *session) remoteIP() string {
	host, _, err := net.SplitHostPort(s.conn.Conn().RemoteAddr().String())
	if err != nil {
		return s.conn.Conn().RemoteAddr().String()
	}
	return host
}

// checkKey asks the control plane whether key is live. Any authenticated
// answer counts: 403 means a live key without that permission.
func (s *Server) checkKey(key string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, s.cfg.APIURL+"/domains", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "openmailwire-submission/"+s.cfg.Version)
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusForbidden:
		return true, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return false, nil
	}
	return false, fmt.Errorf("GET /domains answered %d", resp.StatusCode)
}

type rawRequest struct {
	Raw      []byte   `json:"raw"` // encoding/json writes standard base64
	MailFrom string   `json:"mail_from"`
	Rcpts    []string `json:"rcpts"`
}

type apiAnswer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// submit hands one message to POST /emails/raw and turns the answer into
// the SMTP reply to DATA. It never logs content or full addresses.
func (s *Server) submit(key, from string, rcpts []string, raw []byte, remote string) error {
	log := s.cfg.Log.With("remote", remote, "key", keyPrefix(key), "rcpt_domains", domains(rcpts), "size", len(raw))
	body, err := json.Marshal(rawRequest{Raw: raw, MailFrom: from, Rcpts: rcpts})
	if err != nil {
		log.Error("submission: encode request", "err", err)
		return errTemp
	}
	req, err := http.NewRequest(http.MethodPost, s.cfg.APIURL+"/emails/raw", bytes.NewReader(body))
	if err != nil {
		log.Error("submission: build request", "err", err)
		return errTemp
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "openmailwire-submission/"+s.cfg.Version)
	if k := idempotencyKey(body, raw); k != "" {
		req.Header.Set("Idempotency-Key", k)
	}
	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		log.Error("submission: control plane unreachable", "err", err)
		return errTemp
	}
	defer resp.Body.Close()
	var ans apiAnswer
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ans)

	reply := replyFor(resp.StatusCode, ans)
	switch {
	case reply.Code == 250:
		log.Info("submission: queued", "id", ans.ID)
	case resp.StatusCode >= 500 || reply == errTemp:
		log.Error("submission: control plane refused", "status", resp.StatusCode, "error", ans.Name)
	default:
		log.Warn("submission: refused", "status", resp.StatusCode, "error", ans.Name)
	}
	return reply
}

// replyFor maps the API's answer to the SMTP reply, as the roadmap
// contract fixes it. A 250 is returned as an *SMTPError too, because that
// is how go-smtp lets a session choose the text of a success reply.
func replyFor(status int, a apiAnswer) *smtp.SMTPError {
	reply := func(code int, c1, c2, c3 int, msg string) *smtp.SMTPError {
		return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{c1, c2, c3}, Message: msg}
	}
	msg := replyText(a.Message)
	switch {
	case status/100 == 2 && a.ID != "":
		return reply(250, 2, 0, 0, "queued as "+replyText(a.ID))
	case status == http.StatusUnauthorized:
		return reply(535, 5, 7, 8, "Authentication failed: the API key is invalid or revoked")
	case status == http.StatusForbidden:
		return reply(550, 5, 7, 1, orDefault(msg, "Sending this message is not allowed"))
	case status == http.StatusRequestEntityTooLarge:
		return reply(552, 5, 3, 4, orDefault(msg, "Message too big"))
	case status == http.StatusUnprocessableEntity:
		return reply(554, 5, 6, 0, orDefault(msg, "The message is malformed"))
	case status == http.StatusTooManyRequests:
		return reply(451, 4, 7, 1, orDefault(msg, "Sending limit reached; try again later"))
	}
	// 5xx, and answers this server does not expect: keep the message with
	// the client, which will retry.
	return errTemp
}

// replyText makes s safe for one line of an SMTP reply: printable ASCII,
// at most 400 bytes.
func replyText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		case r > 0x7e:
			if r == '…' {
				b.WriteString("...")
			} else {
				b.WriteByte('?')
			}
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 400 {
		out = out[:400]
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// idempotencyKey makes a client's retry of the same message (for example
// after a timeout, when the first copy was in fact queued) return the first
// copy instead of sending it twice. Only messages that carry their own
// Message-ID and Date get one: those fields make identical bytes a retry,
// not a new message.
func idempotencyKey(body, raw []byte) string {
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return ""
	}
	if strings.TrimSpace(h.Get("Message-Id")) == "" || strings.TrimSpace(h.Get("Date")) == "" {
		return ""
	}
	sum := sha256.Sum256(body)
	return "smtp-" + hex.EncodeToString(sum[:])
}

// keyPrefix is the part of an API key the API shows too, for logs.
func keyPrefix(key string) string {
	if len(key) > 10 {
		return key[:10]
	}
	return key
}

// domains lists the recipient domains, for logs that must not hold
// addresses.
func domains(rcpts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rcpts {
		d := strings.ToLower(r[strings.LastIndexByte(r, '@')+1:])
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// loginServer is the server side of the LOGIN mechanism, which go-sasl
// only implements for clients. Outlook and many older apps need it.
type loginServer struct {
	step     int
	username string
	auth     func(username, password string) error
}

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.step {
	case 0:
		if len(response) > 0 { // the username came as the initial response
			l.username, l.step = string(response), 2
			return []byte("Password:"), false, nil
		}
		l.step = 1
		return []byte("Username:"), false, nil
	case 1:
		l.username, l.step = string(response), 2
		return []byte("Password:"), false, nil
	case 2:
		l.step = 3
		return nil, true, l.auth(l.username, string(response))
	}
	return nil, true, sasl.ErrUnexpectedClientResponse
}

// errorLog sends go-smtp's connection errors to slog.
type errorLog struct{ log *slog.Logger }

func (e errorLog) Printf(format string, v ...any) {
	e.log.Warn("submission: " + strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (e errorLog) Println(v ...any) {
	e.log.Warn("submission: " + strings.TrimSpace(fmt.Sprintln(v...)))
}
