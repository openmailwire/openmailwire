package api

import (
	"testing"
	"time"
)

func TestDNSRunBudget(t *testing.T) {
	var b dnsRunBudget
	now := time.Unix(1_700_000_000, 0)
	for i := range dnsRunBurst {
		if ok, _ := b.allow("d1", now); !ok {
			t.Fatalf("run %d refused", i+1)
		}
	}
	ok, wait := b.allow("d1", now)
	if ok || wait <= 0 || wait > dnsRunEvery {
		t.Fatalf("over the burst: %v %s", ok, wait)
	}
	if ok, _ := b.allow("d2", now); !ok {
		t.Fatal("another domain shares the budget")
	}
	if ok, _ := b.allow("d1", now.Add(dnsRunEvery)); !ok {
		t.Fatal("refused after waiting")
	}
	if ok, _ := b.allow("d1", now.Add(dnsRunEvery)); ok {
		t.Fatal("two runs for one refill")
	}
}
