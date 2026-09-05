package dream

import (
	"context"
	"errors"
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
//
// PROD 2026-08-22/24 (Nemotron 3.5 Lightning): some models served through
// vLLM/LiteLLM ignore "Maximum 5 entries" and enumerate every candidate
// (~40 tokens per pretty-printed array entry). 600 truncated the JSON with
// 10+ candidates, 1200 with 21-28 candidates ("parse links: unexpected end
// of JSON input"). Set dream.num_predict=1600 (covers ~30 entries) on such
// installs — still far below the 12000 that previously let reasoning tokens
// blow the budget. Backends that obey the 5-entry instruction stay well
// under it (measured 61-315 tokens).
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

// ErrOutputCapHit marks an evaluation whose answer was truncated at the
// output cap TWICE — once on the regular attempt and once more on the bounded
// retry at dream.eval_cap_retry_factor times the resolved cap. It is the one
// eval outcome RunDreamCycle does not treat as a transient failure: the block
// is booked as a completed-but-inert eval (back-off advance) instead of being
// re-picked in five minutes, because a prompt that overruns twice the cap will
// overrun it again unchanged. A SINGLE cap hit never carries this sentinel —
// neither with the retry off (the key is a kill switch) nor when the retry
// cannot take effect (an extra_body.max_tokens row) — so nothing escalates on
// a path that has not actually been retried.
var ErrOutputCapHit = errors.New("dream: eval output cap hit")

// evalRequest is everything two attempts at the same evaluation share: the
// built prompt and the derived gate/telemetry inputs. Assembled once so a
// retry re-sends the IDENTICAL prompt — no shrinking, no candidate
// re-planning, so the retry isolates the cap as the single variable.
type evalRequest struct {
	system, user string
	blockIDs     []string
	required     backends.Sensitivity
	source       BlockInfo
	candidates   []BlockInfo
	candidateIDs map[string]bool
	capped       int
}

// attemptResult is one wire call's outcome as the retry decision needs it.
//
// capHit is separate from err ON PURPOSE, rather than a sentinel wrapped into
// err by evalAttempt: the very same truncation must surface as a PLAIN parse
// error when it will not be retried (retry off, or a row the retry cannot
// affect) and as ErrOutputCapHit only after the retry was actually spent.
// Only the caller knows which of the two it is, so only the caller wraps.
type attemptResult struct {
	links  []Link
	served *backends.Backend
	capHit bool
	err    error
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
//
// It owns the cap-hit RETRY POLICY; evalAttempt owns one wire call. At most
// two calls are ever made, each with its own llmlog row (token and cost
// accounting is per wire call — a hidden second call inside one row would
// corrupt both). The policy:
//
//   - anything but a cap hit returns straight through, unchanged;
//   - a cap hit with CapRetryFactor <= 1 returns the PLAIN parse error — the
//     key is a kill switch, and with it off this function behaves exactly as
//     it did before the retry existed;
//   - a cap hit on a row whose extra_body pins a numeric max_tokens ALSO
//     returns the plain parse error: that value outbids Options.NumPredict on
//     the wire (applyOpenAIBodyExtras merges last-write-wins, which is why
//     autowindow.resolveMaxOut ranks it higher too), so the retry would send
//     the identical cap, hit it again and book the block inert for a row
//     setting. Skipping keeps "no escalation on a retry that cannot work";
//   - otherwise ONE retry at NumPredictScale = CapRetryFactor. The scaling is
//     applied inside the chain walk, on the cap that attempt RESOLVES to
//     (llm.applyModelParams), so a model_map num_predict override is scaled
//     rather than bypassed;
//   - a cap hit on that retry — and only then — returns ErrOutputCapHit.
func evaluateRelationships(ctx context.Context, pool *pgxpool.Pool, r *Router, opts llm.Options, source BlockInfo, candidates []BlockInfo, capped int) ([]Link, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	req := buildEvalRequest(source, candidates, capped)

	res := evalAttempt(ctx, pool, r, req, opts, false)
	if !res.capHit {
		return res.links, res.err
	}

	switch {
	case r.CapRetryFactor <= 1 || opts.NumPredict <= 0:
		// Retry disabled (or an uncapped call site, which cannot be scaled):
		// today's behaviour, plain parse error, transient cooldown.
		return nil, res.err
	case rowPinsMaxTokens(res.served):
		slog.Warn("dream: output cap hit on a backend row that pins extra_body.max_tokens — retry skipped, it would send the same cap",
			"block_id", source.ID, "backend", backendName(res.served),
			"num_predict", opts.NumPredict, "factor", r.CapRetryFactor)
		return nil, res.err
	}

	retryOpts := opts
	retryOpts.NumPredictScale = r.CapRetryFactor
	retry := evalAttempt(ctx, pool, r, req, retryOpts, true)
	if retry.capHit {
		return nil, fmt.Errorf("%w: %w", ErrOutputCapHit, retry.err)
	}
	return retry.links, retry.err
}

// buildEvalRequest assembles the per-evaluation invariants: the guarded
// prompt, the block-id list for the llmlog row, the folded sensitivity the
// chain resolves at (max over source + every candidate — a zero value folds to
// credentials, fail-closed) and the candidate-id set the answer is filtered
// against.
func buildEvalRequest(source BlockInfo, candidates []BlockInfo, capped int) *evalRequest {
	system, user := buildEvalPrompt(source, candidates)
	blockIDs := make([]string, 0, 1+len(candidates))
	blockIDs = append(blockIDs, source.ID)
	sensParts := make([]backends.Sensitivity, 0, 1+len(candidates))
	sensParts = append(sensParts, source.Sensitivity)
	candidateIDs := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		blockIDs = append(blockIDs, c.ID)
		sensParts = append(sensParts, c.Sensitivity)
		candidateIDs[c.ID] = true
	}
	return &evalRequest{
		system: system, user: user,
		blockIDs:     blockIDs,
		required:     backends.MaxSensitivity(sensParts...),
		source:       source,
		candidates:   candidates,
		candidateIDs: candidateIDs,
		capped:       capped,
	}
}

// evalAttempt performs ONE link-evaluation wire call and turns its answer into
// validated links. It owns its own llmlog entry and deferred Record, so the
// retry above produces a SECOND row rather than overwriting the first: one row
// per wire call is what makes token counts, durations and costs add up, and
// the retry row is identifiable by metadata.cap_retry.
//
// retry only marks the row; the widened cap travels in opts.NumPredictScale.
func evalAttempt(ctx context.Context, pool *pgxpool.Pool, r *Router, req *evalRequest, opts llm.Options, retry bool) attemptResult {
	dreamVer := int16(Version)

	// Log entry mutated through the function; deferred Record (closure deref at
	// trigger time) captures final state including parse errors that surface
	// after the LLM call. Reached only past the caller's empty-candidates
	// early-return, so no zero-duration no-op rows pollute the log.
	entry := &llmlog.Entry{
		Pipeline:      "dream-eval",
		RequestSystem: req.system,
		RequestUser:   req.user,
		BlockIDs:      req.blockIDs,
		DreamVersion:  &dreamVer,
	}
	// Stamped BEFORE the call, not next to links_capped after it: the cap
	// fired during retrieval, so the count belongs on the row even when the
	// eval times out or the answer fails to parse — those are exactly the
	// cycles where a shortened candidate set is worth seeing. On BOTH attempt
	// rows: the retry evaluates the same shortened set.
	noteCandidatesCapped(entry, req.capped)
	if retry {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["cap_retry"] = true
	}
	defer func() { llmlog.Record(pool, entry.Slimmed(r.Devmode)) }()

	start := time.Now()
	resp, served, attempts, err := r.chat(ctx, backends.RoleDream, req.required,
		req.system, req.user, opts, DreamTimeout)
	entry.Duration = time.Since(start)
	entry.Err = err
	r.applyChainTelemetry(entry, backends.RoleDream, req.required, served, attempts, err)

	if resp != nil {
		entry.ResponseContent = resp.Message.Content
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
		// The provider's own stop reason, raw (issue #26). Recorded on
		// SUCCESS too: "which backends actually report one" is only
		// answerable from the rows where nothing went wrong.
		if resp.FinishReason != "" {
			if entry.Metadata == nil {
				entry.Metadata = map[string]any{}
			}
			entry.Metadata["finish_reason"] = resp.FinishReason
		}
	}

	if err != nil {
		return attemptResult{served: served, err: fmt.Errorf("dream: evaluate: %w", err)}
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
		hit := capHit(entry, resp, opts)
		if hit {
			slog.Warn("dream: response hit the output cap — truncated JSON",
				"num_predict", effectiveCap(opts), "completion_tokens", resp.EvalCount,
				"finish_reason", resp.FinishReason, "cap_retry", retry,
				"error", err, "raw", resp.Message.Content)
		} else {
			slog.Warn("dream: failed to parse LLM response", "error", err, "raw", resp.Message.Content)
		}
		return attemptResult{served: served, capHit: hit, err: fmt.Errorf("dream: parse links: %w", err)}
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
	links, dirDowngraded = enforceSupersedesDirection(links, req.source.CreatedAt, req.candidates)
	if dirDowngraded > 0 {
		entry.Metadata["supersedes_direction_downgraded"] = dirDowngraded
	}

	valid := filterValidCandidates(links, req.candidateIDs)
	if noteDroppedInvalid(entry, len(links), len(valid)) && len(valid) == 0 {
		// Every link the model named was rejected. The function still returns
		// a zero-link success, which the cycle books as "nothing to link" and
		// answers with the multi-day inert back-off — so this line is the only
		// place the loss is visible while it happens.
		slog.Warn("dream: every parsed link dropped by the candidate filter",
			"block_id", req.source.ID, "links_parsed", len(links), "parse_format", format)
	}

	if capped, dropped := applyHardCap(valid, MaxLinksPerCycle); dropped > 0 {
		entry.Metadata["links_capped"] = dropped
		valid = capped
	}

	return attemptResult{links: valid, served: served}
}

// effectiveCap is the output cap the attempt carrying opts actually SENDS —
// the base cap times the scale, matching what applyModelParams computes inside
// the chain walk. The distinction is load-bearing on the retry: opts.NumPredict
// there is still the BASE cap (the scaling happens on a chain-local copy, which
// this package never sees), so a token heuristic against it would classify any
// retry answer whose length lands between the two caps as a second cap hit —
// and book the block inert for what is an ordinary parse failure.
//
// Still only an approximation of the wire cap on a row that overrides
// num_predict in its model_map: that value is scaled by the same factor, so the
// ratio holds while the absolute number does not. Which is precisely why the
// token heuristic below runs ONLY where the provider reports no stop reason.
func effectiveCap(opts llm.Options) int {
	if opts.NumPredictScale <= 1 {
		return opts.NumPredict
	}
	return int(float64(opts.NumPredict) * opts.NumPredictScale)
}

// rowPinsMaxTokens reports whether the serving backend row carries a positive
// numeric extra_body.max_tokens — the one configuration a scaled NumPredict
// cannot outbid, because ExtraBody is merged over the marshalled body last
// (llm.applyOpenAIBodyExtras) and therefore wins on the wire.
//
// The two accepted shapes mirror llm's numericValue: a settings/JSONB round
// trip yields float64, a Go-built row int. Anything else (a string "4096", a
// nested object) is not a cap this check can reason about, so it does not
// suppress the retry — the retry is the conservative branch, the skip is the
// exception.
func rowPinsMaxTokens(b *backends.Backend) bool {
	if b == nil {
		return false
	}
	switch n := b.ExtraBody["max_tokens"].(type) {
	case float64:
		return n > 0
	case int:
		return n > 0
	default:
		return false
	}
}

// backendName is the log-safe name of the row that answered; served is nil on
// paths that never reached a backend.
func backendName(b *backends.Backend) string {
	if b == nil {
		return ""
	}
	return b.Name
}

// noteCandidatesCapped stamps the aggregate candidate cap's drop count onto a
// dream-eval entry, mirroring how links_capped records the MaxLinksPerCycle
// cap. Nothing is written when the cap did not bind — a metadata key that
// appears on every row cannot be counted, and 0 is the overwhelming majority.
//
// Split out for the same reason capHit is: the llmlog entry lives inside
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
// Split out for the reachability reason capHit documents: the llmlog entry
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

// capHit decides whether a parse failure is really an output-cap truncation,
// stamps the verdict onto the entry (cap_hit plus cap_hit_source, so the two
// signals stay distinguishable in the log) and reports it. Returns false and
// touches nothing for every other parse failure.
//
// Two signals, in strict precedence:
//
//  1. The PROVIDER's own stop reason. Since issue #26 the non-streaming path
//     decodes done_reason (Ollama) / choices[0].finish_reason (OpenAI), and
//     "length" is the cap hit stated outright. EqualFold because the value is
//     deliberately not normalised on the way in.
//  2. The token HEURISTIC, and ONLY where the provider reported nothing at
//     all: generation stopping at the budget while the JSON does not parse is
//     the cap hit. >= rather than ==, because some backends count the stop
//     token in. This half stays because plenty of OpenAI-compatible servers
//     report no stop reason — it is the floor, not the primary signal, and
//     cap_hit_source says which of the two spoke so an operator can see when
//     it becomes retirable.
//
// The heuristic is gated on an EMPTY stop reason rather than merely ranked
// below it: a provider that says "stop" has answered the question, and letting
// the token count overrule that would misclassify exactly the case the retry
// makes expensive — a malformed answer on the RETRY attempt, whose length
// naturally lands between the base and the scaled cap, would be read as a
// second cap hit and book the block inert (see effectiveCap).
//
// NumPredict <= 0 means uncapped — nothing for the heuristic to compare
// against, and unreachable from config: 0 on dream.num_predict is the
// package-default sentinel, so the options this sees always carry a positive
// cap. A stated "length" is honoured even then; the provider knows its own cap.
func capHit(entry *llmlog.Entry, resp *llm.ChatResponse, opts llm.Options) bool {
	if entry == nil || resp == nil {
		return false
	}
	var source string
	switch {
	case strings.EqualFold(resp.FinishReason, "length"):
		source = "finish_reason"
	case resp.FinishReason == "" && opts.NumPredict > 0 && resp.EvalCount >= effectiveCap(opts):
		source = "tokens"
	default:
		return false
	}
	if entry.Metadata == nil {
		entry.Metadata = map[string]any{}
	}
	entry.Metadata["cap_hit"] = true
	entry.Metadata["cap_hit_source"] = source
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
//     promptguard.GuardLine (ClampLine AND escape), not through the escape alone.
//
// The ids stay raw: they are DB uuids, not foreign text, and the answer path
// re-checks them against the candidate set anyway (filterValidCandidates) —
// the guard reduces the surface, that filter is the defence.
func buildEvalPrompt(source BlockInfo, candidates []BlockInfo) (system, user string) {
	nonce := promptguard.NewNonce()

	var b strings.Builder
	b.WriteString("<source>\n")
	fmt.Fprintf(&b, "ID: %s\nTitle: %s\nCategory: %s\nUpdated: %s\n",
		source.ID, promptguard.GuardLine(source.Title), promptguard.GuardLine(source.Category),
		source.UpdatedAt.Format("2006-01-02"))
	b.WriteString(promptguard.Wrap(nonce, "source",
		promptguard.GuardText(truncate(source.Content, MaxContentLen))))
	b.WriteString("\n</source>\n\n<candidates>\n")

	for _, c := range candidates {
		fmt.Fprintf(&b, "<block id=\"%s\" title=\"%s\" category=\"%s\" updated=\"%s\">\n",
			c.ID, promptguard.GuardLine(c.Title), promptguard.GuardLine(c.Category), c.UpdatedAt.Format("2006-01-02"))
		b.WriteString(promptguard.Wrap(nonce, "candidate",
			promptguard.GuardText(truncate(c.Content, MaxContentLen/2))))
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
