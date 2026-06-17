package routing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPoolCache_EvictsOldest(t *testing.T) {
	c := NewPoolCache(2)
	c.pools["a"] = nil
	c.pools["b"] = nil
	c.order = []string{"a", "b"}

	c.mu.Lock()
	c.touchLocked("a")
	c.mu.Unlock()

	if c.order[0] != "b" || c.order[1] != "a" {
		t.Fatalf("order after touch: %v", c.order)
	}
}

func TestPoolCache_GetRequiresDSN(t *testing.T) {
	c := NewPoolCache(1)
	_, err := c.Get(context.Background(), "postgres://invalid:9999/nope?sslmode=disable", GetOptions{InstanceKey: "shared"})
	if err == nil {
		t.Fatal("expected error for invalid dsn")
	}
}

func TestPoolCache_CircuitOpensAfterFailures(t *testing.T) {
	c := NewPoolCache(1).WithCircuitBreaker(1, time.Minute)
	_, _ = c.Get(context.Background(), "postgres://invalid:9999/nope?sslmode=disable", GetOptions{InstanceKey: "premium"})
	_, err := c.Get(context.Background(), "postgres://invalid:9999/nope?sslmode=disable", GetOptions{InstanceKey: "premium"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrInstanceCircuitOpen) {
		t.Fatalf("expected ErrInstanceCircuitOpen, got %v", err)
	}
	st := c.Stats()
	if got := st.Instances["premium"].Status; got != "open_circuit" {
		t.Fatalf("status=%q", got)
	}
}

func TestPoolCache_InstancePoolCap(t *testing.T) {
	c := NewPoolCache(10).WithInstancePoolCaps(1, nil)
	c.mu.Lock()
	is := c.instanceLocked("shared")
	is.OpenPools = 1
	c.mu.Unlock()
	_, err := c.Get(context.Background(), "postgres://invalid:9999/nope?sslmode=disable", GetOptions{InstanceKey: "shared"})
	if err == nil {
		t.Fatal("expected cap error")
	}
	if !errors.Is(err, ErrInstancePoolCapExceeded) {
		t.Fatalf("expected ErrInstancePoolCapExceeded, got %v", err)
	}
}
