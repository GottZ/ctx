//go:build integration

// Cost gate of issue #219: `tags_all` (tags @> $n) takes the GIN index on
// tags (idx_context_tags, array_ops), as the overlap filter does, under a
// custom and under a generic plan, on the browse and on the full-text path.
//
// The fixture labels every block along two independent dimensions — the
// entity it is about (one of 300, 100 blocks each) and the kind of record it
// is (one of 10, 3 000 blocks each) — the case the issue names. The
// intersection of one entity and one kind is 10 blocks; the union of the two
// is 3 090. The bound: containment reads no more than the overlap filter on
// the rare tag alone (100 rows) plus a fixed allowance for the posting tree of
// the frequent tag, and far less than the heap.
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	tagsAllEntities = 300
	tagsAllKinds    = 10
	tagsAllRows     = 30000
	// tagsAllTerm is in every block, so the full-text path matches the whole
	// store and the tag filter alone decides the cost.
	tagsAllTerm = "ledgerline"
)

// tagsAllSeed is a mass seed through COPY (the production triggers run, the
// per-row API path does not), like unrankedSeed.
func tagsAllSeed(ctx context.Context, pool *pgxpool.Pool) error {
	newest := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	next := 0
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"context_blocks"},
		[]string{"category", "title", "content", "scope", "type_name", "created_at", "updated_at", "metadata", "tags"},
		pgx.CopyFromFunc(func() ([]any, error) {
			if next >= tagsAllRows {
				return nil, nil
			}
			i := next
			next++
			ts := newest.Add(-time.Duration(i) * time.Second)
			entity := fmt.Sprintf("entity-%03d", i%tagsAllEntities)
			kind := fmt.Sprintf("kind-%02d", (i/tagsAllEntities)%tagsAllKinds)
			return []any{"records", fmt.Sprintf("record %05d", i),
				fmt.Sprintf("%s %d of %s, a %s entry with some body text", tagsAllTerm, i, entity, kind),
				"private", "knowledge", ts, ts, map[string]any{}, []string{entity, kind}}, nil
		}))
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `VACUUM (ANALYZE) context_blocks`)
	return err
}

func TestSearchTagsAllCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupSeededTestDB(t, "i219-tags-all-cost", tagsAllSeed)
	ctx := context.Background()
	heap := unrankedHeapPages(t, pool)

	entity, kind := "entity-007", "kind-03"
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE tags @> $1`, []string{entity, kind}).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != tagsAllRows/(tagsAllEntities*tagsAllKinds) {
		t.Fatalf("fixture: %d blocks carry both tags, want %d", n, tagsAllRows/(tagsAllEntities*tagsAllKinds))
	}

	scopes := []string{"private"}
	for _, query := range []string{"", tagsAllTerm} {
		for _, plan := range []string{"custom", "generic"} {
			explain := testdb.ExplainCustom
			if plan == "generic" {
				explain = testdb.ExplainGeneric
			}
			stmt := func(tags, tagsAll []string) (string, []any) {
				return store.SearchBlocksSQLForTest(store.SearchBlocksParams{
					Query: query, ReadScopes: scopes, Tags: tags, TagsAll: tagsAll,
					Limit: store.SearchMaxLimit, Compact: true, ProbeNext: true,
				})
			}
			allSQL, allArgs := stmt(nil, []string{entity, kind})
			rareSQL, rareArgs := stmt([]string{entity}, nil)
			all := explain(t, pool, allSQL, allArgs...)
			rare := explain(t, pool, rareSQL, rareArgs...)
			// The posting tree of a tag on 3 000 rows and the GIN entry pages:
			// a fixed allowance, independent of the heap.
			const ginAllowance = 32
			bound := rare.Buffers + ginAllowance
			t.Logf("query=%q (%s plan): tags_all %d buffers (%.2f ms), tags [rare] %d buffers, bound %d, heap %d pages",
				query, plan, all.Buffers, all.ExecMs, rare.Buffers, bound, heap)
			if !strings.Contains(all.Plan, `"Index Name": "idx_context_tags"`) {
				t.Errorf("query=%q (%s plan): tags_all does not use idx_context_tags:\n%s", query, plan, all.Plan)
			}
			if all.Buffers > bound {
				t.Errorf("query=%q (%s plan): tags_all read %d buffers, more than the overlap on the rare tag (%d) + %d",
					query, plan, all.Buffers, rare.Buffers, ginAllowance)
			}
			if all.Buffers*20 > heap {
				t.Errorf("query=%q (%s plan): tags_all read %d buffers, more than a twentieth of the heap (%d pages)",
					query, plan, all.Buffers, heap)
			}
		}
	}
}
