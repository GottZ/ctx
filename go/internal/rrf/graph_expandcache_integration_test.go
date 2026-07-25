//go:build integration

// W05.7 (design/05 §4.2/§4.5/§5.1 Nr. 2/§7): the GraphExpand cache arm.
//
// Gate order is the wave's order and it is load-bearing: the two ORACLE /
// SECURITY gates come FIRST and are proven NON-VACUOUS against permanent stub
// arms (rrf.GraphExpandWithStub), because a gate that stays green against the
// broken arm is worthless:
//
//	Gate 1 — damping oracle: foreign private edges on a shared hub must not
//	         change ANY byte of the fusion. RED against StubRawDegree (damping
//	         from Snapshot.Degree raw offsets = cross-scope channel).
//	Gate 2 — HopDepth=2 bridges: a block behind a foreign-scope bridge AND a
//	         block behind a grant-only bridge must never be injected. RED
//	         against StubHintTrusting (walk candidates taken as final). Both
//	         bridge hints LIE (scope moved AFTER the build) — a truthful hint
//	         would make the stub green and the gate vacuous (W05.5 lesson).
//	Gate 3 — differential: flag off vs on ⇒ identical fusion, plus the
//	         hub-damping DEGREE compared directly edge by edge (no tolerance).
//	Gate 4 — GA5: a structural-only neighbour is never injected on the cache
//	         arm either (the structural half of the seam proof; the type half
//	         is TestExpandArmSeam_DreamOnly).
//	Gate 5 — fail-open + stale: a recheck error keeps the ORIGINAL slice, and a
//	         seed outside the snapshot falls COMPLETELY back to SQL.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/rrf/ -run 'ExpandCache' -count=1 -v

package rrf_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// Fixture ids — own id space beside the other rrf_test integration files.
const (
	// Gate 1 (damping oracle): seed, shared hub, two FOREIGN blocks linking in.
	w57Seed    = "019f9700-0000-7000-9000-000000000001"
	w57Hub     = "019f9700-0000-7000-9000-000000000002"
	w57Foreign = "019f9700-0000-7000-9000-000000000003"
	w57Foreig2 = "019f9700-0000-7000-9000-000000000004"

	// Gate 2 (bridges): A is the seed; XF/XG are bridges whose scope hint LIES
	// (shared at build, moved out afterwards), BF/BG sit behind them; V1/V2 are
	// the visible 2-hop positive control.
	w57A  = "019f9700-0000-7000-9000-000000000011"
	w57XF = "019f9700-0000-7000-9000-000000000012"
	w57BF = "019f9700-0000-7000-9000-000000000013"
	w57XG = "019f9700-0000-7000-9000-000000000014"
	w57BG = "019f9700-0000-7000-9000-000000000015"
	w57V1 = "019f9700-0000-7000-9000-000000000016"
	w57V2 = "019f9700-0000-7000-9000-000000000017"

	// Gate 3 (differential): two seeds onto ONE hub (so damping actually bites),
	// plus one neighbour per seed, one hop-2 tail and one below-gate edge.
	w57S1 = "019f9700-0000-7000-9000-000000000021"
	w57S2 = "019f9700-0000-7000-9000-000000000022"
	w57DH = "019f9700-0000-7000-9000-000000000023"
	w57N1 = "019f9700-0000-7000-9000-000000000024"
	w57N2 = "019f9700-0000-7000-9000-000000000025"
	w57M1 = "019f9700-0000-7000-9000-000000000026"
	w57LO = "019f9700-0000-7000-9000-000000000027"

	// Gate 4 (GA5): seed, structural-only neighbour, dream neighbour.
	w57GA = "019f9700-0000-7000-9000-000000000031"
	w57GB = "019f9700-0000-7000-9000-000000000032"
	w57GC = "019f9700-0000-7000-9000-000000000033"

	// Gate 5 (stale): a seed inserted AFTER the snapshot build.
	w57Late  = "019f9700-0000-7000-9000-000000000041"
	w57LateN = "019f9700-0000-7000-9000-000000000042"
)

// w57Cfg mirrors the rrf graph-expand defaults (defaultGraphCfg is unexported)
// with hub damping ON — the damping degree is what this wave is about.
func w57Cfg() rrf.GraphConfig {
	return rrf.GraphConfig{
		Enabled:                true,
		Directed:               true,
		HopDepth:               1,
		SeedCount:              5,
		SeedScoreFloor:         0.5,
		PerSeedCap:             3,
		MaxInjected:            10,
		MinConfidence:          0.75,
		MinConfidenceRecurrent: 0.8,
		BoostWeight:            0.20,
		HubDamping:             true,
		WeightTopical:          0.5,
		WeightFactual:          0.9,
		WeightCausal:           0.9,
		WeightRecurrent:        1.0,
		NewPlacementFrac:       0.6,
	}
}

func w57Block(t *testing.T, pool *pgxpool.Pool, id, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid,'graphtest',$2,'w057 expand cache fixture',$3)`,
		id, "blk-"+id[len(id)-4:], scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func w57Link(t *testing.T, pool *pgxpool.Pool, src, dst, rel string, conf float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid,$2::uuid,$3,$4,$4,'w057',5)`,
		src, dst, rel, conf); err != nil {
		t.Fatalf("insert link %s->%s: %v", src, dst, err)
	}
}

func w57Move(t *testing.T, pool *pgxpool.Pool, id, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET scope = $2 WHERE id = $1::uuid`, id, scope); err != nil {
		t.Fatalf("scope move %s: %v", id, err)
	}
}

func w57Build(t *testing.T, pool *pgxpool.Pool) *graphcache.Snapshot {
	t.Helper()
	snap, err := graphcache.Build(context.Background(), pool)
	if err != nil {
		t.Fatalf("graphcache build: %v", err)
	}
	return snap
}

// w57Render reduces a fused result set to the fields the differential compares:
// identity, exact score, order and graph provenance.
func w57Render(out []rrf.SearchResult) []string {
	rendered := make([]string, 0, len(out))
	for _, r := range out {
		rendered = append(rendered, fmt.Sprintf("%s|%s|%.17g|via=%t|seed=%s|rel=%s",
			r.ID, r.Scope, r.RRFScore, r.ViaGraph, r.GraphSeedID, r.GraphRelationship))
	}
	return rendered
}

func w57IDs(out []rrf.SearchResult) map[string]bool {
	got := make(map[string]bool, len(out))
	for _, r := range out {
		got[r.ID] = true
	}
	return got
}

// w57Violations runs the leak assertions and RETURNS them instead of failing, so
// the same assertion can be pointed at the production arm (expect none) and at
// the stub arm (expect at least one). That inversion is what keeps the gates
// non-vacuous permanently.
func w57Violations(out []rrf.SearchResult, forbidden map[string]string) []string {
	got := w57IDs(out)
	var v []string
	for id, why := range forbidden {
		if got[id] {
			v = append(v, fmt.Sprintf("%s must not be injected (%s)", id, why))
		}
	}
	sort.Strings(v)
	return v
}

// ── GATE 1 (§4.2 Punkt 2, §7): the hub-damping degree is caller-local ─────────
//
// A shared hub gains two FOREIGN inbound edges after the first run. The SQL
// window COUNT partitions the GATED, seed-incident edge set, so those edges are
// invisible to the damping — the fused output must not move by a single bit.
// The raw snapshot degree would count them (1 → 3), which is exactly the
// cross-scope channel §4.2 Punkt 2 forbids.

func TestGraphExpandCache_DampingOracle_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope, foreign = "w057-damp", "w057-damp-foreign"
	w57Block(t, pool, w57Seed, scope)
	w57Block(t, pool, w57Hub, scope)
	w57Link(t, pool, w57Seed, w57Hub, "topical", 0.9)

	cfg := w57Cfg()
	seeds := []rrf.SearchResult{{ID: w57Seed, Title: "seed", RRFScore: 1.0, Scope: scope}}
	types := []string{"knowledge"}

	before := w57Build(t, pool)
	outBefore, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, cfg,
		rrf.ExpandCache{Snapshot: before})
	if err != nil {
		t.Fatalf("cache arm (no foreign edges): %v", err)
	}
	if !w57IDs(outBefore)[w57Hub] {
		t.Fatal("positive control failed: hub not injected — the oracle probe below would be vacuous")
	}
	stubBefore, err := rrf.GraphExpandWithStub(ctx, pool, seeds, []string{scope}, nil, types, cfg,
		before, rrf.StubRawDegree)
	if err != nil {
		t.Fatalf("raw-degree stub (no foreign edges): %v", err)
	}

	// The foreign links appear AFTER the first run; the caller's view of the
	// corpus is unchanged (foreign scope, never in readScopes).
	w57Block(t, pool, w57Foreign, foreign)
	w57Block(t, pool, w57Foreig2, foreign)
	w57Link(t, pool, w57Foreign, w57Hub, "topical", 0.95)
	w57Link(t, pool, w57Foreig2, w57Hub, "topical", 0.95)

	after := w57Build(t, pool)
	outAfter, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, cfg,
		rrf.ExpandCache{Snapshot: after})
	if err != nil {
		t.Fatalf("cache arm (with foreign edges): %v", err)
	}
	stubAfter, err := rrf.GraphExpandWithStub(ctx, pool, seeds, []string{scope}, nil, types, cfg,
		after, rrf.StubRawDegree)
	if err != nil {
		t.Fatalf("raw-degree stub (with foreign edges): %v", err)
	}

	// Non-vacuity FIRST: the stub MUST move, otherwise the fixture cannot detect
	// a raw-degree damping at all.
	if fmt.Sprint(w57Render(stubBefore)) == fmt.Sprint(w57Render(stubAfter)) {
		t.Fatalf("VACUOUS GATE: the raw-degree stub produced identical output with and without "+
			"the foreign edges (%v) — the fixture cannot detect the cross-scope channel",
			w57Render(stubAfter))
	}
	t.Logf("stub (red anchor) leak: raw-degree damping made the fusion a function of foreign edges\n"+
		"  without foreign edges: %v\n  with    foreign edges: %v",
		w57Render(stubBefore), w57Render(stubAfter))

	if got, want := w57Render(outAfter), w57Render(outBefore); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ORACLE LEAK: foreign private edges on a shared hub changed the fusion\n got: %v\nwant: %v", got, want)
	}

	// And the SQL arm agrees — the parity claim is against the live semantics,
	// not against the cache arm's own past behaviour.
	sqlOut, err := rrf.GraphExpand(ctx, pool, seeds, []string{scope}, nil, types, cfg)
	if err != nil {
		t.Fatalf("sql arm: %v", err)
	}
	if got, want := w57Render(outAfter), w57Render(sqlOut); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("cache arm deviates from SQL under foreign hub edges\n cache: %v\n   sql: %v", got, want)
	}
}

// ── GATE 2 (§5.1 Nr. 2, §7): HopDepth=2 across a foreign AND a grant-only bridge.

func w57BridgeFixture(t *testing.T, pool *pgxpool.Pool, scope, foreign string) *graphcache.Snapshot {
	t.Helper()
	for _, id := range []string{w57A, w57XF, w57BF, w57XG, w57BG, w57V1, w57V2} {
		w57Block(t, pool, id, scope) // XF/XG are in-scope AT BUILD TIME
	}
	w57Link(t, pool, w57A, w57XF, "topical", 0.95)
	w57Link(t, pool, w57A, w57XG, "topical", 0.94)
	w57Link(t, pool, w57A, w57V1, "topical", 0.93)
	w57Link(t, pool, w57XF, w57BF, "topical", 0.92)
	w57Link(t, pool, w57XG, w57BG, "topical", 0.92)
	w57Link(t, pool, w57V1, w57V2, "topical", 0.92)

	snap := w57Build(t, pool)
	// The moves happen AFTER the build: both bridge hints now LIE ("shared"),
	// while the live rows say foreign. Without the lie the stub would decline
	// the bridges on its own and the gate would be vacuous.
	w57Move(t, pool, w57XF, foreign)
	w57Move(t, pool, w57XG, foreign)
	return snap
}

func TestGraphExpandCache_HopDepth2Bridges_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope, foreign = "w057-bridge", "w057-bridge-foreign"
	snap := w57BridgeFixture(t, pool, scope, foreign)

	cfg := w57Cfg()
	cfg.HopDepth = 2
	cfg.PerSeedCap = 5
	cfg.MaxInjected = 20
	seeds := []rrf.SearchResult{{ID: w57A, Title: "A", RRFScore: 1.0, Scope: scope}}
	types := []string{"knowledge"}
	// XG is block-granted: visible LEAF (T41), never a re-seed.
	granted := []string{w57XG}

	forbidden := map[string]string{
		w57XF: "foreign-scope bridge itself (live scope outside readScopes, no grant)",
		w57BF: "block BEHIND the foreign-scope bridge — traversal THROUGH an invisible node",
		w57BG: "block BEHIND the grant-only bridge — T41 leaf must not re-seed",
	}

	// Non-vacuity FIRST: the hint-trusting stub must leak.
	stubOut, err := rrf.GraphExpandWithStub(ctx, pool, seeds, []string{scope}, granted, types, cfg,
		snap, rrf.StubHintTrusting)
	if err != nil {
		t.Fatalf("hint-trusting stub: %v", err)
	}
	stubViolations := w57Violations(stubOut, forbidden)
	if len(stubViolations) < 3 {
		t.Fatalf("VACUOUS GATE: the hint-trusting stub leaked only %v — expected all three "+
			"(the bridge hints must lie, otherwise the stub declines on its own)", stubViolations)
	}
	t.Logf("stub (red anchor) violations: %v", stubViolations)

	// Production arm: no bridge, no block behind one.
	out, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, granted, types, cfg,
		rrf.ExpandCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("cache arm: %v", err)
	}
	if v := w57Violations(out, forbidden); len(v) > 0 {
		t.Errorf("BRIDGE LEAK on the cache arm: %v", v)
	}
	got := w57IDs(out)
	// Positive control: the VISIBLE two-hop chain crossed both hops, so the
	// absence assertions above are not the result of a dead traversal.
	if !got[w57V1] || !got[w57V2] {
		t.Fatalf("positive control failed: visible 2-hop chain missing (V1=%t V2=%t) — "+
			"the bridge assertions would be vacuous", got[w57V1], got[w57V2])
	}
	// The grant-only bridge itself IS visible (leaf) — proves the grant OR-arm
	// still works and the gate is not just dropping everything foreign.
	if !got[w57XG] {
		t.Error("grant-only bridge XG missing: the block-grant OR-arm did not survive the cache hydrate")
	}

	// The SQL arm answers identically — same forbidden set, same positive control.
	sqlOut, err := rrf.GraphExpand(ctx, pool, seeds, []string{scope}, granted, types, cfg)
	if err != nil {
		t.Fatalf("sql arm: %v", err)
	}
	if v := w57Violations(sqlOut, forbidden); len(v) > 0 {
		t.Errorf("BRIDGE LEAK on the SQL arm (pre-existing regression): %v", v)
	}
	if a, b := fmt.Sprint(w57Render(out)), fmt.Sprint(w57Render(sqlOut)); a != b {
		t.Errorf("multi-hop differential mismatch (decay stamps / scores)\n cache: %s\n   sql: %s", a, b)
	}
}

// ── GATE 3 (§7): differential, incl. damping-degree EQUALITY (no tolerance).

func w57DiffFixture(t *testing.T, pool *pgxpool.Pool, scope string) *graphcache.Snapshot {
	t.Helper()
	for _, id := range []string{w57S1, w57S2, w57DH, w57N1, w57N2, w57M1, w57LO} {
		w57Block(t, pool, id, scope)
	}
	// Both seeds point at the hub ⇒ damping degree 2 ⇒ 1/sqrt(2) actually bites.
	w57Link(t, pool, w57S1, w57DH, "topical", 0.90)
	w57Link(t, pool, w57S2, w57DH, "topical", 0.88)
	w57Link(t, pool, w57S1, w57N1, "factual", 0.85)
	w57Link(t, pool, w57S2, w57N2, "recurrent", 0.97)
	w57Link(t, pool, w57N1, w57M1, "causal", 0.86)  // hop-2 tail
	w57Link(t, pool, w57S1, w57LO, "topical", 0.50) // below MinConfidence on both arms
	return w57Build(t, pool)
}

func TestGraphExpandCache_Differential_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w057-diff"
	snap := w57DiffFixture(t, pool, scope)

	types := []string{"knowledge"}
	seeds := []rrf.SearchResult{
		{ID: w57S1, Title: "S1", RRFScore: 1.0, Scope: scope},
		{ID: w57S2, Title: "S2", RRFScore: 0.9, Scope: scope},
	}

	for _, tc := range []struct {
		name     string
		directed bool
		hops     int
	}{
		{"directed-1hop", true, 1},
		{"directed-2hop", true, 2},
		{"undirected-2hop", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := w57Cfg()
			cfg.Directed = tc.directed
			cfg.HopDepth = tc.hops

			sqlOut, err := rrf.GraphExpand(ctx, pool, seeds, []string{scope}, nil, types, cfg)
			if err != nil {
				t.Fatalf("sql arm: %v", err)
			}
			cacheOut, rep, err := rrf.GraphExpandCachedWithReport(ctx, pool, seeds, []string{scope}, nil, types, cfg,
				rrf.ExpandCache{Snapshot: snap})
			if err != nil {
				t.Fatalf("cache arm: %v", err)
			}
			if rep.Source != graphcache.SourceCache {
				t.Errorf("report source = %q, want %q (the cache arm answered)", rep.Source, graphcache.SourceCache)
			}
			if !w57IDs(cacheOut)[w57DH] {
				t.Fatal("positive control failed: hub not injected — the differential would be vacuous")
			}
			if w57IDs(cacheOut)[w57LO] {
				t.Error("below-gate neighbour injected: the u16 confidence gate is laxer than SQL")
			}
			if a, b := fmt.Sprint(w57Render(cacheOut)), fmt.Sprint(w57Render(sqlOut)); a != b {
				t.Errorf("DIFFERENTIAL mismatch\n cache: %s\n   sql: %s", a, b)
			}

			// Damping-degree equality, measured directly on the fetched edges —
			// the fused score would hide a compensating difference.
			seedIDs := []string{w57S1, w57S2}
			sqlProbe, err := rrf.ProbeFetchSQL(ctx, pool, seedIDs, []string{scope}, nil, types, cfg, 1.0)
			if err != nil {
				t.Fatalf("sql probe: %v", err)
			}
			cacheProbe, err := rrf.ProbeFetchCache(ctx, pool, snap, seedIDs, []string{scope}, nil, types, cfg, 1.0)
			if err != nil {
				t.Fatalf("cache probe: %v", err)
			}
			if a, b := w57SortProbes(cacheProbe), w57SortProbes(sqlProbe); fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("DAMPING-DEGREE mismatch (no tolerance)\n cache: %v\n   sql: %v", a, b)
			}
			// The degree must actually be > 1 somewhere, else "equality" is trivial.
			hubbed := false
			for _, p := range cacheProbe {
				if p.NeighborID == w57DH && p.Degree > 1 {
					hubbed = true
				}
			}
			if !hubbed {
				t.Errorf("VACUOUS degree assertion: hub degree never exceeded 1 (%v)", cacheProbe)
			}
		})
	}
}

func w57SortProbes(in []rrf.ExpandEdgeProbe) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, fmt.Sprintf("%s->%s|%s|deg=%d|decay=%.17g|scope=%s",
			p.SeedID, p.NeighborID, p.Relationship, p.Degree, p.HopDecay, p.Scope))
	}
	sort.Strings(out)
	return out
}

// ── GATE 4 (GA5): structural links never inject, on the cache arm either.

func TestGraphExpandCache_StructuralIsolation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w057-ga5"
	for _, id := range []string{w57GA, w57GB, w57GC} {
		w57Block(t, pool, id, scope)
	}
	// B hangs off A ONLY via a structural fact edge (definitional confidence 1.0
	// — it would win every per-seed cap if it ever entered the dream adjacency).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'references',$3,'system')`,
		w57GA, w57GB, scope); err != nil {
		t.Fatalf("insert structural link: %v", err)
	}
	w57Link(t, pool, w57GA, w57GC, "factual", 0.80)

	cfg := w57Cfg()
	cfg.PerSeedCap = 1 // displacement lever, as in the W04-3 SQL gate
	snap := w57Build(t, pool)
	out, err := rrf.GraphExpandCached(ctx, pool,
		[]rrf.SearchResult{{ID: w57GA, Title: "A", RRFScore: 1.0, Scope: scope}},
		[]string{scope}, nil, []string{"knowledge"}, cfg, rrf.ExpandCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("cache arm: %v", err)
	}
	got := w57IDs(out)
	if got[w57GB] {
		t.Error("GA5 LEAK: a structural-only neighbour was injected by the cache arm")
	}
	if !got[w57GC] {
		t.Error("cap displacement: the dream neighbour lost its PerSeedCap slot — a structural row entered the walk")
	}
}

// ── GATE 5 (§4.5 matrix): fail-open stays byte-identical; stale ⇒ COMPLETE SQL.

func TestGraphExpandCache_RecheckErrorFailsOpen_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w057-failopen"
	w57Block(t, pool, "019f9700-0000-7000-9000-000000000051", scope)
	w57Block(t, pool, "019f9700-0000-7000-9000-000000000052", scope)
	w57Link(t, pool, "019f9700-0000-7000-9000-000000000051", "019f9700-0000-7000-9000-000000000052", "topical", 0.9)
	snap := w57Build(t, pool)

	// The walk succeeds on the snapshot; the HYDRATE cannot reach a DB. That is
	// precisely TravRecheckError on the query path: fail-open, original slice.
	dead, err := pgxpool.New(ctx, "postgres://nouser:nopass@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("constructing lazy pool: %v", err)
	}
	defer dead.Close()

	in := []rrf.SearchResult{{ID: "019f9700-0000-7000-9000-000000000051", Title: "S", RRFScore: 1.0, Scope: scope}}
	out, gerr := rrf.GraphExpandCached(ctx, dead, in, []string{scope}, nil, []string{"knowledge"}, w57Cfg(),
		rrf.ExpandCache{Snapshot: snap})
	if gerr == nil {
		t.Fatal("expected a hydrate error from an unreachable pool")
	}
	if len(out) != len(in) || out[0].ID != in[0].ID {
		t.Fatalf("fail-open must return the ORIGINAL slice unchanged, got %+v", out)
	}
}

func TestGraphExpandCache_StaleSeedFallsBackToSQL_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w057-stale"
	snap := w57Build(t, pool) // built BEFORE the fixture exists
	w57Block(t, pool, w57Late, scope)
	w57Block(t, pool, w57LateN, scope)
	w57Link(t, pool, w57Late, w57LateN, "topical", 0.9)

	cfg := w57Cfg()
	seeds := []rrf.SearchResult{{ID: w57Late, Title: "late", RRFScore: 1.0, Scope: scope}}
	types := []string{"knowledge"}

	out, rep, err := rrf.GraphExpandCachedWithReport(ctx, pool, seeds, []string{scope}, nil, types, cfg,
		rrf.ExpandCache{Snapshot: snap})
	if err != nil {
		t.Fatalf("stale fallback: %v", err)
	}
	sqlOut, err := rrf.GraphExpand(ctx, pool, seeds, []string{scope}, nil, types, cfg)
	if err != nil {
		t.Fatalf("sql arm: %v", err)
	}
	if !w57IDs(out)[w57LateN] {
		t.Error("QUERY LOSS: the stale fallback dropped the neighbour the SQL arm delivers")
	}
	if a, b := fmt.Sprint(w57Render(out)), fmt.Sprint(w57Render(sqlOut)); a != b {
		t.Errorf("stale fallback is not the SQL answer\n got: %s\nwant: %s", a, b)
	}
	if rep.Source != graphcache.SourceSQL {
		t.Errorf("report source = %q, want %q (the SQL arm answered)", rep.Source, graphcache.SourceSQL)
	}
	if n := rep.Count(graphcache.TravCacheStale); n != 1 {
		t.Errorf("TravCacheStale count = %d, want 1", n)
	}
	if len(rep.WireReport().Limits) != 0 || len(rep.WireReport().Budgets) != 0 {
		t.Errorf("operational class reached a wire layer: %+v", rep.WireReport())
	}
}
