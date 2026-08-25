package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// chatJSON is the test seam through which the dream pipeline calls the LLM.
// Test files install canned implementations (mockChatJSON in evaluate_test.go,
// SetChatJSONForTest in export_test.go) to inject ChatResponse values without
// touching the HTTP transport. The loose (host, apiKey, model, think)
// signature is deliberately stable across the F1 signature waves so the
// override closures in every dream test file keep compiling — the jsonMode
// parameter dreamChat gained deliberately did NOT widen it: an installed seam
// answers canned content, where the wire's JSON mode has no meaning. Every
// statement about that mode therefore belongs in a test with the seam
// UNINSTALLED (dream_protocol_test.go). nil outside tests — dreamChat then
// takes the production wire path, which is the only place the backend's
// protocol and the JSON mode are read.
var chatJSON func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error)

// dreamChat routes one dream LLM call. With the chatJSON test seam installed
// it forwards the backend's loose tuple there; in production it calls llm with
// the full backend, whose Protocol selects the wire path (/api/chat vs
// /v1/chat/completions). The dream.Protocol package var died here in F1-W6:
// the protocol now arrives only inside the backends.Backend parameter of the
// public dream entry points.
//
// jsonMode picks llm.ChatJSON (OpenAI response_format:{"type":"json_object"} /
// Ollama top-level "format":"json") over the plain llm.Chat. It is a PER-CALL
// parameter, not a property of the router: the four parsing stages and the
// daily synthesis disagree about it permanently — the synthesis prompt asks
// for prose and its answer is stored verbatim, so a grammar there can only
// corrupt the artefact, while the parsing stages want the constraint by
// default.
func dreamChat(ctx context.Context, chatB backends.Backend, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration, jsonMode bool) (*llm.ChatResponse, error) {
	if chatJSON != nil {
		return chatJSON(ctx, chatB.Host, chatB.APIKey, chatB.Model, chatB.Think.Ptr(), systemPrompt, userPrompt, opts, timeout)
	}
	if jsonMode {
		return llm.ChatJSON(ctx, chatB, systemPrompt, userPrompt, opts, timeout)
	}
	return llm.Chat(ctx, chatB, systemPrompt, userPrompt, opts, timeout)
}

// The two accepted values of config dream.json_mode. They live here, next to
// the resolver that reads them, because config validates AGAINST this package
// (the layering allows config → dream, never the reverse) — the same relation
// V16c has to KeywordsTimeout and V18 to DefaultNumPredict.
const (
	// JSONModeStrict sends the dialect's JSON-mode marker on the four parsing
	// stages. Today's behavior and the default.
	JSONModeStrict = "strict"
	// JSONModeOff sends plain chat there: the JSON contract stays in the
	// prompt and the local parsers are the only validator.
	JSONModeOff = "off"
)

// wantJSONMode resolves the wire policy of ONE parsing dream stage from the
// router, mirroring temporalTimeout: an unset router field (a router built
// without config wiring — every test, and any caller predating the key) reads
// as strict, so nothing changes without an explicit opt-out. Only the exact
// normalized "off" turns the marker off; config V20 rejects everything else
// long before it reaches here, and a hand-built router with a typo keeps the
// safe half of the pair.
//
// The nil check is parity with temporalTimeout, not a reachable state: chat is
// a method, so the receiver exists at every call site.
func wantJSONMode(r *Router) bool {
	if r == nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(r.JSONMode)) != JSONModeOff
}

const (
	// Version marks the Dream pipeline generation. Persisted with every link.
	// v1 = Pre-Reset (weighted-gate era, Pre-Reset-Audit 2026-04-20: 53% CORRECT at raw>=0.7).
	// v2 = Post-Reset Ideallinie: raw_confidence column, per-type raw thresholds
	//      (factual>=0.9), causal-temporal-check, topic-map-exclude, ApplySupersedes on raw.
	// v3 = Session 24 (2026-04-23): qwen3.6:27b + V5 prompt (topical-as-fallback,
	//      supersedes VERY RARE, causal accepts decision→implementation),
	//      factual threshold 0.9→0.7. Benchmark: 62.3% accuracy on stable gold (+19.7pp).
	// v4 = Session 25 (2026-05-04): hard-cap 5 with type-diversity tie-break,
	//      drift-format counter in metadata, prompt_tokens persistence,
	//      parseLinks tolerates object-form drift, replace-semantics with
	//      snapshot revert. Prompt remains V5 — V6 was attempted (commit 6b37662)
	//      but reverted: stable-gold n=122 showed V6 net-worse than V5 (54.1%
	//      vs 57.4%, topical-recall 76→63%). V7 prompt iteration is open work.
	// v5 = Welle 38b (2026-05-06): adds 'recurrent' relationship class detected
	//      by a separate Phase-1 (same dim+value in context_temporal AND
	//      title-similarity > 0.5) + Phase-2 (LLM-confirm) pass, run after the
	//      main evaluate so recurrent wins over topical for the same pair.
	//      Pre-Empirie audit-w38b-results.json: 27 Phase-1 candidates → 18
	//      RECURRENT + 2 SUPERSEDES + 7 NEITHER (74% precision). 'recurrent'
	//      raw_confidence floor is 0.8 (one quantisation step above the others).
	//      Welle 38a (massFactor in writelinks) was rejected by Pre-Empirie —
	//      target_q is flat across num_dates, no num_dates × quality damping.
	Version = 5

	// CooldownActiveDays is how long Dream waits for blocks that produced links (re-check sooner).
	CooldownActiveDays = 3
	// CooldownInertDays is how long Dream waits for blocks that produced no links.
	CooldownInertDays = 14
	// CooldownTransientMinutes is for transient system errors (LLM timeout, Ollama restart).
	// Short enough that a recovered GPU continues the pass promptly, long enough that a
	// persistent outage doesn't spin-retry every 20s.
	CooldownTransientMinutes = 5

	// CooldownKeywordFailMinutes is for blocks whose LLM keyword extraction
	// failed all retries. Unlike transient system errors, a block that
	// deterministically yields no keywords (content that makes the model
	// degenerate, e.g. heavy backslash paths — prod 2026-08-22, BRADES block)
	// would otherwise be re-picked every ~6 min and burn a full 3-attempt
	// keyword generation each time, stalling the whole dream cycle (workers=1).
	// 12h parks it until a model/content change makes a retry worthwhile.
	CooldownKeywordFailMinutes = 12 * 60

	// MaxKeywords is the keyword-count factor of the aggregate candidate cap
	// (MaxCandidatesPerKeyword*MaxKeywords, enforced in appendCandidates and
	// modelled by promptguard's dream-eval budget row). It does NOT bound the
	// keyword list itself: GenerateKeywords' prompt asks for 5 to 8 concepts
	// (keywords.go) and nothing truncates the result, so a cycle can run more
	// than MaxKeywords searches. Clamping the list is a separate decision —
	// it would drop the topics keywords 6-8 cover.
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
	ID       string
	Title    string
	Category string
	Content  string
	Scope    string
	// Sensitivity is the EFFECTIVE classification at the trust gate:
	// RunDreamCycle floors the stored value once after the pick; candidate
	// lookups floor per row. Zero value acts as credentials downstream
	// (backends.MaxSensitivity, fail-closed).
	Sensitivity              backends.Sensitivity
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

// CycleTimeoutOf resolves the whole-cycle deadline from the bare configured
// value: > 0 wins (config.Dream.CycleTimeout, hot), otherwise the package
// CycleTimeout constant (legacy behavior). 0 is the documented "package
// default" sentinel, mirroring temporalTimeout's contract; a negative value
// cannot reach a running cycle (config V16c is an ERROR on it) and falls back
// here as the second line of defence.
//
// This is the scalar form for callers that hold the duration but no router —
// internal/config's Validate, which is parameter-pure and has no business
// constructing a Router to read one number.
func CycleTimeoutOf(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return CycleTimeout
}

// pickClaimGrace is the head-room PickClaimTTL adds on top of the cycle
// deadline: the claim has to outlive the LAST statement of the cycle, not
// just the deadline itself — the tail bookkeeping (quality score, cooldown,
// canonical promotion) still runs after it.
const pickClaimGrace = 1 * time.Minute

// PickClaimTTL sizes the transient claim PickBlock writes onto the block it
// hands out, from the cycle's effective whole-cycle deadline.
//
// The claim is the ONLY thing keeping a sibling dream worker off an in-flight
// block (Welle-49): the row lock is gone the moment the pick statement
// returns. It was a bare CooldownTransientMinutes = 5 min, which was already
// 2.3x short of the 700s constant on root, and dream.cycle_timeout turns that
// mismatch into an operator-settable one — at the 2400s this PR documents,
// a 5-minute claim expires eight times inside a single cycle and the
// parallelism fix is a no-op for every cycle.
//
// So: cycle + grace, with the 5 minutes kept as a FLOOR so crash recovery of
// a very short cycle stays as prompt as it is today. Crash recovery of a long
// cycle degrades to cycle+1min, which is the honest bound anyway — a worker
// cannot hold a block longer than its own cycle context allows.
//
// CooldownTransientMinutes itself stays what it is for the explicit error
// paths (keyword generation exhausted, eval failure, temporal failure): those
// are post-cycle retry delays, not claims.
func PickClaimTTL(r *Router) time.Duration {
	if claim := CycleTimeoutFor(r) + pickClaimGrace; claim > CooldownTransientMinutes*time.Minute {
		return claim
	}
	return CooldownTransientMinutes * time.Minute
}

// CycleTimeoutFor is CycleTimeoutOf for callers that hold the cycle's router:
// the enclosing context.WithTimeout in RunDreamCycle and the scheduler's
// outer cycle context both read it, so one knob bounds the whole cycle and
// both contexts read the same field of the same struct. A nil router resolves
// to the package default.
func CycleTimeoutFor(r *Router) time.Duration {
	if r == nil {
		return CycleTimeout
	}
	return CycleTimeoutOf(r.CycleTimeout)
}

// Throttle is called between GPU-intensive steps to allow cooldown.
// Returns an error if the context was cancelled during the wait.
type Throttle func(ctx context.Context) error

// NoThrottle is a no-op throttle for full-speed mode.
func NoThrottle(_ context.Context) error { return nil }

// RunDreamCycle executes one dream cycle: pick → keywords → search → evaluate → link.
// Returns the number of links created, or 0 if no block was available.
// r routes every LLM/embed call through the backend pool (G28/F3-P4): the
// chains resolve per call with the involved blocks' effective sensitivity,
// so a pool mutation is live on the next call and the trust gate sits
// structurally before every prompt build. backoff is the only threading
// chain to the cooldown: SetDreamCooldown/SetDreamCooldownMinutes are called
// exclusively cycle-internally, never by the scheduler.
// Cyclop threshold exceeded by 1 due to sequential error/skip branches per pipeline step;
// extracting helpers would obscure the linear flow without reducing real complexity.
//
//nolint:cyclop // pipeline function with linear step sequence
func RunDreamCycle(ctx context.Context, pool *pgxpool.Pool, r *Router, opts llm.Options, backoff BackoffConfig, readScopes []string, throttle Throttle) (int, error) {
	// Step 0 (G28): cycle-skip BEFORE the block pick when no backend serves
	// the dream role at all (gaming toggle, disabled rows, missing role).
	// Returning (0, nil) sends the scheduler loop into its idle wait WITHOUT
	// claiming a block — the skip must not touch any block cooldown, or
	// gaming sessions would smear the back-off statistics (design 03 §2.4).
	if !r.available(backends.RoleDream) {
		slog.Info("dream: no eligible dream backend — cycle skipped before pick")
		return 0, nil
	}

	ctx, cancel := context.WithTimeout(ctx, CycleTimeoutFor(r))
	defer cancel()

	// ONE policy snapshot per cycle (WF T8, blocktype doctrine): pick
	// eligibility, the candidate sieve (searchByKeywords) and the WriteLinks
	// target/link-class gates all read the same tenant-resolved generation.
	// nil registry = wiring bug, fail loudly — never silently builtin.
	typeSet := r.TypeSet(ctx)
	if typeSet == nil {
		return 0, fmt.Errorf("dream: no block-type registry wired (Router.Blocktypes nil)")
	}

	// Step 1: Pick a block — scoped to the iteration's entitlement-clamped read
	// window (WF T12): the DreamLinkableTypes allowlist above is this tenant's
	// resolved generation (Router.TypeSet → SnapshotForTenant), so the pick MUST
	// be bound to the tenant's own scopes or A's policy would govern B's blocks
	// (design/01 §7-T12). readScopes is read_scopes ∩ owned; single-tenant it is
	// the raw read window, so the pick set is byte-identical to pre-T12 there.
	block, err := PickBlock(ctx, pool, typeSet.DreamLinkableTypes(), readScopes, PickClaimTTL(r))
	if err != nil {
		return 0, fmt.Errorf("dream: pick: %w", err)
	}
	if block == nil {
		return 0, nil // No eligible blocks.
	}

	// Effective sensitivity once per cycle: stored class + scope floor. Every
	// downstream consumer (keywords, temporal, eval max-fold, keyword-embed)
	// reads the floored value from the BlockInfo.
	block.Sensitivity = r.FloorSens(block.Sensitivity, block.Scope)

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
		if err := ValidateTemporal(ctx, pool, r, opts, block); err != nil {
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
		generated, genErr := GenerateKeywords(ctx, pool, r, block)
		if genErr != nil {
			slog.Warn("dream: keyword generation failed (LLM + deterministic fallback), parking block",
				"block_id", block.ID, "cooldown_minutes", CooldownKeywordFailMinutes, "error", genErr)
			// Not the 5-min transient cooldown: a block that deterministically
			// fails keyword extraction (degenerate model output on its content)
			// would re-pick every ~6 min and stall the cycle. Park it 12h.
			_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownKeywordFailMinutes)
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
	candidates, capped, err := searchByKeywords(ctx, pool, r, typeSet, keywords, readScopes, block)
	if err != nil {
		slog.Warn("dream: keyword search failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownTransientMinutes)
		return 0, err
	}

	if len(candidates) == 0 {
		slog.Info("dream: no candidates found", "block_id", block.ID)
		_ = SetDreamCooldown(ctx, pool, block.ID, true /* inert: no candidates */, backoff)
		return 0, nil
	}

	// candidate_cap_hit / candidates_capped make a bound that FIRED countable
	// instead of inferable. It is not a pure cost saving: the dropped hits are
	// candidates the eval never sees, and a cycle whose only link would have
	// come from one of them writes zero links and is booked inert below
	// (SetDreamCooldown, up to dream.backoff_cap days). The same count reaches
	// context_llm_log on the dream-eval row (evaluateRelationships).
	slog.Info("dream: candidates found",
		"block_id", block.ID,
		"candidate_count", len(candidates),
		"candidate_cap_hit", capped > 0,
		"candidates_capped", capped,
	)

	// Throttle: after embed (keywords) → before LLM (evaluation).
	if err := throttle(ctx); err != nil {
		return 0, err
	}

	// Step 4: LLM evaluation.
	links, err := evaluateRelationships(ctx, pool, r, opts, *block, candidates, capped)
	if err != nil {
		// Preempt semantics pin (MW19, design/02 §4.6 dream-eval row): a
		// dispatcher preempt is a scheduling decision, not a quality signal
		// about the block — it takes the SAME transient cooldown as any other
		// transient failure and NEVER SetDreamCooldown (no dream_eval_count
		// advance, P-N6). The distinction travels EXCLUSIVELY on the returned
		// error chain via errors.Is (wrap contract §4.2.6); context.Cause
		// stays dispatcher-internal. Log-only branch: an INFO instead of a
		// WARN, because nothing failed — the block retries in ≥5 min with its
		// persisted keywords, the next cycle picks the next block.
		if errors.Is(err, dispatch.ErrPreempted) {
			slog.Info("dream: evaluation preempted by interactive demand — transient cooldown, no back-off advance",
				"block_id", block.ID)
		} else if errors.Is(err, ErrOutputCapHit) {
			// The answer was truncated at the output cap AND again on the
			// bounded retry at r.CapRetryFactor times the resolved cap
			// (evaluateRelationships escalates only after the retry was
			// actually spent). The transient path is the wrong home for that:
			// it has no attempt counter, so the block would re-burn one eval
			// call every five minutes on a prompt that will not fit. Book it
			// as a completed-but-inert eval instead — dream_eval_count + 1,
			// dream_last_inert, InertOffset on the back-off curve — the same
			// booking a "nothing relates" verdict gets. The block is not
			// abandoned: it re-dreams later on the curve, and after the cap or
			// the factor is raised the next cycle links it normally.
			slog.Warn("dream: eval hit the output cap twice — booking a completed-but-inert eval",
				"block_id", block.ID, "num_predict", opts.NumPredict, "factor", r.CapRetryFactor)
			_ = SetDreamCooldown(ctx, pool, block.ID, true /* inert: cap hit twice */, backoff)
			return 0, err
		} else {
			slog.Warn("dream: evaluation failed", "block_id", block.ID, "error", err)
		}
		_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownTransientMinutes)
		return 0, err
	}

	// Step 5: Write links.
	written, err := WriteLinks(ctx, pool, typeSet, block.ID, block.Scope, block.QualityScore, links)
	if err != nil {
		slog.Warn("dream: write links failed", "block_id", block.ID, "error", err)
	}

	// Step 5b (Welle 38b, v5): detect recurrent pairs via temporal+title overlap and confirm
	// per-pair via LLM. Runs AFTER main eval so 'recurrent' overwrites a 'topical' that
	// EvaluateRelationships may have written for the same pair — recurrent is the more
	// specific classification. Non-fatal on error: dream cycle continues.
	//
	// Recurrence exception (MW19, design/02 §4.6 dream-recurrence row —
	// deliberate Tag-1 decision, not a gap): a preempted pair call inside
	// DetectRecurrence is a per-pair non-fatal skip, the cycle completes and
	// step 7 advances the back-off REGULARLY (dream_eval_count+1, full W49c
	// curve) — unlike a preempted MAIN eval above, which retries in 5 min
	// without an advance. The lost verdict is a recall delay on the
	// recurrence edge only; refinement (transient cooldown instead) is owned
	// by axis 05, the error-chain signature lies ready.
	recurrentLinks, recErr := DetectRecurrence(ctx, pool, r, opts, *block)
	if recErr != nil {
		slog.Warn("dream: recurrence detection failed (non-fatal)", "block_id", block.ID, "error", recErr)
	} else if len(recurrentLinks) > 0 {
		rWritten, rwErr := WriteLinks(ctx, pool, typeSet, block.ID, block.Scope, block.QualityScore, recurrentLinks)
		if rwErr != nil {
			slog.Warn("dream: recurrent write failed (non-fatal)", "block_id", block.ID, "error", rwErr)
		} else {
			slog.Info("dream: recurrent links written",
				"block_id", block.ID, "candidates", len(recurrentLinks), "written", rWritten)
			written += rWritten
			links = append(links, recurrentLinks...)
		}
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

	// Step 7: Set adaptive back-off cooldown. inert = no links written this cycle,
	// which starts the block further up the curve (backoff.InertOffset).
	_ = SetDreamCooldown(ctx, pool, block.ID, written == 0, backoff)

	// Step 8: Promote eligible blocks to canonical.
	// Welle 46 Convention-Switch (2026-05-22): under "A supersedes B" → A=source=newer,
	// the SOURCE (current dream block) is the authoritative replacement. If the
	// cycle wrote at least one supersedes-link, the source is the promotion candidate
	// (quality boosted in step 6, no inbound supersedes). Pre-Welle-46 logic promoted
	// targets — under the old convention they were the newer block. With the
	// convention reversed and only the source side being the dream-block-of-cycle,
	// the promote check now runs once on block.ID.
	if written > 0 {
		hasSupersedes := false
		for _, link := range links {
			if link.Relationship == "supersedes" {
				hasSupersedes = true
				break
			}
		}
		if hasSupersedes {
			if promoted, err := PromoteToCanonical(ctx, pool, block.ID); err != nil {
				slog.Debug("dream: promote check failed", "block_id", block.ID, "error", err)
			} else if promoted {
				slog.Info("dream: promoted to canonical", "block_id", block.ID)
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
// quality_score >= 0.8, no inbound supersedes, lifecycle_state = 'knowledge'.
//
// Welle 46 Convention-Switch (2026-05-22): under the English convention
// "A supersedes B" → A=source=newer, B=target=outdated. A block is OUTDATED
// (and must not become canonical) when it appears as the TARGET of a
// supersedes-link — something newer (the source) has replaced it. The NOT
// EXISTS clause was inverted from source_block_id=$1 to target_block_id=$1.
// Returns true if the block was promoted.
func PromoteToCanonical(ctx context.Context, pool *pgxpool.Pool, blockID string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE context_blocks SET lifecycle_state = 'canonical'
		WHERE id = $1
		  AND NOT is_archived
		  AND quality_score >= 0.8
		  AND lifecycle_state = 'knowledge'
		  AND NOT EXISTS (
			SELECT 1 FROM context_dream_links
			WHERE target_block_id = $1 AND relationship = 'supersedes'
		  )`,
		blockID,
	)
	if err != nil {
		return false, fmt.Errorf("dream: promote: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// dreamEligibleWhere is THE single dream-eligibility predicate (WF T8,
// design/01 §7-T8): PickBlock, Stats, QueueDepth and ComputeBackoffStats
// consume this one fragment instead of the former five NOT-is_meta mirrors.
// typesParam binds the DreamLinkableTypes allowlist (dream.linkable=true
// types; the system-meta seed carries linkable=false — the generalization of
// the retired is_meta column, whose Post-Reset-Audit 2026-04-20 showed meta
// blocks cause 85% of NO_REL noise without valid relationships). typesParam
// is a code-owned bind placeholder at every call site, never user input.
// Embedding/cooldown/checked/scope conjuncts stay call-site-specific.
func dreamEligibleWhere(typesParam string) string {
	// 'synthesis' (daily reports, synthesize_report.go) dreams since 2026-07-14:
	// the ego graph renders context_dream_links only, so synthesis blocks were
	// edge-less islands (66 reports, zero out-edges). Their type audit-trail is
	// dream.linkable=true; the lifecycle conjunct was the sole blocker.
	// 'snapshot' stays excluded — superseded content must not grow new edges.
	return `NOT is_archived
		  AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis')
		  AND type_name = ANY(` + typesParam + `::text[])`
}

// PickBlock selects the next block for Dream processing.
// Priority: unchecked blocks first, then oldest-checked with expired cooldown.
// linkable is the DreamLinkableTypes allowlist of the cycle's policy snapshot
// (WF T8) — an empty list is legitimate policy ("no type dreams") and yields
// no pick, fail-closed.
//
// Atomic claim-with-TTL (Welle-49, parallelism-bug-fix): a plain
// pool.QueryRow(... FOR UPDATE SKIP LOCKED) releases the row lock as soon as
// the statement returns, so a multi-second RunDreamCycle that followed had no
// protection from a sibling worker picking the same block. Fix: UPDATE … FROM
// subquery sets a transient cooldown atomically with the pick, so other
// workers see the cooldown active and SKIP. On successful cycle completion the
// caller overrides with the real outcome cooldown (CooldownActiveDays /
// CooldownInertDays). On crash the claim expires and the block re-enters the
// queue.
//
// claim is how long that TTL runs, rounded up to whole seconds. It MUST cover
// the caller's whole cycle — a claim shorter than the cycle expires while the
// worker still holds the block, and any sibling worker (dream.parallelism, up
// to 16) then legitimately re-picks it, re-runs the whole LLM pipeline on it,
// and both cycles write links/quality-score/cooldown for the same block
// version. RunDreamCycle sizes it with PickClaimTTL; see there for why a bare
// CooldownTransientMinutes cannot be the answer once the cycle deadline is an
// operator-settable knob.
// scopes (WF T12) is the iterating tenant's ENTITLEMENT-CLAMPED read window
// (read_scopes ∩ owned) — the pick is bound to `scope = ANY(scopes)` so a
// tenant's DreamLinkableTypes allowlist is applied ONLY to that tenant's blocks.
// Without it the just-iterated tenant's policy would decide FOREIGN tenants'
// blocks (design/01 §7-T12/§9.6: A's dream.linkable=false for type X would
// wrongly skip B's X-blocks in A's cycle). An empty scopes list adds NO
// conjunct — the tenant-less single pass (backgroundTenants fail-safe
// {_global, nil} → raw read_scopes... but a caller may pass nil) stays
// byte-identical to the pre-T12 unrestricted pick.
func PickBlock(ctx context.Context, pool *pgxpool.Pool, linkable []string, scopes []string, claim time.Duration) (*BlockInfo, error) {
	var block BlockInfo
	scopeConjunct := ""
	// Whole seconds, rounded UP: the claim must never come out shorter than
	// the cycle it protects. The unit moved from minutes to seconds with the
	// claim itself — a cycle deadline is a seconds-grained knob.
	args := []any{int(math.Ceil(claim.Seconds())), linkable}
	if len(scopes) > 0 {
		scopeConjunct = "\n\t\t\t  AND scope = ANY($3::text[])"
		args = append(args, scopes)
	}
	err := pool.QueryRow(ctx,
		`UPDATE context_blocks
		SET dream_cooldown_until = now() + ($1 * interval '1 second')
		WHERE id = (
			SELECT id FROM context_blocks
			WHERE `+dreamEligibleWhere("$2")+`
			  AND embedding IS NOT NULL
			  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())`+scopeConjunct+`
			ORDER BY dream_checked_at ASC NULLS FIRST, quality_score ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, title, category, content, scope, sensitivity, quality_score, updated_at, created_at, dream_keywords, dream_temporal_validated_at`,
		args...,
	).Scan(&block.ID, &block.Title, &block.Category, &block.Content, &block.Scope, &block.Sensitivity, &block.QualityScore, &block.UpdatedAt, &block.CreatedAt, &block.DreamKeywords, &block.DreamTemporalValidatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick block: %w", err)
	}
	return &block, nil
}

// BackoffConfig is the Dream re-dream back-off policy (Wave-2, W49c;
// recalibrated W49c-2): the effective re-dream interval grows with a block's
// completed eval count (dream_eval_count), so mature blocks are pulled
// progressively less often while fresh blocks re-dream quickly to catch links
// to newly-inserted related blocks. The curve is in HOURS from a small minimum
// (default 12h) — early dreams are cheap and quality rises fastest then, so
// there is NO grace plateau (grace=0): growth starts at n=0.
//
//	exp:    MinHours * Factor^max(0, n-Grace+off)        [default — 12h→cap by n~10]
//	log:    MinHours * (1 + Factor*ln(1 + max(0,n-Grace+off)))
//	linear: MinHours + Factor*24*max(0, n-Grace+off)
//	off:    CooldownActiveDays / CooldownInertDays (days) [back-off disabled]
//
// n = dream_eval_count BEFORE this increment (so a block dreamed once gets the
// n=1 step, a fresh block n=0). off = InertOffset when the cycle produced NO
// links (inert) — an inert block starts further up the SAME curve instead of
// from a separate base. Result floored at 1h and capped at CapHours. Back-off
// only DELAYS a re-dream, never stops it, so a link missed in a skipped cycle
// is recovered on the next accepted pull (no permanent recall loss). Defaults
// (exp/1.6/min12h/grace0/cap45/inert+7) validated on real pull history
// (.project/bench-session-w49c/backoff_curves.py): ~48% of dream-eval GPU
// saved at lower link-loss than the prior log/3d curve, with fresh blocks
// re-dreaming sub-day.
//
// The policy travels as a parameter from the caller's config snapshot
// (config.DreamBackoff) — F1-W6 deleted the 6 package vars + SetBackoffConfig,
// so a settings flip is live on the next cycle and /api/manage dream-stats
// render the same generation the scheduler runs. Invalid values never reach
// here: config validation V6 resets them to the registry defaults (the old
// SetBackoffConfig ignore-semantics, pulled forward to boot).
type BackoffConfig struct {
	Mode        string  // exp|log|linear|off
	Factor      float64 // growth (exp) / k (log) / slope-per-day (linear)
	Grace       int     // free cycles before growth (0 = grow from n=0)
	MinHours    float64 // floor: cooldown at n=0 (active, hours)
	CapHours    float64 // ceiling (hours)
	InertOffset int     // extra curve steps when a cycle finds no links
}

// effectiveCooldownHours returns the back-off cooldown in HOURS for a block at
// the given pre-increment eval count n. inert applies the inert-offset (no
// links found this cycle). It is the Go mirror of the SQL in SetDreamCooldown
// — keep the two in sync. Used by ComputeBackoffStats and tests.
func (bc BackoffConfig) effectiveCooldownHours(n int, inert bool) float64 {
	if bc.Mode == "off" {
		if inert {
			return float64(CooldownInertDays) * 24
		}
		return float64(CooldownActiveDays) * 24
	}
	x := float64(n - bc.Grace)
	if inert {
		x += float64(bc.InertOffset)
	}
	if x < 0 {
		x = 0
	}
	h := bc.MinHours
	switch bc.Mode {
	case "exp":
		h = bc.MinHours * math.Pow(bc.Factor, x)
	case "log":
		h = bc.MinHours * (1 + bc.Factor*math.Log(1+x))
	case "linear":
		h = bc.MinHours + bc.Factor*24*x
	}
	if h > bc.CapHours {
		h = bc.CapHours
	}
	if h < 1 {
		h = 1
	}
	return h
}

// BackoffStats is the current back-off policy plus the corpus maturity
// distribution: one entry per occupied dream_eval_count level, with the effective
// re-dream cooldown that level now yields (at the active-links base,
// CooldownActiveDays). Lets operators see how far the corpus has cooled off and
// the actual per-level growth of the re-dream interval.
type BackoffStats struct {
	Mode        string         `json:"mode"`           // exp|log|linear|off
	Factor      float64        `json:"factor"`         // growth (exp) / k (log) / slope-per-day (linear)
	Grace       int            `json:"grace"`          // free cycles before growth (0 = grow from n=0)
	MinHours    float64        `json:"min_hours"`      // floor: cooldown at n=0 (active)
	CapHours    float64        `json:"cap_hours"`      // ceiling (hours)
	InertOffset int            `json:"inert_offset"`   // extra curve steps when a cycle finds no links
	MaxEval     int            `json:"max_eval_count"` // highest dream_eval_count in the corpus (growth cut-off)
	Truncated   bool           `json:"truncated"`      // level list capped at maxBackoffLevels
	Levels      []BackoffLevel `json:"levels"`         // per eval-count, ascending up to MaxEval
}

// BackoffLevel is one dream_eval_count value of the maturity distribution.
type BackoffLevel struct {
	EvalCount int     `json:"eval_count"`     // completed eval cycles
	Blocks    int     `json:"blocks"`         // eligible blocks at this exact count
	CooldownH float64 `json:"cooldown_hours"` // effective cooldown this count yields (active, hours)
}

// maxBackoffLevels caps the per-level distribution so a pathologically old corpus
// cannot emit thousands of rows. 100 covers years of back-off growth in practice.
const maxBackoffLevels = 100

// ComputeBackoffStats reports the given back-off policy and a per-eval-count
// maturity distribution: how many eligible blocks sit at each completed-cycle
// count and the effective re-dream cooldown that count yields (active base). One
// row per occupied level, ascending, up to the corpus max (the growth cut-off —
// beyond it no blocks exist), capped at maxBackoffLevels rows. The per-level
// cooldown uses the exact count (not a bucket edge), so the growth is visible and
// the final row shows the true maximum, not a floor. bc comes from the caller's
// request snapshot (ManageHandler dream-stats), so the rendered policy is the
// generation currently in effect, not a boot copy.
func ComputeBackoffStats(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string, bc BackoffConfig) (*BackoffStats, error) {
	out := &BackoffStats{
		Mode: bc.Mode, Factor: bc.Factor, Grace: bc.Grace, CapHours: bc.CapHours,
		MinHours: bc.MinHours, InertOffset: bc.InertOffset,
		Levels: make([]BackoffLevel, 0, 32),
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(dream_eval_count), 0)::int FROM context_blocks
		 WHERE `+dreamEligibleWhere("$2")+`
		   AND embedding IS NOT NULL AND scope = ANY($1::text[])`,
		scopes, linkable).Scan(&out.MaxEval); err != nil {
		return nil, fmt.Errorf("dream: backoff max eval: %w", err)
	}
	rows, err := pool.Query(ctx,
		`SELECT dream_eval_count::int, count(*)::int FROM context_blocks
		 WHERE `+dreamEligibleWhere("$2")+`
		   AND embedding IS NOT NULL AND scope = ANY($1::text[])
		 GROUP BY dream_eval_count ORDER BY dream_eval_count
		 LIMIT $3`,
		scopes, linkable, maxBackoffLevels+1)
	if err != nil {
		return nil, fmt.Errorf("dream: backoff stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Levels) >= maxBackoffLevels {
			out.Truncated = true
			break
		}
		var n, blocks int
		if err := rows.Scan(&n, &blocks); err != nil {
			return nil, fmt.Errorf("dream: backoff scan: %w", err)
		}
		out.Levels = append(out.Levels, BackoffLevel{
			EvalCount: n,
			Blocks:    blocks,
			CooldownH: bc.effectiveCooldownHours(n, false),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dream: backoff rows: %w", err)
	}
	return out, nil
}

// RestampBackoff re-evaluates every existing cooldown stamp under the given
// policy: dream_cooldown_until becomes dream_checked_at + curve(dream_eval_count,
// dream_last_inert) — the same CASE as SetDreamCooldown, but WITHOUT the
// count increment and anchored at the stored checked_at instead of now().
// This is the immediacy half of a settings change: the policy itself is hot
// for FUTURE cycles, but stamps written under the old curve would otherwise
// govern the corpus for up to the old cap. Called by the manage action
// dream-backoff-restamp right after a dream.backoff_* save.
//
// Transient claim stamps (an in-flight worker claim or a transient-error
// retry, both minutes-scale) are recognized by their sub-hour span — the
// curve's floor is 1h, SetDreamCooldownMinutes writes 5m — and left alone:
// restamping a live claim would let a sibling worker pick the same block.
// A new stamp landing in the past simply makes the block eligible now; the
// scheduler pulls it on its normal cadence.
func RestampBackoff(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string, bc BackoffConfig) (restamped, skippedTransient int64, err error) {
	off := bc.InertOffset
	row := pool.QueryRow(ctx,
		`WITH eligible AS (
			SELECT id, (dream_cooldown_until - dream_checked_at) < interval '1 hour' AS transient
			FROM context_blocks
			WHERE dream_checked_at IS NOT NULL
			  AND dream_cooldown_until IS NOT NULL
			  AND `+dreamEligibleWhere("$2")+`
			  AND scope = ANY($1::text[])
		), upd AS (
			UPDATE context_blocks b SET
				dream_cooldown_until = b.dream_checked_at + GREATEST(1.0, LEAST($6::float8,
					CASE $7::text
						WHEN 'exp'    THEN $3::float8 * power($4::float8, GREATEST(0, b.dream_eval_count - $5 + CASE WHEN b.dream_last_inert THEN $8 ELSE 0 END)::float8)
						WHEN 'log'    THEN $3::float8 * (1 + $4::float8 * ln(1 + GREATEST(0, b.dream_eval_count - $5 + CASE WHEN b.dream_last_inert THEN $8 ELSE 0 END)::float8))
						WHEN 'linear' THEN $3::float8 + $4::float8 * 24.0 * GREATEST(0, b.dream_eval_count - $5 + CASE WHEN b.dream_last_inert THEN $8 ELSE 0 END)::float8
						ELSE (CASE WHEN b.dream_last_inert THEN $9::float8 ELSE $10::float8 END) * 24.0
					END
				)) * interval '1 hour'
			FROM eligible e
			WHERE b.id = e.id AND NOT e.transient
			RETURNING b.id
		)
		SELECT (SELECT count(*) FROM upd), (SELECT count(*) FROM eligible WHERE transient)`,
		scopes, linkable, bc.MinHours, bc.Factor, bc.Grace, bc.CapHours, bc.Mode,
		off, float64(CooldownInertDays), float64(CooldownActiveDays),
	)
	if err := row.Scan(&restamped, &skippedTransient); err != nil {
		return 0, 0, fmt.Errorf("dream: backoff restamp: %w", err)
	}
	return restamped, skippedTransient, nil
}

// SetDreamCooldown marks a block as dream-checked, increments its completed-cycle
// counter, and sets the back-off cooldown (in hours) derived from the new count.
// inert=true (no links found this cycle) applies the inert offset so the block
// starts further up the curve. The CASE mirrors effectiveCooldownHours exactly.
// x = (dream_eval_count + 1) - grace + (inert ? inertOffset : 0). For transient
// system failures use SetDreamCooldownMinutes instead — that path does NOT
// increment the counter, so the back-off reflects real completed work only.
// bc threads in from RunDreamCycle (cycle-internal callers only — the policy
// values become SQL parameters of this one statement).
func SetDreamCooldown(ctx context.Context, pool *pgxpool.Pool, blockID string, inert bool, bc BackoffConfig) error {
	off := 0
	if inert {
		off = bc.InertOffset
	}
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			dream_eval_count = dream_eval_count + 1,
			dream_checked_at = now(),
			dream_last_inert = $8::bool,
			dream_cooldown_until = now() + GREATEST(1.0, LEAST($5::float8,
				CASE $6::text
					WHEN 'exp'    THEN $2::float8 * power($3::float8, GREATEST(0, (dream_eval_count + 1) - $4 + $7)::float8)
					WHEN 'log'    THEN $2::float8 * (1 + $3::float8 * ln(1 + GREATEST(0, (dream_eval_count + 1) - $4 + $7)::float8))
					WHEN 'linear' THEN $2::float8 + $3::float8 * 24.0 * GREATEST(0, (dream_eval_count + 1) - $4 + $7)::float8
					ELSE (CASE WHEN $8::bool THEN $9::float8 ELSE $10::float8 END) * 24.0
				END
			)) * interval '1 hour'
		WHERE id = $1`,
		blockID, bc.MinHours, bc.Factor, bc.Grace, bc.CapHours, bc.Mode,
		off, inert, float64(CooldownInertDays), float64(CooldownActiveDays),
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
// and returns candidate blocks (excluding the source block and cross-scope
// blocks), each annotated with its floor-adjusted sensitivity for the eval
// gate. The keyword embeds chain over the pool (role dream-embed when
// configured, embed otherwise) at the SOURCE block's sensitivity — the
// keywords derive from its content (design 03 §2.2, call-site #9). One slim
// llmlog row per wire call; cache hits are no egress and log nothing.
//
// The second return value is how many retrieved candidates the aggregate cap
// dropped (see appendCandidates) — 0 on every cycle the cap did not bind. It
// is telemetry, not an error signal: the caller stamps it onto the dream-eval
// llmlog row so a cap that fires is countable in context_llm_log instead of
// silently shortening the eval input.
func searchByKeywords(ctx context.Context, pool *pgxpool.Pool, r *Router, typeSet *blocktype.Set, keywords []string, scopes []string, source *BlockInfo) ([]BlockInfo, int, error) {
	seen := make(map[string]bool)
	seen[source.ID] = true // Exclude source block.
	var candidates []BlockInfo
	capped := 0
	embedFailures := 0

	chain, embedRole, err := r.EmbedChain(source.Sensitivity)
	if err != nil {
		// Trust/gaming-empty chain: no keyword can embed this cycle — the
		// caller applies the transient cooldown (never escalate across the
		// trust border).
		return nil, 0, fmt.Errorf("dream: keyword embed chain: %w", err)
	}

	// WF T5 (design/01 §4.4 #4) / T8: typeSet is the cycle's ONE
	// tenant-resolved policy snapshot (threaded from RunDreamCycle) — NEVER
	// the compiled-in builtin set. nil = wiring bug, fail loudly (an rrf
	// call with an empty allowlist would reject anyway).
	if typeSet == nil {
		return nil, 0, fmt.Errorf("dream: no block-type policy set (registry not wired)")
	}
	visibleTypes := typeSet.VisibleTypes()

	for _, kw := range keywords {
		// Embed the keyword for semantic search. Cached by (hash(prefix||kw), model) —
		// Dream keywords repeat heavily across cycles (domain vocabulary, proper nouns).
		eadm := r.EmbedAdmit()
		kwEmbedding, served, attempts, wired, err := embedcache.EmbedChain(
			ctx, pool, chain, embedRole, kw, embed.PrefixQuery,
			embedcache.ReportFunc(r.Report), eadm)
		if wired {
			// "" → NULL: dream keyword-embed is background, no caller (T35b).
			// MW11: duration/queue_wait derive from the attempts (§4.4a).
			llm.LogEmbedWire(pool, "dream-keyword-embed", embedRole, source.Sensitivity,
				served, attempts, []string{source.ID}, err, "", eadm.Class)
		} else if err != nil {
			// K9 rejection line (MW11, design/05 §3.2): a never-admitted
			// background acquire persists its futile wait; every other
			// no-wire failure is filtered inside RecordRejection (§4.3).
			llm.RecordRejection(pool, "dream-keyword-embed", err, eadm.Class)
		}
		if err != nil {
			embedFailures++
			slog.Debug("dream: embed keyword failed", "keyword", kw, "error", err)
			// Fail fast if majority of embeddings fail (Ollama likely down).
			if embedFailures > len(keywords)/2 {
				return nil, 0, fmt.Errorf("dream: too many embedding failures (%d/%d)", embedFailures, len(keywords))
			}
			continue
		}

		// RRF search with keyword as query. WF T5: visibility allowlist from
		// the registry snapshot; damping arrays EMPTY (no lift needed — the
		// dream-cycle keyword search wants the full retrieval pool at factor
		// 1.0, exactly the pre-T5 audit-trail-factor 1.0 semantics; user-query
		// pattern-aware damping stays handler-layer only).
		// v2.0.0 C2 (M048): no exclude-lists for dream — dream sees every
		// visible type. T40b: nil grant set — dream-cycle is an internal,
		// scope-only retrieval pass (no per-tenant grantee identity), so the
		// block-grant OR-arm is a no-op here.
		results, _, err := rrf.Search(ctx, pool, kwEmbedding, kw, kw, scopes, nil, nil, MaxCandidatesPerKeyword, "", "", visibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
		if err != nil {
			slog.Debug("dream: rrf search failed", "keyword", kw, "error", err)
			continue
		}

		var dropped int
		candidates, dropped = appendCandidates(candidates, seen, results, source.Scope)
		capped += dropped

		// Cap total candidates.
		if len(candidates) >= MaxCandidatesPerKeyword*MaxKeywords {
			break
		}
	}

	// Type-policy candidate sieve + sensitivity annotation in ONE batch query
	// (WF T8, design/01 §4.4 #21): the PK lookup reads type_name instead of
	// the retired is_meta column; a candidate whose type is not dream-linkable
	// — or not registered at all (fail-closed, §5.1) — is sieved out.
	// dream.linkable acts on BOTH sides (§3.3 R1): the system-meta seed keeps
	// the empirical meta-noise exclusion (Post-Reset-Audit 2026-04-20: 85% of
	// NO_REL noise), and any other linkable=false type is now sieved too.
	// The same lookup carries each candidate's stored sensitivity for the
	// eval gate (max-fold in EvaluateRelationships); a candidate missing from
	// the lookup (deleted between RRF and here) keeps the zero value, which
	// acts as credentials downstream (fail-closed).
	if len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, c.ID)
		}
		type rowInfo struct {
			typeName string
			sens     backends.Sensitivity
		}
		rows, err := pool.Query(ctx,
			`SELECT id::text, type_name, sensitivity, scope FROM context_blocks WHERE id = ANY($1::uuid[])`,
			ids,
		)
		if err == nil {
			info := make(map[string]rowInfo, len(ids))
			for rows.Next() {
				var id, typeName, sens, scope string
				if err := rows.Scan(&id, &typeName, &sens, &scope); err == nil {
					info[id] = rowInfo{typeName: typeName, sens: r.FloorSens(backends.Sensitivity(sens), scope)}
				}
			}
			rows.Close()
			filtered := candidates[:0]
			for _, c := range candidates {
				ri, ok := info[c.ID]
				if ok {
					if p, known := typeSet.Resolve(ri.typeName); !known || !p.Dream.Linkable {
						continue
					}
				}
				c.Sensitivity = ri.sens // zero value on lookup miss = credentials
				filtered = append(filtered, c)
			}
			candidates = filtered
		}
	}

	return candidates, capped, nil
}

// appendCandidates folds one keyword's RRF batch into the running candidate
// set and returns the grown slice plus the number of hits the aggregate cap
// dropped. Pure by construction — no pool, no router, no I/O — so the whole
// cap/dedup/filter policy of the keyword loop is table-testable without a
// database (candidatecap_test.go).
//
// The drop count is the honest one: only results that passed every filter and
// would have been appended are counted, never a duplicate, a cross-scope hit
// or an index block — those were never candidates. It accrues in the batch
// that crosses the cap and in no other: the caller's post-batch break ends the
// keyword loop right after, so nothing further is even retrieved.
//
// The aggregate cap MaxCandidatesPerKeyword*MaxKeywords is enforced BEFORE
// every append, not once per batch. It has to be: nothing clamps the keyword
// list to MaxKeywords (the keyword prompt asks for 5 to 8 concepts,
// keywords.go), so a cycle can run more than MaxKeywords batches, and with a
// post-batch check alone the batch that crosses the cap still appends in full
// — ending the cycle at up to cap+MaxCandidatesPerKeyword-1 candidates (29
// observed in production). MaxCandidatesPerKeyword*MaxKeywords is also the
// item count promptguard's budget gate models for dream-eval
// (internal/promptguard/budget_gate_test.go, "dream-eval" row, against
// promptguard.BudgetDreamEval); the gate's declared worst case is the real
// worst case only while this guard stands.
//
// seen is mutated: it enters carrying the source block ID and every candidate
// already collected, and leaves carrying the newly appended ones. Skipped
// results — duplicate, cross-scope (V5), index category — cost nothing
// against the cap.
func appendCandidates(candidates []BlockInfo, seen map[string]bool, results []rrf.SearchResult, sourceScope string) ([]BlockInfo, int) {
	dropped := 0
	for _, res := range results {
		if seen[res.ID] {
			continue
		}
		// Same-scope filter (V5).
		if res.Scope != sourceScope {
			continue
		}
		// Exclude index blocks as candidates — structural listings, not content.
		if res.Category == "index" {
			continue
		}
		// The aggregate cap, enforced before the append. Counting instead of
		// breaking leaves the candidate set bit-identical — past the cap the
		// loop body only reads — and buys the drop count.
		if len(candidates) >= MaxCandidatesPerKeyword*MaxKeywords {
			dropped++
			continue
		}
		seen[res.ID] = true
		candidates = append(candidates, BlockInfo{
			ID:        res.ID,
			Title:     res.Title,
			Category:  res.Category,
			Content:   res.Content,
			Scope:     res.Scope,
			UpdatedAt: res.UpdatedAt,
		})
	}
	return candidates, dropped
}

// Stats returns dream processing statistics, filtered by scope. Eligibility
// criteria are the PickBlock predicate itself (dreamEligibleWhere + linkable
// allowlist, WF T8) — Stats never counts a block PickBlock would not pick.
//
// Returned counters:
//   - total:          all eligible blocks
//   - checked:        blocks that have been through at least one Dream cycle
//   - linked:         total links in the graph (scope-filtered)
//   - pendingRecheck: already-checked blocks whose cooldown has expired (ready for re-dream)
func Stats(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string) (total, checked, linked, pendingRecheck int, err error) {
	eligible := dreamEligibleWhere("$2")
	err = pool.QueryRow(ctx,
		`SELECT
			(SELECT count(*) FROM context_blocks WHERE `+eligible+` AND embedding IS NOT NULL AND scope = ANY($1::text[]))::int,
			(SELECT count(*) FROM context_blocks WHERE `+eligible+` AND dream_checked_at IS NOT NULL AND scope = ANY($1::text[]))::int,
			(SELECT count(*) FROM context_dream_links WHERE scope = ANY($1::text[]))::int,
			(SELECT count(*) FROM context_blocks
				WHERE `+eligible+`
				  AND embedding IS NOT NULL
				  AND dream_checked_at IS NOT NULL
				  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
				  AND scope = ANY($1::text[]))::int`,
		scopes, linkable,
	).Scan(&total, &checked, &linked, &pendingRecheck)
	return
}

// QueueStats is the actionable Dream backlog plus the incoming-load forecast.
type QueueStats struct {
	PickableNow   int        `json:"pickable_now"`    // eligible + cooldown expired/null — scheduler's next picks
	InCooldown    int        `json:"in_cooldown"`     // eligible but waiting out its cooldown window
	NeverDreamed  int        `json:"never_dreamed"`   // subset of pickable that never completed a cycle
	AwaitingEmbed int        `json:"awaiting_embed"`  // eligible-by-type but embedding still pending
	Incoming1h    int        `json:"incoming_1h"`     // in cooldown now, expiring within the next hour
	Incoming6h    int        `json:"incoming_6h"`     // in cooldown now, expiring within the next 6 hours
	NextPendingAt *time.Time `json:"next_pending_at"` // exact timestamp the next cooldown expires (nil if none)
}

// QueueDepth reports the Dream backlog using the exact eligibility predicate of
// PickBlock, plus a forward-looking forecast: how many cooldowns expire in the
// next hour / 6 hours and the exact timestamp of the next one. The forecast lets
// operators anticipate GPU load instead of only seeing the current backlog.
func QueueDepth(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string) (*QueueStats, error) {
	var q QueueStats
	err := pool.QueryRow(ctx,
		`WITH eligible AS (
			SELECT dream_cooldown_until AS cd, dream_checked_at AS chk, embedding IS NULL AS no_embed
			FROM context_blocks
			WHERE `+dreamEligibleWhere("$2")+`
			  AND scope = ANY($1::text[])
		)
		SELECT
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND (cd IS NULL OR cd < now()))::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd >= now())::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND chk IS NULL)::int,
			(SELECT count(*) FROM eligible WHERE no_embed)::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd > now() AND cd <= now() + interval '1 hour')::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd > now() AND cd <= now() + interval '6 hours')::int,
			(SELECT min(cd) FROM eligible WHERE NOT no_embed AND cd > now())`,
		scopes, linkable,
	).Scan(&q.PickableNow, &q.InCooldown, &q.NeverDreamed, &q.AwaitingEmbed, &q.Incoming1h, &q.Incoming6h, &q.NextPendingAt)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// CleanupDanglingLinks removes links whose source or target block is archived.
// Welle 45: extended from target-only to both sides — Audit (2026-05-22)
// uncovered 2 links where both endpoints were archived but never cleaned up.
func CleanupDanglingLinks(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_dream_links
		WHERE source_block_id IN (SELECT id FROM context_blocks WHERE is_archived)
		   OR target_block_id IN (SELECT id FROM context_blocks WHERE is_archived)`)
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

// SupersedesMap returns a map of outdated_block_id → []newer_block_ids that
// supersede it. A block can be superseded by multiple newer blocks, hence
// the slice value.
//
// Welle 46 Convention-Switch (2026-05-22): under the English convention
// "A supersedes B" → A=source=newer, B=target=outdated. The map is keyed by
// the OUTDATED block (target side of the link); the slice values are the
// newer source IDs that are the canonical replacements. Pre-Welle-46 the
// SQL filtered source_block_id and keyed the map by source — the inversion
// is therefore both at the WHERE clause (target_block_id = ANY) and at the
// scan/append (key = targetID, value = sourceID).
//
// Gates on raw_confidence >= 0.7 (LLM self-assessment). weighted is a ranking
// signal, not a gate. Used by the query handler to enrich responses and for
// filterSuperseded (which still treats map[id] = "ids that supersede id").
func SupersedesMap(ctx context.Context, pool *pgxpool.Pool, blockIDs []string) (map[string][]string, error) {
	if len(blockIDs) == 0 {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT source_block_id::text, target_block_id::text
		FROM context_dream_links
		WHERE relationship = 'supersedes'
		  AND raw_confidence >= 0.7
		  AND target_block_id = ANY($1::uuid[])`,
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
		// Key = outdated target, value = newer source(s) that replace it.
		result[targetID] = append(result[targetID], sourceID)
	}
	return result, rows.Err()
}
