package blocktype

import (
	"fmt"
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
		{"aggregate-to-parent before T11", `{"v":1,"retrieval":{"policy":"aggregate-to-parent"}}`, "aggregate-to-parent"},
		{"parent.mode optional before Achse 02", `{"v":1,"parent":{"mode":"optional"}}`, "parent.mode"},
		{"parent.mode required before Achse 02", `{"v":1,"parent":{"mode":"required"}}`, "parent.mode"},
		{"unknown parent.mode", `{"v":1,"parent":{"mode":"maybe"}}`, "parent.mode"},
		{"threshold out of range", `{"v":1,"guard":{"threshold_duplicate":1.2}}`, "threshold_duplicate"},
		{"unknown guard.mode", `{"v":1,"guard":{"mode":"delete"}}`, "guard.mode"},
		{"unknown guard.candidates", `{"v":1,"guard":{"candidates":"tenant"}}`, "guard.candidates"},
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
