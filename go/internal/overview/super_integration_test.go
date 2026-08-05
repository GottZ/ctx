//go:build integration

// Wave W-F, persist half (design/02 §7 "W-F" gates 5–9 plus the K10 topic-edge
// gates migration 127 adds). The pure half — resolution search, determinism, the
// measured gonum limit — lives in super_test.go and runs under -short.
package overview_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

// superFix builds `clusters` densely linked triples in `scope`, bridged
// pairwise by one weak link so the supergraph has something to work with.
func superFix(t *testing.T, pool *pgxpool.Pool, scope string, prefix int, clusters int) {
	t.Helper()
	id := func(c, i int) string {
		return fmt.Sprintf("019d0000-0000-7000-9%03d-%03d%09d", prefix, c, i)
	}
	for c := range clusters {
		for i := range 3 {
			insBlock(t, pool, id(c, i), scope, "learnings", fmt.Sprintf("s%d c%d b%d", prefix, c, i))
		}
		insLink(t, pool, id(c, 0), id(c, 1), 0.95)
		insLink(t, pool, id(c, 1), id(c, 2), 0.95)
		insLink(t, pool, id(c, 0), id(c, 2), 0.95)
		if c > 0 {
			insLink(t, pool, id(c-1, 0), id(c, 0), 0.02)
		}
	}
}

// crossScopeFix builds `clusters` communities that each span BOTH scopes: two
// private and two shared blocks, densely linked inside the community. Every
// consecutive pair is bridged four times (private↔private, private↔shared,
// shared↔private, shared↔shared) so the aggregated edge rows cover all four
// scope combinations.
func crossScopeFix(t *testing.T, pool *pgxpool.Pool, clusters int) {
	t.Helper()
	id := func(c, i int) string {
		return fmt.Sprintf("019d0000-0000-7000-9b00-%09d%03d", c, i)
	}
	scopeOf := func(i int) string {
		if i < 2 {
			return "private"
		}
		return "shared"
	}
	for c := range clusters {
		for i := range 4 {
			insBlock(t, pool, id(c, i), scopeOf(i), "learnings", fmt.Sprintf("x%d %d", c, i))
		}
		for i := range 4 {
			for j := i + 1; j < 4; j++ {
				insLink(t, pool, id(c, i), id(c, j), 0.95)
			}
		}
		if c > 0 {
			for _, pair := range [][2]int{{0, 0}, {0, 2}, {2, 0}, {2, 2}} {
				insLink(t, pool, id(c-1, pair[0]), id(c, pair[1]), 0.02)
			}
		}
	}
}

func superOpts(scopeFilter []string, enabled bool, maxNodes int) overview.Options {
	types := []string{"knowledge"}
	return overview.Options{
		Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
		ScopeFilter:        scopeFilter,
		SuperEnabled:       enabled,
		SuperTargetRows:    3,
		SuperMinResolution: 0.05,
		SuperMaxNodes:      maxNodes,
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

// W-F-I1 — the level lands: groups, complete membership, a lead topic that is
// the BIGGEST child, and the two liveness columns filled per scope.
func TestSuperLevel_Persisted(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	superFix(t, pool, "private", 1, 6)

	stats, err := overview.Rebuild(ctx, pool, superOpts(nil, true, 0))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.Skipped {
		t.Fatalf("rebuild skipped (%s)", stats.SkipReason)
	}
	if stats.SuperN == 0 {
		t.Fatal("Stats.SuperN = 0 although the level was enabled")
	}

	groups := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super`)
	if groups != stats.SuperN {
		t.Errorf("%d super rows against Stats.SuperN %d", groups, stats.SuperN)
	}
	// Every topic of the partition hangs in exactly one group. The primary key
	// enforces "at most one"; this is the "at least one" half.
	orphans := scalar[int](t, pool, `
		SELECT count(*)::int FROM graph_cluster_node n
		 WHERE n.topic_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM graph_cluster_super_member m WHERE m.topic_id = n.topic_id)`)
	if orphans != 0 {
		t.Errorf("%d topics without a meta group", orphans)
	}
	// lead_topic_id is the biggest child — the map prints its label as the
	// group's name, so a wrong lead is a wrong headline.
	badLeads := scalar[int](t, pool, `
		SELECT count(*)::int FROM graph_cluster_super s
		 WHERE (SELECT n.size FROM graph_cluster_node n WHERE n.topic_id = s.lead_topic_id)
		     < (SELECT max(n2.size) FROM graph_cluster_super_member m
		          JOIN graph_cluster_node n2 ON n2.topic_id = m.topic_id
		         WHERE m.super_id = s.super_id)`)
	if badLeads != 0 {
		t.Errorf("%d groups led by a topic that is not their biggest", badLeads)
	}
	// size/topic_n are the sums over the children, not a guess.
	badSums := scalar[int](t, pool, `
		SELECT count(*)::int FROM graph_cluster_super s
		 WHERE s.size <> (SELECT COALESCE(sum(n.size), 0)::int FROM graph_cluster_super_member m
		                    JOIN graph_cluster_node n ON n.topic_id = m.topic_id
		                   WHERE m.super_id = s.super_id)
		    OR s.topic_n <> (SELECT count(*)::int FROM graph_cluster_super_member m WHERE m.super_id = s.super_id)`)
	if badSums != 0 {
		t.Errorf("%d groups whose size/topic_n disagree with their membership", badSums)
	}

	superN := scalar[int](t, pool, `SELECT COALESCE(super_n, -1) FROM graph_overview_meta WHERE scope = 'private'`)
	if superN != groups {
		t.Errorf("graph_overview_meta.super_n = %d, want %d", superN, groups)
	}
	gamma := scalar[float64](t, pool, `SELECT COALESCE(super_resolution, -1)::float8 FROM graph_overview_meta WHERE scope = 'private'`)
	if gamma <= 0 || gamma > 1 {
		t.Errorf("super_resolution = %v, want a γ inside (0, 1]", gamma)
	}
}

// W-F-I2 — the shipped default writes NOTHING, and leaves super_n NULL. That
// NULL is what lets the map tell "off" from "capped"; a 0 here would make the
// two indistinguishable.
//
// Rot gegen eine Fassung, die den Level unbedingt schreibt: super rows appear
// and super_n turns 0.
func TestSuperLevel_DisabledWritesNothing(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	superFix(t, pool, "private", 2, 4)

	if _, err := overview.Rebuild(ctx, pool, superOpts(nil, false, 0)); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super`); n != 0 {
		t.Errorf("%d super rows although the level is disabled", n)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super_member`); n != 0 {
		t.Errorf("%d membership rows although the level is disabled", n)
	}
	nulls := scalar[bool](t, pool, `SELECT super_n IS NULL AND super_resolution IS NULL
	                                  FROM graph_overview_meta WHERE scope = 'private'`)
	if !nulls {
		t.Error("super_n/super_resolution are not NULL — 'never attempted' is no longer distinguishable from 'capped'")
	}
	// The main map is untouched by any of this.
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_node`); n == 0 {
		t.Error("no node rows — the disabled meta level took the rebuild with it")
	}
}

// W-F-I3 (design gate 8) — THE CAP IS A DEGRADATION. A supergraph above
// super_max_nodes leaves the scope flat, records (0, 0) as "attempted and
// capped", and the MAIN rebuild commits normally.
//
// Rot gegen eine Fassung, die beim Cap den Haupt-Rebuild skippt oder failt: the
// node/member counts below go to zero.
func TestSuperLevel_CapKeepsMainRebuild(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	superFix(t, pool, "private", 3, 5)

	stats, err := overview.Rebuild(ctx, pool, superOpts(nil, true, 2)) // 5 clusters > cap 2
	if err != nil {
		t.Fatalf("rebuild returned an error over a capped META level: %v", err)
	}
	if stats.Skipped {
		t.Fatalf("main rebuild skipped (%s) over a capped meta level", stats.SkipReason)
	}
	if !stats.SuperCapped {
		t.Error("Stats.SuperCapped = false although the supergraph was above the cap")
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super`); n != 0 {
		t.Errorf("%d super rows despite the cap", n)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_node`); n == 0 {
		t.Error("no node rows — the cap took the main rebuild down with it")
	}
	capped := scalar[bool](t, pool, `SELECT super_n = 0 AND super_resolution = 0
	                                   FROM graph_overview_meta WHERE scope = 'private'`)
	if !capped {
		t.Error("a capped scope must report (0, 0) — the 'attempted and degraded' encoding the map renders as a named cap")
	}
}

// W-F-I4 (K10) — the persistent topic edge. Endpoints are TOPIC ids, never
// cluster ids; the pair is normalised; and the scope pair is oriented with it.
//
// Rot gegen eine Fassung, die scope_s/scope_t der Quellzeile übernimmt: the
// scope check below fails as soon as topic order and cluster order disagree —
// the K1-2 defect, one level up.
// The fixture deliberately builds SCOPE-CROSSING clusters — structurally
// possible, live not instantiated (design/01 §9.3 monitor) — because that is the
// only shape in which the defect is visible at all: with every cluster inside
// one scope, scope_s and scope_t are equal and a mis-oriented pair looks
// correct. Fourteen mixed-scope rows make the chance that no topic pair happens
// to invert against its cluster pair 2^-14.
func TestSuperLevel_TopicEdgesProjected(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	crossScopeFix(t, pool, 8)

	if _, err := overview.Rebuild(ctx, pool, superOpts(nil, true, 0)); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	edges := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_topic_edge`)
	if edges == 0 {
		t.Fatal("no topic edges although the fixture bridges every cluster pair")
	}
	if mixed := scalar[int](t, pool,
		`SELECT count(*)::int FROM graph_cluster_edge WHERE scope_s <> scope_t`); mixed < 8 {
		t.Fatalf("only %d mixed-scope edge rows — the fixture cannot expose a mis-oriented pair", mixed)
	}
	// Same edge count as the cluster-level table: the projection renames the
	// endpoints, it does not filter (one topic per (cluster, scope), so the
	// mapping is a bijection here).
	clusterEdges := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_edge`)
	if edges != clusterEdges {
		t.Errorf("%d topic edges against %d cluster edges — the projection lost or invented rows", edges, clusterEdges)
	}
	// Every endpoint resolves to a node row, and the scope column of each
	// endpoint is the scope of THAT endpoint's topic.
	bad := scalar[int](t, pool, `
		SELECT count(*)::int FROM graph_cluster_topic_edge e
		 WHERE NOT EXISTS (SELECT 1 FROM graph_cluster_node n
		                    WHERE n.topic_id = e.topic_a AND n.scope = e.scope_a)
		    OR NOT EXISTS (SELECT 1 FROM graph_cluster_node n
		                    WHERE n.topic_id = e.topic_b AND n.scope = e.scope_b)`)
	if bad != 0 {
		t.Errorf("%d topic edges whose (topic, scope) pair does not resolve — the scope pair was not turned with the topic pair", bad)
	}
	// No cluster id ever appears as an endpoint. This is the masterplan
	// invariant in assertion form.
	leak := scalar[int](t, pool, `
		SELECT count(*)::int FROM graph_cluster_topic_edge e
		  JOIN graph_cluster_member m ON m.block_id = e.topic_a OR m.block_id = e.topic_b`)
	if leak != 0 {
		t.Errorf("%d topic edges carry a BLOCK uuid as an endpoint — cluster_id leaked into a persisted table", leak)
	}
}

// W-F-I5 — teardown atomicity. A second run REPLACES the level instead of
// accumulating it, and a scoped run touches only its own partition.
//
// Rot gegen eine Fassung ohne die drei neuen teardown-Zeilen: the counts double
// on the second run.
func TestSuperLevel_TeardownReplaces(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	superFix(t, pool, "private", 5, 4)
	superFix(t, pool, "shared", 6, 4)

	if _, err := overview.Rebuild(ctx, pool, superOpts(nil, true, 0)); err != nil {
		t.Fatalf("first global rebuild: %v", err)
	}
	first := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super`)
	firstMembers := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super_member`)
	firstEdges := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_topic_edge`)
	if first == 0 || firstMembers == 0 {
		t.Fatalf("nothing written on the first run (%d groups, %d members)", first, firstMembers)
	}

	if _, err := overview.Rebuild(ctx, pool, superOpts(nil, true, 0)); err != nil {
		t.Fatalf("second global rebuild: %v", err)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super`); got != first {
		t.Errorf("%d groups after the second run, want %d — the teardown did not replace", got, first)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super_member`); got != firstMembers {
		t.Errorf("%d membership rows after the second run, want %d", got, firstMembers)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_topic_edge`); got != firstEdges {
		t.Errorf("%d topic edges after the second run, want %d", got, firstEdges)
	}

	// Scoped run over 'private' only: 'shared' keeps its groups.
	sharedBefore := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super WHERE scope = 'shared'`)
	if _, err := overview.Rebuild(ctx, pool, superOpts([]string{"private"}, true, 0)); err != nil {
		t.Fatalf("scoped rebuild: %v", err)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super WHERE scope = 'shared'`); got != sharedBefore {
		t.Errorf("the scoped run changed the foreign partition: %d groups in 'shared', want %d", got, sharedBefore)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM graph_cluster_super WHERE scope = 'private'`); got == 0 {
		t.Error("the scoped run wrote no groups for its own partition")
	}
	// A group never spans scopes — the per-scope computation makes that
	// structural, and this is the assertion that keeps it structural.
	mixed := scalar[int](t, pool, `
		SELECT count(*)::int FROM graph_cluster_super_member m
		  JOIN graph_cluster_super s ON s.super_id = m.super_id
		 WHERE s.scope <> m.scope`)
	if mixed != 0 {
		t.Errorf("%d membership rows whose scope differs from their group's", mixed)
	}
}
