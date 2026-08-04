package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

// render() wire contract: every impact field is a JSON ARRAY, never null.
// blackedOut is append-built and stays nil in the NO-blackout normal case —
// marshalled as null it crashed the web ProfilesCard mid-flush and took the
// backends page down with it (loading-stuck / table vanished until F5).
func TestProfileImpactRenderNeverNull(t *testing.T) {
	cases := []struct {
		name string
		r    profileImpactResult
	}{
		{"all nil (inactive profile, no members visible)", profileImpactResult{}},
		{"blackout nil only (the live normal case)", profileImpactResult{
			backends:      []map[string]any{{"id": "b1"}},
			rolesAffected: []string{"chat"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.r.render())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), "null") {
				t.Fatalf("impact wire shape carries null: %s", raw)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, field := range []string{"backends", "roles_affected", "roles_blacked_out"} {
				if _, ok := m[field].([]any); !ok {
					t.Errorf("%s: not a JSON array — got %T (%v)", field, m[field], m[field])
				}
			}
		})
	}
}
