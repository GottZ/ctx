package pgxdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrRollback is the way OUT of a write transaction without a commit and
// without a failure: fn returns it, the bracket lets its deferred rollback
// run, and hands the sentinel back to the caller UNCHANGED — one line of
// errors.Is(err, pgxdb.ErrRollback) at the call site turns it into whatever
// that caller's "nothing to do" looks like.
//
// UNCHANGED is the whole point, and the reason this is not a swallowing
// helper. If the bracket answered a rollback with nil, then nil would mean
// two different things — committed, and deliberately not committed — and the
// code AFTER the bracket cannot tell them apart: post-commit metrics
// (overview/cluster.go), audit rows, notify calls all run on a transaction
// that never landed, and a closure that fills an outer variable reports a
// half-built state as success.
//
// A rollback that means something SPECIFIC keeps its own package-private
// sentinel instead (skipped because another worker holds the advisory lock,
// source type not registered, pick already released — DECISIONS.md K35/K37).
// This one carries only the generic case: leave, roll back, no error.
var ErrRollback = errors.New("pgxdb: rollback requested")

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

// rollbackDetached is the single exit every bracket in this file defers, so
// the two of them cannot drift apart: the ROLLBACK POLICY documented at
// WriteOpts (WithoutCancel plus rollbackGrace) holds for a probe as much as
// for a write. A rollback error is discarded on purpose — the transaction is
// being abandoned, and the reason it is abandoned is already on its way to
// the caller.
func rollbackDetached(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackGrace)
	defer cancel()
	_ = tx.Rollback(rctx)
}

// Write runs fn inside a read-write transaction: BeginTx, deferred rollback,
// fn, commit.
//
// An error from fn is passed through UNCHANGED (no %w wrapping), so
// errors.Is/As across the boundary stays intact (store.ErrPendingWriteNotFound,
// store.ErrTenantNotFound and their kind). ErrRollback travels on that same
// pass-through, which is what makes it usable as a commit-less exit.
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
	defer rollbackDetached(ctx, tx)
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

// Probe runs fn inside a transaction that is NEVER committed: BeginTx, fn,
// then the same detached rollback the write bracket defers. The error of fn
// travels back unchanged; the rollback's own error is discarded, exactly as
// the hand-written form did.
//
// This is the shape of a SONDE — a transaction whose RESULT is the rollback.
// schemacontract.probeOneGUC (check.go, three statements: load the pgvector
// library, set a GUC transaction-local, ask pg_settings about it) needs one
// pinned backend and must not leak transaction-local state; it has five exits
// and not one commit. handler/status_db.go queryEfSearchEffective is the same
// probe for hnsw.ef_search.
//
// WHY Read DOES NOT COVER THIS: a probe's ordinary outcome is a FAILED
// statement — an unrecognized GUC is the finding, not an accident — and a
// commit on a transaction that a statement error has already aborted is
// itself an error: pgx.ErrTxCommitRollback, "commit unexpectedly resulted in
// rollback", measured against PG18 with AccessMode ReadOnly as well
// (T04-4g P1/P1-ReadOnly). A committing bracket would turn every finding into
// a failure of the whole check.
//
// begin labels a failed BeginTx the way Stages.Begin does, empty meaning the
// error travels unwrapped. There is no Commit label, because there is no
// commit — a Stages here would carry a dead field.
func Probe(ctx context.Context, db Beginner, begin string, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return wrap(begin, err)
	}
	defer rollbackDetached(ctx, tx)
	return fn(tx)
}
