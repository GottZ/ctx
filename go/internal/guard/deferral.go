package guard

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A deferred verdict is a block the run checked and then left pending, with no
// verdict and no audit row, because writing the verdict would have meant
// waiting for another transaction (issues #284, #547): an archive verdict
// that meets a concurrent one on its pair (claimArchivePair) or a supersedes
// write of its scope or target (store.TryLockSupersedesSource), and an audit
// row whose matched neighbour another transaction holds in a mode that
// refuses the foreign key's key-share lock (neighbourKeyShare). The block
// stays at the front of the next pick, and the next run decides it on what
// that transaction left. Nothing else marks a deferral — the block looks
// unchecked — so the run counts them: per process, since its start, and in
// its last committed run (DeferredVerdicts, read by manage guard-stats).
var (
	deferredTotal   atomic.Int64
	deferredLastRun atomic.Int64
)

// DeferredVerdicts returns the verdicts the guard runs of this process have
// deferred since the process started, and how many its last committed run
// that took blocks deferred (an empty run leaves it; runs side by side in one
// process overwrite each other's). A run that fails or is interrupted adds
// nothing: its rollback leaves every block of its batch pending anyway.
func DeferredVerdicts() (sinceStart, lastRun int64) {
	return deferredTotal.Load(), deferredLastRun.Load()
}

// recordDeferrals books the deferrals of one committed run.
func recordDeferrals(n int64) {
	deferredLastRun.Store(n)
	deferredTotal.Add(n)
}

// errVerdictDeferred is the outcome of a block whose verdict the run leaves
// for its next run without an error (see the comment above deferredTotal).
var errVerdictDeferred = errors.New("guard: verdict deferred to the next run")

// neighbourKeyShareSQL takes the foreign key's key-share lock of the audit
// row's matched neighbour without waiting.
const neighbourKeyShareSQL = `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR KEY SHARE NOWAIT`

// neighbourKeyShare takes, before the audit insert, the key-share lock the
// insert's foreign key on matched_block_id takes on the neighbour — NOWAIT
// (issue #547). The run holds its picked blocks for its whole batch, so a wait
// here is an edge a lock cycle can run through: a scope purge batch holds the
// neighbour FOR UPDATE (its pick, or a level of its parent_id closure) and
// waits for a block the run picked, a tenant prune's delete holds the
// neighbour and waits for a picked block of its batch. Only a transaction
// that holds the row FOR UPDATE refuses the key-share lock — one that deletes
// it or changes a key column —, never a writer of the content (FOR NO KEY
// UPDATE). A neighbour held so, or gone by the time of the insert, defers the
// verdict: the next run checks the block against what that transaction left.
// With the lock taken here, the insert's own foreign-key check finds it held
// by this transaction and does not wait either, so the run waits for no row
// lock at all — its pick skips locked rows, its archive verdict takes only
// try locks — and cannot be part of a cycle.
func neighbourKeyShare(ctx context.Context, tx pgx.Tx, matchedID string) error {
	var one int
	err := tx.QueryRow(ctx, neighbourKeyShareSQL, matchedID).Scan(&one)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return errVerdictDeferred
	case errors.As(err, &pgErr) && pgErr.Code == "55P03":
		return errVerdictDeferred
	}
	return fmt.Errorf("matched neighbour key share: %w", err)
}
