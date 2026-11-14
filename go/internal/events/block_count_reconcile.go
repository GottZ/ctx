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

// blockCountReconcileInterval reads the hot cadence of the reconcile pass.
func (s *Scheduler) blockCountReconcileInterval() time.Duration {
	return s.cfg.Snapshot().Scheduler.BlockCountReconcileInterval //nolint:forbidigo // issue #266 background: the block-counter reconcile is one process-wide pass over every tenant's scopes; its cadence is a server-global policy knob (global-only), like the overview-rebuild cadence.
}

// runBlockCountReconcile is the slow repair arm of the block counters (issue
// #266): every scheduler.block_count_reconcile_interval it runs
// store.ReconcileBlockCounts over every scope — the same pass as the manage
// action stats-reconcile. The first pass runs one interval after boot, not at
// boot: the triggers keep the counters exact, so a restart is no reason to
// recount. Each scope is its own REPEATABLE READ transaction that blocks no
// writer; every correction is logged by the store. Several ctx processes on
// one database each run the pass; a second reconcile of the same scope finds
// no drift or loses the serialization race and retries — correct either way.
func (s *Scheduler) runBlockCountReconcile(ctx context.Context) {
	defer guardPanic("block count reconcile")
	for {
		wait := s.blockCountReconcileInterval()
		if wait <= 0 {
			wait = blockCountReconcileIdlePoll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if s.blockCountReconcileInterval() <= 0 {
			continue
		}
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
