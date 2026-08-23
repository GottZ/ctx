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

// DefaultNumPredict is the PACKAGE default for the output cap of the dream
// chat calls that share DreamOptions (link evaluation + recurrence confirm).
// Untyped on purpose: config pins its own default tag against this constant.
//
// 600 (was 400): object-map drift (form 2, qwen3.8-local) emits each link as
// {uuid: {target_id, type, confidence}} instead of the array the prompt asks
// for, and the uuid repeats as both map key AND target_id value. Measured with
// the Qwen3 tokenizer, the object-map form costs about 1.5x the array form:
// five entries are ~420 tokens compact / ~500 pretty-printed, against ~250 /
// ~330 for the array form (~85-100 tokens per object-map entry). 400 therefore
// truncated the pretty variant mid-JSON ("unexpected end of JSON input") and
// the whole evaluation was lost. 600 covers the prompt's "Maximum 5 entries"
// in the worst measured form with ~100 tokens of margin. The DEFAULT is not
// raised further on purpose: on OpenAI-style backends max_tokens is charged
// against the context window, so an over-generous cap gets long prompts
// rejected — which is why dream.num_predict exists as an opt-in per install
// rather than a higher constant for everyone.
const DefaultNumPredict = 600

// DreamOptions returns Ollama options for dream evaluation on the package
// default cap. DreamOptionsFor is the configured form; this is the no-config
// caller's shorthand (tests, bench axes).
func DreamOptions() llm.Options {
	return DreamOptionsFor(0)
}

// DreamOptionsFor returns the dream evaluation options with numPredict as the
// output cap, falling back to DefaultNumPredict when it is not positive — 0 is
// the documented "package default" sentinel of dream.num_predict, and Validate
// (V18) already rejected a negative value, so this only has to be total.
// Sampling is tuned for qwen3.6:27b non-thinking mode per vendor
// recommendation, validated against the qwen3.5:27b baseline (Session 24).
//
// The scheduler passes cfg.Dream.NumPredict here, per cycle, so the key is hot.
// The cap is a DEFAULT in the same sense dream.temporal_timeout is: per-backend
// tuning belongs in the serving row's model_map params (num_predict /
// max_tokens, merged by applyModelParams in llm/chain.go), which override
// whatever this returns at dispatch.
func DreamOptionsFor(numPredict int) llm.Options {
	opts := llm.Options{
		Temperature: 0.7,
		TopP:        0.8,
		TopK:        20,
		NumPredict:  DefaultNumPredict,
	}
	if numPredict > 0 {
		opts.NumPredict = numPredict
	}
	return opts
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
	return evaluateRelationships(ctx, pool, r, opts, source, candidates, 0)
}

// evaluateRelationships is EvaluateRelationships plus the retrieval telemetry
// only the dream cycle can supply: capped is how many retrieved candidates the
// aggregate cap dropped before this set was handed over (searchByKeywords).
// The exported form passes 0 — a caller that assembled its own candidate set
// has no cap of ours to report.
func evaluateRelationships(ctx context.Context, pool *pgxpool.Pool, r *Router, opts llm.Options, source BlockInfo, candidates []BlockInfo, capped int) ([]Link, error) {
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
	// Stamped BEFORE the call, not next to links_capped after it: the cap
	// fired during retrieval, so the count belongs on the row even when the
	// eval times out or the answer fails to parse — those are exactly the
	// cycles where a shortened candidate set is worth seeing.
	noteCandidatesCapped(entry, capped)
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
		if noteCapHit(entry, resp, opts) {
			slog.Warn("dream: response hit the output cap — truncated JSON",
				"num_predict", opts.NumPredict, "completion_tokens", resp.EvalCount,
				"error", err, "raw", resp.Message.Content)
		} else {
			slog.Warn("dream: failed to parse LLM response", "error", err, "raw", resp.Message.Content)
		}
		return nil, fmt.Errorf("dream: parse links: %w", err)
	}
	// Zero-link verdicts are a real answer ("no candidate relates"), not a
	// failure, and until now they were indistinguishable in context_llm_log
	// from a run that never got that far — parse_format only says HOW the
	// answer was shaped, never how much it carried. One int makes them
	// countable without a new format token.
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["links_parsed"] = len(links)

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
	if noteDroppedInvalid(entry, len(links), len(valid)) && len(valid) == 0 {
		// Every link the model named was rejected. The function still returns
		// a zero-link success, which the cycle books as "nothing to link" and
		// answers with the multi-day inert back-off — so this line is the only
		// place the loss is visible while it happens.
		slog.Warn("dream: every parsed link dropped by the candidate filter",
			"block_id", source.ID, "links_parsed", len(links), "parse_format", format)
	}

	if capped, dropped := applyHardCap(valid, MaxLinksPerCycle); dropped > 0 {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["links_capped"] = dropped
		valid = capped
	}

	return valid, nil
}

// noteCandidatesCapped stamps the aggregate candidate cap's drop count onto a
// dream-eval entry, mirroring how links_capped records the MaxLinksPerCycle
// cap. Nothing is written when the cap did not bind — a metadata key that
// appears on every row cannot be counted, and 0 is the overwhelming majority.
//
// Split out for the same reason noteCapHit is: the llmlog entry lives inside
// evaluateRelationships and never leaves it, so the stamp is only reachable
// for a unit test as its own function.
func noteCandidatesCapped(entry *llmlog.Entry, capped int) {
	if entry == nil || capped <= 0 {
		return
	}
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["candidates_capped"] = capped
}

// noteDroppedInvalid stamps how many parsed links filterValidCandidates
// rejected — non-UUID targets, ids outside the candidate set (hallucinations),
// unknown relationship types, out-of-range or below-floor confidences — and
// reports whether it wrote anything. Nothing is written when nothing dropped,
// for the reason noteCandidatesCapped states: a key present on every row
// cannot be counted, and 0 is the common case.
//
// The count is directly comparable to links_parsed because the two stages
// between them preserve it — applyLinkFloor rewrites confidences in place and
// enforceSupersedesDirection downgrades a relationship rather than dropping
// the link, so both return exactly what they were given. A row reading
// "links_parsed n, links_dropped_invalid n" is therefore the signature of the
// silent zero-link cycle: the model answered, nothing survived, and the block
// went into the inert back-off anyway.
//
// Observability only: the caller still returns (valid, nil) in that case; no
// cooldown, retry or error path changes here. Whether a fully dropped parse
// should instead become a transient error is a separate decision that needs
// the rate this counter measures — the transient path has no attempt counter,
// so a backend answering deterministically with hallucinated ids would re-burn
// one eval on the same block every five minutes forever.
//
// Split out for the reachability reason noteCapHit documents: the llmlog entry
// is function-local, so the stamp is only testable as its own function.
func noteDroppedInvalid(entry *llmlog.Entry, parsed, valid int) bool {
	dropped := parsed - valid
	if entry == nil || dropped <= 0 {
		return false
	}
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["links_dropped_invalid"] = dropped
	return true
}

// noteCapHit flags a parse failure that is really an output-cap truncation and
// reports whether it did. Returns false (and touches nothing) for every other
// parse failure.
//
// The pipeline is otherwise BLIND to the cap: llm.ChatResponse carries no
// finish_reason / done_reason — neither wire format decodes it on the
// non-streaming path dream uses — so a cap-truncated answer looks exactly like
// malformed output. Both book the parse error, the 5-minute transient cooldown
// and a re-pick, which means a backend that deterministically overruns the cap
// re-burns one eval call every five minutes with nothing in the log to say why.
//
// The available signal is provider-agnostic: EvalCount is the backend's own
// completion-token count and opts.NumPredict is the cap this call site sent.
// Generation stopping at the budget while the JSON does not parse is the cap
// hit. >= rather than ==: some backends count the stop token in. NumPredict
// <= 0 means uncapped — nothing to hit, and unreachable from config: 0 on
// dream.num_predict is the package-default sentinel, so the options this sees
// always carry a positive cap. Known blind spot: a backend row's model_map
// num_predict/max_tokens override is applied inside the chain walk
// (applyModelParams on a local copy) and is not visible here, so on such rows
// the flag compares against the cap this call site SENT — since dream.
// num_predict the configured one rather than the constant, which makes the
// heuristic exact on every install that tunes the key and leaves only the row
// override blind — a heuristic until ChatResponse carries the provider's
// finish_reason.
//
// Observability only: the caller still returns the parse error and the cooldown
// path is unchanged.
func noteCapHit(entry *llmlog.Entry, resp *llm.ChatResponse, opts llm.Options) bool {
	if entry == nil || resp == nil || opts.NumPredict <= 0 || resp.EvalCount < opts.NumPredict {
		return false
	}
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["cap_hit"] = true
	return true
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
