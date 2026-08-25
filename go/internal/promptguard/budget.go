package promptguard

import "errors"

// runesPerTokenNum/runesPerTokenDen express the rune→token ratio as an EXACT
// fraction (9/5 = 1.8 runes per token) so the budget arithmetic stays integer
// and reproducible across platforms.
//
// Measurement basis, not a guess: over context_llm_log rows with
// prompt_tokens > 0 (live instance, 2026-08-02) the chars-per-token MEAN per
// pipeline runs 2.53–3.24, and the MINIMUM over all pipelines is 1.80
// (dream-daily-synthesis / dream-temporal — German prose plus block titles,
// the densest tokenisation in the corpus). The budget deliberately uses the
// MINIMUM, never the mean: a budget computed from the mean over-estimates how
// much text a window holds by ~40-80 % exactly on the pipelines whose text is
// hardest to tokenise, which is the case it must not get wrong.
//
// The same conservatism is what pays for the completion: at the observed means
// a window holds 1.4-1.8x the runes this ratio predicts, so the head-room the
// pessimistic ratio leaves unused covers the generated answer. The generation
// side has its own bounds (Options.NumPredict, the role timeout) — this budget
// governs the PROMPT.
const (
	runesPerTokenNum = 9
	runesPerTokenDen = 5
)

// Rune budgets per foreign-text pipeline (design/04 §4.7). These are the
// pipelineConst half of `budget = min(pipelineConst, tokenBudget(chain))`:
// a ceiling the pipeline accepts regardless of how large a window the chain
// happens to offer, so a 262 144-token provider does not silently turn a
// 15-document judge prompt into a 400-document one.
//
// Each value is the pipeline's worst case (item cap x item count + rule +
// question) plus head-room for the code-generated markup around the items. The
// arithmetic is not documented here in prose — it is a TEST
// (TestPromptBudgetsCoverPipelineConstants), which goes red the day one of the
// item constants grows without its budget growing with it.
const (
	// BudgetSynthesis is query-synthesize: llm.MaxBlockChars x the query
	// handler's limit clamp, plus the caller's question.
	BudgetSynthesis = 40000
	// BudgetRerankJudge is query-rerank-judge: rrf.RerankContentLimit x
	// rrf.RerankMaxDocs, plus the query.
	BudgetRerankJudge = 16000
	// BudgetDreamEval is dream-eval: the source block plus
	// MaxCandidatesPerKeyword x MaxKeywords candidates at half the cap.
	BudgetDreamEval = 16000
	// BudgetClassifyAudit is sensitivity-audit: exactly ONE block at
	// llm.ClassifyContentLimit plus the code-generated audit question.
	BudgetClassifyAudit = 12000
	// BudgetDailyReport is dream-daily-synthesis: ten new-block titles plus the
	// aggregate sections. The section count is guarded separately — three of
	// its sections carry no SQL LIMIT and are bounded only by their vocabulary
	// (see TestDailyReportUnlimitedSectionCount).
	BudgetDailyReport = 12000
	// BudgetDistill is distill-insights: distill.rows_per_call rows at
	// distill.max_row_runes each, plus the code-generated wrapper markup.
	//
	// The one budget whose item cap and item count are CONFIG values, not Go
	// constants — and therefore the one whose gate does NOT live next to the
	// others in budget_gate_test.go: the F1 layering rule forbids importing
	// internal/config from this package, external test package included. Both
	// halves of the guard sit on the config side instead, reading this constant
	// and RuleReserve from here: TestDistillDefaultsFitPromptBudget covers the
	// compiled defaults, V24 (config/validate.go) refuses a settings write that
	// would outgrow this number.
	//
	// Derived, not set. 24 000 runes ≈ 13 333 prompt tokens at the pessimistic
	// 9/5 ratio above; at the default cap (4 000) and batch size (5) the
	// payload is 20 000 runes and the rest is rule + wrapper markup. Why not
	// larger, when the serving row offers a 262 144-token window: this is a
	// BACKGROUND, BATCHED pipeline, so its budget decides the CALL COUNT per
	// generation — and the call count, not the prompt size, is what the spend
	// guard watches. A bigger budget would lower the count but raise the
	// prefill time of each call, i.e. lengthen exactly the decode occupancy the
	// arm's quiet-gate exists to avoid. Small, frequent, interruptible calls
	// are the right grain for a background arm.
	BudgetDistill = 24000
)

// Reserves the budget gate charges for the two non-item parts of a prompt.
const (
	// RuleReserve is the rune cost of the nonce-bound security sentence
	// (Rule), rounded up. Measured, not estimated — pinned by
	// TestRuleReserveCoversRule.
	RuleReserve = 400
	// QuestionReserve is what the gate charges for the caller's question. The
	// query path does NOT cap the question text, so this is a policy number,
	// not a derived one: a question longer than this is itself the drift the
	// gate is meant to surface, and Assemble shortens it (PriorityQuestion)
	// rather than the rule.
	QuestionReserve = 4000
)

// ErrUndeclaredWindow is returned when a chain member carries no declared
// context window and no fallback is configured. Fail-closed by decision E10.
//
// Why a rate value would be WRONG rather than merely imprecise: the live pool
// carries an openrouter row with num_ctx = NULL. Asking the OpenRouter API for
// the routed model (GET /api/v1/models/:id/endpoints) returns PER-PROVIDER
// windows from 32 768 to 262 144 — and the model-level context_length is the
// MAXIMUM over those providers, not the minimum. A prompt sized against that
// number overflows on every provider below the top, and which provider serves
// a given request is not knowable at prompt-build time. So the honest options
// are exactly two: an operator-declared floor
// (pool.external_num_ctx_fallback / the row's own num_ctx), or refusal.
//
// Since E10-W2 there is a THIRD option for openrouter-class rows, and it is
// the one that answers the objection above rather than working around it:
// discover the per-provider windows (GET …/endpoints), plan against the best
// of them, and constrain the request to the providers that can actually hold
// it (provider.only). That resolution happens BEFORE this function — it hands
// the resolved window in as a normal windows entry. This error therefore keeps
// its exact meaning: no declared window, no discovered one, no operator floor
// — refuse.
var ErrUndeclaredWindow = errors.New("promptguard: chain member without a declared context window")

// RuneBudget converts a context window in TOKENS to a rune budget using the
// conservative ratio (see runesPerTokenNum). Non-positive input yields 0 —
// callers treat that as "no budget", never as "unlimited".
func RuneBudget(tokens int) int {
	if tokens <= 0 {
		return 0
	}
	return tokens * runesPerTokenNum / runesPerTokenDen
}

// TokenEstimate converts a rune count to a TOKEN count — the inverse
// direction of RuneBudget, over the SAME 9/5 fraction, and conservative for
// the same reason.
//
// Why one constant serves both directions without one of them becoming
// optimistic: 1.8 runes/token is the DENSEST tokenisation measured in the
// corpus (budget.go head), i.e. the worst case. Worst-case density is
// pessimistic whichever way it is read — forward it says "this window holds
// only 1.8 runes per token" (RuneBudget under-estimates what fits), backward
// it says "these runes may cost as much as one token per 1.8 of them"
// (TokenEstimate over-estimates what a prompt costs). At the observed means
// (2.53-3.24) the real prompt is 29-44 % cheaper than this estimate, so a
// provider admitted by TokenEstimate has head-room, never a deficit. An
// average-case constant would be the mistake in both directions at once.
//
// Rounds UP: a fractional token is a whole token on the wire, and rounding a
// size estimate down is how an overflow becomes a surprise. Non-positive
// input yields 0.
func TokenEstimate(runes int) int {
	if runes <= 0 {
		return 0
	}
	return (runes*runesPerTokenDen + runesPerTokenNum - 1) / runesPerTokenNum
}

// ChainRuneBudget resolves the prompt budget of a RESOLVED chain.
//
// The budget belongs to the CHAIN, not to the backend that happens to be first
// in it: every member is a backend this call may fail over to, and a failover
// leg is walked with the prompt that was already built. Sizing against the head
// of the chain would produce a prompt the second leg cannot hold — which is the
// leg that runs precisely when the first one is unavailable. Hence
// min() over ALL members, then min() with the pipeline ceiling.
//
// windows carries each member's declared window in tokens; <= 0 means the row
// declares none (SQL NULL scans to the zero value). fallbackTokens is the
// operator's declared floor for exactly those rows (settings key
// pool.external_num_ctx_fallback); <= 0 means unset, and an undeclared member
// then fails the pass with ErrUndeclaredWindow.
//
// An EMPTY chain fails the same way: no member means no evidence that any
// window can hold the prompt, and the caller has no chain to walk anyway.
func ChainRuneBudget(windows []int, fallbackTokens, pipelineCap int) (int, error) {
	if len(windows) == 0 {
		return 0, ErrUndeclaredWindow
	}
	budget := pipelineCap
	for _, w := range windows {
		if w <= 0 {
			if fallbackTokens <= 0 {
				return 0, ErrUndeclaredWindow
			}
			w = fallbackTokens
		}
		if b := RuneBudget(w); b < budget {
			budget = b
		}
	}
	if budget <= 0 {
		return 0, ErrUndeclaredWindow
	}
	return budget, nil
}
