package graphcache_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
)

// W05.6 unit gates for the supersedes DISPLAY segment (design/05 §3.2 Nr. 3) and
// the degree hit cap. The load-bearing property is the ASYMMETRY: the segment is
// reachable from InducedEdges and Degree, and NOT reachable from any neighbour
// accessor — that is what keeps "supersedes is never traversed" structural.

// w056Fixture: A -supersedes-> B, A -topical-> C, C -supersedes-> A.
func w056Fixture(t *testing.T) *graphcache.Snapshot {
	t.Helper()
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
		block(idC, "shared", "knowledge", false),
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "supersedes", Conf: 0.9, RawConf: 0.9},
		{Src: idA, Dst: idC, Rel: "topical", Conf: 0.6, RawConf: 0.6},
		{Src: idC, Dst: idA, Rel: "supersedes", Conf: 0.8, RawConf: 0.8},
	}
	return mustAssemble(t, u, d, nil)
}

// TestSupersedesDisplaySegment_NotInTraversalNotLostForDisplay is the structural
// separation gate: the two supersedes edges are absent from EVERY traversal
// adjacency (all four directions of both CSR pairs) and present in the induced
// display result.
func TestSupersedesDisplaySegment_NotInTraversalNotLostForDisplay(t *testing.T) {
	snap := w056Fixture(t)
	nA, nB, nC := nodeID(t, snap, idA), nodeID(t, snap, idB), nodeID(t, snap, idC)

	if snap.Stats.SupersedesSkipped != 2 {
		t.Errorf("SupersedesSkipped = %d, want 2", snap.Stats.SupersedesSkipped)
	}
	if snap.Stats.SupersedesDisplay != 2 {
		t.Errorf("SupersedesDisplay = %d, want 2", snap.Stats.SupersedesDisplay)
	}
	if snap.Stats.DreamEdges != 1 {
		t.Errorf("DreamEdges = %d, want 1 (only the topical edge traverses)", snap.Stats.DreamEdges)
	}
	fwd, rev := graphcache.SupersedesSegmentSizeForTest(snap)
	if fwd != 2 || rev != 2 {
		t.Errorf("display segment size fwd/rev = %d/%d, want 2/2", fwd, rev)
	}

	// No neighbour accessor may ever surface a supersedes edge. Enumerated as a
	// SET so the assertion is exhaustive rather than a spot check: the only
	// directed pair any adjacency may yield is the topical A->C (fwd of A, rev
	// of C).
	seen := map[[2]uint32]bool{}
	for _, n := range []uint32{nA, nB, nC} {
		for _, tgt := range snap.DreamNeighbors(n, graphcache.Forward).Targets {
			seen[[2]uint32{n, tgt}] = true
		}
		for _, src := range snap.DreamNeighbors(n, graphcache.Reverse).Targets {
			seen[[2]uint32{src, n}] = true
		}
		if len(snap.StructNeighbors(n, graphcache.Forward).Targets) != 0 ||
			len(snap.StructNeighbors(n, graphcache.Reverse).Targets) != 0 {
			t.Errorf("StructNeighbors(%d) not empty in a dream-only fixture", n)
		}
	}
	if len(seen) != 1 || !seen[[2]uint32{nA, nC}] {
		t.Errorf("traversal adjacency = %v, want exactly {A->C} — a supersedes edge is walkable", seen)
	}

	// Display: InducedEdges must carry BOTH supersedes edges plus the topical one.
	res := snap.InducedEdges([]uint32{nA, nB, nC})
	supIdx := -1
	for i, r := range graphcache.GraphRels {
		if r == "supersedes" {
			supIdx = i
		}
	}
	var sup, topical int
	for _, e := range res.Dream {
		if int(e.Rel) == supIdx {
			sup++
		} else {
			topical++
		}
	}
	if sup != 2 {
		t.Errorf("induced supersedes edges = %d, want 2 (Q2 renders supersedes)", sup)
	}
	if topical != 1 {
		t.Errorf("induced topical edges = %d, want 1", topical)
	}
}

// TestSupersedesDisplaySegment_CountsInDegree: Q3 (fillDegrees) has no
// relationship filter, so the cache degree must count supersedes rows too.
func TestSupersedesDisplaySegment_CountsInDegree(t *testing.T) {
	snap := w056Fixture(t)
	nA := nodeID(t, snap, idA)

	// A's rows: fwd supersedes A->B, fwd topical A->C, rev supersedes C->A = 3.
	raw, _ := snap.Degree(nA, nil)
	if raw != 3 {
		t.Errorf("raw degree = %d, want 3 (2 supersedes rows + 1 topical row)", raw)
	}
	hints := snap.MakeDegreeHints([]string{"shared"}, []string{"knowledge"}, 0)
	filtered, capped := snap.Degree(nA, &hints)
	if capped {
		t.Error("unbudgeted degree reported capped")
	}
	if filtered != 3 {
		t.Errorf("filtered degree = %d, want 3 (all neighbours visible)", filtered)
	}
}

// TestDegreeHitCap stops the walk at the hit budget — the "200+" contract.
func TestDegreeHitCap(t *testing.T) {
	u := []graphcache.BlockRowT{block(idA, "shared", "knowledge", false)}
	var d []graphcache.DreamRowT
	for i := 0; i < 10; i++ {
		id := mkID(byte(0x40 + i))
		u = append(u, block(id, "shared", "knowledge", false))
		d = append(d, graphcache.DreamRowT{Src: idA, Dst: id, Rel: "topical", Conf: 0.6, RawConf: 0.6})
	}
	snap := mustAssemble(t, u, d, nil)
	nA := nodeID(t, snap, idA)

	hints := snap.MakeDegreeHints([]string{"shared"}, []string{"knowledge"}, 0)
	hints.HitCap = 4
	got, capped := snap.Degree(nA, &hints)
	if got != 4 {
		t.Errorf("degree with HitCap=4 = %d, want 4", got)
	}
	if capped {
		t.Error("HitCap must not report the WALK-budget capped flag (the count is exact AT the cap)")
	}
}

// TestSupersedesSegment_Deterministic: the display segment is part of the
// Fingerprint, so an input-order permutation must still hash equal — and a
// snapshot WITH supersedes must not hash like one without.
func TestSupersedesSegment_Deterministic(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
	}
	withSup := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "supersedes", Conf: 0.9, RawConf: 0.9},
	}
	f1 := mustAssemble(t, u, withSup, nil).Fingerprint()
	f2 := mustAssemble(t, []graphcache.BlockRowT{u[1], u[0]}, withSup, nil).Fingerprint()
	if f1 != f2 {
		t.Errorf("fingerprint differs across input order: %x vs %x", f1, f2)
	}
	f3 := mustAssemble(t, u, nil, nil).Fingerprint()
	if f1 == f3 {
		t.Error("fingerprint ignores the supersedes display segment — a rebuild could not detect its change")
	}
}

// TestInducedEdges_MembershipArmsAgree pins the micro-bench seam: both membership
// implementations must produce identical results, so the benchmark compares
// equivalent work and the chosen arm is interchangeable.
func TestInducedEdges_MembershipArmsAgree(t *testing.T) {
	snap := w056Fixture(t)
	set := []uint32{nodeID(t, snap, idA), nodeID(t, snap, idB), nodeID(t, snap, idC)}
	a := graphcache.InducedEdgesSortedMembershipForTest(snap, set)
	b := graphcache.InducedEdgesMapMembershipForTest(snap, set)
	if len(a.Dream) != len(b.Dream) || len(a.Struct) != len(b.Struct) {
		t.Fatalf("membership arms disagree: sorted=%d/%d map=%d/%d",
			len(a.Dream), len(a.Struct), len(b.Dream), len(b.Struct))
	}
	for i := range a.Dream {
		if a.Dream[i] != b.Dream[i] {
			t.Errorf("dream edge %d: sorted=%+v map=%+v", i, a.Dream[i], b.Dream[i])
		}
	}
}
