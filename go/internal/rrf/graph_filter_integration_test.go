//go:build integration

// Issue #225 on both GraphExpand arms: the request filter holds on the SQL arm
// and on the W05.7 cache arm alike, at the same position (after the per-seed
// cap, next to the visibility predicate), so the two arms stay one answer. A
// neighbour the filter excludes is neither added nor traversed: the hop-2 tail
// behind it does not appear either.
//
//	go test -tags=integration ./internal/rrf/ -run TestGraphExpandRequestFilter -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGraphExpandRequestFilterBothArms_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w057-filter"
	snap := w57DiffFixture(t, pool, scope)
	// N1 leaves the request's categories after the build: the snapshot does not
	// carry categories, the per-hop hydrate reads the live row.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET category = 'excluded' WHERE id = $1::uuid`, w57N1); err != nil {
		t.Fatalf("move N1: %v", err)
	}
	filter := rrf.RequestFilter{CategoriesExclude: []string{"excluded"}}

	types := []string{"knowledge"}
	seeds := []rrf.SearchResult{
		{ID: w57S1, Title: "S1", RRFScore: 1.0, Scope: scope},
		{ID: w57S2, Title: "S2", RRFScore: 0.9, Scope: scope},
	}
	for _, tc := range []struct {
		name     string
		directed bool
		hops     int
	}{
		{"directed-1hop", true, 1},
		{"directed-2hop", true, 2},
		{"undirected-2hop", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := w57Cfg()
			cfg.Directed = tc.directed
			cfg.HopDepth = tc.hops

			// Premise: without the filter N1 (and on two hops its tail M1) comes in.
			plain, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, rrf.RequestFilter{}, cfg, rrf.ExpandCache{})
			if err != nil {
				t.Fatalf("sql arm, no filter: %v", err)
			}
			if !w57IDs(plain)[w57N1] || (tc.hops >= 2 && !w57IDs(plain)[w57M1]) {
				t.Fatalf("premise: unfiltered expansion lacks N1/M1 (%v) — the filter assertions would be vacuous", w57Render(plain))
			}

			sqlOut, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, filter, cfg, rrf.ExpandCache{})
			if err != nil {
				t.Fatalf("sql arm: %v", err)
			}
			cacheOut, rep, err := rrf.GraphExpandCachedWithReport(ctx, pool, seeds, []string{scope}, nil, types, filter, cfg,
				rrf.ExpandCache{Snapshot: snap})
			if err != nil {
				t.Fatalf("cache arm: %v", err)
			}
			if rep.Source != graphcache.SourceCache {
				t.Errorf("report source = %q, want %q (the cache arm answered)", rep.Source, graphcache.SourceCache)
			}
			for arm, out := range map[string][]rrf.SearchResult{"sql": sqlOut, "cache": cacheOut} {
				ids := w57IDs(out)
				if !ids[w57DH] {
					t.Errorf("%s arm: positive control — the hub, which passes the filter, is missing", arm)
				}
				if ids[w57N1] {
					t.Errorf("%s arm: N1 injected though the request excluded its category", arm)
				}
				if ids[w57M1] {
					t.Errorf("%s arm: M1 injected — reached THROUGH the excluded N1", arm)
				}
			}
			if a, b := fmt.Sprint(w57Render(cacheOut)), fmt.Sprint(w57Render(sqlOut)); a != b {
				t.Errorf("DIFFERENTIAL mismatch under a filter\n cache: %s\n   sql: %s", a, b)
			}
		})
	}
}

// The per-seed cap stays a cap of EDGES under a filter: a neighbour the caller
// cannot see takes its slot whether or not it matches the request filter. If
// the filter moved in front of the cap on its own, the visible neighbour behind
// an invisible one would come in exactly when the invisible block matches an
// exclusion — the response would tell the caller the category of a block it
// cannot read. Both arms, cap 1, the invisible neighbour on the strongest edge.
func TestGraphExpandRequestFilterKeepsTheCapAnEdgeCap_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		scope   = "w057-oracle"
		foreign = "w057-oracle-foreign"
		seed    = "019f9700-0000-7000-9000-000000000051"
		hidden  = "019f9700-0000-7000-9000-000000000052" // foreign scope, category secret
		visible = "019f9700-0000-7000-9000-000000000053"
	)
	w57Block(t, pool, seed, scope)
	w57Block(t, pool, hidden, scope)
	w57Block(t, pool, visible, scope)
	w57Link(t, pool, seed, hidden, "factual", 0.99)
	w57Link(t, pool, seed, visible, "factual", 0.90)
	snap := w57Build(t, pool)
	// After the build: the snapshot keeps the edge, the hydrate reads the live row.
	w57Move(t, pool, hidden, foreign)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET category = 'secret' WHERE id = $1::uuid`, hidden); err != nil {
		t.Fatalf("categorise hidden: %v", err)
	}

	seeds := []rrf.SearchResult{{ID: seed, Title: "S", RRFScore: 1.0, Scope: scope}}
	types := []string{"knowledge"}
	// Premise: with room for both edges the visible neighbour does come in, so
	// its absence below is the cap's doing and not a stage that never expands.
	cfg := w57Cfg()
	cfg.PerSeedCap = 2
	if out, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, rrf.RequestFilter{}, cfg, rrf.ExpandCache{}); err != nil || !w57IDs(out)[visible] {
		t.Fatalf("premise: cap 2 does not bring the visible neighbour (err %v, %v)", err, w57Render(out))
	}
	cfg.PerSeedCap = 1
	for _, arm := range []struct {
		name  string
		cache rrf.ExpandCache
	}{{"sql", rrf.ExpandCache{}}, {"cache", rrf.ExpandCache{Snapshot: snap}}} {
		t.Run(arm.name, func(t *testing.T) {
			var got []string
			for _, f := range []rrf.RequestFilter{
				{},
				{CategoriesExclude: []string{"secret"}},
				{CategoriesExclude: []string{"no-such-category"}},
			} {
				out, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, f, cfg, arm.cache)
				if err != nil {
					t.Fatalf("expand with %+v: %v", f, err)
				}
				if w57IDs(out)[hidden] {
					t.Fatalf("the foreign-scope neighbour was injected (%+v)", f)
				}
				got = append(got, fmt.Sprint(w57Render(out)))
			}
			if got[1] != got[0] || got[2] != got[0] {
				t.Errorf("the answer depends on whether the INVISIBLE neighbour matches the filter:\n none:     %s\n secret:   %s\n no match: %s",
					got[0], got[1], got[2])
			}
		})
	}
}
