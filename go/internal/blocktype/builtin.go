package blocktype

// Compiled-in builtin set — the fail-safe truth the registry starts from and
// falls back to (§4.1). It MUST stay byte-equivalent to the seed rows (072 for
// the four M035 enum classes, 084 for the issue/comment workflow types):
// the golden test applies the REAL migration from migrations.FS to a test DB
// and diffs the SELECT rows against exactly this set (drift gate — no
// test-local JSON copy, §4.1 R1). With this floor in place every deploy state
// is safe: without the table (pre-072) the process behaves exactly like
// today; with the table the DB rows overlay these values (merge reload).
//
// auditPatterns is the compiled-in mirror of the M072 audit-trail seed
// patterns — since T4 the ONLY code-side copy (rrf/pattern.go is a pure
// pattern-arg engine now, §4.4 #16; the T3-window duplication ended). The
// golden integration test pins this copy against the migration seeds, so a
// drift in either direction goes red.
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

// auditTrailDamping mirrors the historical Welle-41 damped factor (0.3),
// seeded as data in M072.
const auditTrailDamping = 0.3

// builtinPolicies returns fresh copies of the six builtin type policies (the
// four M035 enum classes + the issue/comment workflow types, Welle I-C). Fresh
// slices per call: Policies end up in immutable
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
			Guard:     GuardPolicy{Check: true, Candidate: true, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:     DreamPolicy{Linkable: true},
			Digest:    DigestPolicy{Include: true},
			Overview:  OverviewPolicy{Include: true},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
		},
		{
			Name: "reference", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalFullPass},
			Guard:     GuardPolicy{Check: true, Candidate: true, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
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
			Guard:    GuardPolicy{Check: true, Candidate: true, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
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
			Guard:     GuardPolicy{Check: true, Candidate: true, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:     DreamPolicy{Linkable: false},
			Digest:    DigestPolicy{Include: true},
			Overview:  OverviewPolicy{Include: true},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: 10, MetadataFlags: []string{"is_meta"}},
		},
		// ── Workflow-engine issue axis (design/02 §4.1, Welle I-C) ────────────
		// issue: full-pass retrieval, guard participates in FLAG mode (a
		// duplicate issue is surfaced, never auto-archived, §4.7) restricted to
		// its own scope, per-type thresholds 0.97/0.90; dream links issues but
		// digest + overview EXCLUDE them (10k+ issues/repo would flood the
		// topic-map and Louvain clustering, §6.8 — the LOOP overview gate);
		// workflow state machine backlog→in-progress→done with the forge
		// open/closed mapping; structural links references + duplicate-of.
		{
			Name: "issue", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalFullPass},
			Guard: GuardPolicy{
				Check: true, Candidate: true,
				Mode: GuardModeFlag, Candidates: GuardCandidatesSameScope,
				ThresholdDuplicate: f64(0.97), ThresholdReview: f64(0.90),
			},
			Dream:    DreamPolicy{Linkable: true},
			Digest:   DigestPolicy{Include: false},
			Overview: OverviewPolicy{Include: false},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Workflow: WorkflowPolicy{
				States:        []string{"backlog", "in-progress", "done"},
				Initial:       "backlog",
				Terminal:      []string{"done"},
				ForgeStateMap: map[string]string{"open": "backlog", "closed": "done"},
			},
			StructuralLinkClasses: []string{"references", "duplicate-of"},
			Classify:              ClassifyRules{Priority: DefaultClassifyPriority},
		},
		// comment: kept OUT of every autonomous pipeline — guard.check=false,
		// guard.candidate=false, dream.linkable=false, digest.include=false,
		// overview.include=false (all exact §4.1). INTERIM DEVIATION (§5.2 /
		// "kein Gate aufweichen"): §4.1 asks for retrieval=aggregate-to-parent
		// and parent.mode=required/comment-of. WF T11 now ships the aggregate-
		// to-parent FOLD consumer (QueryHandler.foldAggregates over
		// Set.AggregateTypes), so retrieval=aggregate-to-parent is no longer
		// blanket-rejected — but the cross-field rule ties it to parent.mode !=
		// none, and the parent_id WRITE path (store.PutBlockParent, consumer
		// I-D2's InsertCommentBlock) still has no production caller, so
		// parent.mode=required stays rejected (policy.go, Achse 02). The seed
		// flip itself is I-E, not T11. So comment ships with retrieval=excluded
		// (the safe subset of aggregate: a comment stays invisible, never leaks
		// raw into results) and parent.mode=none. FLIP TARGET: when the parent_id
		// write path lands (I-E era), update this row + seed to
		// retrieval=aggregate-to-parent, parent.mode=required,
		// relationship=comment-of. See the I-C wave return + design §9.1a.
		{
			Name: "comment", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalExcluded},
			Guard:     GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:     DreamPolicy{Linkable: false},
			Digest:    DigestPolicy{Include: false},
			Overview:  OverviewPolicy{Include: false},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
		},
	}
}

// f64 returns a pointer to a fresh float64 — per-call allocation so per-type
// threshold pointers are never shared across builtinPolicies() generations
// (same immutability rationale as the fresh pattern slices above).
func f64(v float64) *float64 { return &v }

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
