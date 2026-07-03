// Package blocktype implements the dynamic block-type registry (workflow-
// engine axis 01, design/01-type-registry.md). Behaviour (policy) is data —
// rows in context_block_types (migration 072) — while the pipelines
// (mechanism: RRF fusion, guard similarity, dream loop, digest) stay code.
// Go resolves policy to flat parameters (allowlists, factor arrays,
// thresholds); SQL functions and batch predicates consume only parameters —
// no SQL ever reads the registry table directly (§4 doctrine).
//
// Resolution follows the config.Store pattern: an immutable *Set snapshot
// behind an atomic pointer, hot-reloaded over the ctx_settings_write NOTIFY
// channel, with a compiled-in builtin set as the fail-safe floor. Consumers
// take ONE Set per operation (request, guard batch, dream cycle, digest run)
// and never see a torn generation. At the 1M+ blocks/tenant target scale the
// resolve path is a single atomic load — no per-request DB read, no lock.
package blocktype

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// Retrieval policy kinds (config vocabulary v1, design §3.3).
const (
	// RetrievalFullPass — block is fully visible in RRF/graph/overview.
	RetrievalFullPass = "full-pass"
	// RetrievalExcluded — block is invisible to retrieval (today's
	// system-meta hard literal, generalized).
	RetrievalExcluded = "excluded"
	// RetrievalDamped — visible, but all four RRF arms multiply the score by
	// damping_factor unless the query matches an intent pattern (lift).
	RetrievalDamped = "damped"
	// RetrievalAggregateToParent — hits fold onto the structural parent.
	// Vocabulary exists from T3; the fold mechanism is wave T11 — until it is
	// built the validator REJECTS this value (fail-closed, §5.2: no silent
	// no-op behaviour before the mechanism exists).
	RetrievalAggregateToParent = "aggregate-to-parent"
)

// Parent modes (config vocabulary v1). Like aggregate-to-parent, any mode
// other than "none" is rejected until the parent_id write path exists
// (Achse 02, §3.3 R1): a "required" type would be acceptable from T3 on and
// silently ineffective — exactly the §5.2 breakage class.
const (
	ParentModeNone     = "none"
	ParentModeOptional = "optional"
	ParentModeRequired = "required"
)

// Default guard thresholds (011 line; per-type overrides via
// guard.threshold_duplicate / guard.threshold_review).
const (
	DefaultThresholdDuplicate = 0.98
	DefaultThresholdReview    = 0.92
)

// Guard persist modes (config vocabulary v1, design/02 §4.1/§4.7 — the I-J
// field-extension request to Achse 01, §9.1 point 2a). "archive" is the
// bestand: a near_duplicate is auto-archived (guard.go archive branch).
// "flag" persists a possible_duplicate flag (guard_status + guard_matched_id)
// and NEVER sets is_archived — the issue axis needs this so a duplicate issue
// is surfaced, never silently removed (§4.7).
const (
	GuardModeArchive = "archive"
	GuardModeFlag    = "flag"
)

// Guard candidate scopes (config vocabulary v1, design/02 §4.1/§5.3). "all" is
// the bestand: the guard matches candidates across ALL scopes. "same-scope"
// restricts the candidate set to the checked block's own scope (Go passes
// p_same_scope_only=TRUE to ctx_guard_check) — the issue axis needs this so an
// issue never matches a cross-tenant block (guard-as-side-channel, §5.3).
const (
	GuardCandidatesAll       = "all"
	GuardCandidatesSameScope = "same-scope"
)

// DefaultClassifyPriority orders types without an explicit classify.priority
// last (smaller = evaluated earlier; builtin seeds: system-meta 10,
// audit-trail 20 — the M035 decision-tree order).
const DefaultClassifyPriority = 100

// globalScope is the shipped/global registry namespace ('_global' sentinel,
// F2/051 pattern). Deliberately a local constant — importing internal/store
// for store.GlobalScope would arm an import cycle once store consumes this
// package in T4.
const globalScope = "_global"

// maxConfigBytes caps one config JSONB (R1 resource-exhaustion guard, §3.3:
// the Set cache is server-shared memory across N tenants).
const maxConfigBytes = 16 * 1024

// Array caps (R1, §3.3): every string array in the vocabulary is bounded.
const (
	maxArrayEntries = 64
	maxArrayItemLen = 128
)

// nameFormat is the type-name gate (Go-side, v2.0.0 line — the schema
// deliberately carries no CHECK): trivial names, no quoting potential in
// logs/UI, bind-parameter-only in SQL (§5.5).
var nameFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)

// statusFormat gates workflow status tokens (§5.5 line, same rationale as
// nameFormat): trivial identifiers, no quoting potential in logs/UI/board, and
// they land in the VARCHAR(50) workflow_status column — bind-parameter-only in
// SQL. Forge-state keys (open/closed) share the format.
var statusFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)

// RetrievalPolicy — visibility class of a type in the retrieval pipelines.
type RetrievalPolicy struct {
	Kind string
	// DampingFactor is the score multiplier for Kind==damped, in (0,1].
	DampingFactor float64
	// IntentPatterns are case-insensitive query substrings that LIFT the
	// damping for that query (factor 1.0) — generalizes the retired
	// rrf.AuditTrailFactor (a pure engine, rrf.MatchesAny, remains).
	IntentPatterns []string
}

// GuardPolicy — participation of a type in the dedup guard.
type GuardPolicy struct {
	Check     bool // block is itself guard-checked (batch eligibility)
	Candidate bool // block is a match candidate for others
	// Mode is the persist mode for a near_duplicate finding:
	// GuardModeArchive (bestand, auto-archive) | GuardModeFlag (flag only).
	Mode string
	// Candidates is the candidate scope: GuardCandidatesAll (bestand,
	// cross-scope) | GuardCandidatesSameScope (own scope only).
	Candidates string
	// Per-type thresholds; nil = global defaults 0.98/0.92.
	ThresholdDuplicate *float64
	ThresholdReview    *float64
}

// DreamPolicy — participation in the dream loop. Linkable acts on BOTH sides
// (R1, §3.3): pick eligibility AND candidate/target admission.
type DreamPolicy struct {
	Linkable    bool
	LinkClasses []string // allowed semantic link classes; nil = all
}

// DigestPolicy — inclusion in the topic-map source.
type DigestPolicy struct{ Include bool }

// OverviewPolicy — inclusion in the overview (Louvain) clustering (R1 §6.8;
// the digest.include pendant against issue flooding).
//
// Doc note: the §4.1 Policy sketch omits an Overview field while §3.3 defines
// overview.include and §4.1 lists Set.OverviewTypes() — the field is required
// for the method to exist; kept here, deviation recorded in the wave return.
type OverviewPolicy struct{ Include bool }

// ParentPolicy — structural parent pointer semantics (Achse-02 territory;
// only the vocabulary lives here, see ParentMode* comment).
type ParentPolicy struct {
	Mode         string
	Relationship string // descriptive label (e.g. "comment-of"); UI/graph only
}

// WorkflowPolicy — per-type workflow state machine (config vocabulary v1,
// design/02 §4.1/§9.1 point 2b — the I-B field-extension request to Achse 01).
// This is the POLICY behind the context_blocks.workflow_status column (mig 077):
// the column carries the per-block VALUE, this config carries the SET of valid
// states, the board column order (States is ordered), the entry point (Initial)
// and the forge open/closed mapping (§4.5.4). Empty States = the type has no
// workflow (the whole knowledge corpus) — ValidateTransition rejects any
// transition for it (ErrNoWorkflow).
//
// v1 has NO explicit transition adjacency list (§4.1 form is exactly
// {states, initial, terminal, forge_state_map}): a transition is valid iff both
// endpoints are configured states — a complete graph over States. Terminal is
// NOT an outgoing constraint (reopen closed→open is real, §4.5.4); it flags
// which states map to the forge "closed" state and groups the board. An
// explicit adjacency matrix is a deferred config extension if ever needed.
type WorkflowPolicy struct {
	States        []string          // ordered board columns; empty = no workflow
	Initial       string            // entry state; ∈ States when States non-empty
	Terminal      []string          // subset of States; forge-closed / board grouping
	ForgeStateMap map[string]string // forge state (open/closed) → ctx status ∈ States
}

// ClassifyRules — write-side auto-classification rules of a type.
type ClassifyRules struct {
	Priority       int
	MetadataFlags  []string // metadata[flag]==true ⇒ this type
	SourcePrefixes []string // metadata.source prefix match ⇒ this type
	TitlePatterns  []string // case-insensitive title substring ⇒ this type
}

// Policy is the decoded, default-filled config of one registry row.
type Policy struct {
	Name      string
	Scope     string // '_global' or tenant scope
	Builtin   bool
	IsDefault bool
	Retrieval RetrievalPolicy
	Guard     GuardPolicy
	Dream     DreamPolicy
	Digest    DigestPolicy
	Overview  OverviewPolicy
	Parent    ParentPolicy
	Workflow  WorkflowPolicy
	Classify  ClassifyRules
}

// ── strict config decoding ───────────────────────────────────────────────────
//
// The envelope is decoded with DisallowUnknownFields at EVERY nesting level:
// a typo like {"guards": ...} or {"guard": {"cheack": ...}} must reject with
// the key path instead of silently falling back to default-true behaviour
// (§5.2 breakage class). Absent fields (vs. present-false) are told apart via
// pointer fields, then default-filled.

type cfgEnvelope struct {
	V         *int          `json:"v"`
	Retrieval *cfgRetrieval `json:"retrieval"`
	Guard     *cfgGuard     `json:"guard"`
	Dream     *cfgDream     `json:"dream"`
	Digest    *cfgDigest    `json:"digest"`
	Overview  *cfgOverview  `json:"overview"`
	Parent    *cfgParent    `json:"parent"`
	Workflow  *cfgWorkflow  `json:"workflow"`
	Classify  *cfgClassify  `json:"classify"`
}

type cfgRetrieval struct {
	Policy         *string  `json:"policy"`
	DampingFactor  *float64 `json:"damping_factor"`
	IntentPatterns []string `json:"intent_patterns"`
}

type cfgGuard struct {
	Check              *bool    `json:"check"`
	Candidate          *bool    `json:"candidate"`
	Mode               *string  `json:"mode"`
	Candidates         *string  `json:"candidates"`
	ThresholdDuplicate *float64 `json:"threshold_duplicate"`
	ThresholdReview    *float64 `json:"threshold_review"`
}

type cfgDream struct {
	Linkable    *bool    `json:"linkable"`
	LinkClasses []string `json:"link_classes"`
}

type cfgDigest struct {
	Include *bool `json:"include"`
}

type cfgOverview struct {
	Include *bool `json:"include"`
}

type cfgParent struct {
	Mode         *string `json:"mode"`
	Relationship *string `json:"relationship"`
}

type cfgWorkflow struct {
	States        []string          `json:"states"`
	Initial       *string           `json:"initial"`
	Terminal      []string          `json:"terminal"`
	ForgeStateMap map[string]string `json:"forge_state_map"`
}

type cfgClassify struct {
	Priority       *int     `json:"priority"`
	MetadataFlags  []string `json:"metadata_flags"`
	SourcePrefixes []string `json:"source_prefixes"`
	TitlePatterns  []string `json:"title_patterns"`
}

// DecodePolicy decodes and validates one registry row's config into a
// default-filled Policy. It is the single validation authority for BOTH the
// reload path (a failing row is a corrupt-config event, §4.3 class b) and
// the future T10 write path (422 with field path).
func DecodePolicy(name, scope string, builtin, isDefault bool, rawConfig []byte) (Policy, error) {
	var p Policy
	if !nameFormat.MatchString(name) {
		return p, fmt.Errorf("blocktype %q: name violates format ^[a-z0-9][a-z0-9-]{0,49}$", name)
	}
	if len(rawConfig) > maxConfigBytes {
		return p, fmt.Errorf("blocktype %q: config exceeds %d bytes (%d)", name, maxConfigBytes, len(rawConfig))
	}

	dec := json.NewDecoder(bytes.NewReader(rawConfig))
	dec.DisallowUnknownFields()
	var env cfgEnvelope
	if err := dec.Decode(&env); err != nil {
		return p, fmt.Errorf("blocktype %q: config: %w", name, err)
	}
	if env.V == nil || *env.V != 1 {
		return p, fmt.Errorf("blocktype %q: config: unsupported or missing envelope version (want v=1)", name)
	}

	p = Policy{
		Name:      name,
		Scope:     scope,
		Builtin:   builtin,
		IsDefault: isDefault,
		Retrieval: RetrievalPolicy{Kind: RetrievalFullPass},
		Guard:     GuardPolicy{Check: true, Candidate: true, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
		Dream:     DreamPolicy{Linkable: true},
		Digest:    DigestPolicy{Include: true},
		Overview:  OverviewPolicy{Include: true},
		Parent:    ParentPolicy{Mode: ParentModeNone},
		Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
	}

	if err := applyEnvelope(&p, &env); err != nil {
		return p, err
	}
	if err := validatePolicy(&p); err != nil {
		return p, err
	}
	return p, nil
}

// applyEnvelope overlays the present envelope fields onto the default-filled
// policy (absent pointer = keep default) and enforces the array caps.
func applyEnvelope(p *Policy, env *cfgEnvelope) error {
	if r := env.Retrieval; r != nil {
		if r.Policy != nil {
			p.Retrieval.Kind = *r.Policy
		}
		if r.DampingFactor != nil {
			p.Retrieval.DampingFactor = *r.DampingFactor
		}
		if err := checkStrings(p.Name, "retrieval.intent_patterns", r.IntentPatterns); err != nil {
			return err
		}
		p.Retrieval.IntentPatterns = r.IntentPatterns
	}
	if g := env.Guard; g != nil {
		if g.Check != nil {
			p.Guard.Check = *g.Check
		}
		if g.Candidate != nil {
			p.Guard.Candidate = *g.Candidate
		}
		if g.Mode != nil {
			p.Guard.Mode = *g.Mode
		}
		if g.Candidates != nil {
			p.Guard.Candidates = *g.Candidates
		}
		p.Guard.ThresholdDuplicate = g.ThresholdDuplicate
		p.Guard.ThresholdReview = g.ThresholdReview
	}
	if d := env.Dream; d != nil {
		if d.Linkable != nil {
			p.Dream.Linkable = *d.Linkable
		}
		if err := checkStrings(p.Name, "dream.link_classes", d.LinkClasses); err != nil {
			return err
		}
		p.Dream.LinkClasses = d.LinkClasses
	}
	if d := env.Digest; d != nil && d.Include != nil {
		p.Digest.Include = *d.Include
	}
	if o := env.Overview; o != nil && o.Include != nil {
		p.Overview.Include = *o.Include
	}
	if pa := env.Parent; pa != nil {
		if pa.Mode != nil {
			p.Parent.Mode = *pa.Mode
		}
		if pa.Relationship != nil {
			p.Parent.Relationship = *pa.Relationship
		}
	}
	if err := applyWorkflow(p, env.Workflow); err != nil {
		return err
	}
	return applyClassify(p, env.Classify)
}

// applyWorkflow overlays the workflow section (absent = keep zero = no workflow)
// and enforces its array caps. Cross-field validity is checked in validatePolicy.
func applyWorkflow(p *Policy, w *cfgWorkflow) error {
	if w == nil {
		return nil
	}
	if err := checkStrings(p.Name, "workflow.states", w.States); err != nil {
		return err
	}
	if err := checkStrings(p.Name, "workflow.terminal", w.Terminal); err != nil {
		return err
	}
	if len(w.ForgeStateMap) > maxArrayEntries {
		return fmt.Errorf("blocktype %q: workflow.forge_state_map exceeds %d entries (%d)", p.Name, maxArrayEntries, len(w.ForgeStateMap))
	}
	p.Workflow.States = w.States
	p.Workflow.Terminal = w.Terminal
	p.Workflow.ForgeStateMap = w.ForgeStateMap
	if w.Initial != nil {
		p.Workflow.Initial = *w.Initial
	}
	return nil
}

// applyClassify overlays the classify section and enforces its array caps.
func applyClassify(p *Policy, c *cfgClassify) error {
	if c == nil {
		return nil
	}
	if c.Priority != nil {
		p.Classify.Priority = *c.Priority
	}
	for field, arr := range map[string][]string{
		"classify.metadata_flags":  c.MetadataFlags,
		"classify.source_prefixes": c.SourcePrefixes,
		"classify.title_patterns":  c.TitlePatterns,
	} {
		if err := checkStrings(p.Name, field, arr); err != nil {
			return err
		}
	}
	p.Classify.MetadataFlags = c.MetadataFlags
	p.Classify.SourcePrefixes = c.SourcePrefixes
	p.Classify.TitlePatterns = c.TitlePatterns
	return nil
}

// validatePolicy enforces the cross-field rules of vocabulary v1 (§3.3).
func validatePolicy(p *Policy) error {
	switch p.Retrieval.Kind {
	case RetrievalFullPass, RetrievalExcluded:
		// no companion fields required
	case RetrievalDamped:
		if f := p.Retrieval.DampingFactor; f <= 0 || f > 1 {
			return fmt.Errorf("blocktype %q: retrieval.policy=damped requires damping_factor in (0,1], got %v", p.Name, f)
		}
	case RetrievalAggregateToParent:
		// Fail-closed until the T11 fold mechanism exists (§3.3/§5.2): the
		// vocabulary word is known, accepting it would be silent behaviour.
		return fmt.Errorf("blocktype %q: retrieval.policy=aggregate-to-parent is not accepted before the fold mechanism ships (wave T11)", p.Name)
	default:
		return fmt.Errorf("blocktype %q: unknown retrieval.policy %q", p.Name, p.Retrieval.Kind)
	}

	switch p.Parent.Mode {
	case ParentModeNone:
	case ParentModeOptional, ParentModeRequired:
		// Symmetric to aggregate-to-parent (§3.3 R1): no parent_id write path
		// exists yet — accepting the mode would be silently ineffective.
		return fmt.Errorf("blocktype %q: parent.mode=%q is not accepted before the parent_id write path ships (Achse 02)", p.Name, p.Parent.Mode)
	default:
		return fmt.Errorf("blocktype %q: unknown parent.mode %q", p.Name, p.Parent.Mode)
	}

	for field, thr := range map[string]*float64{
		"guard.threshold_duplicate": p.Guard.ThresholdDuplicate,
		"guard.threshold_review":    p.Guard.ThresholdReview,
	} {
		if thr != nil && (*thr <= 0 || *thr > 1) {
			return fmt.Errorf("blocktype %q: %s must be in (0,1], got %v", p.Name, field, *thr)
		}
	}

	switch p.Guard.Mode {
	case GuardModeArchive, GuardModeFlag:
	default:
		return fmt.Errorf("blocktype %q: unknown guard.mode %q (want archive|flag)", p.Name, p.Guard.Mode)
	}

	switch p.Guard.Candidates {
	case GuardCandidatesAll, GuardCandidatesSameScope:
	default:
		return fmt.Errorf("blocktype %q: unknown guard.candidates %q (want all|same-scope)", p.Name, p.Guard.Candidates)
	}
	return validateWorkflow(p)
}

// validateWorkflow enforces the workflow section cross-field rules (§4.1 form).
// An absent workflow section (States empty AND no companion fields) is valid —
// the type has no workflow. Any companion field WITHOUT states is a
// misconfiguration (silent-ineffective class, §5.2), so it rejects.
func validateWorkflow(p *Policy) error {
	w := &p.Workflow
	if len(w.States) == 0 {
		if w.Initial != "" || len(w.Terminal) > 0 || len(w.ForgeStateMap) > 0 {
			return fmt.Errorf("blocktype %q: workflow config requires a non-empty states set", p.Name)
		}
		return nil
	}
	stateSet := make(map[string]bool, len(w.States))
	for _, s := range w.States {
		if !statusFormat.MatchString(s) {
			return fmt.Errorf("blocktype %q: workflow.states entry %q violates format ^[a-z0-9][a-z0-9-]{0,49}$", p.Name, s)
		}
		if stateSet[s] {
			return fmt.Errorf("blocktype %q: workflow.states contains duplicate %q", p.Name, s)
		}
		stateSet[s] = true
	}
	if w.Initial == "" {
		return fmt.Errorf("blocktype %q: workflow.initial is required when states is set", p.Name)
	}
	if !stateSet[w.Initial] {
		return fmt.Errorf("blocktype %q: workflow.initial %q is not in states", p.Name, w.Initial)
	}
	for _, tstate := range w.Terminal {
		if !stateSet[tstate] {
			return fmt.Errorf("blocktype %q: workflow.terminal %q is not in states", p.Name, tstate)
		}
	}
	for forgeState, ctxStatus := range w.ForgeStateMap {
		if !statusFormat.MatchString(forgeState) {
			return fmt.Errorf("blocktype %q: workflow.forge_state_map key %q violates format ^[a-z0-9][a-z0-9-]{0,49}$", p.Name, forgeState)
		}
		if !stateSet[ctxStatus] {
			return fmt.Errorf("blocktype %q: workflow.forge_state_map[%q]=%q is not in states", p.Name, forgeState, ctxStatus)
		}
	}
	return nil
}

// checkStrings enforces the R1 array caps (§3.3) with the field path in the
// error.
func checkStrings(typeName, field string, arr []string) error {
	if len(arr) > maxArrayEntries {
		return fmt.Errorf("blocktype %q: %s exceeds %d entries (%d)", typeName, field, maxArrayEntries, len(arr))
	}
	for i, s := range arr {
		if len(s) > maxArrayItemLen {
			return fmt.Errorf("blocktype %q: %s[%d] exceeds %d chars (%d)", typeName, field, i, maxArrayItemLen, len(s))
		}
	}
	return nil
}
