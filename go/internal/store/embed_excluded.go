package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// EmbedExcludedReconcileBatch is the number of blocks one reconcile statement
// sets (ReconcileEmbedExcluded). A fixed constant: it bounds the row locks
// and the trigger work (guard-dirty, write NOTIFY, block counts) one
// statement holds, like a backfill batch; the queue is drained batch after
// batch regardless.
const EmbedExcludedReconcileBatch = 1000

// The statements of a reconcile batch for one queue entry, a type name ($1)
// and a group (issue #213, migration 176; issue #291, migration 175). The
// answer of a scope (ctx_block_type_never_embedded(name, scope)) is the same
// for every block of the name in it, so the reconcile reads it once per scope
// and runs only the direction it needs:
//
//   - markEmbedExcludedSQL: answer true. The scope's unmarked pending blocks
//     of the name, on idx_embedding_pending_type_scope (type_name, scope, id)
//     (migration 193, issue #527) as a range of the name and the scope from
//     the keyset cursor $3 on, so a batch reads no block of another name or
//     another scope, nor what the batches before it passed — the form of the
//     clear on idx_blocks_embed_excluded. The statement runs with sorts off
//     (embedExcludedBatchNoSort). While that index does not stand as 193
//     builds it (embedExcludedPendingIndexReadySQL) the reconcile sends
//     markEmbedExcludedBefore193SQL instead: the scope's range of
//     idx_embedding_pending_scope_sens (scope, sensitivity, created_at, id)
//     from the keyset cursor ($3, $4, $5) on, which holds every pending block
//     of the scope, so each of its statements reads the scope's pending
//     blocks of every other name and checks the name on the heap — linear in
//     them (a scope whose sensitivity floor keeps its blocks pending for
//     good, a long embedding backlog). created_at is NOT NULL since
//     migration 186, so its row comparison never holds a NULL.
//   - markEmbedExcludedStaleSQL: answer true. The scope's blocks of the name
//     marked stale (a vector kept over a content change, issue #198 part B),
//     on idx_embedding_stale_scope_sens (scope, sensitivity, created_at, id)
//     as a range of the scope from a keyset cursor like the pending pass,
//     with sorts off (embedExcludedBatchNoSort): no reader ranks their vector, the
//     backfill would never replace it, and the stale-pick indexes of 173 know
//     no marker, so such a block would stay in them for good and every pick
//     of its class would filter it row by row. The batch flips the marker
//     column, which makes trg_block_embed_excluded decide the block again
//     under its locks: when the answer is still true it marks the block and
//     drops the stale vector pair; when a lift committed meanwhile it keeps
//     the vector and false (the batch counts the row as set back, and the
//     pass stops).
//   - clearEmbedExcludedSQL: answer false (for '*': embedExcludedStarClearScopes).
//     The scope's marked live blocks of the name, on idx_blocks_embed_excluded
//     (type_name, scope, id) as a range of the scope from the keyset cursor
//     $3 on, so a batch reads no block of another scope: the cleared rows
//     leave the index, but their entries stay in it until a vacuum may
//     remove them, and a batch from the start would read every one of them
//     again while an older snapshot holds them (a long transaction, a
//     pg_dump). The answer is the scope's, the same for every block of the
//     name in it, so the statement states it once on its parameters: when it
//     turned true since the reconcile read it, the statement reads nothing.
//     Without the index of 176 (a large corpus before the out-of-band build)
//     the statement is the same and reads the name's range of the definition
//     of 175, (type_name, id), sorting the scope's rows: correct, at the cost
//     before 176. The statement runs with sorts off
//     (embedExcludedBatchNoSort).
//   - embedExcludedMarkLeftSQL, embedExcludedClearLeftSQL: whether a block of
//     the scope still needs the change, from the start of the range (a block
//     another worker held, one written or re-typed behind the cursor; before
//     193, also one whose sensitivity moved it behind the cursor).
//
// The mark and the clear batch and their left checks state the name and the
// scope (the statements before 193 the scope alone) as comparisons over the
// leading key columns of their index and order by those columns, not as
// equalities (issue #513; the form of the pending pick, PendingPickSQL,
// issue #239). With equalities the
// leading keys are constants of the statement, the order reduces to the last
// keys, and other indexes deliver that order too: at random_page_cost 4 —
// PostgreSQL's default, which a deployment keeps unless its operator lowers
// it — the clear walked context_blocks_pkey from the cursor and filtered name
// and scope row by row, on its generic plan at 1.1 as well, and the mark read
// the whole scope through idx_context_scope or every pending block of the
// table through idx_embedding_pending and sorted them (the planner estimates
// the name and the scope as independent, so a name that is common in another
// tenant made the batch's rows look plentiful: about 98 000 buffers per clear
// statement with 1 000 000 blocks in each of the two tenants, 43-61 on the
// range). The comparisons admit exactly the rows of the scope and the name
// (both NOT NULL, a database's collation deterministic) and are the start
// and stop key of the index scan; only the index whose leading keys they are
// gives the order without a sort over every row they admit. For the clear's
// left check (LIMIT 1) that suffices on every plan measured. A batch takes
// up to 1 000 rows, and with the same estimate a sort still looked cheaper
// on some plans — for the mark before 193 over the scope's range of
// idx_context_scope (incremental, the scope its presorted key) or over
// idx_embedding_pending, at either value and on the generic plan; for the
// clear, on its generic plan at 4, over the name's range of
// idx_blocks_type_name in every tenant (4 710 buffers at 100 000 marked
// blocks of another tenant); for the stale pass and the stale half of the
// mark's left check, over the stale blocks of the name in every tenant
// (6 177 buffers at 100 000 stale blocks of another tenant). So each batch,
// and the mark's left check, runs in a transaction of its own with sorts off
// (embedExcludedBatchNoSort), where the range of its index is the only path
// without a sort.
//
// A block with a fresh vector or an archived one keeps false: it is in no
// pick index, and the trigger sets the marker when it is unarchived,
// re-typed or moved. Every batch stays within one scope: the shared advisory
// locks the trigger of a mark batch takes per row are of one name and one
// group (a batch spanning groups would take them in row order and could
// close a cycle with a scope assignment, which takes its groups' locks
// sorted), and no statement of an entry reads a block outside the scopes it
// works on.
//
// The id subquery of every batch is an array the update reads by primary
// key (id = ANY(ARRAY(...))): it runs once, before the update, and returns at
// most the batch. As a semi join (id IN (...)) the planner could also hash
// the batch's ids against a scan of every row the outer condition admits, and
// did so for the stale pass at random_page_cost 4 on a table of 40 000 rows
// (a sequential scan, 957 buffers, where the ids read 2). The subquery locks
// the rows it selects (FOR NO KEY
// UPDATE, the strength the update takes anyway): a row another transaction
// changed in the meantime is read again once its lock is granted and checked
// against the whole selection, name and scope included (EvalPlanQual of the
// locking clause), so a block moved to another scope while the batch waited
// for it drops out of the batch instead of being written there. A recheck in
// the outer WHERE would see only the outer conditions, and repeating the
// selection there lets the planner drive the update from the scope's range
// (measured on the cost pin: the clear walked idx_context_scope over the
// scope's 20 000 blocks, 972 buffers, where it reads 7). The outer condition
// repeats the change only, so a row still matching is skipped when another
// statement made the change first. trg_block_embed_excluded decides the
// value from the registry again for every row it writes; a batch reports how
// many rows it wrote and how many of them reached the value the batch set
// (fewer when the answer moved under it).
var (
	markEmbedExcludedSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id = ANY(ARRAY(SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
		                 AND (type_name, scope, id) > ($1::text, $2::text, $3::uuid)
		                 AND (type_name, scope) <= ($1::text, $2::text)
		               ORDER BY type_name, scope, id
		               LIMIT $4
		               FOR NO KEY UPDATE))
		   AND NOT embed_excluded
		RETURNING embed_excluded, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded), (SELECT id FROM u ORDER BY id DESC LIMIT 1)`
	markEmbedExcludedBefore193SQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = true
		 WHERE id = ANY(ARRAY(SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
		                 AND type_name = $1 AND created_at IS NOT NULL
		                 AND (scope, sensitivity, created_at, id) > ($2::text, $3::text, $4::timestamptz, $5::uuid)
		                 AND scope <= $2::text
		               ORDER BY scope, sensitivity, created_at, id
		               LIMIT $6
		               FOR NO KEY UPDATE))
		   AND NOT embed_excluded
		RETURNING embed_excluded, sensitivity, created_at, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded),
		       l.sensitivity, l.created_at, l.id
		  FROM (SELECT 1) one
		  LEFT JOIN LATERAL (SELECT sensitivity, created_at, id FROM u
		                      ORDER BY sensitivity DESC, created_at DESC, id DESC LIMIT 1) l ON true`
	markEmbedExcludedStaleSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = NOT embed_excluded
		 WHERE id = ANY(ARRAY(SELECT id FROM context_blocks
		               WHERE embedding_stale AND NOT is_archived
		                 AND type_name = $1
		                 AND (scope, sensitivity, created_at, id) > ($2::text, $3::text, $4::timestamptz, $5::uuid)
		                 AND scope <= $2::text
		               ORDER BY scope, sensitivity, created_at, id
		               LIMIT $6
		               FOR NO KEY UPDATE))
		   AND embedding_stale
		RETURNING embed_excluded, sensitivity, created_at, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE embed_excluded),
		       l.sensitivity, l.created_at, l.id
		  FROM (SELECT 1) one
		  LEFT JOIN LATERAL (SELECT sensitivity, created_at, id FROM u
		                      ORDER BY sensitivity DESC, created_at DESC, id DESC LIMIT 1) l ON true`
	clearEmbedExcludedSQL = `WITH u AS (
		UPDATE context_blocks SET embed_excluded = false
		 WHERE id = ANY(ARRAY(SELECT id FROM context_blocks
		               WHERE embed_excluded AND NOT is_archived
		                 AND (type_name, scope, id) > ($1::text, $2::text, $3::uuid)
		                 AND (type_name, scope) <= ($1::text, $2::text)
		                 AND NOT ctx_block_type_never_embedded($1::text, $2::text)
		               ORDER BY type_name, scope, id
		               LIMIT $4
		               FOR NO KEY UPDATE))
		   AND embed_excluded
		RETURNING embed_excluded, id)
		SELECT (SELECT count(*) FROM u), (SELECT count(*) FROM u WHERE NOT embed_excluded), (SELECT id FROM u ORDER BY id DESC LIMIT 1)`
	// embedExcludedMarkLeftSQL reads the first unmarked pending block of the
	// name and the scope in the order of idx_embedding_pending_type_scope
	// (migration 193) and the first stale one in the order of
	// idx_embedding_stale_scope_sens (ORDER BY their key LIMIT 1, name and
	// scope, or the scope, as a range), with sorts off: an unordered EXISTS
	// with equalities was planned at random_page_cost 4 on idx_context_scope
	// or idx_embedding_pending and read the whole scope or every pending block
	// of the table (972 and 884 buffers on the cost pin, 1-2 on the range),
	// and its stale half read the stale blocks of the name in every tenant.
	// embedExcludedMarkLeftBefore193SQL is the check while the index of 193
	// does not stand: its pending half reads the scope's range of
	// idx_embedding_pending_scope_sens, every pending block of the scope when
	// none of the name is left.
	embedExcludedMarkLeftSQL = `SELECT EXISTS (SELECT 1 FROM (
		              SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
		                 AND (type_name, scope) >= ($1::text, $2::text) AND (type_name, scope) <= ($1::text, $2::text)
		               ORDER BY type_name, scope, id
		               LIMIT 1) first)
		    OR EXISTS (SELECT 1 FROM (
		              SELECT id FROM context_blocks
		               WHERE embedding_stale AND NOT is_archived
		                 AND type_name = $1 AND scope >= $2::text AND scope <= $2::text
		               ORDER BY scope, sensitivity, created_at, id
		               LIMIT 1) first_stale)`
	embedExcludedMarkLeftBefore193SQL = `SELECT EXISTS (SELECT 1 FROM (
		              SELECT id FROM context_blocks
		               WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
		                 AND type_name = $1 AND scope >= $2::text AND scope <= $2::text
		               ORDER BY scope, sensitivity, created_at, id
		               LIMIT 1) first)
		    OR EXISTS (SELECT 1 FROM (
		              SELECT id FROM context_blocks
		               WHERE embedding_stale AND NOT is_archived
		                 AND type_name = $1 AND scope >= $2::text AND scope <= $2::text
		               ORDER BY scope, sensitivity, created_at, id
		               LIMIT 1) first_stale)`
	// embedExcludedClearLeftSQL reads the first block in the order of the
	// clear (ORDER BY its key LIMIT 1), so it is planned on the scope's range
	// of idx_blocks_embed_excluded like the clear: an unordered EXISTS was
	// planned on idx_context_scope once the name's marked blocks in another
	// tenant moved the statistics, and read the whole scope (976 buffers over
	// 20 000 blocks of the scope at 100 000 marked blocks elsewhere), and with
	// equalities and ORDER BY id it walked context_blocks_pkey at
	// random_page_cost 4 (1 946 -> 6 042 buffers at 10 000 -> 100 000 marked
	// blocks of another tenant on the cost pin).
	embedExcludedClearLeftSQL = `SELECT EXISTS (SELECT 1 FROM (
		              SELECT id FROM context_blocks
		               WHERE embed_excluded AND NOT is_archived
		                 AND (type_name, scope) >= ($1::text, $2::text) AND (type_name, scope) <= ($1::text, $2::text)
		                 AND NOT ctx_block_type_never_embedded($1::text, $2::text)
		               ORDER BY type_name, scope, id
		               LIMIT 1) first)`
	// embedExcludedMarkedScopesSQL: the scopes that hold a marked live block
	// of the name, on idx_blocks_embed_excluded (the name's range, one
	// index entry per marked block): the scopes a '*' entry may have to clear
	// while the name has no excluded _global row.
	embedExcludedMarkedScopesSQL = `SELECT DISTINCT scope::text FROM context_blocks
		 WHERE embed_excluded AND NOT is_archived AND type_name = $1
		 ORDER BY 1`
	// embedExcludedStarScopesSQL: the scopes in which a '*' entry may have a
	// block to mark — the scopes of the name's unmarked pending blocks and of
	// its stale ones (no other block of the name needs a marker). The pending
	// half reads the name's range of idx_embedding_pending_type_scope
	// (type_name, scope, id) one scope at a time (issue #536): each step of
	// the recursion takes the first entry after the scope before it, in the
	// order of the index (name and scope as a range, ORDER BY its leading
	// columns, LIMIT 1), one descent per scope that holds such a block, not
	// one row per block. It runs with sorts off (embedExcludedBatchNoSort),
	// where the index range in key order is the only path of a step without a
	// sort, and only while that index stands (embedExcludedPendingIndexReadySQL);
	// before, embedExcludedStarScopesBefore193SQL. Sent as the pool's own
	// statement before, the pending half was planned on idx_context_scope by
	// the production image's generic plan at random_page_cost 4 and read every
	// pending block of the table's scopes (46 884 buffers at 1 000 000 pending
	// blocks of another type). The stale half has no index carrying the name
	// and reads the stale blocks of every name (idx_embedding_stale_sens), as
	// before; the marked blocks of a name leave both halves.
	embedExcludedStarScopesSQL = `WITH RECURSIVE p(scope) AS (
		  (SELECT scope FROM context_blocks
		    WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
		      AND type_name >= $1::text AND type_name <= $1::text
		    ORDER BY type_name, scope
		    LIMIT 1)
		  UNION ALL
		  SELECT n.scope FROM p CROSS JOIN LATERAL (
		    SELECT scope FROM context_blocks
		     WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
		       AND (type_name, scope) > ($1::text, p.scope) AND type_name <= $1::text
		     ORDER BY type_name, scope
		     LIMIT 1) n)
		SELECT scope::text FROM p
		UNION
		SELECT scope::text FROM context_blocks
		 WHERE embedding_stale AND NOT is_archived AND type_name = $1
		ORDER BY 1`
	// embedExcludedStarScopesBefore193SQL is embedExcludedStarScopesSQL while
	// idx_embedding_pending_type_scope does not stand: both halves select the
	// name by equality, on the pool, at their cost before 193.
	embedExcludedStarScopesBefore193SQL = `SELECT scope::text FROM context_blocks
		 WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded AND type_name = $1
		UNION
		SELECT scope::text FROM context_blocks
		 WHERE embedding_stale AND NOT is_archived AND type_name = $1
		ORDER BY 1`
)

// embedExcludedPendingIndexReadySQL tells whether
// idx_embedding_pending_type_scope stands as migration 193 builds it (valid,
// ready, the deparsed definition the migration adopts). Migration 193 does
// not build it above 500 000 rows, nor over an INVALID or foreign index of
// the name; a re-embed migration swapped by a binary before 193 leaves it on
// the old column (its predicate then names embedding_old) and drops it with
// that column at the cleanup. Without it the mark's selection has no range
// of the name and the scope, so the reconcile sends the statements before
// 193 (markEmbedExcludedBefore193SQL, embedExcludedMarkLeftBefore193SQL), at
// their cost. Read once per reconcile pass that has a queued entry.
const embedExcludedPendingIndexReadySQL = `SELECT EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.idx_embedding_pending_type_scope') AND indisvalid AND indisready
		  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX idx_embedding_pending_type_scope ON public.context_blocks USING btree (type_name, scope, id) WHERE ((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))')`

// embedExcludedBatchNoSort turns sorts off for a mark or clear batch, the
// mark's left check and the scopes of a '*' entry (embedExcludedEntryScopes),
// in the statement's own transaction (embedExcludedBatch):
// every path of its selection besides its index range in key order (the
// name's and scope's range of idx_embedding_pending_type_scope or of
// idx_blocks_embed_excluded, the scope's range of
// idx_embedding_stale_scope_sens or, before 193, of
// idx_embedding_pending_scope_sens) needs a sort, and with sorts off the planner
// takes a path without a disabled node whenever one exists (PostgreSQL 18
// compares the count of disabled nodes before the cost; the guard pick does
// the same, guard.guardPickNoSort). The sort of the batch's own rows for the
// cursor (over u) is in every plan and stays. The setting ends with the
// transaction; it covers the statement's row and statement triggers as well,
// whose plans keep their sort-free alternatives as any plan does.
const embedExcludedBatchNoSort = "SET LOCAL enable_sort = off; SET LOCAL enable_incremental_sort = off"

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
// blocks in the scopes of the group to their answer
// (ctx_block_type_never_embedded(name, scope), read once per scope) —
// marking takes the blocks without a vector and the stale ones (which drop
// their vector), clearing every marked one whose answer is false, so a block
// that holds a fresh vector keeps false — batch by batch, each batch its own
// statement, until no block of the entry needs a change, and then removes
// the queue row unless a change queued it again in the meantime (queued_at
// moved). It holds no queue row while it
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
	if len(q) == 0 {
		return res, nil
	}
	var typeIndex bool
	if err := pool.QueryRow(ctx, embedExcludedPendingIndexReadySQL).Scan(&typeIndex); err != nil {
		return res, fmt.Errorf("store: embed-excluded pending index probe: %w", err)
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
		n, done, err := reconcileEmbedExcludedEntry(ctx, pool, e.name, e.grp, typeIndex)
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

// reconcileEmbedExcludedEntry runs the rounds of one queue entry. Each round
// reads the answer of every scope of the entry once, marks the scopes that
// answer true (pending blocks, then stale ones) and clears the scopes that
// answer false; a '*' entry clears the scopes embedExcludedStarClearScopes
// names. done=false: a block of the entry still needs a change after
// embedExcludedRounds rounds, or a batch's rows were all set back by the
// trigger (the answer moved after the batch chose them), so the entry is
// left for the next pass. typeIndex: idx_embedding_pending_type_scope stands
// (embedExcludedPendingIndexReadySQL), so the mark and its left check read
// its range.
func reconcileEmbedExcludedEntry(ctx context.Context, pool *pgxpool.Pool, name, grp string, typeIndex bool) (updated int, done bool, err error) {
	for round := 0; round < embedExcludedRounds; round++ {
		scopes, err := embedExcludedEntryScopes(ctx, pool, name, grp, typeIndex)
		if err != nil {
			return updated, false, err
		}
		settled := true
		var marking, clearing []string
		for _, scope := range scopes {
			never, err := embedExcludedAnswer(ctx, pool, name, scope)
			if err != nil {
				return updated, false, err
			}
			if !never {
				clearing = append(clearing, scope)
				continue
			}
			marking = append(marking, scope)
			n, ok, err := markEmbedExcludedScope(ctx, pool, name, scope, typeIndex)
			updated += n
			if err != nil {
				return updated, false, err
			}
			settled = settled && ok
		}
		if grp == "*" {
			if clearing, err = embedExcludedStarClearScopes(ctx, pool, name, typeIndex); err != nil {
				return updated, false, err
			}
		}
		for _, scope := range clearing {
			n, ok, err := embedExcludedIDPass(ctx, pool, clearEmbedExcludedSQL, "clear", name, scope)
			updated += n
			if err != nil {
				return updated, false, err
			}
			settled = settled && ok
		}
		left, err := embedExcludedLeft(ctx, pool, name, marking, clearing, typeIndex)
		if err != nil {
			return updated, false, err
		}
		if !left && settled {
			return updated, true, nil
		}
	}
	return updated, false, nil
}

// embedExcludedAnswer reads ctx_block_type_never_embedded(name, scope): the
// same for every block of the name in the scope.
func embedExcludedAnswer(ctx context.Context, pool *pgxpool.Pool, name, scope string) (bool, error) {
	var never bool
	if err := pool.QueryRow(ctx, `SELECT ctx_block_type_never_embedded($1, $2)`, name, scope).Scan(&never); err != nil {
		return false, fmt.Errorf("store: embed-excluded answer %q %q: %w", name, scope, err)
	}
	return never, nil
}

// embedExcludedStarClearScopes returns the scopes a '*' entry (a change of
// the name's _global row, the seed of 176, a moved 175 entry) has to clear:
// the candidates whose answer is false. While the name's _global row is
// excluded, a group answers false only when it holds a row of the name with
// another policy, so the candidates are the scopes of those groups (read
// from the registry); every other group answers true and has nothing to
// clear. Without an excluded _global row (deleted, or another policy) any
// scope may answer false, and the candidates are the scopes that hold a
// marked block of the name (embedExcludedMarkedScopesSQL). typeIndex as for
// reconcileEmbedExcludedEntry.
func embedExcludedStarClearScopes(ctx context.Context, pool *pgxpool.Pool, name string, typeIndex bool) ([]string, error) {
	var globalExcluded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM context_block_types
		 WHERE name = $1 AND scope = '_global' AND config->'retrieval'->>'policy' = 'excluded')`, name).Scan(&globalExcluded); err != nil {
		return nil, fmt.Errorf("store: embed-excluded _global row %q: %w", name, err)
	}
	var candidates []string
	if globalExcluded {
		rows, err := pool.Query(ctx, `SELECT DISTINCT ctx_embed_excluded_group(scope) FROM context_block_types
			 WHERE name = $1 AND scope <> '_global' AND config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded'
			 ORDER BY 1`, name)
		if err != nil {
			return nil, fmt.Errorf("store: embed-excluded lifting groups %q: %w", name, err)
		}
		groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("store: embed-excluded lifting groups %q: %w", name, err)
		}
		for _, g := range groups {
			scopes, err := embedExcludedEntryScopes(ctx, pool, name, g, typeIndex)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, scopes...)
		}
	} else {
		rows, err := pool.Query(ctx, embedExcludedMarkedScopesSQL, name)
		if err != nil {
			return nil, fmt.Errorf("store: embed-excluded marked scopes %q: %w", name, err)
		}
		if candidates, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, fmt.Errorf("store: embed-excluded marked scopes %q: %w", name, err)
		}
	}
	var clearing []string
	for _, scope := range candidates {
		never, err := embedExcludedAnswer(ctx, pool, name, scope)
		if err != nil {
			return nil, err
		}
		if !never {
			clearing = append(clearing, scope)
		}
	}
	return clearing, nil
}

// embedExcludedEntryScopes returns the scopes of a queue entry's group: the
// scopes of the tenant for 't:<tenant_id>' (at this read; an assignment
// queues the tenant's group again), the one scope for 's:<scope>', and for
// '*' the scopes in which a block of the name may need a marker
// (embedExcludedStarScopesSQL, in a transaction with sorts off, while
// idx_embedding_pending_type_scope stands — typeIndex — and
// embedExcludedStarScopesBefore193SQL otherwise; what '*' clears,
// embedExcludedStarClearScopes says).
func embedExcludedEntryScopes(ctx context.Context, pool *pgxpool.Pool, name, grp string, typeIndex bool) ([]string, error) {
	var rows pgx.Rows
	var err error
	switch {
	case grp == "*" && typeIndex:
		var scopes []string
		if err := pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, embedExcludedBatchNoSort); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, embedExcludedStarScopesSQL, name)
			if err != nil {
				return err
			}
			scopes, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			return nil, fmt.Errorf("store: embed-excluded scopes %q %q: %w", name, grp, err)
		}
		return scopes, nil
	case grp == "*":
		rows, err = pool.Query(ctx, embedExcludedStarScopesBefore193SQL, name)
	case strings.HasPrefix(grp, "t:"):
		rows, err = pool.Query(ctx, `SELECT scope::text FROM context_tenant_scopes WHERE tenant_id::text = $1 ORDER BY scope`, grp[2:])
	case strings.HasPrefix(grp, "s:"):
		return []string{grp[2:]}, nil
	default:
		return nil, fmt.Errorf("store: embed-excluded entry %q: unknown group %q", name, grp)
	}
	if err != nil {
		return nil, fmt.Errorf("store: embed-excluded scopes %q %q: %w", name, grp, err)
	}
	scopes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: embed-excluded scopes %q %q: %w", name, grp, err)
	}
	return scopes, nil
}

// embedExcludedLeft reports whether a block of the entry still needs a
// change: an unmarked pending or a stale block of the name in a scope that
// answered true, or a marked block of the name in a scope the round cleared
// that still answers false. typeIndex as for reconcileEmbedExcludedEntry.
func embedExcludedLeft(ctx context.Context, pool *pgxpool.Pool, name string, marking, clearing []string, typeIndex bool) (bool, error) {
	markLeft := embedExcludedMarkLeftBefore193SQL
	if typeIndex {
		markLeft = embedExcludedMarkLeftSQL
	}
	for _, scope := range marking {
		var left bool
		if err := embedExcludedBatch(ctx, pool, markLeft, []any{name, scope}, &left); err != nil {
			return false, fmt.Errorf("store: embed-excluded left %q %q: %w", name, scope, err)
		}
		if left {
			return true, nil
		}
	}
	for _, scope := range clearing {
		var left bool
		if err := pool.QueryRow(ctx, embedExcludedClearLeftSQL, name, scope).Scan(&left); err != nil {
			return false, fmt.Errorf("store: embed-excluded left %q %q: %w", name, scope, err)
		}
		if left {
			return true, nil
		}
	}
	return false, nil
}

// markEmbedExcludedScope marks the name's blocks of one scope that answered
// true: the unmarked pending ones batch by batch from the start of the
// range — of the name and the scope on idx_embedding_pending_type_scope
// (typeIndex), else of the scope (the statement before 193) — then the
// stale ones. A row the trigger set back (the answer moved) stays in the set
// these statements read, and the cursor keeps a pass from reading it again;
// the left check and the next round take what a pass passed. The pass
// without a created_at of the releases before 193 is gone: created_at is NOT
// NULL since migration 186, and that pass read no row since.
// settled=false: a batch wrote rows and none of them kept the marker (the
// trigger set them back, the answer moved), and the pass stopped there.
func markEmbedExcludedScope(ctx context.Context, pool *pgxpool.Pool, name, scope string, typeIndex bool) (updated int, settled bool, err error) {
	var n int
	var ok bool
	if typeIndex {
		n, ok, err = embedExcludedIDPass(ctx, pool, markEmbedExcludedSQL, "mark", name, scope)
	} else {
		n, ok, err = markEmbedExcludedKeyset(ctx, pool, markEmbedExcludedBefore193SQL, name, scope)
	}
	updated += n
	if err != nil || !ok {
		return updated, false, err
	}
	n, ok, err = markEmbedExcludedKeyset(ctx, pool, markEmbedExcludedStaleSQL, name, scope)
	return updated + n, ok && err == nil, err
}

// markEmbedExcludedKeyset runs a mark pass on the keyset (sensitivity,
// created_at, id) of one scope — markEmbedExcludedBefore193SQL or
// markEmbedExcludedStaleSQL — batch by batch from the start of the scope's
// range. settled=false as for markEmbedExcludedScope.
func markEmbedExcludedKeyset(ctx context.Context, pool *pgxpool.Pool, sql, name, scope string) (updated int, settled bool, err error) {
	cursor := embedExcludedKeyStart
	for {
		var wrote, reached int
		var sens *string
		var at *time.Time
		var id *string
		if err := embedExcludedBatch(ctx, pool, sql, []any{name, scope,
			cursor.sens, cursor.at, cursor.id, EmbedExcludedReconcileBatch}, &wrote, &reached, &sens, &at, &id); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded mark %q %q: %w", name, scope, err)
		}
		updated += reached
		if wrote > 0 && reached == 0 {
			return updated, false, nil
		}
		if wrote == 0 {
			return updated, true, nil
		}
		if sens != nil && at != nil && id != nil {
			cursor = embedExcludedKey{sens: *sens, at: *at, id: *id}
		}
	}
}

// embedExcludedBatch runs one batch statement (or the mark's left check) in a
// transaction of its own with sorts off (embedExcludedBatchNoSort) and scans
// its one row into dest.
func embedExcludedBatch(ctx context.Context, pool *pgxpool.Pool, sql string, args []any, dest ...any) error {
	return pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, embedExcludedBatchNoSort); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql, args...).Scan(dest...)
	})
}

// embedExcludedIDPass runs a pass on the keyset id of one name and scope —
// clearEmbedExcludedSQL (what "clear": the name's marked blocks of a scope
// that answered false) or markEmbedExcludedSQL ("mark": its unmarked pending
// blocks of a scope that answered true) — batch by batch from the start of
// the range. settled=false as for markEmbedExcludedScope.
func embedExcludedIDPass(ctx context.Context, pool *pgxpool.Pool, sql, what, name, scope string) (updated int, settled bool, err error) {
	from := embedExcludedKeyStart.id
	for {
		var wrote, reached int
		var last *string
		if err := embedExcludedBatch(ctx, pool, sql, []any{name, scope, from, EmbedExcludedReconcileBatch},
			&wrote, &reached, &last); err != nil {
			return updated, false, fmt.Errorf("store: embed-excluded %s %q %q: %w", what, name, scope, err)
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
