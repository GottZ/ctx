package blocktype

import (
	"strings"
	"testing"
)

// TestWorkflowForgeMapViolation pins the write rule of issue #491: the forge's
// closed state maps to a terminal status, its open state to one that is not.
func TestWorkflowForgeMapViolation(t *testing.T) {
	wf := func(terminal []string, m map[string]string) Policy {
		return Policy{Name: "issue", Workflow: WorkflowPolicy{
			States: []string{"backlog", "review", "done", "wontfix"}, Initial: "backlog", Terminal: terminal, ForgeStateMap: m,
		}}
	}
	for _, tc := range []struct {
		name string
		p    Policy
		want string // substring of the violation, "" = admissible
	}{
		{"builtin shape", wf([]string{"done"}, map[string]string{"open": "backlog", "closed": "done"}), ""},
		{"closed to a second terminal status", wf([]string{"done", "wontfix"}, map[string]string{"open": "review", "closed": "wontfix"}), ""},
		{"closed only", wf([]string{"done"}, map[string]string{"closed": "done"}), ""},
		{"no map", wf([]string{"done"}, nil), ""},
		{"no workflow", Policy{Name: "knowledge"}, ""},
		{"closed to a status that is not terminal", wf([]string{"done"}, map[string]string{"open": "backlog", "closed": "review"}), `forge_state_map["closed"]="review" is not in workflow.terminal`},
		{"closed without a terminal set", wf(nil, map[string]string{"closed": "done"}), `forge_state_map["closed"]="done" is not in workflow.terminal`},
		{"open to a terminal status", wf([]string{"done", "wontfix"}, map[string]string{"open": "wontfix", "closed": "done"}), `forge_state_map["open"]="wontfix" is in workflow.terminal`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WorkflowForgeMapViolation(tc.p)
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("violation = %q, want %q", got, tc.want)
			}
		})
	}
	for _, p := range builtinPolicies() {
		if msg := WorkflowForgeMapViolation(p); msg != "" {
			t.Errorf("builtin %s violates the rule: %s", p.Name, msg)
		}
	}
}
