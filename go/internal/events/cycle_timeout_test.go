package events

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cycleTimeoutConfig is captureTestConfig with an explicit whole-cycle
// deadline. It re-runs the Validate self-check so a fixture below the V16c
// floor could never pass as a "clean" generation here.
func cycleTimeoutConfig(t *testing.T, cycle time.Duration) *config.Config {
	t.Helper()
	c := captureTestConfig(t, 12)
	c.Dream.CycleTimeout = cycle
	if issues := config.Validate(c); config.HasErrors(issues) {
		t.Fatalf("cycle-timeout fixture is not Validate-clean: %+v", issues)
	}
	return c
}

// TestDreamCycleDeadlineTracksHotCycleTimeout pins the two REMAINING
// consumption sites of dream.cycle_timeout — the ones the dream package
// cannot see — plus the hot-reload property in one test:
//
//  1. newRouter (scheduler.go) copies cfg.Dream.CycleTimeout onto the router
//     the cycle is handed. Mutation killed: drop the field from the literal
//     and r.CycleTimeout is 0 on cycle 1.
//  2. runDreamCycle derives the outer, Background-rooted cycle context from
//     the same effective value. Mutation killed: revert it to the bare
//     dream.CycleTimeout constant and the observed budget is 700s, not 2400s.
//  3. Both are read per cycle, not once at boot. Mutation killed: hoist
//     either read out of the loop and cycle 2 still observes generation A.
//
// Shape follows TestDreamLoopSeesReplacedConfigNextCycle: the runCycle seam
// stands in for the DB-bound pipeline, holds each cycle open on a channel so
// the config generation can be flipped strictly BETWEEN cycles, and the
// pool/config axes come from the same dreamPoolRow/captureTestConfig
// fixtures. No wire call is needed here — the deadline and the router field
// are the whole observation.
func TestDreamCycleDeadlineTracksHotCycleTimeout(t *testing.T) {
	cfgA := cycleTimeoutConfig(t, 2400*time.Second)
	cfgB := cycleTimeoutConfig(t, 900*time.Second)

	st := config.NewStore(cfgA)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow("http://dream.example", "dream-model-a")})
	s := NewScheduler(deadPool(t), st, bpool, StartupConfig{})

	type deadlineObs struct {
		budget     time.Duration // time.Until(ctx.Deadline()) as the cycle saw it
		router     time.Duration // dream.Router.CycleTimeout the scheduler built
		noDeadline bool
	}
	got := make(chan deadlineObs, 4)
	release := make(chan struct{})
	s.runCycle = func(ctx context.Context, _ *pgxpool.Pool, r *dream.Router, _ llm.Options,
		_ dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		obs := deadlineObs{router: r.CycleTimeout}
		if dl, ok := ctx.Deadline(); ok {
			obs.budget = time.Until(dl)
		} else {
			obs.noDeadline = true
		}
		got <- obs
		<-release // hold the cycle open until the test releases it
		return 1, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runDreamLoop(ctx)
		close(done)
	}()

	waitObs := func(stage string) deadlineObs {
		t.Helper()
		select {
		case o := <-got:
			return o
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: no dream cycle within 15s", stage)
			return deadlineObs{}
		}
	}

	// A cycle budget is measured from inside the cycle, so it is always a
	// little below the configured value. 5s of tolerance keeps the assertion
	// robust without letting the 700s constant pass for 2400s.
	const tol = 5 * time.Second
	assertBudget := func(stage string, o deadlineObs, want time.Duration) {
		t.Helper()
		if o.noDeadline {
			t.Fatalf("%s: the dream cycle context carries no deadline at all", stage)
		}
		if o.budget > want || o.budget < want-tol {
			t.Errorf("%s: cycle budget = %v, want %v (±%v) — the outer context is not reading the effective cycle timeout",
				stage, o.budget, want, tol)
		}
		if o.router != want {
			t.Errorf("%s: router.CycleTimeout = %v, want %v — newRouter is not wiring the key",
				stage, o.router, want)
		}
	}

	// Cycle 1 runs on generation A: 2400s, four times the 700s constant, so
	// a scheduler still reading the constant cannot pass this.
	assertBudget("cycle 1", waitObs("cycle 1"), 2400*time.Second)

	// Flip the config strictly between the cycles, then let cycle 1 return.
	if err := st.Replace(cfgB); err != nil {
		t.Fatalf("store.Replace(cfgB): %v", err)
	}
	release <- struct{}{}

	// Cycle 2 MUST run on generation B — a boot-time copy of either read
	// would still observe 2400s here.
	assertBudget("cycle 2", waitObs("cycle 2"), 900*time.Second)

	// Shut the loop down: cancel first, then release cycle 2 — the loop's
	// next shutdown check exits before any third cycle.
	cancel()
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}
}
