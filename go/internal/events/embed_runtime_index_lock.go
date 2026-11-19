package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue #332: the runtime index DDL of the re-embed migration (the pending
// index of the migration cycle, the _next twins of the verify gate) runs
// under one session-level advisory lock for context_blocks, across every
// session that runs it — several ctxd instances on one database, and the
// migration cycle and the verify goroutine of one.
//
// One lock for the table, not one per index name: two CREATE INDEX
// CONCURRENTLY on one table never build side by side anyway (both take
// SHARE UPDATE EXCLUSIVE), but the second one waits for that lock while its
// statement holds a snapshot, and the first one, in its wait for older
// snapshots, waits for that statement: SQLSTATE 40P01, whatever the two
// index names are. A DROP INDEX CONCURRENTLY beside a build does the same.
//
// A session-level lock, not a transaction-level one: CREATE and DROP INDEX
// CONCURRENTLY cannot run inside a transaction. The lock and the DDL run on
// the same connection, so a build never outlives its lock: when the session
// ends (pg_terminate_backend, or a canceled caller context — pgx then sends
// a cancel request and closes the connection), the server ends the build
// (an INVALID leftover stays) and releases the lock together; when the
// client is gone without either (the ctxd process killed), the server-side
// statement runs on and the lock stays held until it is done. An INVALID
// index found under the lock is therefore a leftover, never a build in
// progress of a session that takes the lock.
//
// The lock is only tried (pg_try_advisory_lock), as the neighbouring
// background runs do with theirs (the overview rebuild, the block-count
// fold): a session that finds it held returns errRuntimeIndexBusy at once.
// Waiting would hold a pooled connection for the whole build of another
// session, hours for the HNSW twin at 10M vectors.
//
// The key is the two-key form, class key first — the scheme of
// ctx:supersedes-guess, whose class key differs; the two-key space is
// apart from every single-key advisory lock of ctx.
const (
	runtimeIndexLockSQL   = `SELECT pg_try_advisory_lock(hashtext('ctx:runtime_index'), hashtext('context_blocks'))`
	runtimeIndexUnlockSQL = `SELECT pg_advisory_unlock(hashtext('ctx:runtime_index'), hashtext('context_blocks'))`
)

// runtimeIndexXactTrySQL is the runtime index lock in its transaction form,
// tried: the confirm's swap transaction takes it (issue #370) so that no
// runtime index DDL runs between a builder's row check and its DDL while the
// swap renames the _next names away. Transaction-level and session-level
// advisory locks on one key conflict across sessions. Tried, never waited
// on: a wait would hold a snapshot that a CREATE INDEX CONCURRENTLY of the
// lock holder waits for in turn (SQLSTATE 40P01).
const runtimeIndexXactTrySQL = `SELECT pg_try_advisory_xact_lock(hashtext('ctx:runtime_index'), hashtext('context_blocks'))`

// runtimeIndexUnlockTimeout bounds the unlock statement, which runs on a
// context detached from the caller's (a canceled caller must still give the
// lock back). Fixed: it is the budget of one statement on an open
// connection; a connection that does not answer in it is closed instead.
const runtimeIndexUnlockTimeout = 5 * time.Second

// runtimeIndexBusyRetry is how long the verify gate waits before it asks
// for the lock again while another session builds (verifyEnsureIndex). The
// migration cycle's own cadence, embedMigrateInterval: an attempt costs one
// pooled statement, and the builds it waits for take minutes to hours.
const runtimeIndexBusyRetry = embedMigrateInterval

// errRuntimeIndexBusy: another session holds the runtime index lock; it is
// building or dropping a runtime index on context_blocks right now.
var errRuntimeIndexBusy = errors.New("another session is building a runtime index on context_blocks")

// errRuntimeIndexUnwanted: under the runtime index lock, right before its
// DDL, the row no longer waits for the index (issue #370) — no DDL ran.
var errRuntimeIndexUnwanted = errors.New("the row no longer waits for this runtime index — no DDL")

// runtimeIndexWanted asks, on db, whether the row still waits for a runtime
// index (ensureConcurrentIndexValid).
type runtimeIndexWanted func(ctx context.Context, db pgxdb.Rower) (bool, error)

// Stages of ensureConcurrentIndexValid at which runtimeIndexDDLHook runs.
const (
	// runtimeIndexStageUnlocked: after the lock-free catalog read found DDL
	// to do, before the lock is tried.
	runtimeIndexStageUnlocked = "unlocked"
	// runtimeIndexStageLocked: under the lock, after the catalog re-read and
	// the row check, right before the DROP or CREATE.
	runtimeIndexStageLocked = "locked"
)

// runtimeIndexDDLHook is the test seam of the check-then-act window before
// the runtime index DDL (issue #370): nil in production; a test sets it to
// let another session act at a defined stage (a confirm, a finished build) —
// a window of microseconds that no timing produces deterministically. The
// distillWriteBarrier precedent: a package variable, set and reset by a
// test that does not run in parallel.
var runtimeIndexDDLHook func(stage, name string)

// withRuntimeIndexLock acquires one pooled connection, takes the runtime
// index lock on it, runs fn with that connection and gives the lock back.
// A connection goes back into the pool only when the unlock answered that
// the lock was released; on every other way out (the lock statement failed
// with an unknown outcome, the unlock failed or found no lock, the
// connection broke under a canceled context) it is closed, and the server
// releases a closed session's advisory locks.
func withRuntimeIndexLock(ctx context.Context, pool *pgxpool.Pool, fn func(conn *pgxpool.Conn) error) error {
	return withTriedSessionLock(ctx, pool, triedSessionLock{
		label: "runtime index lock", lockSQL: runtimeIndexLockSQL, unlockSQL: runtimeIndexUnlockSQL, busy: errRuntimeIndexBusy,
	}, fn)
}

// triedSessionLock names one session-level advisory lock that is tried,
// never waited on: the runtime index lock, the verify run lock of a watermark
// (embed_verify.go) and the distiller's arm lock (distill.go). label names it
// in errors and log lines; lockSQL returns the boolean of
// pg_try_advisory_lock, unlockSQL that of pg_advisory_unlock, both with args;
// busy is returned when another session holds it.
type triedSessionLock struct {
	label              string
	lockSQL, unlockSQL string
	args               []any
	busy               error
}

// withTriedSessionLock acquires one pooled connection, tries the lock on it,
// runs fn with that connection and gives the lock back. A connection goes
// back into the pool only when the unlock answered that the lock was
// released; on every other way out (the lock statement failed with an
// unknown outcome, the unlock failed or found no lock, the connection broke
// under a canceled context) it is closed, and the server releases a closed
// session's advisory locks. fn may run its statements on other connections:
// the lock then only marks that the holder is alive, and it ends with the
// holder's process.
func withTriedSessionLock(ctx context.Context, pool *pgxpool.Pool, l triedSessionLock, fn func(conn *pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("%s: acquire a connection: %w", l.label, err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, l.lockSQL, l.args...).Scan(&locked); err != nil {
		closeRuntimeIndexConn(ctx, conn)
		return fmt.Errorf("%s: %w", l.label, err)
	}
	if !locked {
		conn.Release()
		return l.busy
	}
	defer unlockTriedSessionLock(ctx, conn, l)
	return fn(conn)
}

// unlockTriedSessionLock gives the lock back and returns conn to the pool, or
// closes conn when the release is not proven.
func unlockTriedSessionLock(ctx context.Context, conn *pgxpool.Conn, l triedSessionLock) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeIndexUnlockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRow(uctx, l.unlockSQL, l.args...).Scan(&released)
	if err != nil || !released {
		slog.Warn("scheduler: "+l.label+" not released on its connection — closing the connection, the server releases the lock with the session",
			"released", released, "error", err)
		closeRuntimeIndexConn(ctx, conn)
		return
	}
	conn.Release()
}

// closeRuntimeIndexConn closes conn and hands it back; the pool destroys a
// closed connection instead of pooling it.
func closeRuntimeIndexConn(ctx context.Context, conn *pgxpool.Conn) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeIndexUnlockTimeout)
	defer cancel()
	_ = conn.Conn().Close(cctx)
	conn.Release()
}
