//go:build integration

// Wave W-B (Cluster-Topic-Map, design/02 §7 "W-B") — the three root-map reads.
// Every number the map prints comes from here, and the map is a PERSISTED
// artefact: a scope leak in a response is one byte on the wire, a scope leak in
// the map is a database row that travels through backups, exports and into the
// context of every agent that loads it (design/02 §5.1).
//
//	go test -tags=integration ./internal/store/ -run RootMap -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// rmNode writes ONE graph_cluster_node partial row directly. Hand-written like
// c1Seed: this gate is about the READ predicates, and only hand-written rows can
// express the shapes a rebuild cannot currently produce (one cluster across two
// scopes, a meta row whose partition has vanished).
func rmNode(t *testing.T, pool *pgxpool.Pool, clusterID, scope string, size int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id, repr_title, repr_quality)
		 VALUES ($1::uuid, $2, $3, '{"learnings": 1}'::jsonb, $4::uuid, $5, 1.0)`,
		clusterID, scope, size, clusterID, "repr "+scope); err != nil {
		t.Fatalf("insert cluster node %s/%s: %v", clusterID, scope, err)
	}
}

// rmMeta writes ONE graph_overview_meta row with the migration-123 columns.
// computedAt nil = a partition that has never been built successfully.
func rmMeta(t *testing.T, pool *pgxpool.Pool, scope string, computedAt *time.Time,
	clusterN, nodeN, candidateN int, skipReason *string, modularity, resolution float64,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph_overview_meta
		     (scope, computed_at, last_attempt_at, skip_reason, candidate_n,
		      modularity, cluster_n, node_n, edge_n, resolution)
		 VALUES ($1, $2, COALESCE($2, now()), $3, $4, $5, $6, $7, 0, $8)`,
		scope, computedAt, skipReason, candidateN, modularity, clusterN, nodeN, resolution); err != nil {
		t.Fatalf("insert meta %s: %v", scope, err)
	}
}

const (
	rmClusterA1 = "019d2000-0000-7000-9000-0000000000a1" // private, size 7
	rmClusterA2 = "019d2000-0000-7000-9000-0000000000a2" // private, size 2 (small)
	rmClusterB1 = "019d2000-0000-7000-9000-0000000000b1" // work, size 40
	rmClusterX  = "019d2000-0000-7000-9000-0000000000c1" // spans private+shared
)

// TestRootMapTotalsAndMeta is W-B gates 1, 3 and the P0-merge follow-up gate on
// STALE meta rows. One container, four independent assertions:
//
//	(1) scope purity of OverviewTotals   — RED without `WHERE scope = ANY($1)`
//	    in the per-cluster CTE: {private} would count the work cluster (BP-1,
//	    foreign corpus size frozen into an own block).
//	(2) small-cluster split              — the collector line of §4.4c.
//	(3) meta aggregation is SUM, not max — RED against a max() version: {A,B}
//	    would report 10 instead of 15 candidates, and the coverage denominator
//	    of §4.4a hangs on exactly that number.
//	(4) STALE meta rows are neither coverage nor gap. The W-A teardown is
//	    restricted to scopes that still HAVE cluster rows
//	    (`DELETE … WHERE scope IN (SELECT DISTINCT scope FROM graph_cluster_node)`),
//	    so a scope that vanished from the corpus keeps a SUCCESS row with old
//	    cluster_n/node_n forever. Counting it would print phantom blocks as
//	    fresh coverage; treating it as a gap would escalate a scope that no
//	    longer exists. RED against a version without the liveness filter in the
//	    `live` CTE (the sums then carry the vanished partition).
func TestRootMapTotalsAndMeta(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// private: cluster A1 (7), A2 (2, small), X (3 of its 5 blocks)
	rmNode(t, pool, rmClusterA1, "private", 7)
	rmNode(t, pool, rmClusterA2, "private", 2)
	rmNode(t, pool, rmClusterX, "private", 3)
	// shared: the other half of X
	rmNode(t, pool, rmClusterX, "shared", 2)
	// work: one big foreign cluster
	rmNode(t, pool, rmClusterB1, "work", 40)

	built := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	older := built.Add(-24 * time.Hour)
	rmMeta(t, pool, "private", &built, 3, 12, 12, nil, 0.5, 1.0)
	rmMeta(t, pool, "shared", &older, 1, 2, 5, nil, 0.25, 1.0)
	rmMeta(t, pool, "work", &built, 1, 40, 40, nil, 0.9, 1.0)
	// 'gone' is the stale row: a successful build is recorded, but the
	// partition has no graph_cluster_node row any more.
	rmMeta(t, pool, "gone", &built, 4, 999, 1000, nil, 0.7, 1.0)

	// (1)+(2) totals, scope-pure.
	tot, err := store.OverviewTotals(ctx, pool, []string{"private"}, 2)
	if err != nil {
		t.Fatalf("OverviewTotals(private): %v", err)
	}
	if tot.ClusterTotal != 3 {
		t.Errorf("[private] ClusterTotal = %d, want 3 (work cluster leaked?)", tot.ClusterTotal)
	}
	if tot.ClusteredBlocks != 12 {
		t.Errorf("[private] ClusteredBlocks = %d, want 12 (7+2+3, shared half of X excluded)", tot.ClusteredBlocks)
	}
	if tot.SmallClusterN != 1 || tot.SmallClusterSize != 2 {
		t.Errorf("[private] small = (%d,%d), want (1,2)", tot.SmallClusterN, tot.SmallClusterSize)
	}

	// The visible size of X grows when shared joins — the small/large split is
	// computed AFTER the per-cluster scope sum, never per partial row.
	tot2, err := store.OverviewTotals(ctx, pool, []string{"private", "shared"}, 2)
	if err != nil {
		t.Fatalf("OverviewTotals(private,shared): %v", err)
	}
	if tot2.ClusterTotal != 3 || tot2.ClusteredBlocks != 14 {
		t.Errorf("[private,shared] = (%d clusters, %d blocks), want (3, 14)", tot2.ClusterTotal, tot2.ClusteredBlocks)
	}
	if tot2.SmallClusterN != 1 || tot2.SmallClusterSize != 2 {
		t.Errorf("[private,shared] small = (%d,%d), want (1,2) — X (5) must not be small", tot2.SmallClusterN, tot2.SmallClusterSize)
	}

	// (3) meta aggregation over scopes is SUM for the counts, max for the stamps.
	m1, err := store.OverviewMeta(ctx, pool, []string{"private"})
	if err != nil {
		t.Fatalf("OverviewMeta(private): %v", err)
	}
	if m1.CandidateN != 12 || m1.NodeN != 12 || m1.ClusterN != 3 {
		t.Errorf("[private] meta = (cand %d, node %d, cluster %d), want (12, 12, 3)", m1.CandidateN, m1.NodeN, m1.ClusterN)
	}
	m2, err := store.OverviewMeta(ctx, pool, []string{"private", "shared"})
	if err != nil {
		t.Fatalf("OverviewMeta(private,shared): %v", err)
	}
	if m2.CandidateN != 17 || m2.NodeN != 14 || m2.ClusterN != 4 {
		t.Errorf("[private,shared] meta = (cand %d, node %d, cluster %d), want (17, 14, 4) — max() instead of SUM?",
			m2.CandidateN, m2.NodeN, m2.ClusterN)
	}
	if m2.ComputedAt == nil || !m2.ComputedAt.Equal(built) {
		t.Errorf("[private,shared] ComputedAt = %v, want the NEWEST row %v", m2.ComputedAt, built)
	}
	if m2.SkipReason != "" {
		t.Errorf("[private,shared] SkipReason = %q, want empty", m2.SkipReason)
	}
	if m2.Modularity != 0.5 {
		t.Errorf("[private,shared] Modularity = %v, want 0.5 (newest row wins)", m2.Modularity)
	}

	// (4) the stale row is excluded from every sum and reported separately.
	m3, err := store.OverviewMeta(ctx, pool, []string{"private", "gone"})
	if err != nil {
		t.Fatalf("OverviewMeta(private,gone): %v", err)
	}
	if m3.CandidateN != 12 || m3.NodeN != 12 || m3.ClusterN != 3 {
		t.Errorf("stale row counted: meta = (cand %d, node %d, cluster %d), want (12, 12, 3)",
			m3.CandidateN, m3.NodeN, m3.ClusterN)
	}
	if len(m3.StaleScopes) != 1 || m3.StaleScopes[0] != "gone" {
		t.Errorf("StaleScopes = %v, want [gone]", m3.StaleScopes)
	}
	if len(m1.StaleScopes) != 0 {
		t.Errorf("[private] StaleScopes = %v, want none", m1.StaleScopes)
	}
}

// TestRootMapMetaEmptyPartitionAndCaps covers the two states the map must tell
// apart, plus the advisory-lock rule:
//
//	(a) "built, empty" — computed_at set, cluster_n = 0, no cluster rows. Live
//	    reality for scope='work' (design/02 §4.5 D4). NOT stale: nothing was
//	    lost, the partition simply has no clusters.
//	(b) "never built" — computed_at NULL (a pure W-A attempt stamp).
//	(c) advisory-lock is CONTENTION, not a freeze: it never wins over a real cap
//	    reason and is reported separately from the freeze reasons. RED against a
//	    version that returns any non-empty skip_reason as the cap.
func TestRootMapMetaEmptyPartitionAndCaps(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	built := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	lock := "advisory-lock"
	frozen := "node-cap"

	rmMeta(t, pool, "empty", &built, 0, 0, 0, nil, 0, 1.0)  // (a)
	rmMeta(t, pool, "fresh", nil, 0, 0, 3, &frozen, 0, 1.0) // (b) + freeze
	rmMeta(t, pool, "busy", &built, 0, 0, 9, &lock, 0, 1.0) // (c)

	m, err := store.OverviewMeta(ctx, pool, []string{"empty"})
	if err != nil {
		t.Fatalf("OverviewMeta(empty): %v", err)
	}
	if len(m.StaleScopes) != 0 {
		t.Errorf("built-but-empty partition flagged stale: %v", m.StaleScopes)
	}
	if m.ComputedAt == nil {
		t.Error("built-but-empty partition lost its computed_at")
	}

	m, err = store.OverviewMeta(ctx, pool, []string{"fresh"})
	if err != nil {
		t.Fatalf("OverviewMeta(fresh): %v", err)
	}
	if m.ComputedAt != nil {
		t.Errorf("never-built partition reports ComputedAt %v", m.ComputedAt)
	}
	if m.LastAttemptAt == nil {
		t.Error("attempt stamp lost: LastAttemptAt nil")
	}
	if m.SkipReason != "node-cap" {
		t.Errorf("SkipReason = %q, want node-cap", m.SkipReason)
	}
	if m.CandidateN != 3 {
		t.Errorf("CandidateN = %d, want 3", m.CandidateN)
	}

	m, err = store.OverviewMeta(ctx, pool, []string{"busy"})
	if err != nil {
		t.Fatalf("OverviewMeta(busy): %v", err)
	}
	if m.SkipReason != "" {
		t.Errorf("advisory-lock rendered as cap: SkipReason = %q, want empty", m.SkipReason)
	}
	if !m.Contended {
		t.Error("advisory-lock lost entirely: Contended = false")
	}

	// A real cap always beats contention, whatever the row order.
	m, err = store.OverviewMeta(ctx, pool, []string{"busy", "fresh"})
	if err != nil {
		t.Fatalf("OverviewMeta(busy,fresh): %v", err)
	}
	if m.SkipReason != "node-cap" || !m.Contended {
		t.Errorf("mixed caps: SkipReason = %q, Contended = %v, want (node-cap, true)", m.SkipReason, m.Contended)
	}
}

// TestRootMapActiveBlockCount is W-B gates 4 and 5 — the cap on the only NEW
// O(corpus) step of the axis.
//
// RED PROBE (gate 4), and the reason the assertion is phrased over a FILLED
// table: a version that sets the cap via `pool.Exec("SET LOCAL …")` in autocommit
// gets `WARNING: SET LOCAL can only be used in transaction blocks` from
// PostgreSQL and caps nothing. The Go sub-budget is deliberately the BACKSTOP
// (timeout + grace), so such a version runs the count to completion and returns
// ActiveKnown = true — the gate then fails, which a same-value Go deadline would
// have masked.
//
// Gate 5: after the cap fires the result is 0/false — never a pg_class.reltuples
// estimate, which can filter neither scope nor is_archived and would freeze the
// GLOBAL corpus size into a scope-owned block (BP-1).
func TestRootMapActiveBlockCount(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, is_archived)
		 SELECT 'learnings', 'blk ' || g, 'body', CASE WHEN g % 4 = 0 THEN 'work' ELSE 'private' END, g % 10 = 0
		   FROM generate_series(1, 60000) g`); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}

	n, known, err := store.ActiveBlockCount(ctx, pool, []string{"private"}, 30*time.Second)
	if err != nil {
		t.Fatalf("ActiveBlockCount: %v", err)
	}
	if !known {
		t.Fatal("ActiveKnown = false on a 30s budget")
	}
	var want int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_blocks WHERE scope = 'private' AND NOT is_archived`).Scan(&want); err != nil {
		t.Fatalf("reference count: %v", err)
	}
	if n != want {
		t.Errorf("ActiveBlockCount = %d, want %d (scope filter or is_archived filter missing)", n, want)
	}

	// The cap fires: 1ms of statement_timeout against 60k rows.
	n, known, err = store.ActiveBlockCount(ctx, pool, []string{"private"}, time.Millisecond)
	if err != nil {
		t.Fatalf("capped ActiveBlockCount returned an error instead of degrading: %v", err)
	}
	if known {
		t.Error("cap did not bite: ActiveKnown = true (SET LOCAL in autocommit?)")
	}
	if n != 0 {
		t.Errorf("capped count returned %d — an estimate leaked in (BP-1)", n)
	}

	// The pool survives the cap: a capped call must not leave a
	// statement_timeout behind on the pooled connection (that is what a `SET`
	// without `LOCAL` would do, capping unrelated queries at random).
	if _, known, err = store.ActiveBlockCount(ctx, pool, []string{"private"}, 30*time.Second); err != nil || !known {
		t.Errorf("after the capped call: known = %v, err = %v — timeout leaked onto a pooled connection", known, err)
	}
}

// TestRootMapEdgeLimitZeroShortCircuit is W-B gate 7: with EdgeLimit = 0 the
// root map never touches graph_cluster_edge. Proven structurally — the table is
// renamed away, so any access raises 42P01 (undefined_table).
//
// Gate 6 (wire identity) is the counterpart in internal/handler: the parser
// enforces edge_limit >= 1, so this short circuit is unreachable from
// GET /api/graph/overview and buildOverviewResponse's golden test stays green.
func TestRootMapEdgeLimitZeroShortCircuit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	rmNode(t, pool, rmClusterA1, "private", 7)
	if _, err := pool.Exec(ctx, `ALTER TABLE graph_cluster_edge RENAME TO graph_cluster_edge_gone`); err != nil {
		t.Fatalf("rename edge table: %v", err)
	}

	res, err := store.GraphOverview(ctx, pool,
		store.OverviewParams{MinClusterSize: 1, NodeLimit: 10, EdgeLimit: 0}, []string{"private"})
	if err != nil {
		t.Fatalf("EdgeLimit=0 touched graph_cluster_edge: %v", err)
	}
	if len(res.Nodes) != 1 || len(res.Edges) != 0 {
		t.Errorf("EdgeLimit=0: got %d nodes / %d edges, want 1 / 0", len(res.Nodes), len(res.Edges))
	}

	// Counter-probe: with a positive EdgeLimit the very same call DOES touch the
	// table — which is what makes the assertion above evidence rather than luck.
	if _, err := store.GraphOverview(ctx, pool,
		store.OverviewParams{MinClusterSize: 1, NodeLimit: 10, EdgeLimit: 1}, []string{"private"}); err == nil {
		t.Error("EdgeLimit=1 did not touch graph_cluster_edge — the short-circuit probe proves nothing")
	}
}
