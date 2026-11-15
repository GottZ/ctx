package handler

import (
	"encoding/json"

	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The answer forms of the MCP `search` tool (#206 paging).
//
// Before #206 the tool answered one JSON array of rows, and clients parse it as
// such (the Hermes ctx_checkpoint plugin json-loads the text; Hermes joins
// several text blocks with a newline, so a second block would break that parse).
// The array therefore stays exactly as it was for every call that sends neither
// new argument, and the two signals such a call can now carry ride BESIDE it in
// structuredContent: a lowered limit, and the cursor when more rows exist. A
// client that reads only the text sees the old bytes; one that reads
// structuredContent (Hermes hands it to the model next to the text) can page.
//
// A call that sends `after` asked for the new contract and gets one JSON
// object as its text: the signals FIRST, so a client that cuts a
// long answer still keeps the cursor, then the rows.

// mcpSearchAfterNeedsBrowseMsg refuses `after` next to a query. The full-text
// path answers top matches and never pages, so the cursor would be ignored and
// the caller would read the same matches as if they were the next page.
const mcpSearchAfterNeedsBrowseMsg = "after pages a listing with an empty query; a query answers its top matches without a cursor"

// mcpSearchSignals is what the tool tells a caller about the page beyond its
// rows. Limit is the effective page size; RequestedLimit is the caller's limit
// when the cap lowered it; NextAfter resumes after the last row when the
// listing goes on.
type mcpSearchSignals struct {
	NextAfter      *store.SearchCursor `json:"next_after,omitempty"`
	Limit          int                 `json:"limit"`
	RequestedLimit int                 `json:"requested_limit,omitempty"`
}

// mcpSearchAnswer is the page object of a call with `after`.
type mcpSearchAnswer struct {
	mcpSearchSignals
	Results []store.BlockPreview `json:"results"`
}

// renderMCPSearch turns one core answer into the tool result.
func renderMCPSearch(input searchInput, page searchPage) *mcp.CallToolResult {
	sig := mcpSearchSignals{Limit: page.Limit, NextAfter: page.NextAfter}
	if input.Limit > page.Limit {
		sig.RequestedLimit = input.Limit
	}
	rows := page.Rows

	if input.After != nil {
		data, _ := json.MarshalIndent(mcpSearchAnswer{mcpSearchSignals: sig, Results: rows}, "", "  ")
		return &mcp.CallToolResult{Content: []mcp.Content{textContent(string(data))}}
	}

	data, _ := json.MarshalIndent(rows, "", "  ")
	res := &mcp.CallToolResult{Content: []mcp.Content{textContent(string(data))}}
	if sig.RequestedLimit != 0 || sig.NextAfter != nil {
		res.StructuredContent = sig
	}
	return res
}
