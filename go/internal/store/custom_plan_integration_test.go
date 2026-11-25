//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestCustomPlanModeWithoutDescriptionCache (issue #536): the statements
// planned with their parameters on every execution (store.CustomPlanMode)
// answer on a pool whose connection string turns pgx's description cache off
// (description_cache_capacity=0) — pgx refuses QueryExecModeCacheDescribe
// there on every call, and the issue search and the id-prefix lookup failed.
func TestCustomPlanModeWithoutDescriptionCache(t *testing.T) {
	_, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	cfg, err := pgxpool.ParseConfig(dsn + sep + "description_cache_capacity=0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.DescriptionCacheCapacity != 0 {
		t.Fatalf("description cache capacity %d, want 0", cfg.ConnConfig.DescriptionCacheCapacity)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	b, err := store.UpsertBlock(ctx, pool, "learnings", "description cache off", "a body with the word quokka", nil, nil, "dc-proj", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ { // past the five executions after which a named statement turns generic
		id, cands, err := store.ResolveBlockID(ctx, pool, b.ID[:13], []string{"dc-proj"}, nil)
		if err != nil {
			t.Fatalf("ResolveBlockID with the description cache off: %v", err)
		}
		if id != b.ID || len(cands) > 1 {
			t.Fatalf("ResolveBlockID(%q) = %q, %d candidates; want %q", b.ID[:13], id, len(cands), b.ID)
		}
		for _, sort := range []string{store.SortUpdated, store.SortCreated} {
			if _, _, err := store.SearchIssues(ctx, pool, store.IssueReadQuery{Scope: "dc-proj", Q: "quokka", Sort: sort, Limit: 10}); err != nil {
				t.Fatalf("SearchIssues (sort %s) with the description cache off: %v", sort, err)
			}
		}
	}
}
