package handler

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestRenderMCPSearchForms pins the two answer forms without a database.
func TestRenderMCPSearchForms(t *testing.T) {
	when := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	rows := []store.BlockPreview{
		{ID: "a", Title: "A", UpdatedAt: when},
		{ID: "b", Title: "B", UpdatedAt: when.Add(-time.Second)},
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

}

// TestMCPSearchSchemaAdvertisesPaging reads tools/list over the real transport:
// `after` is an optional object whose two fields are optional (so `{}` is a
// valid first page), and the limit names its cap.
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
