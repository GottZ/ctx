package dream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uuidPattern validates UUID format for target_id fields.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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

// validRelationships defines the allowed relationship types.
var validRelationships = map[string]bool{
	"topical":    true,
	"factual":    true,
	"causal":     true,
	"supersedes": true,
}

// minRawConfidence is the per-type minimum raw (unweighted) LLM confidence.
// Session 24 (2026-04-23): factual threshold lowered from 0.9 to 0.7. V5 prompt combined with
// qwen3.6:27b produces uncalibrated confidence (full_agree mean 0.87 vs wrong_type mean 0.85 —
// gap 0.02). The 0.9 factual gate was a legacy workaround for qwen3.5; on V5+3.6 it dropped
// 2 correct links and 0 wrong types (net-negative).
var minRawConfidence = map[string]float64{
	"topical":    0.7,
	"factual":    0.7,
	"causal":     0.7,
	"supersedes": 0.7,
}

// EvaluateRelationships asks the LLM to classify relationships between a source block
// and candidate blocks found via keyword search. Returns validated links.
// pool may be nil — if provided, the LLM request/response is logged via llmlog.
func EvaluateRelationships(ctx context.Context, pool *pgxpool.Pool, host, apiKey, model string, think *bool, opts llm.Options, source BlockInfo, candidates []BlockInfo) ([]Link, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	userPrompt := buildEvalPrompt(source, candidates)

	start := time.Now()
	resp, err := llm.ChatJSON(ctx, host, apiKey, model, think, dreamSystemPrompt, userPrompt, opts, DreamTimeout)
	duration := time.Since(start)

	// Log the call (async) regardless of error. block_ids: source + all candidates.
	blockIDs := make([]string, 0, 1+len(candidates))
	blockIDs = append(blockIDs, source.ID)
	for _, c := range candidates {
		blockIDs = append(blockIDs, c.ID)
	}
	dreamVer := int16(Version)
	entry := llmlog.Entry{
		Pipeline:      "dream-eval",
		Model:         model,
		Host:          host,
		Duration:      duration,
		Err:           err,
		RequestSystem: dreamSystemPrompt,
		RequestUser:   userPrompt,
		BlockIDs:      blockIDs,
		DreamVersion:  &dreamVer,
	}
	if resp != nil {
		entry.ResponseContent = resp.Message.Content
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
	}
	llmlog.Record(pool, entry)

	if err != nil {
		return nil, fmt.Errorf("dream: evaluate: %w", err)
	}

	links, err := parseLinks(resp.Message.Content)
	if err != nil {
		slog.Warn("dream: failed to parse LLM response", "error", err, "raw", resp.Message.Content)
		return nil, fmt.Errorf("dream: parse links: %w", err)
	}

	// Filter to valid candidates only (prevent LLM hallucinating target IDs).
	candidateIDs := make(map[string]bool)
	for _, c := range candidates {
		candidateIDs[c.ID] = true
	}

	var valid []Link
	for _, l := range links {
		if !uuidPattern.MatchString(l.TargetID) {
			continue
		}
		if !candidateIDs[l.TargetID] {
			continue
		}
		if !validRelationships[l.Relationship] {
			continue
		}
		if l.Confidence > 1.0 || math.IsNaN(l.Confidence) || math.IsInf(l.Confidence, 0) {
			continue
		}
		if l.Confidence < minRawConfidence[l.Relationship] {
			continue
		}
		valid = append(valid, l)
	}

	return valid, nil
}

// WriteLinks persists dream links to the database within a transaction.
// Enforces same-scope rule: only creates links between blocks of the same scope.
// Checks is_archived on target blocks (Race condition mitigation V6).
func WriteLinks(ctx context.Context, pool *pgxpool.Pool, sourceID, sourceScope string, sourceQuality float64, links []Link) (int, error) {
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

		// V6: Skip archived targets.
		if targetArchived {
			continue
		}

		// V5: Same-scope rule — no cross-scope links.
		if targetScope != sourceScope {
			continue
		}

		// V10: Category-Cross coerce — LLM overextends factual to sibling blocks of the same category.
		// Bulk-test 2026-04-21 (n=100, 10 iters): coercing factual→topical when src_cat == tgt_cat
		// delivered +10pp accuracy (69→79%). factual semantics require spec→impl direction, which
		// virtually always crosses categories (spec in decisions/reference, impl in projects).
		// Same-category factual claims are near-uniformly mislabelled topical ties.
		if link.Relationship == "factual" && srcCategory == targetCategory {
			slog.Debug("dream: factual coerced to topical (same-category sibling)",
				"category", srcCategory, "src", sourceID, "tgt", link.TargetID)
			link.Relationship = "topical"
		}

		// V8: Structural check for supersedes — 9B can't distinguish "complementary" from "replaces".
		// Deterministic pre-filter: same category + source older than target.
		if link.Relationship == "supersedes" {
			if srcCategory != targetCategory {
				slog.Debug("dream: supersedes rejected (different category)",
					"source_cat", srcCategory, "target_cat", targetCategory)
				continue
			}
			if !srcUpdatedAt.Before(targetUpdatedAt) {
				slog.Debug("dream: supersedes rejected (source not older)",
					"source_updated", srcUpdatedAt.Format("2006-01-02"),
					"target_updated", targetUpdatedAt.Format("2006-01-02"))
				continue
			}
			// Title similarity check via pg_trgm.
			var sim float64
			_ = tx.QueryRow(ctx,
				`SELECT similarity($1, $2)`, srcTitle, targetTitle,
			).Scan(&sim)
			if sim < 0.25 {
				slog.Debug("dream: supersedes rejected (low title similarity)",
					"similarity", sim, "source_title", srcTitle, "target_title", targetTitle)
				continue
			}
		}

		// V9: Causal-temporal check — causal implies source precedes target in time.
		// Pre-Reset-Audit 2026-04-20: LLM invents causal links with wrong direction
		// (Graph-Topologe: 28% causal-reciprocity). Enforce src.created_at < target.created_at.
		if link.Relationship == "causal" && !srcCreatedAt.Before(targetCreatedAt) {
			slog.Debug("dream: causal rejected (source not older than target)",
				"source_created", srcCreatedAt.Format("2006-01-02"),
				"target_created", targetCreatedAt.Format("2006-01-02"))
			continue
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

// parseLinks parses the LLM JSON response into Link structs.
func parseLinks(raw string) ([]Link, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}

	var links []Link
	if err := json.Unmarshal([]byte(raw), &links); err != nil {
		return nil, fmt.Errorf("parse links: %w", err)
	}
	return links, nil
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
