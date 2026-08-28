package dream

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// ctSet builds a policy snapshot through the production constructor — never a
// hand-rolled Set — so the derived lists dreamCandidateTypes reads (visible,
// dreamLinkable) come from the same code path production boots.
func ctSet(t *testing.T, policies ...blocktype.Policy) *blocktype.Set {
	t.Helper()
	s, err := blocktype.NewSet(policies)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return s
}

func ctPolicy(name, retrieval string, linkable bool) blocktype.Policy {
	return blocktype.Policy{
		Name:      name,
		Scope:     "_global",
		IsDefault: name == "knowledge",
		Retrieval: blocktype.RetrievalPolicy{Kind: retrieval, DampingFactor: 0.6},
		Dream:     blocktype.DreamPolicy{Linkable: linkable},
	}
}

// TestDreamCandidateTypes_BehaviourMatchesContract pins the BA15 allowlist:
// exactly the types that are BOTH retrievable and dream-linkable, in
// VisibleTypes order. Each row names the class of type it separates.
func TestDreamCandidateTypes_BehaviourMatchesContract(t *testing.T) {
	tests := []struct {
		name     string
		policies []blocktype.Policy
		want     []string
	}{
		{
			name: "damped non-linkable type is out (BA15: insight after E-4)",
			policies: []blocktype.Policy{
				ctPolicy("knowledge", blocktype.RetrievalFullPass, true),
				ctPolicy("insight", blocktype.RetrievalDamped, false),
			},
			want: []string{"knowledge"},
		},
		{
			name: "damped LINKABLE type stays in (audit-trail keeps factor 1.0 here)",
			policies: []blocktype.Policy{
				ctPolicy("audit-trail", blocktype.RetrievalDamped, true),
				ctPolicy("knowledge", blocktype.RetrievalFullPass, true),
			},
			want: []string{"audit-trail", "knowledge"},
		},
		{
			name: "aggregate-to-parent non-linkable type is out (comment)",
			policies: []blocktype.Policy{
				ctPolicy("comment", blocktype.RetrievalAggregateToParent, false),
				ctPolicy("knowledge", blocktype.RetrievalFullPass, true),
			},
			want: []string{"knowledge"},
		},
		{
			name: "excluded LINKABLE type is out — linkable does not make it retrievable",
			policies: []blocktype.Policy{
				ctPolicy("knowledge", blocktype.RetrievalFullPass, true),
				ctPolicy("system-meta", blocktype.RetrievalExcluded, true),
			},
			want: []string{"knowledge"},
		},
		{
			name: "disjoint policy: empty allowlist, and the caller must refuse to search",
			policies: []blocktype.Policy{
				ctPolicy("knowledge", blocktype.RetrievalExcluded, true),
				ctPolicy("tool-evidence", blocktype.RetrievalDamped, false),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dreamCandidateTypes(ctSet(t, tt.policies...))
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("dreamCandidateTypes() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDreamCandidateTypes_BuiltinSet_BehaviourMatchesContract pins the delta of
// V-9 against the COMPILED-IN fallback set — the registry generation a degraded
// boot serves. audit-trail/issue/knowledge/reference survive; comment,
// tool-evidence and tool-overview leave the candidate search. They are the
// three retrievable-but-not-linkable types of the shipped registry, and each
// carried 0 active blocks on the live corpus when this test was written, so the
// wave's behavioural delta on the current corpus is zero blocks — the change
// closes a channel, it does not move today's rankings.
func TestDreamCandidateTypes_BuiltinSet_BehaviourMatchesContract(t *testing.T) {
	// An un-booted registry serves exactly the compiled-in fallback set
	// (NewRegistry stores builtinSet in base; Snapshot returns base).
	set := blocktype.NewRegistry().Snapshot()
	got := dreamCandidateTypes(set)
	want := set.DreamLinkableTypes()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dreamCandidateTypes(builtin) = %v, want %v (every builtin dream-linkable type is retrievable)", got, want)
	}
	dropped := map[string]bool{}
	for _, n := range set.VisibleTypes() {
		dropped[n] = true
	}
	for _, n := range got {
		delete(dropped, n)
	}
	names := make([]string, 0, len(dropped))
	for n := range dropped {
		names = append(names, n)
	}
	if len(names) != 3 {
		t.Errorf("visible-but-not-candidate types = %v, want the three known ones (comment, tool-evidence, tool-overview)", names)
	}
	for _, n := range []string{"comment", "tool-evidence", "tool-overview"} {
		if !dropped[n] {
			t.Errorf("%s must leave the dream candidate allowlist", n)
		}
	}
}
