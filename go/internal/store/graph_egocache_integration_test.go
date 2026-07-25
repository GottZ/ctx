//go:build integration

// W05.5 (design/05 §4.2/§4.5/§4.6/§5.1): the EgoGraph cache arm.
//
// Gate order is the wave's order and it is load-bearing: the two SECURITY
// negative gates (§5.1 Nr. 1 scope-move, Nr. 2 foreign bridge) come first and
// are proven NON-VACUOUS against a hint-trusting stub arm
// (store.EgoGraphWithStubCache) — the stub reproduces exactly the two break
// paths, so a gate that stays green against it would be worthless. Then the
// differential gate (fresh snapshot, flag off vs on ⇒ identical node/edge sets
// incl. truncated parity), the stale fixture (security invariant only, NOT
// fill parity — under-fill inside the staleness window is the documented,
// tolerated deviation) and the oracle gate (§4.5: budget_report byte-identical
// with and without foreign private edges).
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/store/ -run '.*[Ee]go.*' -count=1 -v

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Fixture ids for the cache-arm gates (own id space beside the other store_test
// integration files).
const (
	ecFocus = "019e0007-0000-7000-9000-0000000005a1" // shared focus = bridge head A
	ecMoved = "019e0007-0000-7000-9000-0000000005a2" // shared at build time, moved after

	// Bridge set (§5.1 Nr. 2). Four bridge classes, each with exactly one block
	// behind it that is reachable ONLY through the bridge:
	//   X1 stale scope  — shared at build, moved to 'work' after  (hint lies)
	//   X2 stale archive— unarchived at build, archived after     (hint lies)
	//   X3 foreign      — 'work' from the start                   (hint truthful)
	//   X4 grant-only   — 'work', block-granted: VISIBLE leaf     (T41)
	ecX1 = "019e0007-0000-7000-9000-0000000005a3"
	ecB1 = "019e0007-0000-7000-9000-0000000005a4"
	ecX2 = "019e0007-0000-7000-9000-0000000005a7"
	ecB2 = "019e0007-0000-7000-9000-0000000005a8"
	ecX3 = "019e0007-0000-7000-9000-0000000005a9"
	ecB3 = "019e0007-0000-7000-9000-0000000005aa"
	ecX4 = "019e0007-0000-7000-9000-0000000005a5"
	ecB4 = "019e0007-0000-7000-9000-0000000005a6"

	// Visible 2-hop chain in the same fixture: focus — V1 — V2. It is the
	// positive control of the bridge gate (a traversal that DOES cross two hops)
	// and the payload of the multi-hop differential.
	ecV1 = "019e0007-0000-7000-9000-0000000005ab"
	ecV2 = "019e0007-0000-7000-9000-0000000005ac"
)

// ecBuild builds a snapshot over the current DB state.
func ecBuild(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	snap, err := graphcache.Build(context.Background(), pool)
	if err != nil {
		t.Fatalf("graphcache build: %v", err)
	}
	return snap
}

// ecIDs returns the delivered node ids as a set.
func ecIDs(res *store.EgoResult) map[string]bool {
	out := make(map[string]bool, len(res.Nodes))
	for _, n := range res.Nodes {
		out[n.ID] = true
	}
	return out
}

// ecViolations runs the leak assertions of one gate and RETURNS the violations
// instead of failing — so the same assertion can be pointed at the production
// arm (expect none) and at the stub arm (expect at least one). That inversion
// is what keeps the gates non-vacuous permanently.
func ecViolations(res *store.EgoResult, forbidden map[string]string) []string {
	got := ecIDs(res)
	var v []string
	for id, why := range forbidden {
		if got[id] {
			v = append(v, fmt.Sprintf("%s must not appear (%s)", id, why))
		}
	}
	return v
}

// ── GATE 1 (§5.1 Nr. 1): scope move AFTER the snapshot build ──────────────────
//
// The snapshot's ScopeID hint still says "shared"; the live row says "work"
// (outside readScopes). The candidate must die in the hydrate — hint errors cost
// work, never bytes.

func ecScopeMoveFixture(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	gInsertBlock(t, pool, ecFocus, "shared", "egocache")
	gInsertBlock(t, pool, ecMoved, "shared", "egocache")
	gInsertLink(t, pool, ecFocus, ecMoved, "topical", 0.9, 0.9)
	snap := ecBuild(t, pool)
	// The move happens AFTER the build: the hint is now stale.
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET scope = 'work' WHERE id = $1::uuid`, ecMoved); err != nil {
		t.Fatalf("scope move: %v", err)
	}
	return snap
}

func TestEgoCache_ScopeMoveAfterBuild_NoLeak(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecScopeMoveFixture(t, pool)

	p := gEgoParams(ecFocus)
	p.Hops = 1
	res, err := store.EgoGraphCached(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, Age: 42 * time.Second})
	if err != nil {
		t.Fatalf("ego (cache arm): %v", err)
	}
	if v := ecViolations(res, map[string]string{
		ecMoved: "scope moved to 'work' after the snapshot build — the live row decides (§5.1 Nr. 1)",
	}); len(v) > 0 {
		t.Errorf("SCOPE LEAK: %v", v)
	}
	if res.Budget == nil || res.Budget.Source != graphcache.SourceCache {
		t.Errorf("Source = %v, want %q (the cache arm answered)", res.Budget, graphcache.SourceCache)
	}
}

func TestEgoCacheStub_ScopeMoveAfterBuild_IsRed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecScopeMoveFixture(t, pool)

	p := gEgoParams(ecFocus)
	p.Hops = 1
	res, err := store.EgoGraphWithStubCache(context.Background(), pool, p, gScopesA, nil, gVisibleTypes, snap)
	if err != nil {
		t.Fatalf("ego (stub arm): %v", err)
	}
	if v := ecViolations(res, map[string]string{ecMoved: "stale hint"}); len(v) == 0 {
		t.Fatal("VACUOUS GATE: the hint-trusting stub arm did NOT leak the moved block — " +
			"the scope-move gate would pass without proving anything")
	}
}

// ── GATE 2 (§5.1 Nr. 2): traversal THROUGH an invisible bridge ────────────────
//
// A —(dream)— X —(dream)— B: at hops≥2, B must NEVER appear when X is invisible,
// because the frontier is built ONLY from DB-confirmed nodes and the T41 leaf
// check runs on the HYDRATED scope. Four bridge classes (see the id block): two
// where the snapshot hint LIES (scope moved / archived after the build — the
// classes that separate a recheck arm from a hint-trusting one), one truthful
// foreign bridge, and the grant-only bridge that stays a visible LEAF.
//
// The hint-lying bridges are the load-bearing half: an arm whose safety rested
// on hints would be green on the truthful bridge and leak on these two.

func ecBridgeFixture(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	gInsertBlock(t, pool, ecFocus, "shared", "egocache")
	for _, id := range []string{ecX1, ecX2, ecB1, ecB2, ecB3, ecB4, ecV1, ecV2} {
		gInsertBlock(t, pool, id, "shared", "egocache")
	}
	gInsertBlock(t, pool, ecX3, "work", "egocache") // foreign from the start
	gInsertBlock(t, pool, ecX4, "work", "egocache") // foreign, block-granted below
	for _, e := range [][2]string{
		{ecFocus, ecX1}, {ecX1, ecB1},
		{ecFocus, ecX2}, {ecX2, ecB2},
		{ecFocus, ecX3}, {ecX3, ecB3},
		{ecFocus, ecX4}, {ecX4, ecB4},
		{ecFocus, ecV1}, {ecV1, ecV2},
	} {
		gInsertLink(t, pool, e[0], e[1], "topical", 0.9, 0.9)
	}
	snap := ecBuild(t, pool)
	// AFTER the build the two hint-lying bridges change: the snapshot still says
	// "shared, not archived" for both.
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET scope = 'work' WHERE id = $1::uuid`, ecX1); err != nil {
		t.Fatalf("move X1: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, ecX2); err != nil {
		t.Fatalf("archive X2: %v", err)
	}
	return snap
}

// ecBridgeForbidden is the forbidden set of the bridge gate — shared verbatim by
// the production-arm gate and the stub non-vacuity anchor.
var ecBridgeForbidden = map[string]string{
	ecX1: "scope moved to 'work' after the build — the hint lies (§5.1 Nr. 1)",
	ecB1: "reachable ONLY through the stale-scope bridge X1 (§5.1 Nr. 2)",
	ecX2: "archived after the build — the hint lies (§5.1 Nr. 1)",
	ecB2: "reachable ONLY through the stale-archive bridge X2 (§5.1 Nr. 2)",
	ecX3: "foreign-scope bridge node itself is invisible (§5.1 Nr. 2)",
	ecB3: "reachable ONLY through the foreign bridge X3 (§5.1 Nr. 2)",
	ecB4: "reachable ONLY through the grant-only leaf X4 (T41, §5.1 Nr. 4)",
}

func TestEgoCache_ForeignBridge_NoLeak(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecBridgeFixture(t, pool)

	p := gEgoParams(ecFocus)
	p.Hops = 2
	// grantedBlockIDs makes X4 visible (T40a OR-arm) — and T41 keeps it a leaf.
	res, err := store.EgoGraphCached(context.Background(), pool, p, gScopesA, []string{ecX4}, gVisibleTypes,
		store.EgoCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("ego (cache arm): %v", err)
	}
	if v := ecViolations(res, ecBridgeForbidden); len(v) > 0 {
		t.Errorf("BRIDGE LEAK: %v", v)
	}
	// Positive controls: the granted bridge IS visible (T41 leaf), and the fully
	// visible 2-hop chain IS traversed — otherwise the gate above could be green
	// because nothing was traversed at all.
	got := ecIDs(res)
	if !got[ecX4] {
		t.Errorf("granted block %s missing — the T41 leaf must still be a visible node", ecX4)
	}
	if !got[ecV1] || !got[ecV2] {
		t.Errorf("visible 2-hop chain missing (V1=%v V2=%v) — the gate proves nothing if hop 2 never ran",
			got[ecV1], got[ecV2])
	}
}

// TestEgoCache_DifferentialMultiHop is the differential over TWO hops on the
// bridge fixture: same node/edge sets on both arms, including the grant leaf and
// every invisible bridge. Multi-hop parity is where a frontier built from
// unconfirmed candidates would show up as an extra node.
func TestEgoCache_DifferentialMultiHop(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecBridgeFixture(t, pool)
	ctx := context.Background()

	p := gEgoParams(ecFocus)
	p.Hops = 2
	sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, []string{ecX4}, gVisibleTypes, store.EgoCache{})
	if err != nil {
		t.Fatalf("ego (flag off): %v", err)
	}
	cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, []string{ecX4}, gVisibleTypes,
		store.EgoCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("ego (flag on): %v", err)
	}
	if cacheRes.Budget.Source != graphcache.SourceCache {
		t.Fatalf("source = %q, want cache", cacheRes.Budget.Source)
	}
	ecCompare(t, sqlRes, cacheRes)
}

func TestEgoCacheStub_ForeignBridge_IsRed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecBridgeFixture(t, pool)

	p := gEgoParams(ecFocus)
	p.Hops = 2
	res, err := store.EgoGraphWithStubCache(context.Background(), pool, p, gScopesA, []string{ecX4}, gVisibleTypes, snap)
	if err != nil {
		t.Fatalf("ego (stub arm): %v", err)
	}
	if v := ecViolations(res, ecBridgeForbidden); len(v) == 0 {
		t.Fatal("VACUOUS GATE: the hint-trusting stub arm leaked nothing across the bridge fixture — " +
			"the bridge gate would pass without proving anything")
	}
}

// ── GATE 3 (§7 W05.5): differential, FRESH snapshot, flag off vs on ───────────
//
// The refill proof sits in the fixture: category is NOT in the CSR, so the walk
// hands the hydrate candidates that the category filter then discards. Without
// the refill loop those discards would eat per-node-cap slots (SQL grants cap
// slots ONLY to filter-passing visible rows — the anti-starvation discipline of
// the package header) and the cache arm would under-fill.

const ecDiffHub = "019e0007-0000-7000-9000-0000000005b0"

func ecDiffFixture(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	gInsertBlock(t, pool, ecDiffHub, "shared", "alpha")
	// 12 neighbours, alternating categories, raw/weighted confidence strictly
	// descending: the top-of-adjacency window is alpha/beta interleaved, so a
	// non-refilling arm loses cap slots to the beta rows.
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("019e0007-0000-7000-9000-0000000005c%x", i)
		cat := "alpha"
		if i%2 == 1 {
			cat = "beta"
		}
		gInsertBlock(t, pool, id, "shared", cat)
		conf := 0.99 - float64(i)*0.01
		gInsertLink(t, pool, ecDiffHub, id, "topical", conf, conf)
	}
	// Structural neighbours on the SAME hub (Q1s side of the differential, own
	// ordering key created_at DESC + own cap slot pool, E10): six of them,
	// alternating categories like the dream side so the category refill bites on
	// both walks. Two are also dream neighbours, which exercises the zipper's
	// "a node in BOTH lists takes ONE budget slot" fairness rule through the
	// cache arm.
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("019e0007-0000-7000-9000-0000000005e%x", i)
		cat := "alpha"
		if i%2 == 1 {
			cat = "beta"
		}
		if i >= 4 { // reuse two dream neighbours
			id = fmt.Sprintf("019e0007-0000-7000-9000-0000000005c%x", i-4)
		} else {
			gInsertBlock(t, pool, id, "shared", cat)
		}
		ecStructLink(t, pool, ecDiffHub, id, fmt.Sprintf("2026-03-0%d 12:00:00+00", i+1))
	}
	return ecBuild(t, pool)
}

// ecStructLink seeds one structural fact edge with an explicit created_at (the
// Q1s ordering key).
func ecStructLink(t *testing.T, pool *pgxpool.Pool, src, dst, createdAt string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		 VALUES ($1::uuid,$2::uuid,'references','shared','system',$3::timestamptz)`,
		src, dst, createdAt); err != nil {
		t.Fatalf("insert structural link %s->%s: %v", src, dst, err)
	}
}

func ecCompare(t *testing.T, sqlRes, cacheRes *store.EgoResult) {
	t.Helper()
	if len(sqlRes.Nodes) != len(cacheRes.Nodes) {
		t.Errorf("node count: sql=%d cache=%d", len(sqlRes.Nodes), len(cacheRes.Nodes))
	}
	sqlSet, cacheSet := ecIDs(sqlRes), ecIDs(cacheRes)
	for id := range sqlSet {
		if !cacheSet[id] {
			t.Errorf("node %s in SQL result, missing from cache result", id)
		}
	}
	for id := range cacheSet {
		if !sqlSet[id] {
			t.Errorf("node %s in cache result, missing from SQL result", id)
		}
	}
	// Hop stamps must match too — a different hop means a different traversal.
	sqlHops := map[string]int{}
	for _, n := range sqlRes.Nodes {
		sqlHops[n.ID] = n.Hop
	}
	for _, n := range cacheRes.Nodes {
		if h, ok := sqlHops[n.ID]; ok && h != n.Hop {
			t.Errorf("node %s hop: sql=%d cache=%d", n.ID, h, n.Hop)
		}
	}
	if sqlRes.Truncated != cacheRes.Truncated {
		t.Errorf("truncated: sql=%v cache=%v", sqlRes.Truncated, cacheRes.Truncated)
	}
	if len(sqlRes.Edges) != len(cacheRes.Edges) {
		t.Errorf("dream edge count: sql=%d cache=%d", len(sqlRes.Edges), len(cacheRes.Edges))
	}
	if len(sqlRes.StructEdges) != len(cacheRes.StructEdges) {
		t.Errorf("struct edge count: sql=%d cache=%d", len(sqlRes.StructEdges), len(cacheRes.StructEdges))
	}
	sqlEdges, cacheEdges := gEdgePairs(t, sqlRes), gEdgePairs(t, cacheRes)
	for k, v := range sqlEdges {
		if cacheEdges[k] != v {
			t.Errorf("edge %s: sql=%q cache=%q", k, v, cacheEdges[k])
		}
	}
	// Truncation CAUSES must match as well (the W05.4 report is part of the
	// contract now): same classes, same layers.
	if fmt.Sprint(sqlRes.Budget.Limits) != fmt.Sprint(cacheRes.Budget.Limits) {
		t.Errorf("budget limits: sql=%v cache=%v", sqlRes.Budget.Limits, cacheRes.Budget.Limits)
	}
	if fmt.Sprint(sqlRes.Budget.Budgets) != fmt.Sprint(cacheRes.Budget.Budgets) {
		t.Errorf("budget budgets: sql=%v cache=%v", sqlRes.Budget.Budgets, cacheRes.Budget.Budgets)
	}
}

func TestEgoCache_DifferentialFreshSnapshot(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecDiffFixture(t, pool)
	ctx := context.Background()

	cases := []struct {
		name  string
		tweak func(p *store.EgoParams)
	}{
		{"plain", func(p *store.EgoParams) {}},
		{"refill_category_filter", func(p *store.EgoParams) {
			p.PerNodeCap = 3
			p.Categories = []string{"alpha"}
		}},
		{"node_limit_truncation", func(p *store.EgoParams) { p.Limit = 4 }},
		{"per_node_cap", func(p *store.EgoParams) { p.PerNodeCap = 5 }},
		// 0.955 sits strictly BETWEEN two fixture edge weights. A threshold
		// exactly ON an edge weight is the one documented non-parity case — it
		// has its own gate below (§3.2 Nr. 2: the cache gate is at least as
		// strict as SQL, never laxer).
		{"min_confidence", func(p *store.EgoParams) { p.MinConfidence = 0.955 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := gEgoParams(ecDiffHub)
			p.Hops = 1
			tc.tweak(&p)

			sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
			if err != nil {
				t.Fatalf("ego (flag off): %v", err)
			}
			cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
				store.EgoCache{Snapshot: snap, Age: 7 * time.Second})
			if err != nil {
				t.Fatalf("ego (flag on): %v", err)
			}
			if sqlRes.Budget.Source != graphcache.SourceSQL {
				t.Errorf("flag off: source = %q, want sql", sqlRes.Budget.Source)
			}
			if cacheRes.Budget.Source != graphcache.SourceCache {
				t.Fatalf("flag on: source = %q, want cache (the arm did not answer)", cacheRes.Budget.Source)
			}
			if cacheRes.Budget.CacheAge != 7*time.Second {
				t.Errorf("cache age = %v, want 7s", cacheRes.Budget.CacheAge)
			}
			ecCompare(t, sqlRes, cacheRes)
		})
	}
}

// TestEgoCache_ConfidenceThresholdIsStricterNeverLaxer pins the ONE documented
// non-parity direction (§3.2 Nr. 2): with a threshold that falls exactly on an
// edge weight, the u16 fixpoint rule (edge FLOOR, threshold CEIL) makes the
// cache gate reject where SQL accepts. The gate asserts the DIRECTION — the
// cache node set is a subset of the SQL node set, never a superset. A laxer
// cache (the floor/floor bug the rule forbids) fails here.
func TestEgoCache_ConfidenceThresholdIsStricterNeverLaxer(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecDiffFixture(t, pool)
	ctx := context.Background()

	p := gEgoParams(ecDiffHub)
	p.Hops = 1
	p.MinConfidence = 0.95 // exactly the weight of one fixture edge

	sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
	if err != nil {
		t.Fatalf("ego (sql): %v", err)
	}
	cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("ego (cache): %v", err)
	}
	sqlSet := ecIDs(sqlRes)
	for id := range ecIDs(cacheRes) {
		if !sqlSet[id] {
			t.Errorf("cache delivered %s which SQL did not — the cache gate is LAXER than SQL", id)
		}
	}
}

// ── GATE 4 (§7 W05.5): STALE snapshot ⇒ security invariant only ───────────────
//
// A block moved out of scope after the build must be gone. Fill parity is
// EXPLICITLY not asserted: under-fill inside the staleness window (the arm's
// hint pre-filter dropping a block that moved INTO scope) is the documented,
// tolerated deviation (§4.4).

func TestEgoCache_StaleSnapshot_SecurityInvariantOnly(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := ecScopeMoveFixture(t, pool)

	p := gEgoParams(ecFocus)
	p.Hops = 2
	res, err := store.EgoGraphCached(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("ego (cache arm): %v", err)
	}
	if ecIDs(res)[ecMoved] {
		t.Errorf("stale snapshot leaked %s (moved to 'work' after the build)", ecMoved)
	}
	// Deliberately NOT asserted here: node COUNT parity with the SQL arm.
}

// TestEgoCache_UnknownSeed_CompleteSQLFallback pins the §4.2 fallback rule: a
// focus the snapshot does not know (a block younger than the last build) makes
// the arm decline the WHOLE request — no partial cache result, no merge special
// case. The envelope says source="sql"; the operational class TravCacheStale is
// recorded server-side and dropped by WireReport (it is not a client concern and
// the arm identity is already carried by Source).
func TestEgoCache_UnknownSeed_CompleteSQLFallback(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	gInsertBlock(t, pool, ecFocus, "shared", "egocache")
	snap := ecBuild(t, pool) // built BEFORE the neighbour and the young focus exist

	young := "019e0007-0000-7000-9000-0000000005f1"
	gInsertBlock(t, pool, young, "shared", "egocache")
	gInsertBlock(t, pool, ecMoved, "shared", "egocache")
	gInsertLink(t, pool, young, ecMoved, "topical", 0.9, 0.9)

	p := gEgoParams(young)
	p.Hops = 1
	res, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, Age: 5 * time.Second})
	if err != nil {
		t.Fatalf("ego: %v", err)
	}
	if res.Budget.Source != graphcache.SourceSQL {
		t.Errorf("source = %q, want sql (the arm could not answer)", res.Budget.Source)
	}
	if res.Budget.Count(graphcache.TravCacheStale) == 0 {
		t.Errorf("cache_stale not recorded in the server report: %v", res.Budget.Counts)
	}
	if res.Budget.CacheAge != 0 {
		t.Errorf("cache age = %v, want 0 on the SQL arm", res.Budget.CacheAge)
	}
	// The fallback is COMPLETE: the young neighbour is delivered, unlike a
	// partial arm that would have lost the hop.
	if !ecIDs(res)[ecMoved] {
		t.Errorf("SQL fallback lost the neighbour %s — the fallback is not complete", ecMoved)
	}
	b, err := json.Marshal(res.Budget.WireReport())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "cache_stale") {
		t.Errorf("operational class leaked onto the wire: %s", b)
	}
}

// ── GATE 5 (§4.5 oracle barrier): budget_report byte-identical ────────────────
//
// A shared block with foreign PRIVATE edges must produce the same envelope as
// the same block without them: no budget class, no count, no age may vary with
// the existence of invisible neighbours.

const (
	ecOracleHub  = "019e0007-0000-7000-9000-0000000005d0"
	ecOracleVis  = "019e0007-0000-7000-9000-0000000005d1"
	ecOracleF1   = "019e0007-0000-7000-9000-0000000005d2"
	ecOracleF2   = "019e0007-0000-7000-9000-0000000005d3"
	ecOracleF3   = "019e0007-0000-7000-9000-0000000005d4"
	ecOracleName = "oracle"
)

func ecOracleReport(t *testing.T, withForeign bool) []byte {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	gInsertBlock(t, pool, ecOracleHub, "shared", ecOracleName)
	gInsertBlock(t, pool, ecOracleVis, "shared", ecOracleName)
	gInsertLink(t, pool, ecOracleHub, ecOracleVis, "topical", 0.5, 0.5)
	if withForeign {
		// Foreign private edges with the HIGHEST raw confidence — they sit at
		// the very front of the adjacency, i.e. exactly where a leak would show.
		for _, id := range []string{ecOracleF1, ecOracleF2, ecOracleF3} {
			gInsertBlock(t, pool, id, "work", ecOracleName)
			gInsertLink(t, pool, ecOracleHub, id, "topical", 0.99, 0.99)
		}
	}
	snap := ecBuild(t, pool)

	p := gEgoParams(ecOracleHub)
	p.Hops = 1
	res, err := store.EgoGraphCached(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap}) // Age deliberately 0: it must not vary either
	if err != nil {
		t.Fatalf("ego: %v", err)
	}
	if res.Budget.Source != graphcache.SourceCache {
		t.Fatalf("source = %q, want cache", res.Budget.Source)
	}
	b, err := json.Marshal(res.Budget.WireReport())
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return b
}

func TestEgoCache_OracleBarrier_ForeignEdgesInvisibleInReport(t *testing.T) {
	with := ecOracleReport(t, true)
	without := ecOracleReport(t, false)
	if string(with) != string(without) {
		t.Errorf("budget_report differs with/without foreign private edges:\n with = %s\n without = %s", with, without)
	}
	// And the pre-recheck class must not be in there at all.
	if strings.Contains(string(with), "candidates_capped") {
		t.Errorf("wire report carries a pre-recheck class: %s", with)
	}
}
