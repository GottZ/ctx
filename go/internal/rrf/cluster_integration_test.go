//go:build integration

// Wave C3 (Cluster-Topic-Map, design/03 §4.5/§5.3 + §7 "C3") — the half that
// needs a database: the pausability A/B through the real read, the grant-leaf
// guarantee, fail-open, and the latency gate.
//
//	go test -tags=integration ./internal/rrf/ -run TestClusterBoost -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

func c3Cfg() rrf.ClusterConfig {
	return rrf.ClusterConfig{
		Enabled: true, SeedCount: 10, TopClusters: 2,
		MinShare: 0.25, BoostWeight: 0.12, SizeDamping: true,
		MaxStaleness: 24 * time.Hour,
	}
}

// c3Fresh is the C4 freshness seam as a test double: one map, one age. The
// PRODUCTION seam is *events.Scheduler; its own gates (per-scope minimum, the
// three refresh triggers) live in internal/events.
type c3Fresh struct{ at time.Time }

func (f c3Fresh) ClusterMapComputedAt([]string) (time.Time, bool) {
	return f.at, !f.at.IsZero()
}

// c3Now is a map rebuilt a minute ago — the normal state.
func c3Now() c3Fresh { return c3Fresh{at: time.Now().Add(-time.Minute)} }

// c3Seed writes a block, its membership row and (once per cluster/scope) the
// aggregate partition row the size damping reads.
func c3Seed(t *testing.T, pool *pgxpool.Pool, id, scope, clusterID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid, 'learnings', $2, 'c3 fixture', $3)`,
		id, "c3-"+id, scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
		 VALUES ($1::uuid, $2::uuid, $3)`,
		id, clusterID, scope); err != nil {
		t.Fatalf("insert member %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts)
		 VALUES ($1::uuid, $2::text, 1, $1::uuid, 'repr', 1, '{"learnings":1}'::jsonb)
		 ON CONFLICT (cluster_id, scope) DO UPDATE SET size = graph_cluster_node.size + 1`,
		clusterID, scope); err != nil {
		t.Fatalf("upsert node %s/%s: %v", clusterID, scope, err)
	}
}

func c3BlockID(i int) string   { return fmt.Sprintf("019d7000-0000-7000-9000-%012x", i) }
func c3ClusterID(i int) string { return fmt.Sprintf("019d8000-0000-7000-9000-%012x", i) }

// c3Results builds a descending result slice over the seeded blocks.
func c3Results(n int, scope string) []rrf.SearchResult {
	out := make([]rrf.SearchResult, n)
	for i := range out {
		out[i] = rrf.SearchResult{ID: c3BlockID(i), Scope: scope, RRFScore: 1.0 - float64(i)*0.01}
	}
	return out
}

// c3EqualRanking compares what gate (i) names: ids, ORDER and scores.
func c3EqualRanking(t *testing.T, got, want []rrf.SearchResult, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len %d, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("%s: order drift at %d: %s, want %s", what, i, got[i].ID, want[i].ID)
		}
		if got[i].RRFScore != want[i].RRFScore {
			t.Fatalf("%s: score drift at %d: %v, want %v", what, i, got[i].RRFScore, want[i].RRFScore)
		}
	}
}

// c3Equal is c3EqualRanking plus the provenance field — the shape a caller sees
// when the stage did not touch the results AT ALL (off, fail-open, no winner).
func c3Equal(t *testing.T, got, want []rrf.SearchResult, what string) {
	t.Helper()
	c3EqualRanking(t, got, want, what)
	for i := range want {
		if got[i].ClusterBoost != want[i].ClusterBoost {
			t.Fatalf("%s: provenance drift at %d: %v, want %v", what, i, got[i].ClusterBoost, want[i].ClusterBoost)
		}
	}
}

// Gate (i): PAUSABILITY as a deterministic A/B through the real read. Over a
// fixed result set, ids, ORDER and scores are bit-identical between "stage off"
// and "stage on with boost_weight 0" — the second arm proves the equality is the
// stage's own doing and not an accidentally skipped code path (the DB read and
// the fusion both run, only the multiplier is neutral).
//
// ROT-PROBE: let the stage run unconditionally (drop the `!cfg.Enabled` arm) with
// a non-zero weight ⇒ the off-arm comparison fails. Recorded below as the third
// arm: the same call with the live default weight MUST differ, otherwise the
// fixture proves nothing.
func TestClusterBoostPausableAB(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	in := c3Results(12, "private")
	scopes := []string{"private"}

	off := c3Cfg()
	off.Enabled = false
	gotOff, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, off, c3Now())
	if err != nil {
		t.Fatalf("stage off: %v", err)
	}
	c3Equal(t, gotOff, in, "stage off")

	zero := c3Cfg()
	zero.BoostWeight = 0
	gotZero, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, zero, c3Now())
	if err != nil {
		t.Fatalf("stage armed, weight 0: %v", err)
	}
	// Ranking is bit-identical — gate (i). The PROVENANCE deliberately is not:
	// with weight 0 the read and the fusion both ran and recorded the share they
	// computed, which is what makes this arm an A/B of the multiplier rather than
	// an accidentally skipped code path. cluster.enabled=false (the deployed
	// default, arm one above) is the arm that touches nothing at all.
	c3EqualRanking(t, gotZero, in, "stage armed, weight 0")
	ran := false
	for i := range gotZero {
		if gotZero[i].ClusterBoost != 0 {
			ran = true
			break
		}
	}
	if !ran {
		t.Fatal("weight-0 arm recorded no share — it skipped the stage instead of neutralising it")
	}

	// Control arm: with the live default weight the stage MUST move something,
	// otherwise the two comparisons above are vacuous.
	gotLive, rep, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, c3Cfg(), c3Now())
	if err != nil {
		t.Fatalf("stage armed, live weight: %v", err)
	}
	moved := false
	for i := range gotLive {
		if gotLive[i].RRFScore != in[i].RRFScore || gotLive[i].ClusterBoost != 0 {
			moved = true
			break
		}
	}
	if !moved {
		t.Fatal("control arm changed nothing — the fixture does not exercise the boost")
	}
	if rep == nil {
		t.Fatal("stage returned no telemetry report")
	}
	// Invariant (ii) also holds against the DB: same ids, same count.
	if len(gotLive) != len(in) {
		t.Fatalf("len drift: %d vs %d", len(gotLive), len(in))
	}
	seen := map[string]bool{}
	for _, r := range gotLive {
		seen[r.ID] = true
	}
	for _, r := range in {
		if !seen[r.ID] {
			t.Fatalf("result %s disappeared", r.ID)
		}
	}
}

// Gate (vii): a GRANT-ONLY result casts no cluster vote (§5.3). Its scope is not
// in readScopes, so the C1 scope conjunction already dropped its membership row —
// structural, not a special case in the stage. Without this, a single shared
// block would drag the grantee's OWN blocks up over the grant bridge, the exact
// mechanic T41 protects the graph seeds from.
//
// ROT-PROBE: remove clustersql.MemberOf from MembershipQuery ⇒ the work-scoped
// block votes, its cluster wins, and the assert below fails.
func TestClusterBoostGrantOnlyResultCastsNoVote(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Ranks 0..4 are grant-visible blocks (scope `work`, NOT in readScopes), all
	// in cluster 0 — a DECISIVE bloc: unfiltered they carry ~52 % of the vote and
	// cluster 0 would win outright. Ranks 5..9 are unclustered private blocks, so
	// nothing legitimate can win instead and any movement is the leak.
	for i := 0; i < 5; i++ {
		c3Seed(t, pool, c3BlockID(i), "work", c3ClusterID(0))
	}
	for i := 5; i < 10; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope)
			 VALUES ($1::uuid, 'learnings', $2, 'c3 fixture', 'private')`,
			c3BlockID(i), "c3-"+c3BlockID(i)); err != nil {
			t.Fatalf("insert block: %v", err)
		}
	}

	in := c3Results(10, "private")
	for i := 0; i < 5; i++ {
		in[i].Scope = "work" // grant-visible: delivered, but outside readScopes
	}

	got, _, err := rrf.ClusterBoost(ctx, pool, in, nil, []string{"private"}, c3Cfg(), c3Now())
	if err != nil {
		t.Fatalf("ClusterBoost: %v", err)
	}
	c3Equal(t, got, in, "grant-only seed must not create a winner")
}

// C4 gate (i), the NEGATIVE PROBE at stage level: with the identical fixture
// that boosts under a fresh map, a map computed seven days ago makes the whole
// stage a no-op — and it does so BEFORE any read, so a frozen landkarte costs
// nothing either.
//
// This is the §5.5 bruchpfad in one assertion: the rebuild skips the WHOLE map
// once the node cap is exceeded (WARN, no error, no partial build), and a boost
// on that basis would reinforce months-old topic membership while systematically
// disadvantaging everything created since.
//
// ROT-PROBE: remove the clusterMapUsable call from ClusterBoost ⇒ the stale arm
// boosts exactly like the fresh one and this test fails.
func TestClusterBoostStaleMapIsNoOp(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	in := c3Results(12, "private")
	scopes := []string{"private"}

	// Control: under a fresh map this fixture DOES move.
	live, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, c3Cfg(), c3Now())
	if err != nil {
		t.Fatalf("fresh arm: %v", err)
	}
	moved := false
	for i := range live {
		if live[i].RRFScore != in[i].RRFScore {
			moved = true
			break
		}
	}
	if !moved {
		t.Fatal("fresh arm changed nothing — the fixture proves nothing about the stale arm")
	}

	stale := c3Fresh{at: time.Now().Add(-7 * 24 * time.Hour)}
	got, rep, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, c3Cfg(), stale)
	if err != nil {
		t.Fatalf("stale arm: %v", err)
	}
	c3Equal(t, got, in, "stale map")
	if rep.Count(graphcache.TravClusterStale) != 1 {
		t.Errorf("cluster_stale trip = %d, want 1 — the no-op must say why", rep.Count(graphcache.TravClusterStale))
	}

	// And the unwired seam behaves identically (gate iii at stage level).
	got, rep, err = rrf.ClusterBoost(ctx, pool, in, nil, scopes, c3Cfg(), nil)
	if err != nil {
		t.Fatalf("unwired arm: %v", err)
	}
	c3Equal(t, got, in, "unwired seam")
	if rep.Count(graphcache.TravClusterStale) != 1 {
		t.Errorf("unwired seam: cluster_stale trip = %d, want 1", rep.Count(graphcache.TravClusterStale))
	}
}

// Gate (viii): FAIL-OPEN. A dead pool returns the UNCHANGED input plus the error;
// the handler logs and keeps ranking. A categorical nice-to-have must never turn
// a working query into a 500.
//
// ROT-PROBE: return `nil, rep, err` from the fetch arms ⇒ the caller loses every
// result on a transient DB hiccup.
func TestClusterBoostFailsOpen(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(0))
	}
	in := c3Results(4, "private")

	pool.Close() // every query from here on fails

	got, _, err := rrf.ClusterBoost(ctx, pool, in, nil, []string{"private"}, c3Cfg(), c3Now())
	if err == nil {
		t.Fatal("a dead pool must surface an error (fail-open != fail-silent)")
	}
	c3Equal(t, got, in, "fail-open")
}

// Empty scopes are a HARD error, never a quiet empty membership map: PostgreSQL
// evaluates `scope = ANY('{}')` as a deterministic FALSE, so an unresolved scope
// set would look exactly like "nothing is clustered".
func TestClusterBoostEmptyScopes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	in := c3Results(2, "private")
	// len(readScopes)==0 short-circuits to the untouched slice at the stage head;
	// the hard reject lives one level down, where a caller cannot skip it.
	got, _, err := rrf.ClusterBoost(context.Background(), pool, in, nil, nil, c3Cfg(), c3Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c3Equal(t, got, in, "no scopes")
}

// Gate (ix): LATENCY. The query path is the highest-volume path in the system, so
// the stage gets an absolute bar: +25 ms p95 over a candidate window of 400 —
// the aggregate over-fetch normal case at target scale (handler/query.go widens
// internalLimit to 2x when aggregate types are present) — measured COLD first,
// which §6.3 names as the worst case.
const clusterBoostAcceptanceMs = 25

func TestClusterBoostLatencyAt400Candidates(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const window, clusters = 400, 20
	for i := 0; i < window; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%clusters))
	}
	in := c3Results(window, "private")
	scopes := []string{"private"}

	run := func() time.Duration {
		start := time.Now()
		if _, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, c3Cfg(), c3Now()); err != nil {
			t.Fatalf("ClusterBoost: %v", err)
		}
		return time.Since(start)
	}

	cold := run()
	const iterations = 40
	samples := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		samples = append(samples, run())
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50, p95 := samples[len(samples)/2], samples[(len(samples)*95)/100]

	t.Logf("cluster boost @ %d candidates / %d clusters: cold %.1f ms | warm p50 %.1f ms | warm p95 %.1f ms (bar %d ms)",
		window, clusters, float64(cold.Microseconds())/1000,
		float64(p50.Microseconds())/1000, float64(p95.Microseconds())/1000,
		clusterBoostAcceptanceMs)

	for name, d := range map[string]time.Duration{"cold": cold, "warm p95": p95} {
		if d > clusterBoostAcceptanceMs*time.Millisecond {
			t.Errorf("%s = %.1f ms exceeds the §7 C3(ix) acceptance of %d ms",
				name, float64(d.Microseconds())/1000, clusterBoostAcceptanceMs)
		}
	}
}
