//go:build integration

// Issue #366, point 2: two ctxd instances on one database and the distiller's
// run journal. Instance A is held inside a run (its first Read blocks); B
// starts, or ticks, against the same journal.
//
//	go test -tags=integration ./internal/events/ -run 'TestDistillMultiInstance' -count=1 -v
package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
)

// dmHeldSource is a source whose first Read blocks until proceed is closed;
// entered is closed when it starts blocking.
type dmHeldSource struct {
	*fakeDistillSource
	entered, proceed chan struct{}
}

func newDMHeldSource(root string) *dmHeldSource {
	h := &dmHeldSource{entered: make(chan struct{}), proceed: make(chan struct{})}
	reads := 0
	h.fakeDistillSource = &fakeDistillSource{
		sessions: []distillsource.Ref{{Session: root}},
		head:     map[string]int64{root: 1000},
		hasNew:   map[string]bool{root: true},
	}
	h.readFn = func(after int64) (distillsource.Batch, error) {
		reads++
		if reads == 1 {
			close(h.entered)
			<-h.proceed
			return a6Batch(1000, "a", a6Text(400)), nil
		}
		return distillsource.Batch{Watermark: after, Complete: true}, nil
	}
	return h
}

// dmRunA starts instance A's tick and waits until A is inside its run.
func dmRunA(t *testing.T, s *Scheduler, h *dmHeldSource) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.distillOnce(context.Background(), dfNoDemand)
	}()
	select {
	case <-h.entered:
	case <-time.After(30 * time.Second):
		t.Fatalf("instance A did not reach its first read within 30s")
	}
	return done
}

func dmWait(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s did not end within 60s", label)
	}
}

func TestDistillMultiInstance_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// B starts while A distils: B's startup sweep leaves A's running row open,
	// and A closes it with its own outcome.
	t.Run("second_instance_start_leaves_a_live_run_open", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20261119_010000_dm0001"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		h := newDMHeldSource(root)
		sA := dfScheduler(instancePool(t, pool, 4), a6Config(a6DumpRoot(t), 4000, 200), h)
		doneA := dmRunA(t, sA, h)

		dfScheduler(instancePool(t, pool, 4), dfConfig(), nil).distillStartupSweep(ctx)
		during := dfRows(t, pool)
		close(h.proceed)
		dmWait(t, doneA, "A")
		after := dfRows(t, pool)
		t.Logf("A's row while A ran, after B's start: %+v; after A's end: %+v", during, after)
		if len(during) != 1 || during[0].outcome != "running" {
			t.Fatalf("B's startup sweep: rows %+v, want A's row still running", during)
		}
		if len(after) != 1 || after[0].outcome != "ok" || after[0].to != 1000 {
			t.Fatalf("after A's run: rows %+v, want A's row ok at 1000", after)
		}
	})

	// B ticks while A distils the same root: B opens no run of its own and
	// reads nothing of the source; the root is distilled once.
	t.Run("second_instance_tick_does_not_distill_beside_the_first", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20261119_010000_dm0002"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		h := newDMHeldSource(root)
		sA := dfScheduler(instancePool(t, pool, 4), a6Config(a6DumpRoot(t), 4000, 200), h)
		doneA := dmRunA(t, sA, h)

		var bReads atomic.Int64
		srcB := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: 1000},
			hasNew:   map[string]bool{root: true},
			readFn: func(after int64) (distillsource.Batch, error) {
				bReads.Add(1)
				return a6Batch(1000, "a", a6Text(400)), nil
			},
		}
		sB := dfScheduler(instancePool(t, pool, 4), a6Config(a6DumpRoot(t), 4000, 200), srcB)
		sB.distillOnce(ctx, dfNoDemand)
		// The skipped tick is owed: distillTick retries it after
		// distillDemandRetry instead of waiting for the idle fallback.
		owed := sB.distillDemandDeferred.Load()
		during := dfRows(t, pool)
		close(h.proceed)
		dmWait(t, doneA, "A")
		if len(during) != 1 || srcB.reads != 0 || bReads.Load() != 0 {
			t.Fatalf("B's tick beside A's run: rows %+v, B source calls %d, B reads %d — want only A's run and no read by B",
				during, srcB.reads, bReads.Load())
		}
		if after := dfRows(t, pool); len(after) != 1 || after[0].outcome != "ok" {
			t.Fatalf("after A's run: rows %+v, want A's one ok row", after)
		}
		if !owed {
			t.Fatalf("B's skipped tick is not marked for a retry")
		}
	})

	// A running row whose instance is gone (no arm lock held) is closed by
	// the next tick of a live instance, not only at a restart: the tick
	// derives the watermark it left.
	t.Run("orphan_of_a_dead_instance_is_swept_by_the_next_tick", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20261119_010000_dm0003"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		if _, err := pool.Exec(ctx, `
			INSERT INTO distill_run (source_key, root_session_id, outcome, watermark_from, watermark_to)
			VALUES ($1, $2, 'running', 0, 500)`, key, root); err != nil {
			t.Fatalf("insert orphan: %v", err)
		}
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: root}},
			head:     map[string]int64{root: 500},
			hasNew:   map[string]bool{root: false},
		}
		dfScheduler(pool, a6Config(a6DumpRoot(t), 4000, 200), src).distillOnce(ctx, dfNoDemand)
		rows := dfRows(t, pool)
		if len(rows) == 0 || rows[0].outcome != "killed" || rows[0].errClass != "daemon_restart" || dfDerive(t, pool, key) != 500 {
			t.Fatalf("after a live tick: rows %+v, derivation %d — want the orphan killed and the derivation at 500", rows, dfDerive(t, pool, key))
		}
	})

	// A's row is closed by another session while A runs (A's arm lock was
	// lost with its connection). A books nothing on the closed row and leaves
	// it as the sweep closed it.
	t.Run("run_whose_row_was_closed_leaves_it_closed", func(t *testing.T) {
		dfTruncate(t, pool)
		const root = "20261119_010000_dm0004"
		key := distillSourceKey(dfLabel, dfScope, root)
		a6Clean(t, pool, root, key)
		h := newDMHeldSource(root)
		sA := dfScheduler(instancePool(t, pool, 4), a6Config(a6DumpRoot(t), 4000, 200), h)
		doneA := dmRunA(t, sA, h)
		if _, err := pool.Exec(ctx, `
			UPDATE distill_run SET outcome = 'killed', finished_at = now(), error = 'daemon_restart'
			 WHERE outcome = 'running'`); err != nil {
			t.Fatalf("foreign sweep: %v", err)
		}
		close(h.proceed)
		dmWait(t, doneA, "A")
		rows := dfRows(t, pool)
		if len(rows) != 1 || rows[0].outcome != "killed" || rows[0].to != 0 {
			t.Fatalf("after A's run: rows %+v, want the closed row killed at watermark 0", rows)
		}
	})
}
