package submission

import (
	"bufio"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

// Audit R2 L2: submission had no connection cap and no lifetime: 400
// unauthenticated connections from one address all stayed open.
func TestConnectionCaps(t *testing.T) {
	f := start(t, func(c *Config) { c.MaxConns = 50; c.MaxConnsPerIP = 3; c.MaxConnDuration = 500 * time.Millisecond })
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	greet := func(addr string, implicitTLS bool) string {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if implicitTLS {
			tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
			if err := tc.Handshake(); err != nil {
				return "handshake: " + err.Error()
			}
			l, _ := bufio.NewReader(tc).ReadString('\n')
			return l
		}
		l, _ := bufio.NewReader(c).ReadString('\n')
		return l
	}
	// Both ports share one per-address budget.
	for i, port := range []string{f.addr, f.addr, f.tlsAddr} {
		if l := greet(port, port == f.tlsAddr); !strings.HasPrefix(l, "220 ") {
			t.Fatalf("connection %d: %q", i+1, l)
		}
	}
	if l := greet(f.addr, false); !strings.HasPrefix(l, "421 4.7.0") {
		t.Fatalf("4th connection from one address: %q", l)
	}
	// Every connection ends with its lifetime, however quiet.
	start := time.Now()
	r := bufio.NewReader(conns[0])
	for {
		if _, err := r.ReadString('\n'); err != nil {
			break
		}
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("connection outlived MaxConnDuration")
	}
}

// Audit R2 L2: failed logins were counted per connection only, so a
// client could guess forever by reconnecting. Now one address gets
// MaxAuthFailuresPerIP failures per window, over all its connections;
// after that even the right key is refused.
func TestAuthFailuresPerAddress(t *testing.T) {
	f := start(t, func(c *Config) { c.MaxAuthFailures = 3; c.MaxAuthFailuresPerIP = 4 })
	failures := 0
	for conn := 0; conn < 3 && failures < 4; conn++ {
		c := dial(t, f.addr)
		for i := 0; i < 2 && failures < 4; i++ {
			if code, msg := authPlain(t, c, "omw_wrong"); code != 535 {
				t.Fatalf("guess %d: %d %q", failures+1, code, msg)
			}
			failures++
		}
	}
	c := dial(t, f.addr)
	if code, msg := authPlain(t, c, "omw_good"); code != 454 || !strings.Contains(msg, "Too many failed logins") {
		t.Fatalf("login after %d failures from the address: %d %q", failures, code, msg)
	}
	if code, _, _ := c.Text.ReadResponse(0); code != 421 {
		t.Fatalf("not closed: %d", code)
	}
}

// Before AUTH, a client gets IdleTimeout per command.
func TestIdleBeforeLogin(t *testing.T) {
	f := start(t, func(c *Config) { c.IdleTimeout = 300 * time.Millisecond })
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	start := time.Now()
	for {
		if _, err := r.ReadString('\n'); err != nil {
			break
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("an idle unauthenticated client stayed %s", d)
	}
}
