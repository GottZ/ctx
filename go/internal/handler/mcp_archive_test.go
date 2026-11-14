// Issue #201, tool surface: the MCP server exports an `archive` tool (manage
// delete parity) and the `get` tool takes the opt-in `include_archived`.
//
// Driven over the real transport like mcp_blob_tools_test.go: a field or a tool
// that the Go source declares but the SDK does not export would pass a
// source-level assertion and fail every client. No database: tools/list runs no
// handler.
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mcpListedTool struct {
	Name        string `json:"name"`
	Annotations *struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
	InputSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"inputSchema"`
}

func mcpListedTools(t *testing.T) map[string]mcpListedTool {
	t.Helper()
	h := NewMCPHandler(MCPConfig{})
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []mcpListedTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/list: %v (body %s)", err, rec.Body.String())
	}
	out := make(map[string]mcpListedTool, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestMCPArchiveToolIsExported(t *testing.T) {
	tools := mcpListedTools(t)
	archive, ok := tools["archive"]
	if !ok {
		t.Fatalf("tools/list does not export \"archive\" — an MCP-only client cannot retract a block it wrote")
	}
	if archive.Annotations != nil && archive.Annotations.ReadOnlyHint {
		t.Errorf("archive is annotated read-only; it changes a block")
	}
	if _, ok := archive.InputSchema.Properties["id"]; !ok {
		t.Errorf("archive input schema has no id property: %v", archive.InputSchema.Properties)
	}
	if len(archive.InputSchema.Required) != 1 || archive.InputSchema.Required[0] != "id" {
		t.Errorf("archive required = %v, want [id]", archive.InputSchema.Required)
	}
}

func TestMCPGetTakesIncludeArchived(t *testing.T) {
	get, ok := mcpListedTools(t)["get"]
	if !ok {
		t.Fatalf("tools/list does not export \"get\"")
	}
	if _, ok := get.InputSchema.Properties["include_archived"]; !ok {
		t.Fatalf("get input schema has no include_archived property: %v", get.InputSchema.Properties)
	}
	// Opt-in: a client that never sends the field keeps the old call shape.
	for _, r := range get.InputSchema.Required {
		if r == "include_archived" {
			t.Fatalf("include_archived is required; it must be opt-in")
		}
	}
}
