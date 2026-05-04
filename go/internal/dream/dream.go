package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Version marks the Dream pipeline generation. Persisted with every link.
	// v1 = Pre-Reset (weighted-gate era, Pre-Reset-Audit 2026-04-20: 53% CORRECT at raw>=0.7).
	// v2 = Post-Reset Ideallinie: raw_confidence column, per-type raw thresholds
	//      (factual>=0.9), causal-temporal-check, topic-map-exclude, ApplySupersedes on raw.
	// v3 = Session 24 (2026-04-23): qwen3.6:27b + V5 prompt (topical-as-fallback,
	//      supersedes VERY RARE, causal accepts decision→implementation),
	//      factual threshold 0.9→0.7. Benchmark: 62.3% accuracy on stable gold (+19.7pp).
	Version = 3

	// CooldownActiveDays is how long Dream waits for blocks that produced links (re-check sooner).
	CooldownActiveDays = 3
	// CooldownInertDays is how long Dream waits for blocks that produced no links.
	CooldownInertDays = 14
	// CooldownTransientMinutes is for transient system errors (LLM timeout, Ollama restart).
	// Short enough that a recovered GPU continues the pass promptly, long enough that a
	// persistent outage doesn't spin-retry every 20s.
	CooldownTransientMinutes = 5

	// MaxKeywords is the number of keywords extracted per block.
	MaxKeywords = 5

	// MaxCandidatesPerKeyword limits RRF results per keyword search.
	MaxCandidatesPerKeyword = 5

	// MaxLinksPerCycle caps the number of links created per cycle.
	// Enforced in EvaluateRelationships after confidence filtering, sorted by
	// confidence DESC with type-diversity tie-break.
	MaxLinksPerCycle = 5
)

// BlockInfo holds the fields Dream needs from a block.
type BlockInfo struct {
	ID                       string
	Title                    string
	Category                 string
	Content                  string
	Scope                    string
	QualityScore             float64
	Embedding                []float32
	UpdatedAt                time.Time
	CreatedAt                time.Time
	DreamKeywords            []string   // nil when not yet generated — RunDreamCycle will invoke the LLM
	DreamTemporalValidatedAt *time.Time // nil = never validated; UpdatedAt > this = re-validate
}

// CycleTimeout is the maximum duration for a single dream cycle.
// Prevents cascading Ollama timeouts from blocking the scheduler.
// Must exceed DreamTimeout (evaluate call) + keyword-embed + RRF overhead.
const CycleTimeout = 700 * time.Second

// Throttle is called between GPU-intensive steps to allow cooldown.
// Returns an error if the context was cancelled during the wait.
type Throttle func(ctx context.Context) error

// NoThrottle is a no-op throttle for full-speed mode.
func NoThrottle(_ context.Context) error { return nil }

// RunDreamCycle executes one dream cycle: pick → keywords → search → evaluate → link.
// Returns the number of links created, or 0 if no block was available.
// Cyclop threshold exceeded by 1 due to sequential error/skip branches per pipeline step;
// extracting helpers would obscure the linear flow without reducing real complexity.
//
//nolint:cyclop // pipeline function with linear step sequence
func RunDreamCycle(ctx context.Context, pool *pgxpool.Pool, embedHost, embedAPIKey, embedProtocol, embedModel string, embedNumCtx int, chatHost, chatAPIKey, chatModel string, think *bool, opts llm.Options, readScopes []string, throttle Throttle) (int, error) {
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
	// Once per block-version: if dream_temporal_validated_at is NULL or older than
	// block.updated_at, run validation and mark it done. Repeated cooldown-rechecks
	// of unchanged blocks skip this step — context_temporal already holds findings.
	needsTemporal := block.DreamTemporalValidatedAt == nil ||
		block.UpdatedAt.After(*block.DreamTemporalValidatedAt)
	if needsTemporal {
		if err := ValidateTemporal(ctx, pool, chatHost, chatAPIKey, chatModel, think, opts, block); err != nil {
			slog.Warn("dream: temporal validation failed (non-fatal)", "block_id", block.ID, "error", err)
		}
		// Mark validated even on non-fatal LLM failure — Phase 1 (deterministic)
		// always ran and produced baseline findings. Next block-update will re-trigger.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET dream_temporal_validated_at = now() WHERE id = $1`,
			block.ID,
		); err != nil {
			slog.Warn("dream: temporal validation marker failed (non-fatal)",
				"block_id", block.ID, "error", err)
		}
	}

	// Throttle: after LLM (temporal) → before embed (keywords).
	if err := throttle(ctx); err != nil {
		return 0, err
	}

	// Step 2: Get keywords — LLM-generated and persisted per block.
	// Reuse stored keywords on subsequent cooldown-rechecks; generate once per block
	// via the Dream model (retries on timeout/parse/count-too-low handled inside
	// GenerateKeywords). On final failure: cool the block and try again next pass —
	// no fallback to deterministic extraction, which produces code-syntax noise.
	keywords := block.DreamKeywords
	if len(keywords) == 0 {
		generated, genErr := GenerateKeywords(ctx, pool, chatHost, chatAPIKey, chatModel, think, opts.NumCtx, block.ID, block.Title, block.Content)
		if genErr != nil {
			slog.Warn("dream: LLM keyword generation exhausted retries, transient cooldown",
				"block_id", block.ID, "error", genErr)
			_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownTransientMinutes)
			return 0, fmt.Errorf("dream: keyword generation: %w", genErr)
		}
		slog.Info("dream: keywords generated by LLM",
			"block_id", block.ID, "count", len(generated))
		// Persist so future cycles reuse. Failure is non-fatal — we still have them in memory.
		if _, persistErr := pool.Exec(ctx,
			`UPDATE context_blocks
			SET dream_keywords = $2, dream_keywords_generated_at = now()
			WHERE id = $1`,
			block.ID, generated,
		); persistErr != nil {
			slog.Warn("dream: keyword persistence failed (non-fatal)",
				"block_id", block.ID, "error", persistErr)
		}
		keywords = generated
	}

	slog.Info("dream: keywords ready",
		"block_id", block.ID,
		"keywords", keywords,
	)

	// Throttle after (potential) LLM keyword call → before embed.
	if err := throttle(ctx); err != nil {
		return 0, err
	}

	// Step 3: Search per keyword via RRF.
	candidates, err := searchByKeywords(ctx, pool, embedHost, embedAPIKey, embedProtocol, embedModel, embedNumCtx, keywords, readScopes, block.ID, block.Scope)
	if err != nil {
		slog.Warn("dream: keyword search failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownTransientMinutes)
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

	// Throttle: after embed (keywords) → before LLM (evaluation).
	if err := throttle(ctx); err != nil {
		return 0, err
	}

	// Step 4: LLM evaluation.
	links, err := EvaluateRelationships(ctx, pool, chatHost, chatAPIKey, chatModel, think, opts, *block, candidates)
	if err != nil {
		slog.Warn("dream: evaluation failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownTransientMinutes)
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
// Excludes is_meta blocks — Post-Reset-Audit 2026-04-20 validated that meta blocks
// (Origin-Stories, CV, Compound-Loop, Agent-Briefing, index) cause 85% of NO_REL noise
// without producing any valid relationships. Filter empirically clean (0% FN-Rate).
func PickBlock(ctx context.Context, pool *pgxpool.Pool) (*BlockInfo, error) {
	var block BlockInfo
	err := pool.QueryRow(ctx,
		`SELECT id, title, category, content, scope, quality_score, updated_at, created_at, dream_keywords, dream_temporal_validated_at
		FROM context_blocks
		WHERE NOT is_archived
		  AND embedding IS NOT NULL
		  AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
		  AND NOT is_meta
		  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
		ORDER BY dream_checked_at ASC NULLS FIRST, quality_score ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
	).Scan(&block.ID, &block.Title, &block.Category, &block.Content, &block.Scope, &block.QualityScore, &block.UpdatedAt, &block.CreatedAt, &block.DreamKeywords, &block.DreamTemporalValidatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick block: %w", err)
	}
	return &block, nil
}

// SetDreamCooldown marks a block as dream-checked with a cooldown period in days.
// Use for outcome-based cooldowns (CooldownActiveDays, CooldownInertDays) that
// reflect a property of the block (linked / not linked). For transient system
// failures use SetDreamCooldownMinutes instead.
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

// SetDreamCooldownMinutes is the transient-error variant. Blocks retry quickly
// once the underlying system (GPU, Ollama, network) recovers. dream_checked_at
// is set so the block does not jump ahead of the unchecked queue.
func SetDreamCooldownMinutes(ctx context.Context, pool *pgxpool.Pool, blockID string, minutes int) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			dream_checked_at = now(),
			dream_cooldown_until = now() + interval '1 minute' * $2
		WHERE id = $1`,
		blockID, minutes,
	)
	return err
}

// searchByKeywords runs one RRF search per keyword, deduplicates results,
// and returns candidate blocks (excluding the source block and cross-scope blocks).
func searchByKeywords(ctx context.Context, pool *pgxpool.Pool, embedHost, embedAPIKey, embedProtocol, embedModel string, embedNumCtx int, keywords []string, scopes []string, sourceID, sourceScope string) ([]BlockInfo, error) {
	seen := make(map[string]bool)
	seen[sourceID] = true // Exclude source block.
	var candidates []BlockInfo
	embedFailures := 0

	for _, kw := range keywords {
		// Embed the keyword for semantic search. Cached by (hash(prefix||kw), model) —
		// Dream keywords repeat heavily across cycles (domain vocabulary, proper nouns).
		kwEmbedding, err := embedcache.Embed(ctx, pool, embedProtocol, embedHost, embedAPIKey, embedModel, kw, embed.PrefixQuery, embedNumCtx)
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
			// Exclude index blocks as candidates — structural listings, not content.
			if r.Category == "index" {
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

	// Meta-Block filter: one batch-query to prune candidates flagged is_meta=true.
	// Post-Reset-Audit 2026-04-20 showed meta blocks (Origin-Story, CV, Agent-Briefing,
	// Compound-Loop, index) generate 85% of NO_REL noise without valid relationships.
	if len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, c.ID)
		}
		rows, err := pool.Query(ctx,
			`SELECT id::text FROM context_blocks WHERE id = ANY($1::uuid[]) AND is_meta`,
			ids,
		)
		if err == nil {
			metaIDs := make(map[string]bool)
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					metaIDs[id] = true
				}
			}
			rows.Close()
			if len(metaIDs) > 0 {
				filtered := candidates[:0]
				for _, c := range candidates {
					if !metaIDs[c.ID] {
						filtered = append(filtered, c)
					}
				}
				candidates = filtered
			}
		}
	}

	return candidates, nil
}

// Stats returns dream processing statistics, filtered by scope.
// Eligibility criteria mirror PickBlock: not archived, has embedding, knowledge/source/canonical, not index.
//
// Returned counters:
//   - total:          all eligible blocks
//   - checked:        blocks that have been through at least one Dream cycle
//   - linked:         total links in the graph (scope-filtered)
//   - pendingRecheck: already-checked blocks whose cooldown has expired (ready for re-dream)
func Stats(ctx context.Context, pool *pgxpool.Pool, scopes []string) (total, checked, linked, pendingRecheck int, err error) {
	err = pool.QueryRow(ctx,
		`SELECT
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND embedding IS NOT NULL AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical')) AND NOT is_meta AND scope = ANY($1))::int,
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND dream_checked_at IS NOT NULL AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical')) AND NOT is_meta AND scope = ANY($1))::int,
			(SELECT count(*) FROM context_dream_links WHERE scope = ANY($1))::int,
			(SELECT count(*) FROM context_blocks
				WHERE NOT is_archived
				  AND embedding IS NOT NULL
				  AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
				  AND category != 'index'
				  AND dream_checked_at IS NOT NULL
				  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
				  AND scope = ANY($1))::int`,
		scopes,
	).Scan(&total, &checked, &linked, &pendingRecheck)
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
// Counts only links with raw_confidence >= 0.7 (LLM self-assessment, not weighted).
// Pre-Reset-Audit 2026-04-20: gating on weighted created a Cold-Start-Lock because
// quality_score ≈ 0.5 drove weighted below 0.5 → UpdateQualityScore skipped → scores
// stayed frozen. Gating on raw breaks the loop and lets quality drift upward organically.
// Blocks without qualifying links keep their current score.
func UpdateQualityScore(ctx context.Context, pool *pgxpool.Pool, blockID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET quality_score = LEAST(1.0,
			0.3
			+ 0.1 * LEAST((SELECT count(*) FROM context_dream_links WHERE target_block_id = $1 AND raw_confidence >= 0.7), 5)
			+ 0.05 * LEAST((SELECT count(*) FROM context_dream_links WHERE source_block_id = $1 AND raw_confidence >= 0.7), 5)
			+ 0.15 * LEAST((SELECT count(DISTINCT relationship) FROM context_dream_links WHERE (source_block_id = $1 OR target_block_id = $1) AND raw_confidence >= 0.7), 4)
		)
		WHERE id = $1
		  AND EXISTS (SELECT 1 FROM context_dream_links WHERE (source_block_id = $1 OR target_block_id = $1) AND raw_confidence >= 0.7)`,
		blockID,
	)
	return err
}

// SupersedesMap returns a map of block_id → []superseded_by_ids for the given block IDs.
// A block can be superseded by multiple newer blocks, hence the slice value.
// Gates on raw_confidence >= 0.7 (LLM self-assessment). weighted is a ranking signal, not a gate.
// Used by the query handler to enrich responses and for filterSuperseded.
func SupersedesMap(ctx context.Context, pool *pgxpool.Pool, blockIDs []string) (map[string][]string, error) {
	if len(blockIDs) == 0 {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT source_block_id::text, target_block_id::text
		FROM context_dream_links
		WHERE relationship = 'supersedes'
		  AND raw_confidence >= 0.7
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
