// Issue #406, tool surface: the MCP `update` tool exports `edits` and
// `metadata_merge`, driven over the real transport like mcp_update_type_test.go.
// The SDK validates the arguments against the exported schema before the
// handler runs, so the schema is part of the contract: an edit needs both old
// and new (an absent new must not mean "delete"), and a merge value may be
// null (the removal of a key). No database: a call that passes the schema
// reaches the handler, which refuses it for want of an identity.
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpUpdateCall posts one tools/call of the update tool with the given
// arguments and returns the raw response body.
func mcpUpdateCall(t *testing.T, arguments string) string {
	t.Helper()
	h := NewMCPHandler(MCPConfig{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update","arguments":`+arguments+`}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d (body %s)", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// reachedHandler is the handler's own first refusal of an unauthenticated call.
const reachedHandler = "unauthorized: no resolved tenant identity"

func TestMCPUpdateExportsEditsAndMerge(t *testing.T) {
	update, ok := mcpListedTools(t)["update"]
	if !ok {
		t.Fatalf("tools/list does not export \"update\"")
	}
	var edits struct {
		Type  json.RawMessage `json:"type"`
		Items struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"items"`
	}
	if err := json.Unmarshal(update.InputSchema.Properties["edits"], &edits); err != nil {
		t.Fatalf("edits schema: %v (%s)", err, update.InputSchema.Properties["edits"])
	}
	// The SDK renders an optional slice as ["null","array"].
	if !strings.Contains(string(edits.Type), `"array"`) {
		t.Fatalf("edits type = %s, want array", edits.Type)
	}
	for _, p := range []string{"old", "new", "replace_all"} {
		if _, ok := edits.Items.Properties[p]; !ok {
			t.Errorf("edit item has no %s property: %v", p, edits.Items.Properties)
		}
	}
	if strings.Join(edits.Items.Required, ",") != "old,new" {
		t.Errorf("edit item required = %v, want [old new]", edits.Items.Required)
	}
	if _, ok := update.InputSchema.Properties["metadata_merge"]; !ok {
		t.Fatalf("update input schema has no metadata_merge: %v", update.InputSchema.Properties)
	}
}

func TestMCPUpdateEditsSchemaAtCallTime(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
		reaches   bool
	}{
		{"an edit with old and new", `{"id":"x","edits":[{"old":"a","new":""}]}`, true},
		{"replace_all", `{"id":"x","edits":[{"old":"a","new":"b","replace_all":true}]}`, true},
		{"a merge with a null value", `{"id":"x","metadata_merge":{"drop":null,"set":{"n":1}}}`, true},
		{"an edit without new", `{"id":"x","edits":[{"old":"a"}]}`, false},
		{"an edit without old", `{"id":"x","edits":[{"new":"a"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := mcpUpdateCall(t, tc.arguments)
			if got := strings.Contains(body, reachedHandler); got != tc.reaches {
				t.Fatalf("reached the handler = %v, want %v (body %s)", got, tc.reaches, body)
			}
		})
	}
}
