package api

import (
	"context"
	"testing"
	"time"

	"github.com/openmailwire/openmailwire/internal/config"
)

// The control plane processes at most InboundConcurrency inbound messages
// at once; the next waits, and gives up when its request ends.
func TestInboundSlots(t *testing.T) {
	s := &Server{Cfg: config.Control{InboundConcurrency: 2}}
	r1, ok1 := s.inboundSlot(context.Background())
	r2, ok2 := s.inboundSlot(context.Background())
	if !ok1 || !ok2 {
		t.Fatal("slots within the limit refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, ok := s.inboundSlot(ctx); ok {
		t.Fatal("a third slot was granted")
	}
	r1()
	if r3, ok := s.inboundSlot(context.Background()); !ok {
		t.Fatal("a freed slot was not reused")
	} else {
		r3()
	}
	r2()
	// Another server has its own slots.
	other := &Server{Cfg: config.Control{InboundConcurrency: 1}}
	if r, ok := other.inboundSlot(context.Background()); !ok {
		t.Fatal("servers share slots")
	} else {
		r()
	}
}
