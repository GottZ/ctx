package store

import (
	"context"
	"errors"
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

// The statements of a reconcile batch for one type name ($1), at most $2
// blocks each, issue #291 (migration 175):
//
//   - markEmbedExcludedSQL: the name became never-embedded. Its unmarked
//     pending blocks with a created_at, on idx_embedding_pending_sens
//     (sensitivity, created_at, id) in its order from the keyset cursor ($3,
//     $4, $5) on, so a batch does not read again what the batches before it
//     passed. created_at is nullable, and a row comparison with a NULL in it
//     is NULL: such a block would sort last and turn the cursor into a value
//     no later row compares above, so the cursor pass leaves them out and
//     markEmbedExcludedNullAtSQL takes them (no cursor: a marked block leaves
//     the set it reads). A block with a
//     vector or an archived one keeps false: it is in no pick index, and the
//     trigger sets the marker when it is unarchived or re-typed (a cleared
//     vector puts it back in the pick indexes unmarked, filtered by the type
//     predicate as before migration 175).
//   - clearEmbedExcludedSQL: the name became embeddable. Its marked live
//     blocks, on idx_blocks_embed_excluded (type_name, id) from the keyset
//     cursor $4 on: the cleared rows leave the index, but their entries stay
//     in it until a vacuum may remove them, and a batch from the start would
//     read every one of them again while an older snapshot holds them (a
//     long transaction, a pg_dump).
//   - embedExcludedLeftSQL: whether a block of the name still needs the
//     change, from the start of the range (a block another worker held, one
//     whose sensitivity moved it behind the cursor).
//
// The outer condition of an update repeats the change, so a row another
// statement changed in the meantime is skipped when its lock is granted
// (EvalPlanQual re-reads it). trg_block_embed_excluded decides the value
// from the registry again for every row it writes; a batch reports how many
// rows it wrote and how many of them reached $6, the answer the batch was
// chosen by (fewer when the registry answer moved under it).
const (
	markEmbedExcludedSQL = `WITH u AS (
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
	markEmbedExcludedNullAtSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		                 AND created_at IS NULL
		               LIMIT $2)
		   AND NOT embed_excluded
		RETURNING embed_excluded)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded = $3)`
	clearEmbedExcludedSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = false
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embed_excluded AND NOT is_archived AND type_name = $1 AND id > $4::uuid
		               ORDER BY id
		               LIMIT $2)
		   AND embed_excluded
		RETURNING embed_excluded, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded = $3), (SELECT id FROM u ORDER BY id DESC LIMIT 1)`
	embedExcludedLeftSQL = `SELECT CASE WHEN $2::boolean
		THEN EXISTS (SELECT 1 FROM context_blocks
		              WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1)
		ELSE EXISTS (SELECT 1 FROM context_blocks
		              WHERE embed_excluded AND NOT is_archived AND type_name = $1) END`
)

// embedExcludedKeyStart is the keyset cursor before every block: no
// sensitivity is the empty string.
var embedExcludedKeyStart = embedExcludedKey{sens: "", at: time.Unix(0, 0).UTC().AddDate(-100, 0, 0), id: "00000000-0000-0000-0000-000000000000"}

type embedExcludedKey struct {
	sens string
	at   time.Time
	id   string
}

// EmbedExcludedReconcileResult is what one ReconcileEmbedExcluded call did.
type EmbedExcludedReconcileResult struct {
	Names   int // queue rows worked on
	Done    int // queue rows removed (no later registry change re-queued them)
	Updated int // blocks whose marker reached the registry's answer
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
	// A name that fails stays queued and does not hold up the names behind
	// it; the pass reports every failure at its end.
	var errs []error
	for _, e := range q {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		res.Names++
		n, done, err := reconcileEmbedExcludedName(ctx, pool, e.name)
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

// reconcileEmbedExcludedName runs the batches of one name until none is
// left. done=false: the registry's answer moved under two batches in a row
// (each batch's rows were set back by the trigger), so the name is left for
// the next pass, whose queue row the registry change renewed.
func reconcileEmbedExcludedName(ctx context.Context, pool *pgxpool.Pool, name string) (updated int, done bool, err error) {
	cursor := embedExcludedKeyStart
	clearFrom := embedExcludedKeyStart.id
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
			if err := pool.QueryRow(ctx, markEmbedExcludedSQL, name, EmbedExcludedReconcileBatch,
				cursor.sens, cursor.at, cursor.id, true).Scan(&wrote, &reached, &sens, &at, &id); err != nil {
				return updated, false, fmt.Errorf("store: embed-excluded mark %q: %w", name, err)
			}
			if sens != nil && at != nil && id != nil {
				cursor = embedExcludedKey{sens: *sens, at: *at, id: *id}
			}
		} else {
			var last *string
			if err := pool.QueryRow(ctx, clearEmbedExcludedSQL, name, EmbedExcludedReconcileBatch, false, clearFrom).
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
			cursor, clearFrom = embedExcludedKeyStart, embedExcludedKeyStart.id
		case wrote > 0:
			moved = 0
		default:
			// Nothing from the cursor on: the blocks without a created_at,
			// then a look from the start once more.
			if never {
				n, err := markEmbedExcludedNullAt(ctx, pool, name)
				updated += n
				if err != nil {
					return updated, false, err
				}
			}
			var left bool
			if err := pool.QueryRow(ctx, embedExcludedLeftSQL, name, never).Scan(&left); err != nil {
				return updated, false, fmt.Errorf("store: embed-excluded left %q: %w", name, err)
			}
			if !left {
				return updated, true, nil
			}
			if cursor == embedExcludedKeyStart && clearFrom == embedExcludedKeyStart.id {
				// Left over but not reachable by a batch from the start (held
				// and then changed by another statement): the next pass.
				return updated, false, nil
			}
			cursor, clearFrom = embedExcludedKeyStart, embedExcludedKeyStart.id
		}
	}
}

// markEmbedExcludedNullAt marks the unmarked pending blocks of name without a
// created_at, batch by batch, until none is left or the trigger sets a batch
// back (the answer moved; the caller looks again).
func markEmbedExcludedNullAt(ctx context.Context, pool *pgxpool.Pool, name string) (updated int, err error) {
	for {
		var wrote, reached int
		if err := pool.QueryRow(ctx, markEmbedExcludedNullAtSQL, name, EmbedExcludedReconcileBatch, true).
			Scan(&wrote, &reached); err != nil {
			return updated, fmt.Errorf("store: embed-excluded mark without created_at %q: %w", name, err)
		}
		updated += reached
		if wrote == 0 || reached == 0 {
			return updated, nil
		}
	}
}
