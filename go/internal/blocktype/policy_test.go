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
