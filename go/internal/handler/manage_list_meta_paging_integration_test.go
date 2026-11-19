//go:build integration

// Issue #212: `manage list-meta` stopped at 10 000 rows and the answer looked
// exactly like a complete listing. A cut answer now says so (`truncated:
// true`) and hands out `next_after`, the (category, title, scope) of its last
// row; sent back as `after`, it answers the rows that follow. A complete
// answer keeps its keys {action, success, blocks}.
//
// The readable store holds one row more than the cap. The page boundary falls
// between two blocks of the same category and title in two scopes the key
// reads, so the cursor has to carry the scope to resume between them.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestManageListMetaPaging -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestManageListMetaPaging(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, plain, err := store.CreateApiKey(ctx, pool, "i212-list-meta", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)

	const capRows = 10000
	// The bulk through COPY (the production triggers run, the per-row API
	// path does not — ten thousand of them would take minutes): capRows-1
	// rows that sort before the twins.
	next := 0
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"context_blocks"},
		[]string{"category", "title", "content", "scope", "type_name", "metadata", "tags"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if next >= capRows-1 {
				return nil, nil
			}
			next++
			return []any{"a-bulk", fmt.Sprintf("bulk %05d", next), "bulk row", "private", "knowledge", map[string]any{}, []string{}}, nil
		})); err != nil {
		t.Fatalf("copy: %v", err)
	}
	// The twins (same category and title, scopes private and shared) and a
	// block in a scope the key does not read, through the store's write path.
	for _, scope := range []string{"shared", "private", "i212-foreign"} {
		if _, err := store.UpsertBlock(ctx, pool, "b-same", "twin", "twin in "+scope, nil, nil, scope, true, store.SensitivityWrite{}, "knowledge"); err != nil {
			t.Fatalf("upsert twin %s: %v", scope, err)
		}
	}
	readable := capRows + 1

	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	listMeta := func(t *testing.T, body string) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(body)).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		h.HandleManage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list-meta %s: status %d, body %.300s", body, rec.Code, rec.Body.String())
		}
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}
	type row struct{ ID, Category, Title, Scope string }
	rowsOf := func(t *testing.T, resp map[string]json.RawMessage) []row {
		t.Helper()
		var rows []row
		if err := json.Unmarshal(resp["blocks"], &rows); err != nil {
			t.Fatalf("blocks: %v", err)
		}
		return rows
	}

	start := time.Now()
	first := listMeta(t, `{"action":"list-meta"}`)
	rows := rowsOf(t, first)
	t.Logf("first page: %d rows in %s", len(rows), time.Since(start))
	if len(rows) != capRows {
		t.Fatalf("first page holds %d rows, want the cap %d", len(rows), capRows)
	}
	if string(first["truncated"]) != "true" {
		t.Fatalf("a listing cut at the cap does not say so: truncated = %q", first["truncated"])
	}
	var cursor map[string]string
	if err := json.Unmarshal(first["next_after"], &cursor); err != nil {
		t.Fatalf("next_after %q: %v", first["next_after"], err)
	}
	last := rows[len(rows)-1]
	if want := map[string]string{"after_category": last.Category, "after_title": last.Title, "after_scope": last.Scope}; !maps.Equal(cursor, want) {
		t.Errorf("next_after = %v, want the last row's position %v", cursor, want)
	}
	if last.Category != "b-same" || last.Scope != "private" {
		t.Errorf("the last row of the first page is %+v; the fixture puts the private twin there", last)
	}

	second := listMeta(t, `{"action":"list-meta","after":`+string(first["next_after"])+`}`)
	rest := rowsOf(t, second)
	if len(rest) != 1 || rest[0].Scope != "shared" || rest[0].Title != "twin" {
		t.Fatalf("second page = %+v, want the shared twin alone", rest)
	}
	keys := make([]string, 0, len(second))
	for k := range second {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"action", "blocks", "success"}) {
		t.Errorf("a complete page answers the keys %v, want [action blocks success] as before #212", keys)
	}

	seen := map[string]bool{}
	for _, r := range append(rows, rest...) {
		if seen[r.ID] {
			t.Errorf("row %s answered twice", r.ID)
		}
		seen[r.ID] = true
		if r.Scope == "i212-foreign" {
			t.Errorf("a row of an unread scope answered: %+v", r)
		}
	}
	if len(seen) != readable {
		t.Errorf("the two pages answer %d distinct rows, want all %d readable ones", len(seen), readable)
	}

	// A position that is not the cursor object (a string, the search
	// cursor's shape) is a caller error, never a first page; another action
	// still decodes a body that carries the key.
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"action":"list-meta","after":"b-same"}`, http.StatusBadRequest},
		{`{"action":"list-meta","after":{"after_updated":"2026-11-01T00:00:00Z","after_id":"019f0000-0000-7000-8000-000000000001"}}`, http.StatusBadRequest},
		{`{"action":"list-meta","after":null}`, http.StatusOK},
		{`{"action":"list-categories","after":"b-same"}`, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(tc.body)).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		h.HandleManage(rec, req)
		if rec.Code != tc.code {
			t.Errorf("%s: status %d, want %d (body %.200s)", tc.body, rec.Code, tc.code, rec.Body.String())
		}
		if tc.code == http.StatusBadRequest && !strings.Contains(rec.Body.String(), "next_after") {
			t.Errorf("%s: the refusal does not name next_after: %s", tc.body, rec.Body.String())
		}
	}
}
