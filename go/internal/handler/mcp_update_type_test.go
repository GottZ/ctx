// Issue #203, tool surface: the MCP `update` tool exports an optional `type`
// beside its existing fields. Driven over the real transport (mcpListedTools,
// mcp_archive_test.go): a field the Go struct declares but the SDK does not
// export would pass a source-level assertion and fail every client.
package handler

import (
	"slices"
	"testing"
)

func TestMCPUpdateTakesOptionalType(t *testing.T) {
	update, ok := mcpListedTools(t)["update"]
	if !ok {
		t.Fatalf("tools/list does not export \"update\"")
	}
	if _, ok := update.InputSchema.Properties["type"]; !ok {
		t.Fatalf("update input schema has no type property: %v", update.InputSchema.Properties)
	}
	// Every field the tool had stays, and only id is required: a client that
	// never sends type keeps its call shape.
	got := make([]string, 0, len(update.InputSchema.Properties))
	for name := range update.InputSchema.Properties {
		got = append(got, name)
	}
	slices.Sort(got)
	if want := []string{"category", "content", "edits", "id", "metadata", "metadata_merge", "tags", "title", "type"}; !slices.Equal(got, want) {
		t.Errorf("update properties = %v, want %v", got, want)
	}
	if !slices.Equal(update.InputSchema.Required, []string{"id"}) {
		t.Errorf("update required = %v, want [id]", update.InputSchema.Required)
	}
}
