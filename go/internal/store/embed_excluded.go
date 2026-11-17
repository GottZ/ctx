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

// The statements of a reconcile batch for one queue entry, a type name ($1)
// and a group (issue #213, migration 176; issue #291, migration 175): at
// most $2 blocks each, in the scopes of the group only (embedExcludedInGroup)
// and only the blocks whose marker differs from their own answer
// (ctx_block_type_never_embedded(type_name, scope)): the answer is per
// tenant, so one entry may have blocks to mark and blocks to clear.
//
//   - markEmbedExcludedSQL: the unmarked pending blocks whose answer is true,
//     with a created_at, on idx_embedding_pending_sens (sensitivity,
//     created_at, id) in its order from the keyset cursor ($3, $4, $5) on, so
//     a batch does not read again what the batches before it passed.
//     created_at is nullable, and a row comparison with a NULL in it is NULL:
//     such a block would sort last and turn the cursor into a value no later
//     row compares above, so the cursor pass leaves them out and
//     markEmbedExcludedNullAtSQL takes them (no cursor: a marked block leaves
//     the set it reads). A block with a vector or an archived one keeps
//     false: it is in no pick index, and the trigger sets the marker when it
//     is unarchived, re-typed or moved (a cleared vector puts it back in the
//     pick indexes unmarked, filtered by the type predicate).
//   - clearEmbedExcludedSQL: the marked live blocks whose answer is false, on
//     idx_blocks_embed_excluded (type_name, id) from the keyset cursor $3 on:
//     the cleared rows leave the index, but their entries stay in it until a
//     vacuum may remove them, and a batch from the start would read every one
//     of them again while an older snapshot holds them (a long transaction, a
//     pg_dump).
//   - embedExcludedLeftSQL: whether a block of the entry still needs a
//     change, from the start of the range (a block another worker held, one
//     whose sensitivity moved it behind the cursor).
//
// The outer condition of an update repeats the change, so a row another
// statement changed in the meantime is skipped when its lock is granted
// (EvalPlanQual re-reads it). trg_block_embed_excluded decides the value
// from the registry again for every row it writes; a batch reports how many
// rows it wrote and how many of them reached the value the batch set (fewer
// when the answer moved under it).
var (
	markEmbedExcludedSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		                 AND created_at IS NOT NULL
		                 AND (sensitivity, created_at, id) > ($3::text, $4::timestamptz, $5::uuid)` +
		embedExcludedInGroup6 + `
		                 AND ctx_block_type_never_embedded(type_name, scope)
		               ORDER BY sensitivity, created_at, id
		               LIMIT $2)
		   AND NOT embed_excluded
		RETURNING embed_excluded, sensitivity, created_at, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded),
		       l.sensitivity, l.created_at, l.id
		  FROM (SELECT 1) one
		  LEFT JOIN LATERAL (SELECT sensitivity, created_at, id FROM u
		                      ORDER BY sensitivity DESC, created_at DESC, id DESC LIMIT 1) l ON true`
	markEmbedExcludedNullAtSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		                 AND created_at IS NULL` +
		embedExcludedInGroup3 + `
		                 AND ctx_block_type_never_embedded(type_name, scope)
		               LIMIT $2)
		   AND NOT embed_excluded
		RETURNING embed_excluded)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded)`
	clearEmbedExcludedSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = false
		 WHERE id IN (SELECT id FROM context_blocks
		               WHERE embed_excluded AND NOT is_archived AND type_name = $1 AND id > $3::uuid` +
		embedExcludedInGroup4 + `
		                 AND NOT ctx_block_type_never_embedded(type_name, scope)
		               ORDER BY id
		               LIMIT $2)
		   AND embed_excluded
		RETURNING embed_excluded, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE NOT embed_excluded), (SELECT id FROM u ORDER BY id DESC LIMIT 1)`
	embedExcludedLeftSQL = `SELECT EXISTS (SELECT 1 FROM context_blocks
		              WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1` +
		embedExcludedInGroup2 + `
		                AND ctx_block_type_never_embedded(type_name, scope))
		    OR EXISTS (SELECT 1 FROM context_blocks
		              WHERE embed_excluded AND NOT is_archived AND type_name = $1` +
		embedExcludedInGroup2 + `
		                AND NOT ctx_block_type_never_embedded(type_name, scope))`
)

// embedExcludedInGroup is the scope condition of a queue entry's group
// (ctx_embed_excluded_group, migration 176) on the bare context_blocks: '*'
// every scope, 't:<tenant_id>' the scopes of that tenant at the statement's
// snapshot, 's:<scope>' that one scope. A statement of an entry never
// reaches a block outside it, so the registry change of one tenant never
// writes a block of another.
func embedExcludedInGroup(param string) string {
	return `
		                 AND (CASE WHEN ` + param + `::text = '*' THEN true
		                           WHEN left(` + param + `::text, 2) = 't:' THEN scope IN (
		                                SELECT ts.scope FROM context_tenant_scopes ts
		                                 WHERE ts.tenant_id::text = substr(` + param + `::text, 3))
		                           ELSE scope = substr(` + param + `::text, 3) END)`
}

var (
	embedExcludedInGroup2 = embedExcludedInGroup("$2")
	embedExcludedInGroup3 = embedExcludedInGroup("$3")
	embedExcludedInGroup4 = embedExcludedInGroup("$4")
	embedExcludedInGroup6 = embedExcludedInGroup("$6")
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
	Done    int // queue rows removed (no later change re-queued them)
	Updated int // blocks whose marker reached their answer
}

// embedExcludedRounds bounds the looks one pass takes at a queue entry: a
// round marks and clears from the start of the range; an entry that still
// has a block to change after this many rounds (the answer moved under a
// batch, a block held and changed by another statement) stays queued for
// the next pass instead of holding up the entries behind it.
const embedExcludedRounds = 2

// ReconcileEmbedExcluded drains context_embed_excluded_queue (migrations
// 175, 176): for every (type name, group) a registry change or a scope
// assignment queued, it sets the embed_excluded marker of the name's live
// blocks in the scopes of the group to each block's answer
// (ctx_block_type_never_embedded(type_name, scope)) — marking takes the
// blocks without a vector (the range of idx_embedding_pending_sens),
// clearing every marked one, so a block that holds a vector keeps false —
// batch by batch, each batch its own statement, until no block of the entry
// needs a change, and then removes the queue row unless a change queued it
// again in the meantime (queued_at moved). It holds no queue row while it
// updates blocks (the order in the header of 176_embed_excluded_owner.sql).
// Several ctx processes on one database may run it at the same time; a
// block set twice is skipped by the second statement, and each process
// removes the queue row only after it has seen nothing left.
func ReconcileEmbedExcluded(ctx context.Context, pool *pgxpool.Pool) (EmbedExcludedReconcileResult, error) {
	var res EmbedExcludedReconcileResult
	type queued struct {
		name, grp string
		at        time.Time
	}
	rows, err := pool.Query(ctx, `SELECT name, grp, queued_at FROM context_embed_excluded_queue ORDER BY queued_at, name, grp`)
	if err != nil {
		return res, fmt.Errorf("store: embed-excluded queue: %w", err)
	}
	q, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (queued, error) {
		var e queued
		return e, r.Scan(&e.name, &e.grp, &e.at)
	})
	if err != nil {
		return res, fmt.Errorf("store: embed-excluded queue: %w", err)
	}
	// An entry that fails stays queued and does not hold up the entries
	// behind it; the pass reports every failure at its end.
	var errs []error
	for _, e := range q {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		res.Names++
		n, done, err := reconcileEmbedExcludedEntry(ctx, pool, e.name, e.grp)
		res.Updated += n
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !done {
			continue // left for the next pass
		}
		tag, err := pool.Exec(ctx,
			`DELETE FROM context_embed_excluded_queue WHERE name = $1 AND grp = $2 AND queued_at = $3`, e.name, e.grp, e.at)
		if err != nil {
			errs = append(errs, fmt.Errorf("store: embed-excluded dequeue %q %q: %w", e.name, e.grp, err))
			continue
		}
		res.Done += int(tag.RowsAffected())
	}
	return res, errors.Join(errs...)
}

// reconcileEmbedExcludedEntry runs the rounds of one queue entry. done=false:
// a block of the entry still needs a change after embedExcludedRounds rounds,
// or a batch's rows were all set back by the trigger (the answer moved after
// the batch chose them), so the entry is left for the next pass.
func reconcileEmbedExcludedEntry(ctx context.Context, pool *pgxpool.Pool, name, grp string) (updated int, done bool, err error) {
	for round := 0; round < embedExcludedRounds; round++ {
		n, settled, err := markEmbedExcludedPass(ctx, pool, name, grp)
		updated += n
		if err != nil {
			return updated, false, err
		}
		n, cleared, err := clearEmbedExcludedPass(ctx, pool, name, grp)
		updated += n
		if err != nil {
			return updated, false, err
		}
		var left bool
		if err := pool.QueryRow(ctx, embedExcludedLeftSQL, name, grp).Scan(&left); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded left %q %q: %w", name, grp, err)
		}
		if !left && settled && cleared {
			return updated, true, nil
		}
	}
	return updated, false, nil
}

// markEmbedExcludedPass marks the entry's blocks batch by batch from the
// start of the range, then the ones without a created_at. settled=false: a
// batch wrote rows and none of them kept the marker (the trigger set them
// back, the answer moved), and the pass stopped there.
func markEmbedExcludedPass(ctx context.Context, pool *pgxpool.Pool, name, grp string) (updated int, settled bool, err error) {
	cursor := embedExcludedKeyStart
	for {
		var wrote, reached int
		var sens *string
		var at *time.Time
		var id *string
		if err := pool.QueryRow(ctx, markEmbedExcludedSQL, name, EmbedExcludedReconcileBatch,
			cursor.sens, cursor.at, cursor.id, grp).Scan(&wrote, &reached, &sens, &at, &id); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded mark %q %q: %w", name, grp, err)
		}
		updated += reached
		if wrote > 0 && reached == 0 {
			return updated, false, nil
		}
		if wrote == 0 {
			break
		}
		if sens != nil && at != nil && id != nil {
			cursor = embedExcludedKey{sens: *sens, at: *at, id: *id}
		}
	}
	for {
		var wrote, reached int
		if err := pool.QueryRow(ctx, markEmbedExcludedNullAtSQL, name, EmbedExcludedReconcileBatch, grp).
			Scan(&wrote, &reached); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded mark without created_at %q %q: %w", name, grp, err)
		}
		updated += reached
		if wrote > 0 && reached == 0 {
			return updated, false, nil
		}
		if wrote == 0 {
			return updated, true, nil
		}
	}
}

// clearEmbedExcludedPass clears the entry's marked blocks whose answer is
// false, batch by batch from the cursor on id. settled=false as for
// markEmbedExcludedPass.
func clearEmbedExcludedPass(ctx context.Context, pool *pgxpool.Pool, name, grp string) (updated int, settled bool, err error) {
	from := embedExcludedKeyStart.id
	for {
		var wrote, reached int
		var last *string
		if err := pool.QueryRow(ctx, clearEmbedExcludedSQL, name, EmbedExcludedReconcileBatch, from, grp).
			Scan(&wrote, &reached, &last); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded clear %q %q: %w", name, grp, err)
		}
		updated += reached
		if wrote > 0 && reached == 0 {
			return updated, false, nil
		}
		if wrote == 0 {
			return updated, true, nil
		}
		if last != nil {
			from = *last
		}
	}
}
