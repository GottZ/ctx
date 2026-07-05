//go:build integration

// B-W6 DB gates (internal package: loadNodes/loadEdges are unexported).
// Proves the scoped INPUT path: Rebuild with a ScopeFilter loads only the
// tenant partition (nodes AND edges, both endpoints — review B2-M4), the
// NodeCount matches the owned-visible block count (B1-M2), a mixed
// cross-partition edge never enters a single-scope input, and a filter
// covering ALL corpus scopes reproduces the global run byte-identically
// (the E1/B-W0 equivalence at a corpus that lies inside owned).
package overview

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantLoopB6(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	types := []string{"knowledge"}

	const (
		P1 = "019d0000-0000-7000-9000-0000000000b1" // private
		P2 = "019d0000-0000-7000-9000-0000000000b2" // private
		P3 = "019d0000-0000-7000-9000-0000000000b3" // private
		W1 = "019d0000-0000-7000-9000-0000000000f1" // work
		W2 = "019d0000-0000-7000-9000-0000000000f2" // work
	)
	insBlockB3(t, pool, P1, "private", "P1")
	insBlockB3(t, pool, P2, "private", "P2")
	insBlockB3(t, pool, P3, "private", "P3")
	insBlockB3(t, pool, W1, "work", "W1")
	insBlockB3(t, pool, W2, "work", "W2")
	insLinkB3(t, pool, P1, P2, 0.9)
	insLinkB3(t, pool, P2, P3, 0.8)
	insLinkB3(t, pool, W1, W2, 0.7)
	insLinkB3(t, pool, P3, W1, 0.6) // cross-partition mixed edge

	countBlocks := func(scope string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE scope = $1`, scope).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("scoped loads cut nodes and edges to the partition (B2-M4)", func(t *testing.T) {
		nodes, scopes, err := loadNodes(ctx, pool, types, []string{"private"})
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 3 {
			t.Fatalf("scoped loadNodes = %d nodes, want 3 (private only)", len(nodes))
		}
		for id, s := range scopes {
			if s != "private" {
				t.Fatalf("scoped loadNodes leaked block %s scope %q", id, s)
			}
		}
		edges, err := loadEdges(ctx, pool, []string{"private"})
		if err != nil {
			t.Fatal(err)
		}
		// P1-P2, P2-P3 stay; the mixed P3-W1 edge must NOT enter the input.
		if len(edges) != 2 {
			t.Fatalf("scoped loadEdges = %d edges, want 2 (mixed edge excluded)", len(edges))
		}
		for _, e := range edges {
			if e.src == P3 && e.dst == W1 {
				t.Fatal("mixed cross-partition edge entered the scoped input")
			}
		}
		// Red-probe contrast: the nil filter (global load) DOES carry it.
		all, err := loadEdges(ctx, pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 {
			t.Fatalf("global loadEdges = %d edges, want 4", len(all))
		}
	})

	t.Run("tenant rebuild NodeCount == owned-visible blocks (B1-M2)", func(t *testing.T) {
		stats, err := Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"private"},
		})
		if err != nil {
			t.Fatalf("private rebuild: %v", err)
		}
		if want := countBlocks("private"); stats.NodeCount != want {
			t.Fatalf("private NodeCount = %d, want %d (owned-visible blocks)", stats.NodeCount, want)
		}
		stats, err = Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"work"},
		})
		if err != nil {
			t.Fatalf("work rebuild: %v", err)
		}
		if want := countBlocks("work"); stats.NodeCount != want {
			t.Fatalf("work NodeCount = %d, want %d", stats.NodeCount, want)
		}
		// Red-probe contrast (the B1-M2 drift signal): a nil filter counts the
		// whole corpus — strictly more than either partition.
		global, err := Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
		})
		if err != nil {
			t.Fatal(err)
		}
		if global.NodeCount <= countBlocks("private") {
			t.Fatalf("nil-filter NodeCount = %d — expected the oversized global count the B1-M2 assert exists to catch", global.NodeCount)
		}
	})

	t.Run("two tenant ticks build disjoint partitions", func(t *testing.T) {
		workBefore := snapshotPartition(t, pool, "work")
		if _, err := Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"private"},
		}); err != nil {
			t.Fatal(err)
		}
		if got := snapshotPartition(t, pool, "work"); got != workBefore {
			t.Fatalf("private tick touched the work partition:\nbefore:\n%s\nafter:\n%s", workBefore, got)
		}
		if _, err := Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"work"},
		}); err != nil {
			t.Fatal(err)
		}
		if got := snapshotPartition(t, pool, "work"); got == "" {
			t.Fatal("work partition empty after its tick")
		}
		var mixed int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM graph_cluster_edge WHERE scope_s <> scope_t`).Scan(&mixed); err != nil {
			t.Fatal(err)
		}
		if mixed != 0 {
			t.Fatalf("scoped ticks produced %d cross-scope edge rows, want 0", mixed)
		}
		var memberN int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_member`).Scan(&memberN); err != nil {
			t.Fatal(err)
		}
		if memberN != 5 {
			t.Fatalf("member rows = %d, want 5 (both partitions present)", memberN)
		}
	})

	t.Run("filter covering all corpus scopes == global run (E1 equivalence)", func(t *testing.T) {
		global, err := Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
		})
		if err != nil {
			t.Fatal(err)
		}
		globalSnap := snapshotPartition(t, pool, "private") + snapshotPartition(t, pool, "work")
		scoped, err := Rebuild(ctx, pool, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"private", "work"},
		})
		if err != nil {
			t.Fatal(err)
		}
		scopedSnap := snapshotPartition(t, pool, "private") + snapshotPartition(t, pool, "work")
		if global.NodeCount != scoped.NodeCount || global.ClusterCount != scoped.ClusterCount ||
			global.EdgeRows != scoped.EdgeRows || global.Modularity != scoped.Modularity {
			t.Fatalf("stats diverge: global=%+v scoped=%+v", global, scoped)
		}
		if globalSnap != scopedSnap {
			t.Fatalf("partition content diverges:\nglobal:\n%s\nscoped:\n%s", globalSnap, scopedSnap)
		}
	})

	t.Run("EXPLAIN: scoped edge load joins blocks via index, no full scan (B2-M4)", func(t *testing.T) {
		// Pad a third scope so the planner has a reason to choose the index.
		for i := 0; i < 1000; i++ {
			id := fmt.Sprintf("019d0000-0000-7000-9000-1%011d", i)
			insBlockB3(t, pool, id, "shared", fmt.Sprintf("pad-%d", i))
		}
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks; ANALYZE context_dream_links`); err != nil {
			t.Fatal(err)
		}
		rows, err := pool.Query(ctx, `EXPLAIN SELECT l.source_block_id::text, l.target_block_id::text, l.raw_confidence
		 FROM context_dream_links l
		 JOIN context_blocks bs ON bs.id = l.source_block_id AND bs.scope = ANY($1)
		 JOIN context_blocks bt ON bt.id = l.target_block_id AND bt.scope = ANY($1)
		 WHERE l.relationship <> 'supersedes'
		 ORDER BY l.source_block_id, l.target_block_id`, []string{"private"})
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString("  " + line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("EXPLAIN scoped edge load:\n%s", plan.String())
		if strings.Contains(plan.String(), "Seq Scan on context_blocks") {
			t.Fatalf("scoped edge load full-scans context_blocks:\n%s", plan.String())
		}
	})
}
