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

// RetrievalPolicy — visibility class of a type in the retrieval pipelines.
type RetrievalPolicy struct {
	Kind string
	// DampingFactor is the score multiplier for Kind==damped, in (0,1].
	DampingFactor float64
	// IntentPatterns are case-insensitive query substrings that LIFT the
	// damping for that query (factor 1.0) — generalizes rrf.AuditTrailFactor.
	IntentPatterns []string
}

// GuardPolicy — participation of a type in the dedup guard.
type GuardPolicy struct {
	Check     bool // block is itself guard-checked (batch eligibility)
	Candidate bool // block is a match candidate for others
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
		Guard:     GuardPolicy{Check: true, Candidate: true},
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
	return applyClassify(p, env.Classify)
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
