// W02-8 gate 1: the MCP tool list carries the blob tools — the two write/read
// tools of W02-8 plus blob_link, phase 2 of the two-phase write (W02-10).
//
// RED before this wave: registerTools registered twelve tools (query, store,
// search, get, recent, update, confirm + the three issue tools + the two guard
// tools) and no blob tool at all — a provider could write blocks over MCP but
// had to fall back to the REST route for a payload, i.e. to a second
// credential and a second authorisation path.
//
// The probe drives the REAL transport (NewMCPHandler → Streamable HTTP,
// stateless + JSON response, exactly what test.sh T17 curls) rather than
// reading the registration source: a tool that is registered but not exported
// by the transport would pass a source-level assertion and fail every client.
//
// No database: registerTools never touches the pool, and tools/list runs no
// handler. That keeps the tool-surface gate in the -short suite, where a
// dropped registration shows up in seconds.
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpToolNames drives one tools/list call over the real transport and returns
// the exported tool names.
func mcpToolNames(t *testing.T) []string {
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
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/list: %v (body %s)", err, rec.Body.String())
	}
	names := make([]string, 0, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestMCPToolsListCarriesBlobTools(t *testing.T) {
	names := mcpToolNames(t)

	for _, want := range []string{"blob_store", "blob_fetch", "blob_link"} {
		found := false
		for _, got := range names {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tools/list does not export %q — tools = %v", want, names)
		}
	}

	// The count is pinned in test.sh T17 against the live server. Pinning it
	// here too is what makes the two agree: a tool added without touching T17
	// fails at go test time instead of on a deployed system.
	if len(names) != 15 {
		t.Errorf("tools/list exports %d tools, want 15 (test.sh T17 pins the same number) — tools = %v",
			len(names), names)
	}
}
