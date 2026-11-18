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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
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
}

// guardPendingWhere is THE single guard-batch pending predicate (WF T7,
// design/01 §7-T7): the pick query and PendingStats (manage guard-stats,
// issue #341) consume this one fragment instead of carrying two copies.
// typesParam binds the GuardCheckTypes allowlist (guard.check=true types).
// The first three conjuncts mirror idx_guard_pending (M074) byte-for-byte —
// only pending rows are IN that partial index; NOT embedding_stale (issue
// #198, part B: a title or content change keeps the previous vector until the
// backfill writes the new one, and the change also drops guard_checked_at, so
// without it the batch would check the new text by the old text's vector and
// write the verdict as final), category != 'index' (topic-map mechanism rest,
// §4.2: agent briefings share the system-meta type but ARE checked — dropping
// the category rest would silently guard-check the topic-map), the lifecycle
// gate and the type allowlist filter on the index result. A stale block waits
// in the index until its new vector lands, as a block without a vector waited
// outside it before. typesParam is a code-owned bind placeholder at every call
// site — never user input.
func guardPendingWhere(typesParam string) string {
	return `NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND NOT embedding_stale
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'
		  AND type_name = ANY(` + typesParam + `::text[])`
}

// lastGuardAtSQL stamps the end of a committed guard run. It is the only
// write of context_guard_state left (issue #341, migration 181).
const lastGuardAtSQL = `UPDATE context_guard_state SET last_guard_at = now() WHERE id = true`

// pendingStatsSQL counts what the pick would take, within the read scopes in
// $1 ($2 = the GuardCheckTypes allowlist), and the oldest updated_at among
// those blocks — one pass over idx_guard_pending (its partial predicate has
// no parameter, so a generic plan can use it too); scope = ANY($1) filters
// the index result or joins idx_context_scope in a BitmapAnd. The cost grows
// with the entries of idx_guard_pending, not with the corpus and not only
// with the waiting blocks: the index predicate is the first three conjuncts
// of guardPendingWhere, so it also holds unchecked blocks with a vector that
// the pick filters out and never takes out of the index (a type with
// guard.check = false, category index, another lifecycle, a stale vector)
// — docs/operations.md, "Migration 181".
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
}, set *blocktype.Set, limit int) (int, error) {
	if set == nil {
		return 0, fmt.Errorf("guard: nil block-type policy set (registry not wired?)")
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
	)
	err := pgxdb.Write(ctx, pool, pgxdb.Stages{
		Begin:  "guard: begin tx",
		Commit: "guard: commit tx",
	}, func(tx pgx.Tx) error {
		// Fetch pending blocks (the shared guardPendingWhere fragment: unchecked,
		// not archived, not index, has embedding, knowledge lifecycle, policy
		// type allowlist). ORDER BY created_at ASC rides idx_guard_pending (M074).
		// SKIP LOCKED prevents concurrent guard runs from processing the same
		// blocks; row locks are held for the duration of the transaction.
		//
		// FOR NO KEY UPDATE, not FOR UPDATE (issue #359): it is the lock the
		// verdict's own UPDATE takes (no key column changes), and it does not
		// conflict with the key-share lock a foreign-key check takes. Another
		// run's audit row names one of these blocks as its matched neighbour
		// (matched_block_id); under FOR UPDATE that insert waited for this
		// whole batch, and two runs holding each other's neighbours waited
		// for each other until PostgreSQL aborted one insert with 40P01.
		rows, err := tx.Query(ctx,
			`SELECT id, type_name FROM context_blocks
			WHERE `+guardPendingWhere("$1")+`
			ORDER BY created_at ASC
			LIMIT $2
			FOR NO KEY UPDATE SKIP LOCKED`,
			checkTypes, limit,
		)
		if err != nil {
			return fmt.Errorf("guard: fetch pending: %w", err)
		}

		for rows.Next() {
			var b pendingBlock
			if err := rows.Scan(&b.id, &b.typeName); err != nil {
				rows.Close()
				return fmt.Errorf("guard: scan block id: %w", err)
			}
			blocks = append(blocks, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("guard: rows error: %w", err)
		}

		if len(blocks) == 0 {
			// Nothing to process: a nil return closes the transaction.
			return nil
		}

		for _, block := range blocks {
			// Check for context cancellation (demand interruption).
			select {
			case <-ctx.Done():
				slog.Info("guard: interrupted by context cancellation", "processed", processed)
				return ctx.Err()
			default:
			}

			if processBlock(ctx, tx, block, set) {
				processed++
			}
		}
		return nil
	})

	if err != nil {
		return processed, err
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
	return processed, nil
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

// processBlock runs checkBlock + applyDecision + writeAuditLog for a single
// block inside its own SAVEPOINT. Returns true iff all three steps succeeded.
// On any error the savepoint is rolled back so the surrounding transaction
// stays usable for the next block. A verdict that would archive the block
// first claims both of its blocks (claimArchivePair); an unclaimed verdict
// rolls back the same way and leaves the block pending, without an error.
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
func processBlock(ctx context.Context, tx pgx.Tx, block pendingBlock, set *blocktype.Set) bool {
	blockID := block.id
	sp := savepointName(blockID)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
		slog.Error("guard: savepoint create failed", "block_id", blockID, "error", err)
		return false
	}

	dup, review := set.GuardThresholds(block.typeName)
	sameScopeOnly := set.GuardSameScopeOnly(block.typeName)
	mode := set.GuardMode(block.typeName)
	result, err := checkBlock(ctx, tx, blockID, dup, review, set.GuardCandidateTypes(), sameScopeOnly)
	if err != nil {
		slog.Error("guard: check block failed", "block_id", blockID, "error", err)
		rollbackToSavepoint(ctx, tx, sp)
		return false
	}

	if archives(result.Decision, mode) && result.MatchedID != nil {
		claimed, err := claimArchivePair(ctx, tx, blockID, *result.MatchedID)
		if err != nil {
			slog.Error("guard: claim archive failed", "block_id", blockID, "error", err)
			rollbackToSavepoint(ctx, tx, sp)
			return false
		}
		if !claimed {
			// Not an error: the block stays pending and the next run checks
			// it against what the concurrent decision left.
			slog.Info("guard: archive verdict meets a concurrent one, block left for the next run",
				"block_id", blockID, "matched_id", *result.MatchedID)
			rollbackToSavepoint(ctx, tx, sp)
			return false
		}
	}

	if err := applyDecision(ctx, tx, blockID, result, mode); err != nil {
		slog.Error("guard: apply decision failed", "block_id", blockID, "error", err)
		rollbackToSavepoint(ctx, tx, sp)
		return false
	}

	if err := writeAuditLog(ctx, tx, blockID, result); err != nil {
		slog.Error("guard: audit log failed", "block_id", blockID, "error", err)
		rollbackToSavepoint(ctx, tx, sp)
		return false
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
	return true
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
func checkBlock(ctx context.Context, tx pgx.Tx, blockID string, thresholdDuplicate, thresholdReview float64, candidateTypes []string, sameScopeOnly bool) (*GuardResult, error) {
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
	err := tx.QueryRow(ctx,
		`SELECT g.decision, g.top_similarity, g.matched_id::text, g.matched_title, g.matched_scope, g.is_cross_scope,
		        g.matched_id IS NOT NULL AND EXISTS (
		          SELECT 1 FROM context_dream_links dl
		           WHERE dl.relationship = 'supersedes' AND dl.pinned
		             AND ((dl.source_block_id = $1::uuid AND dl.target_block_id = g.matched_id)
		               OR (dl.source_block_id = g.matched_id AND dl.target_block_id = $1::uuid)))
		FROM ctx_guard_check($1::uuid, $2::real, $3::real, $4::text[], $5::boolean) g`,
		blockID, thresholdDuplicate, thresholdReview, candidateTypes, sameScopeOnly,
	).Scan(&decision, &topSimilarity, &matchedID, &matchedTitle, &matchedScope, &isCrossScope, &supersedesPair)
	if err != nil {
		return nil, fmt.Errorf("guard: ctx_guard_check: %w", err)
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

// FlaggedAtSQL is the SQL expression for the time the guard recorded its
// decision on a context_blocks row: metadata.guard_checked_at, which
// applyDecision writes as RFC 3339. It is what a reader of the review queue
// ages a flagged block by, since recording the decision does not move
// updated_at.
//
// The value is metadata, and an update that leaves the content alone stores
// the caller's metadata document as sent, so the expression takes the key only
// when it can be a flag time of this row and falls back to updated_at
// otherwise:
//
//   - absent or not a timestamp (a row older than the key, a repair
//     migration, a hand edit). pg_input_is_valid, not a bare cast: one
//     unreadable value would otherwise fail the whole aggregate it sits in;
//   - 'infinity' and '-infinity', which are valid input but do not scan into
//     a time.Time, so one such row would fail the read for every scope;
//   - a time before the row existed or after now(): the minimum over all
//     scopes must not be something one row can set to an arbitrary date.
//     created_at is cut to the second because guard_checked_at has second
//     precision.
//
// Nested CASE, because only CASE guarantees that the cast is not evaluated
// before the validity test. Unqualified column names, so it is used in
// queries that read context_blocks without an alias.
const FlaggedAtSQL = `CASE WHEN pg_input_is_valid(metadata->>'guard_checked_at', 'timestamptz')
		THEN CASE WHEN isfinite((metadata->>'guard_checked_at')::timestamptz)
				AND (metadata->>'guard_checked_at')::timestamptz >= date_trunc('second', created_at)
				AND (metadata->>'guard_checked_at')::timestamptz <= now()
			THEN (metadata->>'guard_checked_at')::timestamptz
			ELSE updated_at END
		ELSE updated_at END`

// applyDecision updates the block based on the guard decision and the per-type
// persist mode (design/02 §4.7). mode == GuardModeArchive is the bestand: a
// near_duplicate is auto-archived. mode == GuardModeFlag (issues) NEVER
// archives — a near_duplicate is persisted as a possible_duplicate flag
// (guard_status='possible_duplicate' + guard_matched_id + similarity), so a
// duplicate issue is surfaced (guard-list / later issue-list) and never
// silently removed. needs_review and clean are mode-independent.
func applyDecision(ctx context.Context, tx pgx.Tx, blockID string, result *GuardResult, mode string) error {
	checkedAt := time.Now().UTC().Format(time.RFC3339)

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
					'guard_checked_at', $3::text,
					'guard_similarity', $4::float8,
					'guard_matched_id', $5::text,
					'guard_is_cross_scope', $6::bool
				),
				updated_at = now()
			WHERE id = $1`,
			blockID, result.Decision, checkedAt, result.Similarity, matchedIDVal, result.IsCrossScope,
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
		// time of the decision is metadata.guard_checked_at; the readers of
		// the review queue age a block by it (FlaggedAtSQL).
		statusVal := result.Decision
		if result.Decision == "near_duplicate" {
			statusVal = "possible_duplicate" // flag mode (archive took the branch above)
		}
		_, err := tx.Exec(ctx,
			`UPDATE context_blocks SET
				guard_status = $2::text,
				metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
					'guard_status', $2::text,
					'guard_checked_at', $3::text,
					'guard_similarity', $4::float8,
					'guard_matched_id', $5::text,
					'guard_is_cross_scope', $6::bool,
					'guard_threshold_duplicate', $7::float8,
					'guard_threshold_review', $8::float8
				)
			WHERE id = $1`,
			blockID, statusVal, checkedAt, result.Similarity, matchedIDVal, result.IsCrossScope,
			result.ThresholdDuplicate, result.ThresholdReview,
		)
		if err != nil {
			return fmt.Errorf("mark checked: %w", err)
		}
	}

	return nil
}

// writeAuditLog inserts a guard audit entry into context_write_log.
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
