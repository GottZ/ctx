//go:build integration

// Issue #212: list-meta pages in (category, title, scope) order with a
// keyset position, and reports whether rows follow its last one.
//
// TestListMetaPagesWalkTheWholeListing walks a small store with a small page
// size and holds the walk against the one-page listing: same rows, same order,
// no row twice, `more` only before the last page — with blocks of one
// (category, title) in two read scopes, an archived block and a block of an
// unread scope among them. TestListMetaPageCost pins that a page after a
// position costs no more than the first page: the position is an index
// condition, so walking a large store page by page stays linear.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestListMeta' -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestListMetaPagesWalkTheWholeListing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	write := func(category, title, scope, typeName string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, category, title, "body of "+title, nil, nil, scope, true, store.SensitivityWrite{}, typeName)
		if err != nil {
			t.Fatalf("upsert %s/%s/%s: %v", category, title, scope, err)
		}
		return b.ID
	}
	for i := 0; i < 5; i++ {
		write("alpha", fmt.Sprintf("entry %d", i), "private", "knowledge")
		write("beta", fmt.Sprintf("entry %d", i), []string{"private", "shared"}[i%2], "reference")
	}
	// Twins: one (category, title) in both read scopes, three times, so some
	// page boundary falls between two of them.
	for _, title := range []string{"twin a", "twin b", "twin c"} {
		write("gamma", title, "shared", "knowledge")
		write("gamma", title, "private", "knowledge")
	}
	write("gamma", "elsewhere", "i212-foreign", "knowledge")
	archived := write("alpha", "entry archived", "private", "knowledge")
	if _, err := store.DeleteBlock(ctx, pool, archived, []string{"private"}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	scopes := []string{"private", "shared"}
	key := func(m store.BlockMeta) string { return m.Category + "|" + m.Title + "|" + m.Scope }
	for _, filter := range []struct {
		name  string
		types []string
	}{{"all types", nil}, {"reference", []string{"reference"}}} {
		whole, more, err := store.ListMeta(ctx, pool, scopes, filter.types, nil, nil)
		if err != nil || more {
			t.Fatalf("%s: one-page listing: more=%v err=%v", filter.name, more, err)
		}
		if filter.types == nil && len(whole) != 16 {
			t.Fatalf("%s: one-page listing holds %d rows, want 16 (the foreign and the archived block left out)", filter.name, len(whole))
		}
		for i := 1; i < len(whole); i++ {
			if key(whole[i-1]) >= key(whole[i]) {
				t.Errorf("%s: rows %d and %d are not in (category, title, scope) order: %s, %s", filter.name, i-1, i, key(whole[i-1]), key(whole[i]))
			}
		}
		for _, page := range []int{1, 2, 3, 5} {
			t.Run(fmt.Sprintf("%s/page=%d", filter.name, page), func(t *testing.T) {
				var walked []string
				var after *store.ListMetaCursor
				for step := 0; ; step++ {
					if step > len(whole)+1 {
						t.Fatalf("the walk does not end")
					}
					rows, more, err := store.ListMetaPageForTest(ctx, pool, scopes, filter.types, nil, after, page)
					if err != nil {
						t.Fatalf("page %d: %v", step, err)
					}
					if more && len(rows) != page {
						t.Fatalf("page %d: more=true on a page of %d rows, want %d", step, len(rows), page)
					}
					for _, r := range rows {
						walked = append(walked, key(r))
					}
					if !more {
						break
					}
					last := rows[len(rows)-1]
					after = &store.ListMetaCursor{Category: last.Category, Title: last.Title, Scope: last.Scope}
				}
				want := make([]string, len(whole))
				for i, r := range whole {
					want[i] = key(r)
				}
				if !slices.Equal(walked, want) {
					t.Errorf("the walk answers\n %s\nthe one-page listing\n %s", strings.Join(walked, ", "), strings.Join(want, ", "))
				}
			})
		}
	}
}

// listMetaCostRows is the size of the cost fixture: a tenth of the rows in
// the scope "private", the rest spread over seven other scopes.
const listMetaCostRows = 60000

func listMetaCostSeed(ctx context.Context, pool *pgxpool.Pool) error {
	next := 0
	newest := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"context_blocks"},
		[]string{"category", "title", "content", "scope", "type_name", "created_at", "updated_at", "metadata", "tags"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if next >= listMetaCostRows {
				return nil, nil
			}
			i := next
			next++
			scope := "private"
			if i%10 != 0 {
				scope = fmt.Sprintf("other-%d", i%7)
			}
			ts := newest.Add(-time.Duration(i) * time.Second)
			// 7919 and 104729 are prime: category and title are permutations
			// of the insert order, so the heap order says nothing about them.
			return []any{fmt.Sprintf("cat-%02d", (i*7919)%40), fmt.Sprintf("title %06d", (i*104729)%listMetaCostRows),
				"body", scope, "knowledge", ts, ts, map[string]any{}, []string{}}, nil
		}))
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `VACUUM (ANALYZE) context_blocks`)
	return err
}

func TestListMetaPageCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupSeededTestDB(t, "i212-list-meta-cost", listMetaCostSeed)
	others := []string{"other-0", "other-1", "other-2", "other-3", "other-4", "other-5", "other-6"}
	mid := &store.ListMetaCursor{Category: "cat-20", Title: "title 030000", Scope: "private"}
	for _, scopes := range [][]string{{"private"}, append([]string{"private"}, others...)} {
		firstSQL, firstArgs := store.ListMetaSQLForTest(scopes, nil, nil, nil, store.ListMetaMaxRows)
		midSQL, midArgs := store.ListMetaSQLForTest(scopes, nil, nil, mid, store.ListMetaMaxRows)
		for _, plan := range []string{"custom", "generic"} {
			explain := testdb.ExplainCustom
			if plan == "generic" {
				explain = testdb.ExplainGeneric
			}
			first := explain(t, pool, firstSQL, firstArgs...)
			after := explain(t, pool, midSQL, midArgs...)
			bound := first.Buffers + first.Buffers/10 + 16
			t.Logf("%d scope(s), %s plan: first page %d buffers (%.1f ms), page after the middle %d buffers (%.1f ms), bound %d",
				len(scopes), plan, first.Buffers, first.ExecMs, after.Buffers, after.ExecMs, bound)
			if after.Buffers > bound {
				t.Errorf("%d scope(s), %s plan: the page after a position read %d buffers, more than the first page %d + 10 %% + 16: the position is not an index condition",
					len(scopes), plan, after.Buffers, first.Buffers)
			}
		}
	}
}
