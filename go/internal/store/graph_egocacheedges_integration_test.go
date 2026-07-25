//go:build integration

// W05.6 (design/05 §3.2 Nr. 3, §4.2, §5.1 Nr. 3a, §8 E-05-3): Q2/Q2s + Q3 from
// the CSR snapshot.
//
// Every gate here is proven NON-VACUOUS against a stub arm that neutralises
// exactly the mechanism the gate asserts (store.EgoGraphWithW056Stub):
//
//	gate                                  red anchor
//	edge differential (supersedes)        StubNoSupersedesSegment
//	degree TypeID hardening E-05-3(1)     StubDegreeWithoutTypeHint
//	degree oracle barrier §5.1 Nr. 3a     StubRawDegree
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
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Fixture ids for the W05.6 edge/degree gates (own id space).
const (
	eeHub = "019e0007-0000-7000-9000-000000000601"
	eeN1  = "019e0007-0000-7000-9000-000000000602"
	eeN2  = "019e0007-0000-7000-9000-000000000603"
	eeN3  = "019e0007-0000-7000-9000-000000000604"
	eeN4  = "019e0007-0000-7000-9000-000000000605"

	// Degree fixture: shared hub with visible, foreign and type-invisible
	// neighbours.
	eeDegHub  = "019e0007-0000-7000-9000-000000000611"
	eeDegVis1 = "019e0007-0000-7000-9000-000000000612"
	eeDegVis2 = "019e0007-0000-7000-9000-000000000613"
	eeDegType = "019e0007-0000-7000-9000-000000000614" // visible scope, INVISIBLE type
	eeDegF1   = "019e0007-0000-7000-9000-000000000615" // foreign private
	eeDegF2   = "019e0007-0000-7000-9000-000000000616"
	eeDegF3   = "019e0007-0000-7000-9000-000000000617"
	eeDegGr   = "019e0007-0000-7000-9000-000000000618" // foreign, block-granted
)

// eeInsertTypedBlock seeds a block with an explicit type_name (the T6 allowlist
// axis the degree hardening filters on).
func eeInsertTypedBlock(t *testing.T, pool *pgxpool.Pool, id, scope, category, typeName string) {
	t.Helper()
	gInsertBlock(t, pool, id, scope, category)
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET type_name = $2 WHERE id = $1::uuid`, id, typeName); err != nil {
		t.Fatalf("set type_name of %s: %v", id, err)
	}
}

// eeEdgeFixture builds the edge differential fixture: a hub with dream edges of
// several relationships INCLUDING supersedes (display-only), structural edges of
// two classes and two origins, plus induced edges BETWEEN the neighbours so the
// induced set is not merely a star.
func eeEdgeFixture(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	for _, id := range []string{eeHub, eeN1, eeN2, eeN3, eeN4} {
		gInsertBlock(t, pool, id, "shared", "eedges")
	}
	// Dream edges, distinct 2-decimal confidences (the u16 fixpoint renders them
	// identically at the wire's three decimals; see the file header of
	// graph_cacheedges.go for the resolution deviation).
	gInsertLink(t, pool, eeHub, eeN1, "topical", 0.91, 0.91)
	gInsertLink(t, pool, eeHub, eeN2, "causal", 0.82, 0.82)
	gInsertLink(t, pool, eeHub, eeN3, "factual", 0.73, 0.73)
	// supersedes: display-only. It must appear in Edges and NEVER produce a hop.
	gInsertLink(t, pool, eeHub, eeN4, "supersedes", 0.64, 0.64)
	gInsertLink(t, pool, eeN1, eeN2, "recurrent", 0.55, 0.55)
	gInsertLink(t, pool, eeN2, eeN1, "supersedes", 0.46, 0.46)

	// Structural edges: two classes, two origins, distinct created_at seconds
	// (the cache orders by Unix SECONDS — same-second ties are the documented
	// resolution deviation, so the fixture keeps them a day apart).
	eeStructLink(t, pool, eeHub, eeN1, "references", "system", "2026-03-01 12:00:00+00")
	eeStructLink(t, pool, eeHub, eeN2, "duplicate-of", "manual", "2026-03-02 12:00:00+00")
	eeStructLink(t, pool, eeN1, eeN3, "references", "forge-sync", "2026-03-03 12:00:00+00")
	return ecBuild(t, pool)
}

func eeStructLink(t *testing.T, pool *pgxpool.Pool, src, dst, class, origin, createdAt string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		 VALUES ($1::uuid,$2::uuid,$3,'shared',$4,$5::timestamptz)`,
		src, dst, class, origin, createdAt); err != nil {
		t.Fatalf("insert structural link %s->%s: %v", src, dst, err)
	}
}

// eeEdgeTuples renders the FULL wire form of both edge arrays plus their legends
// — this is the byte-level comparison the W05.6 gate demands ([src,dst,rel,conf]
// and [src,dst,class,origin] tuples, StructRels/Origins legends). Endpoints are
// rendered as block ids, not response indexes, so a differing node ORDER (which
// the differential asserts separately) cannot mask an edge difference.
func eeEdgeTuples(t *testing.T, res *store.EgoResult) (dream, structural []string, rels, origins string) {
	t.Helper()
	for _, e := range res.Edges {
		if e.Src < 0 || e.Src >= len(res.Nodes) || e.Dst < 0 || e.Dst >= len(res.Nodes) {
			t.Fatalf("dream edge index out of node range: %+v", e)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal dream edge: %v", err)
		}
		// [id,id,relName,rawTuple] — the raw tuple keeps the numeric conf byte-exact.
		dream = append(dream, fmt.Sprintf("%s|%s|%s|%s",
			res.Nodes[e.Src].ID, res.Nodes[e.Dst].ID, res.Rels[e.Rel], raw))
	}
	for _, e := range res.StructEdges {
		if e.Src < 0 || e.Src >= len(res.Nodes) || e.Dst < 0 || e.Dst >= len(res.Nodes) {
			t.Fatalf("struct edge index out of node range: %+v", e)
		}
		if e.Class < 0 || e.Class >= len(res.StructRels) || e.Origin < 0 || e.Origin >= len(res.Origins) {
			t.Fatalf("struct edge legend index out of range: %+v (rels=%v origins=%v)", e, res.StructRels, res.Origins)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal struct edge: %v", err)
		}
		structural = append(structural, fmt.Sprintf("%s|%s|%s|%s|%s",
			res.Nodes[e.Src].ID, res.Nodes[e.Dst].ID,
			res.StructRels[e.Class], res.Origins[e.Origin], raw))
	}
	return dream, structural, fmt.Sprint(res.StructRels), fmt.Sprint(res.Origins)
}

// eeCompareEdges is the edge half of the W05.6 differential.
func eeCompareEdges(t *testing.T, sqlRes, cacheRes *store.EgoResult) {
	t.Helper()
	sd, ss, sr, so := eeEdgeTuples(t, sqlRes)
	cd, cs, cr, co := eeEdgeTuples(t, cacheRes)
	if fmt.Sprint(sd) != fmt.Sprint(cd) {
		t.Errorf("dream edge tuples differ:\n sql   = %v\n cache = %v", sd, cd)
	}
	if fmt.Sprint(ss) != fmt.Sprint(cs) {
		t.Errorf("struct edge tuples differ:\n sql   = %v\n cache = %v", ss, cs)
	}
	if sr != cr {
		t.Errorf("StructRels legend: sql=%s cache=%s", sr, cr)
	}
	if so != co {
		t.Errorf("Origins legend: sql=%s cache=%s", so, co)
	}
}

// eeCompareDegrees is the degree half.
func eeCompareDegrees(t *testing.T, sqlRes, cacheRes *store.EgoResult) {
	t.Helper()
	sql := map[string]int{}
	for _, n := range sqlRes.Nodes {
		sql[n.ID] = n.Degree
	}
	for _, n := range cacheRes.Nodes {
		if d, ok := sql[n.ID]; ok && d != n.Degree {
			t.Errorf("degree of %s: sql=%d cache=%d", n.ID, d, n.Degree)
		}
	}
}

// ── GATE 1 (§7 W05.6): edge differential, fresh snapshot, flag off vs on ──────
//
// Both arms must deliver byte-identical GraphEdge tuples, byte-identical
// [src,dst,class,origin] StructGraphEdge tuples and byte-identical
// StructRels/Origins legends over the same node set.

func TestEgoCacheEdges_Differential(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeEdgeFixture(t, pool)
	ctx := context.Background()

	cases := []struct {
		name  string
		tweak func(p *store.EgoParams)
	}{
		{"plain", func(p *store.EgoParams) {}},
		{"min_confidence", func(p *store.EgoParams) { p.MinConfidence = 0.5 }},
		{"dream_class_filter", func(p *store.EgoParams) {
			p.LinkClasses = []string{"topical", "causal", "supersedes"}
			p.StructClasses = []string{}
		}},
		{"struct_class_filter", func(p *store.EgoParams) {
			p.LinkClasses = []string{}
			p.StructClasses = []string{"references"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := gEgoParams(eeHub)
			p.Hops = 2
			tc.tweak(&p)

			sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
			if err != nil {
				t.Fatalf("ego (flag off): %v", err)
			}
			cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
				store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000})
			if err != nil {
				t.Fatalf("ego (flag on): %v", err)
			}
			if cacheRes.Budget.Source != graphcache.SourceCache {
				t.Fatalf("source = %q, want cache", cacheRes.Budget.Source)
			}
			ecCompare(t, sqlRes, cacheRes) // W05.5 node/truncation/report parity
			eeCompareEdges(t, sqlRes, cacheRes)
			eeCompareDegrees(t, sqlRes, cacheRes)
		})
	}
}

// TestEgoCacheEdges_SupersedesIsDisplayedNeverTraversed is the positive control
// of the display segment plus the traversal invariant on the SAME fixture: the
// supersedes edge hub→N4 is IN the edge array, and N4 is NOT a node (a hop
// candidate would have made it one).
func TestEgoCacheEdges_SupersedesIsDisplayedNeverTraversed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeEdgeFixture(t, pool)

	p := gEgoParams(eeHub)
	p.Hops = 2
	res, err := store.EgoGraphCached(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000})
	if err != nil {
		t.Fatalf("ego (cache arm): %v", err)
	}
	if res.Budget.Source != graphcache.SourceCache {
		t.Fatalf("source = %q, want cache", res.Budget.Source)
	}
	if ecIDs(res)[eeN4] {
		t.Errorf("%s entered the node set — the supersedes edge produced a hop candidate", eeN4)
	}
	// N1/N2 are reachable via positive relationships, and the supersedes edge
	// N2→N1 between them must still be DISPLAYED.
	pairs := map[string]bool{}
	for _, e := range res.Edges {
		pairs[res.Nodes[e.Src].ID+"→"+res.Nodes[e.Dst].ID+"="+res.Rels[e.Rel]] = true
	}
	if !pairs[eeN2+"→"+eeN1+"=supersedes"] {
		t.Errorf("supersedes display edge N2→N1 missing from the cache Q2 result: %v", pairs)
	}
}

// TestEgoCacheEdgesStub_NoSupersedesSegment_IsRed is the non-vacuity anchor of
// gate 1: with the display segment neutralised the differential MUST fail.
func TestEgoCacheEdgesStub_NoSupersedesSegment_IsRed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeEdgeFixture(t, pool)
	ctx := context.Background()

	p := gEgoParams(eeHub)
	p.Hops = 2
	sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
	if err != nil {
		t.Fatalf("ego (sql): %v", err)
	}
	stubRes, err := store.EgoGraphWithW056Stub(ctx, pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000}, store.StubNoSupersedesSegment)
	if err != nil {
		t.Fatalf("ego (stub): %v", err)
	}
	if len(sqlRes.Edges) == len(stubRes.Edges) {
		t.Fatalf("VACUOUS GATE: dropping the supersedes display segment changed nothing "+
			"(sql=%d stub=%d edges) — the differential would pass without proving anything",
			len(sqlRes.Edges), len(stubRes.Edges))
	}
	t.Logf("RED ANCHOR ok: sql dream edges=%d, stub (no supersedes segment)=%d",
		len(sqlRes.Edges), len(stubRes.Edges))
}

// ── GATE 2 (§7 W05.6): truncation parity by CAUSE (TravEdgeLimitReached) ──────
//
// A small EdgeLimit must trip BOTH arms, in the same layer, with the same class
// and the same arbitrated edge set — not merely the same boolean.

func TestEgoCacheEdges_TruncationParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeEdgeFixture(t, pool)
	ctx := context.Background()

	for _, limit := range []int{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("edge_limit=%d", limit), func(t *testing.T) {
			p := gEgoParams(eeHub)
			p.Hops = 2
			p.EdgeLimit = limit

			sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
			if err != nil {
				t.Fatalf("ego (flag off): %v", err)
			}
			cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
				store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000})
			if err != nil {
				t.Fatalf("ego (flag on): %v", err)
			}
			if !sqlRes.Truncated {
				t.Fatalf("fixture does not truncate at EdgeLimit=%d — the gate proves nothing", limit)
			}
			if sqlRes.Truncated != cacheRes.Truncated {
				t.Errorf("truncated: sql=%v cache=%v", sqlRes.Truncated, cacheRes.Truncated)
			}
			// Cause parity: both arms must report TravEdgeLimitReached, in the
			// same layer, the same number of times.
			sqlN := sqlRes.Budget.Count(graphcache.TravEdgeLimitReached)
			cacheN := cacheRes.Budget.Count(graphcache.TravEdgeLimitReached)
			if sqlN == 0 {
				t.Fatalf("SQL arm did not report TravEdgeLimitReached: %v", sqlRes.Budget.Counts)
			}
			if sqlN != cacheN {
				t.Errorf("TravEdgeLimitReached count: sql=%d cache=%d", sqlN, cacheN)
			}
			if fmt.Sprint(sqlRes.Budget.Limits) != fmt.Sprint(cacheRes.Budget.Limits) {
				t.Errorf("budget limits: sql=%v cache=%v", sqlRes.Budget.Limits, cacheRes.Budget.Limits)
			}
			// The arbitrated edge MENU must match too, not just the flag.
			eeCompareEdges(t, sqlRes, cacheRes)
		})
	}
}

// ── GATE 3 (E-05-3): degree differential with the DECLARED, BOUNDED delta ─────
//
// Equality without grants; under-count (never over-count) with a grant or a
// walk budget; type-invisible neighbours never counted.

// eeDegreeFixture: a shared hub with two visible neighbours, one type-invisible
// neighbour, three foreign private neighbours and one foreign block-granted
// neighbour. Every class the E-05-3 delta talks about is present.
func eeDegreeFixture(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	gInsertBlock(t, pool, eeDegHub, "shared", "eedeg")
	gInsertBlock(t, pool, eeDegVis1, "shared", "eedeg")
	gInsertBlock(t, pool, eeDegVis2, "shared", "eedeg")
	// system-meta is NOT in gVisibleTypes — the T6 allowlist axis.
	eeInsertTypedBlock(t, pool, eeDegType, "shared", "eedeg", "system-meta")
	for _, id := range []string{eeDegF1, eeDegF2, eeDegF3, eeDegGr} {
		gInsertBlock(t, pool, id, "work", "eedeg")
	}
	for _, id := range []string{eeDegVis1, eeDegVis2, eeDegType, eeDegF1, eeDegF2, eeDegF3, eeDegGr} {
		gInsertLink(t, pool, eeDegHub, id, "topical", 0.9, 0.9)
	}
	return ecBuild(t, pool)
}

// TestEgoCacheDegree_DifferentialNoGrants: fresh snapshot, no grants ⇒ the cache
// degree EQUALS the SQL degree. The hint filter reproduces scope + type +
// archived exactly; the grant OR-arm is the only piece it cannot see, and with
// no grants there is nothing to miss.
func TestEgoCacheDegree_DifferentialNoGrants(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeDegreeFixture(t, pool)
	ctx := context.Background()

	p := gEgoParams(eeDegHub)
	p.Hops = 1
	sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
	if err != nil {
		t.Fatalf("ego (flag off): %v", err)
	}
	cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000})
	if err != nil {
		t.Fatalf("ego (flag on): %v", err)
	}
	hubDeg := 0
	for _, n := range sqlRes.Nodes {
		if n.ID == eeDegHub {
			hubDeg = n.Degree
		}
	}
	if hubDeg != 2 {
		t.Fatalf("SQL hub degree = %d, want 2 (only the two visible neighbours) — "+
			"the fixture does not exercise the filter", hubDeg)
	}
	eeCompareDegrees(t, sqlRes, cacheRes)
}

// TestEgoCacheDegree_GrantNeighbourUnderCounts asserts the DIRECTION of the
// declared delta (E-05-3(2)(ii)): with a granted foreign neighbour the SQL
// degree counts it, the cache degree cannot — cache ≤ SQL, never above.
func TestEgoCacheDegree_GrantNeighbourUnderCounts(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeDegreeFixture(t, pool)
	ctx := context.Background()

	p := gEgoParams(eeDegHub)
	p.Hops = 1
	grants := []string{eeDegGr}
	sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, grants, gVisibleTypes, store.EgoCache{})
	if err != nil {
		t.Fatalf("ego (flag off): %v", err)
	}
	cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, grants, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000})
	if err != nil {
		t.Fatalf("ego (flag on): %v", err)
	}
	sqlDeg, cacheDeg := map[string]int{}, map[string]int{}
	for _, n := range sqlRes.Nodes {
		sqlDeg[n.ID] = n.Degree
	}
	for _, n := range cacheRes.Nodes {
		cacheDeg[n.ID] = n.Degree
	}
	if sqlDeg[eeDegHub] != 3 {
		t.Fatalf("SQL hub degree = %d, want 3 (2 visible + 1 granted) — the fixture does not exercise grants", sqlDeg[eeDegHub])
	}
	for id, d := range cacheDeg {
		if d > sqlDeg[id] {
			t.Errorf("cache degree of %s = %d > sql %d — the delta must be UNDER-count only", id, d, sqlDeg[id])
		}
	}
	if cacheDeg[eeDegHub] != 2 {
		t.Errorf("cache hub degree = %d, want 2 (the granted neighbour is invisible to the snapshot)", cacheDeg[eeDegHub])
	}
}

// TestEgoCacheDegree_WalkBudgetIsLowerBound: a tiny DegreeWalkBudget makes the
// degree a LOWER bound, never an over-count (E-05-3(3)).
func TestEgoCacheDegree_WalkBudgetIsLowerBound(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeDegreeFixture(t, pool)
	ctx := context.Background()

	p := gEgoParams(eeDegHub)
	p.Hops = 1
	sqlRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes, store.EgoCache{})
	if err != nil {
		t.Fatalf("ego (flag off): %v", err)
	}
	sqlDeg := map[string]int{}
	for _, n := range sqlRes.Nodes {
		sqlDeg[n.ID] = n.Degree
	}
	for _, budget := range []int{1, 2, 3} {
		cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
			store.EgoCache{Snapshot: snap, DegreeWalkBudget: budget})
		if err != nil {
			t.Fatalf("ego (budget=%d): %v", budget, err)
		}
		for _, n := range cacheRes.Nodes {
			if n.Degree > sqlDeg[n.ID] {
				t.Errorf("budget=%d: degree of %s = %d > sql %d — a budget must never over-count",
					budget, n.ID, n.Degree, sqlDeg[n.ID])
			}
		}
	}
	// And the budget really bites: budget=1 over a degree-7 hub cannot reach both
	// visible neighbours (they sit behind foreign ones in the adjacency).
	cacheRes, err := store.EgoGraphCached(ctx, pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 1})
	if err != nil {
		t.Fatalf("ego (budget=1): %v", err)
	}
	for _, n := range cacheRes.Nodes {
		if n.ID == eeDegHub && n.Degree >= sqlDeg[eeDegHub] {
			t.Errorf("budget=1 hub degree = %d, want < %d — the walk budget did not bite",
				n.Degree, sqlDeg[eeDegHub])
		}
	}
}

// TestEgoCacheDegree_TypeInvisibleNeighbourNotCounted pins E-05-3(1): the
// TypeID hardening. Its red anchor is StubDegreeWithoutTypeHint below.
func TestEgoCacheDegree_TypeInvisibleNeighbourNotCounted(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeDegreeFixture(t, pool)

	p := gEgoParams(eeDegHub)
	p.Hops = 1
	res, err := store.EgoGraphCached(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000})
	if err != nil {
		t.Fatalf("ego (cache): %v", err)
	}
	for _, n := range res.Nodes {
		if n.ID == eeDegHub && n.Degree != 2 {
			t.Errorf("cache hub degree = %d, want 2 — the type-invisible neighbour was counted", n.Degree)
		}
	}
}

func TestEgoCacheDegreeStub_WithoutTypeHint_IsRed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	snap := eeDegreeFixture(t, pool)

	p := gEgoParams(eeDegHub)
	p.Hops = 1
	res, err := store.EgoGraphWithW056Stub(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
		store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000}, store.StubDegreeWithoutTypeHint)
	if err != nil {
		t.Fatalf("ego (stub): %v", err)
	}
	for _, n := range res.Nodes {
		if n.ID == eeDegHub {
			if n.Degree == 2 {
				t.Fatal("VACUOUS GATE: dropping the TypeID hint filter did NOT change the degree — " +
					"the E-05-3(1) hardening gate would pass without proving anything")
			}
			t.Logf("RED ANCHOR ok: hub degree without the TypeID hint = %d (production arm: 2)", n.Degree)
		}
	}
}

// ── GATE 4 (§5.1 Nr. 3a): the degree oracle barrier ──────────────────────────
//
// A shared hub's degree must be a function of the CALLER-VISIBLE neighbourhood
// only. Foreign private edges may not shift it by one.

func eeOracleDegree(t *testing.T, withForeign bool, stub bool) int {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	gInsertBlock(t, pool, eeDegHub, "shared", "eedeg")
	gInsertBlock(t, pool, eeDegVis1, "shared", "eedeg")
	gInsertBlock(t, pool, eeDegVis2, "shared", "eedeg")
	gInsertLink(t, pool, eeDegHub, eeDegVis1, "topical", 0.5, 0.5)
	gInsertLink(t, pool, eeDegHub, eeDegVis2, "topical", 0.5, 0.5)
	if withForeign {
		// Highest raw confidence: the very front of the adjacency, i.e. exactly
		// where a leak would show. Includes a supersedes row so the display
		// segment is on the probed path too.
		for i, id := range []string{eeDegF1, eeDegF2, eeDegF3} {
			gInsertBlock(t, pool, id, "work", "eedeg")
			rel := "topical"
			if i == 2 {
				rel = "supersedes"
			}
			gInsertLink(t, pool, eeDegHub, id, rel, 0.99, 0.99)
		}
	}
	snap := ecBuild(t, pool)

	p := gEgoParams(eeDegHub)
	p.Hops = 1
	cache := store.EgoCache{Snapshot: snap, DegreeWalkBudget: 4000}
	var res *store.EgoResult
	var err error
	if stub {
		res, err = store.EgoGraphWithW056Stub(context.Background(), pool, p, gScopesA, nil, gVisibleTypes,
			cache, store.StubRawDegree)
	} else {
		res, err = store.EgoGraphCached(context.Background(), pool, p, gScopesA, nil, gVisibleTypes, cache)
	}
	if err != nil {
		t.Fatalf("ego: %v", err)
	}
	if res.Budget.Source != graphcache.SourceCache {
		t.Fatalf("source = %q, want cache", res.Budget.Source)
	}
	for _, n := range res.Nodes {
		if n.ID == eeDegHub {
			return n.Degree
		}
	}
	t.Fatal("hub missing from the response")
	return -1
}

func TestEgoCacheDegree_OracleBarrier_ForeignEdgesInvisible(t *testing.T) {
	with := eeOracleDegree(t, true, false)
	without := eeOracleDegree(t, false, false)
	if with != without {
		t.Errorf("hub degree leaks foreign private edges: with=%d without=%d", with, without)
	}
	if without != 2 {
		t.Errorf("hub degree = %d, want 2 (the two visible neighbours)", without)
	}
}

func TestEgoCacheDegreeStub_RawDegree_IsRed(t *testing.T) {
	with := eeOracleDegree(t, true, true)
	without := eeOracleDegree(t, false, true)
	if with == without {
		t.Fatalf("VACUOUS GATE: the RAW snapshot degree did not vary with the foreign private edges "+
			"(with=%d without=%d) — the oracle gate would pass without proving anything", with, without)
	}
	t.Logf("RED ANCHOR ok: RAW degree with foreign private edges=%d, without=%d "+
		"(production hint-filtered arm: 2 in both cases)", with, without)
}
