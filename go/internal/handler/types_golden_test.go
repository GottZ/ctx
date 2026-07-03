package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestTypesGoldenShape pins the /api/types wire shape (masterplan §3 K5:
// 01-§3.3 is the field truth, the registry is the source; 03/04 follow. Freeze
// = this Go golden + web/src/lib/api/types.ts, drift ⇒ rot). A renamed or
// dropped field breaks the byte-exact `want` here AND forces a deliberate
// update on the TS mirror — that is the whole point of the freeze. The config
// envelope is carried verbatim (json.RawMessage), so this golden also documents
// the full 01-§3.3 config vocabulary the UI must be able to render.
func TestTypesGoldenShape(t *testing.T) {
	updatedBy := "22222222-2222-2222-2222-222222222222"
	ts := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	config := json.RawMessage(`{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":true,"candidate":true},"dream":{"linkable":true},"digest":{"include":true},"overview":{"include":true},"parent":{"mode":"none"},"classify":{"priority":100}}`)

	view := typeView{
		BlockTypeRow: store.BlockTypeRow{
			ID:          "11111111-1111-1111-1111-111111111111",
			Name:        "issue",
			Scope:       store.GlobalScope,
			DisplayName: "Issue",
			Description: "A tracked work item",
			Builtin:     true,
			IsDefault:   false,
			Config:      config,
			CreatedAt:   ts,
			UpdatedAt:   ts,
			UpdatedBy:   &updatedBy,
		},
		Source: "builtin",
	}

	got, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"id":"11111111-1111-1111-1111-111111111111","name":"issue","scope":"_global","display_name":"Issue","description":"A tracked work item","builtin":true,"is_default":false,"config":{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":true,"candidate":true},"dream":{"linkable":true},"digest":{"include":true},"overview":{"include":true},"parent":{"mode":"none"},"classify":{"priority":100}},"created_at":"2026-07-03T00:00:00Z","updated_at":"2026-07-03T00:00:00Z","updated_by":"22222222-2222-2222-2222-222222222222","source":"builtin"}`
	if string(got) != want {
		t.Fatalf("types wire shape drift (K5 freeze — update web/src/lib/api/types.ts in lockstep):\n got: %s\nwant: %s", got, want)
	}
}

// TestResolveEffectiveTypes pins the effective-list resolution: a tenant-scoped
// row SHADOWS the '_global' row of the same name (Achse-01 resolver order), the
// source badge is scope-derived, and the output is name-sorted and never nil.
func TestResolveEffectiveTypes(t *testing.T) {
	rows := []store.BlockTypeRow{
		{Name: "knowledge", Scope: store.GlobalScope, Builtin: true},
		{Name: "issue", Scope: store.GlobalScope, Builtin: true},
		{Name: "issue", Scope: "tenant-a", Builtin: false}, // shadows _global issue
		{Name: "custom", Scope: "tenant-a", Builtin: false},
	}
	got := resolveEffectiveTypes(rows)

	if len(got) != 3 {
		t.Fatalf("effective list len = %d, want 3 (issue de-duplicated): %+v", len(got), got)
	}
	// Name-sorted: custom, issue, knowledge.
	if got[0].Name != "custom" || got[1].Name != "issue" || got[2].Name != "knowledge" {
		t.Fatalf("not name-sorted: %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
	// issue must be the tenant row (shadowing) with source=tenant.
	if got[1].Scope != "tenant-a" || got[1].Source != "tenant" {
		t.Errorf("issue = scope %q source %q, want tenant-a/tenant (resolver shadow order)", got[1].Scope, got[1].Source)
	}
	// knowledge is a _global row ⇒ source=builtin.
	if got[2].Source != "builtin" {
		t.Errorf("knowledge source = %q, want builtin (_global namespace)", got[2].Source)
	}
}

// TestResolveEffectiveTypes_EmptyIsArray guards the wire invariant: an empty
// registry serializes as [], never null.
func TestResolveEffectiveTypes_EmptyIsArray(t *testing.T) {
	got := resolveEffectiveTypes(nil)
	b, err := json.Marshal(map[string]any{"types": got})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"types":[]}` {
		t.Fatalf("empty types = %s, want {\"types\":[]}", b)
	}
}
