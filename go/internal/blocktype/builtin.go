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

// auditTrailDamping is the MEASURED damped factor, not the historical one. The
// Welle-41 value 0.3 (seeded as data in M072, folded into 113_baseline.sql) was
// last scored in Welle 40 under the UNIFORM damping regime — an unpaired
// eval-cyclic bench with no declared noise floor (M036/M037/M038 in
// 113_baseline.sql:3646-3658, :3865-3868) — and never again once Welle 41 put
// the query-aware intent lift around it; W01-M1 (2026-08-27) scored the world
// at 0.6 against it on 1 000 paired gold cases and found +0.035954 nDCG@10 on
// G-KI, 95-% CI [+0.018784, +0.055984], 11.2x the X-W1 noise floor, McNemar
// 13/0 with p = 0.000244 — and across all five slices 22 cases gained their
// Hit@5 while not one lost it. Migration 146 lifts the row to match; the two
// move in lockstep or TestRegistryGolden_Integration goes red.
const auditTrailDamping = 0.6

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

// insightPatterns / catalogPatterns are the compiled-in mirrors of the M143
// intent-pattern seeds (auditPatterns precedent above). They are INERT while
// both types are retrieval=excluded — Set.DampedTypesFor only ever walks damped
// types — and they are carried anyway, so the later visibility switch (E-4,
// after the pilots) is a one-field data change over a list whose false-lift
// rate against the 47 eval questions has already been measured
// (derived_types_test.go), rather than the moment somebody first thinks about
// patterns. Same multi-word reasoning as the tool lists: MatchesAny is a
// case-insensitive SUBSTRING test with no partial lift.
var insightPatterns = []string{
	"session insight",
	"sitzungs-erkenntnis",
	"was haben wir gelernt",
	"erkenntnisse der session",
	"was lief schief",
	"was ist passiert",
	"lessons learned",
	"befunde der sitzung",
}

var catalogPatterns = []string{
	"katalog",
	"überblick über",
	"übersicht über",
	"worum geht es bei",
	"was gibt es zu",
	"themenübersicht",
	"welche themen",
	"gib mir einen überblick",
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

// builtinPolicies returns fresh copies of the eleven builtin type policies (the
// four M035 enum classes + the issue/comment workflow types, Welle I-C +
// checkpoint, M107 + the two tool-evidence axes, M136 + the two derived
// knowledge layers, M143). Fresh
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
				// Foreign text by definition (W02-4): the payload is captured
				// tool output — commands, file names, stderr — every byte of
				// which a third party can shape. Damping keeps it from flooding
				// the ranking; this flag keeps the synthesis prompt from
				// reading what survives as knowledge or as an instruction.
				Untrusted: true,
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
				// Same foreign-text reasoning as tool-evidence: summarising
				// attacker-shapable output does not launder it.
				Untrusted: true,
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
			Retrieval: RetrievalPolicy{
				Kind: RetrievalExcluded,
				// Foreign text for the same reason as the two tool axes, one
				// level up (V-W7, migration 141): transcript prose reproduces
				// tool output, fetched web content and foreign agent prompts,
				// so a third party shapes part of what this type carries.
				//
				// The flag is INERT here — an excluded type never reaches a
				// synthesis prompt, so nothing renders trust="untrusted"
				// today — and it is set anyway, because a DERIVED type that
				// distils this material has to be able to READ the property at
				// its source. The rule such a type runs is fail-closed:
				// untrusted unless EVERY source class is provably first-party.
				// With checkpoint unflagged that rule answers "first-party"
				// and the derived layer launders foreign text into knowledge.
				// Being excluded is WHY the flag was never needed before, not
				// evidence that the content is trustworthy.
				Untrusted: true,
			},
			Guard:    GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:    DreamPolicy{Linkable: false},
			Digest:   DigestPolicy{Include: false},
			Overview: OverviewPolicy{Include: false},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Classify: ClassifyRules{Priority: 30, TitlePatterns: []string{"compaction source", "compaction checkpoint"}},
		},
		// ── Derived knowledge layers (design D-01 §3.3/§3.4 + §4.2, M143) ────
		// insight + catalog are the two first-order derivatives: blocks written
		// ABOUT other blocks in this store. Both are seeded by ONE migration
		// (masterplan K2) because they share the type counters and the schema
		// contract manifest, and neither is a knob the other could be tuned
		// without.
		//
		// BOTH START retrieval=excluded, and that is a decision, not an
		// oversight. D-01 proposed damped at 0.50/0.60, D-02 0.35, D-03
		// "excluded until measured"; masterplan K7 — user-confirmed as board
		// decision E-4 — resolves it to excluded for both until the pilots
		// (X-W4/X-W5), after which the swept factor (M-W8 over {0.25…1.0})
		// arrives as a REGISTRY DATA update. Both excluded and damped are
		// reversible data positions; full-pass would not be. No damping_factor
		// is carried: a factor on an excluded row is inert today and would
		// silently become the START value the moment the policy flips, which is
		// exactly the "Startwert" K7 refused.
		//
		// BOTH CARRY write.internal_only=true since C2-8 (D-02 §3.1, bruchpfad
		// BA14; the migration half is 148). It is the REGISTRY half of the write
		// lock whose compiled half is derived.StratumOf: these two types are the
		// only ones in the set that are simultaneously guard.check=false,
		// guard.candidate=false and retrieval-eligible once E-4 flips them to
		// damped, so a client that could name them would own a corpus channel
		// that never enters the dedup queue and never gets archived as a
		// near-duplicate. The field additionally gives DecodePolicy a floor to
		// refuse against, which is what stops a `{"v":1}` PUT on these two rows
		// from resetting the whole posture above to its wide defaults.
		//
		// The rest of the posture is shared and total: guard.check=false AND
		// guard.candidate=false (a derivative must neither archive its ORIGINAL
		// nor itself — the second would orphan its own regeneration),
		// dream.linkable=false (dream links are the ONLY input Louvain reads, so
		// a linkable derivative would shape the partition it is derived from),
		// digest.include=false, overview.include=false (§0/K1 — a catalogue in
		// the topic map derives the topic map from itself), parent.mode=none
		// (parent_id is ON DELETE CASCADE and carries ONE parent; a derivative
		// has N sources).
		//
		// insight: anchored on root session × watermark — strictly monotonic,
		// never dies, append-only. retrieval.untrusted=true because it distils
		// transcript and tool material, and M138's doctrine is verbatim that
		// summarising attacker-shapable output does not launder it; M141 flagged
		// checkpoint precisely so this layer can read the property AT ITS
		// SOURCE. Classify priority 17 is BELOW audit-trail's 20 for the same
		// first-match-wins reason as the tool types, and here it is not
		// hypothetical: measured on the pre-wave tree, "Session insights <root>
		// ab #<watermark>" classified to audit-trail — which guards AND dreams.
		{
			Name: "insight", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{
				Kind:           RetrievalExcluded,
				IntentPatterns: fresh(insightPatterns),
				Untrusted:      true,
			},
			Guard:    GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:    DreamPolicy{Linkable: false},
			Digest:   DigestPolicy{Include: false},
			Overview: OverviewPolicy{Include: false},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Write:    WritePolicy{InternalOnly: true},
			Classify: ClassifyRules{Priority: 17, TitlePatterns: []string{"session insights "}},
		},
		// catalog: anchored on a cluster topic — which drifts, dies and merges —
		// and overwritten in place, one block per living topic.
		// retrieval.untrusted stays FALSE and is written explicitly: this type
		// distils corpus blocks somebody wrote as knowledge, and a single
		// untrusted SOURCE is framed per block via the inheritance clause
		// (§4.8.3) instead of flipping the whole type. The explicit false is
		// also what an existence-guarded blanket backfill of the M138/M141 shape
		// would have to step over deliberately. Classify priority 16 sits below
		// insight's 17 because catalog is the larger population at target scale;
		// the two title patterns are disjoint, so their relative order is
		// inconsequential.
		{
			Name: "catalog", Scope: globalScope, Builtin: true,
			Retrieval: RetrievalPolicy{
				Kind:           RetrievalExcluded,
				IntentPatterns: fresh(catalogPatterns),
				Untrusted:      false,
			},
			Guard:    GuardPolicy{Check: false, Candidate: false, Mode: GuardModeArchive, Candidates: GuardCandidatesAll},
			Dream:    DreamPolicy{Linkable: false},
			Digest:   DigestPolicy{Include: false},
			Overview: OverviewPolicy{Include: false},
			Parent:   ParentPolicy{Mode: ParentModeNone},
			Write:    WritePolicy{InternalOnly: true},
			Classify: ClassifyRules{Priority: 16, TitlePatterns: []string{"katalog #"}},
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
