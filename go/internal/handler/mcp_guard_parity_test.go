package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// Guard W3 parity pins (the mcp_issues_parity_test line, DB-free): the MCP
// guard tools serialize the SAME store structs as the REST manage actions, so
// the wire field names cannot drift between the two surfaces. An add/remove/
// rename on GuardListItem or GuardSkip is a deliberate contract change that
// must update this pin and the REST consumers together.

func keysOf(t *testing.T, raw []byte) map[string]bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func assertExactKeys(t *testing.T, label string, raw []byte, want []string) {
	t.Helper()
	got := keysOf(t, raw)
	for _, k := range want {
		if !got[k] {
			t.Errorf("%s: missing key %q", label, k)
		}
		delete(got, k)
	}
	for k := range got {
		t.Errorf("%s: unexpected key %q", label, k)
	}
}

func TestMCPGuardListItemParity(t *testing.T) {
	sim := "0.9412"
	matchedID := "019f0000-0000-7000-8000-000000000002"
	matchedTitle := "the canonical block"
	checked := "2026-07-20T10:00:00Z"
	item := store.GuardListItem{
		ID: "019f0000-0000-7000-8000-000000000001", Title: "t", Category: "learnings",
		Scope: "private", Type: "knowledge", GuardStatus: "needs_review",
		Similarity: &sim, MatchedID: &matchedID, MatchedTitle: &matchedTitle,
		CheckedAt: &checked, UpdatedAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
	}
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertExactKeys(t, "guard-list item", b, []string{
		"id", "title", "category", "scope", "type_name", "guard_status",
		"similarity", "matched_id", "matched_title", "checked_at", "updated_at",
	})
}

func TestMCPGuardSkipParity(t *testing.T) {
	b, err := json.Marshal(store.GuardSkip{ID: "x", Reason: "not_found"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertExactKeys(t, "guard-resolve skip", b, []string{"id", "reason"})
}
