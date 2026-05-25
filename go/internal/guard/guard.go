// Package guard implements the write guard that detects near-duplicate blocks.
// Uses HNSW similarity check with thresholds: >=0.98 auto-archive, 0.92-0.98 flag for review.
package guard

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GuardResult holds the outcome of processing a single block.
type GuardResult struct {
	BlockID       string
	Decision      string
	Similarity    float64
	MatchedID     *string
	MatchedTitle  *string
	IsCrossScope  bool
}

// RunGuardBatch processes pending blocks through the duplicate detection guard.
// Uses the ctx_guard_check PG function for each block.
// Returns the number of blocks processed and any error.
func RunGuardBatch(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	// Wrap entire batch in a transaction so FOR UPDATE SKIP LOCKED row locks
	// are held until all blocks are processed, preventing race conditions.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("guard: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	// Fetch pending blocks (unchecked, not archived, not index, has embedding).
	// FOR UPDATE SKIP LOCKED prevents concurrent guard runs from processing the same blocks.
	// Row locks are held for the duration of the transaction.
	rows, err := tx.Query(ctx,
		`SELECT id FROM context_blocks
		WHERE NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND category != 'index'
		  AND embedding IS NOT NULL
		  AND (block_type IS NULL OR block_type IN ('knowledge', 'source'))
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`,
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("guard: fetch pending: %w", err)
	}

	var blockIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("guard: scan block id: %w", err)
		}
		blockIDs = append(blockIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("guard: rows error: %w", err)
	}

	if len(blockIDs) == 0 {
		// No pending blocks. Clear dirty state.
		_, _ = tx.Exec(ctx,
			`UPDATE context_guard_state SET
				last_guard_at = now(),
				dirty_since = NULL,
				pending_count = 0
			WHERE id = true`,
		)
		return 0, tx.Commit(ctx)
	}

	processed := 0
	for _, blockID := range blockIDs {
		// Check for context cancellation (demand interruption).
		select {
		case <-ctx.Done():
			slog.Info("guard: interrupted by context cancellation", "processed", processed)
			return processed, ctx.Err()
		default:
		}

		result, err := checkBlock(ctx, tx, blockID)
		if err != nil {
			slog.Error("guard: check block failed", "block_id", blockID, "error", err)
			continue
		}

		if err := applyDecision(ctx, tx, blockID, result); err != nil {
			slog.Error("guard: apply decision failed", "block_id", blockID, "error", err)
			continue
		}

		if err := writeAuditLog(ctx, tx, blockID, result); err != nil {
			slog.Error("guard: audit log failed", "block_id", blockID, "error", err)
		}

		processed++
		slog.Debug("guard: block processed",
			"block_id", blockID,
			"decision", result.Decision,
			"similarity", result.Similarity,
		)
	}

	// Update guard state.
	_, err = tx.Exec(ctx,
		`UPDATE context_guard_state SET
			last_guard_at = now(),
			dirty_since = CASE
				WHEN (SELECT count(*) FROM context_blocks
					WHERE NOT is_archived
					  AND (metadata->>'guard_checked_at') IS NULL
					  AND category != 'index'
					  AND embedding IS NOT NULL
					  AND (block_type IS NULL OR block_type IN ('knowledge', 'source'))
				) = 0 THEN NULL
				ELSE dirty_since
			END,
			pending_count = (SELECT count(*)::int FROM context_blocks
				WHERE NOT is_archived
				  AND (metadata->>'guard_checked_at') IS NULL
				  AND category != 'index'
				  AND embedding IS NOT NULL
				  AND (block_type IS NULL OR block_type IN ('knowledge', 'source'))
			)
		WHERE id = true`,
	)
	if err != nil {
		slog.Error("guard: update state failed", "error", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return processed, fmt.Errorf("guard: commit tx: %w", err)
	}

	slog.Info("guard: batch complete", "processed", processed, "total_pending", len(blockIDs))
	return processed, nil
}

// checkBlock calls the ctx_guard_check PG function for a single block.
func checkBlock(ctx context.Context, tx pgx.Tx, blockID string) (*GuardResult, error) {
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
		FROM ctx_guard_check($1::uuid)`,
		blockID,
	).Scan(&decision, &topSimilarity, &matchedID, &matchedTitle, &matchedScope, &isCrossScope)
	if err != nil {
		return nil, fmt.Errorf("guard: ctx_guard_check: %w", err)
	}

	return &GuardResult{
		BlockID:      blockID,
		Decision:     decision,
		Similarity:   topSimilarity,
		MatchedID:    matchedID,
		MatchedTitle: matchedTitle,
		IsCrossScope: isCrossScope,
	}, nil
}

// applyDecision updates the block based on the guard decision.
func applyDecision(ctx context.Context, tx pgx.Tx, blockID string, result *GuardResult) error {
	checkedAt := time.Now().UTC().Format(time.RFC3339)

	matchedIDVal := ""
	if result.MatchedID != nil {
		matchedIDVal = *result.MatchedID
	}

	if result.Decision == "near_duplicate" {
		// Auto-archive near duplicates.
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
		// Mark as checked (needs_review or clean).
		// $2::text on the column assignment too: PG18 rejects mixed deductions
		// (varchar from column vs text from jsonb_build_object cast).
		_, err := tx.Exec(ctx,
			`UPDATE context_blocks SET
				guard_status = $2::text,
				metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
					'guard_status', $2::text,
					'guard_checked_at', $3::text,
					'guard_similarity', $4::float8,
					'guard_matched_id', $5::text,
					'guard_is_cross_scope', $6::bool,
					'guard_threshold_duplicate', 0.98,
					'guard_threshold_review', 0.92
				),
				updated_at = now()
			WHERE id = $1`,
			blockID, result.Decision, checkedAt, result.Similarity, matchedIDVal, result.IsCrossScope,
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
				'threshold_duplicate', 0.98,
				'threshold_review', 0.92
			)
		)`,
		blockID, matchedIDArg, result.Decision, result.Similarity, scope, title, category,
	)
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}

	return nil
}
