//go:build integration

// Wave C9 (Cluster-Topic-Map, design/03 §5.4 + §7 "C9") — the INJECTION: the
// one place the cluster stage may change the result SET instead of its order.
//
//	(i)   VISIBILITY: a foreign-private sibling of a winning cluster NEVER
//	      appears — the membership scope filter is not enough once a stage
//	      introduces a block;
//	(ii)  an empty visibleTypes allowlist is a HARD error, not 0 rows;
//	(iii) inject_max = 0 (the default) ⇒ byte-identical to C3;
//	plus the cap, the provenance and the fail-open contract.
//
//	go test -tags=integration ./internal/rrf/ -run TestClusterInject -count=1 -v
package rrf_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// c9Types is the resolved retrieval allowlist the query path hands in. The
// fixtures write blocks on the registry default type, so this is the same list
// the live path would carry.
func c9Types() []string { return []string{"knowledge", "reference", "audit-trail"} }

func c9Cfg(max int) rrf.ClusterConfig {
	c := c3Cfg()
	c.InjectMax = max
	c.MinShare = 0.1 // the fixture's single cluster must qualify as a winner
	return c
}

// c9Sibling writes a cluster member that the RESULT SET does not contain — the
// unseen sibling C9 exists to surface.
func c9Sibling(t *testing.T, pool *pgxpool.Pool, id, scope, clusterID string, quality float64, archived bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope, quality_score, is_archived)
		 VALUES ($1::uuid, 'learnings', $2, 'c9 sibling', $3, $4, $5)`,
		id, "c9-"+id, scope, quality, archived); err != nil {
		t.Fatalf("insert sibling %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, $3)`,
		id, clusterID, scope); err != nil {
		t.Fatalf("insert sibling membership: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE graph_cluster_node SET size = size + 1 WHERE cluster_id = $1::uuid AND scope = $2`,
		clusterID, scope); err != nil {
		t.Fatalf("bump node size: %v", err)
	}
}

func c9SiblingID(i int) string { return "019dc000-0000-7000-9000-00000000000" + string(rune('a'+i)) }

// Gate (iii) — THE DEFAULT IS DARK. inject_max 0 means the stage never adds a
// block, and the result is byte-identical to C3 even when unseen siblings exist
// and a cluster wins. A build that ships an armed injection is a behaviour
// change smuggled in with a deploy.
//
// ROT-PROBE: drop the `cfg.InjectMax <= 0` guard in injectClusterMembers ⇒ the
// siblings appear and len(got) != len(want).
func TestClusterInjectDefaultIsDark(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		// TWO clusters, not one: with a single cluster the size damping
		// (share_raw * (1 - size/total)) collapses to zero by construction — the
		// cluster IS the whole visible map. That is correct behaviour and it makes
		// a one-cluster fixture prove nothing.
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	c9Sibling(t, pool, c9SiblingID(0), "private", c3ClusterID(0), 5, false)
	c9Sibling(t, pool, c9SiblingID(1), "private", c3ClusterID(0), 4, false)

	in := c3Results(12, "private")
	scopes := []string{"private"}

	// The two arms differ in EXACTLY the injection knob, so the comparison
	// isolates it: any other config difference would make the boost itself
	// diverge and the test would be measuring the wrong thing.
	dark, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, nil, c9Types(), c9Cfg(0), c3Now())
	if err != nil {
		t.Fatalf("inject_max 0: %v", err)
	}
	if len(dark) != len(in) {
		t.Fatalf("inject_max 0 produced %d results, want %d — the default must not add anything", len(dark), len(in))
	}
	got := make(map[string]bool, len(dark))
	for i := range dark {
		if dark[i].ViaCluster {
			t.Fatalf("inject_max 0 delivered an injected result: %s", dark[i].ID)
		}
		got[dark[i].ID] = true
	}
	for i := range in {
		if !got[in[i].ID] {
			t.Fatalf("inject_max 0 lost the native result %s", in[i].ID)
		}
	}

	// The counter-arm: the SAME fixture and the SAME config with the knob armed
	// DOES add — otherwise "the default adds nothing" would also pass on a stage
	// that can never add anything at all.
	armed, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, nil, c9Types(), c9Cfg(2), c3Now())
	if err != nil {
		t.Fatalf("inject_max 2: %v", err)
	}
	if len(armed) != len(in)+2 {
		t.Fatalf("inject_max 2 produced %d results, want %d — the dark arm proves nothing without this", len(armed), len(in)+2)
	}
}

// Gate (i) — THE VISIBILITY CONTRACT. A foreign-private sibling of the winning
// cluster must NEVER be delivered. The membership row is already scope-filtered,
// but the moment the stage introduces a block, the BLOCK's own visibility has to
// hold as well — archived, type-invisible and foreign-scope alike.
//
// ROT-PROBE (executed): replacing visibility.Predicate with a tautology in
// fetchClusterInjection lets BOTH the archived and the scope-moved sibling into
// the result set and this test fails on two lines at once. Note what the same
// probe also shows: keeping only `NOT cb.is_archived` still leaks the
// scope-moved block — the two conjunctions are not interchangeable.
func TestClusterInjectVisibilityFailsClosed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		// TWO clusters, not one: with a single cluster the size damping
		// (share_raw * (1 - size/total)) collapses to zero by construction — the
		// cluster IS the whole visible map. That is correct behaviour and it makes
		// a one-cluster fixture prove nothing.
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	// Three siblings the caller must not get, each blocked by a DIFFERENT
	// conjunction — which is exactly why both have to be there:
	//
	//   - foreign scope: the MEMBERSHIP row carries it, so clustersql.MemberOf
	//     already drops it (R3, the community side channel);
	//   - archived: only visibility.Predicate sees that;
	//   - MOVED SCOPE: the membership row still says 'private', because that is
	//     the partition the rebuild ran in, while context_blocks.scope is now
	//     'work'. MemberOf cannot see the move — the block's own scope is the
	//     authoritative one and only visibility.Predicate reads it. This is the
	//     case that makes the second conjunction load-bearing rather than
	//     redundant, and it is reachable in production every time a block is
	//     re-scoped between two rebuilds.
	c9Sibling(t, pool, c9SiblingID(0), "work", c3ClusterID(0), 9, false)
	c9Sibling(t, pool, c9SiblingID(1), "private", c3ClusterID(0), 9, true)
	c9Sibling(t, pool, c9SiblingID(3), "private", c3ClusterID(0), 9, false)
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET scope = 'work' WHERE id = $1::uuid`, c9SiblingID(3)); err != nil {
		t.Fatalf("re-scope sibling: %v", err)
	}
	// One sibling the caller SHOULD get, so the fixture proves the path works at
	// all — a test where nothing is injectable would pass with the query deleted.
	c9Sibling(t, pool, c9SiblingID(2), "private", c3ClusterID(0), 3, false)

	in := c3Results(12, "private")
	got, _, err := rrf.ClusterBoost(ctx, pool, in, nil, []string{"private"}, nil, c9Types(), c9Cfg(5), c3Now())
	if err != nil {
		t.Fatalf("inject: %v", err)
	}

	var injected []string
	for i := range got {
		if got[i].ViaCluster {
			injected = append(injected, got[i].ID)
		}
	}
	if len(injected) != 1 || injected[0] != c9SiblingID(2) {
		t.Fatalf("injected %v, want exactly [%s] — foreign-scope and archived siblings must never be delivered",
			injected, c9SiblingID(2))
	}
	for i := range got {
		if got[i].ID == c9SiblingID(0) {
			t.Error("foreign-scope block of the winning cluster was injected")
		}
		if got[i].ID == c9SiblingID(1) {
			t.Error("archived block of the winning cluster was injected")
		}
		if got[i].ID == c9SiblingID(3) {
			t.Error("scope-moved block was injected — the membership row is stale, cb.scope is authoritative")
		}
	}
}

// Gate (ii) — AN EMPTY TYPE ALLOWLIST IS A HARD ERROR. SQL alone would return 0
// rows and the caller would read "nothing to inject" where the truth is "the
// block-type registry is not wired". The stage's fail-open contract then keeps
// the boosted results AND surfaces the error, which is the difference between a
// visible wiring bug and an invisible one.
//
// ROT-PROBE: return `nil, nil` instead of the error ⇒ no error reaches the
// caller and the mis-wiring is silent.
func TestClusterInjectEmptyTypesIsLoud(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		// TWO clusters, not one: with a single cluster the size damping
		// (share_raw * (1 - size/total)) collapses to zero by construction — the
		// cluster IS the whole visible map. That is correct behaviour and it makes
		// a one-cluster fixture prove nothing.
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	c9Sibling(t, pool, c9SiblingID(0), "private", c3ClusterID(0), 5, false)

	in := c3Results(12, "private")
	got, _, err := rrf.ClusterBoost(ctx, pool, in, nil, []string{"private"}, nil, nil, c9Cfg(3), c3Now())
	if err == nil {
		t.Fatal("an empty visible-types allowlist must be an error, never 0 injected rows")
	}
	// FAIL-OPEN: the boost survives the injection failure. Discarding a boost
	// that already succeeded would punish the caller twice for one wiring bug.
	if len(got) != len(in) {
		t.Fatalf("fail-open returned %d results, want the %d boosted ones", len(got), len(in))
	}
}

// THE CAP, and the provenance that comes with an injected block. The cut keeps
// the highest-share candidates (quality, then id, as tiebreak) and records the
// trip — a silent cut is how an operator ends up tuning a knob that is not the
// one that bit.
func TestClusterInjectCapAndProvenance(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		// TWO clusters, not one: with a single cluster the size damping
		// (share_raw * (1 - size/total)) collapses to zero by construction — the
		// cluster IS the whole visible map. That is correct behaviour and it makes
		// a one-cluster fixture prove nothing.
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	for i := 0; i < 4; i++ {
		c9Sibling(t, pool, c9SiblingID(i), "private", c3ClusterID(0), float64(10-i), false)
	}

	in := c3Results(12, "private")
	got, rep, err := rrf.ClusterBoost(ctx, pool, in, nil, []string{"private"}, nil, c9Types(), c9Cfg(2), c3Now())
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(got) != len(in)+2 {
		t.Fatalf("got %d results, want %d (12 native + inject_max 2)", len(got), len(in)+2)
	}
	if rep.Count(graphcache.TravClusterInjectCapped) == 0 {
		t.Error("the cut was silent — cluster_inject_capped must be recorded")
	}

	var topScore float64
	for i := range in {
		if in[i].RRFScore > topScore {
			topScore = in[i].RRFScore
		}
	}
	for i := range got {
		if !got[i].ViaCluster {
			continue
		}
		// Provenance: the share rides along, every derived score field is cleared,
		// and the synthetic score stays within the stage's own declared authority
		// (topScore * boost_weight * share <= topScore * boost_weight).
		if got[i].ClusterBoost <= 0 {
			t.Errorf("injected %s carries no cluster share", got[i].ID)
		}
		if got[i].RRFScoreOriginal != nil || got[i].RerankScore != nil || got[i].CosineSim != nil {
			t.Errorf("injected %s carries a derived score it never earned", got[i].ID)
		}
		if got[i].RRFScore > topScore*c9Cfg(2).BoostWeight+1e-12 {
			t.Errorf("injected %s scored %g, above the stage's own ceiling %g",
				got[i].ID, got[i].RRFScore, topScore*c9Cfg(2).BoostWeight)
		}
		if got[i].RRFScore >= in[len(in)-1].RRFScore {
			t.Errorf("injected %s outranks a native hit — cluster evidence alone must not beat retrieval evidence", got[i].ID)
		}
	}
}

// A STALE MAP INJECTS NOTHING. The freshness gate is the first thing the stage
// checks, so C9 inherits it — and it matters more here than for the boost: a
// reordering from a frozen map is a wrong ranking, an INJECTION from a frozen
// map is a wrong result set.
func TestClusterInjectRespectsStalenessGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		// TWO clusters, not one: with a single cluster the size damping
		// (share_raw * (1 - size/total)) collapses to zero by construction — the
		// cluster IS the whole visible map. That is correct behaviour and it makes
		// a one-cluster fixture prove nothing.
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	c9Sibling(t, pool, c9SiblingID(0), "private", c3ClusterID(0), 5, false)

	in := c3Results(12, "private")
	stale := c3Fresh{at: c3Now().at.Add(-8 * 24 * 60 * 60 * 1e9)}
	got, _, err := rrf.ClusterBoost(ctx, pool, in, nil, []string{"private"}, nil, c9Types(), c9Cfg(3), stale)
	if err != nil {
		t.Fatalf("stale inject: %v", err)
	}
	c3Equal(t, got, in, "a stale map must not inject")
}
