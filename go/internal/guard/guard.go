// Package guard implements the write guard that detects near-duplicate blocks.
// Uses an HNSW similarity check; since WF T7 (M074) the thresholds and the
// candidate set are POLICY — resolved per block type from the block-type
// registry (blocktype.Set.GuardThresholds / GuardCandidateTypes, seed
// defaults 0.98 auto-archive / 0.92 flag-for-review = the former literals)
// and passed to ctx_guard_check as mandatory parameters.
//
// Wave I-J (design/02 §4.7) adds two more per-type policy dimensions, both
// resolved in processBlock and threaded through EXPLICITLY:
//   - GuardSameScopeOnly → p_same_scope_only (guard.candidates=same-scope):
//     the issue axis restricts candidates to the block's own scope so a guard
//     match can never leak the existence/similarity of a cross-tenant block
//     (§5.3). When TRUE, M074 sets hnsw.iterative_scan='relaxed_order' so the
//     filtered-ANN keeps its recall at 1M+ scale.
//   - GuardMode (guard.mode=flag) → applyDecision persists a possible_duplicate
//     flag instead of auto-archiving, so a duplicate issue is surfaced, never
//     silently removed (§4.7). archive stays the knowledge-line bestand.
package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// GuardResult holds the outcome of processing a single block. The two
// thresholds record the POLICY VALUES the decision was made under (per-type,
// T7) — applyDecision and the audit log persist them instead of literals.
type GuardResult struct {
	BlockID            string
	Decision           string
	Similarity         float64
	MatchedID          *string
	MatchedTitle       *string
	IsCrossScope       bool
	ThresholdDuplicate float64
	ThresholdReview    float64
	// SupersedesPair is true when the match is the other end of a PINNED
	// supersedes link with the checked block (issue #197): the pair is
	// already explained as a supersession, so checkBlock records it clean.
	SupersedesPair bool
}

// pendingBlock is one pick-query row: the block plus its policy type (the
// per-type threshold key).
type pendingBlock struct {
	id       string
	typeName string
	// exact is set by prepareChecks: the block's candidate circle is small
	// enough to be checked exactly (candidateCircle.exact, issue #381).
	exact bool
}

// guardPendingIndexWhere is the part of the guard-batch pending predicate a
// catalog can hold, and word for word the predicate of idx_guard_pending_type
// (migration 182, issue #360): not archived, no verdict, a vector that is not
// stale, a category other than index, the lifecycle knowledge. The type
// allowlist comes from the registry and cannot be part of a predicate; it is
// that index's leading key instead. The first three conjuncts are the
// predicate of idx_guard_pending (M074), which the statement before 182 reads
// while migration 183 keeps it (guardPickBefore182SQL). A change of one of
// these conjuncts changes idx_guard_pending_type
// in the same step (a migration and its twin in events.verifySisterIndexes):
// the planner takes a partial index only when it proves that the statement
// implies the predicate.
const guardPendingIndexWhere = `NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND NOT embedding_stale
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'`

// guardPendingWhere is THE single guard-batch pending predicate (WF T7,
// design/01 §7-T7): the pick and PendingStats (manage guard-stats, issue
// #341) consume this one fragment instead of carrying two copies.
// typesParam binds the GuardCheckTypes allowlist (guard.check=true types).
// NOT embedding_stale (issue #198, part B: a title or content change keeps
// the previous vector until the backfill writes the new one, and the change
// also drops guard_checked_at, so without it the batch would check the new
// text by the old text's vector and write the verdict as final), category !=
// 'index' (topic-map mechanism rest, §4.2: agent briefings share the
// system-meta type but ARE checked — dropping the category rest would
// silently guard-check the topic-map), the lifecycle gate. typesParam is a
// code-owned bind placeholder at every call site — never user input.
func guardPendingWhere(typesParam string) string {
	return guardPendingIndexWhere + `
		  AND type_name = ANY(` + typesParam + `::text[])`
}

// guardPickKey is the order of the pick: created_at as UTC wall time, which
// orders exactly as created_at does (UTC has no offset changes) and is
// immutable, so an index can carry it. It is the second key column of
// idx_guard_pending_type (migration 182) and no other index carries it — the
// point of it (issue #360): for an ORDER BY on created_at itself the planner
// can also walk idx_context_created, the whole store in created_at order,
// and it took that walk past a large checked corpus whenever its estimate
// put the waiting blocks early in it (they are the newest). Ordered by this
// key the only ordered path is idx_guard_pending_type, a range per type that
// ends after the batch limit. Written as the function call so that the
// statement and the index deparse alike in every session time zone.
const guardPickKey = `timezone('UTC', created_at)`

// guardPickSavepoint names the savepoint around the pick's first step.
const guardPickSavepoint = "guard_pick"

// guardPickNoSort is set for step 1 only, inside its savepoint: every path a
// type's branch could take besides the range of idx_guard_pending_type in
// key order needs a sort (no other index carries the key), and with
// enable_sort off the planner takes a path without a disabled node whenever
// one exists (PostgreSQL 18 compares the count of disabled nodes before the
// cost). Without it the planner chose, per branch, a sort over all entries
// of idx_guard_pending — the index of 113 that stood beside 182 until
// migration 183 — whenever its row estimates were small, and read that index
// once per checked type (issue #360: 29 158 buffers beside 100 000 foreign
// entries, 5 831 for the pick before 182); a sort over all waiting blocks of
// a type is the same path without it. The sort above the branches is in
// every plan and stays. ROLLBACK TO SAVEPOINT undoes the setting; when the
// pick keeps step 1's locks, it is set back before the savepoint is released.
const (
	guardPickNoSort      = "SET LOCAL enable_sort = off"
	guardPickRestoreSort = "SET LOCAL enable_sort TO DEFAULT"
)

// guardPickLockSQL is step 1 of the pick ($1 = the GuardCheckTypes allowlist,
// $2 = the batch limit): per checked type the oldest waiting blocks, at most
// the batch limit of each, locked and returned with their row version
// (ctid), oldest first over all types. Each type's branch is one range of
// idx_guard_pending_type read in key order, so the cost follows the blocks it
// locks — not the waiting blocks of unchecked types, nor the blocks the
// predicate leaves out, nor the checked corpus. The lock sits in the branch:
// SKIP LOCKED there passes over blocks another run holds and goes on to the
// next of that type (a lock above the branch would read the same oldest
// blocks of the type, find them held and skip them, so a second run got
// nothing while more blocks of the type wait). DISTINCT keeps a type named
// twice from returning its blocks twice.
var guardPickLockSQL = `SELECT x.tid::text, x.id, x.type_name
	  FROM (SELECT DISTINCT unnest($1::text[])) AS t(name)
	 CROSS JOIN LATERAL (
		SELECT ctid AS tid, id, type_name, ` + guardPickKey + ` AS k
		  FROM context_blocks
		 WHERE type_name = t.name
		   AND ` + guardPendingIndexWhere + `
		 ORDER BY ` + guardPickKey + `
		 LIMIT $2
		   FOR NO KEY UPDATE SKIP LOCKED) x
	 ORDER BY x.k`

// guardPickRelockSQL is step 2 of the pick ($1 = the row versions of the
// blocks step 1 kept, $2 = the allowlist, $3 = their ids): it locks those
// versions again, after step 1's locks were given back. The WHERE clause is
// the row versions and the ids only, both bounded by the batch, so the plan
// is a TID scan or a primary-key lookup whatever the statistics (the pick
// predicate there let the planner turn to a pending index and read every
// waiting block, issue #360). The id test before the lock keeps a row
// version that VACUUM freed and a later write reused for another block out
// of the batch and unlocked. Whether a locked row joins the batch is the
// column waiting, computed on the version the lock got: the whole pick
// predicate and the identity of the block. A block written between the two
// steps has a new version and the old one is no longer visible, so it falls
// out of this batch and waits for the next; one written while this statement
// runs is locked on its newest version and joins the batch only if the
// recheck of the scan returns that version and waiting holds there. A
// locked row with waiting false stays locked to the end of the batch
// transaction and is not processed.
var guardPickRelockSQL = `SELECT id, type_name,
	       COALESCE(` + guardPendingWhere("$2") + `
		  AND id = ANY($3::uuid[]), false) AS waiting
	  FROM context_blocks
	 WHERE ctid = ANY($1::tid[])
	   AND id = ANY($3::uuid[])
	 ORDER BY created_at
	   FOR NO KEY UPDATE SKIP LOCKED`

// guardPickIndexReadySQL tells whether idx_guard_pending_type stands as
// migration 182 builds it (valid, ready, the deparsed definition the
// migration adopts). Migration 182 does not build it above 500 000 rows, nor
// over an INVALID or foreign index of the name, and a re-embed migration
// swapped by a binary before 182 leaves it on the old column and drops it at
// the cleanup. Without it every branch of step 1 needs a sort, and the
// planner may sort a whole index or all blocks of a type per checked type;
// the pick then sends its statement before 182 (guardPickBefore182SQL),
// which costs what it cost before.
const guardPickIndexReadySQL = `SELECT EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.idx_guard_pending_type') AND indisvalid AND indisready
		  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX idx_guard_pending_type ON public.context_blocks USING btree (type_name, timezone(''UTC''::text, created_at)) WHERE ((NOT is_archived) AND ((metadata ->> ''guard_checked_at''::text) IS NULL) AND (embedding IS NOT NULL) AND (NOT embedding_stale) AND ((category)::text <> ''index''::text) AND ((lifecycle_state)::text = ''knowledge''::text))')`

// guardPickBefore182SQL is the pick of the releases before migration 182,
// one statement ordered by created_at over idx_guard_pending: what the pick
// runs while idx_guard_pending_type does not stand (guardPickIndexReadySQL).
// Migration 183 drops idx_guard_pending only once idx_guard_pending_type
// stands, so this statement keeps its index (without it, it walks
// idx_context_created through the store, docs/operations.md "Migration
// 183"); should idx_guard_pending_type be lost after 183, rebuild it.
var guardPickBefore182SQL = `SELECT id, type_name FROM context_blocks
	WHERE ` + guardPendingWhere("$1") + `
	ORDER BY created_at ASC
	LIMIT $2
	FOR NO KEY UPDATE SKIP LOCKED`

// lockedPick is one row of step 1: the block and the row version it locked.
type lockedPick struct {
	tid   string
	block pendingBlock
}

// pickPending takes the guard batch in tx: the oldest waiting blocks over all
// checked types, at most limit, locked FOR NO KEY UPDATE until tx ends, in
// created_at order; blocks another transaction holds are passed over (SKIP
// LOCKED), so runs side by side split the waiting blocks (issue #360).
//
// Step 1 (guardPickLockSQL) runs in a savepoint, with sorts off for its plan
// (guardPickNoSort), and locks up to limit blocks of every checked type, more
// than the batch when several types wait. When it locked no more than limit,
// every one of them is in the batch and the savepoint is released with the
// locks. Otherwise the first limit by created_at are kept, ROLLBACK TO
// SAVEPOINT gives back every lock of step 1,
// and step 2 (guardPickRelockSQL) locks the kept blocks again by their row
// versions. Between the two steps the kept blocks are not locked: another
// run can take them (step 2 then skips them, and this batch is smaller), and
// a writer can change them (they fall out of this batch). A run never takes
// a block another run holds, and never a block that does not wait.
//
// While idx_guard_pending_type does not stand as migration 182 builds it,
// the pick is the one statement of the releases before 182
// (guardPickBefore182SQL), at their cost.
func pickPending(ctx context.Context, tx pgx.Tx, checkTypes []string, limit int) ([]pendingBlock, error) {
	var ready bool
	if err := tx.QueryRow(ctx, guardPickIndexReadySQL).Scan(&ready); err != nil {
		return nil, fmt.Errorf("guard: pick index probe: %w", err)
	}
	if !ready {
		return pickBefore182(ctx, tx, checkTypes, limit)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT "+guardPickSavepoint+"; "+guardPickNoSort); err != nil {
		return nil, fmt.Errorf("guard: pick savepoint: %w", err)
	}
	locked, err := lockOldestPerType(ctx, tx, checkTypes, limit)
	if err != nil {
		return nil, err
	}
	if len(locked) <= limit {
		if _, err := tx.Exec(ctx, guardPickRestoreSort+"; RELEASE SAVEPOINT "+guardPickSavepoint); err != nil {
			return nil, fmt.Errorf("guard: pick release: %w", err)
		}
		blocks := make([]pendingBlock, len(locked))
		for i, l := range locked {
			blocks[i] = l.block
		}
		return blocks, nil
	}
	kept := locked[:limit]
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+guardPickSavepoint+"; RELEASE SAVEPOINT "+guardPickSavepoint); err != nil {
		return nil, fmt.Errorf("guard: pick rollback: %w", err)
	}
	return relockKept(ctx, tx, kept, checkTypes)
}

// pickBefore182 runs guardPickBefore182SQL.
func pickBefore182(ctx context.Context, tx pgx.Tx, checkTypes []string, limit int) ([]pendingBlock, error) {
	rows, err := tx.Query(ctx, guardPickBefore182SQL, checkTypes, limit)
	if err != nil {
		return nil, fmt.Errorf("guard: fetch pending: %w", err)
	}
	defer rows.Close()
	var blocks []pendingBlock
	for rows.Next() {
		var b pendingBlock
		if err := rows.Scan(&b.id, &b.typeName); err != nil {
			return nil, fmt.Errorf("guard: scan block id: %w", err)
		}
		blocks = append(blocks, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard: rows error: %w", err)
	}
	return blocks, nil
}

// lockOldestPerType runs step 1 of the pick.
func lockOldestPerType(ctx context.Context, tx pgx.Tx, checkTypes []string, limit int) ([]lockedPick, error) {
	rows, err := tx.Query(ctx, guardPickLockSQL, checkTypes, limit)
	if err != nil {
		return nil, fmt.Errorf("guard: fetch pending: %w", err)
	}
	defer rows.Close()
	var locked []lockedPick
	for rows.Next() {
		var l lockedPick
		if err := rows.Scan(&l.tid, &l.block.id, &l.block.typeName); err != nil {
			return nil, fmt.Errorf("guard: scan block id: %w", err)
		}
		locked = append(locked, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard: rows error: %w", err)
	}
	return locked, nil
}

// relockKept runs step 2 of the pick and returns the kept blocks it locked
// that still wait, in created_at order.
func relockKept(ctx context.Context, tx pgx.Tx, kept []lockedPick, checkTypes []string) ([]pendingBlock, error) {
	tids := make([]string, len(kept))
	ids := make([]string, len(kept))
	for i, k := range kept {
		tids[i], ids[i] = k.tid, k.block.id
	}
	rows, err := tx.Query(ctx, guardPickRelockSQL, tids, checkTypes, ids)
	if err != nil {
		return nil, fmt.Errorf("guard: relock pending: %w", err)
	}
	defer rows.Close()
	var blocks []pendingBlock
	for rows.Next() {
		var b pendingBlock
		var waiting bool
		if err := rows.Scan(&b.id, &b.typeName, &waiting); err != nil {
			return nil, fmt.Errorf("guard: scan relocked block: %w", err)
		}
		if waiting {
			blocks = append(blocks, b)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard: relock rows error: %w", err)
	}
	return blocks, nil
}

// lastGuardAtSQL stamps the end of a committed guard run. It is the guard's
// only write of context_guard_state left (issue #341, migration 181); the
// write-log retention records its eviction boundary in the same row
// (store.EvictWriteLog, migration 194), in a statement of its own.
const lastGuardAtSQL = `UPDATE context_guard_state SET last_guard_at = now() WHERE id = true`

// pendingStatsSQL counts what the pick would take, within the read scopes in
// $1 ($2 = the GuardCheckTypes allowlist), and the oldest updated_at among
// those blocks — one pass over a partial pending index (the predicates have
// no parameter, so a generic plan can use them too): idx_guard_pending_type
// with the allowlist as its index condition since migration 182 (issue
// #360); scope = ANY($1) filters the index result or joins
// idx_context_scope in a BitmapAnd. The cost follows the waiting blocks of
// the checked types. Until migration 183 dropped idx_guard_pending the
// planner could also read that index (the three first conjuncts), whose
// entries include the unchecked blocks with a vector the pick filters out —
// docs/operations.md, "Migration 181", "Migration 182" and "Migration 183".
var pendingStatsSQL = `SELECT count(*)::int, min(updated_at) FROM context_blocks
	WHERE scope = ANY($1::text[])
	  AND ` + guardPendingWhere("$2")

// PendingStats returns how many blocks within readScopes the guard pick would
// still take (the pick's own predicate, guardPendingWhere, with the
// allowlist checkTypes — the pick's, i.e. the BASE registry snapshot), and
// the oldest updated_at among them (nil when none waits). It backs manage
// guard-stats' pending_count and dirty_since (issue #341): counted when asked,
// per caller, instead of a global counter every block writer had to lock.
// Empty or nil readScopes count nothing (scope = ANY of an empty or NULL
// array matches no row).
func PendingStats(ctx context.Context, q pgxdb.Rower, readScopes, checkTypes []string) (int, *time.Time, error) {
	var (
		n      int
		oldest *time.Time
	)
	if err := q.QueryRow(ctx, pendingStatsSQL, readScopes, checkTypes).Scan(&n, &oldest); err != nil {
		return 0, nil, fmt.Errorf("guard: pending stats: %w", err)
	}
	return n, oldest, nil
}

// RunGuardBatch processes pending blocks through the duplicate detection guard.
// Uses the ctx_guard_check PG function for each block.
// Returns the number of blocks processed and any error.
//
// pool is the minimum *pgxpool.Pool surface this function needs, composed from
// the two pgxdb handles it actually uses. Naming the SHAPE rather than the
// pool type is what lets tests pass a pgxmock-backed pool without exercising a
// real database; *pgxpool.Pool implicitly satisfies it.
//
// set is the resolved block-type policy snapshot (WF T7): the pick predicate
// consumes GuardCheckTypes, the per-block call resolves GuardThresholds by
// the block's type and passes GuardCandidateTypes as the candidate allowlist.
// A nil set is a wiring bug and fails loudly (rrf.Search pattern) — an empty
// GuardCheckTypes list is legitimate policy ("no type is guard-checked") and
// simply yields zero picks.
//
// The run takes no lock on context_guard_state while it works (issue #341):
// since migration 181 no trigger counts block writes into that row, and the
// run writes only last_guard_at, in a statement of its own after its
// transaction has committed. It then holds no block row while it waits for
// the state row (another instance's run may hold it for a moment), so no
// block writer waits for it and no lock cycle runs through the row. The
// value is the time just after the run's commit (before 181 it was the start
// of the run's transaction); a run that fails or is interrupted writes none,
// as before. How many blocks wait is counted on demand (PendingStats).
//
// Tx-Abort-Kaskade fix (W47-02): Each block is wrapped in a SAVEPOINT so a
// failed block (SQL error in checkBlock/applyDecision/writeAuditLog) does not
// poison the surrounding transaction. Without the savepoint, the first error
// puts the tx into the "aborted" state (PG error 25P02) and every subsequent
// statement fails — losing all later block updates. With per-block savepoints,
// a failure is ROLLBACK'd back to its savepoint, the outer tx stays clean, and
// the loop continues to the next block.
func RunGuardBatch(ctx context.Context, pool interface {
	pgxdb.Beginner
	pgxdb.Execer
}, set *blocktype.Set, limit int, search Search) (int, error) {
	processed, _, err := RunGuardBatchDeferring(ctx, pool, set, limit, search)
	return processed, err
}

// RunGuardBatchDeferring is RunGuardBatch that also returns how many verdicts
// the committed run deferred (0 for a run that failed): the scheduler keeps
// the guard pending while a run defers any, so the deferred blocks get their
// verdict at a later tick without waiting for another write (issue #555).
func RunGuardBatchDeferring(ctx context.Context, pool interface {
	pgxdb.Beginner
	pgxdb.Execer
}, set *blocktype.Set, limit int, search Search) (int, int64, error) {
	if set == nil {
		return 0, 0, fmt.Errorf("guard: nil block-type policy set (registry not wired?)")
	}
	if search.EfSearch < EfSearchMin || search.EfSearch > EfSearchMax || search.ExactMaxCandidates < 0 {
		return 0, 0, fmt.Errorf("guard: search %+v outside hnsw.ef_search [%d, %d] or a negative exact limit (guard.* settings not wired?)",
			search, EfSearchMin, EfSearchMax)
	}
	if limit <= 0 {
		limit = 100
	}
	checkTypes := set.GuardCheckTypes()

	// Wrap entire batch in a transaction so the pick's SKIP LOCKED row locks
	// are held until all blocks are processed, preventing race conditions.
	// The bracket itself — open, deferred rollback, close — belongs to
	// pgxdb.Write; the two stage labels stay HERE and keep their wording,
	// because the pair is asymmetric (a tx suffix on both stages) and
	// pgxdb.At would rewrite it.
	//
	// blocks and processed outlive the closure: the batch line below is
	// logged only after a successful close, exactly as before, and the
	// caller still gets the number of processed blocks alongside an error.
	var (
		blocks    []pendingBlock
		processed int
		deferred  int64
	)
	err := pgxdb.Write(ctx, pool, pgxdb.Stages{
		Begin:  "guard: begin tx",
		Commit: "guard: commit tx",
	}, func(tx pgx.Tx) error {
		// Take the batch (pickPending: the shared guardPendingWhere fragment —
		// unchecked, not archived, not index, a current vector, knowledge
		// lifecycle, policy type allowlist —, the oldest by created_at over all
		// checked types, read by type from idx_guard_pending_type, migration
		// 182). SKIP LOCKED keeps concurrent guard runs from processing the
		// same blocks; the row locks are held for the duration of the
		// transaction.
		//
		// FOR NO KEY UPDATE, not FOR UPDATE (issue #359): it is the lock the
		// verdict's own UPDATE takes (no key column changes), and it does not
		// conflict with the key-share lock a foreign-key check takes. Another
		// run's audit row names one of these blocks as its matched neighbour
		// (matched_block_id); under FOR UPDATE that insert waited for this
		// whole batch, and two runs holding each other's neighbours waited
		// for each other until PostgreSQL aborted one insert with 40P01.
		var err error
		if blocks, err = pickPending(ctx, tx, checkTypes, limit); err != nil {
			return err
		}

		if len(blocks) == 0 {
			// Nothing to process: a nil return closes the transaction.
			return nil
		}

		// After the pick, so its statement plans without the setting, and
		// before the first check, so every check sees it (checkpath.go).
		if err := prepareChecks(ctx, tx, blocks, set, search); err != nil {
			return err
		}

		if processed, deferred, err = checkBlocks(ctx, tx, blocks, set, 0); err != nil || deferred == 0 {
			return err
		}
		// Deferred blocks stay at the front of the next pick (issue #547):
		// as many blocks again, past this run's, so the newer ones get their
		// verdict while the older wait for what holds them (pickPast).
		more, err := pickPast(ctx, tx, checkTypes, blocks, len(blocks)-processed, int(deferred))
		if err != nil || len(more) == 0 {
			return err
		}
		if err := prepareChecks(ctx, tx, more, set, search); err != nil {
			return err
		}
		blocks = append(blocks, more...)
		n, d, err := checkBlocks(ctx, tx, more, set, processed)
		processed, deferred = processed+n, deferred+d
		return err
	})

	if err != nil {
		return processed, 0, err
	}
	if len(blocks) > 0 {
		recordDeferrals(deferred) // an empty run leaves last_run to the last run that checked blocks
	}

	// After the commit, in a statement of its own: see the doc comment. A
	// context cancelled right after the commit (shutdown) leaves the stamp
	// out without an error line; the batch itself is committed.
	if _, err := pool.Exec(ctx, lastGuardAtSQL); err != nil && ctx.Err() == nil {
		slog.Error("guard: update state failed", "error", err)
	}

	if len(blocks) > 0 {
		slog.Info("guard: batch complete", "processed", processed, "total_pending", len(blocks))
	}
	return processed, deferred, nil
}

// savepointName builds a PG-safe SAVEPOINT identifier from a block UUID.
// PG identifiers must start with a letter/underscore, can contain letters,
// digits, and underscores. UUIDs contain hyphens which are not allowed, so
// we replace them with underscores and prepend "block_" to guarantee a
// letter-first start. UUID length without hyphens is 32 chars; with the
// prefix the result is 38 chars, well under NAMEDATALEN's 63-byte limit.
func savepointName(blockID string) string {
	return "block_" + strings.ReplaceAll(blockID, "-", "_")
}

// blockOutcome is what processBlock did with one block.
type blockOutcome int

const (
	blockFailed    blockOutcome = iota // an error, logged; the block stays pending
	blockProcessed                     // verdict and audit row written
	blockDeferred                      // left pending without an error (deferral.go)
)

// processBlock runs checkBlock + applyDecision + writeAuditLog for a single
// block inside its own SAVEPOINT. Returns blockProcessed iff all three steps
// succeeded. On any error the savepoint is rolled back so the surrounding
// transaction stays usable for the next block. A verdict that would archive
// the block first claims both of its blocks (claimArchivePair); an unclaimed
// verdict rolls back the same way and leaves the block pending, without an
// error, and so does an audit row whose matched neighbour another transaction
// holds against its key-share lock (neighbourKeyShare): blockDeferred.
//
// The per-type policy resolves HERE (by the block's type name) — one policy
// source for the SQL decision, the persisted metadata and the audit row:
//   - GuardThresholds: the dup/review thresholds (null config → 0.98/0.92).
//   - GuardSameScopeOnly: the p_same_scope_only value passed EXPLICITLY to
//     ctx_guard_check (guard.candidates=same-scope ⇒ TRUE; §4.7/§5.3).
//   - GuardMode: the persist mode (GuardModeArchive | GuardModeFlag) handed to
//     applyDecision so a flag-mode type (issues) is NEVER auto-archived (§4.7).
//
// The atomic-per-block semantic means an audit-log failure rolls the
// decision back too. That is acceptable: the block stays pending and will be
// picked up on the next guard cycle, where it can succeed cleanly or, if the
// underlying failure persists, fail again without poisoning the batch.
func processBlock(ctx context.Context, tx pgx.Tx, block pendingBlock, set *blocktype.Set) blockOutcome {
	blockID := block.id
	sp := savepointName(blockID)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		slog.Error("guard: savepoint create failed", "block_id", blockID, "error", err)
		return blockFailed
	}

	dup, review := set.GuardThresholds(block.typeName)
	sameScopeOnly := set.GuardSameScopeOnly(block.typeName)
	mode := set.GuardMode(block.typeName)
	result, err := checkBlock(ctx, tx, blockID, dup, review, set.GuardCandidateTypes(), sameScopeOnly, block.exact)
	if err != nil {
		slog.Error("guard: check block failed", "block_id", blockID, "error", err)
		rollbackToSavepoint(ctx, tx, sp)
		return blockFailed
	}

	var supersedesTargets []string
	if archives(result.Decision, mode) {
		targets, ok, failed := claimArchive(ctx, tx, blockID, result)
		if !ok {
			rollbackToSavepoint(ctx, tx, sp)
			if failed {
				return blockFailed
			}
			return blockDeferred
		}
		supersedesTargets = targets
	}

	if err := applyDecision(ctx, tx, blockID, result, mode); err != nil {
		slog.Error("guard: apply decision failed", "block_id", blockID, "error", err)
		rollbackToSavepoint(ctx, tx, sp)
		return blockFailed
	}

	// An archived block supersedes nothing: the targets of its supersedes
	// links are reconciled in the verdict's savepoint, as every archive does
	// (issue #284). Their rows are held already (claimArchive).
	if _, err := store.ReconcileSupersedesTargets(ctx, tx, supersedesTargets, ""); err != nil {
		slog.Error("guard: supersedes reconcile failed", "block_id", blockID, "error", err)
		rollbackToSavepoint(ctx, tx, sp)
		return blockFailed
	}

	if err := writeAuditLog(ctx, tx, blockID, result); err != nil {
		rollbackToSavepoint(ctx, tx, sp)
		if errors.Is(err, errVerdictDeferred) {
			slog.Info("guard: matched neighbour held by a deleting transaction or gone, block left for the next run",
				"block_id", blockID, "matched_id", *result.MatchedID)
			return blockDeferred
		}
		slog.Error("guard: audit log failed", "block_id", blockID, "error", err)
		return blockFailed
	}

	if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT "+sp); err != nil {
		// RELEASE failure is unusual but non-fatal — the work is already
		// part of the outer tx. Log and continue.
		slog.Warn("guard: savepoint release failed", "block_id", blockID, "error", err)
	}

	slog.Debug("guard: block processed",
		"block_id", blockID,
		"decision", result.Decision,
		"similarity", result.Similarity,
	)
	return blockProcessed
}

// claimArchive takes what an archive verdict needs before applyDecision
// archives blockID, none of it with a wait: the pair claim against the matched
// neighbour (claimArchivePair), then the archive's supersedes locks
// (store.TryLockSupersedesSource: the scope's guess lock, shared, which keeps
// out a dream batch that could add a supersedes link from the block, and the
// targets of its supersedes links), returning those targets for the reconcile
// after the archive. ok=false leaves the block pending for the next run — the
// caller rolls back to its savepoint, which releases every lock taken here —
// when another verdict claims one of the two blocks, the neighbour is gone,
// or another transaction holds the guess lock exclusively or a target (a
// dream batch with a supersedes guess of the scope, an update of a target):
// the next run decides on what that transaction left, no half verdict stays
// behind (issue #284). failed=true marks an error (logged) rather than a
// deferral.
func claimArchive(ctx context.Context, tx pgx.Tx, blockID string, result *GuardResult) (targets []string, ok, failed bool) {
	if result.MatchedID != nil {
		claimed, err := claimArchivePair(ctx, tx, blockID, *result.MatchedID)
		if err != nil {
			slog.Error("guard: claim archive failed", "block_id", blockID, "error", err)
			return nil, false, true
		}
		if !claimed {
			// Not an error: the block stays pending and the next run checks
			// it against what the concurrent decision left.
			slog.Info("guard: archive verdict meets a concurrent one, block left for the next run",
				"block_id", blockID, "matched_id", *result.MatchedID)
			return nil, false, false
		}
	}
	targets, held, err := store.TryLockSupersedesSource(ctx, tx, blockID)
	if err != nil {
		slog.Error("guard: archive supersedes locks failed", "block_id", blockID, "error", err)
		return nil, false, true
	}
	if !held {
		slog.Info("guard: archive verdict meets a supersedes write of its scope or target, block left for the next run",
			"block_id", blockID)
		return nil, false, false
	}
	return targets, true, false
}

// archives reports whether a verdict archives the checked block: a
// near_duplicate in every mode except flag (applyDecision's archive branch).
func archives(decision, mode string) bool {
	return decision == "near_duplicate" && mode != blocktype.GuardModeFlag
}

// archiveLockSQL takes, without waiting, the transaction-level advisory locks
// of an archive verdict: exclusive on the block that is archived ($1), shared
// on the neighbour that has to stay ($2), the lower id first and the second
// only when the first was granted (CASE fixes the order of evaluation). One
// key per block in a two-key class of its own ('ctx:guard-archive'), which
// shares no key with the one-key advisory locks or the supersedes-guess class.
const archiveLockSQL = `SELECT CASE WHEN $1::uuid < $2::uuid
	THEN CASE WHEN pg_try_advisory_xact_lock(hashtext('ctx:guard-archive'), hashtext($1::uuid::text))
		THEN pg_try_advisory_xact_lock_shared(hashtext('ctx:guard-archive'), hashtext($2::uuid::text))
		ELSE false END
	ELSE CASE WHEN pg_try_advisory_xact_lock_shared(hashtext('ctx:guard-archive'), hashtext($2::uuid::text))
		THEN pg_try_advisory_xact_lock(hashtext('ctx:guard-archive'), hashtext($1::uuid::text))
		ELSE false END
	END`

// archiveNeighbourLiveSQL reads, in a statement of its own after the locks,
// whether the matched neighbour still stands.
const archiveNeighbourLiveSQL = `SELECT EXISTS (SELECT 1 FROM context_blocks WHERE id = $1 AND NOT is_archived)`

// claimArchivePair makes archive verdicts that share a block mutually
// exclusive across concurrent guard runs (issue #359): archiving blockID
// against matchedID first takes the advisory lock of blockID exclusive and
// that of matchedID shared, then reads that matchedID still stands. Each run
// checks its block in its own snapshot, where another run's uncommitted
// archive is invisible, and the pick cannot order the runs by neighbour,
// because it takes its blocks before any neighbour is known.
//
// Two archive verdicts in a row of a cycle meet in one block, the archived
// block of the one (exclusive) and the neighbour of the other (shared), so
// they cannot be in flight at the same time, and the later one reads the
// earlier one's archive after its locks. That keeps archives from closing a
// cycle of any length: A→B and B→A cannot both stand, nor A→B, B→C, C→A —
// where C finds A as its neighbour only once B is gone, three different
// pairs. Verdicts that archive several blocks against the same standing
// neighbour only share its shared lock and go through side by side.
//
// It returns false, and the caller leaves the block pending for the next run,
// when another transaction holds a conflicting lock (it is deciding an
// archive that involves one of the two blocks; the decision becomes visible
// with its commit) or when the neighbour no longer stands. The read comes
// AFTER the locks, in a statement of its own: the check's snapshot predates
// them, and an archive committed in between is visible only to a later
// statement — under READ COMMITTED, the isolation the batch transaction runs
// at (pgxdb.Write opens it with the server default); a snapshot that lasted
// the whole transaction would read the neighbour as of the batch start. The
// locks never wait, so they add no edge a lock cycle could run through; they
// are held to the end of the batch transaction, when its archives become
// visible (a rolled-back savepoint releases them earlier).
func claimArchivePair(ctx context.Context, tx pgx.Tx, blockID, matchedID string) (bool, error) {
	var locked bool
	if err := tx.QueryRow(ctx, archiveLockSQL, blockID, matchedID).Scan(&locked); err != nil {
		return false, fmt.Errorf("archive locks: %w", err)
	}
	if !locked {
		return false, nil
	}
	var live bool
	if err := tx.QueryRow(ctx, archiveNeighbourLiveSQL, matchedID).Scan(&live); err != nil {
		return false, fmt.Errorf("archive neighbour: %w", err)
	}
	return live, nil
}

// rollbackToSavepoint rolls the tx back to the given savepoint. After
// ROLLBACK TO SAVEPOINT the surrounding tx is usable again. A failure here
// is logged but not propagated — the caller has already decided to abandon
// the block.
func rollbackToSavepoint(ctx context.Context, tx pgx.Tx, sp string) {
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); err != nil {
		slog.Error("guard: savepoint rollback failed", "savepoint", sp, "error", err)
	}
}

// checkBlock calls the ctx_guard_check PG function for a single block with
// the resolved policy parameters (M074 signature — NO defaults for the
// thresholds/candidates on the SQL side, and Go passes ALL FIVE parameters
// explicitly at this single call site (design/02 §4.7 enumeration rule:
// never rely on the p_same_scope_only SQL default). candidateTypes nil/empty
// ⇒ 0 candidates in SQL (fail-closed `= ANY(NULL)`); sameScopeOnly is the
// per-type policy value (GuardSameScopeOnly) — false is the knowledge-line
// bestand (cross-scope dedup), TRUE is the issue axis (guard.candidates=
// same-scope, §4.7/§5.3). When TRUE, ctx_guard_check sets
// hnsw.iterative_scan='relaxed_order' so the filtered-ANN does not silently
// miss the same-scope neighbour at 1M+ scale (M074, §4.7 recall gate).
// exact selects the statement that computes the same check exactly, without
// the HNSW index, for a block whose candidate circle is small; otherwise the
// call runs with its plan pinned to the index (guardIndexPlanOn, checkpath.go,
// issue #381). Both forms take the same five parameters.
func checkBlock(ctx context.Context, tx pgx.Tx, blockID string, thresholdDuplicate, thresholdReview float64, candidateTypes []string, sameScopeOnly, exact bool) (*GuardResult, error) {
	var (
		decision      string
		topSimilarity float64
		matchedID     *string
		matchedTitle  *string
		matchedScope  *string
		isCrossScope  bool
	)

	// supersedesPair rides on the same statement (issue #197): a match that is
	// the other end of a PINNED supersedes link with the checked block — a
	// writer's declaration, or a dream guess an operator confirmed — is not a
	// duplicate finding. A correction and its predecessor are near-identical
	// by construction, and the guard alone cannot tell which of the two
	// stands: it archived the NEWER statement against the older one. The link
	// tells it. Both directions, two primary-key probes; a dream guess
	// (unpinned) changes nothing here, as it changes nothing in the query
	// filter. The check stays top-1: the pair is recorded clean, and a second,
	// unrelated near-duplicate behind the predecessor is not looked for.
	var supersedesPair bool
	stmt := guardCheckExactSQL
	if !exact {
		stmt = guardCheckIndexSQL
		if _, err := tx.Exec(ctx, guardIndexPlanOn); err != nil {
			return nil, fmt.Errorf("guard: index plan: %w", err)
		}
	}
	err := tx.QueryRow(ctx, stmt,
		blockID, thresholdDuplicate, thresholdReview, candidateTypes, sameScopeOnly,
	).Scan(&decision, &topSimilarity, &matchedID, &matchedTitle, &matchedScope, &isCrossScope, &supersedesPair)
	if err != nil {
		return nil, fmt.Errorf("guard: ctx_guard_check: %w", err)
	}
	if !exact {
		if _, err := tx.Exec(ctx, guardIndexPlanOff); err != nil {
			return nil, fmt.Errorf("guard: index plan end: %w", err)
		}
	}
	if supersedesPair {
		decision = "clean"
	}

	return &GuardResult{
		BlockID:            blockID,
		Decision:           decision,
		Similarity:         topSimilarity,
		MatchedID:          matchedID,
		MatchedTitle:       matchedTitle,
		IsCrossScope:       isCrossScope,
		ThresholdDuplicate: thresholdDuplicate,
		ThresholdReview:    thresholdReview,
		SupersedesPair:     supersedesPair,
	}, nil
}

// FlaggedSQL returns the derived table every reader of the review queue's age
// reads (the /api/status rollup, the daily report): the live flagged rows of
// context_blocks — not archived, guard_status needs_review, near_duplicate or
// possible_duplicate — that also meet cond, with their id, scope,
// guard_status and flagged_at, the time the guard recorded its decision on
// the row. cond is "" or code-owned SQL text that starts with " AND ", never
// input. The caller names the table (FROM FlaggedSQL(…) AS f).
//
// flagged_at is metadata.guard_checked_at, which applyDecision writes (see
// flagStampSQL), since recording the decision does not move updated_at. The
// key is taken only when it can be a flag time of the row, and updated_at
// stands in otherwise: rows written before the write path kept the key to the
// guard (issue #238) may carry anything a caller sent.
//
//   - Absent or not a timestamp (a row older than the key, a repair
//     migration, a hand edit, a caller's value): pg_input_is_valid, not a bare
//     cast, since one unreadable value would fail the whole aggregate.
//   - A time before the row existed or after now(): the minimum over all
//     scopes must not be something one row can set to an arbitrary date.
//     created_at is cut to the second because the stamp has second
//     precision. The two bounds also keep 'infinity' and '-infinity' out
//     (valid input that does not scan into a time.Time, so one such row would
//     fail the read for every scope): -infinity is before every created_at,
//     infinity after every now().
//
// The key is read once per row, and its text is cast once: the two inner
// selects carry OFFSET 0, which keeps the planner from pulling them up and
// writing the expression back into every place that names its column. A
// metadata document stored out of line (TOAST) is fetched once per row
// instead of once per reference (issue #238: five fetches per row before).
// The CASE keeps the cast behind the validity test.
func FlaggedSQL(cond string) string {
	return `(SELECT id, scope, guard_status,
		        CASE WHEN flag_stamp >= date_trunc('second', created_at) AND flag_stamp <= now()
		             THEN flag_stamp ELSE updated_at END AS flagged_at
		   FROM (SELECT id, scope, guard_status, created_at, updated_at,
		                CASE WHEN pg_input_is_valid(flag_text, 'timestamptz') THEN flag_text::timestamptz END AS flag_stamp
		           FROM (SELECT id, scope, guard_status, created_at, updated_at,
		                        metadata->>'guard_checked_at' AS flag_text
		                   FROM context_blocks
		                  WHERE NOT is_archived
		                    AND guard_status IN ('needs_review', 'near_duplicate', 'possible_duplicate')` + cond + `
		                 OFFSET 0) AS flag_texts
		         OFFSET 0) AS flag_stamps)`
}

// flagStampSQL is the stamp applyDecision writes into metadata.guard_checked_at:
// the database's clock at the verdict's statement, cut to the second, in the
// RFC 3339 UTC form the Go clock wrote before (2006-01-02T15:04:05Z, byte for
// byte; GuardList hands the text to clients as checked_at). One clock with the
// readers: FlaggedSQL bounds the stamp by the reader's now(), and a stamp from
// an application clock that runs ahead of the database's made a fresh flag
// fall back to updated_at for the length of the skew (issue #238).
// statement_timestamp(), not now(): every block of a batch keeps the time of
// its own verdict, not the start of the batch's transaction.
const flagStampSQL = `to_char(statement_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`

// applyDecision updates the block based on the guard decision and the per-type
// persist mode (design/02 §4.7). mode == GuardModeArchive is the bestand: a
// near_duplicate is auto-archived. mode == GuardModeFlag (issues) NEVER
// archives — a near_duplicate is persisted as a possible_duplicate flag
// (guard_status='possible_duplicate' + guard_matched_id + similarity), so a
// duplicate issue is surfaced (guard-list / later issue-list) and never
// silently removed. needs_review and clean are mode-independent.
func applyDecision(ctx context.Context, tx pgx.Tx, blockID string, result *GuardResult, mode string) error {
	matchedIDVal := ""
	if result.MatchedID != nil {
		matchedIDVal = *result.MatchedID
	}

	if archives(result.Decision, mode) {
		// Auto-archive near duplicates. Bestand for every mode EXCEPT flag —
		// fail-safe to archive so an empty/unset mode (e.g. a policy built
		// outside DecodePolicy) never silently degrades to flag-only.
		// Explicit casts inside jsonb_build_object: VARIADIC "any" can't infer
		// types under pgx extended protocol on PG18 → 42P08 at PREPARE time.
		// updated_at moves here, unlike in the branch below: the block leaves
		// every default read, which is a change a reader can see.
		_, err := tx.Exec(ctx,
			`UPDATE context_blocks SET
				is_archived = true,
				guard_status = 'archived_dup',
				metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
					'guard_status', $2::text,
					'guard_checked_at', `+flagStampSQL+`,
					'guard_similarity', $3::float8,
					'guard_matched_id', $4::text,
					'guard_is_cross_scope', $5::bool
				),
				updated_at = now()
			WHERE id = $1`,
			blockID, result.Decision, result.Similarity, matchedIDVal, result.IsCrossScope,
		)
		if err != nil {
			return fmt.Errorf("auto-archive: %w", err)
		}
	} else {
		// Mark as checked WITHOUT archiving: needs_review, clean, OR a
		// near_duplicate under flag mode. The reached-only-in-flag near_duplicate
		// is persisted as guard_status='possible_duplicate' (§4.7 flag-only
		// persist) — is_archived stays false; the matched_id/similarity flag it.
		// $2::text on the column assignment too: PG18 rejects mixed deductions
		// (varchar from column vs text from jsonb_build_object cast).
		// The persisted thresholds are the RESOLVED per-type policy values
		// (T7) — the metadata documents what the decision was made under.
		// updated_at is NOT assigned here: recording the decision is the
		// guard's bookkeeping, not a write to the block. The column is the
		// last write as readers see it (browse order and its keyset cursor,
		// age_days, the drift pin of a staged update), and one batch would
		// otherwise stamp every block it checks with the same instant. The
		// time of the decision is metadata.guard_checked_at (flagStampSQL); the
		// readers of the review queue age a block by it (FlaggedSQL).
		statusVal := result.Decision
		if result.Decision == "near_duplicate" {
			statusVal = "possible_duplicate" // flag mode (archive took the branch above)
		}
		_, err := tx.Exec(ctx,
			`UPDATE context_blocks SET
				guard_status = $2::text,
				metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
					'guard_status', $2::text,
					'guard_checked_at', `+flagStampSQL+`,
					'guard_similarity', $3::float8,
					'guard_matched_id', $4::text,
					'guard_is_cross_scope', $5::bool,
					'guard_threshold_duplicate', $6::float8,
					'guard_threshold_review', $7::float8
				)
			WHERE id = $1`,
			blockID, statusVal, result.Similarity, matchedIDVal, result.IsCrossScope,
			result.ThresholdDuplicate, result.ThresholdReview,
		)
		if err != nil {
			return fmt.Errorf("mark checked: %w", err)
		}
	}

	return nil
}

// writeAuditLog inserts a guard audit entry into context_write_log. A matched
// neighbour is key-shared first, without waiting (neighbourKeyShare): a
// neighbour held against it defers the verdict (errVerdictDeferred).
func writeAuditLog(ctx context.Context, tx pgx.Tx, blockID string, result *GuardResult) error {
	// Get block title, category, scope for the audit log.
	var title, category, scope string
	err := tx.QueryRow(ctx,
		`SELECT title, category, scope FROM context_blocks WHERE id = $1`,
		blockID,
	).Scan(&title, &category, &scope)
	if err != nil {
		return fmt.Errorf("fetch block meta: %w", err)
	}

	matchedIDArg := interface{}(nil)
	if result.MatchedID != nil && *result.MatchedID != "" {
		matchedIDArg = *result.MatchedID
		if err := neighbourKeyShare(ctx, tx, *result.MatchedID); err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO context_write_log
			(block_id, matched_block_id, decision, similarity, scope, block_title, block_category, metadata)
		VALUES (
			$1::uuid, $2::uuid, $3, $4, $5, $6, $7,
			jsonb_build_object(
				'threshold_duplicate', $8::float8,
				'threshold_review', $9::float8
			) || CASE WHEN $10::bool THEN '{"supersedes_pair": true}'::jsonb ELSE '{}'::jsonb END
		)`,
		blockID, matchedIDArg, result.Decision, result.Similarity, scope, title, category,
		result.ThresholdDuplicate, result.ThresholdReview, result.SupersedesPair,
	)
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}

	return nil
}
