//go:build integration

// WF T10 wire gates on /api/search: the types/types_exclude request fields,
// the block_roles_exclude legacy alias (identical effect; both names ⇒
// union), and the response golden shape (type/lifecycle_state/type_source on
// every result row — the FE drift anchor for types.ts).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSearchTypeFilter -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSearchTypeFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, fx := range []struct{ title, typeName string }{
		{"WT Knowledge", "knowledge"},
		{"WT Audit", "audit-trail"},
		{"WT Reference", "reference"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ('wiretest', $1, 'wire fixture', 'private', $2)`, fx.title, fx.typeName); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	ar := &auth.AuthResult{IsValid: true, ApiKeyID: "00000000-0000-7000-8000-000000000000",
		HomeScope: "private", ReadScopes: []string{"private"}}
	h := NewSearchHandler(pool, staticConfigStore{cfg: &config.Config{}})

	search := func(body map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(string(raw)))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleSearch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("search: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}
	typeNames := func(resp map[string]any) map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, x := range resp["results"].([]any) {
			row := x.(map[string]any)
			name, _ := row["type"].(string)
			out[name]++
		}
		return out
	}

	t.Run("types_filter", func(t *testing.T) {
		resp := search(map[string]any{"category": "wiretest", "types": []string{"knowledge"}})
		if got := typeNames(resp); got["knowledge"] != 1 || len(got) != 1 {
			t.Errorf("types=[knowledge] → %v", got)
		}
	})

	t.Run("types_exclude_and_alias_identical", func(t *testing.T) {
		canonical := search(map[string]any{"category": "wiretest", "types_exclude": []string{"knowledge"}})
		alias := search(map[string]any{"category": "wiretest", "block_roles_exclude": []string{"knowledge"}})
		if cg, ag := typeNames(canonical), typeNames(alias); cg["knowledge"] != 0 || ag["knowledge"] != 0 || cg["audit-trail"] != ag["audit-trail"] || cg["reference"] != ag["reference"] {
			t.Errorf("alias mismatch: types_exclude → %v, block_roles_exclude → %v (must be identical)", cg, ag)
		}
	})

	t.Run("both_names_union", func(t *testing.T) {
		resp := search(map[string]any{
			"category":            "wiretest",
			"types_exclude":       []string{"knowledge"},
			"block_roles_exclude": []string{"audit-trail"},
		})
		got := typeNames(resp)
		if got["knowledge"] != 0 || got["audit-trail"] != 0 || got["reference"] != 1 {
			t.Errorf("union of both names → %v, want only reference", got)
		}
		// The response echoes the EFFECTIVE (unioned) exclude list.
		filters := resp["filters"].(map[string]any)
		excl, _ := filters["types_exclude"].([]any)
		if len(excl) != 2 {
			t.Errorf("filters.types_exclude echo = %v, want the 2-element union", excl)
		}
	})

	t.Run("response_golden_type_fields", func(t *testing.T) {
		resp := search(map[string]any{"category": "wiretest"})
		results := resp["results"].([]any)
		if len(results) != 3 {
			t.Fatalf("unfiltered wiretest rows = %d, want 3", len(results))
		}
		for _, x := range results {
			row := x.(map[string]any)
			for _, key := range []string{"type", "lifecycle_state", "type_source"} {
				v, ok := row[key].(string)
				if !ok || v == "" {
					t.Errorf("result %v: missing/empty %q (FE drift anchor)", row["title"], key)
				}
			}
		}
	})
}
