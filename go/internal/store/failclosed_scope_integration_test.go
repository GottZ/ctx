//go:build integration

// Fail-closed read-path hardening (T07, design/01 §5.0/§5.4): EVERY store read
// function that filters visibility by a readScopes slice must REJECT an empty
// scope set with store.ErrNoScopes — it must NOT silently return 0 rows. An
// empty/unknown/suspended tenant resolves to an empty scope set (the TenantScopes
// contract, §4.2); without an explicit guard the PG `scope = ANY('{}')` artefact
// collapses to "matches nothing", which is INDISTINGUISHABLE in the result from
// "no data" — one new caller (a future OR-arm, a new mount, an internal call)
// that misreads that as "no filter" is a silent fail-OPEN. The guard mirrors the
// reject that today lives ONLY in rrf.Search (search.go:65-67) onto every read
// path, as the per-path second line of defence behind the auth choke point (§5.0).
//
// The case list is the COMPLETE set of store read functions taking a readScopes
// visibility filter (objective grep criterion: a `func [A-Z]... readScopes
// []string` with a `scope = ANY(readScopes)` predicate). CreateSession also takes
// readScopes but SNAPSHOTS them into a new row (a write), so it is correctly NOT
// a read path and is excluded.
//
// RED (pre-T07): the functions run the query and return empty results with a NIL
// error — errors.Is(err, ErrNoScopes) is false for all of them → this test fails.
//
//	go test -tags=integration ./internal/store/ -run TestReadPathsFailClosed -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedFailClosedBlock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// A real row in "private" so the negative is NON-VACUOUS: the data EXISTS,
	// only the empty scope set hides it. A pre-guard run returns 0 rows + nil
	// error here (the fail-open we close), not because the store is empty.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', 'failclosed-seed', 'seed content', 'private')`); err != nil {
		t.Fatalf("seed block: %v", err)
	}
}

func TestReadPathsFailClosedOnEmptyScopes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedFailClosedBlock(t, pool)

	empty := []string{}
	const someUUID = "00000000-0000-7000-8000-000000000001"
	cases := []struct {
		name string
		call func() error
	}{
		// blocks.go
		{"ResolveBlockID", func() error { _, _, e := store.ResolveBlockID(ctx, pool, "0123abcd", empty, nil); return e }},
		{"GetBlock", func() error { _, e := store.GetBlock(ctx, pool, nil, someUUID, empty, nil); return e }},
		{"SearchBlocks", func() error { _, e := store.SearchBlocks(ctx, pool, nil, "", empty, "", nil, 10, true, nil, nil, nil, nil, nil); return e }},
		{"RecentBlocks", func() error { _, e := store.RecentBlocks(ctx, pool, nil, empty, "", 10, nil, nil); return e }},
		{"ListCategories", func() error { _, e := store.ListCategories(ctx, pool, empty); return e }},
		{"GetStats", func() error { _, e := store.GetStats(ctx, pool, empty); return e }},
		{"ListMeta", func() error { _, e := store.ListMeta(ctx, pool, empty, nil, nil); return e }},
		{"GuardList", func() error { _, e := store.GuardList(ctx, pool, empty, "", "", nil, 10); return e }},
		{"GetGuardStats", func() error { _, e := store.GetGuardStats(ctx, pool, empty); return e }},
		// blobs.go
		{"GetBlobByID", func() error { _, e := store.GetBlobByID(ctx, pool, someUUID, empty, true); return e }},
		{"GetBlobByCategoryTitle", func() error { _, e := store.GetBlobByCategoryTitle(ctx, pool, "c", "t", empty, true); return e }},
		{"SearchBlobs", func() error { _, e := store.SearchBlobs(ctx, pool, empty, "", nil, "", 10); return e }},
		{"GetBlobStats", func() error { _, e := store.GetBlobStats(ctx, pool, empty); return e }},
		{"ListBlobs", func() error { _, e := store.ListBlobs(ctx, pool, empty, 10); return e }},
		// overview.go / graph.go
		{"GraphOverview", func() error {
			_, e := store.GraphOverview(ctx, pool, store.OverviewParams{NodeLimit: 10, MinClusterSize: 1}, empty)
			return e
		}},
		{"EgoGraph", func() error {
			_, e := store.EgoGraph(ctx, pool, store.EgoParams{Focus: someUUID, Hops: 1, Limit: 10}, empty, nil, gVisibleTypes)
			return e
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); !errors.Is(err, store.ErrNoScopes) {
				t.Fatalf("%s with empty scopes: got err=%v, want store.ErrNoScopes (fail-closed)", c.name, err)
			}
		})
	}

	// A nil slice (what TenantScopes returns for an unknown tenant — the realistic
	// empty) must reject too; len()==0 covers both, this pins it.
	if _, err := store.GetStats(ctx, pool, nil); !errors.Is(err, store.ErrNoScopes) {
		t.Fatalf("GetStats(nil scopes): got err=%v, want store.ErrNoScopes", err)
	}
}
