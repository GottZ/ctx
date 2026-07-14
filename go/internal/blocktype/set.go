package blocktype

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/GottZ/ctx/internal/rrf"
)

// Set is an immutable snapshot of one resolved type namespace (analogous to
// *config.Config). Consumers take ONE Set per operation (request, guard
// batch, dream cycle, digest run) and must not mutate returned slices —
// they are shared with every other holder of the same generation.
type Set struct {
	policies    map[string]Policy
	defaultName string

	// Precomputed, name-sorted policy slices — the hot-path accessors return
	// them without allocation.
	visible        []string // retrieval ∈ {full-pass, damped, aggregate-to-parent}
	guardCheck     []string // guard.check
	guardCandidate []string // guard.candidate
	dreamLinkable  []string // dream.linkable
	digestTypes    []string // digest.include
	overviewTypes  []string // overview.include
	aggregate      []string // retrieval == aggregate-to-parent
	damped         []dampedEntry
	classifyOrder  []Policy // sorted by (priority, name)
	// structuralClasses is the sorted DISTINCT union of every type's
	// structural_link_classes — the request-snapshot vocabulary for the
	// link_class partition (GB5, design/02 §4.2/§4.5).
	structuralClasses []string
}

// dampedEntry holds the per-type damping data for DampedTypesFor. Matching
// runs through rrf.MatchesAny — ONE engine for read-side intent lift and
// write-side classification (T4, design/01 §4.5 dual-use decoupling).
type dampedEntry struct {
	name     string
	factor   float64
	patterns []string
}

// NewSet builds an immutable Set from decoded policies. It requires at least
// one policy and exactly one default (the classifier fallback; the 072
// partial unique index guarantees ≤1 per scope in the DB — 0 or 2 after a
// merge is a corrupt-config event and fails loudly).
func NewSet(policies []Policy) (*Set, error) {
	if len(policies) == 0 {
		return nil, fmt.Errorf("blocktype: empty policy set")
	}
	s := &Set{policies: make(map[string]Policy, len(policies))}
	for _, p := range policies {
		if _, dup := s.policies[p.Name]; dup {
			return nil, fmt.Errorf("blocktype: duplicate policy name %q", p.Name)
		}
		s.policies[p.Name] = p
		if p.IsDefault {
			if s.defaultName != "" {
				return nil, fmt.Errorf("blocktype: multiple default types (%q, %q)", s.defaultName, p.Name)
			}
			s.defaultName = p.Name
		}
	}
	if s.defaultName == "" {
		return nil, fmt.Errorf("blocktype: no default type in set")
	}

	names := make([]string, 0, len(s.policies))
	for n := range s.policies {
		names = append(names, n)
	}
	sort.Strings(names)

	structSet := map[string]bool{}
	for _, n := range names {
		p := s.policies[n]
		for _, c := range p.StructuralLinkClasses {
			// Defense-in-depth (split precedence, design/01 §4.6(b)): a dream
			// name can never route into the structural vocabulary. DecodePolicy
			// already rejects reserved names (GA6) — this arm only matters for
			// a hypothetical decode-bypass and is deliberately silent (the
			// loud path is the GA7 tenant-fallback warn).
			if slices.Contains(reservedGraphClasses, c) {
				continue
			}
			structSet[c] = true
		}
		switch p.Retrieval.Kind {
		case RetrievalFullPass, RetrievalDamped, RetrievalAggregateToParent:
			s.visible = append(s.visible, n)
		}
		if p.Retrieval.Kind == RetrievalDamped {
			s.damped = append(s.damped, dampedEntry{name: n, factor: p.Retrieval.DampingFactor, patterns: p.Retrieval.IntentPatterns})
		}
		if p.Retrieval.Kind == RetrievalAggregateToParent {
			s.aggregate = append(s.aggregate, n)
		}
		if p.Guard.Check {
			s.guardCheck = append(s.guardCheck, n)
		}
		if p.Guard.Candidate {
			s.guardCandidate = append(s.guardCandidate, n)
		}
		if p.Dream.Linkable {
			s.dreamLinkable = append(s.dreamLinkable, n)
		}
		if p.Digest.Include {
			s.digestTypes = append(s.digestTypes, n)
		}
		if p.Overview.Include {
			s.overviewTypes = append(s.overviewTypes, n)
		}
		s.classifyOrder = append(s.classifyOrder, p)
	}
	sort.SliceStable(s.classifyOrder, func(i, j int) bool {
		if s.classifyOrder[i].Classify.Priority != s.classifyOrder[j].Classify.Priority {
			return s.classifyOrder[i].Classify.Priority < s.classifyOrder[j].Classify.Priority
		}
		return s.classifyOrder[i].Name < s.classifyOrder[j].Name
	})
	for c := range structSet {
		s.structuralClasses = append(s.structuralClasses, c)
	}
	sort.Strings(s.structuralClasses)
	return s, nil
}

// StructuralClasses returns the sorted distinct union of every registered
// type's structural_link_classes — the accepted structural vocabulary for the
// link_class partition (GB5). Precomputed, no allocation on the hot path.
func (s *Set) StructuralClasses() []string { return s.structuralClasses }

// Resolve returns the policy for name. ok=false means the name is not
// registered — callers treat that fail-closed (§5.1: invisible, no
// candidate, no dream, no digest).
func (s *Set) Resolve(name string) (Policy, bool) {
	p, ok := s.policies[name]
	return p, ok
}

// Default returns the is_default policy (seed: knowledge) — the classifier
// fallback when no rule matches.
func (s *Set) Default() Policy {
	return s.policies[s.defaultName]
}

// Names returns all registered type names, sorted. (Orphan sweep + tests.)
func (s *Set) Names() []string {
	names := make([]string, 0, len(s.policies))
	for n := range s.policies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// VisibleTypes returns the retrieval allowlist: every type whose policy is
// full-pass, damped or aggregate-to-parent. This slice becomes ctx_rrf's
// p_types_visible (T5) — NULL/empty means 0 hits by design (fail-closed).
func (s *Set) VisibleTypes() []string { return s.visible }

// DampedTypesFor returns the parallel (types, factors) arrays for the query:
// damped types whose intent patterns do NOT match the query. An intent match
// lifts the type out of the arrays (factor 1.0 downstream) — generalizes the
// former rrf.AuditTrailFactor; matching is rrf.MatchesAny (the shared engine).
func (s *Set) DampedTypesFor(query string) ([]string, []float64) {
	if len(s.damped) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(s.damped))
	factors := make([]float64, 0, len(s.damped))
	for _, d := range s.damped {
		if rrf.MatchesAny(query, d.patterns) {
			continue // intent lift: type leaves the damping arrays
		}
		names = append(names, d.name)
		factors = append(factors, d.factor)
	}
	return names, factors
}

// GuardCheckTypes returns the types whose blocks enter the guard batch.
func (s *Set) GuardCheckTypes() []string { return s.guardCheck }

// GuardCandidateTypes returns the types admissible as guard match candidates.
func (s *Set) GuardCandidateTypes() []string { return s.guardCandidate }

// GuardThresholds resolves the per-type guard thresholds; null config fields
// and unknown names fall back to the global 0.98/0.92 defaults.
func (s *Set) GuardThresholds(name string) (dup, review float64) {
	dup, review = DefaultThresholdDuplicate, DefaultThresholdReview
	p, ok := s.policies[name]
	if !ok {
		return dup, review
	}
	if p.Guard.ThresholdDuplicate != nil {
		dup = *p.Guard.ThresholdDuplicate
	}
	if p.Guard.ThresholdReview != nil {
		review = *p.Guard.ThresholdReview
	}
	return dup, review
}

// GuardMode resolves the per-type guard persist mode for a near_duplicate
// finding (GuardModeArchive | GuardModeFlag). Unknown names fall back to the
// archive bestand (design/02 §4.7). guard.go passes the resolved mode into
// applyDecision so a flag-mode type is NEVER auto-archived.
func (s *Set) GuardMode(name string) string {
	p, ok := s.policies[name]
	if !ok {
		return GuardModeArchive
	}
	return p.Guard.Mode
}

// GuardSameScopeOnly reports whether the type's guard candidate set is
// restricted to the checked block's own scope (guard.candidates=same-scope).
// Unknown names fall back to false (cross-scope bestand). guard.go passes the
// resolved value EXPLICITLY as p_same_scope_only at the ctx_guard_check call
// site — never relying on the SQL default (design/02 §4.7/§5.3).
func (s *Set) GuardSameScopeOnly(name string) bool {
	p, ok := s.policies[name]
	if !ok {
		return false
	}
	return p.Guard.Candidates == GuardCandidatesSameScope
}

// ParentMode resolves the type's parent.mode (ParentModeNone|Optional|Required,
// unlocked in Achse 02 Welle I-D). Unknown names and the zero value fall back to
// ParentModeNone (no parent). The comment write path (store.InsertCommentBlock,
// via the manage/REST handler) consults it: parent.mode=required means a block of
// that type MUST be created with a parent (orphan prevention, design/01 §9.1a).
func (s *Set) ParentMode(name string) string {
	p, ok := s.policies[name]
	if !ok || p.Parent.Mode == "" {
		return ParentModeNone
	}
	return p.Parent.Mode
}

// DreamLinkableTypes returns the types admitted to the dream loop — BOTH
// sides: pick eligibility and candidate/target sieve (§3.3 R1).
func (s *Set) DreamLinkableTypes() []string { return s.dreamLinkable }

// DigestTypes returns the types included in the topic-map source.
func (s *Set) DigestTypes() []string { return s.digestTypes }

// OverviewTypes returns the types included in the overview clustering.
func (s *Set) OverviewTypes() []string { return s.overviewTypes }

// AggregateTypes returns the types with retrieval=aggregate-to-parent — the
// input to the WF T11 query-handler fold (QueryHandler.foldAggregates). Empty
// on the builtin set (no builtin type aggregates; the comment seed ships
// excluded, flipped only in the I-E era), so the fold is a zero-DB no-op on the
// current corpus (eval baseline-neutral).
func (s *Set) AggregateTypes() []string { return s.aggregate }

// Classify runs every type's classify rules in (priority, name) order,
// first match wins. No match returns the default type name with
// matched=false. Semantics mirror the pre-T4 store.ClassifyBlockAfterUpsert
// decision tree (metadata flag → source prefix → title pattern, per type) —
// pinned by the T4 golden corpus (store/classify_golden_test.go) that was
// generated against the old tree BEFORE it was replaced.
//
// Source prefixes match as PROPER prefixes: the source must carry a payload
// after the prefix (old tree: len(src) > 6 && src[:6] == "dream-" — a source
// that IS the bare prefix, e.g. exactly "dream-", never matched and still
// does not). Title patterns run through rrf.MatchesAny (shared engine, §4.5).
func (s *Set) Classify(title string, metadata map[string]any) (string, bool) {
	source, _ := metadata["source"].(string)
	for _, p := range s.classifyOrder {
		for _, flag := range p.Classify.MetadataFlags {
			if v, ok := metadata[flag].(bool); ok && v {
				return p.Name, true
			}
		}
		for _, prefix := range p.Classify.SourcePrefixes {
			if prefix != "" && len(source) > len(prefix) && strings.HasPrefix(source, prefix) {
				return p.Name, true
			}
		}
		if rrf.MatchesAny(title, p.Classify.TitlePatterns) {
			return p.Name, true
		}
	}
	return s.defaultName, false
}
