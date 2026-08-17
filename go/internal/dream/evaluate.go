package dream

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/util"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DreamTimeout is the HTTP timeout for dream LLM calls.
	// Empirically tuned from 5878 successful prod dream-eval calls (2026-04-20..05-28):
	// p50=23s, p90=30s, p99=156s — the entire success distribution lives under 156s,
	// covering cold-start (~130s after Ollama updates) and long 27B prompts. 180s adds
	// a safety margin over p99 while reclaiming the dead overhang: at the old 600s, 43
	// stuck calls burned ~7.2 GPU-hours of pure timeout wait, each blocking the workers=1
	// loop for a full 10 minutes. At 180s a stuck call fails 3.3× faster, costing only
	// ~0.71% of legitimate calls a re-queue (the 42 successes that ran 180-600s).
	DreamTimeout = 180 * time.Second

	// MaxContentLen limits content passed to LLM to reduce prompt injection
	// surface. Candidates get half of it (buildEvalPrompt). Exported since H12:
	// the prompt-budget gate multiplies it against the candidate count and has
	// to be able to SEE it — a cap that only the package can read is a cap the
	// budget cannot be held to.
	MaxContentLen = 800

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
//
// NumPredict 600 (was 400): object-map drift (form 2, qwen3.8-local) emits
// each link as {uuid: {target_id, type, confidence}} — roughly 2x the token
// cost of the array form the prompt requests, because the uuid repeats as both
// map key AND target_id value. Five entries in that form need ~500-600 tokens,
// so 400 truncated mid-JSON ("unexpected end of JSON input") and the whole
// evaluation was lost. 600 fits the prompt's "Maximum 5 entries" with margin.
func DreamOptions() llm.Options {
	return llm.Options{
		Temperature: 0.7,
		TopP:        0.8,
		TopK:        20,
		NumPredict:  600,
	}
}

// Link represents a cross-reference between two blocks.
type Link struct {
	TargetID     string  `json:"target_id"`
	Relationship string  `json:"type"`
	Confidence   float64 `json:"confidence"`
	// Floored marks a link whose confidence was NOT emitted by the model:
	// the parser assigned the per-type minRawConfidence floor (absent
	// confidence, string-map drift form — PR #12). applyLinkFloor lifts such
	// links to the configured dream.link_floor_confidence before filtering.
	Floored bool `json:"-"`
}

// EvaluateRelationships asks the LLM to classify relationships between a source block
// and candidate blocks found via keyword search. Returns validated links.
// pool may be nil — if provided, the LLM request/response is logged via llmlog.
// The prompt carries source + ALL candidate contents, so the chain resolves
// at the max over every involved block's sensitivity (design 03 §2.2, dream
// row); a zero-value sensitivity folds to credentials (fail-closed).
func EvaluateRelationships(ctx context.Context, pool *pgxpool.Pool, r *Router, opts llm.Options, source BlockInfo, candidates []BlockInfo) ([]Link, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	systemPrompt, userPrompt := buildEvalPrompt(source, candidates)
	blockIDs := make([]string, 0, 1+len(candidates))
	blockIDs = append(blockIDs, source.ID)
	sensParts := make([]backends.Sensitivity, 0, 1+len(candidates))
	sensParts = append(sensParts, source.Sensitivity)
	for _, c := range candidates {
		blockIDs = append(blockIDs, c.ID)
		sensParts = append(sensParts, c.Sensitivity)
	}
	required := backends.MaxSensitivity(sensParts...)
	dreamVer := int16(Version)

	// Log entry mutated through the function; deferred Record (closure deref at
	// trigger time) captures final state including parse errors that surface
	// after the LLM call. Registered AFTER the empty-candidates early-return so
	// no zero-duration no-op rows pollute the log.
	entry := &llmlog.Entry{
		Pipeline:      "dream-eval",
		RequestSystem: systemPrompt,
		RequestUser:   userPrompt,
		BlockIDs:      blockIDs,
		DreamVersion:  &dreamVer,
	}
	defer func() { llmlog.Record(pool, entry.Slimmed()) }()

	start := time.Now()
	resp, served, attempts, err := r.chat(ctx, backends.RoleDream, required,
		systemPrompt, userPrompt, opts, DreamTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err
	r.applyChainTelemetry(entry, backends.RoleDream, required, served, attempts, err)

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

	// Operator floor for links the model named without a strength signal
	// (Floored, PR #12): lift to dream.link_floor_confidence (default 0.9,
	// hot via the per-iteration router) before direction enforcement and
	// filtering, so downstream gates and the write path see the final value.
	links = applyLinkFloor(links, r.LinkFloor)

	// Welle 46 (2026-05-22): post-parse hard-constraint on supersedes
	// direction. Downgrades inverted supersedes links to topical. The old
	// len-diff telemetry here was structurally dead (downgrade never changes
	// the count) — the function now reports the downgrade count itself.
	var dirDowngraded int
	links, dirDowngraded = enforceSupersedesDirection(links, source.CreatedAt, candidates)
	if dirDowngraded > 0 {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["supersedes_direction_downgraded"] = dirDowngraded
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

// buildEvalPrompt constructs the user prompt for relationship evaluation and
// the system prompt that belongs to it.
//
// ONE source and N candidates whose boundaries CARRY MEANING: the model answers
// with a target_id, so a block it cannot delimit is a link it can misattribute.
// That is the nonce case (design 04 §4.3) — one per build, binding every wrap
// and the rule that names it in the system prompt.
//
// The two metadata positions are guarded differently because they break
// differently (§2.3-c). Category is the carrier in both: a free string whose
// only constraint is len<=100, no format CHECK, foreign-written by definition.
//   - candidate side: the value sits in a double-quoted ATTRIBUTE and used to
//     reach it unescaped — a quote closed the element and opened a forged one.
//   - source side: the value sits on a LINE-BASED key:value line, where a bare
//     newline forges the next line without needing a single "<". XML escaping
//     provably does not cover this one, which is why both positions run through
//     guardLine (ClampLine AND EscapeXml), not through EscapeXml alone.
//
// The ids stay raw: they are DB uuids, not foreign text, and the answer path
// re-checks them against the candidate set anyway (filterValidCandidates) —
// the guard reduces the surface, that filter is the defence.
func buildEvalPrompt(source BlockInfo, candidates []BlockInfo) (system, user string) {
	nonce := promptguard.NewNonce()

	var b strings.Builder
	b.WriteString("<source>\n")
	fmt.Fprintf(&b, "ID: %s\nTitle: %s\nCategory: %s\nUpdated: %s\n",
		source.ID, guardLine(source.Title), guardLine(source.Category),
		source.UpdatedAt.Format("2006-01-02"))
	b.WriteString(promptguard.Wrap(nonce, "source",
		guardText(truncate(source.Content, MaxContentLen))))
	b.WriteString("\n</source>\n\n<candidates>\n")

	for _, c := range candidates {
		fmt.Fprintf(&b, "<block id=\"%s\" title=\"%s\" category=\"%s\" updated=\"%s\">\n",
			c.ID, guardLine(c.Title), guardLine(c.Category), c.UpdatedAt.Format("2006-01-02"))
		b.WriteString(promptguard.Wrap(nonce, "candidate",
			guardText(truncate(c.Content, MaxContentLen/2))))
		b.WriteString("\n</block>\n")
	}
	b.WriteString("</candidates>")

	return dreamSystemPrompt + "\n\n" + promptguard.Rule(nonce), b.String()
}

// truncate limits string length to n, cutting at a word boundary when one sits
// in the second half of the budget.
//
// The fallback cut is rune-aware: a byte slice can split a multi-byte rune and
// emit invalid UTF-8 into the prompt (internal/llm/synthesize.go names the same
// defect). Consequence worth knowing: on that path n is a RUNE budget, so
// non-ASCII content may exceed n bytes. The trigger threshold stays byte-based
// and on ASCII the byte image is exactly the pre-guard one.
//
// Shared with the keyword and recurrence builders — the rune boundary lands
// there too.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head := util.TruncateRunesWithSuffix(s, "", n)
	// A space is single-byte, so cutting at one can never split a rune.
	cut := strings.LastIndex(head, " ")
	if cut < n/2 {
		return head
	}
	return head[:cut]
}
