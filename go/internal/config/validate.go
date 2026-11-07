package config

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
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
	issues = append(issues, validateDistill(c)...)       // V22-V25, V27-V33
	// V37 (W06-8): the foreign-load detector's floors and ratio range.
	issues = append(issues, validateStatusForeignLoad(c)...)
	// V38, V39 (W02-02): the gateway group's floors, caps and budget relation;
	// V40 (W01-6): the model-key door's limits.
	issues = append(issues, validateGateway(c)...)
	// V41 (W01-5): the consumer administration's grace ceiling and page floors.
	issues = append(issues, validateGatewayConsumers(c)...)
	// V42 (A03-W3): dispatch.external_order names a known admission order.
	issues = append(issues, validateDispatchExternalOrder(c)...)
	// V43 (A03-W17): dispatch.external_max_wait at most 45 s.
	issues = append(issues, validateDispatchExternalMaxWait(c)...)
	// V44 (W06-9): gateway.body_logging names a known body policy.
	issues = append(issues, validateGatewayBodyLogging(c)...)
	// V45 (F-W8): federation.max_depth in 1..fedhop.MaxHopEntries.
	issues = append(issues, validateFederation(c)...)
	// V46 (A03-W6): dispatch.external_retry_after_steps parses, at most
	// dispatch.external_lease_max.
	issues = append(issues, validateDispatchRetryAfterSteps(c)...)
	// V47 (W06-10): the per-consumer meter's limits >= 1, the XU reserve
	// ratio in [0, 1).
	issues = append(issues, validateGatewayMeter(c)...)
	issues = append(issues, validateGatewayConcurrency(c)...)
	// V49 (W01-8): gateway.inflight_sweep_interval at least 1 s.
	issues = append(issues, validateGatewayInflightSweep(c)...)
	// V50 (W06-13): the daily token defaults >= 1, the envelope factors >= 1.
	issues = append(issues, validateGatewayBudgets(c)...)
	// V51 (W06-14): gateway.consumer_monthly_cost_usd_default > 0, finite.
	issues = append(issues, validateGatewayCostBudget(c)...)
	// V35 runs LAST, and the position is the mechanism, not a style choice.
	// Three of the int keys are served by NORMALIZING passes that write the
	// value back into the struct (v6 for dream.backoff_grace and
	// dream.backoff_inert_offset, V10's clamp for dream.parallelism), and
	// validateDream stands third in this list. Running last, V35 sees what
	// those passes left behind, so a normalized value is no longer negative
	// and gets no second, contradicting report. The invariant that follows is
	// the statement of the walk: after a Validate without SeverityError, no
	// int field of the registry is negative — either a normalizing pass
	// corrected it (WARN, boot continues, surface == consumer) or V35 filed
	// the error (boot abort / 422).
	issues = append(issues, validateIntSigns(c)...) // V35
	return issues
}

// validateDistill holds the cross-field invariants of the distiller group
// (design/03 §5.4, §5.5, §6.4; wave A03-W03-3, plus V27 from wave C2-8,
// V28-V30 from wave A02-4, V31 from wave A02-6, V33 from wave C5-E and V34 from
// wave C6-A).
// All of them are fatal: boot drops the offending
// override, a settings PUT is a 422 — the class every "renders as configured,
// acts as something else" knob in this file gets.
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
	issues = append(issues, validateDistillNoveltyFloor(d)...) // V33
	issues = append(issues, validateDistillConcurrency(d)...)  // V34
	issues = append(issues, validateDistillSpendWindow(d)...)  // V32
	issues = append(issues, validateDistillCtxSource(d)...)    // V28, V29, V30
	issues = append(issues, validateDistillDryRunDir(d)...)    // V31
	return issues
}

// validateDistillDryRunDir is V31: the dry-run dump target must be an absolute
// path, or empty.
//
// THE KEY IS AN EGRESS TARGET, not a convenience path (§5 BA13). It receives the
// raw session prose the credential detector did NOT catch, so a value that lands
// in a git working copy is one `git push` away from publishing it. It shipped in
// wave A02-6 as the only key of this group with no rule at all, on the reasoning
// that a validator sees only the /api/settings path — which is wrong at the code
// and was measured to be wrong: cmd/ctxd/main.go runs FromEnv + Validate and
// exits on SeverityError, build.go validates the assembled config, store.go the
// settings write. All three writers pass through here.
//
// EMPTY STAYS LEGAL and is not an oversight: it is the documented "no plaintext
// dump" setting, the one BA13's own resolution asks for (counted figures and
// chunk hashes stay in the journal, plaintext only on an explicit operator
// request).
//
// THE RULE IS DELIBERATELY SYNTACTIC. Whether a path lies inside a working copy
// is a fact about the file system at the moment of use — a symlink, a mount or a
// `git init` changes the answer without the value changing — and Validate is
// parameter-pure and runs at boot and on every settings write. That half belongs
// to the arm and is enforced there (events.distillDumpDir, which resolves
// symlinks and walks the ancestors before every tick, and again after the
// directory is created). Two layers, one question each; neither covers the
// other's case.
//
// A relative path is the one shape that is wrong under every file system: it
// resolves against the daemon's working directory, which in a development
// install IS the repository.
func validateDistillDryRunDir(d *DistillConfig) []Issue {
	dir := strings.TrimSpace(d.DryRunDir)
	if dir == "" || filepath.IsAbs(dir) {
		return nil
	}
	return []Issue{{Field: "distill.dryrun_dir", Severity: SeverityError,
		Msg: fmt.Sprintf("distill.dryrun_dir %q is not an absolute path — the dry-run dump carries raw session prose, and a relative target resolves against the daemon's working directory (in a development install: the repository). Give an absolute path outside every git working copy, or leave the key empty to switch the plaintext dump off",
			d.DryRunDir)}}
}

// builtinTypeSet is the COMPILED block-type registry — the fail-safe floor
// blocktype.NewRegistry() serves before any DB generation overlays it
// (blocktype/registry.go:126-132: no pool, no query, no goroutine). V29
// validates against THIS set and never against the live one, for exactly the
// reason V27 keeps derived.ReservedCategories code-owned: context_block_types
// is a table an operator can edit with SQL, so a rule that read the live rows
// could be satisfied by INSERTing the very row it exists to refuse. The list
// stays a mechanism, and the key stays a choice among compiled names.
//
// It is also the only shape available here: Validate is parameter-pure — no
// context, no pool, no request scope — so a tenant-resolved generation has no
// meaning at this call site. Every distill.* key is tenancy:"global-only"
// anyway, so the base generation IS the arm's world.
//
// OnceValue because Validate runs at every boot and on every settings write
// while the compiled set is a constant of the build.
var builtinTypeSet = sync.OnceValue(func() *blocktype.Set {
	return blocktype.NewRegistry().Snapshot()
})

// validateDistillCtxSource holds the three cross-field invariants the
// ctx-checkpoint source brings with it (design D-02 §7.1, wave A02-4). All
// three are fatal, the class of the group around them: boot drops the
// offending override, a settings PUT is a 422.
//
// The source has no reader and the arm has no code yet. The rules ship WITH
// the keys for the group's stated reason — a rule added later would be a rule
// added against an already-configurable key — and because two of the three
// describe a state that is not recoverable after the fact: a merged watermark
// series has silently skipped ranges, and blocks written under the wrong type
// carry that type until somebody notices.
func validateDistillCtxSource(d *DistillConfig) []Issue {
	var issues []Issue
	issues = append(issues, validateDistillCtxLabel(d)...)       // V28
	issues = append(issues, validateDistillBlockType(d)...)      // V29
	issues = append(issues, validateDistillCtxSensitivity(d)...) // V30
	return issues
}

// validateDistillCtxLabel is V28: the arm's source label must name something.
//
// A label is the stable half of a journal source_key ("<label>:<session>",
// §3.1) and therefore of the DERIVED watermark. An empty label makes every
// source_key start with ":", so a source added next to this one shares its
// series: each run advances the other's watermark, and the ranges in between
// are not re-read but silently skipped. That is why the refusal is fatal
// rather than a warn — the damage is a silent data merge, not a cosmetic
// default.
//
// V28 kept its number and its registration when the arm's second source was
// retired in v5.17.0 (T05-8b): the collision half it also carried compared
// this label against distill.source_label, and that key no longer exists.
// What survives is the half that was never about the pair — a source_key needs
// a name — and the message it raises is unchanged, because the reason it gives
// is unchanged.
//
// THE VALUE IS NOT NORMALIZED IN PLACE, and that half is still deliberate:
// distill.ctx_source_label goes into the source_key verbatim, so trimming it
// here would RENAME a live source (new key, no journal history, restart at
// initial_backfill_rows — the consequence its own doc comment carries), and
// that is not a validator's decision to make. V27 normalizes distill.category
// because the value is half an upsert identity that would otherwise CREATE a
// divergent category; a label creates nothing. TrimSpace is therefore applied
// to the CHECK only: "   " is not a name, and accepting it would let the
// refusal be bought with a space bar.
func validateDistillCtxLabel(d *DistillConfig) []Issue {
	if strings.TrimSpace(d.CtxSourceLabel) == "" {
		return []Issue{{Field: "distill.ctx_source_label", Severity: SeverityError,
			Msg: "distill.ctx_source_label must not be empty — it is the stable half of the ctx-checkpoint source's journal key, and an empty label would merge the watermark series of every configured source into one"}}
	}
	return nil
}

// validateDistillBlockType is V29: distill.block_type must name a COMPILED
// block type that the arm may actually write under.
//
// The key decides three properties of every written block that the arm's own
// code cannot: whether the block is retrievable at all, whether it is
// retrievable UNDAMPED, and whether the dedup guard may archive the originals
// the block quotes. Four refusals, each closing a different failure:
//
//  1. an unknown name (the empty string included): the write would name no
//     registry row and fall through to the DEFAULT type, which is full-pass
//     knowledge — the widest posture in the set, reached by a typo.
//  2. guard.check: a derivative that takes part in the dedup guard archives
//     the very originals it quotes, and it can archive itself.
//  3. full-pass: distilled transcript and tool material would enter every
//     query's candidate pool at full weight. M138's doctrine is verbatim that
//     summarising attacker-shapable output does not launder it.
//  4. excluded OUTSIDE the derived layer: the arm would write blocks nobody
//     can retrieve — the silent null decision EA-10 names.
//
// WHY (4) CARRIES AN EXCEPTION, and why it is not a hole: the derived types are
// excluded TODAY by masterplan K7 / board decision E-4 — a declared, reversible
// DATA position held until the visibility pilots (X-W4/X-W5) measure a damping
// factor, at which point the registry rows flip to damped. D-02 §7.1 wrote the
// rule as "Kind not in {full-pass, excluded}" at a time when the type was
// planned as damped from the start; K7 moved the start state afterwards, so the
// literal rule would now refuse its OWN default and turn every boot into a
// validation failure. The exception restores what the rule meant: excluded is
// refused wherever it is the type's PERMANENT shape (checkpoint, system-meta),
// and admitted only for the layer whose exclusion is a dated decision with a
// scheduled reversal.
//
// The exception is read from CODE on both sides — derived.IsDerivedType and the
// compiled set — never from a config value, so nothing an operator writes can
// move a type into it.
func validateDistillBlockType(d *DistillConfig) []Issue {
	// Normalized in place for the V27 reason, sharpened by what this value
	// becomes: the block's type_name verbatim. A padded or shift-keyed spelling
	// would name no row at write time and take path (1) above at runtime,
	// AFTER the settings write was accepted.
	d.BlockType = strings.ToLower(strings.TrimSpace(d.BlockType))
	fail := func(format string, args ...any) []Issue {
		return []Issue{{Field: "distill.block_type", Severity: SeverityError, Msg: fmt.Sprintf(format, args...)}}
	}
	p, ok := builtinTypeSet().Resolve(d.BlockType)
	switch {
	case !ok:
		return fail("distill.block_type %q is not a compiled block type (%s) — a name the registry does not know falls through to the default type at write time, which is the widest retrieval posture in the set",
			d.BlockType, strings.Join(builtinTypeSet().Names(), ", "))
	case p.Guard.Check:
		return fail("distill.block_type %q takes part in the dedup guard (guard.check) — a derived block that guards would archive the very originals it quotes, and eventually itself",
			d.BlockType)
	case p.Retrieval.Kind == blocktype.RetrievalFullPass:
		return fail("distill.block_type %q is retrieval %q — distilled transcript and tool material would enter every query's candidate pool at full weight, and summarising foreign output does not make it first-party",
			d.BlockType, p.Retrieval.Kind)
	case p.Retrieval.Kind == blocktype.RetrievalExcluded && !derived.IsDerivedType(d.BlockType):
		return fail("distill.block_type %q is retrieval %q and does not belong to the derived layer (%s) — the arm would write blocks no query can ever return, which is a null decision rather than a setting",
			d.BlockType, p.Retrieval.Kind, strings.Join(derived.DerivedTypeNames(), ", "))
	}
	return nil
}

// validateDistillCtxSensitivity is V30 (decision EA-4): the block sensitivity
// may not be lowered while the ctx-checkpoint source is switched on.
//
// V23 puts the hard floor at "internal", which leaves TWO steps of descent
// below the "credentials" default — and this axis CREATES the pressure to take
// them. Because llm.MaxSensitivity folds over the final prompt set
// (llm/synthesize.go), a SINGLE retrieved insight block at "credentials" locks
// every query out of external synthesis; at the corpus rates this arm produces,
// external synthesis would be off nearly always. The obvious relief is to lower
// this key — and that relief opens the distilled session text, the most
// sensitive material the store holds about its own operator, to every external
// backend the pool has.
//
// So the descent stays legal exactly while the source is OFF: lowering it is an
// act BEFORE switching the arm on, never a knob during operation. The
// availability price (external synthesis effectively disabled) is a named cost
// in the operating report, not a silent side effect.
//
// THE OFFENDER IS THE SENSITIVITY KEY, not the switch, and that decides how a
// bad boot recovers: build.go's dropOffenders withdraws the override it can
// attribute, and withdrawing the sensitivity restores "credentials" — the
// fail-closed direction. Attributing the switch instead would leave the lowered
// value standing under an arm that is merely off.
//
// The counter-precedent the design names explicitly: rootmap/run.go:184-188
// decided the OTHER way for its derived block (explicit "internal", so external
// backends stay reachable). It is a different trade — that block is a map of
// the operator's own corpus structure, not a distillate of their transcripts.
func validateDistillCtxSensitivity(d *DistillConfig) []Issue {
	if !d.CtxEnabled || d.BlockSensitivity.Rank() >= backends.SensCredentials.Rank() {
		return nil
	}
	return []Issue{{Field: "distill.block_sensitivity", Severity: SeverityError,
		Msg: fmt.Sprintf("distill.block_sensitivity %q is below %q while distill.ctx_enabled is on — the distillate of this operator's own sessions would become eligible for every external backend the pool has; lower it only with the ctx source switched off",
			d.BlockSensitivity, backends.SensCredentials)}}
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

// validateDistillSpendWindow is V32: the spend window may not be zero.
//
// It is the DENOMINATOR of both ceilings, not a third switch beside them.
// `created_at > now() - make_interval(secs => 0)` is `created_at > now()`, i.e.
// the empty set, so both axes read 0 and every budget passes — measured: 1 000
// own rows at 30 000 ms with spend_max_calls = 40 and spend_max_gpu_seconds =
// 240 armed closed `ok` with `call_budget = 40`. That is the guard's ONLY
// fail-open path; every other failure in it (an unreadable back-off, an
// unreadable window) stops the arm.
//
// The design does not define this zero, which is why it is refused rather than
// documented: §4.6 names exactly one kill switch ("spend_max_calls = 0 ist der
// Kill-Switch", design/02:1522), and docs/operations.md spells the 0 reading
// out for every other key of the group while saying nothing for _SPEND_WINDOW.
// The class is the one validateDistillCounters names for its sizing keys — "a
// silent second off-switch next to distill.enabled, one that the settings
// surface renders as a configured size" — and the settings surface does exactly
// that here: it keeps rendering both budgets while neither can ever bind.
//
// Only the zero. The negative half is V17's generic duration walk, so a
// per-key sign check would double-report (the shape validateEmbedBackoff
// established for the same question on embed_backfill.backoff_base).
func validateDistillSpendWindow(d *DistillConfig) []Issue {
	if d.SpendWindow != 0 {
		return nil
	}
	return []Issue{{Field: "distill.spend_window", Severity: SeverityError,
		Msg: "distill.spend_window must be > 0 — it is the window BOTH spend ceilings are counted in, so 0 makes the guard count an empty range and pass every budget while the settings surface keeps rendering spend_max_calls and spend_max_gpu_seconds as configured limits; switch the guard off with spend_max_calls = 0 and spend_max_gpu_seconds = 0 instead"}}
}

// validateDistillCounters is V25: the FLOOR half of the group — the counted
// keys whose zero is not a legal value. The SIGN half went to V35
// (validateIntSigns) in the wave that made that walk generic over every int
// key, which is why the loop condition below reads ">= 0 && < min" and why
// the seven keys with a documented zero have no row here any more.
//
// Two readings of zero, deliberately kept apart:
//
//   - spend_max_calls 0 is the DOCUMENTED kill switch (guard off, effective
//     from the next tick because the snapshot is re-read per iteration), the
//     retention pairs' 0 is the recall_check no-op ("keep forever"),
//     max_blocks_per_root's 0 is the shard cap being off (wave W-L3),
//     spend_max_gpu_seconds' 0 is the GPU axis off, initial_backfill_rows' 0
//     is the documented cold start at the head of the source, and
//     min_row_runes' 0 is no substance threshold at all. Those seven stay
//     legal and are therefore NOT listed below: with no floor to enforce,
//     their rows would have been pure sign checks, and a pure sign check next
//     to V35's walk double-reports the field. Their negatives are refused by
//     V35 on the same house grounds this group always gave — a negative
//     renders as a configured number while acting as the off-switch.
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
		// num_predict joins the sizing half rather than the zero-is-documented
		// half (wave A02-4): 0 predict tokens is not "no ceiling", it is a call
		// that can produce no answer — a second, silent off-switch next to
		// distill.ctx_enabled that the settings surface renders as a budget.
		{"distill.num_predict", d.NumPredict, 1, "a call with no answer budget can produce no insight"},
		{"distill.breaker_failures", d.BreakerFailures, 1, "the breaker would stand open before the first attempt"},
	} {
		if k.val >= 0 && k.val < k.min {
			issues = append(issues, Issue{Field: k.key, Severity: SeverityError,
				Msg: fmt.Sprintf("%s %d must be >= %d — %s, and the settings surface would still render it as a configured value",
					k.key, k.val, k.min, k.note)})
		}
	}

	return issues
}

// validateDistillNoveltyFloor is V33 (wave C5-E): the per-claim substance floor
// has to stay inside the range its own quantity has.
//
// novelty is |claim tokens \ quote tokens| / |claim tokens| (derived.Adequacy),
// so it lives in [0, 1] by construction. Both ends are refused for the SAME
// house reason and with different consequences:
//
//   - A NEGATIVE floor renders as a configured threshold and acts as an
//     off-switch, which the group already has one documented reading for (0).
//   - A floor ABOVE 1 is the sharper failure: no claim can reach it, so the arm
//     keeps calling, keeps paying GPU seconds and writes NOTHING, while the
//     settings surface still shows a threshold. That is not "the arm is off",
//     it is "the arm is silently empty" — the shape the arm's own error class
//     for a permanently braked run cannot distinguish from a bad generator.
//
// Exactly 1 stays legal: it says "every token of the claim must be the model's
// own", which is a defensible, extreme policy and not a nonsense value.
func validateDistillNoveltyFloor(d *DistillConfig) []Issue {
	if d.NoveltyFloor >= 0 && d.NoveltyFloor <= 1 {
		return nil
	}
	return []Issue{{Field: "distill.novelty_floor", Severity: SeverityError,
		Msg: fmt.Sprintf("distill.novelty_floor %v must be within [0, 1] — novelty is a share of a claim's token set (derived.Adequacy), so a negative value would be a second, silent off-switch next to the documented 0, and a value above 1 would let the arm keep calling while every claim it produces is discarded",
			d.NoveltyFloor)}}
}

// validateDistillConcurrency is V34 (wave C6-A): the tick's source fan-out has
// to stay inside the range the arm can actually serve.
//
// BOTH ENDS ARE REFUSED RATHER THAN CLAMPED, which is the V33 shape and not the
// V10 one, and the difference is the mutability class. dream.parallelism is
// mut:"restart" and its clamp is a boot-time WARN an operator reads in the same
// log line as the start-up; this key is hot, so a clamp would answer a settings
// PUT with 200 and then run a number the surface never shows. That is the
// "renders as configured, acts as something else" class this file refuses.
//
//   - BELOW 1 is the off-switch shape: 0 would be a tick that touches no source
//     at all while every gate reports healthy — the silent null operation D-02
//     §4.2.1(b) wants to see red — and the arm HAS an off-switch, distill.enabled,
//     which journals its answer. The NEGATIVE half of that range belongs to V35
//     (validateIntSigns), the generic sign walk over every int key, so the guard
//     below reads "< 0 is not mine": the floor is this check's statement, the
//     sign is the walk's, and no value is reported twice.
//   - ABOVE DistillMaxConcurrency is the starvation shape: the pool is 20
//     connections for the whole daemon (store.NewPool), so a fan-out beyond the
//     bound queues guard, digest, dream and the HTTP surface behind one
//     background arm. The ceiling is the arm's, never the serving side's — what
//     a backend takes in parallel is the operator's number, and it is capped
//     here only where the DATABASE stops.
//
// The runtime keeps its own clamp for the unvalidated snapshot (events.
// distillConcurrency, the distillInterval shape) — not as a second policy, but
// because a hand-built Config that never passed this function must not turn a
// zero into an arm that silently does nothing.
func validateDistillConcurrency(d *DistillConfig) []Issue {
	if d.Concurrency < 0 || (d.Concurrency >= 1 && d.Concurrency <= DistillMaxConcurrency) {
		return nil
	}
	return []Issue{{Field: "distill.concurrency", Severity: SeverityError,
		Msg: fmt.Sprintf("distill.concurrency %d must be within [1, %d] — it counts the SOURCES of one tick that may run at the same time, so a value below 1 would be a silent second off-switch next to distill.enabled (a tick that touches no source while every gate reports healthy), and a value above %d would queue every other arm behind the distiller on a %d-connection pool; inside one source the arm stays sequential either way, because only a complete batch prefix may move a watermark",
			d.Concurrency, DistillMaxConcurrency, DistillMaxConcurrency, 20)}}
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

// validateStatusForeignLoad is V37 (waves W06-8, W06-8b): the floors and the
// ratio range of the foreign-load detector's policy keys (design/06 §4.9).
// The signs stay with the generic walks (V17 for the timeout and the
// interval, V35 for the two counts); this check owns the values that parse,
// are not negative, and still cannot mean what the surface would render:
//   - status.foreign_load_timeout 0: every /metrics GET fails before it is
//     sent, so the detector reports probe errors forever instead of readings.
//   - status.foreign_load_interval 0 (W06-8b): the detector's ticker would
//     start the next round without a pause.
//   - status.foreign_load_interval + status.foreign_load_timeout >= 1 h
//     (W06-8b, cross-field): that sum is the hourly check's grace, and the
//     rollup read of hour H is due grace after H ends — at or past the next
//     boundary, whose round has already replaced H, so no hour is ever
//     compared and nothing says so.
//   - status.foreign_load_hysteresis_ticks 0: a target would turn suspected
//     and clear on the same round, one WARN pair per tick.
//   - status.foreign_load_hourly_min_ratio outside [0,1] (float keys have no
//     generic walk): a negative tolerance flags every hour, one above 1 hides
//     an hour in which foreign requests outnumber ctx's own, and NaN — which
//     ParseFloat accepts and every comparison rejects — switches the hourly
//     verdict off silently.
func validateStatusForeignLoad(c *Config) []Issue {
	var issues []Issue
	if c.Status.ForeignLoadTimeout == 0 {
		issues = append(issues, Issue{Field: "status.foreign_load_timeout", Severity: SeverityError,
			Msg: "status.foreign_load_timeout must be >= 1 second — 0 fails every foreign-load probe before it is sent"})
	}
	if c.Status.ForeignLoadInterval == 0 {
		issues = append(issues, Issue{Field: "status.foreign_load_interval", Severity: SeverityError,
			Msg: "status.foreign_load_interval must be >= 1 second — 0 would start the next foreign-load probe round without a pause"})
	}
	if iv, to := c.Status.ForeignLoadInterval, c.Status.ForeignLoadTimeout; iv > 0 && to >= 0 && iv+to >= time.Hour {
		issues = append(issues, Issue{Field: "status.foreign_load_interval", Severity: SeverityError,
			Msg: fmt.Sprintf("status.foreign_load_interval (%v) + status.foreign_load_timeout (%v) must stay below 1 hour — the hourly foreign-load check would never read an hour", iv, to)})
	}
	if c.Status.ForeignLoadHysteresisTicks == 0 {
		issues = append(issues, Issue{Field: "status.foreign_load_hysteresis_ticks", Severity: SeverityError,
			Msg: "status.foreign_load_hysteresis_ticks must be >= 1 — 0 would suspect and clear a target in the same probe round"})
	}
	if r := c.Status.ForeignLoadHourlyMinRatio; math.IsNaN(r) || r < 0 || r > 1 {
		issues = append(issues, Issue{Field: "status.foreign_load_hourly_min_ratio", Severity: SeverityError,
			Msg: fmt.Sprintf("status.foreign_load_hourly_min_ratio %g must be in [0,1]", r)})
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

// validateIntSigns is V35: the sign check over EVERY count-typed key (typInt)
// in the registry — the second half of the statement V17 makes for durations,
// and built exactly like it.
//
// The failure mode is the same one V17 names, one type over: a negative count
// PARSES (parseIntValue is a bare strconv.Atoi, load.go), lands in the struct,
// and renders as a configured number in the settings surface, while every
// consumer that reads "<= 0" as "unset"/"off" serves something else. Before
// this walk, 62 of the 90 int keys had no sign check at all — among them
// recall_check.leg_timeout_ms, recall_check.park_max_ms and
// scheduler.llmlog_retention_days, where the divergence is a silently
// different retention or a leg that never times out.
//
// Generic on purpose, and for the reason V17 states above: the 16 hand-written
// sign checks this wave FOLDED into the walk (V9's rate-limit loop, V9b-V9e,
// rerank.max_docs, V25's seven min:0 rows, V18's sign half) would otherwise
// have to live on an allowlist maintained in lockstep with every future
// per-key check, and forgetting one double-reports the same field. The three
// checks that kept a FLOOR (graph.hop_depth's 1, V25's min:1 rows, V34's
// [1, DistillMaxConcurrency] range) were narrowed to ">= 0 && < floor"
// instead: the floor is theirs, the sign is this walk's.
//
// 0 is NOT checked — the same limit V17 gives itself. What zero means is per
// key (the documented kill switch of distill.spend_max_calls, "no blob
// staging" for pool.blob_stage_max_bytes, "unset" for
// pool.external_num_ctx_fallback, the package-default sentinel of
// dream.num_predict), and those readings live in the field docs in config.go,
// not here.
//
// NOT covered, deliberately: the three typHours keys (parseCooldownHours
// refuses a negative in both branches, load.go — the value never lands) and
// typFloat, whose members are thresholds in [0,1] with their own range checks
// (V19, rerank.blend_weight, the graph weights).
//
// The type assertion is safe by construction, like V17's: typInt is
// reflect.TypeOf(int(0)) (registry.go), so every entry carrying it has an int
// leaf field.
//
// Field MUST stay the canonical registry key: build.go's dropOffenders
// attributes a failing override by Field, and an error it cannot attribute
// withdraws ALL DB overrides of that generation instead of just the offender.
func validateIntSigns(c *Config) []Issue {
	var issues []Issue
	rv := reflect.ValueOf(c).Elem()
	for _, e := range registry() {
		if e.typ != typInt {
			continue
		}
		if n := rv.FieldByIndex(e.path).Interface().(int); n < 0 {
			issues = append(issues, Issue{Field: e.Key, Severity: SeverityError,
				Msg: fmt.Sprintf("%s %d must be >= 0 — a negative count renders as a configured value in the settings surface while the consumer reads it as unset or off and serves its own default (what 0 means stays per key)",
					e.Key, n)})
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
//     openai, ollama, anthropic, rerank.
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

	// V9's rate-limit loop (query.rate_limit_write, query.rate_limit_read,
	// pool.blob_rate_limit_write) and the four single-key sign checks V9d
	// (pool.blob_stage_max_bytes), V9e (pool.blob_scan_max_bytes), V9b
	// (pool.external_num_ctx_fallback) and V9c (pool.openrouter_window_ttl)
	// were FOLDED into V35 (validateIntSigns) — the generic sign walk over
	// every int key, for the reason V17 gives for the same fold on durations:
	// a per-key allowlist next to a generic walk double-reports the field.
	// None of them said anything the walk does not say; what each said about
	// its own ZERO (staging off, scan off, "unset", discovery off) is a
	// statement about the key, not about the check, and now lives in the field
	// docs of the four PoolConfig fields in config.go, where descriptions.go
	// mirrors it. The three rate limits said nothing about their zero — the
	// 0-is-off convention is already in their own field docs.

	return issues
}

func validateRerankGraph(c *Config) []Issue {
	var issues []Issue

	// V9 — range garbage produces silent scoring chaos, not a crash.
	if !(c.Rerank.BlendWeight >= 0 && c.Rerank.BlendWeight <= 1) { // NaN-safe
		issues = append(issues, Issue{Field: "rerank.blend_weight", Severity: SeverityError,
			Msg: fmt.Sprintf("blend_weight %g must be in [0,1]", c.Rerank.BlendWeight)})
	}
	// rerank.max_docs was a pure sign check and folded into V35
	// (validateIntSigns) with the other fifteen.
	//
	// graph.hop_depth keeps its FLOOR and gives up its sign: 0 hops is a
	// traversal that never leaves the seed while the surface renders it as a
	// configured depth, which is this check's own statement and not something
	// a generic walk can know. The negative belongs to V35, so the condition
	// is narrowed to ">= 0 && < 1" — one issue per value, never two.
	if c.Graph.HopDepth >= 0 && c.Graph.HopDepth < 1 {
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

	// V21 — dream.decide_mode enum, same shape and doctrine as V20.
	issues = append(issues, validateDreamDecideMode(c)...)

	// V36 — dream.decide_tie_odds range (own function for the V20 reason).
	issues = append(issues, validateDreamDecideTieOdds(c)...)

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

	// V18 — dream.num_predict floor. The SIGN half was folded into V35
	// (validateIntSigns) when that walk was widened from durations to every
	// int key: it made exactly the statement V35 makes generically (a negative
	// renders as a configured cap in the settings surface while DreamOptionsFor
	// serves the package default), and keeping both would double-report the
	// field. 0 stays legal here and there: it IS the documented "package
	// default" sentinel, and V35 does not check zeros.
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
	if n := c.Dream.NumPredict; n > 0 && n < dream.DefaultNumPredict {
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
func validateDreamDecideMode(c *Config) []Issue {
	c.Dream.DecideMode = strings.ToLower(strings.TrimSpace(c.Dream.DecideMode))
	switch c.Dream.DecideMode {
	case "", dream.DecideModeOff, dream.DecideModeEval, dream.DecideModeAll:
		return nil
	default:
		return []Issue{{Field: "dream.decide_mode", Severity: SeverityError,
			Msg: fmt.Sprintf("decide mode %q must be %q, %q or %q — empty reads as %q",
				c.Dream.DecideMode, dream.DecideModeOff, dream.DecideModeEval, dream.DecideModeAll, dream.DecideModeOff)}}
	}
}

// validateDispatchExternalOrder is V42 (wave A03-W3, DECISIONS A-E2 no. 1):
// dispatch.external_order in the V21 shape — trim + lower IN PLACE, then the
// name must be one of the dispatcher's own named orders (dispatch.RankOrderFor:
// config validates against the consumer's vocabulary, never a second copy of
// it). Empty reads as o1, like the zero value of the rank table. Fatal, not
// warn-and-reset: an operator reaching for this key wants the order they
// typed, and a typo silently keeping o1 would let the settings surface show
// their value while the dispatcher serves the other — boot aborts, the
// settings PUT is a 422.
func validateDispatchExternalOrder(c *Config) []Issue {
	c.Dispatch.ExternalOrder = strings.ToLower(strings.TrimSpace(c.Dispatch.ExternalOrder))
	if c.Dispatch.ExternalOrder == "" {
		return nil
	}
	if _, ok := dispatch.RankOrderFor(c.Dispatch.ExternalOrder); ok {
		return nil
	}
	return []Issue{{Field: "dispatch.external_order", Severity: SeverityError,
		Msg: fmt.Sprintf("external order %q must be %q (interactive, external, background) or %q (interactive, background, external) — empty reads as %q",
			c.Dispatch.ExternalOrder, dispatch.ExternalOrderO1, dispatch.ExternalOrderO2, dispatch.ExternalOrderO1)}}
}

// Dispatch bounds derived from the gateway caps (design/02 §4.11.2) — named
// so V43 says where its number comes from.
const (
	// gatewayCommitDistance is the distance the stream header commit keeps
	// above the external wait budget (design/02 §4.7: the commit grace is
	// clamped to at least dispatch.external_max_wait + 5 s, so no byte goes
	// out before the acquire returns — a refusal must still be able to be a
	// 429).
	gatewayCommitDistance = 5 * time.Second
	// dispatchExternalMaxWaitCeiling is the V43 ceiling: the commit grace
	// ceiling (gatewayCommitGraceCeiling, 50 s, V39) minus that distance —
	// a larger wait budget would push the commit clamp above the grace a
	// typical 60 s proxy idle timeout allows.
	dispatchExternalMaxWaitCeiling = gatewayCommitGraceCeiling - gatewayCommitDistance
)

// validateDispatchExternalMaxWait is V43 (wave A03-W17, masterplan K21,
// design/02 §4.11.2): dispatch.external_max_wait — the one wait budget of an
// external acquire (A03-W5) — at most dispatchExternalMaxWaitCeiling (45 s),
// so the stream commit clamp of W02-08 never has to rise above the V39 grace
// ceiling. The key came with A03-W5 and its ceiling only now (K21 assigns it
// to this wave — the exception to the "checks come WITH the keys" precedent
// of validateDistill): no installation sets it (live 2026-11-04: no
// dispatch.* settings row, the env var unset). Negative values stay with the
// V17 sign walk — no second, contradicting issue here; 0 is legitimate
// ("never wait"). Fatal: boot abort, settings write 422.
func validateDispatchExternalMaxWait(c *Config) []Issue {
	w := c.Dispatch.ExternalMaxWait
	if w <= dispatchExternalMaxWaitCeiling {
		return nil
	}
	return []Issue{{Field: "dispatch.external_max_wait", Severity: SeverityError,
		Msg: fmt.Sprintf("external max wait %v must be <= %v: the stream commit grace ceiling %v minus the %v the header commit keeps above the wait budget",
			w, dispatchExternalMaxWaitCeiling, gatewayCommitGraceCeiling, gatewayCommitDistance)}}
}

// validateDispatchRetryAfterSteps is V46 (wave A03-W6, DECISIONS A-E2 no. 2,
// design/03 §4.9): dispatch.external_retry_after_steps in the dispatcher's own
// grammar (dispatch.ParseRetryAfterSteps — config validates against the
// consumer's parser, never a second copy of it: at least one entry, whole
// seconds >= 1, strictly ascending), and the largest step at most
// dispatch.external_lease_max when that is set (> 0): no external lease
// outlives admission + external_lease_max (+ the reap grace), so a larger
// Retry-After would tell a client to stay away past the moment every lease
// held now has ended. external_lease_max = 0 (no external cap) sets no bound
// here. Fatal: boot abort, settings write 422 — the settings surface must
// never show steps the dispatcher replaces by its default.
func validateDispatchRetryAfterSteps(c *Config) []Issue {
	steps, err := dispatch.ParseRetryAfterSteps(c.Dispatch.ExternalRetryAfterSteps)
	if err != nil {
		return []Issue{{Field: "dispatch.external_retry_after_steps", Severity: SeverityError,
			Msg: fmt.Sprintf("retry-after steps %q: %v", c.Dispatch.ExternalRetryAfterSteps, err)}}
	}
	if largest := steps[len(steps)-1]; c.Dispatch.ExternalLeaseMax > 0 && largest > c.Dispatch.ExternalLeaseMax {
		return []Issue{{Field: "dispatch.external_retry_after_steps", Severity: SeverityError,
			Msg: fmt.Sprintf("largest retry-after step %v must be <= dispatch.external_lease_max %v", largest, c.Dispatch.ExternalLeaseMax)}}
	}
	return nil
}

// validateGatewayBodyLogging is V44 (wave W06-9, design/06 §4.6, masterplan
// K10): gateway.body_logging in the V42 shape — trim + lower IN PLACE, then
// the name must be one of llmlog's policies (llmlog.BodyLoggingValid: config
// validates against the consumer's vocabulary, never a second copy of it).
// Empty reads as none and is written back as none, so the settings surface
// and the boot record show the policy that is served. Fatal, not
// warn-and-reset: a typo must not silently keep a policy the operator did
// not choose — in either direction (bodies stored that were meant to stay
// out, or missing that were meant to be there). Boot aborts, the settings
// PUT is a 422.
func validateGatewayBodyLogging(c *Config) []Issue {
	c.Gateway.BodyLogging = strings.ToLower(strings.TrimSpace(c.Gateway.BodyLogging))
	if c.Gateway.BodyLogging == "" {
		c.Gateway.BodyLogging = llmlog.BodyLoggingNone
		return nil
	}
	if llmlog.BodyLoggingValid(c.Gateway.BodyLogging) {
		return nil
	}
	return []Issue{{Field: "gateway.body_logging", Severity: SeverityError,
		Msg: fmt.Sprintf("body logging %q must be %q (no bodies), %q (bodies on error rows only) or %q (bodies on every row, 64 KiB each) — empty reads as %q",
			c.Gateway.BodyLogging, llmlog.BodyLoggingNone, llmlog.BodyLoggingErrors, llmlog.BodyLoggingFull, llmlog.BodyLoggingNone)}}
}

// validateGatewayMeter is V47 (wave W06-10, design/06 §4.2, §4.11): the
// meter's six limits and the completion reserve must be at least 1 — a
// limit of 0 would refuse every request of its op, a reserve of 0 would
// book a chat request without a client maximum at its prompt alone.
// Negatives stay with V35 (one issue per field). gateway.xu_reserve_ratio
// must lie in [0, 1): at 1 the urgent lane would keep the whole bucket and
// the bulk lane could never be served; NaN fails every comparison and is
// refused explicitly (float keys have no generic sign walk). Fatal like
// V38: boot aborts, the settings PUT is a 422.
func validateGatewayMeter(c *Config) []Issue {
	g := &c.Gateway
	var issues []Issue
	for _, f := range []struct {
		key string
		v   int
	}{
		{"gateway.chat_rpm_default", g.ChatRPMDefault},
		{"gateway.chat_tpm_default", g.ChatTPMDefault},
		{"gateway.embed_rpm_default", g.EmbedRPMDefault},
		{"gateway.embed_tpm_default", g.EmbedTPMDefault},
		{"gateway.rerank_rpm_default", g.RerankRPMDefault},
		{"gateway.rerank_tpm_default", g.RerankTPMDefault},
		{"gateway.tpm_reserve_default", g.TPMReserveDefault},
	} {
		if f.v == 0 {
			issues = append(issues, Issue{Field: f.key, Severity: SeverityError,
				Msg: f.key + " must be >= 1 — 0 would refuse every request of its op (or reserve no completion at all)"})
		}
	}
	if r := g.XUReserveRatio; !(r >= 0 && r < 1) {
		issues = append(issues, Issue{Field: "gateway.xu_reserve_ratio", Severity: SeverityError,
			Msg: fmt.Sprintf("gateway.xu_reserve_ratio (%v) must be in [0, 1) — at 1 the bulk lane could never be served", r)})
	}
	return issues
}

// validateGatewayBudgets is V50 (wave W06-13, design/06 §4.11): the daily
// token defaults of the three ops are at least 1 — 0 would refuse every
// request of its op for the rest of the UTC day, and the day budget is the
// one every op always has (the month and request budgets may be 0 =
// unbounded; their negatives, like those of the daily ones, stay with V35,
// one issue per field); the two envelope factors are at least 1 — an
// envelope below the consumer's own budget would bind a lone consumer under
// its own limit — and finite. Fatal like V47: boot aborts, a settings PUT is
// a 422.
func validateGatewayBudgets(c *Config) []Issue {
	g := &c.Gateway
	var issues []Issue
	for _, f := range []struct {
		key string
		v   int
	}{
		{"gateway.chat_daily_tokens_default", g.ChatDailyTokensDefault},
		{"gateway.embed_daily_tokens_default", g.EmbedDailyTokensDefault},
		{"gateway.rerank_daily_tokens_default", g.RerankDailyTokensDefault},
	} {
		if f.v == 0 {
			issues = append(issues, Issue{Field: f.key, Severity: SeverityError,
				Msg: f.key + " must be >= 1 — 0 would refuse every request of its op until 00:00 UTC"})
		}
	}
	for _, f := range []struct {
		key string
		v   float64
	}{
		{"gateway.tenant_envelope_factor", g.TenantEnvelopeFactor},
		{"gateway.instance_envelope_factor", g.InstanceEnvelopeFactor},
	} {
		if !(f.v >= 1) || math.IsInf(f.v, 1) {
			issues = append(issues, Issue{Field: f.key, Severity: SeverityError,
				Msg: fmt.Sprintf("%s (%v) must be a finite number >= 1 — an envelope below the consumer's own budget would bind a lone consumer under its own limit", f.key, f.v)})
		}
	}
	return issues
}

// validateGatewayCostBudget is V51 (wave W06-14, design/06 §4.11, B-06-20):
// gateway.consumer_monthly_cost_usd_default is a finite amount > 0. There is
// no 0 sentinel: 0 would read as "no budget" in one place and "nothing left"
// in another — the permission to reach billable rows is its own key
// (gateway.consumer_allow_external_default), the budget only bounds it.
// Fatal: boot aborts, a settings PUT is a 422.
func validateGatewayCostBudget(c *Config) []Issue {
	v := c.Gateway.ConsumerMonthlyCostUSDDefault
	if v > 0 && !math.IsInf(v, 1) {
		return nil
	}
	return []Issue{{Field: "gateway.consumer_monthly_cost_usd_default", Severity: SeverityError,
		Msg: fmt.Sprintf("gateway.consumer_monthly_cost_usd_default (%v) must be a finite amount > 0 — there is no 0 sentinel; to keep consumers off billable rows leave gateway.consumer_allow_external_default false", v)}}
}

// validateGatewayConcurrency is V48 (wave W06-11, design/06 §4.2, §4.11):
// gateway.consumer_concurrency_default is C, the meter's L1 per (consumer,
// op). At least 1 — 0 would refuse every request, and L1 is never unbounded
// (design/04 R-06-4); negatives stay with V35 (one issue per field). While
// dispatch.external_queue_per_consumer caps the queue (> 0; 0 = off) C is at
// most that cap: L1 counts a consumer's requests waiting in the queue and
// running, and the per-consumer cap of the external class is its L2
// counterpart — a larger C would promise a consumer more concurrent requests
// than the queue lets it wait with on one target (deviation from design/06
// §4.11, which names dispatch.interactive_queue_per_principal: since A03-W3
// the external class has a cap of its own). Fatal like V47: boot aborts, and
// a settings PUT of gateway.consumer_concurrency_default that breaks the rule
// is a 422. The issue hangs on that one field, so a PUT that lowers
// dispatch.external_queue_per_consumer under a stored C is a 200 with a
// warning: the stored C is dropped and C falls back to its environment value
// or the default (the mechanism V39 has for gateway.inflight_budget) — the
// effective C never exceeds the cap. Only when that fallback breaks the rule
// too is the PUT of the cap a 422.
func validateGatewayConcurrency(c *Config) []Issue {
	const key = "gateway.consumer_concurrency_default"
	conc, queue := c.Gateway.ConsumerConcurrencyDefault, c.Dispatch.ExternalQueuePerConsumer
	switch {
	case conc == 0:
		return []Issue{{Field: key, Severity: SeverityError,
			Msg: key + " must be >= 1 — 0 would refuse every request, and the concurrency limit is never unbounded"}}
	case queue > 0 && conc > queue:
		return []Issue{{Field: key, Severity: SeverityError,
			Msg: fmt.Sprintf("%s (%d) must not exceed dispatch.external_queue_per_consumer (%d) — the queue would refuse a consumer what its concurrency limit admits", key, conc, queue)}}
	}
	return nil
}

// gatewayInflightSweepFloor is V49's floor of gateway.inflight_sweep_interval.
const gatewayInflightSweepFloor = time.Second

// validateGatewayInflightSweep is V49 (wave W01-8, design/01 §4.2):
// gateway.inflight_sweep_interval is the period of the in-flight registry's
// periodic sweep — one ctx_model_auth_live query over the key IDs of every
// running /v1 request, the second line behind the ctx_model_access_write
// notifications. At least 1 s: 0 has no period (time.NewTicker panics on
// it), and a sub-second sweep would put one query after another on the
// database pool. The seconds parser yields whole seconds only, so from a
// setting or the environment this refuses exactly 0; the floor still holds
// for a hand-built snapshot. Negatives stay with V17 (one issue per field).
// Fatal like V48: boot aborts, the settings PUT is a 422.
func validateGatewayInflightSweep(c *Config) []Issue {
	const key = "gateway.inflight_sweep_interval"
	if d := c.Gateway.InflightSweepInterval; d >= 0 && d < gatewayInflightSweepFloor {
		return []Issue{{Field: key, Severity: SeverityError,
			Msg: fmt.Sprintf("%s (%v) must be >= %v (V49) — 0 has no period, and a shorter sweep would put one ctx_model_auth_live query after another on the database pool", key, d, gatewayInflightSweepFloor)}}
	}
	return nil
}

// validateFederation is V45 (wave F-W8, design/04 §3.1): federation.max_depth
// must lie in 1..fedhop.MaxHopEntries. Negative values stay with V35 (one
// issue per field). 0 would refuse every request — a chain of length 0 is
// already "≥ 0" — and a value above MaxHopEntries can never be reached:
// ParseInbound refuses a longer Ctx-Hop with 400 first, so the setting would
// show a limit nobody enforces. Fatal like V42: the key is settings-only, so
// a settings PUT answers 422, and a psql-written row (which replaces the one
// row of the key) is dropped by the reload with a WARN — Build's override
// tolerance falls back to the registry default 3, never to an invalid limit.
func validateFederation(c *Config) []Issue {
	d := c.Federation.MaxDepth
	if d < 0 || (d >= 1 && d <= fedhop.MaxHopEntries) {
		return nil
	}
	return []Issue{{Field: "federation.max_depth", Severity: SeverityError,
		Msg: fmt.Sprintf("federation.max_depth (%d) must be between 1 and %d (the Ctx-Hop parse limit) — 0 refuses every request, a larger value is never reached",
			d, fedhop.MaxHopEntries)}}
}

// validateDreamDecideTieOdds is V36: dream.decide_tie_odds is an odds RATIO
// between the anchor link and a near-tie, so its readings are 0 (rule off)
// and anything from 1 up (1 = anchor and exact ties). A value in (0, 1) would
// put the limit ABOVE the anchor and silently write nothing at all; a
// negative one, NaN or ±Inf has no reading. Fatal like V19/V21: boot aborts,
// the settings PUT is a 422.
func validateDreamDecideTieOdds(c *Config) []Issue {
	f := c.Dream.DecideTieOdds
	if f == 0 || (f >= 1 && !math.IsInf(f, 1)) {
		return nil
	}
	return []Issue{{Field: "dream.decide_tie_odds", Severity: SeverityError,
		Msg: fmt.Sprintf("decide tie odds %v must be 0 (off) or a finite odds ratio >= 1", f)}}
}

// validateDreamJSONMode is V20 — see below; validateDreamDecideMode above is
// V21, its twin for dream.decide_mode.
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

// Gateway bounds that are external facts, not policy (design/02 §3.3) — named
// so V39 says where each comes from.
const (
	// gatewayFrameCeiling is the internal SSE frame cap of the stream reader
	// (llm/stream.go maxSSEFrameBytes, unexported there): a relayed frame
	// larger than what the reader accepts can never arrive.
	gatewayFrameCeiling = 4 << 20
	// gatewaySDKTimeout is the OpenAI SDKs' default request timeout
	// (design/02 §3.3, extern-recherche §1.7 DEFAULT_TIMEOUT): a request
	// deadline at or above it lets the SDK give up and resend the whole
	// request while the first one still runs.
	gatewaySDKTimeout = 600 * time.Second
	// gatewayCommitGraceCeiling keeps the stream header commit under a typical
	// 60 s proxy idle timeout (design/02 §3.3, handler/sse.go).
	gatewayCommitGraceCeiling = 50 * time.Second
	// gatewayResponseCopies is the chat route's response share per capped
	// unit (design/02 §4.2.1): raw upstream body plus rewritten copy
	// (non-stream), one frame raw plus rewritten (stream). The embeddings
	// and rerank routes' answers are their own floors' (oai.EmbedRequestFloor,
	// oai.RerankRequestFloor).
	gatewayResponseCopies = 2
)

// validateGateway is V38 and V39 (wave W02-02, design/02 §3.3). The keys come
// with their checks although their consumers arrive in later waves — the
// precedent of validateDistill: a check added after a key is configurable
// would be added against values already in use. Negative values stay with
// the generic walks (V17 durations, V35 ints); these checks own the values
// that parse, are not negative, and still cannot mean what they say:
//
// V38 — floors. 0 for any byte size, token count or duration of the group is
// refused: a 0 budget or cap refuses every request, a 0 timeout or window
// ends every request at once, a 0 keepalive spins. The one legal 0 is
// gateway.tenant_alias_ceiling.tools_max_count (tenant aliases without
// tools).
//
// V39 — caps and the one relation:
//   - gateway.max_response_bytes <= httpx.MaxResponseBytes: the transport cap
//     it narrows; above it the setting would claim a read the transport never
//     delivers.
//   - gateway.max_stream_frame_bytes <= 4 MiB: the stream reader's frame cap.
//   - gateway.stream_relay_buffer_bytes >= gateway.max_stream_frame_bytes +
//     oai.RelayRecordHeader (A03-W10): the relay buffer holds one frame at
//     the cap, or such a frame could never be relayed (every stream that
//     carries one would be cut as an overflow).
//   - gateway.request_deadline < 600 s: the SDKs' default timeout.
//   - gateway.stream_commit_grace <= 50 s: under a typical proxy idle.
//   - gateway.inflight_budget >= one request at every cap (RC-D C8: over
//     every route and class): the largest of six needs (gatewayFloorParts)
//     plus oai.UpstreamFrameReserve. Each route at credentials with a body
//     up to gateway.max_request_bytes, and below credentials with a body
//     up to the effective scan cap, min(gateway.scan_max_request_bytes,
//     gateway.max_request_bytes) — a longer body is 413 before the scan
//     books anything, and what it booked until then is a part of the
//     credentials need. Chat (gatewayChatNeed): 3 × the body (body,
//     RawMessage copy, upstream body) + the read term (the body + min(the
//     body + 512 B, 1 MiB): what reading a body without a declared length
//     spends beyond a declared one, oai.ChunkedReadReserve, FOLGE-P
//     Nachtrag C-1) + the JSON check's nesting state at the decoder's
//     depth limit (240 024 B, hardening 5 B-2), below credentials + the
//     sensitivity scan's booking (23 × the body + 128 KiB + 1 040 104 B of
//     nesting state, FOLGE-P H-8 and hardening 7), plus the chat response
//     share — 2 × gateway.max_response_bytes or 2 ×
//     gateway.max_stream_frame_bytes + gateway.stream_relay_buffer_bytes (a
//     stream's relay buffer, A03-W10). Embeddings (oai.EmbedRequestFloor)
//     and rerank (oai.RerankRequestFloor): their ledger terms at the worst
//     form — every input (256) or document (1000) its own block (the row's
//     capacity 1), every string byte the form the route's decode and wire
//     make the most of, the embeddings answer and decode at
//     gateway.embedding_max_floats — derived from the routes' constants and
//     gated against the routes by oai TestBudgetFloorHoldsTheWorstForms
//     (held within [0.7, 1] × the need + the reserve at 64 KiB … 8 MiB).
//     oai.UpstreamFrameReserve (64 KiB) is the backend row's frame in the
//     upstream bodies (its model name and parameters, which the
//     configuration does not know and the schema does not bound): the
//     deepest rewritten chat request without a declared length at a sum of
//     the read's chunks got 503 at the chat need from a 212-byte name on
//     (oai TestDeepestDeveloperRequestLacksOnlyTheUpstreamFrame pins 200 at
//     the need plus the reserve). The embeddings and rerank routes write the
//     name into every block, so at the block ceiling the reserve holds a
//     name of up to 256 B (embeddings) and of 9 B at the rerank wire's
//     build factor of 29/4 (65 B at a factor of 1). At the defaults the
//     rerank route decides: 532.42 MiB (558 285 758 B) at the build factor
//     29/4, past the default budget of 512 MiB — the default holds it once
//     the rerank wire's byte writer (FOLGE-R) brings the factor to 1
//     (193.76 MiB then); embeddings 146.31 / 141.46 MiB, chat 41.23 /
//     64.35 MiB (credentials at 8 MiB / below credentials at the scan cap of
//     2 MiB). What the floor promises is one attempt without failover and
//     without a rewritten message array (Delta-Review 3 R3-4, Delta-Review 5
//     L-2): every further attempt of a chain books its upstream bodies once
//     more, and a rerank query is counted once although every block's body
//     carries it (a long query over many blocks books more). A smaller
//     budget could never serve such a request, and the client would read
//     "overloaded, retry in 1 s" forever. The error names the deciding
//     route and class, all six needs and the ways out (gatewayCapWayOut).
//     Stricter since FOLGE-P (budgets under 226.35 MiB at the 8 MiB cap)
//     and since RC-D C8 (the embeddings and rerank needs; budgets the chat
//     need alone held may be refused — see docs/operations.md, the upgrade
//     note).
//   - gateway.retry_after_jitter in [0, 1] (float keys have no generic sign
//     walk; NaN fails every comparison and is refused explicitly).
//
// V58 (RC-D C8) — gateway.scan_max_request_bytes >= 1: 0 would answer every
// request with a body to an alias below credentials with 413. A value above
// gateway.max_request_bytes is legal and without effect — the body cap
// refuses first, and the effective scan cap is the smaller of the two
// (oai.EffectiveScanCap), which is also what V39's floor books for the
// scanned class. Deliberately no coupling "<= gateway.max_request_bytes":
// both keys are hot and written one at a time, and a coupling would refuse
// lowering the body cap alone below the scan cap (the operator would have
// to know the order of two writes); the min keeps every such write valid
// and its meaning plain. Negative values stay with V35.
func validateGateway(c *Config) []Issue {
	var issues []Issue
	g := &c.Gateway
	bad := func(field, msg string) {
		issues = append(issues, Issue{Field: field, Severity: SeverityError, Msg: msg})
	}
	for _, f := range []struct {
		key string
		v   int
	}{
		{"gateway.max_request_bytes", g.MaxRequestBytes},
		{"gateway.inflight_budget", g.InflightBudget},
		{"gateway.max_response_bytes", g.MaxResponseBytes},
		{"gateway.max_stream_frame_bytes", g.MaxStreamFrameBytes},
		{"gateway.embedding_max_floats", g.EmbeddingMaxFloats},
		{"gateway.stream_relay_buffer_bytes", g.StreamRelayBufferBytes},
		{"gateway.stream_min_drain_rate", g.StreamMinDrainRate},
		{"gateway.default_max_tokens", g.DefaultMaxTokens},
		{"gateway.max_output_tokens_ceiling", g.MaxOutputTokensCeiling},
		{"gateway.tenant_alias_ceiling.max_output_tokens", g.TenantAliasCeiling.MaxOutputTokens},
		{"gateway.tenant_alias_ceiling.max_n", g.TenantAliasCeiling.MaxN},
		{"gateway.tenant_alias_ceiling.embedding_max_inputs", g.TenantAliasCeiling.EmbeddingMaxInputs},
		{"gateway.tenant_alias_ceiling.rerank_max_documents", g.TenantAliasCeiling.RerankMaxDocuments},
	} {
		if f.v == 0 {
			bad(f.key, f.key+" must be >= 1 — 0 would refuse or truncate every gateway request")
		}
	}
	for _, f := range []struct {
		key string
		v   time.Duration
	}{
		{"gateway.stream_commit_grace", g.StreamCommitGrace},
		{"gateway.keepalive_interval", g.KeepaliveInterval},
		{"gateway.client_write_window", g.ClientWriteWindow},
		{"gateway.stream_min_drain_window", g.StreamMinDrainWindow},
		{"gateway.request_timeout", g.RequestTimeout},
		{"gateway.request_deadline", g.RequestDeadline},
		{"gateway.tenant_alias_ceiling.timeout", g.TenantAliasCeiling.Timeout},
	} {
		if f.v == 0 {
			bad(f.key, f.key+" must be >= 1 second — 0 would end every gateway request (or spin the keepalive) at once")
		}
	}

	if int64(g.MaxResponseBytes) > httpx.MaxResponseBytes {
		bad("gateway.max_response_bytes", fmt.Sprintf("gateway.max_response_bytes (%d) must not exceed the transport cap httpx.MaxResponseBytes (%d)", g.MaxResponseBytes, httpx.MaxResponseBytes))
	}
	if g.MaxStreamFrameBytes > gatewayFrameCeiling {
		bad("gateway.max_stream_frame_bytes", fmt.Sprintf("gateway.max_stream_frame_bytes (%d) must not exceed the stream reader's frame cap (%d)", g.MaxStreamFrameBytes, gatewayFrameCeiling))
	}
	if g.StreamRelayBufferBytes > 0 && g.MaxStreamFrameBytes > 0 && int64(g.StreamRelayBufferBytes) < int64(g.MaxStreamFrameBytes)+oai.RelayRecordHeader {
		bad("gateway.stream_relay_buffer_bytes", fmt.Sprintf("gateway.stream_relay_buffer_bytes (%d) must hold one frame at the frame cap: >= gateway.max_stream_frame_bytes (%d) + %d (the relay's record header) — a frame at the cap could never be relayed",
			g.StreamRelayBufferBytes, g.MaxStreamFrameBytes, oai.RelayRecordHeader))
	}
	if g.RequestDeadline >= gatewaySDKTimeout {
		bad("gateway.request_deadline", fmt.Sprintf("gateway.request_deadline (%v) must stay below the OpenAI SDKs' default timeout (%v) — the SDK would resend a request still running", g.RequestDeadline, gatewaySDKTimeout))
	}
	if g.StreamCommitGrace > gatewayCommitGraceCeiling {
		bad("gateway.stream_commit_grace", fmt.Sprintf("gateway.stream_commit_grace (%v) must be <= %v — a later commit runs into a typical 60 s proxy idle timeout", g.StreamCommitGrace, gatewayCommitGraceCeiling))
	}
	if need, ok := gatewayBudgetFloor(g); ok && g.InflightBudget > 0 {
		switch {
		case need < 0:
			bad("gateway.inflight_budget", fmt.Sprintf("gateway.inflight_budget (%d) cannot hold one request at every cap — the routes' needs at gateway.max_request_bytes (%d) exceed any representable budget — lower gateway.max_request_bytes or the response caps", g.InflightBudget, g.MaxRequestBytes))
		case int64(g.InflightBudget) < need:
			bad("gateway.inflight_budget", gatewayFloorError(g, need))
		}
	}
	if g.ScanMaxRequestBytes == 0 {
		bad("gateway.scan_max_request_bytes", "gateway.scan_max_request_bytes must be >= 1 — 0 would refuse every request with a body to an alias below credentials with 413 (V58)")
	}
	if j := g.RetryAfterJitter; !(j >= 0 && j <= 1) {
		bad("gateway.retry_after_jitter", fmt.Sprintf("gateway.retry_after_jitter (%v) must be in [0, 1]", j))
	}
	return append(issues, validateGatewayAuth(g)...)
}

// gatewayDBPoolConns mirrors the pool size of store/pool.go (tunePool
// MaxConns): config may not import the store, so V40 checks against this
// mirror, and TestGatewayDBPoolMirror reads store/pool.go to keep it equal.
const gatewayDBPoolConns = 20

// validateGatewayAuth is V40 (wave W01-6, design/01 §4.2, §4.9): the
// model-key door's four limits. Negative values stay with V17/V35.
//
//   - gateway.auth_failure_limit >= 1: 0 would lock a pair before its first
//     failure has been counted against anything.
//   - gateway.auth_failure_window >= 1 s: 0 would make the lock a no-op.
//   - gateway.auth_probe_rate >= 1: 0 would never let an unknown hash reach
//     the database — after a restart no key could authenticate at all.
//   - 1 <= gateway.auth_db_concurrency < the pool size (20): 0 refuses every
//     check with 503; at the pool size a forgery flood could hold every
//     connection, which is what the semaphore exists to prevent (B18).
func validateGatewayAuth(g *GatewayConfig) []Issue {
	var issues []Issue
	bad := func(field, msg string) {
		issues = append(issues, Issue{Field: field, Severity: SeverityError, Msg: msg})
	}
	if g.AuthFailureLimit == 0 {
		bad("gateway.auth_failure_limit", "gateway.auth_failure_limit must be >= 1 — 0 would lock a key before any failure")
	}
	if g.AuthFailureWindow == 0 {
		bad("gateway.auth_failure_window", "gateway.auth_failure_window must be >= 1 second — 0 would make the failure lock a no-op")
	}
	if g.AuthProbeRate == 0 {
		bad("gateway.auth_probe_rate", "gateway.auth_probe_rate must be >= 1 — 0 would never let a key without a past success reach the database")
	}
	switch n := g.AuthDBConcurrency; {
	case n == 0:
		bad("gateway.auth_db_concurrency", "gateway.auth_db_concurrency must be >= 1 — 0 would refuse every model-key check with 503")
	case n >= gatewayDBPoolConns:
		bad("gateway.auth_db_concurrency", fmt.Sprintf("gateway.auth_db_concurrency (%d) must stay below the database pool size (%d) — a forgery flood could otherwise hold every connection", n, gatewayDBPoolConns))
	}
	return issues
}

// gatewayMaxRotationGrace is store.MaxRotationGrace (the store refuses a
// longer grace; config may not import store). The equality is pinned in
// internal/handler (TestRotationGraceCeilingMatchesStore).
const gatewayMaxRotationGrace = 7 * 24 * time.Hour

// validateGatewayConsumers is V41 (wave W01-5, design/01 §4.9). Negative
// values stay with the generic walks (V17, V35); these checks own what parses
// and is not negative, yet cannot mean what it says:
//   - gateway.key_rotation_grace <= 7 d: the store refuses a longer grace, so
//     every rotation without an explicit grace would answer 400.
//   - the three page caps >= 1: 0 would serve empty pages forever while the
//     settings surface renders a configured cap.
//
// The lifetimes and the expiry warning may be 0 (no ceiling, no warning).
func validateGatewayConsumers(c *Config) []Issue {
	var issues []Issue
	g := &c.Gateway.Consumers
	if g.KeyRotationGrace > gatewayMaxRotationGrace {
		issues = append(issues, Issue{Field: "gateway.key_rotation_grace", Severity: SeverityError,
			Msg: fmt.Sprintf("gateway.key_rotation_grace (%v) must be <= %v — the store refuses a longer grace, every rotation without its own grace would fail", g.KeyRotationGrace, gatewayMaxRotationGrace)})
	}
	for _, f := range []struct {
		key string
		v   int
	}{
		{"gateway.consumer_list_max_limit", g.ListMaxLimit},
		{"gateway.consumer_chain_max", g.ChainMax},
		{"gateway.consumer_audit_trail_max", g.AuditTrailMax},
	} {
		if f.v == 0 {
			issues = append(issues, Issue{Field: f.key, Severity: SeverityError,
				Msg: f.key + " must be >= 1 — 0 would serve an empty page on every read"})
		}
	}
	return issues
}

// gatewayBudgetFloor is the in-flight reservation of one request at every
// gateway cap (V39, extended by RC-D C8): the largest need of one request
// over the routes and classes (gatewayFloorParts) plus
// oai.UpstreamFrameReserve. ok is false while a size is not positive —
// V35/V38 report those, and a floor over them would only add a second
// error. The arithmetic saturates (Fremd-Review 2, FR2-3): a floor beyond
// int64 is reported as -1 — "no budget can hold it" — instead of wrapping
// around into a negative value every budget clears.
func gatewayBudgetFloor(g *GatewayConfig) (int64, bool) {
	if g.MaxRequestBytes <= 0 || g.MaxResponseBytes <= 0 || g.MaxStreamFrameBytes <= 0 || g.EmbeddingMaxFloats <= 0 {
		return 0, false
	}
	floor := gatewayFloorAt(g, int64(g.MaxRequestBytes), int64(g.ScanMaxRequestBytes))
	if floor == math.MaxInt64 {
		return -1, true
	}
	return floor, true
}

// gatewayFloorPart is one route's and class's need in the floor: the most
// one request of that route books with a body of up to body bytes.
type gatewayFloorPart struct {
	route, class string
	body, need   int64
}

// gatewayFloorParts are the six needs of the floor at the body cap n and the
// scan setting scan: each route at credentials with a body up to n (no
// scan), and below credentials with a body up to the effective scan cap
// (oai.EffectiveScanCap: the scan cap's 413 refuses a longer one before the
// scan books anything; what such a request booked until then is a part of
// the credentials need at n). The chat parts carry the chat response share,
// the embeddings and rerank parts their routes' own answers.
func gatewayFloorParts(g *GatewayConfig, n, scan int64) [6]gatewayFloorPart {
	s := oai.EffectiveScanCap(scan, n)
	resp, floats := gatewayResponseShare(g), int64(g.EmbeddingMaxFloats)
	return [6]gatewayFloorPart{
		{"chat", "credentials", n, gatewayAddSat(gatewayChatNeed(n, false), resp)},
		{"chat", "below credentials", s, gatewayAddSat(gatewayChatNeed(s, true), resp)},
		{"embeddings", "credentials", n, oai.EmbedRequestFloor(n, false, floats)},
		{"embeddings", "below credentials", s, oai.EmbedRequestFloor(s, true, floats)},
		{"rerank", "credentials", n, oai.RerankRequestFloor(n, false)},
		{"rerank", "below credentials", s, oai.RerankRequestFloor(s, true)},
	}
}

// gatewayFloorAt is the floor at the body cap n and the scan setting scan
// (saturating): the largest part plus oai.UpstreamFrameReserve.
func gatewayFloorAt(g *GatewayConfig, n, scan int64) int64 {
	var most int64
	for _, p := range gatewayFloorParts(g, n, scan) {
		most = max(most, p.need)
	}
	return gatewayAddSat(most, oai.UpstreamFrameReserve)
}

// gatewayChatNeed is the request part of one chat request with a body of n
// bytes (saturating): oai.RequestReserveFactor × n (the body, its
// RawMessage copy, the upstream body), the read of such a body without a
// declared length beyond a declared one (oai.ChunkedReadReserve — n +
// min(n + 512, 1 MiB): the read spends the request share, a rewritten
// message array and the upstream body book past it; Nachtrag C-1 of
// FOLGE-P), the JSON check's nesting state at the decoder's depth limit
// (oai.JSONStateReserve: booked before the check on top of the share
// wherever the share's rest is spent — by a body under 120 012 B, or by a
// rewritten message array at any cap; hardening 5, B-2), and, scanned (an
// alias below credentials), the sensitivity scan's booking:
// oai.ScanReserveFactor × n + oai.ScanReserveBase + its nesting state at
// the same limit, oai.ScanDepthReserve (FOLGE-P H-8, hardening 7). It does
// not hold the upstream frame of a rewritten request (oai.UpstreamFrameReserve
// does, V39 below).
func gatewayChatNeed(n int64, scanned bool) int64 {
	need := gatewayAddSat(gatewayAddSat(gatewayMulSat(oai.RequestReserveFactor, n), oai.ChunkedReadReserve(n)), oai.JSONStateReserve)
	if scanned {
		need = gatewayAddSat(need, gatewayAddSat(gatewayMulSat(oai.ScanReserveFactor, n), oai.ScanReserveBase+oai.ScanDepthReserve))
	}
	return need
}

// gatewayResponseShare is the chat route's largest response share
// (saturating; the sizes are positive, gatewayBudgetFloor checks it).
func gatewayResponseShare(g *GatewayConfig) int64 {
	return max(
		gatewayMulSat(gatewayResponseCopies, int64(g.MaxResponseBytes)),
		// A stream: one frame raw plus rewritten, and its relay buffer
		// (A03-W10), which the route books in the request's ledger before it
		// allocates it — at the defaults 2 + 2 MiB, below the non-stream share
		// of 8 MiB.
		gatewayAddSat(gatewayMulSat(gatewayResponseCopies, int64(g.MaxStreamFrameBytes)), max(int64(g.StreamRelayBufferBytes), 0)),
	)
}

// gatewayFloorError is V39's message for a budget under the floor need: the
// deciding route and class, all six needs, and the ways out with their
// numbers (gatewayCapWayOut).
func gatewayFloorError(g *GatewayConfig, need int64) string {
	parts := gatewayFloorParts(g, int64(g.MaxRequestBytes), int64(g.ScanMaxRequestBytes))
	top := parts[0]
	for _, p := range parts[1:] {
		if p.need > top.need {
			top = p
		}
	}
	return fmt.Sprintf("gateway.inflight_budget (%d) must hold one request at every cap: %d = the largest need of one request — %s %s, %d at a body of %d — + %d (oai.UpstreamFrameReserve: the backend row's model name and parameters in its upstream bodies); one attempt without failover and without a rewritten message array — raise gateway.inflight_budget to >= %d%s. "+
		"The needs at credentials (a body up to gateway.max_request_bytes, %d) / below credentials (a body up to the scan cap, min(gateway.scan_max_request_bytes, gateway.max_request_bytes) = %d): chat %d / %d, embeddings %d / %d, rerank %d / %d — below credentials the sensitivity scan's booking (%d × the body + %d + %d, its nesting state at the decoder's depth limit) counts on top, which a request at credentials does not take, and a body past the scan cap is 413 before it",
		g.InflightBudget, need, top.route, top.class, top.need, top.body, oai.UpstreamFrameReserve, need, gatewayCapWayOut(g, top),
		parts[0].body, parts[1].body, parts[0].need, parts[1].need, parts[2].need, parts[3].need, parts[4].need, parts[5].need,
		oai.ScanReserveFactor, oai.ScanReserveBase, oai.ScanDepthReserve)
}

// gatewayCapWayOut is the floor error's further ways out (Delta-Review 3
// R3-3): " or lower gateway.max_request_bytes to <= X", X the largest body
// cap the budget holds at the other caps as they are — "" when no body cap
// fits (the routes' fixed parts alone fill the budget) — and, when a class
// below credentials decides the floor, " or lower
// gateway.scan_max_request_bytes to <= Y", Y the largest scan cap the
// budget holds at the body cap as it is (none when the credentials needs
// alone pass it). The needs are not linear in the caps (the read term bends
// at 1 048 064 B, the routes' copies at their buffers), so X and Y are
// found by bisection over the same gatewayFloorAt the floor uses; both are
// monotone in their cap.
func gatewayCapWayOut(g *GatewayConfig, top gatewayFloorPart) string {
	budget, scan, n := int64(g.InflightBudget), int64(g.ScanMaxRequestBytes), int64(g.MaxRequestBytes)
	out := ""
	if x := gatewayLargestFit(budget, func(c int64) int64 { return gatewayFloorAt(g, c, scan) }); x > 0 {
		out = fmt.Sprintf(" or lower gateway.max_request_bytes to <= %d", x)
	}
	if top.class != "credentials" {
		if y := gatewayLargestFit(budget, func(c int64) int64 { return gatewayFloorAt(g, n, c) }); y > 0 {
			out += fmt.Sprintf(" or lower gateway.scan_max_request_bytes to <= %d", y)
		}
	}
	return out
}

// gatewayLargestFit is the largest c in [1, budget] with floor(c) <= budget
// (floor monotone non-decreasing), 0 when none fits.
func gatewayLargestFit(budget int64, floor func(int64) int64) int64 {
	fits := func(c int64) bool {
		need := floor(c)
		return need < math.MaxInt64 && need <= budget
	}
	if budget < 1 || !fits(1) {
		return 0
	}
	lo, hi := int64(1), budget // fits(lo); no cap above the budget fits
	for lo < hi {
		if m := lo + (hi-lo+1)/2; fits(m) {
			lo = m
		} else {
			hi = m - 1
		}
	}
	return lo
}

// gatewayMulSat and gatewayAddSat are a·b and a+b for non-negative operands,
// saturating at math.MaxInt64.
func gatewayMulSat(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

func gatewayAddSat(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
