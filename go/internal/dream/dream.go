package dream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Protocol is the wire protocol for dream LLM calls ("ollama" or "openai").
// Set by main() from CTX_DREAM_PROTOCOL. Historically this existed so dream
// calls would not silently follow the chat protocol (llm.DefaultProtocol,
// deleted in F1-W3) under a split chat/dream configuration; the var itself
// dies in F1-W6 when the dream entry points take a backends.Backend.
var Protocol = "ollama"

// chatJSON is the seam through which the dream pipeline calls the LLM.
// Tests override this to inject canned ChatResponse values without touching
// llm.ChatJSON's HTTP transport. Production callers (evaluate, keywords,
// validate_temporal) use it identically to llm.ChatJSON.
var chatJSON = func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
	return llm.ChatJSONWithProtocol(ctx, Protocol, host, apiKey, model, think, systemPrompt, userPrompt, opts, timeout)
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
func RunDreamCycle(ctx context.Context, pool *pgxpool.Pool, embedB backends.Backend, chatHost, chatAPIKey, chatModel string, think *bool, opts llm.Options, readScopes []string, throttle Throttle) (int, error) {
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
	candidates, err := searchByKeywords(ctx, pool, embedB, keywords, readScopes, block.ID, block.Scope)
	if err != nil {
		slog.Warn("dream: keyword search failed", "block_id", block.ID, "error", err)
		_ = SetDreamCooldownMinutes(ctx, pool, block.ID, CooldownTransientMinutes)
		return 0, err
	}

	if len(candidates) == 0 {
		slog.Info("dream: no candidates found", "block_id", block.ID)
		_ = SetDreamCooldown(ctx, pool, block.ID, true /* inert: no candidates */)
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

	// Step 5b (Welle 38b, v5): detect recurrent pairs via temporal+title overlap and confirm
	// per-pair via LLM. Runs AFTER main eval so 'recurrent' overwrites a 'topical' that
	// EvaluateRelationships may have written for the same pair — recurrent is the more
	// specific classification. Non-fatal on error: dream cycle continues.
	recurrentLinks, recErr := DetectRecurrence(ctx, pool, chatHost, chatAPIKey, chatModel, think, opts, *block)
	if recErr != nil {
		slog.Warn("dream: recurrence detection failed (non-fatal)", "block_id", block.ID, "error", recErr)
	} else if len(recurrentLinks) > 0 {
		rWritten, rwErr := WriteLinks(ctx, pool, block.ID, block.Scope, block.QualityScore, recurrentLinks)
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
	// which starts the block further up the curve (backoffInertOffset).
	_ = SetDreamCooldown(ctx, pool, block.ID, written == 0)

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
// quality_score >= 0.8, no inbound supersedes, block_type = 'knowledge' or NULL.
//
// Welle 46 Convention-Switch (2026-05-22): under the English convention
// "A supersedes B" → A=source=newer, B=target=outdated. A block is OUTDATED
// (and must not become canonical) when it appears as the TARGET of a
// supersedes-link — something newer (the source) has replaced it. The NOT
// EXISTS clause was inverted from source_block_id=$1 to target_block_id=$1.
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
			WHERE target_block_id = $1 AND relationship = 'supersedes'
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
// Excludes is_meta blocks — Post-Reset-Audit 2026-04-20 validated that meta blocks
// (Origin-Stories, CV, Compound-Loop, Agent-Briefing, index) cause 85% of NO_REL noise
// without producing any valid relationships. Filter empirically clean (0% FN-Rate).
//
// Atomic claim-with-TTL (Welle-49, parallelism-bug-fix): a plain
// pool.QueryRow(... FOR UPDATE SKIP LOCKED) releases the row lock as soon as
// the statement returns, so a multi-second RunDreamCycle that followed had no
// protection from a sibling worker picking the same block. Fix: UPDATE … FROM
// subquery sets a transient cooldown (CooldownTransientMinutes) atomically with
// the pick, so other workers see the cooldown active and SKIP. On successful
// cycle completion the caller overrides with the real outcome cooldown
// (CooldownActiveDays / CooldownInertDays). On crash the transient claim
// expires after 5 min and the block re-enters the queue.
func PickBlock(ctx context.Context, pool *pgxpool.Pool) (*BlockInfo, error) {
	var block BlockInfo
	err := pool.QueryRow(ctx,
		`UPDATE context_blocks
		SET dream_cooldown_until = now() + ($1 * interval '1 minute')
		WHERE id = (
			SELECT id FROM context_blocks
			WHERE NOT is_archived
			  AND embedding IS NOT NULL
			  AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
			  AND NOT is_meta
			  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
			ORDER BY dream_checked_at ASC NULLS FIRST, quality_score ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, title, category, content, scope, quality_score, updated_at, created_at, dream_keywords, dream_temporal_validated_at`,
		CooldownTransientMinutes,
	).Scan(&block.ID, &block.Title, &block.Category, &block.Content, &block.Scope, &block.QualityScore, &block.UpdatedAt, &block.CreatedAt, &block.DreamKeywords, &block.DreamTemporalValidatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pick block: %w", err)
	}
	return &block, nil
}

// Back-off cooldown (Wave-2, W49c; recalibrated W49c-2): the effective re-dream
// interval grows with a block's completed eval count (dream_eval_count), so mature
// blocks are pulled progressively less often while fresh blocks re-dream quickly to
// catch links to newly-inserted related blocks. The curve is in HOURS from a small
// minimum (default 12h) — early dreams are cheap and quality rises fastest then, so
// there is NO grace plateau (grace=0): growth starts at n=0.
//
//	exp:    minHours * factor^max(0, n-grace+off)        [default — 12h→cap by n~10]
//	log:    minHours * (1 + factor*ln(1 + max(0,n-grace+off)))
//	linear: minHours + factor*24*max(0, n-grace+off)
//	off:    CooldownActiveDays / CooldownInertDays (days) [back-off disabled]
//
// n = dream_eval_count BEFORE this increment (so a block dreamed once gets the n=1
// step, a fresh block n=0). off = backoffInertOffset when the cycle produced NO
// links (inert) — an inert block starts further up the SAME curve instead of from a
// separate base. Result floored at 1h and capped at backoffCapHours. Back-off only
// DELAYS a re-dream, never stops it, so a link missed in a skipped cycle is
// recovered on the next accepted pull (no permanent recall loss). Defaults
// (exp/1.6/min12h/grace0/cap45/inert+7) validated on real pull history
// (.project/bench-session-w49c/backoff_curves.py): ~48% of dream-eval GPU saved at
// lower link-loss than the prior log/3d curve, with fresh blocks re-dreaming sub-day.
var (
	backoffMode        = "exp"
	backoffFactor      = 1.6
	backoffGrace       = 0
	backoffMinHours    = 12.0
	backoffCapHours    = 45.0 * 24
	backoffInertOffset = 7
)

// SetBackoffConfig overrides the Dream back-off parameters at startup. mode must be
// one of exp|log|linear|off (anything else is ignored, keeping the default). factor,
// grace, minHours, inertOffset < 0 and capHours <= 0 are ignored.
func SetBackoffConfig(mode string, factor float64, grace int, capHours, minHours float64, inertOffset int) {
	switch mode {
	case "exp", "log", "linear", "off":
		backoffMode = mode
	}
	if factor >= 0 {
		backoffFactor = factor
	}
	if grace >= 0 {
		backoffGrace = grace
	}
	if capHours > 0 {
		backoffCapHours = capHours
	}
	if minHours >= 0 {
		backoffMinHours = minHours
	}
	if inertOffset >= 0 {
		backoffInertOffset = inertOffset
	}
}

// effectiveCooldownHours returns the back-off cooldown in HOURS for a block at the
// given pre-increment eval count n, using the active config. inert applies the
// inert-offset (no links found this cycle). It is the Go mirror of the SQL in
// SetDreamCooldown — keep the two in sync. Used by BackoffStats and tests.
func effectiveCooldownHours(n int, inert bool) float64 {
	if backoffMode == "off" {
		if inert {
			return float64(CooldownInertDays) * 24
		}
		return float64(CooldownActiveDays) * 24
	}
	x := float64(n - backoffGrace)
	if inert {
		x += float64(backoffInertOffset)
	}
	if x < 0 {
		x = 0
	}
	h := backoffMinHours
	switch backoffMode {
	case "exp":
		h = backoffMinHours * math.Pow(backoffFactor, x)
	case "log":
		h = backoffMinHours * (1 + backoffFactor*math.Log(1+x))
	case "linear":
		h = backoffMinHours + backoffFactor*24*x
	}
	if h > backoffCapHours {
		h = backoffCapHours
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

// ComputeBackoffStats reports the active back-off policy and a per-eval-count
// maturity distribution: how many eligible blocks sit at each completed-cycle
// count and the effective re-dream cooldown that count yields (active base). One
// row per occupied level, ascending, up to the corpus max (the growth cut-off —
// beyond it no blocks exist), capped at maxBackoffLevels rows. The per-level
// cooldown uses the exact count (not a bucket edge), so the growth is visible and
// the final row shows the true maximum, not a floor.
func ComputeBackoffStats(ctx context.Context, pool *pgxpool.Pool, scopes []string) (*BackoffStats, error) {
	out := &BackoffStats{
		Mode: backoffMode, Factor: backoffFactor, Grace: backoffGrace, CapHours: backoffCapHours,
		MinHours: backoffMinHours, InertOffset: backoffInertOffset,
		Levels: make([]BackoffLevel, 0, 32),
	}
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(dream_eval_count), 0)::int FROM context_blocks
		 WHERE NOT is_archived AND embedding IS NOT NULL
		   AND (block_type IS NULL OR block_type IN ('knowledge','source','canonical'))
		   AND NOT is_meta AND scope = ANY($1::text[])`,
		scopes).Scan(&out.MaxEval); err != nil {
		return nil, fmt.Errorf("dream: backoff max eval: %w", err)
	}
	rows, err := pool.Query(ctx,
		`SELECT dream_eval_count::int, count(*)::int FROM context_blocks
		 WHERE NOT is_archived AND embedding IS NOT NULL
		   AND (block_type IS NULL OR block_type IN ('knowledge','source','canonical'))
		   AND NOT is_meta AND scope = ANY($1::text[])
		 GROUP BY dream_eval_count ORDER BY dream_eval_count
		 LIMIT $2`,
		scopes, maxBackoffLevels+1)
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
			CooldownH: effectiveCooldownHours(n, false),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dream: backoff rows: %w", err)
	}
	return out, nil
}

// SetDreamCooldown marks a block as dream-checked, increments its completed-cycle
// counter, and sets the back-off cooldown (in hours) derived from the new count.
// inert=true (no links found this cycle) applies the inert offset so the block
// starts further up the curve. The CASE mirrors effectiveCooldownHours exactly.
// x = (dream_eval_count + 1) - grace + (inert ? inertOffset : 0). For transient
// system failures use SetDreamCooldownMinutes instead — that path does NOT
// increment the counter, so the back-off reflects real completed work only.
func SetDreamCooldown(ctx context.Context, pool *pgxpool.Pool, blockID string, inert bool) error {
	off := 0
	if inert {
		off = backoffInertOffset
	}
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET
			dream_eval_count = dream_eval_count + 1,
			dream_checked_at = now(),
			dream_cooldown_until = now() + GREATEST(1.0, LEAST($5::float8,
				CASE $6::text
					WHEN 'exp'    THEN $2::float8 * power($3::float8, GREATEST(0, (dream_eval_count + 1) - $4 + $7)::float8)
					WHEN 'log'    THEN $2::float8 * (1 + $3::float8 * ln(1 + GREATEST(0, (dream_eval_count + 1) - $4 + $7)::float8))
					WHEN 'linear' THEN $2::float8 + $3::float8 * 24.0 * GREATEST(0, (dream_eval_count + 1) - $4 + $7)::float8
					ELSE (CASE WHEN $8::bool THEN $9::float8 ELSE $10::float8 END) * 24.0
				END
			)) * interval '1 hour'
		WHERE id = $1`,
		blockID, backoffMinHours, backoffFactor, backoffGrace, backoffCapHours, backoffMode,
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
// and returns candidate blocks (excluding the source block and cross-scope blocks).
func searchByKeywords(ctx context.Context, pool *pgxpool.Pool, embedB backends.Backend, keywords []string, scopes []string, sourceID, sourceScope string) ([]BlockInfo, error) {
	seen := make(map[string]bool)
	seen[sourceID] = true // Exclude source block.
	var candidates []BlockInfo
	embedFailures := 0

	for _, kw := range keywords {
		// Embed the keyword for semantic search. Cached by (hash(prefix||kw), model) —
		// Dream keywords repeat heavily across cycles (domain vocabulary, proper nouns).
		kwEmbedding, err := embedcache.Embed(ctx, pool, embedB, kw, embed.PrefixQuery)
		if err != nil {
			embedFailures++
			slog.Debug("dream: embed keyword failed", "keyword", kw, "error", err)
			// Fail fast if majority of embeddings fail (Ollama likely down).
			if embedFailures > len(keywords)/2 {
				return nil, fmt.Errorf("dream: too many embedding failures (%d/%d)", embedFailures, len(keywords))
			}
			continue
		}

		// RRF search with keyword as query. Welle 41 M039: audit-trail-factor
		// 1.0 (no damping) — dream-cycle keyword search needs full retrieval
		// pool, user-query pattern-aware damping is handler-layer only.
		// v2.0.0 C2 (M048): no exclude-lists for dream — dream sees everything.
		results, err := rrf.Search(ctx, pool, kwEmbedding, kw, kw, scopes, nil, nil, MaxCandidatesPerKeyword, "", "", 1.0, nil, nil)
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
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND embedding IS NOT NULL AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical')) AND NOT is_meta AND scope = ANY($1::text[]))::int,
			(SELECT count(*) FROM context_blocks WHERE NOT is_archived AND dream_checked_at IS NOT NULL AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical')) AND NOT is_meta AND scope = ANY($1::text[]))::int,
			(SELECT count(*) FROM context_dream_links WHERE scope = ANY($1::text[]))::int,
			(SELECT count(*) FROM context_blocks
				WHERE NOT is_archived
				  AND embedding IS NOT NULL
				  AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
				  AND NOT is_meta
				  AND dream_checked_at IS NOT NULL
				  AND (dream_cooldown_until IS NULL OR dream_cooldown_until < now())
				  AND scope = ANY($1::text[]))::int`,
		scopes,
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
func QueueDepth(ctx context.Context, pool *pgxpool.Pool, scopes []string) (*QueueStats, error) {
	var q QueueStats
	err := pool.QueryRow(ctx,
		`WITH eligible AS (
			SELECT dream_cooldown_until AS cd, dream_checked_at AS chk, embedding IS NULL AS no_embed
			FROM context_blocks
			WHERE NOT is_archived
			  AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
			  AND NOT is_meta AND scope = ANY($1::text[])
		)
		SELECT
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND (cd IS NULL OR cd < now()))::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd >= now())::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND chk IS NULL)::int,
			(SELECT count(*) FROM eligible WHERE no_embed)::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd > now() AND cd <= now() + interval '1 hour')::int,
			(SELECT count(*) FROM eligible WHERE NOT no_embed AND cd > now() AND cd <= now() + interval '6 hours')::int,
			(SELECT min(cd) FROM eligible WHERE NOT no_embed AND cd > now())`,
		scopes,
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
