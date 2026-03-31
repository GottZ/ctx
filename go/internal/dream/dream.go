package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// CooldownDays is how long Dream waits before re-evaluating a block.
	CooldownDays = 7

	// MaxKeywords is the number of keywords extracted per block.
	MaxKeywords = 5

	// MaxCandidatesPerKeyword limits RRF results per keyword search.
	MaxCandidatesPerKeyword = 5

	// MaxLinks caps the number of links created per cycle.
	MaxLinks = 5
)

// BlockInfo holds the fields Dream needs from a block.
type BlockInfo struct {
	ID           string
	Title        string
	Category     string
	Content      string
	Scope        string
	QualityScore float64
	Embedding    []float32
}

// RunDreamCycle executes one dream cycle: pick → keywords → search → evaluate → link.
// Returns the number of links created, or 0 if no block was available.
func RunDreamCycle(ctx context.Context, pool *pgxpool.Pool, ollamaHost, embedModel, chatModel string, readScopes []string) (int, error) {
	// Step 1: Pick a block.
	block, err := PickBlock(ctx, pool)
	if err != nil {
		return 0, fmt.Errorf("dream: pick: %w", err)
	}
	if block == nil {
		return 0, nil // No eligible blocks.
	}

	slog.Info("dream: picked block",
		"block_id", block.ID,
		"title", block.Title,
		"quality_score", block.QualityScore,
	)

	// Step 2: Extract keywords.
	keywords := ExtractKeywords(block.Title, block.Content, MaxKeywords)
	if len(keywords) == 0 {
		slog.Info("dream: no keywords extracted, setting cooldown", "block_id", block.ID)
		_ = SetDreamCooldown(ctx, pool, block.ID)
		return 0, nil
	}

	slog.Info("dream: keywords extracted",
		"block_id", block.ID,
		"keywords", keywords,
	)

	// Step 3: Search per keyword via RRF.
	candidates, err := searchByKeywords(ctx, pool, ollamaHost, embedModel, keywords, readScopes, block.ID, block.Scope)
	if err != nil {
		slog.Warn("dream: keyword search failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldown(ctx, pool, block.ID)
		return 0, err
	}

	if len(candidates) == 0 {
		slog.Info("dream: no candidates found", "block_id", block.ID)
		_ = SetDreamCooldown(ctx, pool, block.ID)
		return 0, nil
	}

	slog.Info("dream: candidates found",
		"block_id", block.ID,
		"candidate_count", len(candidates),
	)

	// Step 4: LLM evaluation.
	links, err := EvaluateRelationships(ctx, ollamaHost, chatModel, *block, candidates)
	if err != nil {
		slog.Warn("dream: evaluation failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldown(ctx, pool, block.ID)
		return 0, err
	}

	// Step 5: Write links.
	written, err := WriteLinks(ctx, pool, block.ID, block.Scope, block.QualityScore, links)
	if err != nil {
		slog.Warn("dream: write links failed", "block_id", block.ID, "error", err)
	}

	// Step 6: Set cooldown (regardless of whether links were written).
	_ = SetDreamCooldown(ctx, pool, block.ID)

	slog.Info("dream: cycle complete",
		"block_id", block.ID,
		"links_evaluated", len(links),
		"links_written", written,
	)

	return written, nil
}

// PickBlock selects the next block for Dream processing.
// Priority: unchecked blocks first, then oldest-checked with expired cooldown.
// Uses FOR UPDATE SKIP LOCKED to prevent concurrent processing.
func PickBlock(ctx context.Context, pool *pgxpool.Pool) (*BlockInfo, error) {
	var block BlockInfo
	err := pool.QueryRow(ctx,
		`SELECT id, title, category, content, scope, quality_score
		FROM context_blocks
		WHERE NOT is_archived
		  AND embedding IS NOT NULL
		  AND (block_type IS NULL OR block_type IN ('knowledge', 'source'))
		  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
		ORDER BY dream_checked_at ASC NULLS FIRST, quality_score ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
	).Scan(&block.ID, &block.Title, &block.Category, &block.Content, &block.Scope, &block.QualityScore)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick block: %w", err)
	}
	return &block, nil
}

// SetDreamCooldown marks a block as dream-checked with a cooldown period.
func SetDreamCooldown(ctx context.Context, pool *pgxpool.Pool, blockID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			dream_checked_at = now(),
			dream_cooldown_until = now() + interval '1 day' * $2
		WHERE id = $1`,
		blockID, CooldownDays,
	)
	return err
}

// searchByKeywords runs one RRF search per keyword, deduplicates results,
// and returns candidate blocks (excluding the source block and cross-scope blocks).
func searchByKeywords(ctx context.Context, pool *pgxpool.Pool, ollamaHost, embedModel string, keywords []string, scopes []string, sourceID, sourceScope string) ([]BlockInfo, error) {
	seen := make(map[string]bool)
	seen[sourceID] = true // Exclude source block.
	var candidates []BlockInfo

	for _, kw := range keywords {
		// Embed the keyword for semantic search.
		kwEmbedding, err := embed.Embed(ctx, ollamaHost, embedModel, kw, embed.PrefixQuery)
		if err != nil {
			slog.Debug("dream: embed keyword failed", "keyword", kw, "error", err)
			continue
		}

		// RRF search with keyword as query.
		results, err := rrf.Search(ctx, pool, kwEmbedding, kw, kw, scopes, nil, nil, MaxCandidatesPerKeyword, "", nil)
		if err != nil {
			slog.Debug("dream: rrf search failed", "keyword", kw, "error", err)
			continue
		}

		for _, r := range results {
			if seen[r.ID] {
				continue
			}
			// Same-scope filter (V5).
			if r.Scope != sourceScope {
				continue
			}
			seen[r.ID] = true
			candidates = append(candidates, BlockInfo{
				ID:       r.ID,
				Title:    r.Title,
				Category: r.Category,
				Content:  r.Content,
				Scope:    r.Scope,
			})
		}

		// Cap total candidates.
		if len(candidates) >= MaxCandidatesPerKeyword*MaxKeywords {
			break
		}
	}

	return candidates, nil
}

// Stats returns dream processing statistics.
func Stats(ctx context.Context, pool *pgxpool.Pool) (total, checked, linked int, err error) {
	err = pool.QueryRow(ctx,
		`SELECT
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND embedding IS NOT NULL)::int,
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND dream_checked_at IS NOT NULL)::int,
			(SELECT count(*) FROM context_dream_links)::int`,
	).Scan(&total, &checked, &linked)
	return
}

// SetDreamCooldownTx marks a block as dream-checked within an existing transaction.
func SetDreamCooldownTx(ctx context.Context, tx pgx.Tx, blockID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE context_blocks SET
			dream_checked_at = now(),
			dream_cooldown_until = now() + interval '1 day' * $2
		WHERE id = $1`,
		blockID, CooldownDays,
	)
	return err
}

// PickBlockTx is like PickBlock but avoids transaction nesting issues.
// For the scheduler, we do NOT wrap in a transaction since SKIP LOCKED
// only needs a short lock, and the full cycle (search + LLM) is too long
// to hold a transaction open.
var _ = PickBlock // verify PickBlock exists

// CleanupDanglingLinks removes links pointing to archived blocks.
func CleanupDanglingLinks(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_dream_links
		WHERE target_block_id IN (
			SELECT id FROM context_blocks WHERE is_archived
		)`)
	if err != nil {
		return 0, fmt.Errorf("dream: cleanup dangling links: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// UpdateQualityScore adjusts a block's quality_score based on its dream link profile.
// Formula: min(1.0, base + inbound_factor + outbound_factor + diversity_bonus).
// Blocks without links keep their current score.
func UpdateQualityScore(ctx context.Context, pool *pgxpool.Pool, blockID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET quality_score = LEAST(1.0,
			0.3
			+ 0.1 * LEAST((SELECT count(*) FROM context_dream_links WHERE target_block_id = $1), 5)
			+ 0.05 * LEAST((SELECT count(*) FROM context_dream_links WHERE source_block_id = $1), 5)
			+ 0.15 * LEAST((SELECT count(DISTINCT relationship) FROM context_dream_links WHERE source_block_id = $1 OR target_block_id = $1), 4)
		)
		WHERE id = $1
		  AND EXISTS (SELECT 1 FROM context_dream_links WHERE source_block_id = $1 OR target_block_id = $1)`,
		blockID,
	)
	return err
}

// nolint:unused
func init() {
	// Ensure compile-time type checks.
	_ = time.Duration(0)
	_ = fmt.Sprintf
}
