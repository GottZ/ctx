package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/promptguard"
)

// sharedScope is the one scope name V22 refuses for distill.scope. A literal
// here rather than store.GlobalScope's neighbour constant: config is
// parameter-pure and imports no storage layer, and the name is a wire value of
// the tenancy model, not of a package.
const sharedScope = "shared"

// Prompt versions accepted by V5. Mirrors llm.PromptVersionV52/V6 — kept as
// literals here because the layering rule forbids importing config from llm
// and vice versa would couple the domain package to config.
const (
	promptVersionV52 = "v5.2"
	promptVersionV6  = "v6"
)

// V14 shape gate for dream.language (validateDream). The tag is the ONLY
// config value that reaches an LLM system prompt verbatim, so the accepted
// alphabet is deliberately narrower than full BCP-47: primary subtag +
// optional alphanumeric subtags, nothing else — no spaces, no quotes, no
// newlines, no non-ASCII. The length cap bounds the interpolation.
const (
	dreamLanguagePattern = `^[a-z]{2,3}(-[a-z0-9]{2,8})*$`
	dreamLanguageMaxLen  = 35
)

var dreamLanguageRe = regexp.MustCompile(dreamLanguagePattern)

// temporalTimeoutBudgetOf is the largest dream.temporal_timeout that still
// leaves the two LLM stages behind Phase-2 temporal their own ceilings inside
// a cycle of the given whole-cycle deadline (V16b): the whole cycle runs
// under CycleTimeoutOf, and temporal is step 1b — keyword extraction
// (KeywordsTimeout) and relationship evaluation (DreamTimeout) follow it and
// write the links. Derived from the cycle deadline, not mirrored, so it
// cannot drift when the timeouts are retuned. It reads the effective (hot)
// cycle timeout, falling back to the package constant, so a configured
// dream.cycle_timeout widens the budget accordingly instead of warning
// spuriously.
func temporalTimeoutBudgetOf(c *Config) time.Duration {
	return dream.CycleTimeoutOf(c.Dream.CycleTimeout) - (dream.KeywordsTimeout + dream.DreamTimeout)
}

// Validate checks the surviving cross-field invariants and returns all
// findings. The V-numbers are historical labels, not a contiguous range — the
// cut train retired V1, V4, V7, V8, V12 and V13 with the six backend tuples
// they read (validateBackendTuples, gone in β8; the history is below).
// WARN classes with "today's silent fallback" semantics (V5 prompt version,
// V6 back-off, V10 parallelism clamp) NORMALIZE the config in place — exactly
// what llm's init(), dream.SetBackoffConfig and the scheduler clamp did
// downstream until now, made visible. Callers: boot aborts on HasErrors after
// logging everything; Store.Replace rejects.
func Validate(c *Config) []Issue {
	var issues []Issue
	issues = append(issues, validateQuery(c)...)         // V2, V5, V9b, V9c, V9d, V11, V26
	issues = append(issues, validateRerankGraph(c)...)   // V3, V9
	issues = append(issues, validateDream(c)...)         // V6, V10, V14, V15, V16b, V16c, V18, V19, V20
	issues = append(issues, validateGraphOverview(c)...) // V17b
	issues = append(issues, validateDurations(c)...)     // V17
	issues = append(issues, validateEmbedBackoff(c)...)  // V21
	issues = append(issues, validateDistill(c)...)       // V22, V23, V24, V25, V27
	return issues
}

// validateDistill holds the cross-field invariants of the distiller group
// (design/03 §5.4, §5.5, §6.4; wave A03-W03-3, plus V27 from wave C2-8). All of
// them are fatal: boot drops the offending override, a settings PUT is a 422 —
// the class every "renders as configured, acts as something else" knob in this
// file gets.
//
// The group has no consumer yet. That is deliberate and it is exactly why the
// checks come WITH the keys rather than after them: the arm that will read
// distill.scope writes foreign content into a scope, and a validation added
// later would have to be added against an already-configurable key.
//
// Field stays the canonical registry key for all four — build.go's
// dropOffenders attributes a failing override by Field, and an unattributable
// error withdraws ALL DB overrides of that generation instead of the offender.
func validateDistill(c *Config) []Issue {
	var issues []Issue
	d := &c.Distill

	// V22 — distill.scope may never be "shared". Normalized in place first
	// (trim + lower, the V14/V20 pattern), because a check that " Shared "
	// walks past is not a fail-closed check.
	//
	// EMPTY stays legal and is the default: it is the inheritance path
	// (effectiveHomeScope over scheduler.home_scope), the same path every other
	// arm takes. Any other scope name is the operator's business — the arm
	// writes there and nowhere else.
	//
	// Why "shared" specifically: in this one case it would be a propagation
	// path for FOREIGN content across the tenant border. The arm's material is
	// a single operator's agent transcript, and shared is the one scope where
	// that material would reach readers it was never collected for.
	//
	// This key is only half the guard, and the design is explicit that it must
	// be: it validates what is EXPLICITLY set, never what is INHERITED. The
	// runtime half — the inherited path resolving to shared — is gate 5 of the
	// arm (skipped/scope_forbidden), which belongs to the wave that builds the
	// arm. Neither half covers the other's case.
	d.Scope = strings.ToLower(strings.TrimSpace(d.Scope))
	if d.Scope == sharedScope {
		issues = append(issues, Issue{Field: "distill.scope", Severity: SeverityError,
			Msg: fmt.Sprintf("distill.scope %q is refused: the distiller writes FOREIGN transcript content, and %q would carry it across the tenant border — leave it empty to inherit scheduler.home_scope, or name a scope of this operator",
				sharedScope, sharedScope)})
	}

	// V23 — distill.block_sensitivity floor. The type parser already rejects
	// anything outside the four defined levels, so what is left here is the
	// FLOOR: "internal" (rank 1). "public" (rank 0) is a 422.
	//
	// The value is the ONLY lever over the block's life cycle (§5.5): embed
	// backfill, dream, digest and query synthesis each derive their required
	// backend trust from the block's sensitivity, and none of them sets
	// LocalOnly. A public insight block full of raw tool output would be
	// eligible for every backend the pool has.
	//
	// The floor does NOT claim to close every external path — the design says
	// plainly that no sensitivity value excludes both live external rows, and
	// the remaining one is a documented exception on the arm's egress gate.
	// What the floor does is remove the value that would open all of them.
	if r := d.BlockSensitivity.Rank(); r < backends.SensInternal.Rank() {
		issues = append(issues, Issue{Field: "distill.block_sensitivity", Severity: SeverityError,
			Msg: fmt.Sprintf("distill.block_sensitivity %q is below the %q floor — insight blocks carry raw foreign tool output, and every later consumer (embed backfill, dream, digest, synthesis) derives which backends may see a block from this value alone",
				d.BlockSensitivity, backends.SensInternal)})
	}

	// V27 — distill.category must name a category the derived layer OWNS.
	// Normalized in place first (trim + lower), for the V22 reason one line up
	// and for a second one that is specific to this key: the value is half of
	// the arm's upsert identity, so " Session-Insights " would not merely walk
	// past a check, it would CREATE a category that differs byte-wise from the
	// reserved one while reading as the same word.
	//
	// THIS IS THE RECONCILIATION derived/reserved.go:24-30 delegates to the arm
	// wave, and it takes the SECOND of the two ways that comment names ("either
	// by pinning the key to this value or by refusing a distill.category outside
	// this list"). Refusing, not pinning, for four reasons:
	//
	//  1. Pinning means deleting an operator-visible, hot-mutable, documented
	//     settings key. A key that renders in GET /api/settings and is silently
	//     ignored is the "renders as configured, acts as something else" class
	//     this file exists to refuse; deleting it outright breaks a surface that
	//     is contract, and A03/A02-4 add further distill.* keys around it.
	//  2. Refusing keeps the security list where §4.3.1 puts it — code-owned in
	//     internal/derived, "Mechanismus, nicht Politik". The key stays a CHOICE
	//     AMONG reserved values and never a way OUT of the reservation, which is
	//     exactly the property the comment asks for.
	//  3. A pin could only ever pin ONE arm. ReservedCategories carries two
	//     entries because two arms write into it; the refusal covers both shapes
	//     with one rule.
	//  4. It is the same shape as V22/V23 right above — same 422 surface, same
	//     boot-time attribution by Field, testable without a running arm.
	//
	// EMPTY IS REFUSED TOO, and that is not pedantry: the type default is
	// "session-insights", so an empty value can only come from an operator
	// override, and it would put the arm's blocks into the category "" — outside
	// every reservation, invisible to reservedCategoryReject, and free for any
	// client to upsert onto.
	d.Category = strings.ToLower(strings.TrimSpace(d.Category))
	if !derived.IsReservedCategory(d.Category) {
		issues = append(issues, Issue{Field: "distill.category", Severity: SeverityError,
			Msg: fmt.Sprintf("distill.category %q is not a category of the derived layer (%s) — the arm's category is half of its upsert identity AND the reservation that keeps clients out of it (403 reserved_category), so a value outside this list would move the arm out of its own protection",
				d.Category, strings.Join(derived.ReservedCategories, ", "))})
	}

	issues = append(issues, validateDistillBudget(d)...) // V24
	issues = append(issues, validateDistillCounters(d)...)
	return issues
}

// validateDistillBudget is V24: the prompt-budget coupling. One distill batch
// is rows_per_call rows at max_row_runes each plus the nonce-bound rule, and
// that product must fit promptguard.BudgetDistill.
//
// The same arithmetic runs in TestDistillDefaultsFitPromptBudget against the
// registry DEFAULTS — on THIS side of the layering border, because promptguard
// may not import config and hand-copied numbers would assert nothing (the note
// on promptguard.BudgetDistill carries the reasoning). Both halves are needed
// and neither is redundant: the static gate catches a raised default at
// `go test`, this check catches a raised OVERRIDE at boot / on the settings
// write — a live install can outgrow a budget its compiled defaults still fit,
// and the symptom would be a prompt the assembler silently cuts instead of a
// refused write.
//
// Config validating against the CONSUMER's constant, never a second copy of it,
// is the same relation V16c has to dream.KeywordsTimeout. promptguard imports
// nothing from this package, so the direction is free of a cycle.
//
// int64 on purpose: the product of two operator-supplied ints is the one place
// in this file where an overflow could turn a refusal into an accept.
func validateDistillBudget(d *DistillConfig) []Issue {
	if d.RowsPerCall < 1 || d.MaxRowRunes < 1 {
		return nil // the range check below owns this shape; no product to take
	}
	worst := int64(d.RowsPerCall)*int64(d.MaxRowRunes) + int64(promptguard.RuleReserve)
	if worst <= int64(promptguard.BudgetDistill) {
		return nil
	}
	return []Issue{{Field: "distill.rows_per_call", Severity: SeverityError,
		Msg: fmt.Sprintf("distill.rows_per_call %d x distill.max_row_runes %d + rule reserve %d = %d runes exceeds the %d-rune distill prompt budget — lower either key, or raise promptguard.BudgetDistill AND re-check it against the smallest context window the digest role's chain can resolve to",
			d.RowsPerCall, d.MaxRowRunes, promptguard.RuleReserve, worst, promptguard.BudgetDistill)}}
}

// validateDistillCounters is V25: the range half of the group — the counted
// keys, since the generic V17 walk is typed and visits duration keys only.
//
// Two readings of zero, deliberately kept apart:
//
//   - spend_max_calls 0 is the DOCUMENTED kill switch (guard off, effective
//     from the next tick because the snapshot is re-read per iteration), and
//     the retention pairs' 0 is the recall_check no-op ("keep forever"). Those
//     stay legal; only their negatives are refused, on the house grounds that a
//     negative renders as a configured number while acting as the off-switch.
//   - the sizing keys have NO safe zero. rows_per_call 0 or max_row_runes 0
//     means no batch can ever be built, rows_per_read 0 means every read
//     returns nothing, max_sessions_per_run 0 means no source is ever
//     considered, max_block_runes 0 means every written block is empty. Each of
//     them is a silent second off-switch next to distill.enabled — one that the
//     settings surface renders as a configured size.
//
// breaker_failures is the one whose floor is not obvious: 0 would open the
// circuit breaker at zero failures, i.e. permanently, before the arm has ever
// tried. That is not "breaker off", it is "arm off with a misleading name", so
// the floor is 1 — one failure, one open.
func validateDistillCounters(d *DistillConfig) []Issue {
	var issues []Issue
	for _, k := range []struct {
		key  string
		val  int
		min  int
		note string
	}{
		{"distill.rows_per_call", d.RowsPerCall, 1, "no batch could ever be assembled"},
		{"distill.max_row_runes", d.MaxRowRunes, 1, "every selected row would be cut to nothing"},
		{"distill.rows_per_read", d.RowsPerRead, 1, "every read of the source would return nothing"},
		{"distill.max_sessions_per_run", d.MaxSessionsPerRun, 1, "no source would ever be considered"},
		{"distill.max_block_runes", d.MaxBlockRunes, 1, "every written block would be empty"},
		{"distill.breaker_failures", d.BreakerFailures, 1, "the breaker would stand open before the first attempt"},
		{"distill.min_row_runes", d.MinRowRunes, 0, "a negative substance threshold has no reading"},
		{"distill.initial_backfill_rows", d.InitialBackfillRows, 0, "0 is the documented cold start at the head of the source"},
		{"distill.spend_max_calls", d.SpendMaxCalls, 0, "0 is the documented kill switch that disables the guard"},
		{"distill.retention_days", d.RetentionDays, 0, "0 is the documented no-op that keeps rows forever"},
		{"distill.seen_retention_days", d.SeenRetentionDays, 0, "0 is the documented no-op that keeps hashes forever"},
	} {
		if k.val < k.min {
			issues = append(issues, Issue{Field: k.key, Severity: SeverityError,
				Msg: fmt.Sprintf("%s %d must be >= %d — %s, and the settings surface would still render it as a configured value",
					k.key, k.val, k.min, k.note)})
		}
	}

	// The journal's source identity must have a name. An empty label collapses
	// every source into a source_key that starts with ":", so two different
	// state.db files would share one watermark series — a silent data merge,
	// not a cosmetic default.
	if strings.TrimSpace(d.SourceLabel) == "" {
		issues = append(issues, Issue{Field: "distill.source_label", Severity: SeverityError,
			Msg: "distill.source_label must not be empty — it is the stable half of the journal's source key, and an empty label would merge the watermark series of every configured source into one"})
	}
	return issues
}

// validateEmbedBackoff is V21 (issue #38): the two embed back-off bases must
// be strictly positive. Their consumer (store/embed_failures.go) plugs the
// base straight into Postgres' make_interval as `base * 2^attempts` — with
// base 0 every failure memo computes next_attempt_at = now(), so the memo
// parks nothing and a failing embed backend is retried in a tight loop. At
// the target scale (10M+ blocks) that is a self-inflicted DoS of the embed
// lane, which is why the class is fatal (boot drops the offending override /
// 422 on the settings write) and not a WARN: unlike the duration keys whose
// 0 means "off" or "package default", these two have NO safe zero reading.
//
// graph_cache.debounce_window was named in the same issue and deliberately
// stays legal at 0: its consumer compares `quiet >= DebounceWindow` inside a
// poll-bounded scheduler arm that min_rebuild_interval brakes independently,
// so 0 means "rebuild on the next poll without a quiet requirement" — a
// legitimate setting, now documented on the key.
//
// The negative half of both keys stays V17's (generic duration walk); this
// check owns exactly the zero.
func validateEmbedBackoff(c *Config) []Issue {
	var issues []Issue
	for _, k := range []struct {
		key string
		val time.Duration
	}{
		{"embed_backfill.backoff_base", c.EmbedBackfill.BackoffBase},
		{"embed_migration.backoff_base", c.EmbedMigration.BackoffBase},
	} {
		if k.val == 0 {
			issues = append(issues, Issue{Field: k.key, Severity: SeverityError,
				Msg: fmt.Sprintf("%s must be > 0 — the consumer reads 0 as \"retry immediately\" (base * 2^attempts stays 0), so the failure memo parks nothing and a failing embed backend is retried in a tight loop", k.key)})
		}
	}
	return issues
}

// validateGraphOverview holds the label arm's cross-field invariants.
//
// V17b — graph_overview.label_timeout against graph_overview.label_interval.
// The sign half of the key is NOT here: it is V17's class and the generic
// duration walk already owns it, so a per-key check would only double-report
// (the statement TestValidateRejectsNegativeOnEveryDurationKey pins).
//
// What is key-specific is the relation to the cadence. runBatch checks its
// overrun brake BEFORE each label (topiclabel runBatch), so a label already
// running when the tick reaches label_interval is not cut — it finishes on
// its own budget. With label_timeout above label_interval a tick can
// therefore outlast its interval by up to one label_timeout, and the batch
// ends after a single label because the brake trips on the next pass. That
// is a legitimate configuration for a slow backend with a short cadence, so
// this is a WARN in the V16b spirit and never a clamp: SeverityWarn is
// log-only — boot logs it and continues, Store.Replace rejects on
// SeverityError alone, so a settings write carrying it is a 200, not a 422.
func validateGraphOverview(c *Config) []Issue {
	var issues []Issue

	to, iv := c.GraphOverview.LabelTimeout, c.GraphOverview.LabelInterval
	if to > 0 && iv > 0 && to > iv {
		issues = append(issues, Issue{Field: "graph_overview.label_timeout", Severity: SeverityWarn,
			Msg: fmt.Sprintf("label timeout %v exceeds the %v label interval — the overrun brake is checked before each label, so a tick can outlast its interval by up to one label timeout and the batch ends after a single label",
				to, iv)})
	}

	return issues
}

// validateDurations is V17: the sign check over EVERY seconds-typed key
// (typDuration) in the registry, walked generically instead of key by key.
//
// The statement is the one V9b/V9c and V16/V16c made per key, and it holds
// for all of them: every consumer of a duration key reads a non-positive
// value as "unset" and substitutes its own default, so a negative value
// SERVES the default while the settings surface RENDERS it as a configured
// duration — the operator reads `-30s`, the runtime runs 700s, and nothing
// in between says so. At fleet scale that is a diagnosis killer, which is
// why the class is fatal (boot abort / 422 on the settings write) and not a
// tolerant WARN.
//
// Generic on purpose: the class has no key-specific half worth writing 37
// times, and a duration key added later is covered without a second edit
// here. That is also why V16 and V16c's sign halves were FOLDED into this
// walk rather than exempted from it — an allowlist of "keys with their own
// sign check" would have to be maintained in lockstep with every future
// per-key check, and forgetting it double-reports the same field.
//
// 0 is NOT checked. What zero means is per key — "package default" for the
// dream timeouts, "off" for status.channel_probe_interval, "retry
// immediately" for the embed back-off bases — and this walk does not have
// that knowledge. The two back-off bases carry their own `> 0` check
// (V21, validateEmbedBackoff, issue #38).
//
// The type assertion is safe by construction: typDuration is
// reflect.TypeOf(time.Duration(0)) (registry.go), so every entry carrying it
// has a time.Duration leaf field — a checked assertion would test the
// registry builder, not this code. Same reflective field walk RenderValue
// (describe.go) already uses.
//
// Field MUST stay the canonical registry key: build.go's dropOffenders
// attributes a failing override by Field, and an error it cannot attribute
// withdraws ALL DB overrides of that generation instead of just the offender.
func validateDurations(c *Config) []Issue {
	var issues []Issue
	rv := reflect.ValueOf(c).Elem()
	for _, e := range registry() {
		if e.typ != typDuration {
			continue
		}
		if d := rv.FieldByIndex(e.path).Interface().(time.Duration); d < 0 {
			issues = append(issues, Issue{Field: e.Key, Severity: SeverityError,
				Msg: fmt.Sprintf("%s %v must be >= 0 — a negative duration renders as a configured value in the settings surface while the consumer reads it as unset and serves its own default (what 0 means stays per key)",
					e.Key, d)})
		}
	}
	return issues
}

// validateBackendTuples and validateHostURL retired with the chat tuple in β8
// (design/01 §7 W7), the last of the six they read. What each check said, and
// where its statement lives now — none of them was dropped without a successor
// or an explicit ruling:
//
//   - V7 (host URLs: parseable, http(s), no trailing slash, NO USERINFO) was
//     the security-carrying one: a credential inside a host URL bypasses the
//     field-name-based secret masking everywhere hosts flow — dump, error logs,
//     the F2 API. Its pendant is on the LIVING write path since α3: a base_url
//     with userinfo is a 422 FieldError at the pool boundary
//     (backends/validate.go validateIdentity), which is where hosts are
//     configured now. The rendering guard redactHostURL (dump.go) is untouched
//     and still covers the .host namespace convention.
//   - V4 (protocol typos: anything but ollama/openai fell silently onto the
//     ollama wire path → 404 against llama.cpp) is enforced at the same
//     boundary: validateIdentity rejects a row whose protocol is not one of
//     openai, ollama, rerank.
//   - V1 (dual-runner VRAM WARN, β6), V12 (dream-embed credential boundary,
//     β5), V8 and V13 (rerank host reachability WARNs, β3) are NOT rebuildable
//     here and are not rebuilt: each compared fields of two tuples, and which
//     rows serve which role is a pool question that Validate — parameter-pure,
//     it never sees the pool — cannot ask. design/01 §5.5 rules V1 out by name
//     ("nicht nachbauen, der Pool kennt Prioritäten und Rollen").

func validateQuery(c *Config) []Issue {
	var issues []Issue

	// V11 — required check, moved from LoadConfig. Stays fatal.
	if c.Server.DBPass == "" {
		issues = append(issues, Issue{Field: "server.db_password", Severity: SeverityError,
			Msg: "CONTEXT_DB_PASSWORD is required"})
	}

	// V2 — inverted thresholds make "low_confidence" unreachable
	// (ClassifyConfidence checks confident first, then score).
	if c.Query.ScoreThreshold > c.Query.ConfidentThreshold {
		issues = append(issues, Issue{Field: "query.score_threshold", Severity: SeverityError,
			Msg: fmt.Sprintf("score_threshold %g > confident_threshold %g makes low_confidence unreachable",
				c.Query.ScoreThreshold, c.Query.ConfidentThreshold)})
	}

	// V26 (E-M6) — query.semantic_floor is compared against a COSINE
	// SIMILARITY, so the only values that can ever fire live in [0,1): 1.0
	// demands a verbatim match of the query against a stored block and would
	// refuse every real question without ever calling the LLM, and a negative
	// value presents as a configured number in the settings surface while the
	// gate reads <= 0 as "off" — the same silent-off shape V9b/V9c refuse.
	// Fatal, not a clamp: a floor is a rejection switch, and guessing what an
	// operator meant by 1.0 would decide for them which questions get answered.
	if c.Query.SemanticFloor < 0 || c.Query.SemanticFloor >= 1 {
		issues = append(issues, Issue{Field: "query.semantic_floor", Severity: SeverityError,
			Msg: fmt.Sprintf("semantic floor %g must be >= 0 and < 1 (0 = off)", c.Query.SemanticFloor)})
	}

	// V5 — today's llm init() semantics: log + fall back to v5.2.
	if c.Query.PromptVersion != promptVersionV52 && c.Query.PromptVersion != promptVersionV6 {
		issues = append(issues, Issue{Field: "query.prompt_version", Severity: SeverityWarn,
			Msg: fmt.Sprintf("unknown prompt version %q — using %q", c.Query.PromptVersion, promptVersionV52)})
		c.Query.PromptVersion = promptVersionV52
	}

	// V9 (rate-limit part) — negative limits are range garbage.
	for _, r := range []struct {
		key string
		val int
	}{
		{"query.rate_limit_write", c.Query.RateLimitWrite},
		{"query.rate_limit_read", c.Query.RateLimitRead},
		{"pool.blob_rate_limit_write", c.Pool.BlobRateLimitWrite},
	} {
		if r.val < 0 {
			issues = append(issues, Issue{Field: r.key, Severity: SeverityError,
				Msg: fmt.Sprintf("rate limit %d must be >= 0", r.val)})
		}
	}

	// V9d (W02-8) — a negative staging cap has no reading at all: 0 already
	// carries the "no blob staging" meaning, so anything below it would render
	// as a configured byte count while the runtime treated it as the switch.
	if c.Pool.BlobStageMaxBytes < 0 {
		issues = append(issues, Issue{Field: "pool.blob_stage_max_bytes", Severity: SeverityError,
			Msg: fmt.Sprintf("staged blob payload cap %d must be >= 0 (0 = blob staging disabled)", c.Pool.BlobStageMaxBytes)})
	}

	// V9e (W02-9) — same shape as V9d, one step further: 0 already means "scan
	// nothing", so a negative scan cap would present as a configured byte count
	// while the runtime read it as the switch.
	if c.Pool.BlobScanMaxBytes < 0 {
		issues = append(issues, Issue{Field: "pool.blob_scan_max_bytes", Severity: SeverityError,
			Msg: fmt.Sprintf("blob scan cap %d must be >= 0 (0 = payload scan disabled)", c.Pool.BlobScanMaxBytes)})
	}

	// V9b (H12) — a negative context-window fallback is range garbage in the
	// one direction that matters: ChainRuneBudget reads <= 0 as "unset" and
	// refuses, so a negative value would silently mean "off" while reading as
	// a configured number in the settings surface.
	if c.Pool.ExternalNumCtxFallback < 0 {
		issues = append(issues, Issue{Field: "pool.external_num_ctx_fallback", Severity: SeverityError,
			Msg: fmt.Sprintf("context window fallback %d must be >= 0 (0 = unset)", c.Pool.ExternalNumCtxFallback)})
	}

	// V9c (E10-W2) — same shape as V9b for the same reason: the discovery TTL
	// reads <= 0 as "discovery off", so a negative value would silently mean
	// off while presenting as a configured duration.
	if c.Pool.OpenRouterWindowTTL < 0 {
		issues = append(issues, Issue{Field: "pool.openrouter_window_ttl", Severity: SeverityError,
			Msg: fmt.Sprintf("endpoint discovery TTL %d must be >= 0 (0 = off)", c.Pool.OpenRouterWindowTTL)})
	}

	return issues
}

func validateRerankGraph(c *Config) []Issue {
	var issues []Issue

	// V9 — range garbage produces silent scoring chaos, not a crash.
	if !(c.Rerank.BlendWeight >= 0 && c.Rerank.BlendWeight <= 1) { // NaN-safe
		issues = append(issues, Issue{Field: "rerank.blend_weight", Severity: SeverityError,
			Msg: fmt.Sprintf("blend_weight %g must be in [0,1]", c.Rerank.BlendWeight)})
	}
	if c.Rerank.MaxDocs < 0 {
		issues = append(issues, Issue{Field: "rerank.max_docs", Severity: SeverityError,
			Msg: fmt.Sprintf("max_docs %d must be >= 0", c.Rerank.MaxDocs)})
	}
	if c.Graph.HopDepth < 1 {
		issues = append(issues, Issue{Field: "graph.hop_depth", Severity: SeverityError,
			Msg: fmt.Sprintf("hop_depth %d must be >= 1", c.Graph.HopDepth)})
	}
	for _, w := range []struct {
		key string
		val float64
	}{
		{"graph.boost_weight", c.Graph.BoostWeight},
		{"graph.weight_topical", c.Graph.WeightTopical},
		{"graph.weight_factual", c.Graph.WeightFactual},
		{"graph.weight_causal", c.Graph.WeightCausal},
		{"graph.weight_recurrent", c.Graph.WeightRecurrent},
	} {
		if !(w.val >= 0 && w.val <= 1) { // NaN-safe
			issues = append(issues, Issue{Field: w.key, Severity: SeverityError,
				Msg: fmt.Sprintf("graph weight %g must be in [0,1]", w.val)})
		}
	}

	// V3 — Wave-3 empiricism: the cross-encoder as final arbiter over
	// graph-injected neighbors is destructive (R@10 0.715→0.571).
	if c.Rerank.BlendWeight == 1.0 && c.Graph.Enabled {
		issues = append(issues, Issue{Field: "rerank.blend_weight", Severity: SeverityWarn,
			Msg: "blend_weight 1.0 with graph expansion enabled: pure cross-encoder order overrides graph-injected neighbors (Wave-3: destructive) — consider 0.5"})
	}

	// V8 and V13 retired with the rerank tuple (β3, design/01 §7 W2). Both keyed
	// on rerank.host as the config-side name of the cross-encoder dispatch:
	// V8 warned that "enabled without host" means the LLM-judge path without the
	// body heartbeat, V13 that a fallback-synthesis host is unprotected unless
	// that same cross-encoder is armed. With the key gone the question is a pool
	// question — whether a rerank-role row serves — and Validate never sees the
	// pool (config validation is parameter-pure). Neither check is rebuildable
	// here, and neither was a correctness gate: both are WARNs about a timeout
	// risk the runtime already meets by failing open (query.go, rrf/rerank.go).

	return issues
}

func validateDream(c *Config) []Issue {
	var issues []Issue

	// V6 — today's dream.SetBackoffConfig ignore semantics (dream.go:404-428),
	// pulled forward and made visible: invalid values keep the default.
	bc := &c.Dream.Backoff
	switch bc.Mode {
	case "exp", "log", "linear", "off":
	default:
		issues = append(issues, v6(bc, "dream.backoff_mode",
			fmt.Sprintf("backoff mode %q must be exp|log|linear|off", bc.Mode)))
	}
	if bc.Factor < 0 {
		issues = append(issues, v6(bc, "dream.backoff_factor",
			fmt.Sprintf("backoff factor %g must be >= 0", bc.Factor)))
	}
	if bc.Grace < 0 {
		issues = append(issues, v6(bc, "dream.backoff_grace",
			fmt.Sprintf("backoff grace %d must be >= 0", bc.Grace)))
	}
	if bc.CapHours <= 0 {
		issues = append(issues, v6(bc, "dream.backoff_cap",
			fmt.Sprintf("backoff cap %gh must be > 0", float64(bc.CapHours))))
	}
	if bc.MinHours < 0 {
		issues = append(issues, v6(bc, "dream.backoff_min",
			fmt.Sprintf("backoff min %gh must be >= 0", float64(bc.MinHours))))
	}
	if bc.InertOffset < 0 {
		issues = append(issues, v6(bc, "dream.backoff_inert_offset",
			fmt.Sprintf("backoff inert offset %d must be >= 0", bc.InertOffset)))
	}

	// V10 — today's scheduler clamp (Run: workers<1→1, >16→16), pulled into
	// Validate. The runtime clamp stays — this makes it visible at boot.
	if c.Dream.Parallelism < 1 {
		issues = append(issues, Issue{Field: "dream.parallelism", Severity: SeverityWarn,
			Msg: fmt.Sprintf("parallelism %d clamped to 1", c.Dream.Parallelism)})
		c.Dream.Parallelism = 1
	} else if c.Dream.Parallelism > 16 {
		issues = append(issues, Issue{Field: "dream.parallelism", Severity: SeverityWarn,
			Msg: fmt.Sprintf("parallelism %d clamped to 16", c.Dream.Parallelism)})
		c.Dream.Parallelism = 16
	}

	// V14 — dream.language shape. The value is INTERPOLATED into the daily
	// synthesis SYSTEM PROMPT (dream.langName), so free text here is a prompt
	// injection + prompt-length vector on a background pipeline nobody reads
	// before it runs. Normalize like every other case-insensitive key (trim +
	// lower, in place — the V-order normalization pattern above), then hard
	// ERROR on anything that is not a BCP-47-shaped tag: boot aborts, and a
	// Settings write is rejected instead of silently reaching the LLM.
	// Empty stays legal — it is the legacy-behavior default.
	c.Dream.Language = strings.ToLower(strings.TrimSpace(c.Dream.Language))
	if lang := c.Dream.Language; lang != "" {
		switch {
		case len(lang) > dreamLanguageMaxLen:
			issues = append(issues, Issue{Field: "dream.language", Severity: SeverityError,
				Msg: fmt.Sprintf("language %q is %d chars — max %d", lang, len(lang), dreamLanguageMaxLen)})
		case !dreamLanguageRe.MatchString(lang):
			issues = append(issues, Issue{Field: "dream.language", Severity: SeverityError,
				Msg: fmt.Sprintf("language %q must be a BCP-47-style tag (%s), e.g. \"de\", \"en\", \"pt-br\" — empty = legacy German report", lang, dreamLanguagePattern)})
		}
	}

	// V20 — dream.json_mode enum (own function: this block is at the cyclop
	// ceiling, and an enum check has no cross-field half to keep here).
	issues = append(issues, validateDreamJSONMode(c)...)

	// V15 — dream.link_floor_confidence range. The value becomes the raw
	// confidence of every link the LLM names without a strength signal; an
	// out-of-range float would either die at the write gate (silent no-op
	// pipeline) or claim impossible certainty. Same fatal class as the other
	// out-of-range knobs (e.g. _BLEND_WEIGHT outside [0,1]).
	if f := c.Dream.LinkFloorConfidence; f < 0 || f > 1 {
		issues = append(issues, Issue{Field: "dream.link_floor_confidence", Severity: SeverityError,
			Msg: fmt.Sprintf("link floor confidence %g must be within [0,1]", f)})
	}

	// V16c — dream.cycle_timeout floor, on the key itself. Its SIGN half is
	// gone from here: it was V16's class ("negative reads as configured but
	// serves the package default"), and that class is now the generic V17
	// walk over every duration key (validateDurations). Folding it there
	// rather than exempting this key keeps the block free of a "has its own
	// sign check" allowlist. 0 stays legal — it IS the documented "package
	// default" sentinel, and V17 leaves zero alone too.
	//
	// The floor half is key-specific and stays: a WARN in the V16b spirit,
	// not a clamp, because an
	// operator may knowingly run a corpus whose calls finish far inside their
	// ceilings. Below keywords + eval, though, the cycle deadline cuts the
	// link-writing stages before they can start, the block ends its cycle
	// without a cooldown and is re-picked next cycle — a silent, self-
	// sustaining starvation loop that would otherwise surface only as a V16b
	// WARN filed under dream.temporal_timeout, a key the operator never
	// touched. This names the key they did change. The `d > 0` guard already
	// kept negatives out of this branch before the fold.
	if d, floor := c.Dream.CycleTimeout, dream.KeywordsTimeout+dream.DreamTimeout; d > 0 && d < floor {
		issues = append(issues, Issue{Field: "dream.cycle_timeout", Severity: SeverityWarn,
			Msg: fmt.Sprintf("cycle timeout %v is below the %v floor (keywords %v + eval %v) — the cycle deadline cuts the link-writing stages before they run, so the block ends without a cooldown and is re-picked every cycle",
				d, floor, dream.KeywordsTimeout, dream.DreamTimeout)})
	}

	// V16 — dream.temporal_timeout sign — folded into V17 for the same reason
	// as V16c's: one generic registry walk instead of a per-key check that
	// the other 35 seconds keys never got. What remains here is V16b, the
	// key-specific cycle-budget WARN.
	//
	// The `d >= 0` guard preserves the sign check's ownership of negative
	// values now that its branch is gone — it replaces the `else` that branch
	// used to provide. It is load-bearing, not decoration:
	// temporalTimeoutBudgetOf turns NEGATIVE once dream.cycle_timeout is set
	// below keywords + eval, so without it a value V17 already ERRORs on
	// would collect a second, misleading budget WARN on top.
	if d := c.Dream.TemporalTimeout; d >= 0 && d > temporalTimeoutBudgetOf(c) {
		// V16b — the cycle-budget WARN. Not a clamp: the operator may
		// know their keyword/eval calls finish far inside their own
		// ceilings, and the runtime already fails safely (the cycle
		// deadline cuts the call). Warn only, in the V10 spirit of
		// making a downstream truncation visible at boot.
		//
		// Effective whole-cycle deadline: the hot dream.cycle_timeout wins
		// (CycleTimeoutOf), else the package CycleTimeout default. The
		// budget and the "cannot take effect" gate read it, so a raised
		// cycle timeout widens the window instead of warning spuriously.
		cycle := dream.CycleTimeoutOf(c.Dream.CycleTimeout)
		msg := fmt.Sprintf("temporal timeout %v leaves only %v of the %v dream cycle for keywords (%v) + eval (%v) — the link-writing stages can be starved",
			d, cycle-d, cycle, dream.KeywordsTimeout, dream.DreamTimeout)
		if d >= cycle {
			msg = fmt.Sprintf("temporal timeout %v is not below the %v dream cycle budget — the cycle deadline cuts the Phase-2 call first, so the value cannot take effect",
				d, cycle)
		}
		issues = append(issues, Issue{Field: "dream.temporal_timeout", Severity: SeverityWarn, Msg: msg})
	}

	// V18 — dream.num_predict sign and floor. The sign half is NOT V17's:
	// that walk is typed, it visits typDuration fields only, and this key is
	// a plain token count (typInt). Same failure mode though — a negative
	// value renders as a configured cap in the settings surface while
	// DreamOptionsFor serves the package default — so it gets the same ERROR
	// class, on its own check rather than by widening the duration walk to a
	// type whose zero and negative values mean something different per key.
	// 0 stays legal: it IS the documented "package default" sentinel.
	//
	// The floor half is a WARN in the V16b/V16c spirit, never a clamp. Below
	// dream.DefaultNumPredict the operator reopens exactly the regression the
	// default was measured against: five links in the object-map drift form
	// cost ~500 tokens pretty-printed, and a cap under that truncates the
	// answer mid-JSON, which the pipeline cannot tell from malformed output —
	// it books a parse error, a 5-minute transient cooldown and a re-pick,
	// burning one eval call per block per five minutes with only
	// metadata.cap_hit in the llmlog to say why. It stays a WARN because an
	// install whose backend answers compactly may knowingly buy latency with
	// a shorter cap.
	if n := c.Dream.NumPredict; n < 0 {
		issues = append(issues, Issue{Field: "dream.num_predict", Severity: SeverityError,
			Msg: fmt.Sprintf("num_predict %d must be >= 0 — 0 is the package-default sentinel (%d tokens), a negative value renders as a configured cap while the default is served",
				n, dream.DefaultNumPredict)})
	} else if n > 0 && n < dream.DefaultNumPredict {
		issues = append(issues, Issue{Field: "dream.num_predict", Severity: SeverityWarn,
			Msg: fmt.Sprintf("num_predict %d is below the built-in default of %d tokens — five links in the object-map drift form cost ~500 tokens pretty-printed, so answers can be truncated mid-JSON and are then indistinguishable from malformed output (parse error, transient cooldown, re-pick; metadata.cap_hit is the only signal)",
				n, dream.DefaultNumPredict)})
	}

	// V19 — dream.eval_cap_retry_factor sign. Not V17's walk (that one is
	// typed and visits duration keys only) and not V18's either (that check
	// owns a token count whose 0 is a "package default" sentinel): here the
	// documented off-switch is the whole range <= 1, so 0 and 1 are ordinary
	// legal values meaning "no retry" and need no warning. A NEGATIVE factor
	// is the one shape with no reading at all — it renders in the settings
	// surface as a configured multiplier while the retry is off, and if it
	// ever reached the scaling it would SHRINK the cap it is meant to widen.
	// Fatal, the class every other "renders as configured, serves something
	// else" knob gets.
	if f := c.Dream.EvalCapRetryFactor; f < 0 {
		issues = append(issues, Issue{Field: "dream.eval_cap_retry_factor", Severity: SeverityError,
			Msg: fmt.Sprintf("eval cap retry factor %g must be >= 0 — values <= 1 disable the retry, a negative one renders as a configured multiplier while disabling it", f)})
	}

	return issues
}

// validateDreamJSONMode is V20 — the dream.json_mode enum. It normalizes like
// every other case-insensitive key (trim + lower, IN PLACE, the V-order
// pattern) and then accepts exactly "" (the legacy sentinel), "strict" and
// "off". The two accepted spellings are the dream package's own constants, the
// same relation V16c has to KeywordsTimeout: config validates against the
// consumer, never against a second copy of its vocabulary.
//
// Deliberately NOT the V6 warn-and-reset shape. There an unknown value
// silently becomes the registry default — and the default here is strict, the
// setting an operator reaching for this key is trying to leave. A typo ("of",
// "false", "plain") would keep the grammar on the wire while GET /api/settings
// renders their value, and the symptom they are chasing is a backend decoding
// at half speed, which no log line attributes to this key. Fatal instead: boot
// aborts, the settings PUT is a 422.
func validateDreamJSONMode(c *Config) []Issue {
	c.Dream.JSONMode = strings.ToLower(strings.TrimSpace(c.Dream.JSONMode))
	switch c.Dream.JSONMode {
	case "", dream.JSONModeStrict, dream.JSONModeOff:
		return nil
	default:
		return []Issue{{Field: "dream.json_mode", Severity: SeverityError,
			Msg: fmt.Sprintf("json mode %q must be %q or %q — empty reads as %q (legacy default)",
				c.Dream.JSONMode, dream.JSONModeStrict, dream.JSONModeOff, dream.JSONModeStrict)}}
	}
}

// v6 emits the V6 WARN for one back-off field and resets it to its registry
// default (the same value SetBackoffConfig would have kept by ignoring the
// input).
func v6(bc *BackoffConfig, key, msg string) Issue {
	def := defaultFor(key)
	switch key {
	case "dream.backoff_mode":
		bc.Mode = def.(string)
	case "dream.backoff_factor":
		bc.Factor = def.(float64)
	case "dream.backoff_grace":
		bc.Grace = def.(int)
	case "dream.backoff_cap":
		bc.CapHours = def.(Hours)
	case "dream.backoff_min":
		bc.MinHours = def.(Hours)
	case "dream.backoff_inert_offset":
		bc.InertOffset = def.(int)
	}
	return Issue{Field: key, Severity: SeverityWarn, Msg: msg + " — using default"}
}

// defaultFor returns the parsed registry default for a key.
func defaultFor(key string) any {
	for _, e := range registry() {
		if e.Key == key {
			return e.defVal
		}
	}
	panic("config: unknown registry key " + key)
}
