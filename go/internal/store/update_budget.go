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
// #519). The transaction takes the key's budget lock first, compares the row
// with what the update writes (changedSQL, the batch's no-op decision), and
// for a change counts the window (a *WriteBudgetError writes nothing) and
// books one 'write' row naming the block with the commit — no booking
// without the write, no write without the booking. An update that changes
// nothing, or finds no row, costs nothing. The internal writers (guard,
// dream, digest, forge sync, backfill) do not update through this function.
func UpdateBlockBooked(ctx context.Context, pool *pgxpool.Pool, id string, data UpdateBlockData, writeScopes []string, budget WriteBudget) (*Block, bool, error) {
	return updateBlock(ctx, pool, id, data, writeScopes, &budget)
}

// lockBudget takes the key's budget lock (nothing without a budget): the
// first lock of the update's transaction, as of a batch's.
func lockBudget(ctx context.Context, tx pgx.Tx, budget *WriteBudget) error {
	if budget == nil {
		return nil
	}
	return budget.lock(ctx, tx)
}

// charge decides, in the update's transaction and before its statement,
// whether the update is a write its budget pays for: the row it would write,
// read with the lock the statement takes, differs from what it writes. A
// change that does not fit is refused (*WriteBudgetError). Without a budget,
// or for a row the statement will not find, nothing is charged.
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
	if !changed {
		return false, nil
	}
	return true, budget.check(ctx, tx, 1)
}
