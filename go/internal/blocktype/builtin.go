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

// toolEvidencePatterns / toolOverviewPatterns are the compiled-in mirrors of
// the M136 intent-pattern seeds (auditPatterns precedent above). Deliberately
// multi-word and domain-specific: DampedTypesFor lifts a type COMPLETELY on
// the first rrf.MatchesAny hit, and MatchesAny is a case-insensitive SUBSTRING
// test — generic single words ("tool", "exit", "shell") would disable the
// damping for a large share of ordinary queries. The false-lift rate against
// the 47 eval questions is measured, not asserted (tool_evidence_test.go).
var toolEvidencePatterns = []string{
	"tool index",
	"tool output",
	"tool call",
	"tool-ausgabe",
	"stderr",
	"stdout",
	"exit code",
	"exit-code",
	"traceback",
	"kommando",
	"befehl",
	"kommandozeile",
	"welches kommando",
	"welcher befehl",
	"terminal-ausgabe",
	"shell-befehl",
}

var toolOverviewPatterns = []string{
	"tool overview",
	"werkzeug-übersicht",
	"welche werkzeuge",
	"welche kommandos",
	"welche befehle",
	"welche dateien",
	"tool-fehler",
	"fehlgeschlagene calls",
	"artefakt-übersicht",
	"was wurde bearbeitet",
}

// toolEvidenceDamping is sharper than auditTrailDamping: the tool-index
// population grows with every compaction and is near-duplicate BY
// CONSTRUCTION. toolOverviewDamping is milder because the overview axis is
// upsert-stable (one block per axis per root session) — the flooding argument
// that drives 0.15 does not apply to it. Both seeded as data in M136.
const (
	toolEvidenceDamping = 0.15
	toolOverviewDamping = 0.35
)

// builtinPolicies returns fresh copies of the nine builtin type policies (the
// four M035 enum classes + the issue/comment workflow types, Welle I-C +
// checkpoint, M107 + the two tool-evidence axes, M136). Fresh
// slices per call: Policies end up in immutable
// Sets — shared backing arrays between generations would let one Set's
// consumer observe another's mutation.
func builtinPolicies() []Policy {
	patterns := func() []string {
		out := make([]string, len(auditPatterns))
		copy(out, auditPatterns)
		return out
	}
	fresh := func(src []string) []string {
		out := make([]string, len(src))
		copy(out, src)
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
			// M103: the daily synthesis report writes deterministic
			// report→source edges (writeReportSourceLinks); the class must be
			// declared on the source type (design/02 §4.1 fail-closed).
			StructuralLinkClasses: []string{"references"},
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
		// overview.include=false (all exact §4.1). FLIPPED to the §4.1 target in
		// Welle I-E (migration 085): retrieval=aggregate-to-parent (a ranked
		// comment folds onto its parent issue via QueryHandler.foldAggregates over
		// Set.AggregateTypes — the T11 fold consumer, now live) + parent.mode=
		// required/comment-of (the parent_id WRITE path store.InsertCommentBlock /
		// PutBlockParent is live since I-D, so required is effective, not silently
		// ineffective). The interim shape (retrieval=excluded, parent.mode=none)
		// that 084 planted while both mechanisms were unbuilt is retired. This row
		// and migration 085 move in lockstep (registry drift gate). The other
		// pipeline fields stay OFF: a comment never guards/dreams/digests/overviews.
		{
			Name: "comment", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalAggregateToParent},
			Guard:     GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:     DreamPolicy{Linkable: false},
			Digest:    DigestPolicy{Include: false},
			Overview:  OverviewPolicy{Include: false},
			Parent:    ParentPolicy{Mode: ParentModeRequired, Relationship: "comment-of"},
			Classify:  ClassifyRules{Priority: DefaultClassifyPriority},
		},
		// ── Tool-evidence axes (design/02 §3.1 + design/02a §3, M136) ────────
		// tool-evidence: the per-compaction tool index. A SEPARATE type from
		// checkpoint precisely because checkpoint is retrieval=excluded: an
		// excluded type gets no embed slot and never answers "which command was
		// that again?" — and that question IS the purpose of this axis
		// (checkpoint resolves over exact IDs, this one over queries; two
		// resolution modes, two types). Damped at 0.15 rather than full-pass
		// because the population grows with every compaction and is
		// near-duplicate BY CONSTRUCTION (same tools, similar commands,
		// overlapping windows) — at the 1M+ target scale that floods candidate
		// sets and overflows the reranker slot window, the same mechanic that
		// forced 107 to exclude transcript parts outright. Out of every
		// autonomous pipeline for the checkpoint reason verbatim: the guard's
		// default archive lane would archive consecutive indices as duplicates
		// and break the manifest→index ID chain (2026-07-20 dangling-manifest
		// incident). Priority 19 is BELOW audit-trail's 20, not above it:
		// Classify runs ascending / first match wins, and the title carries the
		// ROOT SESSION NAME — a root containing "session"/"audit"/"baseline"/
		// "reset" would hand the block to audit-trail, which guards AND dreams.
		// The reverse cannot happen: "compaction checkpoint tool index" is a
		// four-word chain that occurs in no real audit-trail or checkpoint title.
		{
			Name: "tool-evidence", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{
				Kind:           RetrievalDamped,
				DampingFactor:  toolEvidenceDamping,
				IntentPatterns: fresh(toolEvidencePatterns),
			},
			Guard:    GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:    DreamPolicy{Linkable: false},
			Digest:   DigestPolicy{Include: false},
			Overview: OverviewPolicy{Include: false},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Classify: ClassifyRules{Priority: 19, TitlePatterns: []string{"compaction checkpoint tool index"}},
		},
		// tool-overview: the per-root-session tool summary. Same pipeline
		// posture as tool-evidence, but damped at 0.35 instead of 0.15 — the
		// flooding argument does not apply here: the axis is upsert-stable (one
		// block per axis per root session), so the population stays small. 0.35
		// sits just above the historical audit-trail 0.3 line. Priority 18 for
		// the same first-match-wins reason as tool-evidence; the two title
		// patterns are disjoint, so their order relative to each other is
		// inconsequential.
		{
			Name: "tool-overview", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{
				Kind:           RetrievalDamped,
				DampingFactor:  toolOverviewDamping,
				IntentPatterns: fresh(toolOverviewPatterns),
			},
			Guard:    GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:    DreamPolicy{Linkable: false},
			Digest:   DigestPolicy{Include: false},
			Overview: OverviewPolicy{Include: false},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Classify: ClassifyRules{Priority: 18, TitlePatterns: []string{"compaction tool overview"}},
		},
		// checkpoint: ID-anchored evidence blocks (compaction-checkpoint
		// manifests + transcript source parts, migration 107). Resolution runs
		// EXCLUSIVELY over exact block IDs (manifest content/metadata carry the
		// source_block_ids + parent_manifest chain), so the type stays out of
		// every autonomous pipeline: retrieval=excluded (transcript parts are
		// token-dense near-duplicates — in retrieval they flood candidate sets
		// and overflow the reranker slot window), guard.check=false AND
		// guard.candidate=false (consecutive checkpoints of one session are
		// near-duplicates BY CONSTRUCTION — the default archive lane silently
		// broke ID chains, the 2026-07-20 dangling-manifest incident),
		// dream/digest/overview all false. Classified by the two stable writer
		// title prefixes ("Compaction source …" for transcript parts, M107;
		// "Compaction checkpoint head …" for the ID-anchoring manifest, M120 —
		// heads used to fall through to the default type and re-entered exactly
		// the pipelines this type closes), priority 30 after system-meta/
		// audit-trail; writers SHOULD still set type=checkpoint explicitly.
		{
			Name: "checkpoint", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{Kind: RetrievalExcluded},
			Guard:     GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:     DreamPolicy{Linkable: false},
			Digest:    DigestPolicy{Include: false},
			Overview:  OverviewPolicy{Include: false},
			Parent:    ParentPolicy{Mode: ParentModeNone},
			Classify:  ClassifyRules{Priority: 30, TitlePatterns: []string{"compaction source", "compaction checkpoint"}},
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
