package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
// ends (ctxd killed, connection terminated), the server ends the build (an
// INVALID leftover stays) and releases the lock together; when the client
// is gone but the server-side statement still runs, the lock stays held
// until the statement is done. An INVALID index found under the lock is
// therefore a leftover, never a build in progress of a session that takes
// the lock.
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

// withRuntimeIndexLock acquires one pooled connection, takes the runtime
// index lock on it, runs fn with that connection and gives the lock back.
// A connection goes back into the pool only when the unlock answered that
// the lock was released; on every other way out (the lock statement failed
// with an unknown outcome, the unlock failed or found no lock, the
// connection broke under a canceled context) it is closed, and the server
// releases a closed session's advisory locks.
func withRuntimeIndexLock(ctx context.Context, pool *pgxpool.Pool, fn func(conn *pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("embed-migrate: runtime index lock: acquire a connection: %w", err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, runtimeIndexLockSQL).Scan(&locked); err != nil {
		closeRuntimeIndexConn(ctx, conn)
		return fmt.Errorf("embed-migrate: runtime index lock: %w", err)
	}
	if !locked {
		conn.Release()
		return errRuntimeIndexBusy
	}
	defer unlockRuntimeIndex(ctx, conn)
	return fn(conn)
}

// unlockRuntimeIndex gives the runtime index lock back and returns conn to
// the pool, or closes conn when the release is not proven.
func unlockRuntimeIndex(ctx context.Context, conn *pgxpool.Conn) {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeIndexUnlockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRow(uctx, runtimeIndexUnlockSQL).Scan(&released)
	if err != nil || !released {
		slog.Warn("scheduler: runtime index lock not released on its connection — closing the connection, the server releases the lock with the session",
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
