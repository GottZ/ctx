//go:build integration

// `manage stats-reconcile` (issue #266) through the dispatch table against
// PG18: the server-admin tier, the payload contract, and a correction of
// drift the triggers could not see, reported in the answer and gone after it.
//
//	go test -tags=integration ./internal/handler/ -run TestStatsReconcileManage -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStatsReconcileManage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	const scope = "private"
	for _, title := range []string{"sr-a", "sr-b"} {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, "body "+title, nil, nil, scope, true,
			store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Drift: one insert the counters never see.
	for _, sql := range []string{
		`ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_ins`,
		`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'sr-hidden', 'x', 'private')`,
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_ins`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	admin := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: scope, ReadScopes: []string{scope}}
	member := &auth.AuthResult{IsValid: true, HomeScope: scope, ReadScopes: []string{scope}}
	call := func(ar *auth.AuthResult, data any) (int, map[string]any) {
		t.Helper()
		req := map[string]any{"action": "stats-reconcile"}
		if data != nil {
			req["data"] = data
		}
		raw, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
		r = r.WithContext(context.WithValue(r.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleManage(rec, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	total := func() int {
		s, err := store.GetStats(ctx, pool, []string{scope})
		if err != nil {
			t.Fatal(err)
		}
		return s.TotalBlocks
	}

	if code, _ := call(member, nil); code != http.StatusForbidden {
		t.Fatalf("non-admin: status %d, want 403", code)
	}
	if code, _ := call(admin, map[string]any{"scope": scope, "extra": 1}); code != http.StatusBadRequest {
		t.Fatalf("unknown field: status %d, want 400", code)
	}
	if got := total(); got != 2 {
		t.Fatalf("stats before reconcile = %d, want the drifted 2", got)
	}
	code, body := call(admin, map[string]any{"scope": scope})
	if code != http.StatusOK || body["success"] != true || body["scopes"] != float64(1) {
		t.Fatalf("reconcile one scope: %d %v", code, body)
	}
	corr, _ := body["corrections"].([]any)
	if len(corr) != 1 {
		t.Fatalf("corrections = %v, want one", body["corrections"])
	}
	c := corr[0].(map[string]any)
	if c["scope"] != scope || c["category"] != "learnings" || c["guard_status"] != "active" ||
		c["is_archived"] != false || c["counted"] != float64(2) || c["actual"] != float64(3) {
		t.Fatalf("correction = %v", c)
	}
	if got := total(); got != 3 {
		t.Fatalf("stats after reconcile = %d, want 3", got)
	}
	code, body = call(admin, map[string]any{})
	if code != http.StatusOK || body["success"] != true {
		t.Fatalf("reconcile all: %d %v", code, body)
	}
	if corr, _ := body["corrections"].([]any); len(corr) != 0 {
		t.Fatalf("second pass corrected %v, want nothing", corr)
	}
}
