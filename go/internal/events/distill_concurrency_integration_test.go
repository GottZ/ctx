//go:build integration

// Gate C6-A (design/02 §4.8, wave C6-A): the tick's SOURCE fan-out. Three
// properties, and the middle one is what makes the other two safe to have:
//
//   - at distill.concurrency = N several sources of one tick run at the same
//     time, and never more than N of them;
//   - at 1 the arm is byte-for-byte the sequential loop of every wave before
//     this one, including the ORDER its candidates are walked in;
//   - a single source is never in two workers at once, whatever the candidate
//     list looks like — the watermark of a run is only moved by a complete
//     batch prefix, so two workers on one root would be two writers on one
//     watermark series.
//
// RED, measured before the fan-out landed (the config key already in place, the
// tick still walking `for _, ref := range refs`): TestDistillConcurrency/Parallel
// observed max_in_flight = 1 against the wanted 4, and /PerSourceSerial the same
// 1 — the probe cannot tell a bounded pool from a serial loop when there is no
// pool, which is exactly the contrast this file exists to make visible.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestDistillConcurrency -count=1 -v
//	go test -tags=integration -race ./internal/events/ -run TestDistillConcurrency -count=1
package events

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
)

// dcProbe is the occupancy meter of the fan-out: how many sources were inside
// the source at the same time, how many workers ever shared ONE root, and in
// which order the roots were entered.
//
// It is a counter around the reader's own calls rather than a hook in the arm,
// because the property under test is observable from outside: whoever holds the
// source is doing that root's work.
type dcProbe struct {
	mu       sync.Mutex
	cur      int
	max      int
	perRoot  map[string]int
	maxRoot  int
	order    []string
	hold     time.Duration
	entered  chan struct{}
	sessions int
}

func newDCProbe(hold time.Duration) *dcProbe {
	return &dcProbe{perRoot: map[string]int{}, hold: hold, entered: make(chan struct{}, 64)}
}

func (p *dcProbe) enter(root string) {
	p.mu.Lock()
	p.cur++
	if p.cur > p.max {
		p.max = p.cur
	}
	p.perRoot[root]++
	if p.perRoot[root] > p.maxRoot {
		p.maxRoot = p.perRoot[root]
	}
	p.order = append(p.order, root)
	p.mu.Unlock()
	select {
	case p.entered <- struct{}{}:
	default:
	}
	// The hold is what makes an overlap OBSERVABLE at all: without it two
	// workers can walk their roots in lockstep and never be inside the meter at
	// the same microsecond, which would make a green run indistinguishable from
	// a lucky one.
	time.Sleep(p.hold)
}

func (p *dcProbe) leave(root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cur--
	p.perRoot[root]--
}

func (p *dcProbe) read() (maxInFlight, maxPerRoot int, order []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max, p.maxRoot, append([]string(nil), p.order...)
}

// dcSource is a reader of its own rather than an embedding of
// fakeDistillSource: that one counts its calls in plain fields, and a shared
// counter under a fan-out is a data race the -race run would report as the
// TEST's bug rather than the arm's.
type dcSource struct {
	refs  []distillsource.Ref
	probe *dcProbe
}

func (d *dcSource) Label() string { return dfLabel }

func (d *dcSource) Sessions(context.Context) ([]distillsource.Ref, error) {
	return d.refs, nil
}

// Head is the meter's gate: it is the first per-source call distillSession
// makes and it runs once per root, so the occupancy it measures is the
// occupancy of the fan-out itself.
func (d *dcSource) Head(_ context.Context, sess string) (int64, error) {
	d.probe.enter(sess)
	defer d.probe.leave(sess)
	return 100, nil
}

func (d *dcSource) HasNew(context.Context, string, int64) (bool, error) { return true, nil }

// Read answers the batch loop with an exhausted range at the caller's own
// watermark: this gate is about the fan-out over sources, not about what a
// source's batches do, and a run that ends right after its first read keeps the
// probe's numbers about the pool.
func (d *dcSource) Read(_ context.Context, sess string, after int64, _, _ int) (distillsource.Batch, error) {
	d.probe.enter(sess)
	defer d.probe.leave(sess)
	return distillsource.Batch{Watermark: after, Complete: true}, nil
}

func (d *dcSource) QuietFor(context.Context, string, time.Time) (time.Duration, error) {
	return 0, distillsource.ErrNoActiveRows
}

func (d *dcSource) Close() error { return nil }

// dcRefs builds n distinct root ids in the shape the arm's character class
// admits (distillMetaString) — an unnameable root would be answered before the
// fan-out and never reach the meter.
func dcRefs(n int) []distillsource.Ref {
	out := make([]distillsource.Ref, 0, n)
	for i := range n {
		out = append(out, distillsource.Ref{Session: fmt.Sprintf("20260830_1200%02d_c6a", i)})
	}
	return out
}

func TestDistillConcurrency(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The occupancy hold. 40 ms is far above the microseconds a probe entry
	// costs and far below the test's own runtime; the assertions are on the
	// MAXIMUM, so a slow container can only ever make the pool look emptier,
	// never fuller — the direction that fails loudly instead of passing by luck.
	const hold = 40 * time.Millisecond

	t.Run("Parallel", func(t *testing.T) {
		dfTruncate(t, pool)
		probe := newDCProbe(hold)
		src := &dcSource{refs: dcRefs(8), probe: probe}
		cfg := dfConfig()
		cfg.Distill.Concurrency = 4
		s := dfScheduler(pool, cfg, src)

		if !s.distillOnce(ctx, dfNoDemand) {
			t.Fatal("the tick did not reach its sources")
		}
		maxInFlight, _, order := probe.read()
		if maxInFlight != 4 {
			t.Fatalf("max sources in flight = %d, want exactly 4 (the configured bound) — 1 means the tick is still the serial loop, more than 4 means the pool is unbounded", maxInFlight)
		}
		if len(order) < 8 {
			t.Fatalf("the meter saw %d entries, want at least 8 — one Head per root", len(order))
		}
	})

	t.Run("SerialDefault", func(t *testing.T) {
		dfTruncate(t, pool)
		probe := newDCProbe(hold)
		refs := dcRefs(6)
		src := &dcSource{refs: refs, probe: probe}
		cfg := dfConfig()
		cfg.Distill.Concurrency = 1
		s := dfScheduler(pool, cfg, src)

		if !s.distillOnce(ctx, dfNoDemand) {
			t.Fatal("the tick did not reach its sources")
		}
		maxInFlight, _, order := probe.read()
		if maxInFlight != 1 {
			t.Fatalf("max sources in flight = %d at concurrency 1, want 1 — the default must be the sequential arm, not a pool of one", maxInFlight)
		}
		// The ORDER is half of "identical to before the wave": the arm walks its
		// candidate list in the order the source handed it over, and a fan-out
		// that reorders at 1 would move which source spends a shared ceiling
		// first.
		for i, ref := range refs {
			// Head then Read per root, so the meter's entries come in pairs.
			if got := order[2*i]; got != ref.Session {
				t.Fatalf("entry %d = %q, want %q — the candidate order changed at concurrency 1", 2*i, got, ref.Session)
			}
		}
	})

	t.Run("PerSourceSerial", func(t *testing.T) {
		dfTruncate(t, pool)
		probe := newDCProbe(hold)
		// The same four roots three times over, GROUPED (a a a b b b …) — the
		// shape a source implementation outside ctxcheckpoint (whose GROUP BY
		// cannot produce a duplicate) is free to hand over. Grouped rather than
		// interleaved is load-bearing (review C6-A major 1): with interleaved
		// duplicates and four workers over four roots the collision this test
		// exists for cannot arise, and a chain gate reduced to "one ref, one
		// chain" stays green. Grouped, that regression turns the assertion red.
		var refs []distillsource.Ref
		for _, r := range dcRefs(4) {
			for range 3 {
				refs = append(refs, r)
			}
		}
		src := &dcSource{refs: refs, probe: probe}
		cfg := dfConfig()
		cfg.Distill.Concurrency = 4
		s := dfScheduler(pool, cfg, src)

		if !s.distillOnce(ctx, dfNoDemand) {
			t.Fatal("the tick did not reach its sources")
		}
		maxInFlight, maxPerRoot, _ := probe.read()
		if maxPerRoot != 1 {
			t.Fatalf("max workers on ONE root = %d, want 1 — two workers on one root are two writers on one watermark series", maxPerRoot)
		}
		if maxInFlight < 2 {
			t.Fatalf("max sources in flight = %d, want > 1 — with 4 distinct roots the per-root guarantee must not cost the fan-out", maxInFlight)
		}
	})
}
