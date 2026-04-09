package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// CooldownActiveDays is how long Dream waits for blocks that produced links (re-check sooner).
	CooldownActiveDays = 3
	// CooldownInertDays is how long Dream waits for blocks that produced no links.
	CooldownInertDays = 14

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
	UpdatedAt    time.Time
	CreatedAt    time.Time
}

// CycleTimeout is the maximum duration for a single dream cycle.
// Prevents cascading Ollama timeouts from blocking the scheduler.
// 180s to accommodate large model cold-starts with Ollama model swapping.
const CycleTimeout = 180 * time.Second

// RunDreamCycle executes one dream cycle: pick → keywords → search → evaluate → link.
// Returns the number of links created, or 0 if no block was available.
func RunDreamCycle(ctx context.Context, pool *pgxpool.Pool, embedHost, embedAPIKey, embedModel string, embedNumCtx int, chatHost, chatAPIKey, chatModel string, think *bool, opts llm.Options, readScopes []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, CycleTimeout)
	defer cancel()
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

	// Step 1b: Validate temporal dimensions (deterministic + LLM).
	if err := ValidateTemporal(ctx, pool, chatHost, chatAPIKey, chatModel, think, opts, block); err != nil {
		slog.Warn("dream: temporal validation failed (non-fatal)", "block_id", block.ID, "error", err)
	}

	// Step 2: Extract keywords.
	keywords := ExtractKeywords(block.Title, block.Content, MaxKeywords)
	if len(keywords) == 0 {
		slog.Info("dream: no keywords extracted, setting cooldown", "block_id", block.ID)
		_ = SetDreamCooldown(ctx, pool, block.ID, CooldownInertDays)
		return 0, nil
	}

	slog.Info("dream: keywords extracted",
		"block_id", block.ID,
		"keywords", keywords,
	)

	// Step 3: Search per keyword via RRF.
	candidates, err := searchByKeywords(ctx, pool, embedHost, embedAPIKey, embedModel, embedNumCtx, keywords, readScopes, block.ID, block.Scope)
	if err != nil {
		slog.Warn("dream: keyword search failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldown(ctx, pool, block.ID, CooldownInertDays)
		return 0, err
	}

	if len(candidates) == 0 {
		slog.Info("dream: no candidates found", "block_id", block.ID)
		_ = SetDreamCooldown(ctx, pool, block.ID, CooldownInertDays)
		return 0, nil
	}

	slog.Info("dream: candidates found",
		"block_id", block.ID,
		"candidate_count", len(candidates),
	)

	// Step 4: LLM evaluation.
	links, err := EvaluateRelationships(ctx, chatHost, chatAPIKey, chatModel, think, opts, *block, candidates)
	if err != nil {
		slog.Warn("dream: evaluation failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldown(ctx, pool, block.ID, 1) // Short retry — LLM errors are transient
		return 0, err
	}

	// Step 5: Write links.
	written, err := WriteLinks(ctx, pool, block.ID, block.Scope, block.QualityScore, links)
	if err != nil {
		slog.Warn("dream: write links failed", "block_id", block.ID, "error", err)
	}

	// Step 6: Update quality scores for source and linked blocks.
	if written > 0 {
		if err := UpdateQualityScore(ctx, pool, block.ID); err != nil {
			slog.Warn("dream: update quality score failed", "block_id", block.ID, "error", err)
		}
		for _, link := range links {
			if err := UpdateQualityScore(ctx, pool, link.TargetID); err != nil {
				slog.Debug("dream: update target quality score failed", "target_id", link.TargetID, "error", err)
			}
		}
	}

	// Step 7: Set adaptive cooldown — active blocks re-checked sooner.
	cooldownDays := CooldownInertDays
	if written > 0 {
		cooldownDays = CooldownActiveDays
	}
	_ = SetDreamCooldown(ctx, pool, block.ID, cooldownDays)

	// Step 8: Promote eligible blocks to canonical.
	// Targets of newly written links may now qualify (quality boosted in step 6).
	if written > 0 {
		for _, link := range links {
			if promoted, err := PromoteToCanonical(ctx, pool, link.TargetID); err != nil {
				slog.Debug("dream: promote check failed", "block_id", link.TargetID, "error", err)
			} else if promoted {
				slog.Info("dream: promoted to canonical", "block_id", link.TargetID)
			}
		}
	}

	slog.Info("dream: cycle complete",
		"block_id", block.ID,
		"links_evaluated", len(links),
		"links_written", written,
	)

	return written, nil
}

// PromoteToCanonical upgrades a block to 'canonical' if it meets all criteria:
// quality_score >= 0.8, no inbound supersedes, block_type = 'knowledge' or NULL.
// Returns true if the block was promoted.
func PromoteToCanonical(ctx context.Context, pool *pgxpool.Pool, blockID string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE context_blocks SET block_type = 'canonical'
		WHERE id = $1
		  AND NOT is_archived
		  AND quality_score >= 0.8
		  AND (block_type IS NULL OR block_type = 'knowledge')
		  AND NOT EXISTS (
			SELECT 1 FROM context_dream_links
			WHERE source_block_id = $1 AND relationship = 'supersedes'
		  )`,
		blockID,
	)
	if err != nil {
		return false, fmt.Errorf("dream: promote: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// PickBlock selects the next block for Dream processing.
// Priority: unchecked blocks first, then oldest-checked with expired cooldown.
// Uses FOR UPDATE SKIP LOCKED to prevent concurrent processing.
func PickBlock(ctx context.Context, pool *pgxpool.Pool) (*BlockInfo, error) {
	var block BlockInfo
	err := pool.QueryRow(ctx,
		`SELECT id, title, category, content, scope, quality_score, updated_at, created_at
		FROM context_blocks
		WHERE NOT is_archived
		  AND embedding IS NOT NULL
		  AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
		  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
		ORDER BY dream_checked_at ASC NULLS FIRST, quality_score ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
	).Scan(&block.ID, &block.Title, &block.Category, &block.Content, &block.Scope, &block.QualityScore, &block.UpdatedAt, &block.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick block: %w", err)
	}
	return &block, nil
}

// SetDreamCooldown marks a block as dream-checked with a cooldown period.
func SetDreamCooldown(ctx context.Context, pool *pgxpool.Pool, blockID string, days int) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			dream_checked_at = now(),
			dream_cooldown_until = now() + interval '1 day' * $2
		WHERE id = $1`,
		blockID, days,
	)
	return err
}

// searchByKeywords runs one RRF search per keyword, deduplicates results,
// and returns candidate blocks (excluding the source block and cross-scope blocks).
func searchByKeywords(ctx context.Context, pool *pgxpool.Pool, embedHost, embedAPIKey, embedModel string, embedNumCtx int, keywords []string, scopes []string, sourceID, sourceScope string) ([]BlockInfo, error) {
	seen := make(map[string]bool)
	seen[sourceID] = true // Exclude source block.
	var candidates []BlockInfo
	embedFailures := 0

	for _, kw := range keywords {
		// Embed the keyword for semantic search.
		kwEmbedding, err := embed.Embed(ctx, embedHost, embedAPIKey, embedModel, kw, embed.PrefixQuery, embedNumCtx)
		if err != nil {
			embedFailures++
			slog.Debug("dream: embed keyword failed", "keyword", kw, "error", err)
			// Fail fast if majority of embeddings fail (Ollama likely down).
			if embedFailures > len(keywords)/2 {
				return nil, fmt.Errorf("dream: too many embedding failures (%d/%d)", embedFailures, len(keywords))
			}
			continue
		}

		// RRF search with keyword as query.
		results, err := rrf.Search(ctx, pool, kwEmbedding, kw, kw, scopes, nil, nil, MaxCandidatesPerKeyword, "", "")
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
				ID:        r.ID,
				Title:     r.Title,
				Category:  r.Category,
				Content:   r.Content,
				Scope:     r.Scope,
				UpdatedAt: r.UpdatedAt,
			})
		}

		// Cap total candidates.
		if len(candidates) >= MaxCandidatesPerKeyword*MaxKeywords {
			break
		}
	}

	return candidates, nil
}

// Stats returns dream processing statistics, filtered by scope.
func Stats(ctx context.Context, pool *pgxpool.Pool, scopes []string) (total, checked, linked int, err error) {
	err = pool.QueryRow(ctx,
		`SELECT
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND embedding IS NOT NULL AND scope = ANY($1))::int,
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND dream_checked_at IS NOT NULL AND scope = ANY($1))::int,
			(SELECT count(*) FROM context_dream_links WHERE scope = ANY($1))::int`,
		scopes,
	).Scan(&total, &checked, &linked)
	return
}


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
// Only counts links with confidence >= 0.5 to prevent noise from inflating scores.
// Blocks without qualifying links keep their current score.
func UpdateQualityScore(ctx context.Context, pool *pgxpool.Pool, blockID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET quality_score = LEAST(1.0,
			0.3
			+ 0.1 * LEAST((SELECT count(*) FROM context_dream_links WHERE target_block_id = $1 AND confidence >= 0.5), 5)
			+ 0.05 * LEAST((SELECT count(*) FROM context_dream_links WHERE source_block_id = $1 AND confidence >= 0.5), 5)
			+ 0.15 * LEAST((SELECT count(DISTINCT relationship) FROM context_dream_links WHERE (source_block_id = $1 OR target_block_id = $1) AND confidence >= 0.5), 4)
		)
		WHERE id = $1
		  AND EXISTS (SELECT 1 FROM context_dream_links WHERE (source_block_id = $1 OR target_block_id = $1) AND confidence >= 0.5)`,
		blockID,
	)
	return err
}

// SupersedesMap returns a map of block_id → []superseded_by_ids for the given block IDs.
// A block can be superseded by multiple newer blocks, hence the slice value.
// Only returns links with confidence >= 0.7 to prevent false-positive filtering.
// Used by the query handler to enrich responses and for filterSuperseded.
func SupersedesMap(ctx context.Context, pool *pgxpool.Pool, blockIDs []string) (map[string][]string, error) {
	if len(blockIDs) == 0 {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT source_block_id::text, target_block_id::text
		FROM context_dream_links
		WHERE relationship = 'supersedes'
		  AND confidence >= 0.7
		  AND source_block_id = ANY($1::uuid[])`,
		blockIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("dream: supersedes map: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var sourceID, targetID string
		if err := rows.Scan(&sourceID, &targetID); err != nil {
			return nil, fmt.Errorf("dream: scan supersedes: %w", err)
		}
		result[sourceID] = append(result[sourceID], targetID)
	}
	return result, rows.Err()
}
