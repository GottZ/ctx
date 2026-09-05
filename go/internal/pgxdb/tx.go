package pgxdb

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// rollbackGrace bounds the deferred rollback of a transaction whose own
// context is already done. It is the budget for one ROLLBACK on an open
// connection, not for the work that failed.
const rollbackGrace = 5 * time.Second

// Beginner is anything that can open a transaction: *pgxpool.Pool, *pgx.Conn
// and pgx.Tx all satisfy it, and so does a test double.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Stages carries the CALLER'S error texts. An empty field means: return the
// error of that stage UNWRAPPED — the form a whole group of openings uses
// today, forge/apply.go:194 among them, where a failed Begin travels back
// untouched.
//
// Why not a single generated `what`: only a minority of the transaction
// openings in this tree share one wording, so a generated text would silently
// rewrite the rest — including texts that are asserted on
// (dream/writelinks_test.go:362 requires "begin tx") and texts that operators
// grep for. Stages costs one field pair per call site and keeps every wording
// exactly where it is; the helper owns the ORDER, not the words.
type Stages struct {
	// Begin labels a failure of BeginTx, written WITHOUT the trailing
	// ": %w" — the helper appends it.
	Begin string
	// Commit labels a failure of Commit, same form as Begin.
	Commit string
}

// At builds the most common pair: "<what>: begin" and "<what>: commit".
func At(what string) Stages {
	return Stages{Begin: what + ": begin", Commit: what + ": commit"}
}

// wrap labels err with the caller's text for one stage. An empty label — and
// a nil err — return err unchanged, so errors.Is/As across the boundary sees
// exactly what the driver produced.
func wrap(label string, err error) error {
	if err == nil || label == "" {
		return err
	}
	return fmt.Errorf("%s: %w", label, err)
}

// Write runs fn inside a read-write transaction: BeginTx, deferred rollback,
// fn, commit.
//
// An error from fn is passed through UNCHANGED (no %w wrapping), so
// errors.Is/As across the boundary stays intact (store.ErrPendingWriteNotFound,
// store.ErrTenantNotFound and their kind).
func Write(ctx context.Context, db Beginner, s Stages, fn func(pgx.Tx) error) error {
	return WriteOpts(ctx, db, pgx.TxOptions{}, s, fn)
}

// WriteOpts is Write with explicit pgx.TxOptions, for the sites that need an
// isolation level of their own (overview/csr.go RepeatableRead+ReadOnly).
//
// ROLLBACK POLICY: the deferred rollback runs on context.WithoutCancel(ctx),
// bounded by rollbackGrace. A rollback on an already-cancelled context makes
// pgx CLOSE the connection instead of rolling it back — at 1M-10M blocks and
// aborted requests that is connection churn instead of a return to the pool.
// store/overview_rootmap.go:359 (cappedCountTx) already detaches the rollback
// this way; what it does NOT carry is the grace bound, and that is the part
// this helper adds: WithoutCancel alone would let a rollback wait without a
// limit of its own.
func WriteOpts(ctx context.Context, db Beginner, opts pgx.TxOptions, s Stages, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return wrap(s.Begin, err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackGrace)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return wrap(s.Commit, tx.Commit(ctx))
}

// Read is WriteOpts with pgx.TxOptions{AccessMode: pgx.ReadOnly}. The
// SELECT-only perimeter it opens is enforced by the database itself, which
// answers a write inside such a transaction with SQLSTATE 25006.
func Read(ctx context.Context, db Beginner, s Stages, fn func(pgx.Tx) error) error {
	return WriteOpts(ctx, db, pgx.TxOptions{AccessMode: pgx.ReadOnly}, s, fn)
}
