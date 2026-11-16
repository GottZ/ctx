package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// embedExcludedReconcileInterval is the cadence of the embed_excluded
// reconcile (issue #291, migration 175): how soon after a registry change
// the blocks of a type that became never-embedded leave the pick indexes,
// and the blocks of a type that became embeddable return to them. A constant
// (blockCountFoldInterval precedent): an empty queue costs one index-only
// read of an empty table per tick, and a registry change is rare.
const embedExcludedReconcileInterval = 30 * time.Second

// runEmbedExcludedReconcile drains the embed_excluded reconcile queue every
// embedExcludedReconcileInterval (store.ReconcileEmbedExcluded). It runs
// whether or not the embed backfill arm does: the query path's pick reads
// the same indexes, and a type that became embeddable must reach them in
// either mode. Its own goroutine, so a long drain (the blocks of a type with
// millions of them) never holds the scheduler's select.
func (s *Scheduler) runEmbedExcludedReconcile(ctx context.Context) {
	defer guardPanic("embed excluded reconcile")
	t := time.NewTicker(embedExcludedReconcileInterval)
	defer t.Stop()
	for {
		res, err := store.ReconcileEmbedExcluded(ctx, s.pool)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("scheduler: embed excluded reconcile failed", "error", err)
		} else if res.Names > 0 {
			slog.Info("scheduler: embed excluded reconcile",
				"names", res.Names, "done", res.Done, "updated", res.Updated)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
