package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// linkPool is the minimum *pgxpool.Pool surface that WriteLinks needs.
// The interface lets tests pass a pgxmock-backed pool without exercising a
// real database. *pgxpool.Pool implicitly satisfies it.
type linkPool interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const (
	// DreamTimeout is the HTTP timeout for dream LLM calls.
	// 600s handles cold-start loads (observed ~130s after Ollama updates), Ollama request
	// queueing when prior cycles were client-cancelled, and long prompts on 27B+ models.
	// Combined with CycleTimeout (shared parent), the practical budget for one evaluate call.
	DreamTimeout = 600 * time.Second

	// maxContentLen limits content passed to LLM to reduce prompt injection surface.
	maxContentLen = 800

	// dreamSystemPrompt instructs the LLM how to evaluate block relationships.
	// Session 24 (2026-04-23): V5 prompt — topical-as-fallback default, supersedes hard-tightened,
	// causal liberalised to accept decision→implementation chains. Validated on top-50 hardest
	// benchmark vs double-judge stable gold: 62.3% accuracy (+19.7pp over baseline 42.6%).
	//
	// Session 25 (2026-05-04): V6 (causal anti-pattern clauses) was deployed in 6b37662
	// based on a 30-sample Sub-Agent audit (recall 0.71 vs V5 0.47). Stable-Gold re-bench
	// (commit dc1776c, n=122) showed V6 NET-WORSE: 54.1% vs V5 57.4%. Topical-recall fell
	// from 76% to 63% (Δ-13pp, 7 lost) — V6's REJECT clauses + "when in doubt: topical"
	// caused over-hedging to 'none'. Marginal causal/factual/none gains (+1 each) did not
	// compensate. V6 reverted in commit ce8e8f6 — V5 is production again. V7 prompt iteration
	// must protect topical-recall while improving causal precision.
	dreamSystemPrompt = `Classify relationships between a source block and candidate blocks.

For each candidate, decide which type applies — or skip it.

Default: if a candidate shares meaningful content with source but doesn't clearly fit a stronger type, use "topical".

Types:
- supersedes: source contains a concrete specific fact that target explicitly corrects or replaces. VERY RARE — requires the source fact to be concretely wrong/obsolete AND target to state the authoritative replacement. Thematic update, newer timestamp, or general progress is NOT supersedes.
- causal: source describes a concrete event, decision, or change whose occurrence enabled target to exist or take its current form. Source must meaningfully predate target. A decision followed by its implementation qualifies. Parallel activity or shared timeline alone does NOT.
- factual: source SPECIFIES a concrete thing (parameter, rule, config, contract, spec) that target directly implements or uses. One-way SPEC→IMPL direction required. Peer-level blocks at the same abstraction are NOT factual — they are topical.
- topical: both blocks treat the same specific topic substantively. This is the common case for genuinely related blocks that aren't in a spec→impl or cause→effect relationship.

Output a JSON array of {target_id, type, confidence}. Empty [] when no candidate relates. Maximum 5 entries.`
)

// DreamOptions returns Ollama options for dream evaluation.
// Tuned for qwen3.6:27b non-thinking mode per vendor recommendation,
// validated against qwen3.5:27b baseline in /tmp/bench_l2 (Session 24).
func DreamOptions() llm.Options {
	return llm.Options{
		Temperature: 0.7,
		TopP:        0.8,
		TopK:        20,
		NumPredict:  400,
	}
}

// Link represents a cross-reference between two blocks.
type Link struct {
	TargetID     string  `json:"target_id"`
	Relationship string  `json:"type"`
	Confidence   float64 `json:"confidence"`
}

// EvaluateRelationships asks the LLM to classify relationships between a source block
// and candidate blocks found via keyword search. Returns validated links.
// pool may be nil — if provided, the LLM request/response is logged via llmlog.
func EvaluateRelationships(ctx context.Context, pool *pgxpool.Pool, host, apiKey, model string, think *bool, opts llm.Options, source BlockInfo, candidates []BlockInfo) ([]Link, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	userPrompt := buildEvalPrompt(source, candidates)
	blockIDs := make([]string, 0, 1+len(candidates))
	blockIDs = append(blockIDs, source.ID)
	for _, c := range candidates {
		blockIDs = append(blockIDs, c.ID)
	}
	dreamVer := int16(Version)

	// Log entry mutated through the function; deferred Record (closure deref at
	// trigger time) captures final state including parse errors that surface
	// after the LLM call. Registered AFTER the empty-candidates early-return so
	// no zero-duration no-op rows pollute the log.
	entry := &llmlog.Entry{
		Pipeline:      "dream-eval",
		Model:         model,
		Host:          host,
		RequestSystem: dreamSystemPrompt,
		RequestUser:   userPrompt,
		BlockIDs:      blockIDs,
		DreamVersion:  &dreamVer,
	}
	defer func() { llmlog.Record(pool, *entry) }()

	start := time.Now()
	resp, err := chatJSON(ctx, host, apiKey, model, think, dreamSystemPrompt, userPrompt, opts, DreamTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err

	if resp != nil {
		entry.ResponseContent = resp.Message.Content
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
	}

	if err != nil {
		return nil, fmt.Errorf("dream: evaluate: %w", err)
	}

	links, format, err := parseLinks(resp.Message.Content)
	if format != "" {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["parse_format"] = format
	}
	if err != nil {
		entry.Err = fmt.Errorf("parse: %w", err)
		slog.Warn("dream: failed to parse LLM response", "error", err, "raw", resp.Message.Content)
		return nil, fmt.Errorf("dream: parse links: %w", err)
	}

	candidateIDs := make(map[string]bool)
	for _, c := range candidates {
		candidateIDs[c.ID] = true
	}
	valid := filterValidCandidates(links, candidateIDs)

	if capped, dropped := applyHardCap(valid, MaxLinksPerCycle); dropped > 0 {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["links_capped"] = dropped
		valid = capped
	}

	return valid, nil
}

// deletedLink captures what a stale-link DELETE returned, so the caller can
// revert side-effects (e.g. block_type='snapshot' set by ApplySupersedes when
// the supersedes-link was first written).
type deletedLink struct {
	TargetID     string
	Relationship string
}

// deleteStaleLinks removes context_dream_links rows for sourceID whose
// target_block_id is not in keptTargets. If keptTargets is empty, ALL rows
// for sourceID are deleted (caller must guard against this when the empty
// state is a transient LLM failure rather than a deliberate clear-out).
// Returns the rows that were deleted so the caller can run targeted reverse
// effects (snapshot revert).
func deleteStaleLinks(ctx context.Context, tx pgx.Tx, sourceID string, keptTargets []string) ([]deletedLink, error) {
	var rows pgx.Rows
	var err error
	if len(keptTargets) == 0 {
		rows, err = tx.Query(ctx,
			`DELETE FROM context_dream_links
			WHERE source_block_id = $1::uuid
			RETURNING target_block_id::text, relationship`,
			sourceID)
	} else {
		rows, err = tx.Query(ctx,
			`DELETE FROM context_dream_links
			WHERE source_block_id = $1::uuid
			  AND target_block_id != ALL($2::uuid[])
			RETURNING target_block_id::text, relationship`,
			sourceID, keptTargets)
	}
	if err != nil {
		return nil, fmt.Errorf("delete stale: %w", err)
	}
	defer rows.Close()

	var deleted []deletedLink
	for rows.Next() {
		var d deletedLink
		if err := rows.Scan(&d.TargetID, &d.Relationship); err != nil {
			return nil, fmt.Errorf("scan deleted: %w", err)
		}
		deleted = append(deleted, d)
	}
	return deleted, rows.Err()
}

// replaceStaleLinks deletes context_dream_links for sourceID that are not in
// keptTargets and reverts ApplySupersedes-side-effects for any deleted
// supersedes-links (block_type='snapshot' → NULL when superseded_by matched
// the deleted target).
func replaceStaleLinks(ctx context.Context, tx pgx.Tx, sourceID string, keptTargets []string) error {
	deleted, err := deleteStaleLinks(ctx, tx, sourceID, keptTargets)
	if err != nil {
		return fmt.Errorf("dream: delete stale links: %w", err)
	}
	for _, d := range deleted {
		if d.Relationship != "supersedes" {
			continue
		}
		_, revertErr := tx.Exec(ctx,
			`UPDATE context_blocks
			SET block_type = NULL, superseded_by = NULL
			WHERE id = $1::uuid
			  AND block_type = 'snapshot'
			  AND superseded_by = $2::uuid`,
			sourceID, d.TargetID)
		if revertErr != nil {
			slog.Warn("dream: snapshot revert failed (non-fatal)",
				"source", sourceID, "target", d.TargetID, "error", revertErr)
		}
	}
	return nil
}

// WriteLinks persists dream links to the database within a transaction.
// Enforces same-scope rule: only creates links between blocks of the same scope.
// Checks is_archived on target blocks (Race condition mitigation V6).
//
// Replace-Semantik (S25 Welle 7): when the cycle produces at least one
// successfully-written link, stale links for the same source are removed
// inside the same transaction. ApplySupersedes-side-effects on previously
// snapshot-marked blocks are reverted when the supersedes-link is deleted.
// 0-written cycles do NOT trigger replace — that path is reserved for
// transient LLM failures (Pessimist M1: stochastic empty responses must
// not be destructive).
//
// Cyclomatic complexity 26 vs lint cap 25: V5/V6/V8/V9/V10 structural
// checks form a linear filter chain in one loop body — extracting them
// would obscure the per-link decision flow without reducing real complexity.
//
//nolint:cyclop // pipeline function with linear V5/V6/V8/V9/V10 filter chain
func WriteLinks(ctx context.Context, pool linkPool, sourceID, sourceScope string, sourceQuality float64, links []Link) (int, error) {
	if len(links) == 0 {
		return 0, nil
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("dream: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Fetch source block metadata for structural checks.
	var srcCategory string
	var srcUpdatedAt, srcCreatedAt time.Time
	var srcTitle string
	_ = tx.QueryRow(ctx,
		`SELECT category, updated_at, created_at, title FROM context_blocks WHERE id = $1`,
		sourceID,
	).Scan(&srcCategory, &srcUpdatedAt, &srcCreatedAt, &srcTitle)

	written := 0
	keptTargets := make([]string, 0, len(links))
	for _, link := range links {
		// Fetch target block scope + archived status + metadata for structural checks.
		var targetScope string
		var targetArchived bool
		var targetQuality float64
		var targetCategory string
		var targetUpdatedAt, targetCreatedAt time.Time
		var targetTitle string
		err := tx.QueryRow(ctx,
			`SELECT scope, is_archived, quality_score, category, updated_at, created_at, title FROM context_blocks WHERE id = $1`,
			link.TargetID,
		).Scan(&targetScope, &targetArchived, &targetQuality, &targetCategory, &targetUpdatedAt, &targetCreatedAt, &targetTitle)
		if err != nil {
			slog.Warn("dream: target block not found", "target_id", link.TargetID)
			continue
		}

		// V5/V6: scope + archived gate.
		if ok, reason := acceptScopeAndArchived(targetScope, sourceScope, targetArchived); !ok {
			slog.Debug("dream: link rejected", "src", sourceID, "tgt", link.TargetID, "reason", reason)
			continue
		}

		// V10: factual same-category coerce → topical.
		if newRel := coerceCategoryFactual(link.Relationship, srcCategory, targetCategory); newRel != link.Relationship {
			slog.Debug("dream: factual coerced to topical (same-category sibling)",
				"category", srcCategory, "src", sourceID, "tgt", link.TargetID)
			link.Relationship = newRel
		}

		// V8: supersedes structural pre-filter (same cat + src older + title sim).
		if link.Relationship == "supersedes" {
			var sim float64
			_ = tx.QueryRow(ctx, `SELECT similarity($1, $2)`, srcTitle, targetTitle).Scan(&sim)
			if ok, reason := acceptSupersedes(srcCategory, targetCategory, srcUpdatedAt, targetUpdatedAt, sim); !ok {
				slog.Debug("dream: supersedes rejected", "src", sourceID, "tgt", link.TargetID, "reason", reason, "similarity", sim)
				continue
			}
		}

		// V9: causal predates check (created_at).
		if link.Relationship == "causal" {
			if ok, reason := acceptCausal(srcCreatedAt, targetCreatedAt); !ok {
				slog.Debug("dream: causal rejected", "src", sourceID, "tgt", link.TargetID, "reason", reason)
				continue
			}
		}

		// Weighted confidence: relationship_strength × source_quality × target_quality.
		// Persisted as `confidence` for downstream ranking signals.
		// Gates operate on `raw_confidence` (LLM self-assessment).
		weightedConfidence := link.Confidence * sourceQuality * targetQuality
		if math.IsNaN(weightedConfidence) || math.IsInf(weightedConfidence, 0) {
			weightedConfidence = 0.5
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, dream_version, scope, metadata)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::jsonb)
			ON CONFLICT (source_block_id, target_block_id) DO UPDATE SET
				relationship = EXCLUDED.relationship,
				confidence = EXCLUDED.confidence,
				raw_confidence = EXCLUDED.raw_confidence,
				dream_version = EXCLUDED.dream_version,
				metadata = EXCLUDED.metadata,
				created_at = now()`,
			sourceID, link.TargetID, link.Relationship, weightedConfidence, link.Confidence, Version, sourceScope,
			`{}`,
		)
		if err != nil {
			slog.Warn("dream: write link failed", "source", sourceID, "target", link.TargetID, "error", err)
			break // TX is in failed state after PG error — cannot continue.
		}
		written++
		keptTargets = append(keptTargets, link.TargetID)

		// ApplySupersedes: mark source block as snapshot when superseded.
		// Source is the OLD block (Dream Rule 6: "target block is a newer version").
		// Only apply at high confidence to prevent false-positive snapshot marking.
		if link.Relationship == "supersedes" && weightedConfidence >= 0.7 {
			_, err = tx.Exec(ctx,
				`UPDATE context_blocks SET block_type = 'snapshot', superseded_by = $2::uuid
				WHERE id = $1::uuid AND block_type != 'snapshot'`,
				sourceID, link.TargetID,
			)
			if err != nil {
				slog.Warn("dream: apply supersedes failed", "source", sourceID, "target", link.TargetID, "error", err)
				break
			}
			slog.Info("dream: marked block as snapshot",
				"block_id", sourceID,
				"superseded_by", link.TargetID,
			)
		}
	}

	// Replace-Semantik: only when at least one link was successfully written.
	// 0-written cycles preserve old links (LLM stochastic empty/all-filtered must
	// not be destructive — see Pessimist M1).
	if written > 0 {
		if err := replaceStaleLinks(ctx, tx, sourceID, keptTargets); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("dream: commit links: %w", err)
	}

	// Audit log outside TX — failure here doesn't roll back links.
	if written > 0 {
		type auditLink struct {
			TargetID     string  `json:"target_id"`
			Relationship string  `json:"relationship"`
			Confidence   float64 `json:"confidence"`
		}
		auditLinks := make([]auditLink, 0, written)
		for _, l := range links {
			auditLinks = append(auditLinks, auditLink(l))
		}
		meta, _ := json.Marshal(map[string]any{
			"source":        "dream_v1",
			"links_created": written,
			"links":         auditLinks,
		})
		_, _ = pool.Exec(ctx,
			`INSERT INTO context_write_log
				(block_id, decision, similarity, scope, block_title, block_category, metadata)
			SELECT $1::uuid, 'dream_link', 0, scope, title, category, $2::jsonb
			FROM context_blocks WHERE id = $1`,
			sourceID, meta,
		)
	}

	return written, nil
}

// buildEvalPrompt constructs the user prompt for relationship evaluation.
func buildEvalPrompt(source BlockInfo, candidates []BlockInfo) string {
	var b strings.Builder
	b.WriteString("<source>\n")
	fmt.Fprintf(&b, "ID: %s\nTitle: %s\nCategory: %s\nUpdated: %s\n",
		source.ID, llm.EscapeXml(source.Title), source.Category, source.UpdatedAt.Format("2006-01-02"))
	b.WriteString("Content: ")
	b.WriteString(llm.EscapeXml(truncate(source.Content, maxContentLen)))
	b.WriteString("\n</source>\n\n<candidates>\n")

	for _, c := range candidates {
		fmt.Fprintf(&b, "<block id=\"%s\" title=\"%s\" category=\"%s\" updated=\"%s\">\n",
			c.ID, llm.EscapeXml(c.Title), c.Category, c.UpdatedAt.Format("2006-01-02"))
		b.WriteString(llm.EscapeXml(truncate(c.Content, maxContentLen/2)))
		b.WriteString("\n</block>\n")
	}
	b.WriteString("</candidates>")
	return b.String()
}

// truncate limits string length to n bytes, cutting at word boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Find last space before limit.
	cut := strings.LastIndex(s[:n], " ")
	if cut < n/2 {
		cut = n
	}
	return s[:cut]
}
