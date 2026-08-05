//go:build integration

// Wave C2 (Cluster-Topic-Map, design/03 §4.2/§5.6 + §7 "C2") — the STORE half of
// the ego cluster annotation. Two properties that only a real database can show:
//
//	(i)  SCOPE PURITY of the size: a cluster whose members live in two scopes,
//	     one of them invisible, must report ONLY the visible partition's size.
//	     Taking a single graph_cluster_node row's size — or summing without the
//	     scope conjunction — is a direct count leak over foreign partitions, the
//	     exact vector the scope-partitioned precomputation exists to close (§5.6).
//	(iv) NO TRUNCATION: this path hands ordinals to a POSITIONAL wire array, so a
//	     dropped cluster entry would leave cluster_of[i] pointing at nothing. The
//	     landkarte's own aggregate would cut at node_limit=500 (handler/
//	     overview.go) — inheriting those defaults here is the "hängender Index"
//	     bug (Linse 2 / B10), and the fixture below is deliberately larger than 500.
//
//	go test -tags=integration ./internal/store/ -run TestClusterAnnotation -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// c2Node writes one graph_cluster_node partition row (cluster_id, scope). Rows
// are hand-written rather than rebuilt for the same reason as in the C1 gate: a
// rebuild cannot currently produce a cluster spanning two scopes, and that is
// precisely the shape under test.
func c2Node(t *testing.T, pool *pgxpool.Pool, clusterID, scope string, size int, cat string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts)
		 VALUES ($1::uuid, $2::text, $3::int, $1::uuid, 'repr', 1, jsonb_build_object($4::text, $3::int))`,
		clusterID, scope, size, cat); err != nil {
		t.Fatalf("insert node %s/%s: %v", clusterID, scope, err)
	}
}

// c2Member writes a block plus its membership row in one go.
func c2Member(t *testing.T, pool *pgxpool.Pool, id, scope, clusterID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid, 'learnings', $2, 'c2 fixture', $3)`,
		id, "blk-"+id, scope); err != nil { // full id: (category,title,scope) is UNIQUE
		t.Fatalf("insert block %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		id, clusterID, scope); err != nil {
		t.Fatalf("insert member %s: %v", id, err)
	}
}

// Gate (i): the size of a cluster spanning `private` and `work` is the sum over
// the caller's VISIBLE partitions only, and scope_mix names exactly those.
//
// ROT-PROBE: remove the scope conjunction from clustersql.VisibleSizeQuery
// (drop the `AND n.scope = ANY($2::text[])`, i.e. NodeVisible) ⇒ size becomes
// 30 instead of 10 and scope_mix carries the foreign scope ⇒ this test goes red.
// The same edit reddens the C1 gate and the overview scope tests — that is the
// point of having ONE site for the conjunction.
func TestClusterAnnotationScopePurity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const cluster = "019d2000-0000-7000-9000-00000000c001"
	const blockPrivate = "019d2000-0000-7000-9000-0000000000a1"
	const blockWork = "019d2000-0000-7000-9000-0000000000a2"

	c2Node(t, pool, cluster, "private", 10, "learnings")
	c2Node(t, pool, cluster, "work", 20, "decisions")
	c2Member(t, pool, blockPrivate, "private", cluster)
	c2Member(t, pool, blockWork, "work", cluster)

	ann, err := store.ClusterAnnotation(ctx, pool, []string{blockPrivate, blockWork}, []string{"private"})
	if err != nil {
		t.Fatalf("ClusterAnnotation: %v", err)
	}
	if len(ann.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1: %+v", len(ann.Clusters), ann.Clusters)
	}
	got := ann.Clusters[0]
	if got.Size != 10 {
		t.Errorf("size = %d, want 10 (ONLY the visible partition — 30 means the scope conjunction is gone)", got.Size)
	}
	if len(got.ScopeMix) != 1 || got.ScopeMix[0] != "private" {
		t.Errorf("scope_mix = %v, want [private]", got.ScopeMix)
	}
	// The work-scoped block has no VISIBLE membership row (C1 conjunction), so it
	// is absent from MemberOf entirely and the handler renders it as -1.
	if _, ok := ann.MemberOf[blockWork]; ok {
		t.Errorf("foreign-scoped block appeared in MemberOf: %v", ann.MemberOf)
	}
	if ann.MemberOf[blockPrivate] != cluster {
		t.Errorf("MemberOf[private block] = %q, want %q", ann.MemberOf[blockPrivate], cluster)
	}
	// in_response counts the PASSED blocks in this cluster — the invisible one
	// must not be counted either.
	if got.InResponse != 1 {
		t.Errorf("in_response = %d, want 1", got.InResponse)
	}
	if len(got.TopCategories) != 1 || got.TopCategories[0] != "learnings" {
		t.Errorf("top_categories = %v, want [learnings] (decisions is the foreign partition)", got.TopCategories)
	}
}

// Gate (iv): 600 distinct clusters — more than overviewDefaultNodeLimit (500) —
// all come back, so every ordinal the handler assigns resolves.
//
// ROT-PROBE: give clustersql.VisibleSizeQuery the landkarte's defaults
// (`HAVING sum(n.size) >= 1 ORDER BY sum(n.size) DESC, n.cluster_id LIMIT 500`)
// ⇒ 100 clusters vanish while their blocks still carry membership rows ⇒ the
// count assert below goes red. That is exactly the dangling-index bug, made
// visible before it can reach a client.
func TestClusterAnnotationNoTruncation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const n = 600
	blockIDs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		cluster := fmt.Sprintf("019d3000-0000-7000-9000-%012x", i)
		block := fmt.Sprintf("019d4000-0000-7000-9000-%012x", i)
		c2Node(t, pool, cluster, "private", 1, "learnings")
		c2Member(t, pool, block, "private", cluster)
		blockIDs = append(blockIDs, block)
	}

	ann, err := store.ClusterAnnotation(ctx, pool, blockIDs, []string{"private"})
	if err != nil {
		t.Fatalf("ClusterAnnotation: %v", err)
	}
	if len(ann.Clusters) != n {
		t.Fatalf("clusters = %d, want %d — a limit on this path is a dangling ordinal", len(ann.Clusters), n)
	}
	seen := make(map[string]bool, n)
	for _, c := range ann.Clusters {
		seen[c.ClusterID] = true
	}
	for _, b := range blockIDs {
		if cid := ann.MemberOf[b]; !seen[cid] {
			t.Fatalf("block %s maps to cluster %s which is absent from clusters[]", b, cid)
		}
	}
}

// Fail-closed: no resolved scopes is an ERROR, never an empty map. PostgreSQL
// evaluates `scope = ANY('{}')` as a deterministic FALSE, so a resolver bug
// would otherwise hide as a quiet loss of annotation.
//
// ROT-PROBE: drop the RequireScopes call at the head of ClusterMembership ⇒ the
// call returns an empty annotation and no error ⇒ red.
func TestClusterAnnotationRequiresScopes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	if _, err := store.ClusterAnnotation(context.Background(), pool, []string{"019d2000-0000-7000-9000-0000000000a1"}, nil); err == nil {
		t.Fatal("ClusterAnnotation with no read scopes must fail closed, got nil error")
	}
}
