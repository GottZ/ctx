package handler

import (
	"encoding/json"

	"github.com/GottZ/ctx/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The answer forms of the MCP `search` tool (#206 paging, #207 content).
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
// A call that sends `after` or `include_content` asked for the new contract and
// gets one JSON object as its text: the signals FIRST, so a client that cuts a
// long answer still keeps the cursor, then the rows.

// mcpSearchContentBudget bounds the summed `content` of one include_content
// answer, in bytes.
//
// Compiled-in rather than a settings key, for the reason the blob_fetch range
// bounds give (mcp_blob.go): it bounds how much block text enters one model
// context in one answer, a property of the tool surface rather than an operator
// policy. Without it one page could carry 50 blocks of 50 KiB, 2.5 MB. 64 KiB is
// the smallest binary size above the write gate's block cap (blockSizeLimit,
// 50 KiB), so every block written through a gated path fits a page on its own.
//
// The budget never cuts a block. A page ends before the first row that would
// cross it, `truncated` says so, and on the browse path next_after resumes at
// exactly that row. The first row of a page is always answered whole, even when
// it alone is larger (a block from before the size gate, or one an internal
// writer produced): a budget that skipped it would stop the listing for good,
// and one that cut it would hand on a prefix that reads like the whole text.
const mcpSearchContentBudget = 64 * 1024

// mcpSearchAfterNeedsBrowseMsg refuses `after` next to a query. The full-text
// path answers top matches and never pages, so the cursor would be ignored and
// the caller would read the same matches as if they were the next page.
const mcpSearchAfterNeedsBrowseMsg = "after pages a listing with an empty query; a query answers its top matches without a cursor"

// mcpSearchSignals is what the tool tells a caller about the page beyond its
// rows. Limit is the effective page size; RequestedLimit is the caller's limit
// when the cap lowered it; NextAfter resumes after the last row when the
// listing goes on; Truncated says that the content budget ended the page
// before `limit` rows (the rows present are whole).
type mcpSearchSignals struct {
	NextAfter      *store.SearchCursor `json:"next_after,omitempty"`
	Limit          int                 `json:"limit"`
	RequestedLimit int                 `json:"requested_limit,omitempty"`
	Truncated      bool                `json:"truncated,omitempty"`
}

// mcpSearchAnswer is the page object of a call with `after` or
// `include_content`.
type mcpSearchAnswer struct {
	mcpSearchSignals
	Results []store.BlockPreview `json:"results"`
}

// fitSearchContentBudget returns how many leading rows fit the budget: at least
// one (when there is one), then every row whose content still fits.
func fitSearchContentBudget(rows []store.BlockPreview, budget int) int {
	used := 0
	for i, r := range rows {
		if i > 0 && used+len(r.Content) > budget {
			return i
		}
		used += len(r.Content)
	}
	return len(rows)
}

// renderMCPSearch turns one core answer into the tool result.
func renderMCPSearch(input searchInput, page searchPage) *mcp.CallToolResult {
	sig := mcpSearchSignals{Limit: page.Limit, NextAfter: page.NextAfter}
	if input.Limit > page.Limit {
		sig.RequestedLimit = input.Limit
	}
	rows := page.Rows
	if input.IncludeContent {
		if n := fitSearchContentBudget(rows, mcpSearchContentBudget); n < len(rows) {
			rows = rows[:n]
			sig.Truncated = true
			if input.Query == "" {
				sig.NextAfter = searchCursorAfter(rows[n-1])
			}
		}
	}

	if input.After != nil || input.IncludeContent {
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
