package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WriteLogRetentionBatch is how many context_write_log rows one statement of
// the write-log retention deletes (issue #277). A batch holds the row locks of
// its rows for one statement, so a block or key delete that clears a
// reference on one of them (ON DELETE SET NULL) waits for one batch at most;
// measured in docs/operations.md, "Write-log retention".
const WriteLogRetentionBatch = 10000

// writeLogHorizonSQL fixes the horizon of one run on the database's clock:
// every batch of the run deletes below the same instant, so a run ends.
const writeLogHorizonSQL = `SELECT now() - make_interval(days => $1)`

// writeLogEvictBatchSQL deletes the oldest rows of the write log below the
// horizon $1, from the keyset $2 on (created_at of the previous batch's last
// row; the zero time for the first), at most $3 of them. It answers how many
// rows it selected, how many it deleted and the created_at of the last
// selected row.
//
// The selection walks idx_write_log_created from the keyset up, so a batch
// reads its own rows and not the dead index entries the batches before it
// left behind; ties on created_at (a guard batch stamps its rows with one
// transaction time) are read again, the deleted ones as dead entries.
// FOR UPDATE SKIP LOCKED passes over rows another transaction holds — the
// SET NULL update of a block or key delete running beside it, or a batch of
// the same retention on another ctxd instance — so the retention never waits
// for a row lock, and two instances delete disjoint rows; a skipped row goes
// with a later run. The ctid list keeps the delete to the rows the selection
// locked. A row whose update committed after the statement's snapshot is
// locked in its new version, which the delete does not see: it stays for a
// later run, and the run goes on, because it counts the rows selected, not
// the rows deleted.
const writeLogEvictBatchSQL = `WITH s AS (
	    SELECT ctid, created_at FROM context_write_log
	     WHERE created_at < $1 AND created_at >= $2
	     ORDER BY created_at
	     LIMIT $3
	     FOR UPDATE SKIP LOCKED
	), d AS (
	    DELETE FROM context_write_log
	     WHERE ctid = ANY(ARRAY(SELECT ctid FROM s))
	    RETURNING 1)
	SELECT (SELECT count(*) FROM s)::bigint, (SELECT count(*) FROM d)::bigint, (SELECT max(created_at) FROM s)`

// EvictWriteLog deletes the context_write_log rows older than retentionDays
// days (scheduler.writelog_retention_days, issue #277) and returns how many
// it deleted. retentionDays <= 0 deletes nothing — the default keeps every
// row. It deletes in statements of at most WriteLogRetentionBatch rows until
// one selects fewer; the horizon is taken once, from the database's
// clock, before the first batch. Rows without a created_at are never
// deleted. Several ctxd instances may run it side by side (see
// writeLogEvictBatchSQL); a cancelled ctx stops it between batches with the
// count so far.
func EvictWriteLog(ctx context.Context, pool *pgxpool.Pool, retentionDays int) (int64, error) {
	if pool == nil || retentionDays <= 0 {
		return 0, nil
	}
	var horizon time.Time
	if err := pool.QueryRow(ctx, writeLogHorizonSQL, retentionDays).Scan(&horizon); err != nil {
		return 0, fmt.Errorf("store: write log retention horizon: %w", err)
	}
	var total int64
	var from time.Time // zero: below every created_at
	for {
		var selected, deleted int64
		var last *time.Time
		if err := pool.QueryRow(ctx, writeLogEvictBatchSQL, horizon, from, WriteLogRetentionBatch).Scan(&selected, &deleted, &last); err != nil {
			return total, fmt.Errorf("store: write log retention delete: %w", err)
		}
		total += deleted
		if selected < WriteLogRetentionBatch || last == nil {
			return total, nil
		}
		from = *last
	}
}
