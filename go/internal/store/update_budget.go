package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UpdateBlockBooked is UpdateBlock for a client's update: an update that
// changes the row is a write of the key's budget (query.rate_limit_write),
// spent inside the update's transaction as a block batch spends it (issue
// #519). The transaction locks the row and compares it with what the update
// writes (changedSQL, the batch's no-op decision); a change is written, then
// the key's budget lock is taken, the window counted (a *WriteBudgetError
// rolls the write back) and one 'write' row naming the block booked with the
// commit — no booking without the write, no write without the booking. An
// update that changes nothing, or finds no row, costs nothing and never takes
// the budget lock. The internal writers (guard, dream, digest, forge sync,
// backfill) do not update through this function.
//
// The budget lock is the last lock of the transaction, as in a batch
// (UpsertBlocks), held from the count to the commit — WriteBudget.spend: a
// writer of the key waits for rows without it, so an update waiting for a
// row another transaction holds keeps no other write of its key waiting
// (review of PR #553), and its holder waits for nothing but its own count
// and booking, so no lock cycle runs through it.
//
// A nil budget books nothing: UpdateBlock.
func UpdateBlockBooked(ctx context.Context, pool *pgxpool.Pool, id string, data UpdateBlockData, writeScopes []string, budget *WriteBudget) (*Block, bool, error) {
	return updateBlock(ctx, pool, id, data, writeScopes, budget)
}

// charge decides, in the update's transaction and before its statement,
// whether the update is a write its budget pays for: the row it would write,
// read with the lock the statement takes, differs from what it writes.
// Without a budget, or for a row the statement will not find, nothing is
// charged. The budget is spent after the statement (WriteBudget.spend).
func (u blockUpdate) charge(ctx context.Context, tx pgx.Tx, budget *WriteBudget) (bool, error) {
	if budget == nil {
		return false, nil
	}
	var changed bool
	err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT %s FROM context_blocks
		  WHERE id = $%d AND scope = ANY($%d::text[]) AND NOT is_archived AND `+notDerivedSQL+`%s
		  FOR NO KEY UPDATE`,
		u.changedSQL(), u.idIdx, u.scopeIdx, u.pinSQL()), u.args...).Scan(&changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // the statement finds no row either
	}
	if err != nil {
		return false, fmt.Errorf("store: update block: compare: %w", err)
	}
	return changed, nil
}

// spend is the last step of a transaction that wrote the blocks ids on the
// budget: the key's budget lock, the count — n writes that do not fit are
// refused with *WriteBudgetError, and the transaction rolls its writes back —
// and one booking row per block, which commits with the writes. The lock is
// held from the count to the commit, so two writers of one key count and book
// one after the other, on every ctxd instance; taken last, after every write,
// it never waits while its holder holds a row a waiter for it wants.
func (b WriteBudget) spend(ctx context.Context, tx pgx.Tx, ids []string) error {
	if err := b.reserve(ctx, tx, len(ids)); err != nil {
		return err
	}
	return b.book(ctx, tx, ids, 0)
}
