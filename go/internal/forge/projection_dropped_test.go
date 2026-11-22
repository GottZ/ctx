package forge

import (
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// TestCtxIssueBaseDroppedStatus pins issueTypePolicy.ctxIssueBase (issue
// #491): a status the resolved workflow carries projects by terminal-set
// membership; one it does not carry projects the state of the base snapshot,
// then metadata.forge_state, then the terminal-set rule.
func TestCtxIssueBaseDroppedStatus(t *testing.T) {
	pol := issueTypePolicy{registryOK: true, states: []string{"triage", "done"}, terminalSet: []string{"done"}}
	closedBase, _ := issueProjectionJSON(issueProjection{Title: "Crane", State: "closed"})
	openBase, _ := issueProjectionJSON(issueProjection{Title: "Crane", State: "open"})
	state := func(t *testing.T, pol issueTypePolicy, status string, meta map[string]any, base []byte) string {
		t.Helper()
		fields, _ := pol.ctxIssueBase(&store.Block{Title: "#5 Crane", WorkflowStatus: status, Metadata: meta}, base)
		p, ok := parseIssueProjection(fields)
		if !ok {
			t.Fatalf("unparsable projection %s", fields)
		}
		return p.State
	}
	for _, tc := range []struct {
		name   string
		pol    issueTypePolicy
		status string
		meta   map[string]any
		base   []byte
		want   string
	}{
		{"dropped status, closed base", pol, "wontfix", map[string]any{"forge_state": "open"}, closedBase, "closed"},
		{"dropped status, open base", pol, "wontfix", nil, openBase, "open"},
		{"dropped status, no snapshot, forge_state", pol, "wontfix", map[string]any{"forge_state": "closed"}, nil, "closed"},
		{"dropped status, nothing to read", pol, "wontfix", nil, nil, "open"},
		{"configured terminal status, open base", pol, "done", nil, openBase, "closed"},
		{"configured open status, closed base", pol, "triage", nil, closedBase, "open"},
		{"no status, forge_state", pol, "", map[string]any{"forge_state": "closed"}, openBase, "closed"},
		{"no workflow, forge_state", issueTypePolicy{}, "wontfix", map[string]any{"forge_state": "open"}, closedBase, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := state(t, tc.pol, tc.status, tc.meta, tc.base); got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}
