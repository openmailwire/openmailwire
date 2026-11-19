package main

import (
	"net/http"
	"testing"
	"time"
)

// Audit R4#5: the control plane set only ReadHeaderTimeout, so a client
// could hold a connection open forever by sending its body slowly, or by
// never reading the answer.
func TestAuditR4HTTPServerLimits(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 {
		t.Fatalf("limits missing: %+v", s)
	}
	// Long enough for the longest handler (an MCP wait of 120 s) and for a
	// 20 MB body at about 1 Mbit/s; WriteTimeout covers the body read too.
	if s.ReadTimeout < 150*time.Second || s.WriteTimeout < s.ReadTimeout || s.MaxHeaderBytes > 1<<20 {
		t.Fatalf("read %s write %s max header %d", s.ReadTimeout, s.WriteTimeout, s.MaxHeaderBytes)
	}
}
