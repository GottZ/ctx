package events

import (
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/dispatch"
)

// MW2 herald probes (design/01 §7 W2 gate): QueryStart/QueryEnd delegate to
// the dispatcher's InteractiveArrived/done while activeQueries stays the
// mirror — both counters move from the ONE entry point (B7), so the
// divergence over any test run is zero and every Arrived has its done (no
// herald leak).
//
// Red probe (2026-07-06, documented per wave contract): with the delegation
// block in QueryStart removed (the `done := s.dispatcher.InteractiveArrived()`
// append), TestQueryStartEnd_HeraldDelegation failed with
// "after QueryStart: demand = 0, want 1" and the divergence probe failed at
// the all-started quiesce point — the tests observe the delegation itself,
// not the mirror.

// newHeraldScheduler builds a zero-value scheduler with a live dispatcher
// (default settings, empty policy — pass-through, exactly the MW2 state).
func newHeraldScheduler(t *testing.T) (*Scheduler, *dispatch.Dispatcher) {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s := &Scheduler{}
	s.SetDispatcher(d)
	return s, d
}

// TestQueryStartEnd_HeraldDelegation is the herald probe: entry raises
// InteractiveDemand(), exit lowers it, and the activeQueries mirror moves in
// lockstep — including nested entries (the MCP/chat path re-enters through
// the same wrapped query handler; counter, not bool).
func TestQueryStartEnd_HeraldDelegation(t *testing.T) {
	s, d := newHeraldScheduler(t)

	s.QueryStart()
	if got := d.InteractiveDemand(); got != 1 {
		t.Fatalf("after QueryStart: demand = %d, want 1", got)
	}
	if got := s.activeQueries.Load(); got != 1 {
		t.Fatalf("after QueryStart: activeQueries = %d, want 1", got)
	}

	s.QueryStart() // nested entry (chat turn delegating to ctx_query)
	if got := d.InteractiveDemand(); got != 2 {
		t.Fatalf("after nested QueryStart: demand = %d, want 2", got)
	}

	s.QueryEnd()
	if got := d.InteractiveDemand(); got != 1 {
		t.Fatalf("after first QueryEnd: demand = %d, want 1", got)
	}
	if got := s.activeQueries.Load(); got != 1 {
		t.Fatalf("after first QueryEnd: activeQueries = %d, want 1", got)
	}

	s.QueryEnd()
	if got := d.InteractiveDemand(); got != 0 {
		t.Fatalf("after last QueryEnd: demand = %d, want 0 (herald leak)", got)
	}
	if got := s.activeQueries.Load(); got != 0 {
		t.Fatalf("after last QueryEnd: activeQueries = %d, want 0", got)
	}
}

// TestQueryStartEnd_NilDispatcher pins behavior neutrality for the unwired
// state (tests, boot before SetDispatcher): the mirror keeps working, no
// panic, no herald.
func TestQueryStartEnd_NilDispatcher(t *testing.T) {
	s := &Scheduler{}
	s.QueryStart()
	if got := s.activeQueries.Load(); got != 1 {
		t.Fatalf("activeQueries = %d, want 1", got)
	}
	s.QueryEnd()
	if got := s.activeQueries.Load(); got != 0 {
		t.Fatalf("activeQueries = %d, want 0", got)
	}
}

// TestQueryStartEnd_MirrorDivergenceZero is the W2 negative gate (design/01
// §7 W2): field↔dispatcher divergence == 0 over a concurrent run, checked at
// two quiesce points (all started, all ended). The final zero also proves
// every Arrived had its done — a leaked done would leave demand > 0 and
// permanently starve background admission (the herald term is not softenable).
func TestQueryStartEnd_MirrorDivergenceZero(t *testing.T) {
	s, d := newHeraldScheduler(t)

	const goroutines = 8
	const perG = 50

	// Phase 1: everyone starts.
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				s.QueryStart()
			}
		}()
	}
	wg.Wait()
	if mirror, demand := s.activeQueries.Load(), d.InteractiveDemand(); int(mirror) != demand || demand != goroutines*perG {
		t.Fatalf("all-started quiesce: activeQueries = %d, demand = %d, want both %d", mirror, demand, goroutines*perG)
	}

	// Phase 2: everyone ends (ends pair with arbitrary starts — the done
	// closures are interchangeable, LIFO pop).
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				s.QueryEnd()
			}
		}()
	}
	wg.Wait()
	if mirror, demand := s.activeQueries.Load(), d.InteractiveDemand(); mirror != 0 || demand != 0 {
		t.Fatalf("all-ended quiesce: activeQueries = %d, demand = %d, want both 0", mirror, demand)
	}
}
