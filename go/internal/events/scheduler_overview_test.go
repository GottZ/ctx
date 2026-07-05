package events

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestYieldThenRebuildOverview_DefersToActiveQueries is the B-W1 gate for the
// dispatch yield (B2-M1): with an active query the rebuild is deferred — the
// rebuild path is never reached before ctx expiry. The zero-value Scheduler
// (nil cfg) makes the assertion mechanical: reaching rebuildOverviewOnce would
// panic on cfg.Snapshot(), so a clean return proves the deferral.
func TestYieldThenRebuildOverview_DefersToActiveQueries(t *testing.T) {
	s := &Scheduler{}
	s.activeQueries.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.yieldThenRebuildOverview(ctx, backgroundTenant{scope: store.GlobalScope})
	}()
	select {
	case <-done: // returned without panicking ⇒ rebuild never dispatched
	case <-time.After(5 * time.Second):
		t.Fatal("yield loop did not exit on ctx cancellation")
	}
}

// TestYieldThenRebuildOverview_DispatchesWhenIdle is the red probe: with no
// active queries the rebuild path IS reached — observable as the cfg.Snapshot()
// panic on the zero-value Scheduler. If this stops panicking, the yield guard
// has started swallowing the dispatch and the gate above proves nothing.
func TestYieldThenRebuildOverview_DispatchesWhenIdle(t *testing.T) {
	s := &Scheduler{}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected rebuild dispatch (panic on nil cfg), got clean return — dispatch path unreachable?")
		}
	}()
	s.yieldThenRebuildOverview(context.Background(), backgroundTenant{scope: store.GlobalScope})
}

// TestNextOverviewTenant_RoundRobin pins the B-W1 foundation: the cursor
// rotates over the authoritative tenant list one tenant per tick (runDreamLoop
// pattern), and an empty list degrades to the global single pass.
func TestNextOverviewTenant_RoundRobin(t *testing.T) {
	s := &Scheduler{
		backgroundTenantsFn: func(context.Context) []backgroundTenant {
			return []backgroundTenant{{scope: "alpha"}, {scope: "beta"}}
		},
	}
	var cursor uint64
	got := []string{
		s.nextOverviewTenant(context.Background(), &cursor).scope,
		s.nextOverviewTenant(context.Background(), &cursor).scope,
		s.nextOverviewTenant(context.Background(), &cursor).scope,
	}
	want := []string{"alpha", "beta", "alpha"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin pick %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return nil }
	cursor = 0
	if bt := s.nextOverviewTenant(context.Background(), &cursor); bt.scope != store.GlobalScope {
		t.Fatalf("empty tenant list: scope = %q, want global fallback %q", bt.scope, store.GlobalScope)
	}
}
