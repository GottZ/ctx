//go:build integration

// Wave C1 (Cluster-Topic-Map, design/03 §4.1/§5.2 + §7 "C1") — the security
// gate of the whole cluster-consumption axis, risk R3 of the masterplan.
//
// graph_cluster_member carries NO visibility logic of its own: block_id is the
// SOLE primary key, cluster_id has no foreign key, and the only partition
// information is the scope column (migration 087). A membership join without
// `AND scope = ANY(readScopes)` therefore hands out the cluster affiliation of
// FOREIGN-PRIVATE blocks — and with it a side channel on foreign community
// structure: an attacker holding a single block grant on a foreign block sees
// its cluster_id and can test which of its OWN blocks carry the same value,
// reconstructing foreign community boundaries without ever reading a foreign
// block.
//
// The countermeasure is structural, not incidental: the conjunction exists
// EXACTLY ONCE, in clustersql.MemberOf, and every cluster read of this axis
// (RRF boost, ego annotation, cluster route, facet) binds it through that one
// function. This test is the negative probe on that single point.
//
//	go test -tags=integration ./internal/store/ -run TestClusterMembership -count=1 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/clustersql"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const c1Cluster = "019d1000-0000-7000-9000-00000000c001"

// c1Seed inserts one block plus its membership row. The membership row is
// written DIRECTLY (not through a rebuild): this gate is about the READ
// predicate, and a hand-written row is the only way to also cover the shapes a
// rebuild cannot currently produce — e.g. two scopes inside one cluster.
func c1Seed(t *testing.T, pool *pgxpool.Pool, id, scope, clusterID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid, 'learnings', $2, 'c1 fixture', $3)`,
		id, "blk-"+id[len(id)-4:], scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		id, clusterID, scope); err != nil {
		t.Fatalf("insert member %s: %v", id, err)
	}
}

// TestClusterMembershipScopePurity is gate (i): a caller who may read `work`
// never learns that P (private) sits in the same cluster as Q (work) — even
// though P's block id is handed to the function explicitly, which is the
// strongest form of the ask (the attacker already knows the id).
//
// RED PROBE: drop clustersql.MemberOf from clustersql.MembershipQuery ⇒ P's row
// comes back and this test fails. Because every consumer of the axis reads
// through this ONE query, that single edit reddens all of them.
func TestClusterMembershipScopePurity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		p = "019d1000-0000-7000-9000-000000000001" // scope private
		q = "019d1000-0000-7000-9000-000000000002" // scope work
	)
	c1Seed(t, pool, p, "private", c1Cluster)
	c1Seed(t, pool, q, "work", c1Cluster)

	got, err := store.ClusterMembership(ctx, pool, []string{p, q}, []string{"work"})
	if err != nil {
		t.Fatalf("ClusterMembership: %v", err)
	}
	if _, leaked := got[p]; leaked {
		t.Errorf("private block %s leaked its cluster to a work-only reader: %v — "+
			"an unscoped membership join makes foreign community boundaries reconstructible (R3)", p, got)
	}
	if got[q] != c1Cluster {
		t.Errorf("visible block %s: cluster = %q, want %q", q, got[q], c1Cluster)
	}
	if len(got) != 1 {
		t.Errorf("membership map has %d entries, want exactly 1 (the visible one): %v", len(got), got)
	}
}

// TestClusterMembershipFailsClosedOnEmptyScopes is gate (ii): an empty scope
// set is an ERROR, never silently "all scopes". Without the guard PostgreSQL
// would evaluate `scope = ANY('{}')` as a deterministic FALSE and the function
// would return an EMPTY MAP — which looks identical to "nothing found" and so
// hides a resolver bug instead of surfacing it. RequireScopes must therefore be
// the FIRST statement, before any query runs.
//
// RED PROBE: delete the RequireScopes call ⇒ the call returns (empty map, nil)
// and this test fails on the missing ErrNoScopes.
func TestClusterMembershipFailsClosedOnEmptyScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const p = "019d1000-0000-7000-9000-000000000003"
	c1Seed(t, pool, p, "private", c1Cluster)

	got, err := store.ClusterMembership(ctx, pool, []string{p}, nil)
	if !errors.Is(err, store.ErrNoScopes) {
		t.Errorf("empty readScopes: err = %v, want ErrNoScopes (fail-closed, not an empty map)", err)
	}
	if got != nil {
		t.Errorf("empty readScopes: result = %v, want nil — a map is a usable answer, an error is not", got)
	}
}

// TestClusterMembershipQueryBindsTheOneConjunction pins the structural claim
// itself: the batch query does not merely happen to filter by scope, it
// contains the fragment MemberOf produced. That is what makes "the conjunction
// exists exactly once" a checkable property rather than a review note — a
// hand-written copy of the predicate in the query string fails here.
func TestClusterMembershipQueryBindsTheOneConjunction(t *testing.T) {
	want := clustersql.MemberOf("m", "$2")
	if !strings.Contains(clustersql.MembershipQuery, want) {
		t.Errorf("MembershipQuery does not embed MemberOf(%q, %q) = %q:\n%s",
			"m", "$2", want, clustersql.MembershipQuery)
	}
	if strings.Count(clustersql.MembershipQuery, ".scope") != 1 {
		t.Errorf("MembershipQuery mentions .scope %d times, want exactly 1 (one definition, one site):\n%s",
			strings.Count(clustersql.MembershipQuery, ".scope"), clustersql.MembershipQuery)
	}
}
