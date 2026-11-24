//go:build integration

// Issue #519: manage update with only "metadata": null answered 500 with an
// ERROR line ("no fields to update") — a patch that names no field reached
// the store, whose statement has nothing to set. The sweep drives every
// optional field of an update alone as null, as an empty object and as an
// empty list through the production router, on manage update, the MCP update
// tool and an update item of a batch, and pins: no 500,
// no ERROR line, and a patch that names nothing refused as one (400
// missing_fields, nothing to update).
package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestUpdateEmptyPatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := updateBudgetFixture{nulSweepRouter(t, nil), t}
	k := f.key("up-patch", false)
	id := f.seed("up patch")
	fields := []string{"category", "title", "content", "tags", "metadata", "metadata_merge", "edits", "scope", "sensitivity", "type"}
	values := []struct {
		name string
		v    any
	}{{"null", nil}, {"{}", map[string]any{}}, {"[]", []any{}}}

	var table strings.Builder
	for _, fld := range fields {
		for _, val := range values {
			data := map[string]any{fld: val.v}
			label := fld + "=" + val.name
			// A patch whose only field is null names nothing.
			names := val.v != nil

			rest := f.restUpdate(k, id, data)
			mcp := f.mcpUpdate(k, id, data)
			batch := f.do(t, nulSweepCase{method: http.MethodPost, url: "/api/store/batch", cred: k,
				body: nulSweepJSON(map[string]any{"items": []any{map[string]any{"id": id, fld: val.v}}})}, false)
			fmt.Fprintf(&table, "| %s | %d %.120s | isError=%v code=%s %.120s | %d %.120s |\n", label,
				rest.status, rest.answer, mcp.isError, mcp.code, mcp.answer, batch.status, batch.answer)

			for _, r := range []struct {
				surface string
				res     nulSweepResult
			}{{"REST", rest}, {"MCP", mcp}, {"batch", batch}} {
				if r.res.status == http.StatusInternalServerError || strings.Contains(r.res.answer, "internal error") ||
					strings.Contains(r.res.answer, "Internal server error") {
					t.Errorf("%s %s: a server fault: %d %.200s", r.surface, label, r.res.status, r.res.answer)
				}
				for _, l := range r.res.lines {
					if strings.HasPrefix(l, "ERROR") {
						t.Errorf("%s %s: an ERROR line: %s", r.surface, label, l)
					}
				}
			}
			if !names {
				if rest.status != http.StatusBadRequest || rest.code != "missing_fields" || !strings.Contains(rest.answer, "nothing to update") {
					t.Errorf("REST %s: want 400 nothing to update, got %d %.200s", label, rest.status, rest.answer)
				}
				// The MCP tool refuses it as well: by its own check, or by its
				// input schema where that admits no null or no such field.
				if !mcp.isError || (!strings.Contains(mcp.answer, "nothing to update") && !strings.HasPrefix(mcp.answer, `validating "arguments"`)) {
					t.Errorf("MCP %s: want a refusal, got isError=%v %.200s", label, mcp.isError, mcp.answer)
				}
			}
		}
	}
	t.Logf("\n| patch | manage update | MCP update | batch update item |\n|---|---|---|---|\n%s", table.String())
}
