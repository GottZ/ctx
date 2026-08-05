//go:build integration

// Wave C4 (Cluster-Topic-Map, design/03 §4.6/§4.7 + §7 "C4") — the SCHEDULER
// half of the staleness seam: the two gates that only a real rebuild can show.
//
//	(iv) the arm stamp is NOT a freshness source. LastArmRuns advances on a
//	     SKIPPED rebuild while the map stays exactly as frozen as it was; a gate
//	     built on it would keep boosting from a map that is not being rebuilt.
//	(vi) MULTI-INSTANCE: an instance that only ever sees Skipped:"advisory-lock"
//	     — because another instance is doing the work, and doing it well — must
//	     NOT switch its stage off. Refresh trigger three (lazy TTL) is what makes
//	     that true; without it the feature would be dead on every non-building
//	     instance after max_staleness.
//
//	go test -tags=integration ./internal/events/ -run TestClusterFreshness -count=1 -v
package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// c4Config enables the overview arm with a node cap the fixture deliberately
// exceeds, so rebuildOverviewOnce takes the node-cap SKIP exit.
func c4Config(maxNodes int, staleness time.Duration) *config.Config {
	c := &config.Config{}
	c.GraphOverview.Enabled = true
	c.GraphOverview.Resolution = 1.0
	c.GraphOverview.RebuildTimeout = time.Minute
	c.GraphOverview.MaxNodes = maxNodes
	c.ClusterOps.MaxStaleness = staleness
	return c
}

// c4Meta writes a meta row directly — the shape another instance's successful
// rebuild leaves behind.
func c4Meta(t *testing.T, pool *pgxpool.Pool, scope string, computedAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, node_n, edge_n, cluster_n, modularity, candidate_n)
		VALUES ($1, $2, $2, 1, 0, 1, 0, 1)
		ON CONFLICT (scope) DO UPDATE SET computed_at = EXCLUDED.computed_at,
		                                  last_attempt_at = EXCLUDED.last_attempt_at,
		                                  skip_reason = NULL`,
		scope, computedAt); err != nil {
		t.Fatalf("c4Meta %s: %v", scope, err)
	}
}

// Gate (iv): THE ARM STAMP IS NOT A FRESHNESS SOURCE — pinned in code, not just
// in a design comment. A node-cap-skipped rebuild moves LastArmRuns' overview
// stamp forward; the cluster map's own timestamp does not budge.
//
// ROT-PROBE: implement ClusterMapComputedAt from LastArmRuns instead of
// graph_overview_meta ⇒ the freshness value walks forward on every skipped
// attempt, the assert below fails, and in production the stage would boost
// forever from a map that stopped being rebuilt.
func TestClusterFreshnessArmStampIsNotFreshness(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}

	// A map built three days ago, then blocks pushing the rebuild over the cap.
	built := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Millisecond)
	c4Meta(t, pool, "private", built)
	for i := 0; i < 3; i++ {
		stampBlock(t, pool, c4BlockID(i), "private", "c4-skip-"+c4BlockID(i))
	}

	s := stampScheduler(t, pool, c4Config(1, 24*time.Hour)) // max_nodes 1 ⇒ node-cap skip
	s.refreshClusterFreshness(ctx)

	before, ok := s.ClusterMapComputedAt([]string{"private"})
	if !ok {
		t.Fatal("seeded meta row must be readable")
	}
	_, _, armBefore := s.LastArmRuns()

	s.rebuildOverviewOnce(ctx, bt)

	_, _, armAfter := s.LastArmRuns()
	if !armAfter.After(armBefore) {
		t.Fatal("fixture broken: the skipped rebuild did not move the arm stamp — nothing to distinguish")
	}
	s.refreshClusterFreshness(ctx) // even an EXPLICIT refresh must not invent freshness

	after, ok := s.ClusterMapComputedAt([]string{"private"})
	if !ok {
		t.Fatal("meta row disappeared")
	}
	if !after.Equal(before) {
		t.Fatalf("cluster map freshness moved on a SKIPPED rebuild: %v → %v", before, after)
	}
	if got := s.ConsecutiveOverviewFails(); got != 1 {
		t.Errorf("consecutive_failures = %d, want 1 — the skip must be countable", got)
	}
	// And the skip is on record with its reason (migration 123), which is what
	// tells "signal off" apart from "map broken" on /api/status.
	r := readStamp(t, pool, "private")
	if r.skipReason == nil || *r.skipReason != "node-cap" {
		t.Errorf("skip_reason = %v, want node-cap", r.skipReason)
	}
}

// Gate (vi): MULTI-INSTANCE. This instance never builds anything — every attempt
// comes back skipped — while ANOTHER instance keeps the map fresh. The stage
// must stay ACTIVE, and it does because of refresh trigger three (lazy TTL,
// max_staleness/4).
//
// ROT-PROBE: refresh only after a rebuild and at boot (drop
// maybeRefreshClusterFreshness) ⇒ the process-local map freezes at its boot
// value, the stage switches itself off after max_staleness, and the cluster_stale
// trip tells a false story about a perfectly fresh landkarte. The assert below
// fails on the SECOND read.
func TestClusterFreshnessLazyRefreshSeesForeignRebuild(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}

	old := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Millisecond)
	c4Meta(t, pool, "private", old)
	for i := 0; i < 3; i++ {
		stampBlock(t, pool, c4BlockID(i), "private", "c4-lazy-"+c4BlockID(i))
	}

	// max_staleness 4ms ⇒ TTL 1ms: the lazy refresh is due on the second read.
	s := stampScheduler(t, pool, c4Config(1, 4*time.Millisecond))
	s.refreshClusterFreshness(ctx)

	// This instance only ever skips (node cap) — it never refreshes on success.
	s.rebuildOverviewOnce(ctx, bt)
	if got := s.ConsecutiveOverviewFails(); got == 0 {
		t.Fatal("fixture broken: this instance was supposed to skip")
	}

	// The OTHER instance rebuilds successfully.
	fresh := time.Now().UTC().Truncate(time.Millisecond)
	c4Meta(t, pool, "private", fresh)

	time.Sleep(10 * time.Millisecond) // past the TTL
	got, ok := s.ClusterMapComputedAt([]string{"private"})
	if !ok {
		t.Fatal("freshness lost after a foreign rebuild")
	}
	if !got.Equal(fresh) {
		t.Fatalf("lazy refresh did not pick up the foreign rebuild: got %v, want %v", got, fresh)
	}
}

// Per-scope MINIMUM, at the scheduler: two scopes, one fresh, one never built ⇒
// !ok for the pair, still ok for the fresh one alone. A missing partition
// disqualifies the read SET, not the feature.
//
// ROT-PROBE: aggregate with max (the landkarte display convention) or skip
// unknown scopes ⇒ the pair reads as fresh and the frozen partition is invisible.
func TestClusterFreshnessPerScopeMinimum(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	older := now.Add(-3 * time.Hour)
	c4Meta(t, pool, "private", now)
	c4Meta(t, pool, "work", older)

	s := stampScheduler(t, pool, c4Config(200000, 24*time.Hour))
	s.refreshClusterFreshness(ctx)

	got, ok := s.ClusterMapComputedAt([]string{"private", "work"})
	if !ok {
		t.Fatal("both partitions exist — the pair must resolve")
	}
	if !got.Equal(older) {
		t.Fatalf("aggregate = %v, want the OLDER partition %v (min, not max)", got, older)
	}
	if _, ok := s.ClusterMapComputedAt([]string{"private", "never-built"}); ok {
		t.Fatal("an unbuilt partition must disqualify the read set")
	}
	if _, ok := s.ClusterMapComputedAt(nil); ok {
		t.Fatal("an empty scope set must never read as fresh")
	}
}

func c4BlockID(i int) string {
	const digits = "0123456789abcdef"
	return "019d9000-0000-7000-9000-00000000000" + string(digits[i])
}
