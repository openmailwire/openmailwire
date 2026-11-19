package smtplimit

import (
	"net"
	"testing"
	"time"
)

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1:25": "192.0.2.1", "[::ffff:192.0.2.1]:25": "192.0.2.1",
		"[2001:db8:1:2:3:4:5:6]:25": "2001:db8:1:2::/64", "[2001:db8:1:2::9]:587": "2001:db8:1:2::/64",
	} {
		a, _ := net.ResolveTCPAddr("tcp", in)
		if got := Key(a); got != want {
			t.Errorf("Key(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestWindowAndLimiter(t *testing.T) {
	w := &Window{Span: 100 * time.Millisecond}
	w.Add("a", 1)
	if n := w.Add("a", 2); n != 3 || w.Count("b") != 0 {
		t.Fatalf("window: %d", n)
	}
	time.Sleep(150 * time.Millisecond)
	if n := w.Count("a"); n != 0 {
		t.Fatalf("after the window: %d", n)
	}

	l := &Limiter{Rate: 10, Burst: 2}
	if !l.Allow("x") || !l.Allow("x") || l.Allow("x") {
		t.Fatal("burst")
	}
	if !l.Allow("y") {
		t.Fatal("keys are separate")
	}
	time.Sleep(150 * time.Millisecond)
	if !l.Allow("x") {
		t.Fatal("no refill")
	}
}

// The tables stay bounded under a flood from many addresses.
func TestTablesBounded(t *testing.T) {
	w := &Window{Span: time.Hour}
	l := &Limiter{Rate: 1, Burst: 1}
	for i := 0; i < maxKeys+10; i++ {
		k := net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String()
		w.Add(k, 1)
		l.Allow(k)
	}
	if len(w.m) > maxKeys || len(l.m) > maxKeys {
		t.Fatalf("window %d, limiter %d keys", len(w.m), len(l.m))
	}
}
