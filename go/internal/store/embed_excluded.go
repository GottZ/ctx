package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EmbedExcludedReconcileBatch is the number of blocks one reconcile statement
// sets (ReconcileEmbedExcluded). A fixed constant: it bounds the row locks
// and the trigger work (guard-dirty, write NOTIFY, block counts) one
// statement holds, like a backfill batch; the queue is drained batch after
// batch regardless.
const EmbedExcludedReconcileBatch = 1000

// The two statements of a reconcile batch for one type name ($1), at most $2
// blocks each, issue #291 (migration 175). Each reads exactly the rows it
// changes, in an index range the changed rows leave:
//
//   - markEmbedExcludedSQL: the name became never-embedded. Its unmarked
//     pending blocks, on idx_embedding_pending_sens (sensitivity, created_at,
//     id) in its order, which holds no marked row; blocks with a vector or
//     archived keep false (they are in no pick index, and an unarchive sets
//     the marker through the trigger).
//   - clearEmbedExcludedSQL: the name became embeddable. Its marked live
//     blocks, on idx_blocks_embed_excluded (type_name).
//
// The outer condition repeats the change, so a row another statement changed
// in the meantime is skipped when its lock is granted (EvalPlanQual re-reads
// it). trg_block_embed_excluded decides the value from the registry again for
// every row it writes.
const (
	markEmbedExcludedSQL = `UPDATE context_blocks SET embed_excluded = true
		WHERE id IN (SELECT id FROM context_blocks
		              WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		              ORDER BY sensitivity, created_at, id
		              LIMIT $2)
		  AND NOT embed_excluded`
	clearEmbedExcludedSQL = `UPDATE context_blocks SET embed_excluded = false
		WHERE id IN (SELECT id FROM context_blocks
		              WHERE embed_excluded AND NOT is_archived AND type_name = $1
		              LIMIT $2)
		  AND embed_excluded`
)

// EmbedExcludedReconcileResult is what one ReconcileEmbedExcluded call did.
type EmbedExcludedReconcileResult struct {
	Names   int // queue rows worked on
	Done    int // queue rows removed (no later registry change re-queued them)
	Updated int // blocks whose marker changed
}

// ReconcileEmbedExcluded drains context_embed_excluded_reconcile (migration
// 175): for every type name a registry change queued, it sets the
// embed_excluded marker of the name's blocks to the registry's answer
// (ctx_block_type_never_embedded), batch by batch, each batch its own
// statement, and then removes the queue row unless a registry change queued
// the name again in the meantime (queued_at moved). It holds no queue or
// registry row while it updates blocks, so a registry change never waits on
// it longer than one batch (the order in the header of 175_embed_excluded.sql).
// Several ctx processes on one database may run it at the same time; a block
// set twice is skipped by the second statement.
func ReconcileEmbedExcluded(ctx context.Context, pool *pgxpool.Pool) (EmbedExcludedReconcileResult, error) {
	var res EmbedExcludedReconcileResult
	type queued struct {
		name string
		at   time.Time
	}
	rows, err := pool.Query(ctx, `SELECT name, queued_at FROM context_embed_excluded_reconcile ORDER BY queued_at, name`)
	if err != nil {
		return res, fmt.Errorf("store: embed-excluded queue: %w", err)
	}
	q, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (queued, error) {
		var e queued
		return e, r.Scan(&e.name, &e.at)
	})
	if err != nil {
		return res, fmt.Errorf("store: embed-excluded queue: %w", err)
	}
	for _, e := range q {
		res.Names++
		var never bool
		if err := pool.QueryRow(ctx, `SELECT ctx_block_type_never_embedded($1)`, e.name).Scan(&never); err != nil {
			return res, fmt.Errorf("store: embed-excluded answer %q: %w", e.name, err)
		}
		stmt := clearEmbedExcludedSQL
		if never {
			stmt = markEmbedExcludedSQL
		}
		for {
			tag, err := pool.Exec(ctx, stmt, e.name, EmbedExcludedReconcileBatch)
			if err != nil {
				return res, fmt.Errorf("store: embed-excluded reconcile %q: %w", e.name, err)
			}
			res.Updated += int(tag.RowsAffected())
			if tag.RowsAffected() < EmbedExcludedReconcileBatch {
				break
			}
		}
		tag, err := pool.Exec(ctx,
			`DELETE FROM context_embed_excluded_reconcile WHERE name = $1 AND queued_at = $2`, e.name, e.at)
		if err != nil {
			return res, fmt.Errorf("store: embed-excluded dequeue %q: %w", e.name, err)
		}
		res.Done += int(tag.RowsAffected())
	}
	return res, nil
}
