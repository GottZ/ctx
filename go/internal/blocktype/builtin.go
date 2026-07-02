package blocktype

// Compiled-in builtin set — the fail-safe truth the registry starts from and
// falls back to (§4.1). It MUST stay byte-equivalent to the 072 seed rows:
// the golden test applies the REAL migration from migrations.FS to a test DB
// and diffs the SELECT rows against exactly this set (drift gate — no
// test-local JSON copy, §4.1 R1). With this floor in place every deploy state
// is safe: without the table (pre-072) the process behaves exactly like
// today; with the table the DB rows overlay these values (merge reload).
//
// auditPatterns is a deliberate second copy of internal/rrf/pattern.go:25-36
// for the T3 window: pattern.go stays the live consumer until T4/T5 turn it
// into a pattern-arg engine and retire its own list (§4.4 #16). The golden
// test pins THIS copy against the migration; TestBuiltinPatternsMatchRRF pins
// it against the rrf copy, so a drift in either direction goes red.
var auditPatterns = []string{
	"session",
	"welle",
	"audit",
	"recurrent",
	"handover",
	"self-audit",
	"dream v",
	"performance",
	"reset",
	"baseline",
}

// auditTrailDamping mirrors rrf.AuditTrailFactor's damped branch (0.3).
const auditTrailDamping = 0.3

// builtinPolicies returns fresh copies of the four builtin type policies
// (M035 enum classes). Fresh slices per call: Policies end up in immutable
// Sets — shared backing arrays between generations would let one Set's
// consumer observe another's mutation.
func builtinPolicies() []Policy {
	patterns := func() []string {
		out := make([]string, len(auditPatterns))
		copy(out, auditPatterns)
		return out
	}
	return []Policy{
		{
			Name: "knowledge", Scope: globalScope, Builtin: true, IsDefault: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalFullPass},
			Guard:     GuardPolicy{Check: true, Candidate: true},
			Dream:     DreamPolicy{Linkable: true},
			Digest:    DigestPolicy{Include: true},
			Overview:  OverviewPolicy{Include: true},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
		},
		{
			Name: "reference", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalFullPass},
			Guard:     GuardPolicy{Check: true, Candidate: true},
			Dream:     DreamPolicy{Linkable: true},
			Digest:    DigestPolicy{Include: true},
			Overview:  OverviewPolicy{Include: true},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
		},
		{
			Name: "audit-trail", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{
				Kind:           RetrievalDamped,
				DampingFactor:  auditTrailDamping,
				IntentPatterns: patterns(),
			},
			Guard:    GuardPolicy{Check: true, Candidate: true},
			Dream:    DreamPolicy{Linkable: true},
			Digest:   DigestPolicy{Include: true},
			Overview: OverviewPolicy{Include: true},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Classify: ClassifyRules{
				Priority:       20,
				SourcePrefixes: []string{"dream-"},
				TitlePatterns:  patterns(),
			},
		},
		{
			Name: "system-meta", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalExcluded},
			Guard:     GuardPolicy{Check: true, Candidate: true},
			Dream:     DreamPolicy{Linkable: false},
			Digest:    DigestPolicy{Include: true},
			Overview:  OverviewPolicy{Include: true},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: 10, MetadataFlags: []string{"is_meta"}},
		},
	}
}

// builtinSet builds the compiled-in fallback Set. Panics on an invalid
// builtin definition — that is a programming error caught by every test run,
// never a runtime condition.
func builtinSet() *Set {
	s, err := NewSet(builtinPolicies())
	if err != nil {
		panic("blocktype: builtin set invalid: " + err.Error())
	}
	return s
}
