package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// A single update is a write of the key's budget (query.rate_limit_write)
// on every surface, as a store is and as an update item of a block batch is
// (issue #519). A direct update — manage update, the MCP update tool — spends
// it in its own transaction (store.UpdateBlockBooked): an update that
// changes the row is booked with its commit, one that changes nothing costs
// nothing, one that does not fit writes nothing and answers 429 rate_limit,
// the single store's answer at the budget. A staged update — the MCP update
// tool for a confirm_writes key, the web chat's ctx_update — pays at intent
// in the stage's transaction, as every staged write does, and its confirm
// books nothing. This file is the one place the rule is applied: to stop
// booking single updates, the two surfaces call store.UpdateBlock and
// store.StagePendingWrite again.

// updateWriteBudget is the key's write budget of a single update, the limit
// read from the request's snapshot (0 = no limit: nothing refused, every
// changing update still booked); nil for an identity without a key, which
// has no budget to book on.
func updateWriteBudget(ctx context.Context, cfg ConfigStore, ar *auth.AuthResult) *store.WriteBudget {
	if ar.ApiKeyID == "" {
		return nil
	}
	limit := 0
	if cfg != nil {
		limit = cfg.SnapshotForRequest(ctx).Query.RateLimitWrite
	}
	return &store.WriteBudget{APIKeyID: ar.ApiKeyID, Limit: limit}
}

// updateBudgetReject is the refusal of an update the budget does not fit
// (*store.WriteBudgetError): the single store's sentence and class; nil for
// any other error.
func updateBudgetReject(err error) *writeReject {
	var over *store.WriteBudgetError
	if !errors.As(err, &over) {
		return nil
	}
	return classRateLimit.reject(fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", over.Limit))
}

// stageWithIntent stages a card and books its write intent in one
// transaction, under the key's budget lock (store.ReserveWriteIntents, the
// stage of an MCP batch): no card without its booking, no booking without
// its card, and two stages of one key count one after the other. A budget
// that does not fit stages nothing (*store.WriteBudgetError).
func stageWithIntent(ctx context.Context, pool *pgxpool.Pool, budget *store.WriteBudget, stage func(pgxdb.Rower) (*store.PendingWrite, error)) (*store.PendingWrite, error) {
	var pw *store.PendingWrite
	err := pgxdb.Write(ctx, pool, pgxdb.At("stage update"), func(tx pgx.Tx) error {
		if budget != nil {
			if err := store.ReserveWriteIntents(ctx, tx, *budget, 1); err != nil {
				return err
			}
		}
		var err error
		pw, err = stage(tx)
		return err
	})
	return pw, err
}
