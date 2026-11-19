package worker

// Regression tests for the v0.5.0 audit findings on the worker (R3#4,
// R3#5, R3#6, R3#9, R3#11, R3#12 and the unproven ones), and tests of the
// v0.6 worker features. Each audit test failed before its fix.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/openmailwire/openmailwire/internal/config"
	"github.com/openmailwire/openmailwire/internal/protocol"
	"github.com/openmailwire/openmailwire/internal/smtptest"
)

// rawSMTP is a hand-scripted SMTP server: for each command it calls reply,
// which returns the reply line(s) to send ("" closes the connection).
func rawSMTP(t *testing.T, onData func(r *bufio.Reader) string, reply func(cmd string) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				io.WriteString(c, "220 raw ESMTP\r\n")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimRight(line, "\r\n")
					if strings.EqualFold(cmd, "DATA") {
						io.WriteString(c, "354 go\r\n")
						io.WriteString(c, onData(r))
						continue
					}
					out := reply(cmd)
					if out == "" {
						return
					}
					io.WriteString(c, out)
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func okReply(cmd string) string {
	switch {
	case strings.HasPrefix(strings.ToUpper(cmd), "EHLO"):
		return "250-raw\r\n250 8BITMIME\r\n"
	case strings.HasPrefix(strings.ToUpper(cmd), "QUIT"):
		return "221 bye\r\n"
	}
	return "250 ok\r\n"
}

// readBody reads the message up to the terminating dot, pausing between
// chunks so the transfer takes about transfer.
func readBody(r *bufio.Reader, transfer time.Duration) {
	steps := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil || line == ".\r\n" {
			return
		}
		if steps++; steps%2000 == 0 {
			time.Sleep(transfer / 30)
		}
	}
}

// TestDataBlockAndFinalTimeouts covers R3#5. The DATA phase had one
// deadline for the body and the final reply together, so a transfer and a
// final reply that each took less than it, but more together, turned an
// accepted message into "unknown" (and a duplicate). Now each block of the
// body has its own deadline (DataTimeout) and the final reply its own
// (FinalTimeout, 10 minutes by default). Scaled down: DataTimeout 1 s,
// transfer about 0.6 s, final reply 0.6 s after the dot.
func TestDataBlockAndFinalTimeouts(t *testing.T) {
	addr := rawSMTP(t, func(r *bufio.Reader) string {
		readBody(r, 600*time.Millisecond)
		time.Sleep(600 * time.Millisecond) // content scanning
		return "250 2.0.0 queued as X\r\n"
	}, okReply)
	body := strings.Repeat("0123456789012345678901234567890123456789012345678901234567890123456789\r\n", 60000) // ~4.3 MB
	j := protocol.Job{AttemptID: "00000000-0000-4000-8000-000000000001", MailFrom: "b@x.test", Rcpt: "r@example.test",
		RcptDomain: "example.test", Raw: []byte("Subject: s\r\n\r\n" + body)}
	res := (&Deliverer{Override: addr, EHLO: "w.test", DataTimeout: time.Second}).Deliver(context.Background(), j)
	if res.Outcome != protocol.OutcomeDelivered || res.Stage != protocol.StageMessage {
		t.Fatalf("an accepted message is reported %+v", res)
	}

	// A final reply slower than FinalTimeout is still unknown.
	addr = rawSMTP(t, func(r *bufio.Reader) string {
		readBody(r, 0)
		time.Sleep(700 * time.Millisecond)
		return "250 2.0.0 queued as X\r\n"
	}, okReply)
	res = (&Deliverer{Override: addr, EHLO: "w.test", FinalTimeout: 300 * time.Millisecond}).Deliver(context.Background(), job("r@example.test"))
	if res.Outcome != protocol.OutcomeUnknown {
		t.Fatalf("a final reply after FinalTimeout: %+v", res)
	}
}

// TestEHLO421KeepsCode covers R3#9: a 421 to EHLO (a receiver telling us to
// slow down) used to lose its code, because the HELO fallback ran on the
// closing connection.
func TestEHLO421KeepsCode(t *testing.T) {
	addr := rawSMTP(t, nil, func(cmd string) string {
		if strings.HasPrefix(strings.ToUpper(cmd), "EHLO") {
			return "421 4.7.0 Too many connections from your IP, slow down\r\n"
		}
		return ""
	})
	res := (&Deliverer{Override: addr, EHLO: "w.test"}).Deliver(context.Background(), job("r@example.test"))
	if res.Code != 421 || res.Enhanced != "4.7.0" || res.Stage != protocol.StageEHLO || res.Outcome != protocol.OutcomeDeferred {
		t.Fatalf("the receiver's 421 at EHLO is reported as %+v", res)
	}

	// Only "command not recognised" falls back to HELO.
	var helo bool
	var mu sync.Mutex
	addr = rawSMTP(t, func(r *bufio.Reader) string { readBody(r, 0); return "250 ok\r\n" }, func(cmd string) string {
		switch {
		case strings.HasPrefix(strings.ToUpper(cmd), "EHLO"):
			return "502 5.5.2 command not recognised\r\n"
		case strings.HasPrefix(strings.ToUpper(cmd), "HELO"):
			mu.Lock()
			helo = true
			mu.Unlock()
		}
		return okReply(cmd)
	})
	res = (&Deliverer{Override: addr, EHLO: "w.test"}).Deliver(context.Background(), job("r@example.test"))
	mu.Lock()
	defer mu.Unlock()
	if res.Outcome != protocol.OutcomeDelivered || !helo {
		t.Fatalf("HELO fallback: %+v (helo %v)", res, helo)
	}
}

// TestStagesOfReplies: each reply says which step it came from, so the
// control plane can tell a connection-level throttle from a recipient's.
func TestStagesOfReplies(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.ReplyToRcpt("busy@example.test", "450 4.2.1 receiving mail at a rate that prevents delivery")
	d := &Deliverer{Override: srv.Addr, EHLO: "w.test"}
	if res := d.Deliver(context.Background(), job("busy@example.test")); res.Stage != protocol.StageRcpt || res.Code != 450 {
		t.Fatalf("RCPT reply: %+v", res)
	}
	if res := d.Deliver(context.Background(), job("ok@example.test")); res.Stage != protocol.StageMessage || res.Code != 250 {
		t.Fatalf("final reply: %+v", res)
	}
	// A greeting other than 220.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.WriteString(c, "421 4.7.0 too many connections\r\n")
			c.Close()
		}
	}()
	res := (&Deliverer{Override: ln.Addr().String(), EHLO: "w.test"}).Deliver(context.Background(), job("r@example.test"))
	if res.Stage != protocol.StageGreeting || res.Code != 421 {
		t.Fatalf("greeting: %+v", res)
	}
}

type deadlineDeliverer struct{ got chan time.Time }

func (d deadlineDeliverer) Deliver(ctx context.Context, j protocol.Job) Result {
	dl, _ := ctx.Deadline()
	d.got <- dl
	return Result{Outcome: protocol.OutcomeDelivered, Code: 250}
}

func testRunner(t *testing.T, cp *httptest.Server, d deliverer, cfg config.Worker) *Runner {
	return &Runner{
		cfg: cfg, client: &Client{Base: cp.URL, Token: "omwk_x", HTTP: cp.Client()},
		d: d, log: slog.New(slog.NewTextHandler(io.Discard, nil)), outbox: t.TempDir(),
	}
}

// TestJobBoundedByLease covers R3#6: a delivery must be over, with a
// margin for the report, before the lease expires and the control plane
// hands the delivery to another attempt. The runner used to bound it by
// OMW_JOB_TIMEOUT only.
func TestJobBoundedByLease(t *testing.T) {
	var reports []protocol.Report
	var mu sync.Mutex
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep protocol.Report
		json.NewDecoder(r.Body).Decode(&rep)
		mu.Lock()
		reports = append(reports, rep)
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	defer cp.Close()
	got := make(chan time.Time, 1)
	r := testRunner(t, cp, deadlineDeliverer{got}, config.Worker{JobTimeout: 10 * time.Minute})
	lease := time.Now().Add(5 * time.Minute)
	r.handle(context.Background(), protocol.Job{AttemptID: "00000000-0000-4000-8000-000000000001", LeaseExpiresAt: lease})
	dl := <-got
	if !dl.Before(lease.Add(-25*time.Second)) || dl.Before(lease.Add(-2*time.Minute)) {
		t.Fatalf("lease expires %v; the delivery may run until %v", lease.Format(time.TimeOnly), dl.Format(time.TimeOnly))
	}

	// With a lease already (nearly) over, nothing is attempted.
	r.handle(context.Background(), protocol.Job{AttemptID: "00000000-0000-4000-8000-000000000002", LeaseExpiresAt: time.Now().Add(time.Second)})
	select {
	case <-got:
		t.Fatal("a delivery started on an expired lease")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 2 || reports[1].Outcome != protocol.OutcomeDeferred || !strings.Contains(reports[1].Response, "not attempted") {
		t.Fatalf("reports: %+v", reports)
	}
}

// TestReportsKeptOnAuthErrors (R3, unproven): a revoked token or a wrong
// role (401, 403) is fixed by the operator, so the report stays in the
// outbox, as does a refusal of new fields by a control plane older than the
// worker; other 4xx answers drop it.
func TestReportsKeptOnAuthErrors(t *testing.T) {
	var mu sync.Mutex
	status, body := 0, ""
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer cp.Close()
	r := testRunner(t, cp, nil, config.Worker{})
	cases := []struct {
		status int
		body   string
		kept   bool
	}{
		{http.StatusUnauthorized, `{"name":"invalid_worker_token"}`, true},
		{http.StatusForbidden, `{"name":"wrong_worker_role"}`, true},
		{http.StatusUnprocessableEntity, `{"name":"validation_error","message":"json: unknown field \"stage\""}`, true},
		{http.StatusUnprocessableEntity, `{"name":"validation_error","message":"attempt_id or outcome is invalid"}`, false},
		{http.StatusConflict, `{"name":"attempt_of_another_worker"}`, false},
		{http.StatusNotFound, `{"name":"not_found"}`, false},
	}
	for i, c := range cases {
		mu.Lock()
		status, body = c.status, c.body
		mu.Unlock()
		id := "00000000-0000-4000-8000-00000000000" + string(rune('1'+i))
		r.submit(context.Background(), protocol.Report{AttemptID: id, Outcome: protocol.OutcomeDelivered})
		_, err := os.Stat(filepath.Join(r.outbox, id+".json"))
		if kept := err == nil; kept != c.kept {
			t.Errorf("answer %d %s: report kept = %v", c.status, c.body, kept)
		}
	}
}

// TestOpportunisticTLS10 covers R3#11: opportunistic TLS used to insist on
// TLS 1.2, so a server that only speaks TLS 1.0 or 1.1 got the message in
// plaintext. MTA-STS (and DANE) keep requiring TLS 1.2.
func TestOpportunisticTLS10(t *testing.T) {
	cert, err := smtptest.SelfSigned("mx.example.test")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cfg := smtptest.TLSConfig(cert)
	cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
	srv.EnableSTARTTLS(cfg)
	res := (&Deliverer{Override: srv.Addr, EHLO: "w.test"}).Deliver(context.Background(), job("ok@example.test"))
	msgs := srv.Messages()
	if res.Outcome != protocol.OutcomeDelivered || res.TLS != "opportunistic" || len(msgs) != 1 || !msgs[0].TLS {
		t.Fatalf("%+v; received over TLS: %v", res, len(msgs) == 1 && msgs[0].TLS)
	}

	// The same server for a domain with an MTA-STS policy in enforce mode.
	m := startMXServers(t)
	ca, _ := m.ca.Issue("mx.example.test")
	old := smtptest.TLSConfig(ca)
	old.MinVersion, old.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
	m.good.EnableSTARTTLS(old)
	res = m.deliverer(m.good, policy("enforce", "mx.example.test"), nil).Deliver(context.Background(), job("a@example.test"))
	if res.Outcome != protocol.OutcomeDeferred || res.Enhanced != "4.7.5" || len(m.good.Messages()) != 0 {
		t.Fatalf("MTA-STS with TLS 1.1 only: %+v", res)
	}
}

// Test8BitNeedsServerSupport covers R3#12: a message with 8-bit data goes
// only to a server that announces 8BITMIME (with BODY=8BITMIME), and one
// with non-ASCII addresses or header fields only to one that announces
// SMTPUTF8. Otherwise the attempt is deferred with 4.6.3 and nothing is
// sent: converting the message would break its DKIM signature.
func Test8BitNeedsServerSupport(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// A new connection for each message: the server's extensions change.
	d := &Deliverer{Override: srv.Addr, EHLO: "w.test", NoReuse: true}
	body8 := protocol.Job{AttemptID: "00000000-0000-4000-8000-000000000001", MailFrom: "b@x.test", Rcpt: "r@example.test",
		RcptDomain: "example.test", Raw: []byte("Subject: s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nسلام\r\n")}
	header8 := body8
	header8.Raw = []byte("Subject: سلام\r\n\r\nhello\r\n")
	addr8 := job("رضا@example.test")

	srv.Extensions() // neither 8BITMIME nor SMTPUTF8
	for name, j := range map[string]protocol.Job{"8-bit body": body8, "UTF-8 header": header8, "UTF-8 address": addr8} {
		res := d.Deliver(context.Background(), j)
		if res.Outcome != protocol.OutcomeDeferred || res.Enhanced != "4.6.3" || res.Code != 0 {
			t.Errorf("%s to a 7-bit server: %+v", name, res)
		}
	}
	if n := len(srv.Messages()); n != 0 {
		t.Fatalf("%d messages reached a 7-bit server", n)
	}

	srv.Extensions("8BITMIME")
	if res := d.Deliver(context.Background(), body8); res.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("8-bit body to an 8BITMIME server: %+v", res)
	}
	if res := d.Deliver(context.Background(), header8); res.Enhanced != "4.6.3" {
		t.Fatalf("UTF-8 header without SMTPUTF8: %+v", res)
	}
	srv.Extensions("8BITMIME", "SMTPUTF8")
	if res := d.Deliver(context.Background(), header8); res.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("UTF-8 header to an SMTPUTF8 server: %+v", res)
	}
	if res := d.Deliver(context.Background(), job("ok@example.test")); res.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("ASCII message: %+v", res)
	}
	msgs := srv.Messages()
	if len(msgs) != 3 || msgs[0].MailParams != "BODY=8BITMIME" || msgs[1].MailParams != "BODY=8BITMIME SMTPUTF8" || msgs[2].MailParams != "" {
		t.Fatalf("MAIL parameters: %q %q %q", msgs[0].MailParams, msgs[1].MailParams, msgs[2].MailParams)
	}
}

// TestReplyCutOnRuneBoundary covers the worker's side of R3#4: a long
// reply is cut to 1000 bytes on a character boundary, as valid UTF-8.
func TestReplyCutOnRuneBoundary(t *testing.T) {
	msg := "5.1.1 " + strings.Repeat("x", 987) + strings.Repeat("é", 60) + "\xff"
	r := fromReply(550, msg)
	if len(r.Response) > 1000 || !utf8.ValidString(r.Response) {
		t.Fatalf("response of %d bytes, valid UTF-8: %v", len(r.Response), utf8.ValidString(r.Response))
	}
	if got := protocol.CleanText("ab\x00c\xffd", 100); got != "abc�d" {
		t.Fatalf("CleanText: %q", got)
	}
	if got := protocol.CleanText("aé", 2); got != "a" {
		t.Fatalf("CleanText cut a character: %q", got)
	}
}

// TestConnectionReuse: messages to the same server share one connection,
// with RSET in between, up to MaxPerConn messages per connection.
func TestConnectionReuse(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.ReplyToRcpt("gone@example.test", "550 5.1.1 no such user")
	d := &Deliverer{Override: srv.Addr, EHLO: "w.test", MaxPerConn: 3}
	defer d.Close()
	for _, rcpt := range []string{"a@example.test", "gone@example.test", "b@example.test", "c@example.test", "d@example.test"} {
		d.Deliver(context.Background(), job(rcpt))
	}
	if n := len(srv.Messages()); n != 4 {
		t.Fatalf("%d messages", n)
	}
	// a, (gone), b, c on the first connection; d on a second one after
	// three messages.
	if c, r := srv.Connections(), srv.Resets(); c != 2 || r != 3 {
		t.Fatalf("%d connections and %d RSETs, want 2 and 3", c, r)
	}

	n := &Deliverer{Override: srv.Addr, EHLO: "w.test", NoReuse: true}
	before := srv.Connections()
	n.Deliver(context.Background(), job("e@example.test"))
	n.Deliver(context.Background(), job("f@example.test"))
	if srv.Connections()-before != 2 {
		t.Fatal("NoReuse reused a connection")
	}

	// A connection the server closed while idle is replaced.
	drop, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer drop.Close()
	drop.DropAfterData("x@example.test")
	d2 := &Deliverer{Override: drop.Addr, EHLO: "w.test"}
	defer d2.Close()
	d2.Deliver(context.Background(), job("x@example.test"))
	if res := d2.Deliver(context.Background(), job("y@example.test")); res.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("after a dropped connection: %+v", res)
	}
}

// TestDANEUnusableRecordsRequireTLS (R3, unproven): when every TLSA record
// of an MX host is unusable, TLS is still required (RFC 7672 section 2.2),
// without authentication.
func TestDANEUnusableRecordsRequireTLS(t *testing.T) {
	m := startMXServers(t)
	pkix := TLSARecord{Usage: 1, Selector: 1, MatchingType: 1, Data: make([]byte, 32)}
	tlsa := fakeTLSA{recs: map[string][]TLSARecord{"mx.example.test": {pkix}}}
	res := m.deliverer(m.noTLS, nil, tlsa).Deliver(context.Background(), job("a@example.test"))
	if res.Outcome != protocol.OutcomeDeferred || res.Enhanced != "4.7.5" || !strings.Contains(res.Response, "none is usable") {
		t.Fatalf("no STARTTLS with unusable TLSA records: %+v", res)
	}
	if len(m.noTLS.Messages()) != 0 {
		t.Fatal("sent in plaintext")
	}
	res = m.deliverer(m.selfSig, nil, tlsa).Deliver(context.Background(), job("a@example.test"))
	if res.Outcome != protocol.OutcomeDelivered || res.TLS != "opportunistic" {
		t.Fatalf("unusable TLSA records and a self-signed certificate: %+v", res)
	}
}

// TestDANEMXThroughValidatingResolver (R3, unproven): with DANE on, MX
// records come from the validating resolver, and DANE applies only behind a
// DNSSEC-secure MX answer.
func TestDANEMXThroughValidatingResolver(t *testing.T) {
	s := startDNSSEC(t)
	m := startMXServers(t)
	usable := TLSARecord{Usage: 3, Selector: 1, MatchingType: 1, Data: make([]byte, 32)} // matches nothing
	s.mu.Lock()
	s.mx = map[string]string{"secure.test.": "mx.example.test.", "insecure.test.": "mx.example.test."}
	s.secure["secure.test."] = true
	s.a["mx.example.test."], s.secure["mx.example.test."] = "127.0.0.1", true
	s.tlsa["_25._tcp.mx.example.test."], s.secure["_25._tcp.mx.example.test."] = []TLSARecord{usable}, true
	s.mu.Unlock()

	r := &DNSSECResolver{DANEResolver{Server: s.addr}}
	mxs, secure, err := r.LookupMXSecure(context.Background(), "secure.test")
	if err != nil || !secure || len(mxs) != 1 || mxs[0].Host != "mx.example.test." {
		t.Fatalf("secure MX: %v %v %v", mxs, secure, err)
	}
	if _, secure, err := r.LookupMXSecure(context.Background(), "insecure.test"); err != nil || secure {
		t.Fatalf("insecure MX: %v %v", secure, err)
	}
	if _, _, err := r.LookupMXSecure(context.Background(), "none.test"); !isNotFound(err) {
		t.Fatalf("no MX: %v", err)
	}
	if ips, err := r.LookupIPAddr(context.Background(), "mx.example.test"); err != nil || len(ips) != 1 {
		t.Fatalf("address: %v %v", ips, err)
	}

	d := m.deliverer(m.good, nil, &DANEResolver{Server: s.addr})
	_, port, _ := net.SplitHostPort(m.good.Addr)
	d.Resolver = r
	d.Port = atoi(port)
	sec := job("a@secure.test")
	sec.RcptDomain = "secure.test"
	if res := d.Deliver(context.Background(), sec); res.Enhanced != "4.7.5" {
		t.Fatalf("secure MX, TLSA mismatch: %+v (DANE must apply)", res)
	}
	insec := job("a@insecure.test")
	insec.RcptDomain = "insecure.test"
	if res := d.Deliver(context.Background(), insec); res.Outcome != protocol.OutcomeDelivered {
		t.Fatalf("insecure MX: %+v (DANE must not apply)", res)
	}
	if !s.asked("MX secure.test.") {
		t.Fatal("the MX lookup did not go through the validating resolver")
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// TestSTSRefreshBeforeExpiry (R3, unproven): a cached MTA-STS policy is
// fetched again before it expires, so enforcement never lapses; when the
// refresh fails, the cached policy stays until its max_age.
func TestSTSRefreshBeforeExpiry(t *testing.T) {
	ca, err := smtptest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Issue("mta-sts.example.test")
	if err != nil {
		t.Fatal(err)
	}
	hosts := &policyHosts{bodies: map[string]string{}, fetches: map[string]int{}}
	hosts.set("mta-sts.example.test", "version: STSv1\nmode: enforce\nmx: mx.example.test\nmax_age: 86400\n")
	srv := httptest.NewUnstartedServer(hosts)
	srv.TLS = smtptest.TLSConfig(cert)
	srv.StartTLS()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}}
	dns := &txtMap{m: map[string][]string{}}
	dns.set("_mta-sts.example.test", "v=STSv1; id=1")
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	f := &STSFetcher{Resolver: dns, HTTP: client, Now: func() time.Time { return now }}
	ctx := context.Background()
	if p, err := f.Policy(ctx, "example.test"); err != nil || p == nil {
		t.Fatalf("first fetch: %v %v", p, err)
	}
	now = now.Add(12 * time.Hour) // half way: cached
	f.Policy(ctx, "example.test")
	if n := hosts.count("mta-sts.example.test"); n != 1 {
		t.Fatalf("%d fetches half way through max_age", n)
	}
	now = now.Add(7 * time.Hour) // inside the last quarter: refreshed
	if p, _ := f.Policy(ctx, "example.test"); p == nil || hosts.count("mta-sts.example.test") != 2 {
		t.Fatalf("not refreshed before expiry: %d fetches", hosts.count("mta-sts.example.test"))
	}
	// The refresh restarted max_age: a day later it is still valid, even
	// though the policy host now fails.
	hosts.set("mta-sts.example.test", "")
	now = now.Add(23 * time.Hour)
	if p, _ := f.Policy(ctx, "example.test"); p == nil || p.Mode != STSEnforce {
		t.Fatalf("policy lapsed: %v", p)
	}
}

type selfCheckDNS struct {
	a   map[string][]string
	ptr map[string][]string
	mx  map[string][]*net.MX
}

func (d selfCheckDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	var out []net.IPAddr
	for _, ip := range d.a[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	if out == nil {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

func (d selfCheckDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	if n := d.ptr[addr]; n != nil {
		return n, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
}

func (d selfCheckDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if mx := d.mx[name]; mx != nil {
		return mx, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func TestSelfCheck(t *testing.T) {
	srv, err := smtptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	good := selfCheckDNS{
		a:   map[string][]string{"mail1.example.net": {"203.0.113.7"}},
		ptr: map[string][]string{"203.0.113.7": {"mail1.example.net."}},
	}
	c := (&SelfChecker{EHLO: "mail1.example.net", PublicIP: "203.0.113.7", Resolver: good, SMTP: srv.Addr}).Run(context.Background())
	if !c.OK || len(c.Checks) != 4 {
		t.Fatalf("healthy setup: %+v", c)
	}
	failed := func(c protocol.SelfCheck) map[string]string {
		out := map[string]string{}
		for _, i := range c.Checks {
			if !i.OK {
				out[i.Name] = i.Detail
			}
		}
		return out
	}
	// No PTR, wrong A record, a name that is not an FQDN, port 25 closed.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	bad := selfCheckDNS{a: map[string][]string{"mail1.example.net": {"198.51.100.1"}}, ptr: map[string][]string{}}
	c = (&SelfChecker{EHLO: "mail1.example.net", PublicIP: "203.0.113.7", Resolver: bad, SMTP: closed, Timeout: time.Second}).Run(context.Background())
	f := failed(c)
	if c.OK || !strings.Contains(f["ehlo_a_record"], "198.51.100.1") || !strings.Contains(f["reverse_dns"], "no reverse DNS") ||
		!strings.Contains(f["port25_egress"], "port 25") || f["ehlo_fqdn"] != "" {
		t.Fatalf("broken setup: %+v", f)
	}
	c = (&SelfChecker{EHLO: "localhost", PublicIP: "203.0.113.7", Resolver: good, SMTP: srv.Addr}).Run(context.Background())
	if f := failed(c); f["ehlo_fqdn"] == "" {
		t.Fatalf("localhost passed as an FQDN: %+v", c)
	}
	// The PTR names another host.
	other := good
	other.ptr = map[string][]string{"203.0.113.7": {"ip-203-0-113-7.cloud.example."}}
	c = (&SelfChecker{EHLO: "mail1.example.net", PublicIP: "203.0.113.7", Resolver: other, SMTP: srv.Addr}).Run(context.Background())
	if f := failed(c); !strings.Contains(f["reverse_dns"], "ip-203-0-113-7.cloud.example") {
		t.Fatalf("PTR mismatch: %+v", f)
	}
	// Without SMTP, the MX of the probe domain is used.
	host, _, _ := net.SplitHostPort(srv.Addr)
	viaMX := good
	viaMX.mx = map[string][]*net.MX{"probe.test": {{Host: host + ".", Pref: 5}}}
	sc := &SelfChecker{EHLO: "mail1.example.net", PublicIP: "203.0.113.7", Resolver: viaMX, ProbeDomain: "probe.test", Timeout: time.Second}
	c = sc.Run(context.Background())
	if f := failed(c); !strings.Contains(f["port25_egress"], host+":25") {
		t.Fatalf("port 25 probe through MX: %+v", f)
	}
}

// TestRunnerSelfCheck: a direct worker does not start when its self-check
// fails, unless OMW_SKIP_SELF_CHECK is set; the result goes out in the
// heartbeat.
func TestRunnerSelfCheck(t *testing.T) {
	var beats []protocol.HeartbeatRequest
	var mu sync.Mutex
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			var hb protocol.HeartbeatRequest
			json.NewDecoder(r.Body).Decode(&hb)
			mu.Lock()
			beats = append(beats, hb)
			mu.Unlock()
		}
		w.Write([]byte(`{"jobs":[]}`))
	}))
	defer cp.Close()
	r := testRunner(t, cp, &Deliverer{}, config.Worker{EHLO: "localhost", PublicIP: "203.0.113.7", Concurrency: 1, PollInterval: time.Millisecond})
	r.checker = &SelfChecker{EHLO: "localhost", PublicIP: "203.0.113.7", Resolver: selfCheckDNS{}, SMTP: "127.0.0.1:1", Timeout: time.Second}
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "OMW_SKIP_SELF_CHECK") || !strings.Contains(err.Error(), "ehlo_fqdn") {
		t.Fatalf("Run with a failing self-check: %v", err)
	}

	r.cfg.SkipSelfCheck = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(beats)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(beats) == 0 || beats[0].SelfCheck == nil || !strings.Contains(beats[0].SelfCheck.Skipped, "OMW_SKIP_SELF_CHECK") {
		t.Fatalf("heartbeat self-check: %+v", beats)
	}
}
