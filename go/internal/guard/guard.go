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
}

// pendingBlock is one pick-query row: the block plus its policy type (the
// per-type threshold key).
type pendingBlock struct {
	id       string
	typeName string
}

// guardPendingWhere is THE single guard-batch pending predicate (WF T7,
// design/01 §7-T7): the pick query and both count subqueries in the state
// update consume this one fragment instead of carrying three copies.
// typesParam binds the GuardCheckTypes allowlist (guard.check=true types).
// The first three conjuncts mirror idx_guard_pending (M074) byte-for-byte —
// only pending rows are IN that partial index; category != 'index'
// (topic-map mechanism rest, §4.2: agent briefings share the system-meta
// type but ARE checked — dropping the category rest would silently
// guard-check the topic-map), the lifecycle gate and the type allowlist
// filter on the index result. typesParam is a code-owned bind placeholder
// at every call site — never user input.
func guardPendingWhere(typesParam string) string {
	return `NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'
		  AND type_name = ANY(` + typesParam + `::text[])`
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
// simply yields zero picks. The singleton state counts stay GLOBAL and count
// with the same allowlist (telemetry, not policy — design/01 §7-T12 note).
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

	// Wrap entire batch in a transaction so FOR UPDATE SKIP LOCKED row locks
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
		// FOR UPDATE SKIP LOCKED prevents concurrent guard runs from processing
		// the same blocks; row locks are held for the duration of the transaction.
		rows, err := tx.Query(ctx,
			`SELECT id, type_name FROM context_blocks
			WHERE `+guardPendingWhere("$1")+`
			ORDER BY created_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED`,
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
			// No pending blocks. Clear dirty state.
			_, _ = tx.Exec(ctx,
				`UPDATE context_guard_state SET
					last_guard_at = now(),
					dirty_since = NULL,
					pending_count = 0
				WHERE id = true`,
			)
			// Nothing to process: the state update above is all this batch
			// writes, and the helper persists it — a nil return closes the
			// transaction exactly where the explicit call used to sit.
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

		// Update guard state — both count subqueries reuse the SAME pending
		// fragment as the pick ($1 = the type allowlist).
		_, err = tx.Exec(ctx,
			`UPDATE context_guard_state SET
				last_guard_at = now(),
				dirty_since = CASE
					WHEN (SELECT count(*) FROM context_blocks
						WHERE `+guardPendingWhere("$1")+`
					) = 0 THEN NULL
					ELSE dirty_since
				END,
				pending_count = (SELECT count(*)::int FROM context_blocks
					WHERE `+guardPendingWhere("$1")+`
				)
			WHERE id = true`,
			checkTypes,
		)
		if err != nil {
			slog.Error("guard: update state failed", "error", err)
		}
		return nil
	})

	if err != nil {
		return processed, err
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
// stays usable for the next block.
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

	err := tx.QueryRow(ctx,
		`SELECT decision, top_similarity, matched_id::text, matched_title, matched_scope, is_cross_scope
		FROM ctx_guard_check($1::uuid, $2::real, $3::real, $4::text[], $5::boolean)`,
		blockID, thresholdDuplicate, thresholdReview, candidateTypes, sameScopeOnly,
	).Scan(&decision, &topSimilarity, &matchedID, &matchedTitle, &matchedScope, &isCrossScope)
	if err != nil {
		return nil, fmt.Errorf("guard: ctx_guard_check: %w", err)
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
	}, nil
}

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

	if result.Decision == "near_duplicate" && mode != blocktype.GuardModeFlag {
		// Auto-archive near duplicates. Bestand for every mode EXCEPT flag —
		// fail-safe to archive so an empty/unset mode (e.g. a policy built
		// outside DecodePolicy) never silently degrades to flag-only.
		// Explicit casts inside jsonb_build_object: VARIADIC "any" can't infer
		// types under pgx extended protocol on PG18 → 42P08 at PREPARE time.
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
				),
				updated_at = now()
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
			)
		)`,
		blockID, matchedIDArg, result.Decision, result.Similarity, scope, title, category,
		result.ThresholdDuplicate, result.ThresholdReview,
	)
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}

	return nil
}
