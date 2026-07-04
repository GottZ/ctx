// W12 field-name PARITY golden (design/03 §7-W12 gate "Feldnamen-Paritäts-Golden
// REST↔MCP"): the MCP issue tools must serialise the SAME wire field names as the
// frozen REST contract (web/src/lib/api/__fixtures__/issue-mutate.json /
// comment-create.json, masterplan K5). This test builds the issue/comment blocks
// EXACTLY as contract_freeze_golden_test.go does, runs them through the REAL MCP
// serializer (mcpIssueResult), and DeepEquals the result against the freeze files.
//
// RED on tag drift: rename the envelope key "issue"→x in mcpIssueResult, or rename
// a store.Block json tag (e.g. `type`→`type_name`), and this DeepEqual fails — the
// MCP path can no longer silently diverge from the REST freeze. Runs in -short (no
// DB): pure serialization over in-memory blocks.
package handler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpText extracts the concatenated text content of an MCP tool result.
func mcpText(res *mcp.CallToolResult) string {
	var out string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	return out
}

func TestMCPIssueToolsFreezeParity(t *testing.T) {
	dir := filepath.Join("..", "..", "web", "src", "lib", "api", "__fixtures__")
	ts := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	const (
		issueID   = "11111111-1111-1111-1111-111111111111"
		commentID = "22222222-2222-2222-2222-222222222222"
		scope     = "acme:main"
	)

	// Blocks identical to contract_freeze_golden_test.go (the freeze source).
	issueBlock := store.Block{
		ID: issueID, Category: "task", Tags: []string{"bug", "p1"},
		Title: "Example issue", Content: "# Example\n\nBody markdown.",
		Metadata:          map[string]any{"labels": []any{"bug", "p1"}},
		Scope:             scope,
		Sensitivity:       "internal",
		SensitivitySource: "manual",
		TypeName:          store.IssueTypeName,
		LifecycleState:    "active",
		TypeSource:        "manual",
		WorkflowStatus:    "open",
		CreatedAt:         created, UpdatedAt: ts,
	}
	commentBlock := store.Block{
		ID: commentID, Category: "comment", Tags: []string{},
		Title: "", Content: "A comment on the issue.",
		Metadata:          map[string]any{},
		Scope:             scope,
		Sensitivity:       "internal",
		SensitivitySource: "auto",
		TypeName:          store.CommentTypeName,
		LifecycleState:    "active",
		TypeSource:        "manual",
		CreatedAt:         created, UpdatedAt: ts,
	}

	cases := []struct {
		name    string
		key     string
		block   *store.Block
		fixture string
	}{
		{"issue_create/state ↔ issue-mutate.json", "issue", &issueBlock, "issue-mutate.json"},
		{"issue_comment ↔ comment-create.json", "comment", &commentBlock, "comment-create.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mcpIssueResult(c.key, c.block)
			if res.IsError {
				t.Fatalf("mcpIssueResult returned IsError: %s", mcpText(res))
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(mcpText(res)), &got); err != nil {
				t.Fatalf("unmarshal MCP result: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, c.fixture))
			if err != nil {
				t.Fatalf("read freeze %s: %v", c.fixture, err)
			}
			var want map[string]any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("unmarshal freeze %s: %v", c.fixture, err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("MCP↔REST field-name parity drift for %s\n--- MCP ---\n%s\n--- freeze ---\n%s", c.fixture, g, w)
			}
		})
	}
}
