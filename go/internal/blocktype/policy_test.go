package blocktype

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// validBase is a minimal valid v1 config.
const validBase = `{"v": 1}`

func TestDecodePolicyDefaults(t *testing.T) {
	p, err := DecodePolicy("some-type", globalScope, false, false, []byte(validBase))
	if err != nil {
		t.Fatalf("DecodePolicy: %v", err)
	}
	if p.Retrieval.Kind != RetrievalFullPass {
		t.Errorf("retrieval default = %q, want full-pass", p.Retrieval.Kind)
	}
	if !p.Guard.Check || !p.Guard.Candidate {
		t.Errorf("guard defaults = %+v, want check+candidate true", p.Guard)
	}
	// I-J vocabulary defaults MUST be the bestand (archive / all): a rollback
	// or an un-migrated type must keep the knowledge-line guard behaviour.
	if p.Guard.Mode != GuardModeArchive {
		t.Errorf("guard.mode default = %q, want archive (bestand)", p.Guard.Mode)
	}
	if p.Guard.Candidates != GuardCandidatesAll {
		t.Errorf("guard.candidates default = %q, want all (bestand)", p.Guard.Candidates)
	}
	if !p.Dream.Linkable {
		t.Error("dream.linkable default = false, want true")
	}
	if !p.Digest.Include || !p.Overview.Include {
		t.Errorf("digest/overview include defaults false, want true")
	}
	if p.Parent.Mode != ParentModeNone {
		t.Errorf("parent.mode default = %q, want none", p.Parent.Mode)
	}
	if p.Classify.Priority != DefaultClassifyPriority {
		t.Errorf("classify.priority default = %d, want %d", p.Classify.Priority, DefaultClassifyPriority)
	}
}

// TestDecodePolicyRejects pins every §3.3/§5.2 reject rule — each case is a
// negative probe: a tolerant decoder (silent default behaviour on typo /
// unknown vocabulary) turns at least one of these green-to-red.
func TestDecodePolicyRejects(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		errPart string
	}{
		{"unknown top-level key (guards typo)", `{"v":1,"guards":{"check":false}}`, "guards"},
		{"unknown sub-key", `{"v":1,"guard":{"cheack":false}}`, "cheack"},
		{"missing v", `{}`, "envelope version"},
		{"wrong v", `{"v":2}`, "envelope version"},
		{"damped without factor", `{"v":1,"retrieval":{"policy":"damped"}}`, "damping_factor"},
		{"damped factor zero", `{"v":1,"retrieval":{"policy":"damped","damping_factor":0}}`, "damping_factor"},
		{"damped factor >1", `{"v":1,"retrieval":{"policy":"damped","damping_factor":1.5}}`, "damping_factor"},
		{"unknown retrieval policy", `{"v":1,"retrieval":{"policy":"invisible"}}`, "retrieval.policy"},
		// WF T11: the blanket "wave T11" reject is gone; aggregate-to-parent now
		// hits the cross-field rule when parent.mode is (default) none. errPart
		// "parent.mode" is RED against the pre-T11 blanket message (which never
		// mentioned parent.mode) and GREEN against the cross-field message.
		{"aggregate-to-parent with parent.mode=none (cross-field)", `{"v":1,"retrieval":{"policy":"aggregate-to-parent"}}`, "parent.mode"},
		// parent.mode optional|required are UNLOCKED in Welle I-D (positive probe:
		// TestDecodePolicyParentModeUnlocked); only an UNKNOWN mode still rejects.
		{"unknown parent.mode", `{"v":1,"parent":{"mode":"maybe"}}`, "parent.mode"},
		{"threshold out of range", `{"v":1,"guard":{"threshold_duplicate":1.2}}`, "threshold_duplicate"},
		{"unknown guard.mode", `{"v":1,"guard":{"mode":"delete"}}`, "guard.mode"},
		{"unknown guard.candidates", `{"v":1,"guard":{"candidates":"tenant"}}`, "guard.candidates"},
		// I-C structural_link_classes (design/02 §4.1).
		{"structural_link_classes bad format", `{"v":1,"structural_link_classes":["Bad Class"]}`, "structural_link_classes"},
		{"structural_link_classes duplicate", `{"v":1,"structural_link_classes":["references","references"]}`, "duplicate"},
		// Typo of the new key still rejects as unknown (strict decode, key path):
		// this is the negative counterpart proving structural_link_classes had to
		// be ADDED to the envelope — a config carrying it would reject before I-C.
		{"structural_link_classes typo key", `{"v":1,"structural_links":["references"]}`, "structural_links"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodePolicy("some-type", globalScope, false, false, []byte(tc.config))
			if err == nil {
				t.Fatalf("config %s accepted, want reject", tc.config)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("error %q does not name %q", err, tc.errPart)
			}
		})
	}
}

// TestDecodePolicyGuardModeCandidates pins the I-J field-extension (design/02
// §4.1/§9.1 point 2a): the issue seed config decodes to flag + same-scope,
// and Set.GuardMode / Set.GuardSameScopeOnly resolve them. Before the
// extension the strict decoder rejected these keys as unknown (§4.1) — this
// is the positive counterpart to that reject.
func TestDecodePolicyGuardModeCandidates(t *testing.T) {
	cfg := `{"v":1,"guard":{"mode":"flag","candidates":"same-scope","threshold_duplicate":0.97,"threshold_review":0.90}}`
	p, err := DecodePolicy("issue", globalScope, false, false, []byte(cfg))
	if err != nil {
		t.Fatalf("DecodePolicy(issue): %v", err)
	}
	if p.Guard.Mode != GuardModeFlag {
		t.Errorf("guard.mode = %q, want flag", p.Guard.Mode)
	}
	if p.Guard.Candidates != GuardCandidatesSameScope {
		t.Errorf("guard.candidates = %q, want same-scope", p.Guard.Candidates)
	}

	// Resolve through a Set (needs a default type for NewSet).
	def, err := DecodePolicy("knowledge", globalScope, true, true, []byte(validBase))
	if err != nil {
		t.Fatalf("DecodePolicy(knowledge): %v", err)
	}
	set, err := NewSet([]Policy{def, p})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if got := set.GuardMode("issue"); got != GuardModeFlag {
		t.Errorf("Set.GuardMode(issue) = %q, want flag", got)
	}
	if !set.GuardSameScopeOnly("issue") {
		t.Error("Set.GuardSameScopeOnly(issue) = false, want true")
	}
	// Bestand type + unknown name fall back to archive / cross-scope.
	if got := set.GuardMode("knowledge"); got != GuardModeArchive {
		t.Errorf("Set.GuardMode(knowledge) = %q, want archive", got)
	}
	if set.GuardSameScopeOnly("knowledge") {
		t.Error("Set.GuardSameScopeOnly(knowledge) = true, want false (bestand)")
	}
	if got := set.GuardMode("does-not-exist"); got != GuardModeArchive {
		t.Errorf("Set.GuardMode(unknown) = %q, want archive fallback", got)
	}
	if set.GuardSameScopeOnly("does-not-exist") {
		t.Error("Set.GuardSameScopeOnly(unknown) = true, want false fallback")
	}
}

// TestDecodePolicyStructuralLinkClasses pins the I-C field extension (design/02
// §4.1): structural_link_classes is a NEW known top-level key (a bare []string,
// NOT dream.link_classes). Absent = nil (no restriction declared).
func TestDecodePolicyStructuralLinkClasses(t *testing.T) {
	p, err := DecodePolicy("issue", globalScope, false, false,
		[]byte(`{"v":1,"structural_link_classes":["references","duplicate-of"]}`))
	if err != nil {
		t.Fatalf("DecodePolicy: %v", err)
	}
	if !reflect.DeepEqual(p.StructuralLinkClasses, []string{"references", "duplicate-of"}) {
		t.Errorf("StructuralLinkClasses = %v, want [references duplicate-of]", p.StructuralLinkClasses)
	}
	q, err := DecodePolicy("plain", globalScope, false, false, []byte(validBase))
	if err != nil {
		t.Fatalf("DecodePolicy(plain): %v", err)
	}
	if q.StructuralLinkClasses != nil {
		t.Errorf("absent structural_link_classes = %v, want nil", q.StructuralLinkClasses)
	}
}

// TestIssueSeedConfigDecodes is the I-C positive probe (§7 gate): the issue seed
// config — carrying the requested extensions (guard.mode/candidates from I-J,
// workflow from I-B, structural_link_classes new in I-C) — decodes against the
// extended v1 schema. RED against a schema missing ANY of these keys
// (DisallowUnknownFields rejects the unknown key path) — that reject is the
// proof the extension was necessary.
func TestIssueSeedConfigDecodes(t *testing.T) {
	cfg := `{"v":1,"retrieval":{"policy":"full-pass"},` +
		`"guard":{"check":true,"candidate":true,"mode":"flag","candidates":"same-scope","threshold_duplicate":0.97,"threshold_review":0.90},` +
		`"dream":{"linkable":true},"digest":{"include":false},"overview":{"include":false},"parent":{"mode":"none"},` +
		`"workflow":{"states":["backlog","in-progress","done"],"initial":"backlog","terminal":["done"],"forge_state_map":{"open":"backlog","closed":"done"}},` +
		`"structural_link_classes":["references","duplicate-of"],"classify":{}}`
	p, err := DecodePolicy("issue", globalScope, true, false, []byte(cfg))
	if err != nil {
		t.Fatalf("issue seed config rejected by extended schema: %v", err)
	}
	if !reflect.DeepEqual(p.StructuralLinkClasses, []string{"references", "duplicate-of"}) {
		t.Errorf("structural_link_classes = %v", p.StructuralLinkClasses)
	}
	if len(p.Workflow.States) != 3 || p.Workflow.Initial != "backlog" {
		t.Errorf("workflow = %+v", p.Workflow)
	}
	if p.Guard.Mode != GuardModeFlag || p.Guard.Candidates != GuardCandidatesSameScope {
		t.Errorf("guard = %+v", p.Guard)
	}
	if p.Digest.Include || p.Overview.Include {
		t.Errorf("issue must be digest+overview excluded, got %+v %+v", p.Digest, p.Overview)
	}
}

// TestAggregateToParentFreischaltung pins the WF T11 validation freischaltung:
// the blanket pre-T11 reject of retrieval=aggregate-to-parent is gone. A config
// carrying it with a non-none parent.mode no longer fails on the aggregate value
// itself — any remaining error is the (foreign, I-D-owned) parent.mode gate.
// RED against the pre-T11 code (blanket "fold mechanism ships" reject fires
// first); GREEN in either merge order (T11 alone ⇒ parent.mode gate error; T11
// after I-D ⇒ nil). The assertion never depends on the parent.mode gate itself.
func TestAggregateToParentFreischaltung(t *testing.T) {
	_, err := DecodePolicy("agg-fixture", globalScope, false, false,
		[]byte(`{"v":1,"retrieval":{"policy":"aggregate-to-parent"},"parent":{"mode":"optional"}}`))
	if err != nil && strings.Contains(err.Error(), "fold mechanism") {
		t.Fatalf("aggregate-to-parent still blanket-rejected after T11: %v", err)
	}
}

// TestCommentSeedConfigInterim pins the INTERIM comment seed AND the fail-closed
// gate that "kein Gate aufweichen" preserves: the effective §4.1 values (all-off
// guard/dream/digest/overview) decode. Integrator note (T11 ∧ I-D merged): BOTH
// mechanisms now ship — T11 unlocked aggregate-to-parent, I-D unlocked
// parent.mode — so the §4.1 target combo (aggregate-to-parent +
// parent.mode=required/comment-of) now DECODES; the probe below is the positive
// proof that the seed FLIP (wave I-E) is unblocked. The comment SEED itself
// stays INTERIM (retrieval=excluded, parent.mode=none) until I-E flips it.
func TestCommentSeedConfigInterim(t *testing.T) {
	cfg := `{"v":1,"retrieval":{"policy":"excluded"},"guard":{"check":false,"candidate":false},` +
		`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},` +
		`"parent":{"mode":"none"},"classify":{}}`
	p, err := DecodePolicy("comment", globalScope, true, false, []byte(cfg))
	if err != nil {
		t.Fatalf("interim comment seed rejected: %v", err)
	}
	if p.Retrieval.Kind != RetrievalExcluded || p.Parent.Mode != ParentModeNone {
		t.Errorf("comment interim = %+v / %+v", p.Retrieval, p.Parent)
	}
	if p.Guard.Check || p.Guard.Candidate || p.Dream.Linkable || p.Digest.Include || p.Overview.Include {
		t.Errorf("comment must be out of every pipeline, got %+v", p)
	}
	// Integrator (T11 ∧ I-D): the §4.1 target config now DECODES — both gates
	// are unlocked with their mechanisms shipped. This positive probe proves the
	// I-E seed flip is unblocked; a regression re-introducing either blanket
	// reject turns it red.
	q, err := DecodePolicy("comment", globalScope, true, false,
		[]byte(`{"v":1,"retrieval":{"policy":"aggregate-to-parent"},"parent":{"mode":"required","relationship":"comment-of"}}`))
	if err != nil {
		t.Fatalf("§4.1 comment target (aggregate-to-parent + required) rejected after T11+I-D: %v", err)
	}
	if q.Retrieval.Kind != RetrievalAggregateToParent || q.Parent.Mode != ParentModeRequired || q.Parent.Relationship != "comment-of" {
		t.Errorf("§4.1 target decoded to %+v / %+v", q.Retrieval, q.Parent)
	}
}

// TestDecodePolicyParentModeUnlocked is the I-D positive probe: parent.mode
// optional|required + a comment-of relationship now DECODE (the parent_id write
// path ships in this wave). RED against the pre-I-D validator that rejected any
// non-"none" parent.mode ("not accepted before the parent_id write path ships").
func TestDecodePolicyParentModeUnlocked(t *testing.T) {
	for _, mode := range []string{ParentModeOptional, ParentModeRequired} {
		cfg := `{"v":1,"parent":{"mode":"` + mode + `","relationship":"comment-of"}}`
		p, err := DecodePolicy("comment", globalScope, false, false, []byte(cfg))
		if err != nil {
			t.Fatalf("parent.mode=%q rejected after I-D unlock: %v", mode, err)
		}
		if p.Parent.Mode != mode || p.Parent.Relationship != "comment-of" {
			t.Errorf("parent.mode=%q decoded to %+v", mode, p.Parent)
		}
	}
	// Resolve through a Set to exercise the accessor.
	def, err := DecodePolicy("knowledge", globalScope, true, true, []byte(validBase))
	if err != nil {
		t.Fatalf("DecodePolicy(knowledge): %v", err)
	}
	req, err := DecodePolicy("comment", globalScope, false, false, []byte(`{"v":1,"parent":{"mode":"required"}}`))
	if err != nil {
		t.Fatalf("DecodePolicy(comment required): %v", err)
	}
	set, err := NewSet([]Policy{def, req})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if got := set.ParentMode("comment"); got != ParentModeRequired {
		t.Errorf("Set.ParentMode(comment) = %q, want required", got)
	}
	if got := set.ParentMode("knowledge"); got != ParentModeNone {
		t.Errorf("Set.ParentMode(knowledge) = %q, want none (bestand)", got)
	}
	if got := set.ParentMode("does-not-exist"); got != ParentModeNone {
		t.Errorf("Set.ParentMode(unknown) = %q, want none fallback", got)
	}
}

func TestDecodePolicyNameFormat(t *testing.T) {
	for _, bad := range []string{"", "Upper", "-lead", "has space", "ümlaut", strings.Repeat("a", 51)} {
		if _, err := DecodePolicy(bad, globalScope, false, false, []byte(validBase)); err == nil {
			t.Errorf("name %q accepted, want format reject", bad)
		}
	}
	for _, good := range []string{"knowledge", "wf-issue", "a", "type-2"} {
		if _, err := DecodePolicy(good, globalScope, false, false, []byte(validBase)); err != nil {
			t.Errorf("name %q rejected: %v", good, err)
		}
	}
}

// TestDecodePolicyCaps pins the R1 resource caps (§3.3): config size, array
// entries, array item length.
func TestDecodePolicyCaps(t *testing.T) {
	big := `{"v":1,"description-pad":"` + strings.Repeat("x", maxConfigBytes) + `"}`
	if _, err := DecodePolicy("some-type", globalScope, false, false, []byte(big)); err == nil {
		t.Error("oversized config accepted, want reject")
	}

	entries := make([]string, maxArrayEntries+1)
	for i := range entries {
		entries[i] = fmt.Sprintf("%q", fmt.Sprintf("p%d", i))
	}
	tooMany := `{"v":1,"retrieval":{"policy":"full-pass","intent_patterns":[` + strings.Join(entries, ",") + `]}}`
	if _, err := DecodePolicy("some-type", globalScope, false, false, []byte(tooMany)); err == nil {
		t.Error("array with >64 entries accepted, want reject")
	}

	longItem := `{"v":1,"classify":{"title_patterns":["` + strings.Repeat("y", maxArrayItemLen+1) + `"]}}`
	if _, err := DecodePolicy("some-type", globalScope, false, false, []byte(longItem)); err == nil {
		t.Error("array item >128 chars accepted, want reject")
	}
}
