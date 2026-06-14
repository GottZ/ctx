//go:build integration

package overview_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

func insBlock(t *testing.T, pool *pgxpool.Pool, id, scope, category, title string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid, $2, $3, $4, $5)`,
		id, category, title, "content "+title, scope)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func insLink(t *testing.T, pool *pgxpool.Pool, src, dst string, rawConf float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', $3, $3, 'private')`,
		src, dst, rawConf)
	if err != nil {
		t.Fatalf("insert link %s->%s: %v", src, dst, err)
	}
}

func dumpMembers(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT block_id::text, cluster_id::text FROM graph_cluster_member`)
	if err != nil {
		t.Fatalf("dump members: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var b, c string
		if err := rows.Scan(&b, &c); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		out[b] = c
	}
	return out
}

// TestRebuild_ScopePartitioned is the W1 DB gate: it proves the scope-PARTITIONED
// aggregation (the solved count-leak) holds against a real schema. Two triangles
// (A,B private + C shared / D,E,F work) joined by a weak cross-scope bridge.
func TestRebuild_ScopePartitioned(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-000000000001" // private
		B = "019d0000-0000-7000-9000-000000000002" // private
		C = "019d0000-0000-7000-9000-000000000003" // shared
		D = "019d0000-0000-7000-9000-000000000004" // work
		E = "019d0000-0000-7000-9000-000000000005" // work
		F = "019d0000-0000-7000-9000-000000000006" // work
	)
	insBlock(t, pool, A, "private", "learnings", "A")
	insBlock(t, pool, B, "private", "learnings", "B")
	insBlock(t, pool, C, "shared", "decisions", "C")
	insBlock(t, pool, D, "work", "infrastructure", "D")
	insBlock(t, pool, E, "work", "infrastructure", "E")
	insBlock(t, pool, F, "work", "infrastructure", "F")
	insLink(t, pool, A, B, 0.9)
	insLink(t, pool, B, C, 0.9)
	insLink(t, pool, A, C, 0.9)
	insLink(t, pool, D, E, 0.9)
	insLink(t, pool, E, F, 0.9)
	insLink(t, pool, D, F, 0.9)
	insLink(t, pool, C, F, 0.05) // weak cross-scope bridge

	stats, err := overview.Rebuild(ctx, pool, 1.0)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.NodeCount != 6 {
		t.Errorf("NodeCount = %d, want 6", stats.NodeCount)
	}
	if stats.ClusterCount < 1 {
		t.Errorf("ClusterCount = %d, want >= 1", stats.ClusterCount)
	}

	// member count matches.
	var memberN int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_cluster_member`).Scan(&memberN); err != nil {
		t.Fatal(err)
	}
	if memberN != 6 {
		t.Errorf("graph_cluster_member count = %d, want 6", memberN)
	}

	// meta single row populated.
	var metaN, clusterN int
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(cluster_n),0) FROM graph_overview_meta`).Scan(&metaN, &clusterN); err != nil {
		t.Fatal(err)
	}
	if metaN != 1 || clusterN < 1 {
		t.Errorf("graph_overview_meta: rows=%d cluster_n=%d", metaN, clusterN)
	}

	// INVARIANT 1 (scope partitioning of nodes): every graph_cluster_node.size
	// equals the count of members of that cluster IN THAT ONE SCOPE. This is the
	// design's core: a node aggregate never counts across scope boundaries.
	var nodeMismatch int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM graph_cluster_node n WHERE n.size <> (
			SELECT count(*) FROM graph_cluster_member m
			JOIN context_blocks b ON b.id = m.block_id
			WHERE m.cluster_id = n.cluster_id AND b.scope = n.scope)`).Scan(&nodeMismatch); err != nil {
		t.Fatal(err)
	}
	if nodeMismatch != 0 {
		t.Errorf("%d graph_cluster_node rows mismatch member⋈blocks scope count (scope leak!)", nodeMismatch)
	}

	// INVARIANT 2 (scope-pair partitioning of edges): every graph_cluster_edge
	// link_count equals the real number of inter-cluster links with that exact
	// (cluster-pair, scope-pair). A meta-edge never aggregates across scopes.
	var edgeMismatch int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM graph_cluster_edge e WHERE e.link_count <> (
			SELECT count(*) FROM context_dream_links l
			JOIN graph_cluster_member ms ON ms.block_id = l.source_block_id
			JOIN graph_cluster_member mt ON mt.block_id = l.target_block_id
			JOIN context_blocks bs ON bs.id = l.source_block_id
			JOIN context_blocks bt ON bt.id = l.target_block_id
			WHERE l.relationship <> 'supersedes' AND ms.cluster_id <> mt.cluster_id
			  AND LEAST(ms.cluster_id, mt.cluster_id) = e.cluster_a
			  AND GREATEST(ms.cluster_id, mt.cluster_id) = e.cluster_b
			  AND bs.scope = e.scope_s AND bt.scope = e.scope_t)`).Scan(&edgeMismatch); err != nil {
		t.Fatal(err)
	}
	if edgeMismatch != 0 {
		t.Errorf("%d graph_cluster_edge rows mismatch real scope-pair link count (scope leak!)", edgeMismatch)
	}

	// Sum of node sizes over all scopes equals total members (no member dropped,
	// none double-counted across scope rows).
	var sizeSum int
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(size),0) FROM graph_cluster_node`).Scan(&sizeSum); err != nil {
		t.Fatal(err)
	}
	if sizeSum != 6 {
		t.Errorf("sum(graph_cluster_node.size) = %d, want 6", sizeSum)
	}

	// DETERMINISM against a real DB: a second rebuild (TRUNCATE+INSERT) yields the
	// identical member→cluster assignment — proves the fixed seed + ORDER BY load.
	before := dumpMembers(t, pool)
	if _, err := overview.Rebuild(ctx, pool, 1.0); err != nil {
		t.Fatalf("rebuild #2: %v", err)
	}
	after := dumpMembers(t, pool)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("non-deterministic across rebuilds:\n  before=%v\n  after=%v", before, after)
	}
}
