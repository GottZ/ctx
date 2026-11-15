package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// blockCountReconcilePoll is how often the reconcile loop re-reads
// scheduler.block_count_reconcile_interval: while it is 0 (off), and as the
// longest slice of its wait, so a hot change of the interval applies within one
// slice instead of after the wait that began under the old value (the
// backfillArmPoll convention of runEmbedBackfillLoop). A var, not a const, for
// the same reason as there: the wait test shortens it.
var blockCountReconcilePoll = time.Minute

// blockCountReconcileFirstDelay caps the wait before the first pass after a
// boot. Without it a process restarted more often than the interval (several
// deploys a day against the 24 h default) would never run the pass; with it the
// first pass comes at most an hour after the boot, the following ones every
// interval. A fixed constant: it only bounds the first wait, the cadence is the
// setting.
const blockCountReconcileFirstDelay = time.Hour

// blockCountFoldInterval is the cadence of the delta fold (migration 170): the
// block-count delta rows written since the last fold are what every stats read
// sums on top of the counter rows, so the fold bounds that tail to the writes
// of a few seconds. A constant (pendingWriteEvictInterval precedent): one fold
// is a single statement over a table that holds only those few seconds of
// writes, and no operator decision depends on its cadence.
const blockCountFoldInterval = 5 * time.Second

// blockCountReconcileInterval reads the hot cadence of the reconcile pass.
func (s *Scheduler) blockCountReconcileInterval() time.Duration {
	return s.cfg.Snapshot().Scheduler.BlockCountReconcileInterval //nolint:forbidigo // issue #266 background: the block-counter reconcile is one process-wide pass over every tenant's scopes; its cadence is a server-global policy knob (global-only), like the overview-rebuild cadence.
}

// waitHotInterval waits until interval() has passed since start, re-reading
// interval() at least every slice, so a hot change applies within one slice.
// maxWait (> 0) bounds the total wait. A non-positive interval ends the wait at
// once. It reports false when ctx ended.
func waitHotInterval(ctx context.Context, start time.Time, interval func() time.Duration, slice, maxWait time.Duration) bool {
	for {
		iv := interval()
		if iv <= 0 {
			return true
		}
		if maxWait > 0 && iv > maxWait {
			iv = maxWait
		}
		left := time.Until(start.Add(iv))
		if left <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(min(left, slice)):
		}
	}
}

// runBlockCountReconcile is the slow repair arm of the block counters (issue
// #266): every scheduler.block_count_reconcile_interval it runs
// store.ReconcileBlockCounts over every scope — the same pass as the manage
// action stats-reconcile. The first pass runs blockCountReconcileFirstDelay
// (or one interval, if shorter) after boot, not at boot: the triggers keep the
// counters exact, so a restart is no reason to recount at once. A scope
// without drift is one read; a scope with drift is corrected under its
// advisory lock, which also serializes the passes of several ctx processes on
// one database. A failing scope is logged and the pass goes on.
func (s *Scheduler) runBlockCountReconcile(ctx context.Context) {
	defer guardPanic("block count reconcile")
	first := true
	for {
		start := time.Now()
		if s.blockCountReconcileInterval() <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(blockCountReconcilePoll):
			}
			continue
		}
		var firstCap time.Duration
		if first {
			firstCap = blockCountReconcileFirstDelay
		}
		if !waitHotInterval(ctx, start, s.blockCountReconcileInterval, blockCountReconcilePoll, firstCap) {
			return
		}
		if s.blockCountReconcileInterval() <= 0 {
			continue
		}
		first = false
		res, err := store.ReconcileBlockCounts(ctx, s.pool, "")
		if ctx.Err() != nil {
			return
		}
		if res == nil {
			slog.Warn("scheduler: block count reconcile failed", "error", err)
			continue
		}
		slog.Info("scheduler: block count reconcile",
			"scopes", res.Scopes, "corrections", len(res.Corrections), "pruned", res.Pruned,
			"failed", len(res.Failed), "duration", time.Since(start))
	}
}

// runBlockCountFold folds the block-count delta rows into the counter rows
// every blockCountFoldInterval. Several ctx processes on one database each run
// it; a fold that finds another fold at work skips its turn.
func (s *Scheduler) runBlockCountFold(ctx context.Context) {
	defer guardPanic("block count fold")
	t := time.NewTicker(blockCountFoldInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := store.FoldBlockCountDeltas(ctx, s.pool); err != nil && ctx.Err() == nil {
			slog.Warn("scheduler: block count fold failed", "error", err)
		}
	}
}
