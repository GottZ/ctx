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

// numPredictConfig is captureTestConfig with an explicit output cap, re-run
// through Validate so a fixture that V18 would reject (or warn on) could never
// pass as a "clean" generation here.
func numPredictConfig(t *testing.T, numPredict int) *config.Config {
	t.Helper()
	c := captureTestConfig(t, 12)
	c.Dream.NumPredict = numPredict
	if issues := config.Validate(c); config.HasErrors(issues) {
		t.Fatalf("num-predict fixture is not Validate-clean: %+v", issues)
	}
	return c
}

// TestDreamOptionsTrackHotNumPredict pins the consumption site of
// dream.num_predict that neither the config nor the dream package can see —
// the scheduler building the cycle's llm.Options — plus the hot-reload
// property:
//
//  1. runDreamCycle resolves the options from the cycle's config snapshot.
//     Mutation killed: revert it to the bare dream.DreamOptions() (or let
//     DreamOptionsFor ignore its argument) and cycle 1 observes 600, not 900.
//  2. The read happens per cycle, not once at boot. Mutation killed: hoist the
//     snapshot or the resolver out of the loop and cycle 2 still observes 900.
//  3. The sentinel round-trips through the whole wiring: generation B sets the
//     key back to 0, and the value that reaches the pipeline is the package
//     default, not a 0 that would mean "uncapped" to noteCapHit and to every
//     backend on the wire.
//
// The observed value is the one BOTH DreamOptions consumers receive — the link
// evaluation and the recurrence confirm share this options value for the whole
// cycle (dream.RunDreamCycle hands it to DetectRecurrence unchanged) — which is
// the documented scope of the key.
//
// Shape follows TestDreamCycleDeadlineTracksHotCycleTimeout: the runCycle seam
// stands in for the DB-bound pipeline and holds each cycle open on a channel so
// the config generation can be flipped strictly BETWEEN cycles.
func TestDreamOptionsTrackHotNumPredict(t *testing.T) {
	cfgA := numPredictConfig(t, 900)
	cfgB := numPredictConfig(t, 0) // back to the sentinel: expect the package default

	st := config.NewStore(cfgA)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow("http://dream.example", "dream-model-a")})
	s := NewScheduler(deadPool(t), st, bpool, StartupConfig{})

	got := make(chan llm.Options, 4)
	release := make(chan struct{})
	s.runCycle = func(_ context.Context, _ *pgxpool.Pool, _ *dream.Router, opts llm.Options,
		_ dream.BackoffConfig, _ []string, _ dream.Throttle) (int, error) {
		got <- opts
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

	waitOpts := func(stage string) llm.Options {
		t.Helper()
		select {
		case o := <-got:
			return o
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: no dream cycle within 15s", stage)
			return llm.Options{}
		}
	}

	assertCap := func(stage string, o llm.Options, want int) {
		t.Helper()
		if o.NumPredict != want {
			t.Errorf("%s: opts.NumPredict = %d, want %d — the scheduler is not wiring dream.num_predict", stage, o.NumPredict, want)
		}
		// The sampling tuple is not this key's business; a scheduler that
		// built the options by hand instead of through the resolver would
		// pass the cap assertion and fail here.
		if base := dream.DreamOptions(); o.Temperature != base.Temperature || o.TopP != base.TopP || o.TopK != base.TopK {
			t.Errorf("%s: sampling tuple = %+v, want the DreamOptions tuple %+v", stage, o, base)
		}
	}

	// Cycle 1 runs on generation A: 900, well clear of the 600 default, so a
	// scheduler still reading the constant cannot pass this.
	assertCap("cycle 1", waitOpts("cycle 1"), 900)

	// Flip the config strictly between the cycles, then let cycle 1 return.
	if err := st.Replace(cfgB); err != nil {
		t.Fatalf("store.Replace(cfgB): %v", err)
	}
	release <- struct{}{}

	// Cycle 2 MUST run on generation B — and the sentinel resolves to the
	// package default, never to a literal 0.
	assertCap("cycle 2", waitOpts("cycle 2"), dream.DefaultNumPredict)

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
