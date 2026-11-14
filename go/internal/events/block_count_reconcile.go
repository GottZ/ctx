package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// blockCountReconcileIdlePoll is how often the reconcile loop re-reads
// scheduler.block_count_reconcile_interval while it is 0 (off), so switching
// it on applies without a restart. A fixed constant: it bounds the reaction
// time to a settings change, it is no policy.
const blockCountReconcileIdlePoll = time.Minute

// blockCountReconcileFirstDelay caps the wait before the first pass after a
// boot. Without it a process restarted more often than the interval (several
// deploys a day against the 24 h default) would never run the pass; with it the
// first pass comes at most an hour after the boot, the following ones every
// interval. A fixed constant: it only bounds the first wait, the cadence is the
// setting.
const blockCountReconcileFirstDelay = time.Hour

// blockCountReconcileInterval reads the hot cadence of the reconcile pass.
func (s *Scheduler) blockCountReconcileInterval() time.Duration {
	return s.cfg.Snapshot().Scheduler.BlockCountReconcileInterval //nolint:forbidigo // issue #266 background: the block-counter reconcile is one process-wide pass over every tenant's scopes; its cadence is a server-global policy knob (global-only), like the overview-rebuild cadence.
}

// runBlockCountReconcile is the slow repair arm of the block counters (issue
// #266): every scheduler.block_count_reconcile_interval it runs
// store.ReconcileBlockCounts over every scope — the same pass as the manage
// action stats-reconcile. The first pass runs blockCountReconcileFirstDelay
// (or one interval, if shorter) after boot, not at boot: the triggers keep the
// counters exact, so a restart is no reason to recount at once. A scope
// without drift is one read; a scope with drift is corrected under the
// write-ordering row, which also serializes the passes of several ctx
// processes on one database. Every correction is logged by the store.
func (s *Scheduler) runBlockCountReconcile(ctx context.Context) {
	defer guardPanic("block count reconcile")
	first := true
	for {
		wait := s.blockCountReconcileInterval()
		if wait <= 0 {
			wait = blockCountReconcileIdlePoll
		} else if first && wait > blockCountReconcileFirstDelay {
			wait = blockCountReconcileFirstDelay
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if s.blockCountReconcileInterval() <= 0 {
			continue
		}
		first = false
		start := time.Now()
		res, err := store.ReconcileBlockCounts(ctx, s.pool, "")
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("scheduler: block count reconcile failed", "error", err)
			continue
		}
		slog.Info("scheduler: block count reconcile",
			"scopes", res.Scopes, "corrections", len(res.Corrections), "pruned", res.Pruned,
			"duration", time.Since(start))
	}
}
