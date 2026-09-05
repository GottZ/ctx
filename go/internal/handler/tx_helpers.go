package handler

import (
	"context"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// attributedTx runs fn in a transaction that carries the request id for the
// audit triggers: begin → SetTxRequestID → fn → commit, with a rollback on
// every error path (pgxdb.Write owns that bracket).
//
// ATTRIBUTION is the whole point: the 051/093 triggers write their audit row
// from `ctx.request_id`, a transaction-local GUC. It has to be set INSIDE the
// transaction that carries the row, so every handler that wants an attributed
// audit line repeats the same three steps around a single store call. This is
// that repetition, once.
//
// Every error travels back UNWRAPPED — pgxdb.Stages{} with both labels empty
// — because that is what all six call sites do today: a failed begin, a
// failed stamp, a failed store call and a failed commit each reach the
// handler exactly as the driver produced it, and errors.Is/As across the
// boundary stays intact.
//
// The value follows the same rule the six bodies followed by hand: fn's
// result is returned only when fn succeeded, so a failed store call yields
// the zero value, while a failed COMMIT still returns what fn produced.
func attributedTx[T any](ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) (T, error)) (T, error) {
	var out T
	err := pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
			return err
		}
		v, err := fn(tx)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}
