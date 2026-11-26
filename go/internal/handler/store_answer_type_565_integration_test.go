//go:build integration

// Issue #565, part 3: the answer of a write names the type the block is
// stored as. The classify hook runs after the write committed and files the
// block by its title and metadata; POST /api/store, POST /api/store/batch and
// manage update answer with the block the write returned, and until #565 that
// block still carried the type it had before the hook — "knowledge" for a
// block stored as audit-trail.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestStoreAnswerType565 -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStoreAnswerType565(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	rowType := func(t *testing.T, id string) (name, source string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&name, &source); err != nil {
			t.Fatalf("read row %s: %v", id, err)
		}
		return name, source
	}
	type answered struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		TypeSource string `json:"type_source"`
	}
	store := func(t *testing.T, k sbKey, body map[string]any) answered {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(raw)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewStoreHandler(f.pool, f.cfg, f.reg).HandleStore(rec, req)
		var resp struct {
			Block answered `json:"block"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
			t.Fatalf("store: %d %s", rec.Code, rec.Body.String())
		}
		return resp.Block
	}
	check := func(t *testing.T, got answered, wantType, wantSource string) {
		t.Helper()
		name, source := rowType(t, got.ID)
		if name != wantType || source != wantSource {
			t.Fatalf("row = (%s, %s), want (%s, %s) — fixture premise", name, source, wantType, wantSource)
		}
		if got.Type != name || got.TypeSource != source {
			t.Fatalf("answer names (%s, %s), the row is (%s, %s)", got.Type, got.TypeSource, name, source)
		}
	}

	t.Run("POST /api/store", func(t *testing.T) {
		k := f.key(t, false)
		// The issue's write: an agent-briefing digest title the audit-trail
		// rule "session" files.
		check(t, store(t, k, map[string]any{
			"category": "agent-briefing", "title": "Session digest rollprebb seg 002 2026-11-26", "content": "digest body",
		}), "audit-trail", "auto")
		check(t, store(t, k, map[string]any{
			"category": "notes", "title": "Plain note t565", "content": "nothing to classify",
		}), "knowledge", "auto")
		check(t, store(t, k, map[string]any{
			"category": "notes", "title": "Session notes named t565", "content": "x", "type": "reference",
		}), "reference", "manual")
	})

	t.Run("POST /api/store/batch", func(t *testing.T) {
		k := f.key(t, false)
		code, m := f.rest(t, k, map[string]any{"items": []any{
			map[string]any{"category": "agent-briefing", "title": "Session digest t565 seg 001", "content": "a"},
			map[string]any{"category": "notes", "title": "Plain batch note t565", "content": "b"},
		}})
		if code != http.StatusOK {
			t.Fatalf("batch: %d %v", code, m)
		}
		items, _ := m["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("batch items = %v", m)
		}
		for i, want := range []string{"audit-trail", "knowledge"} {
			raw, _ := json.Marshal(items[i].(map[string]any)["block"])
			var got answered
			_ = json.Unmarshal(raw, &got)
			check(t, got, want, "auto")
		}
	})

	t.Run("manage update", func(t *testing.T) {
		k := f.key(t, false)
		plain := store(t, k, map[string]any{"category": "notes", "title": "Plain then renamed t565", "content": "c"})
		check(t, plain, "knowledge", "auto")
		body, _ := json.Marshal(map[string]any{"action": "update", "id": plain.ID,
			"data": map[string]any{"title": "Session handover renamed t565"}})
		req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewManageHandler(f.pool, f.cfg, nil, nil, nil, nil, nil, f.reg).HandleManage(rec, req)
		var resp struct {
			Block answered `json:"block"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
			t.Fatalf("manage update: %d %s", rec.Code, rec.Body.String())
		}
		check(t, resp.Block, "audit-trail", "auto")
	})
}
