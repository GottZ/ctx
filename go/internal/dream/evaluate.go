package dream

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
