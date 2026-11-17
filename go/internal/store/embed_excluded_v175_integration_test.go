//go:build integration

// The embed_excluded reconcile of v5.49.3 (migration 175), verbatim but for
// the names: the binary before migration 176. TestEmbedExcludedRollingOldBinary
// runs it against the schema of 176 (a rolling deploy), and the migration
// test uses it to settle a database at 175. Copied from
// go/internal/store/embed_excluded.go at 6d6ddacf; not production code.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

const (
	v175MarkSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		                 AND created_at IS NOT NULL
		                 AND (sensitivity, created_at, id) > ($3::text, $4::timestamptz, $5::uuid)
		               ORDER BY sensitivity, created_at, id
		               LIMIT $2)
		   AND NOT embed_excluded
		RETURNING embed_excluded, sensitivity, created_at, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded = $6),
		       l.sensitivity, l.created_at, l.id
		  FROM (SELECT 1) one
		  LEFT JOIN LATERAL (SELECT sensitivity, created_at, id FROM u
		                      ORDER BY sensitivity DESC, created_at DESC, id DESC LIMIT 1) l ON true`
	v175MarkNullAtSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		                 AND created_at IS NULL
		               LIMIT $2)
		   AND NOT embed_excluded
		RETURNING embed_excluded)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded = $3)`
	v175ClearSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = false
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embed_excluded AND NOT is_archived AND type_name = $1 AND id > $4::uuid
		               ORDER BY id
		               LIMIT $2)
		   AND embed_excluded
		RETURNING embed_excluded, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded = $3), (SELECT id FROM u ORDER BY id DESC LIMIT 1)`
	v175LeftSQL = `SELECT CASE WHEN $2::boolean
		THEN EXISTS (SELECT 1 FROM context_blocks
		              WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1)
		ELSE EXISTS (SELECT 1 FROM context_blocks
		              WHERE embed_excluded AND NOT is_archived AND type_name = $1) END`
)

// v175KeyStart is the keyset cursor before every block: no
// sensitivity is the empty string.
var v175KeyStart = v175Key{sens: "", at: time.Unix(0, 0).UTC().AddDate(-100, 0, 0), id: "00000000-0000-0000-0000-000000000000"}

type v175Key struct {
	sens string
	at   time.Time
	id   string
}


// ReconcileEmbedExcluded drains context_embed_excluded_reconcile (migration
// 175): for every type name a registry change queued, it sets the
// embed_excluded marker of the name's live blocks to the registry's answer
// (ctx_block_type_never_embedded) — marking takes the blocks without a
// vector (the range of idx_embedding_pending_sens), clearing every marked
// one, so a block that holds a vector keeps false — batch by batch, each batch its own
// statement, until no block of the name needs the change, and then removes
// the queue row unless a registry change queued the name again in the
// meantime (queued_at moved). It holds no queue row while it updates blocks
// (the order in the header of 175_embed_excluded.sql). Several ctx processes
// on one database may run it at the same time; a block set twice is skipped
// by the second statement, and each process removes the queue row only after
// it has seen nothing left.
func reconcile175(ctx context.Context, pool *pgxpool.Pool) (store.EmbedExcludedReconcileResult, error) {
	var res store.EmbedExcludedReconcileResult
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
	// A name that fails stays queued and does not hold up the names behind
	// it; the pass reports every failure at its end.
	var errs []error
	for _, e := range q {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		res.Names++
		n, done, err := v175ReconcileName(ctx, pool, e.name)
		res.Updated += n
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !done {
			continue // the answer kept moving: the queue row stays for the next pass
		}
		tag, err := pool.Exec(ctx,
			`DELETE FROM context_embed_excluded_reconcile WHERE name = $1 AND queued_at = $2`, e.name, e.at)
		if err != nil {
			errs = append(errs, fmt.Errorf("store: embed-excluded dequeue %q: %w", e.name, err))
			continue
		}
		res.Done += int(tag.RowsAffected())
	}
	return res, errors.Join(errs...)
}

// v175ReconcileName runs the batches of one name until none is
// left. done=false: the registry's answer moved under two batches in a row
// (each batch's rows were set back by the trigger), so the name is left for
// the next pass, whose queue row the registry change renewed.
func v175ReconcileName(ctx context.Context, pool *pgxpool.Pool, name string) (updated int, done bool, err error) {
	cursor := v175KeyStart
	clearFrom := v175KeyStart.id
	moved := 0
	for {
		var never bool
		if err := pool.QueryRow(ctx, `SELECT ctx_block_type_never_embedded($1)`, name).Scan(&never); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded answer %q: %w", name, err)
		}
		var wrote, reached int
		if never {
			var sens *string
			var at *time.Time
			var id *string
			if err := pool.QueryRow(ctx, v175MarkSQL, name, store.EmbedExcludedReconcileBatch,
				cursor.sens, cursor.at, cursor.id, true).Scan(&wrote, &reached, &sens, &at, &id); err != nil {
				return updated, false, fmt.Errorf("store: embed-excluded mark %q: %w", name, err)
			}
			if sens != nil && at != nil && id != nil {
				cursor = v175Key{sens: *sens, at: *at, id: *id}
			}
		} else {
			var last *string
			if err := pool.QueryRow(ctx, v175ClearSQL, name, store.EmbedExcludedReconcileBatch, false, clearFrom).
				Scan(&wrote, &reached, &last); err != nil {
				return updated, false, fmt.Errorf("store: embed-excluded clear %q: %w", name, err)
			}
			if last != nil {
				clearFrom = *last
			}
		}
		updated += reached
		switch {
		case wrote > 0 && reached == 0:
			// The trigger set every row back: the answer moved after it was
			// read. Read it again; twice in a row leaves the name queued.
			if moved++; moved == 2 {
				return updated, false, nil
			}
			cursor, clearFrom = v175KeyStart, v175KeyStart.id
		case wrote > 0:
			moved = 0
		default:
			// Nothing from the cursor on: the blocks without a created_at,
			// then a look from the start once more.
			if never {
				n, err := v175MarkNullAt(ctx, pool, name)
				updated += n
				if err != nil {
					return updated, false, err
				}
			}
			var left bool
			if err := pool.QueryRow(ctx, v175LeftSQL, name, never).Scan(&left); err != nil {
				return updated, false, fmt.Errorf("store: embed-excluded left %q: %w", name, err)
			}
			if !left {
				return updated, true, nil
			}
			if cursor == v175KeyStart && clearFrom == v175KeyStart.id {
				// Left over but not reachable by a batch from the start (held
				// and then changed by another statement): the next pass.
				return updated, false, nil
			}
			cursor, clearFrom = v175KeyStart, v175KeyStart.id
		}
	}
}

// v175MarkNullAt marks the unmarked pending blocks of name without a
// created_at, batch by batch, until none is left or the trigger sets a batch
// back (the answer moved; the caller looks again).
func v175MarkNullAt(ctx context.Context, pool *pgxpool.Pool, name string) (updated int, err error) {
	for {
		var wrote, reached int
		if err := pool.QueryRow(ctx, v175MarkNullAtSQL, name, store.EmbedExcludedReconcileBatch, true).
			Scan(&wrote, &reached); err != nil {
			return updated, fmt.Errorf("store: embed-excluded mark without created_at %q: %w", name, err)
		}
		updated += reached
		if wrote == 0 || reached == 0 {
			return updated, nil
		}
	}
}
