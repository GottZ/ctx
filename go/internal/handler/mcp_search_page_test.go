package handler

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestFitSearchContentBudget: rows fit until the next would cross the budget;
// the first row always fits, and a block is never split.
func TestFitSearchContentBudget(t *testing.T) {
	row := func(n int) store.BlockPreview { return store.BlockPreview{Content: strings.Repeat("x", n)} }
	for _, c := range []struct {
		name  string
		sizes []int
		want  int
	}{
		{"empty", nil, 0},
		{"all fit", []int{10, 20, 30}, 3},
		{"exactly the budget", []int{50, 50}, 2},
		{"one byte over", []int{50, 51}, 1},
		{"oversized first row alone", []int{500, 1}, 1},
		{"oversized later row ends the page", []int{10, 500, 1}, 1},
	} {
		rows := make([]store.BlockPreview, 0, len(c.sizes))
		for _, n := range c.sizes {
			rows = append(rows, row(n))
		}
		if got := fitSearchContentBudget(rows, 100); got != c.want {
			t.Errorf("%s: fit = %d, want %d", c.name, got, c.want)
		}
	}
	if mcpSearchContentBudget < 50*1024 {
		t.Errorf("budget %d is below the write gate's block cap (50 KiB): a gated block would not fit a page alone", mcpSearchContentBudget)
	}
}

// TestRenderMCPSearchForms pins the two answer forms without a database.
func TestRenderMCPSearchForms(t *testing.T) {
	when := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	rows := []store.BlockPreview{
		{ID: "a", Title: "A", Content: strings.Repeat("a", 40*1024), UpdatedAt: when},
		{ID: "b", Title: "B", Content: strings.Repeat("b", 40*1024), UpdatedAt: when.Add(-time.Second)},
	}
	next := &store.SearchCursor{UpdatedAt: when.Add(-time.Second), ID: "b"}

	t.Run("legacy_without_signals_is_the_array_alone", func(t *testing.T) {
		res := renderMCPSearch(searchInput{Limit: 10}, searchPage{Rows: rows, Limit: 10})
		want, _ := json.MarshalIndent(rows, "", "  ")
		if len(res.Content) != 1 || mcpTextOf(res) != string(want) || res.StructuredContent != nil {
			t.Errorf("legacy answer = %d blocks, structured %#v; want the array alone", len(res.Content), res.StructuredContent)
		}
	})

	t.Run("legacy_signals_ride_beside_the_array", func(t *testing.T) {
		res := renderMCPSearch(searchInput{Limit: 200}, searchPage{Rows: rows, Limit: 50, NextAfter: next})
		want, _ := json.MarshalIndent(rows, "", "  ")
		if mcpTextOf(res) != string(want) {
			t.Error("legacy text changed")
		}
		sig, ok := res.StructuredContent.(mcpSearchSignals)
		if !ok || sig.Limit != 50 || sig.RequestedLimit != 200 || sig.NextAfter != next {
			t.Errorf("structured = %#v; want limit 50, requested_limit 200 and the cursor", res.StructuredContent)
		}
	})

	t.Run("content_budget_ends_the_browse_page_at_a_row", func(t *testing.T) {
		res := renderMCPSearch(searchInput{IncludeContent: true}, searchPage{Rows: rows, Limit: 10})
		var got struct {
			NextAfter *store.SearchCursor `json:"next_after"`
			Truncated bool                `json:"truncated"`
			Results   []store.BlockPreview `json:"results"`
		}
		if err := json.Unmarshal([]byte(mcpTextOf(res)), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Results) != 1 || !got.Truncated || got.NextAfter == nil || got.NextAfter.ID != "a" {
			t.Errorf("page = %d rows, truncated %t, next_after %v; want 1, true, after row a", len(got.Results), got.Truncated, got.NextAfter)
		}
		if res.StructuredContent != nil {
			t.Error("the page object carries its signals in the text; no structuredContent")
		}
		if !strings.HasPrefix(mcpTextOf(res), "{\n  \"next_after\"") {
			t.Errorf("the cursor must lead the object (survives a client cutting a long answer): %.60s", mcpTextOf(res))
		}
	})

	t.Run("content_budget_on_a_query_has_no_cursor", func(t *testing.T) {
		res := renderMCPSearch(searchInput{Query: "q", IncludeContent: true}, searchPage{Rows: rows, Limit: 10})
		var got mcpSearchAnswer
		if err := json.Unmarshal([]byte(mcpTextOf(res)), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Results) != 1 || !got.Truncated || got.NextAfter != nil {
			t.Errorf("query page = %d rows, truncated %t, next_after %v; want 1, true, none", len(got.Results), got.Truncated, got.NextAfter)
		}
	})
}

// TestMCPSearchSchemaAdvertisesPaging reads tools/list over the real transport:
// `after` is an optional object whose two fields are optional (so `{}` is a
// valid first page), include_content an optional boolean, and the limit names
// its cap.
func TestMCPSearchSchemaAdvertisesPaging(t *testing.T) {
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Properties map[string]struct {
						Type        any            `json:"type"`
						Description string         `json:"description"`
						Properties  map[string]any `json:"properties"`
						Required    []string       `json:"required"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpWireCall(t, MCPConfig{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	for _, tool := range list.Result.Tools {
		if tool.Name != "search" {
			continue
		}
		props := tool.InputSchema.Properties
		after, ok := props["after"]
		if !ok || len(after.Required) != 0 || after.Properties["after_updated"] == nil || after.Properties["after_id"] == nil {
			t.Errorf("after = %+v; want an object with optional after_updated and after_id", after)
		}
		if ic, ok := props["include_content"]; !ok || ic.Type != "boolean" {
			t.Errorf("include_content = %+v; want an optional boolean", ic)
		}
		if !strings.Contains(props["limit"].Description, "max 50") || !strings.Contains(tool.Description, "50") {
			t.Errorf("the cap is not named: limit %q, tool %q", props["limit"].Description, tool.Description)
		}
		if !reflect.DeepEqual(tool.InputSchema.Required, []string(nil)) {
			t.Errorf("required = %v, want none", tool.InputSchema.Required)
		}
		return
	}
	t.Fatal("tools/list has no search tool")
}
