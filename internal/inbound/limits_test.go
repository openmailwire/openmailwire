package inbound

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/protocol"
)

// Audit R2 M5: one connection made 3,001 recipient checks, each a request
// to the control plane, and was never closed; it doubled as an oracle for
// which addresses exist. Now the connection closes after MaxUnknownRcpts
// refusals.
func TestRcptFloodClosesConnection(t *testing.T) {
	fc, addr := start(t, Config{})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	r.ReadString('\n')
	fmt.Fprintf(c, "HELO x\r\nMAIL FROM:<a@b.test>\r\n")
	r.ReadString('\n')
	r.ReadString('\n')
	go func() {
		w := bufio.NewWriter(c)
		for i := 0; i < 3000; i++ {
			fmt.Fprintf(w, "RCPT TO:<guess%d@x.test>\r\n", i)
		}
		fmt.Fprintf(w, "RCPT TO:<ok@x.test>\r\n")
		w.Flush()
	}()
	var replies []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}
		replies = append(replies, strings.TrimSpace(line))
	}
	if len(replies) != 11 || !strings.HasPrefix(replies[9], "550 5.1.1") || !strings.HasPrefix(replies[10], "421 4.7.0") {
		t.Fatalf("%d replies, last %q", len(replies), replies[len(replies)-1])
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.checks > 10 {
		t.Fatalf("%d control-plane checks", fc.checks)
	}
}

// Audit R2 L1: one client could hold every connection slot.
func TestConnectionCapPerIP(t *testing.T) {
	_, addr := start(t, Config{MaxConns: 50, MaxConnsPerIP: 3})
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		if l, _ := bufio.NewReader(c).ReadString('\n'); !strings.HasPrefix(l, "220 ") {
			t.Fatalf("connection %d: %q", i, l)
		}
	}
	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	if l, _ := bufio.NewReader(c).ReadString('\n'); !strings.HasPrefix(l, "421 4.7.0") {
		t.Fatalf("4th connection from one address: %q", l)
	}
	// Closing one frees its slot.
	conns[0].Close()
	time.Sleep(100 * time.Millisecond)
	c2, _ := net.Dial("tcp", addr)
	defer c2.Close()
	if l, _ := bufio.NewReader(c2).ReadString('\n'); !strings.HasPrefix(l, "220 ") {
		t.Fatalf("after a slot freed: %q", l)
	}
}

// Audit R2 L1: before MAIL, a client gets IdleTimeout per command, not
// the 10 minutes that DATA needs.
func TestIdleTimeoutBeforeMail(t *testing.T) {
	_, addr := start(t, Config{IdleTimeout: 300 * time.Millisecond, ReadTimeout: 5 * time.Second})
	idle := func(mail bool) time.Duration {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		r := bufio.NewReader(c)
		r.ReadString('\n')
		fmt.Fprintf(c, "EHLO x\r\n")
		for {
			if l, _ := r.ReadString('\n'); !strings.HasPrefix(l, "250-") {
				break
			}
		}
		if mail {
			fmt.Fprintf(c, "MAIL FROM:<a@b.test>\r\n")
			r.ReadString('\n')
		}
		start := time.Now()
		for {
			if _, err := r.ReadString('\n'); err != nil {
				break
			}
		}
		return time.Since(start)
	}
	if d := idle(false); d > 2*time.Second {
		t.Fatalf("idle before MAIL lasted %s", d)
	}
	if d := idle(true); d < 2*time.Second {
		t.Fatalf("after MAIL the connection was dropped after %s", d)
	}
}

func TestRcptCacheAndRate(t *testing.T) {
	fc, addr := start(t, Config{RcptChecksPerMinute: 1, RcptCheckBurst: 3})
	for i := 0; i < 3; i++ {
		if err := send(addr, false, []string{"ok@x.test"}, "Subject: x\r\n\r\nx\r\n"); err != nil {
			t.Fatal(err)
		}
		if err := send(addr, false, []string{"nobody@x.test"}, "x"); code(err) != 550 {
			t.Fatalf("unknown: %v", err)
		}
	}
	fc.mu.Lock()
	n := fc.checks
	fc.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d checks for two addresses", n)
	}
	// One more check fits the burst, then the address must slow down.
	send(addr, false, []string{"third@x.test"}, "x")
	if err := send(addr, false, []string{"fourth@x.test"}, "x"); code(err) != 450 || !strings.Contains(err.Error(), "4.7.1") {
		t.Fatalf("over the rate: %v", err)
	}
}

// The control plane's per-recipient outcome reaches the sender: a
// recipient refused at DATA (an inbox deleted while the node still had it
// cached) refuses the message, and a deferred one asks for a retry.
func TestOutcomeAtData(t *testing.T) {
	var outcome atomic.Value
	outcome.Store("stored")
	checks := atomic.Int64{}
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/inbound/check":
			checks.Add(1)
			json.NewEncoder(w).Encode(protocol.InboundCheckResponse{Accept: true, Kind: "inbox"})
		case "/internal/inbound":
			var m protocol.InboundMessage
			json.NewDecoder(r.Body).Decode(&m)
			json.NewEncoder(w).Encode(protocol.InboundResponse{Results: []protocol.InboundResult{{Rcpt: m.Rcpts[0], Outcome: outcome.Load().(string)}}})
		}
	}))
	defer cp.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := New(Config{Hostname: "mx.test"}, &Client{URL: cp.URL, Token: "omwk_test", HTTP: cp.Client()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go srv.Serve(ln)
	defer srv.Close()
	addr := ln.Addr().String()
	for _, c := range []struct {
		outcome string
		code    int
	}{{"stored", 0}, {"duplicate", 0}, {"deferred", 451}, {"rejected", 550}} {
		outcome.Store(c.outcome)
		if err := send(addr, false, []string{"a@x.test"}, "Subject: x\r\n\r\nx\r\n"); code(err) != c.code {
			t.Fatalf("%s: %v", c.outcome, err)
		}
	}
	// The refused address left the cache, so it is checked again.
	before := checks.Load()
	send(addr, false, []string{"a@x.test"}, "Subject: x\r\n\r\nx\r\n")
	if checks.Load() != before+1 {
		t.Fatal("a recipient refused at DATA stayed cached")
	}
}
