package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// embedExcludedReconcileInterval is the cadence of the embed_excluded
// reconcile (issue #291, migration 175; issue #213, migration 176): how soon
// after a registry change or a scope assignment the blocks of a type that
// became never-embedded leave the pick indexes, and the blocks of a type that
// became embeddable return to them. Fixed
// (blockCountFoldInterval precedent): an empty queue costs one read of an
// empty table per tick, and a registry change is rare. A var, not a const,
// for tests (blockCountReconcilePoll precedent).
var embedExcludedReconcileInterval = 30 * time.Second

// reconcileEmbedExcluded is the pass the arm runs; a var for tests.
var reconcileEmbedExcluded = store.ReconcileEmbedExcluded

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
		s.embedExcludedReconcilePass(ctx)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// embedExcludedReconcilePass is one pass of the arm. A panic ends the pass,
// not the arm (guardPanic inside the pass): the next tick runs again.
func (s *Scheduler) embedExcludedReconcilePass(ctx context.Context) {
	defer guardPanic("embed excluded reconcile pass")
	res, err := reconcileEmbedExcluded(ctx, s.pool)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		slog.Warn("scheduler: embed excluded reconcile failed", "error", err,
			"names", res.Names, "done", res.Done, "updated", res.Updated)
	} else if res.Names > 0 {
		slog.Info("scheduler: embed excluded reconcile",
			"names", res.Names, "done", res.Done, "updated", res.Updated)
	}
}
