package handler

import (
	"context"
	"errors"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
)

// answeredTx runs fn inside a write transaction for a handler body that answers
// the request ITSELF.
//
// The split is the point. A manage handler has two kinds of failure inside one
// transaction, and they are answered by two different parties:
//
//   - Everything fn can decide — a rejected payload, a row that does not exist,
//     a store error, a break-glass guard — fn answers on the spot with its own
//     status and its own body, exactly where it did before, and returns false.
//     The transaction is then left WITHOUT a commit.
//   - The two failures the BRACKET owns — a BeginTx that never let fn run and a
//     Commit after it did — reach fail with the stage that produced them
//     ("begin" or "commit"), so every call site keeps its own wording: the
//     backend surface answers both with a 500 body, the disable-profile surface
//     logs "<action>: <stage> failed" behind a generic 500, the webhook surface
//     logs "webhook-secret: <stage>". None of those texts move into this helper.
//
// The commit-less exit travels as pgxdb.ErrRollback — the generic "leave, roll
// back, no error" of the bracket (DECISIONS.md K37) — and is translated back
// into false right here. fn's bool is what carries the MEANING ("a response has
// been written"), the same way ownedProject and resolveProfileTarget already
// report it in this package, so the error channel needs no vocabulary of its own
// and fn cannot leak an unanswered error past the bracket.
//
// Returns true only when the transaction committed, i.e. when the caller still
// owns the response.
func answeredTx(ctx context.Context, db pgxdb.Beginner, fail func(stage string, err error), fn func(pgx.Tx) bool) bool {
	// ran tells the two bracket failures apart: pgxdb.Write reports both as a
	// plain error, and only the fact that fn was entered separates a failed
	// BeginTx from a failed Commit.
	ran := false
	err := pgxdb.Write(ctx, db, pgxdb.Stages{}, func(tx pgx.Tx) error {
		ran = true
		if !fn(tx) {
			return pgxdb.ErrRollback
		}
		return nil
	})
	switch {
	case err == nil:
		return true
	case errors.Is(err, pgxdb.ErrRollback):
		return false // fn answered the request and asked to roll back
	case ran:
		fail("commit", err)
	default:
		fail("begin", err)
	}
	return false
}
