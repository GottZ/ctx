//go:build integration

// Issue #271: `manage stats-reconcile` answered every correction of the pass
// in one response. A correction is one drifted counter key, and after a
// restore into an existing database (docs/operations.md: every restored block
// counted twice) every key of the store drifts — at the 1M–10M-block target
// hundreds of thousands of them, an answer of tens of megabytes that the
// CLI's 10 MB read cap cuts into invalid JSON. The answer now carries at most
// statsReconcileMaxCorrections corrections and the pass's total; every
// correction is still applied and logged.
//
//	go test -tags=integration ./internal/handler/ -run TestStatsReconcileManageCorrectionsBounded -count=1 -v
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

func TestStatsReconcileManageCorrectionsBounded(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	const scope = "private"
	drifted := statsReconcileMaxCorrections + 25
	// Drift on drifted keys: counter rows for categories that hold no block.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_count_deltas (scope, category, is_archived, guard_status, n)
		 SELECT $1, 'rc-' || g, false, NULL, 1 FROM generate_series(1, $2::int) g`, scope, drifted); err != nil {
		t.Fatalf("drift: %v", err)
	}

	admin := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: scope, ReadScopes: []string{scope}}
	call := func() (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"action": "stats-reconcile", "data": map[string]any{"scope": scope}})
		r := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
		r = r.WithContext(context.WithValue(r.Context(), authResultKey, admin))
		rec := httptest.NewRecorder()
		h.HandleManage(rec, r)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return rec.Code, body
	}

	code, body := call()
	if code != http.StatusOK || body["success"] != true {
		t.Fatalf("reconcile: %d %v", code, body["error"])
	}
	corr, _ := body["corrections"].([]any)
	if len(corr) != statsReconcileMaxCorrections {
		t.Errorf("the answer carries %d corrections, want the bound %d", len(corr), statsReconcileMaxCorrections)
	}
	if body["corrections_total"] != float64(drifted) {
		t.Errorf("corrections_total = %v, want %d", body["corrections_total"], drifted)
	}
	// Every correction was applied, not only the answered ones.
	cats, err := store.ListCategories(ctx, pool, []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if len(c.Category) > 3 && c.Category[:3] == "rc-" {
			t.Errorf("category %s still counts %d after the reconcile", c.Category, c.Count)
		}
	}
	code, body = call()
	if corr, _ := body["corrections"].([]any); code != http.StatusOK || len(corr) != 0 || body["corrections_total"] != float64(0) {
		t.Errorf("second pass: %d corrections %d total %v, want none", code, len(corr), body["corrections_total"])
	}
}
