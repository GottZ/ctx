//go:build integration

// Gate for review C6-B finding m4: a tick that gate 2 turns away for
// interactive demand must be retried shortly, not after the idle fallback. By
// the time gate 2 answers, the wake window that summoned the tick is drained —
// without the retry the case this arm exists for (a compaction followed at once
// by the user's next query) would wait a whole distill.interval.
package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestC6BDemandDeferRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	prev := distillDemandRetry
	distillDemandRetry = 300 * time.Millisecond
	t.Cleanup(func() { distillDemandRetry = prev })

	t.Run("OwedTickRunsWhenDemandEnds", func(t *testing.T) {
		dfTruncate(t, pool)
		c6bClearCheckpoints(t, pool)
		cfg := dfConfig()
		cfg.Distill.Interval = 30 * time.Second // the fallback must never be what ends this test
		s := dfScheduler(pool, cfg, nil)

		// Busy for the first two answers, free from the third on.
		var asked atomic.Int32
		demand := func() int {
			if asked.Add(1) <= 2 {
				return 1
			}
			return 0
		}

		start := time.Now()
		if !s.distillTick(context.Background(), demand) {
			t.Fatal("distillTick reported shutdown on a live context")
		}
		elapsed := time.Since(start)

		if got := asked.Load(); got != 3 {
			t.Fatalf("gate 2 was asked %d times, want 3 (deferred, deferred, ran)", got)
		}
		// Two retries at 300 ms — well inside the budget and nowhere near the
		// 30 s fallback the pre-fix arm would have waited for.
		if elapsed < 2*distillDemandRetry || elapsed > 5*distillDemandRetry {
			t.Fatalf("owed tick ran after %v, want ≈ 2×%v", elapsed, distillDemandRetry)
		}

		// THE JOURNAL STAYS QUIET across retries: the demand skip obeys the
		// state-change rule, so two deferrals in a row write ONE row, not two.
		var demandRows int
		for _, r := range dfRows(t, pool) {
			if r.skipReason == distillSkipDemand {
				demandRows++
			}
		}
		if demandRows != 1 {
			t.Fatalf("demand skip rows = %d, want exactly 1 (state-change rule)", demandRows)
		}
	})

	t.Run("ShutdownEndsTheRetry", func(t *testing.T) {
		dfTruncate(t, pool)
		c6bClearCheckpoints(t, pool)
		s := dfScheduler(pool, dfConfig(), nil)
		alwaysBusy := func() int { return 1 }

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool, 1)
		go func() { done <- s.distillTick(ctx, alwaysBusy) }()
		time.Sleep(2 * distillDemandRetry)
		cancel()
		select {
		case ran := <-done:
			if ran {
				t.Fatal("distillTick reported a completed tick on a cancelled context")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("distillTick did not return after cancel — the retry loop leaks on shutdown")
		}
	})
}
