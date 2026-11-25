package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/blocktype"
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

// checkBlocks runs processBlock over blocks in the run's transaction and
// counts the blocks with a verdict and the deferred ones; done is what the
// run processed before (for the interruption line).
func checkBlocks(ctx context.Context, tx pgx.Tx, blocks []pendingBlock, set *blocktype.Set, done int) (processed int, deferred int64, err error) {
	for _, block := range blocks {
		// Check for context cancellation (demand interruption).
		select {
		case <-ctx.Done():
			slog.Info("guard: interrupted by context cancellation", "processed", done+processed)
			return processed, deferred, ctx.Err()
		default:
		}
		switch processBlock(ctx, tx, block, set) {
		case blockProcessed:
			processed++
		case blockDeferred:
			deferred++
		}
	}
	return processed, deferred, nil
}

// pickPast is the run's second pick (issue #547, review round 1): up to n
// waiting blocks it has not taken yet. A deferred block stays pending and at
// the front of the pick (the oldest first) until the transaction that holds
// its neighbour ends — a purge or prune batch, one batch long, then the next
// one may hold the next neighbour — so with at least a batch of them
// deferred, a run picked them again and again and no newer block got its
// verdict (0 of 10 in the review). The pick itself is unchanged: this
// transaction holds the blocks it took (SKIP LOCKED passes only another
// transaction's), the ones with a verdict no longer wait, so a pick of
// waiting+n returns the run's own still waiting blocks and n more, which are
// kept. No lock beyond the pick's own.
func pickPast(ctx context.Context, tx pgx.Tx, checkTypes []string, taken []pendingBlock, waiting, n int) ([]pendingBlock, error) {
	picked, err := pickPending(ctx, tx, checkTypes, waiting+n)
	if err != nil {
		return nil, err
	}
	more := make([]pendingBlock, 0, n)
	for _, b := range picked {
		if len(more) == n {
			break
		}
		if !slices.ContainsFunc(taken, func(t pendingBlock) bool { return t.id == b.id }) {
			more = append(more, b)
		}
	}
	return more, nil
}
