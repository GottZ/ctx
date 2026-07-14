//go:build integration

// W04-2 negative gate (plan graph-structural 2026-07-14, design/04 §4.1;
// doctrine: docs/architecture.md §"Structural links", edge-consumer table —
// "inference reads dream, display counts both"): Overview/Louvain is an
// INFERENCE consumer and reads exclusively context_dream_links. Structural
// `references` edges are chronology/reference FACTS (weight 1.0 by
// definition), not topical similarity — their participation would let
// report star-hubs clamp unrelated topic communities together and would
// dominate every dream confidence class (§4.1 points 1–3).
//
// This file is package overview (NOT overview_test, unlike
// cluster_integration_test.go) on purpose: assert (i) reads the unexported
// loadEdges directly.
//
// TWO asserts on the TWO independent read paths — a single clustering-outcome
// assert would not be red-provable: Louvain (community.Modularize,
// cluster.go:293) typically does NOT merge two dream-dense components across
// a single weight-1.0 bridge (barbell behaviour), so a leaked structural
// edge could pass an outcome-only gate.
//
//	(i)  INPUT level: loadEdges (cluster.go:355) yields NO pair that is only
//	     structurally connected — checked for the global (nil filter) and the
//	     scoped query variant (two distinct SQL strings).
//	(ii) AGGREGATE level: after Rebuild, graph_cluster_edge holds NO
//	     meta-edge between the two component clusters — edgeAggTemplate
//	     (cluster.go:452) reads context_dream_links INDEPENDENTLY of
//	     loadEdges, so (i) green does not imply (ii) green.
//
// Red probes (both documented in the wave commit body): extending the
// loadEdges SQL with `UNION ALL … FROM context_structural_links` turns
// exactly assert (i) red; extending edgeAggTemplate the same way turns
// exactly assert (ii) red. Each probe reverted → green.
package overview

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

func insIsoBlock(t *testing.T, pool *pgxpool.Pool, id, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid, 'learnings', $2, $3, 'private')`,
		id, title, "content "+title)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func insIsoDreamLink(t *testing.T, pool *pgxpool.Pool, src, dst string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'private')`,
		src, dst)
	if err != nil {
		t.Fatalf("insert dream link %s->%s: %v", src, dst, err)
	}
}

// TestRebuild_StructuralLinkIsolation: two dream-disjoint triangles bridged
// by ONE structural `references` edge. The bridge must be invisible to both
// overview read paths.
func TestRebuild_StructuralLinkIsolation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A1 = "019d0000-0000-7000-9000-0000000000a1"
		A2 = "019d0000-0000-7000-9000-0000000000a2"
		A3 = "019d0000-0000-7000-9000-0000000000a3"
		B1 = "019d0000-0000-7000-9000-0000000000b1"
		B2 = "019d0000-0000-7000-9000-0000000000b2"
		B3 = "019d0000-0000-7000-9000-0000000000b3"
	)
	compA := map[string]bool{A1: true, A2: true, A3: true}

	for id, title := range map[string]string{A1: "A1", A2: "A2", A3: "A3", B1: "B1", B2: "B2", B3: "B3"} {
		insIsoBlock(t, pool, id, title)
	}
	// Component A: dream triangle, high confidence.
	insIsoDreamLink(t, pool, A1, A2)
	insIsoDreamLink(t, pool, A2, A3)
	insIsoDreamLink(t, pool, A1, A3)
	// Component B: dream triangle, high confidence.
	insIsoDreamLink(t, pool, B1, B2)
	insIsoDreamLink(t, pool, B2, B3)
	insIsoDreamLink(t, pool, B1, B3)
	// The ONE structural bridge between the dream-disjoint components.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid, $2::uuid, 'references', 'private', 'system')`,
		A3, B1); err != nil {
		t.Fatalf("insert structural bridge %s->%s: %v", A3, B1, err)
	}

	// Assert (i) — INPUT level: loadEdges must yield ONLY dream pairs; no
	// edge may cross the components (the bridge is their sole connection).
	crossEdges := func(edges []rawEdge) []rawEdge {
		var cross []rawEdge
		for _, e := range edges {
			if compA[e.src] != compA[e.dst] {
				cross = append(cross, e)
			}
		}
		return cross
	}
	edges, err := loadEdges(ctx, pool, nil)
	if err != nil {
		t.Fatalf("loadEdges (global): %v", err)
	}
	if len(edges) != 6 {
		t.Errorf("loadEdges (global) returned %d edges, want exactly the 6 dream links", len(edges))
	}
	if cross := crossEdges(edges); len(cross) > 0 {
		t.Errorf("assert (i) FAILED: loadEdges (global) leaked %d structural-only cross-component pair(s): %v", len(cross), cross)
	}
	scopedEdges, err := loadEdges(ctx, pool, []string{"private"})
	if err != nil {
		t.Fatalf("loadEdges (scoped): %v", err)
	}
	if len(scopedEdges) != 6 {
		t.Errorf("loadEdges (scoped) returned %d edges, want exactly the 6 dream links", len(scopedEdges))
	}
	if cross := crossEdges(scopedEdges); len(cross) > 0 {
		t.Errorf("assert (i) FAILED: loadEdges (scoped) leaked %d structural-only cross-component pair(s): %v", len(cross), cross)
	}

	// Rebuild over the real path (global run, like cluster_integration_test).
	stats, err := Rebuild(ctx, pool, Options{Resolution: 1.0, VisibleTypes: []string{"knowledge"}, OverviewTypes: []string{"knowledge"}})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.NodeCount != 6 {
		t.Errorf("NodeCount = %d, want 6", stats.NodeCount)
	}

	// Precondition for (ii): with the bridge excluded the components share no
	// input edge at all, so they MUST land in different clusters (this is not
	// the barbell coincidence — there is nothing to merge across).
	memberCluster := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT block_id::text, cluster_id::text FROM graph_cluster_member`)
	if err != nil {
		t.Fatalf("read members: %v", err)
	}
	for rows.Next() {
		var b, c string
		if err := rows.Scan(&b, &c); err != nil {
			rows.Close()
			t.Fatalf("scan member: %v", err)
		}
		memberCluster[b] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("member rows: %v", err)
	}
	clustersA := map[string]bool{}
	clustersB := map[string]bool{}
	for b, c := range memberCluster {
		if compA[b] {
			clustersA[c] = true
		} else {
			clustersB[c] = true
		}
	}
	for c := range clustersA {
		if clustersB[c] {
			t.Fatalf("fixture precondition broken: cluster %s spans both components — components are not dream-disjoint", c)
		}
	}

	// Assert (ii) — AGGREGATE level: no graph_cluster_edge meta-edge may
	// connect a component-A cluster with a component-B cluster. The edge
	// aggregation (edgeAggTemplate) reads context_dream_links independently
	// of loadEdges; a structural UNION there would materialize the bridge as
	// a meta-edge even though the Louvain input never saw it.
	eRows, err := pool.Query(ctx, `SELECT cluster_a::text, cluster_b::text, link_count FROM graph_cluster_edge`)
	if err != nil {
		t.Fatalf("read cluster edges: %v", err)
	}
	defer eRows.Close()
	for eRows.Next() {
		var ca, cb string
		var n int
		if err := eRows.Scan(&ca, &cb, &n); err != nil {
			t.Fatalf("scan cluster edge: %v", err)
		}
		if (clustersA[ca] && clustersB[cb]) || (clustersB[ca] && clustersA[cb]) {
			t.Errorf("assert (ii) FAILED: graph_cluster_edge holds a meta-edge %s—%s (link_count=%d) between the dream-disjoint components — structural bridge leaked into the aggregate", ca, cb, n)
		}
	}
	if err := eRows.Err(); err != nil {
		t.Fatalf("cluster edge rows: %v", err)
	}
}
