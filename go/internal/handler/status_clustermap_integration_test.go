//go:build integration

// Wave C4 (Cluster-Topic-Map, design/03 §4.6 + masterplan K2 / amendment A01-1)
// — the /api/status cluster_map section.
//
// Two things it has to carry, for two different questions:
//
//   - per-scope liveness (migration 123: computed_at / last_attempt_at /
//     skip_reason / candidate_n). The cluster_stale trip says "the retrieval
//     signal is off"; only this says whether the MAP is still being built. A
//     reproducibly timing-out rebuild and a correctly fail-safed feature look
//     identical from the trip alone.
//   - the K2 MONITOR: how many clusters span more than one scope. Topic identity
//     is scope-BOUND by decision K2, live measured at 0 of 59. This number turns
//     that invariant from an assumption into an observation.
//
//	go test -tags=integration ./internal/handler/ -run TestStatusClusterMap -count=1 -v
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func c4Collector(t *testing.T, pool *pgxpool.Pool) *StatusCollector {
	t.Helper()
	return NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{},
		config.NewStore(&config.Config{}), nil, nil)
}

func c4StatusMeta(t *testing.T, pool *pgxpool.Pool, scope string, computedAt *time.Time, skipReason string, candidates int) {
	t.Helper()
	var reason any
	if skipReason != "" {
		reason = skipReason
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, node_n, edge_n, cluster_n, modularity, skip_reason, candidate_n)
		VALUES ($1, $2, now(), 1, 0, 1, 0, $3, $4)`,
		scope, computedAt, reason, candidates); err != nil {
		t.Fatalf("meta %s: %v", scope, err)
	}
}

func c4ClusterNode(t *testing.T, pool *pgxpool.Pool, clusterID, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts)
		VALUES ($1::uuid, $2::text, 1, $1::uuid, 'repr', 1, '{"learnings":1}'::jsonb)`,
		clusterID, scope); err != nil {
		t.Fatalf("cluster node %s/%s: %v", clusterID, scope, err)
	}
}

// The section renders the liveness of every partition, distinguishes "never
// successfully built" from "0 ms old", and carries the failure counter through.
func TestStatusClusterMapLiveness(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	built := time.Now().Add(-90 * time.Minute)
	c4StatusMeta(t, pool, "private", &built, "", 1200)
	// A partition that has ATTEMPTED but never SUCCEEDED — the fresh-deploy-over-
	// the-cap shape migration 123 made representable.
	c4StatusMeta(t, pool, "work", nil, "node-cap", 400000)

	got := c4Collector(t, pool).buildClusterMapStatus(context.Background(), 3)
	if got == nil {
		t.Fatal("cluster_map section must be present when the read succeeds")
	}
	if got.ConsecutiveFailures != 3 {
		t.Errorf("consecutive_failures = %d, want 3", got.ConsecutiveFailures)
	}
	if len(got.Scopes) != 2 {
		t.Fatalf("scopes = %d, want 2: %+v", len(got.Scopes), got.Scopes)
	}
	priv, work := got.Scopes[0], got.Scopes[1] // ORDER BY scope
	if priv.Scope != "private" || work.Scope != "work" {
		t.Fatalf("scope order = %s, %s — want deterministic ORDER BY scope", priv.Scope, work.Scope)
	}
	if priv.SkipReason != "" {
		t.Errorf("private skip_reason = %q, want empty (last attempt succeeded)", priv.SkipReason)
	}
	if priv.StalenessMs < 60*60*1000 {
		t.Errorf("private staleness_ms = %d, want >= one hour", priv.StalenessMs)
	}
	if priv.CandidateN != 1200 {
		t.Errorf("private candidate_n = %d, want 1200", priv.CandidateN)
	}
	// -1, NOT 0: "never successfully built" and "built just now" are different
	// statements and must not collapse into the same number.
	if work.ComputedAt != nil || work.StalenessMs != -1 {
		t.Errorf("work computed_at=%v staleness_ms=%d, want nil / -1", work.ComputedAt, work.StalenessMs)
	}
	if work.SkipReason != "node-cap" {
		t.Errorf("work skip_reason = %q, want node-cap", work.SkipReason)
	}
}

// K2 / A01-1 MONITOR. Live expectation is 0 — topic identity is scope-bound, so
// a cluster whose partitions live in two scopes means a handle and its aggregate
// have started describing different objects.
//
// ROT-PROBE: drop the `HAVING count(DISTINCT scope) > 1` (or count clusters
// instead of spanning ones) ⇒ the clean fixture reports 2 instead of 0, and the
// monitor stops distinguishing the very thing it exists for.
func TestStatusClusterMapCrossScopeMonitor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	built := time.Now()
	c4StatusMeta(t, pool, "private", &built, "", 10)

	const (
		clean    = "019da000-0000-7000-9000-00000000c001"
		clean2   = "019da000-0000-7000-9000-00000000c002"
		spanning = "019da000-0000-7000-9000-00000000c003"
	)
	// The live shape: every cluster lives in exactly one scope.
	c4ClusterNode(t, pool, clean, "private")
	c4ClusterNode(t, pool, clean2, "work")

	got := c4Collector(t, pool).buildClusterMapStatus(ctx, 0)
	if got == nil {
		t.Fatal("section missing")
	}
	if got.CrossScopeClusters != 0 {
		t.Fatalf("cross_scope_clusters = %d on a scope-pure map, want 0", got.CrossScopeClusters)
	}

	// Now the shape the invariant forbids.
	c4ClusterNode(t, pool, spanning, "private")
	c4ClusterNode(t, pool, spanning, "work")

	got = c4Collector(t, pool).buildClusterMapStatus(ctx, 0)
	if got.CrossScopeClusters != 1 {
		t.Fatalf("cross_scope_clusters = %d after seeding ONE spanning cluster, want 1", got.CrossScopeClusters)
	}
}
