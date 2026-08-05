package overview

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// Gates of wave W-F, unit half (design/02 §7 "W-F" gates 1–4 plus the two the
// scope-purity hardening of migration 127 adds). The persist half lives in
// super_integration_test.go.

// superFixture builds n clusters of `per` blocks each in one scope. Cluster i
// gets a dense internal ring; `bridges` consecutive cluster pairs are joined by
// ONE weak inter-cluster link, so the supergraph has a path and Louvain has
// something to merge.
func superFixture(scope string, n, per, bridges int) (clustering, map[string]string, []rawEdge) {
	cl := clustering{blockToCluster: map[string]string{}, intraDegree: map[string]float64{}}
	scopes := map[string]string{}
	var edges []rawEdge
	block := func(c, i int) string { return fmt.Sprintf("%04d-%04d-block", c, i) }
	for c := range n {
		root := block(c, 0)
		for i := range per {
			b := block(c, i)
			cl.blockToCluster[b] = root
			scopes[b] = scope
			if i > 0 {
				edges = append(edges, rawEdge{src: block(c, i-1), dst: b, weight: 1.0})
			}
		}
	}
	for c := 0; c < bridges && c+1 < n; c++ {
		edges = append(edges, rawEdge{src: block(c, 0), dst: block(c+1, 0), weight: 0.05})
	}
	return cl, scopes, edges
}

func params(target int, enabled bool) superParams {
	return superParams{
		Enabled: enabled, TargetRows: target,
		MinResolution: 0.05, MaxNodes: 0, Resolution: 1.0,
	}
}

func groupCount(l superLevel, scope string) int {
	n := 0
	for _, g := range l.Groups {
		if g.scope == scope {
			n++
		}
	}
	return n
}

// G1 — γ IS THE ONLY KNOB, and it is monotone. This is the surviving,
// operative half of the design's proof probe: the level a smaller resolution
// produces is never finer than the level a larger one produces. A version that
// tried to get a coarser level out of gonum's own hierarchy (Expanded() returns
// the next LOWER level — louvain_common.go:63-70) would be red here, because
// that direction cannot produce a monotone series at all.
func TestSuperLevelGammaIsMonotone(t *testing.T) {
	cl, scopes, edges := superFixture("private", 24, 5, 23)
	prev := -1
	for _, g := range []float64{1.0, 0.8, 0.5, 0.3, 0.1} {
		p := params(9999, true) // target above everything ⇒ the upper bound wins
		p.Resolution, p.MinResolution = g, g
		got := groupCount(computeSuperLevel(context.Background(), cl, scopes, edges, p), "private")
		if prev >= 0 && got > prev {
			t.Errorf("γ=%.2f gives %d groups after a larger γ gave %d — the knob is not monotone", g, got, prev)
		}
		prev = got
	}
}

// G1b — THE MEASURED LIMIT, pinned in both directions (see the super.go limit
// note). gonum drops input self loops at reduction level 0
// (louvain_undirected.go:195-222), so the supergraph carries no memory of
// internal cohesion and the design's "γ = γ_main is a fixpoint" does NOT hold:
//
//   - without inter-cluster bridges nothing CAN merge ⇒ one group per cluster;
//   - with bridges the level already coarsens at the main γ.
//
// Both halves are asserted so the limit cannot silently change: if a later
// gonum, or a hand-built reduced graph, restores the fixpoint, the second half
// goes red and the doc note has to be rewritten rather than quietly outlived.
func TestSuperLevelGammaMainIsNotAFixpoint(t *testing.T) {
	clean, cleanScopes, cleanEdges := superFixture("private", 12, 5, 0)
	if got := groupCount(computeSuperLevel(context.Background(), clean, cleanScopes, cleanEdges, params(9999, true)), "private"); got != 12 {
		t.Errorf("%d groups over 12 unbridged clusters — nothing can merge there", got)
	}

	cl, scopes, edges := superFixture("private", 12, 5, 11)
	got := groupCount(computeSuperLevel(context.Background(), cl, scopes, edges, params(9999, true)), "private")
	if got >= 12 {
		t.Errorf("%d groups at the main γ over 12 bridged clusters — that would BE the fixpoint the design assumed; super.go's limit note is then stale and must be rewritten", got)
	}
}

// G2 — the budget is actually reached: with a target below what the main γ
// already yields, the search must go DOWN and return a partition that fits.
func TestSuperLevelReachesRowTarget(t *testing.T) {
	cl, scopes, edges := superFixture("private", 40, 4, 39)
	ceiling := groupCount(computeSuperLevel(context.Background(), cl, scopes, edges, params(9999, true)), "private")
	if ceiling < 3 {
		t.Fatalf("fixture ceiling is %d groups — too coarse to prove the search moves", ceiling)
	}
	target := ceiling - 1

	l := computeSuperLevel(context.Background(), cl, scopes, edges, params(target, true))
	got := groupCount(l, "private")
	if got == 0 {
		t.Fatal("no groups at all — the search returned nothing")
	}
	if got > target {
		t.Errorf("%d groups for target %d — the budget search did not converge (γ=%v)",
			got, target, l.Gamma["private"])
	}
	if l.Gamma["private"] >= 1.0 {
		t.Errorf("γ = %v — a level below the ceiling cannot sit at the upper bound",
			l.Gamma["private"])
	}
}

// G3 — determinism over 50 runs: identical partition AND identical γ. Same
// discipline as cluster_test.go's Louvain probe, and load-bearing for the map:
// the group order is the row order, and a wobbling γ rewrites a map whose
// partition never moved.
func TestSuperLevelDeterministic(t *testing.T) {
	cl, scopes, edges := superFixture("private", 30, 4, 29)
	p := params(6, true)
	first := computeSuperLevel(context.Background(), cl, scopes, edges, p)
	for i := range 49 {
		got := computeSuperLevel(context.Background(), cl, scopes, edges, p)
		if !reflect.DeepEqual(first.Groups, got.Groups) {
			t.Fatalf("run %d produced a different partition", i+2)
		}
		if !reflect.DeepEqual(first.Gamma, got.Gamma) {
			t.Fatalf("run %d chose γ %v instead of %v", i+2, got.Gamma, first.Gamma)
		}
	}
}

// G4 — the empirical limit named in design/02 §4.7: live, 32 of 59 clusters have
// no inter-cluster edge at all. A meta Louvain can only condense the CONNECTED
// part; isolated clusters stay one-element groups and are carried by the
// collector line, not by 32 meta rows. This pins that the level does not pretend
// otherwise.
func TestSuperLevelIsolatedClustersStaySingletons(t *testing.T) {
	cl, scopes, edges := superFixture("private", 20, 3, 0) // zero bridges
	l := computeSuperLevel(context.Background(), cl, scopes, edges, params(4, true))

	if got := groupCount(l, "private"); got != 20 {
		t.Errorf("%d groups over 20 unconnected clusters — want 20 singletons; a coarser answer would be inventing an edge", got)
	}
}

// Scope purity (migration 127 hardening, K2): the level is computed PER SCOPE,
// so a link crossing scopes must never pull two scopes' clusters into one group
// — and each scope's group count is decided by its own graph only.
func TestSuperLevelIsScopePure(t *testing.T) {
	clA, scopesA, edgesA := superFixture("private", 6, 3, 5)
	clB, scopesB, edgesB := superFixture("shared", 6, 3, 5)
	cl := clustering{blockToCluster: map[string]string{}, intraDegree: map[string]float64{}}
	scopes := map[string]string{}
	for k, v := range clA.blockToCluster {
		cl.blockToCluster[k+"-a"] = v + "-a"
	}
	for k, v := range clB.blockToCluster {
		cl.blockToCluster[k+"-b"] = v + "-b"
	}
	for k, v := range scopesA {
		scopes[k+"-a"] = v
	}
	for k, v := range scopesB {
		scopes[k+"-b"] = v
	}
	var edges []rawEdge
	for _, e := range edgesA {
		edges = append(edges, rawEdge{src: e.src + "-a", dst: e.dst + "-a", weight: e.weight})
	}
	for _, e := range edgesB {
		edges = append(edges, rawEdge{src: e.src + "-b", dst: e.dst + "-b", weight: e.weight})
	}
	// A heavy cross-scope link. If it were counted it would be the strongest
	// edge in the whole input and would merge the two sides on sight.
	for b := range clA.blockToCluster {
		for c := range clB.blockToCluster {
			edges = append(edges, rawEdge{src: b + "-a", dst: c + "-b", weight: 1000})
			break
		}
		break
	}

	l := computeSuperLevel(context.Background(), cl, scopes, edges, params(999, true))
	for _, g := range l.Groups {
		for _, c := range g.clusters {
			wantSuffix := "-a"
			if g.scope == "shared" {
				wantSuffix = "-b"
			}
			if len(c) < 2 || c[len(c)-2:] != wantSuffix {
				t.Fatalf("group of scope %q contains cluster %q — the level crossed a scope boundary", g.scope, c)
			}
		}
	}
	// The two halves are structurally identical, so their group counts must be
	// too — and must equal what the private half yields on its OWN. Anything
	// else means the cross-scope link reached into a scope's partition.
	solo := computeSuperLevel(context.Background(), clA, scopesA, edgesA, params(999, true))
	if groupCount(l, "private") != groupCount(l, "shared") || groupCount(l, "private") != groupCount(solo, "private") {
		t.Errorf("group counts %d/%d against %d in isolation — the cross-scope link changed a scope's own partition",
			groupCount(l, "private"), groupCount(l, "shared"), groupCount(solo, "private"))
	}
}

// The cap is a DEGRADATION, never a skip (design/02 §4.7 step 3): a supergraph
// above root_map.super_max_nodes leaves that scope flat, keeps Attempted true —
// which is what lets the map SAY it was capped — and touches nothing else.
func TestSuperLevelCapDegradesFlat(t *testing.T) {
	cl, scopes, edges := superFixture("private", 10, 2, 9)
	p := params(4, true)
	p.MaxNodes = 3
	l := computeSuperLevel(context.Background(), cl, scopes, edges, p)

	if !l.Attempted {
		t.Error("Attempted = false after a cap — the map could then not tell 'off' from 'capped'")
	}
	if !l.Capped["private"] {
		t.Error("Capped[private] = false although 10 clusters exceed max_nodes 3")
	}
	if len(l.Groups) != 0 {
		t.Errorf("%d groups written despite the cap", len(l.Groups))
	}
	scopesOut, ns, gammas := superMetaArrays(l)
	if len(scopesOut) != 1 || ns[0] != 0 || gammas[0] != 0 {
		t.Errorf("meta arrays %v/%v/%v — a capped scope must report (0, 0), the 'attempted and degraded' encoding",
			scopesOut, ns, gammas)
	}
}

// Disabled is the shipped state and must be a total no-op: no probes, no groups,
// no meta arrays — so graph_overview_meta.super_n stays NULL.
func TestSuperLevelDisabledIsSilent(t *testing.T) {
	cl, scopes, edges := superFixture("private", 8, 3, 7)
	before := superProbes.Load()
	l := computeSuperLevel(context.Background(), cl, scopes, edges, params(4, false))

	if superProbes.Load() != before {
		t.Errorf("%d Louvain probes with the level disabled", superProbes.Load()-before)
	}
	if l.Attempted || len(l.Groups) != 0 {
		t.Errorf("Attempted=%v groups=%d — disabled must write nothing", l.Attempted, len(l.Groups))
	}
	if s, _, _ := superMetaArrays(l); len(s) != 0 {
		t.Errorf("meta arrays %v — a level that was never attempted must leave the columns NULL", s)
	}
}

// The probe budget is FIXED (superGammaProbes) and that is a determinism
// property, not a performance one: a search that stops "when good enough" picks
// a different γ under different float rounding and rewrites the whole map.
func TestSuperLevelProbeBudgetIsBounded(t *testing.T) {
	cl, scopes, edges := superFixture("private", 60, 3, 59)
	before := superProbes.Load()
	computeSuperLevel(context.Background(), cl, scopes, edges, params(5, true))

	if spent := superProbes.Load() - before; spent > superGammaProbes {
		t.Errorf("%d probes for one scope, budget is %d", spent, superGammaProbes)
	}
}

// A cancelled context abandons the level WHOLE. Half a level would be a map that
// reports groups for one scope and silence for the next with nothing saying why
// (design/02 §7 W-F gate 9).
func TestSuperLevelCancelAbandonsWhole(t *testing.T) {
	cl, scopes, edges := superFixture("private", 8, 3, 7)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := computeSuperLevel(ctx, cl, scopes, edges, params(4, true))

	if l.Attempted || len(l.Groups) != 0 {
		t.Errorf("Attempted=%v groups=%d after cancellation — want a level that was never attempted",
			l.Attempted, len(l.Groups))
	}
}

// The group ordinal handed to the INSERT has to be unique across the WHOLE run,
// not per scope: the SQL joins membership rows to their group id on that number
// alone, and two scopes sharing ordinal 0 would fuse two groups.
func TestSuperArraysOrdinalsAreRunUnique(t *testing.T) {
	l := superLevel{Attempted: true, Groups: []superGroup{
		{scope: "private", clusters: []string{"c1", "c2"}},
		{scope: "shared", clusters: []string{"c3"}},
		{scope: "shared", clusters: []string{"c4"}},
	}}
	ords, scopes, clusters := superArrays(l)
	if len(ords) != 4 || len(scopes) != 4 || len(clusters) != 4 {
		t.Fatalf("arrays have lengths %d/%d/%d, want 4 each", len(ords), len(scopes), len(clusters))
	}
	byOrd := map[int32]string{}
	for i, o := range ords {
		if s, seen := byOrd[o]; seen && s != scopes[i] {
			t.Fatalf("ordinal %d appears in scopes %q and %q — two groups would fuse", o, s, scopes[i])
		}
		byOrd[o] = scopes[i]
	}
	if len(byOrd) != 3 {
		t.Errorf("%d distinct ordinals for 3 groups", len(byOrd))
	}
}
