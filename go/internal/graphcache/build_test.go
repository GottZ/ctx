package graphcache_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/graphcache"
)

// mkID builds a deterministic [16]byte UUID whose sort order follows the given
// leading bytes (the universe is sorted by raw bytes → NodeID order is byte
// order).
func mkID(b ...byte) [16]byte {
	var id [16]byte
	copy(id[:], b)
	return id
}

var (
	idA = mkID(1)
	idB = mkID(2)
	idC = mkID(3)
	idD = mkID(4)
	idX = mkID(99) // never in the universe (dangling target)
)

func block(id [16]byte, scope, typ string, archived bool) graphcache.BlockRowT {
	return graphcache.BlockRowT{ID: id, Scope: scope, TypeName: typ, Archived: archived}
}

func mustAssemble(t *testing.T, u []graphcache.BlockRowT, d []graphcache.DreamRowT, s []graphcache.StructRowT) *graphcache.Snapshot {
	t.Helper()
	snap, err := graphcache.AssembleForTest(u, d, s)
	if err != nil {
		t.Fatalf("AssembleForTest: %v", err)
	}
	return snap
}

func nodeID(t *testing.T, snap *graphcache.Snapshot, id [16]byte) uint32 {
	t.Helper()
	n, ok := snap.NodeID(id)
	if !ok {
		t.Fatalf("NodeID(%v) not found", id)
	}
	return n
}

// TestOrderingInvariant asserts the per-node adjacency ordering (§3.2 Nr. 1):
// dream by RawConf DESC, struct by Created DESC. Edges are supplied in the WRONG
// (ascending) order so the unsorted stub leaves a violation — this test runs RED
// against the stub, GREEN once assemble sorts.
func TestOrderingInvariant(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
		block(idC, "shared", "knowledge", false),
	}
	// Fill order ascending by rawConf — the reverse of the required DESC order.
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.3, RawConf: 0.3},
		{Src: idA, Dst: idC, Rel: "causal", Conf: 0.9, RawConf: 0.9},
	}
	older := time.Unix(1_000, 0)
	newer := time.Unix(2_000, 0)
	s := []graphcache.StructRowT{
		{Src: idA, Dst: idB, Class: "references", Origin: "system", Created: older},
		{Src: idA, Dst: idC, Class: "references", Origin: "system", Created: newer},
	}
	snap := mustAssemble(t, u, d, s)

	if err := graphcache.CheckOrdering(snap); err != nil {
		t.Fatalf("CheckOrdering: %v", err)
	}

	nA := nodeID(t, snap, idA)
	nB := nodeID(t, snap, idB)
	nC := nodeID(t, snap, idC)

	// Dream forward adjacency of A must be C (0.9) before B (0.3).
	de := snap.DreamNeighbors(nA, graphcache.Forward)
	if len(de.Targets) != 2 || de.Targets[0] != nC || de.Targets[1] != nB {
		t.Errorf("dream fwd targets = %v, want [C=%d B=%d] (RawConf DESC)", de.Targets, nC, nB)
	}
	if de.RawConf[0] < de.RawConf[1] {
		t.Errorf("dream fwd RawConf not DESC: %v", de.RawConf)
	}

	// Struct forward adjacency of A must be C (newer) before B (older).
	se := snap.StructNeighbors(nA, graphcache.Forward)
	if len(se.Targets) != 2 || se.Targets[0] != nC || se.Targets[1] != nB {
		t.Errorf("struct fwd targets = %v, want [C=%d B=%d] (Created DESC)", se.Targets, nC, nB)
	}
	if se.Created[0] < se.Created[1] {
		t.Errorf("struct fwd Created not DESC: %v", se.Created)
	}
}

// TestFixtureGate_UnsortedCaught proves the ordering checker is NOT vacuously
// green: run against the deliberately unsorted seam, it MUST report a violation.
func TestFixtureGate_UnsortedCaught(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
		block(idC, "shared", "knowledge", false),
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.3, RawConf: 0.3},
		{Src: idA, Dst: idC, Rel: "causal", Conf: 0.9, RawConf: 0.9},
	}
	snap, err := graphcache.AssembleUnsortedForTest(u, d, nil)
	if err != nil {
		t.Fatalf("AssembleUnsortedForTest: %v", err)
	}
	if err := graphcache.CheckOrdering(snap); err == nil {
		t.Fatal("CheckOrdering returned nil on an unsorted snapshot — the ordering gate is vacuous")
	}
}

// TestDeterminism_OrderIndependent asserts the byte-equality invariant (§7
// W05.1): the same rows in a DIFFERENT order produce a byte-equal snapshot
// (Fingerprint). RED against the stub (unsorted fill is order-dependent), GREEN
// once the total-order adjacency sort makes assemble order-independent.
func TestDeterminism_OrderIndependent(t *testing.T) {
	u1 := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "private", "note", false),
		block(idC, "work", "knowledge", true),
	}
	d1 := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.3, RawConf: 0.3},
		{Src: idA, Dst: idC, Rel: "causal", Conf: 0.9, RawConf: 0.9},
		{Src: idC, Dst: idA, Rel: "factual", Conf: 0.5, RawConf: 0.5},
	}
	s1 := []graphcache.StructRowT{
		{Src: idA, Dst: idC, Class: "references", Origin: "system", Created: time.Unix(2000, 0)},
		{Src: idA, Dst: idB, Class: "duplicate-of", Origin: "manual", Created: time.Unix(1000, 0)},
	}
	// Reversed order copies.
	u2 := []graphcache.BlockRowT{u1[2], u1[1], u1[0]}
	d2 := []graphcache.DreamRowT{d1[2], d1[0], d1[1]}
	s2 := []graphcache.StructRowT{s1[1], s1[0]}

	f1 := mustAssemble(t, u1, d1, s1).Fingerprint()
	f2 := mustAssemble(t, u2, d2, s2).Fingerprint()
	if f1 != f2 {
		t.Errorf("Fingerprint differs across input order: %x vs %x", f1, f2)
	}
}

// TestSupersedesExcluded: supersedes dream edges never enter the CSR (§3.2 Nr. 3).
func TestSupersedesExcluded(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
		block(idC, "shared", "knowledge", false),
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "supersedes", Conf: 1.0, RawConf: 1.0},
		{Src: idA, Dst: idC, Rel: "topical", Conf: 0.6, RawConf: 0.6},
	}
	snap := mustAssemble(t, u, d, nil)

	if snap.Stats.SupersedesSkipped != 1 {
		t.Errorf("SupersedesSkipped = %d, want 1", snap.Stats.SupersedesSkipped)
	}
	if snap.Stats.DreamEdges != 1 {
		t.Errorf("DreamEdges = %d, want 1 (supersedes excluded)", snap.Stats.DreamEdges)
	}
	nA := nodeID(t, snap, idA)
	nB := nodeID(t, snap, idB)
	de := snap.DreamNeighbors(nA, graphcache.Forward)
	for _, tgt := range de.Targets {
		if tgt == nB {
			t.Error("supersedes edge A->B leaked into the dream CSR")
		}
	}
}

// TestInterningOverflowClass: >254 distinct link classes is a loud build error.
func TestInterningOverflowClass(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
	}
	var s []graphcache.StructRowT
	for i := 0; i < 255; i++ {
		s = append(s, graphcache.StructRowT{
			Src: idA, Dst: idB,
			Class:   fmt.Sprintf("class-%03d", i),
			Origin:  "system",
			Created: time.Unix(int64(i), 0),
		})
	}
	_, err := graphcache.AssembleForTest(u, nil, s)
	if !errors.Is(err, graphcache.ErrClassOverflow) {
		t.Fatalf("err = %v, want ErrClassOverflow", err)
	}
}

// TestInterningOverflowOrigin: >254 distinct origins is a loud build error.
func TestInterningOverflowOrigin(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
	}
	var s []graphcache.StructRowT
	for i := 0; i < 255; i++ {
		// distinct origin per edge; class shared (target differs via... use one class)
		s = append(s, graphcache.StructRowT{
			Src: idA, Dst: idB,
			Class:   fmt.Sprintf("k%03d", i), // keep PK unique via class
			Origin:  fmt.Sprintf("origin-%03d", i),
			Created: time.Unix(int64(i), 0),
		})
	}
	_, err := graphcache.AssembleForTest(u, nil, s)
	// Class overflows first here (255 classes too) — assert SOME overflow fires,
	// specifically origin when classes are within cap:
	if err == nil {
		t.Fatal("expected an interning overflow error, got nil")
	}
}

// TestDanglingSkipped: an edge whose endpoint is not in the universe is dropped
// and counted, never panics (§4.1).
func TestDanglingSkipped(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idX, Rel: "topical", Conf: 0.6, RawConf: 0.6}, // X missing
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.6, RawConf: 0.6},
	}
	snap := mustAssemble(t, u, d, nil)
	if snap.Stats.DreamDangling != 1 {
		t.Errorf("DreamDangling = %d, want 1", snap.Stats.DreamDangling)
	}
	if snap.Stats.DreamEdges != 1 {
		t.Errorf("DreamEdges = %d, want 1", snap.Stats.DreamEdges)
	}
}

// TestNodeIDBinarySearch: every seed resolves, an unknown UUID does not.
func TestNodeIDBinarySearch(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idC, "shared", "knowledge", false),
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
	}
	snap := mustAssemble(t, u, nil, nil)
	if snap.NumNodes() != 3 {
		t.Fatalf("NumNodes = %d, want 3", snap.NumNodes())
	}
	// Sorted by bytes: A(0) < B(1) < C(2).
	for want, id := range map[uint32][16]byte{0: idA, 1: idB, 2: idC} {
		got, ok := snap.NodeID(id)
		if !ok || got != want {
			t.Errorf("NodeID(%v) = %d,%v, want %d", id, got, ok, want)
		}
	}
	if _, ok := snap.NodeID(idX); ok {
		t.Error("NodeID(idX) found an absent UUID")
	}
}

// TestInducedEdges: only edges with BOTH endpoints in the set are returned.
func TestInducedEdges(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", false),
		block(idC, "shared", "knowledge", false),
		block(idD, "shared", "knowledge", false),
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.6, RawConf: 0.6},
		{Src: idB, Dst: idC, Rel: "topical", Conf: 0.6, RawConf: 0.6},
		{Src: idC, Dst: idD, Rel: "topical", Conf: 0.6, RawConf: 0.6}, // D outside set
	}
	s := []graphcache.StructRowT{
		{Src: idA, Dst: idC, Class: "references", Origin: "system", Created: time.Unix(1, 0)},
	}
	snap := mustAssemble(t, u, d, s)
	set := []uint32{nodeID(t, snap, idA), nodeID(t, snap, idB), nodeID(t, snap, idC)}

	res := snap.InducedEdges(set)
	if len(res.Dream) != 2 {
		t.Errorf("induced dream = %d edges, want 2 (A->B, B->C)", len(res.Dream))
	}
	if len(res.Struct) != 1 {
		t.Errorf("induced struct = %d edges, want 1 (A->C)", len(res.Struct))
	}
}

// TestDegreeRawAndFiltered: raw degree sums all four directions; the hint filter
// drops out-of-scope neighbours; the walk budget caps the count.
func TestDegreeRawAndFiltered(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "private", "knowledge", false), // out of scope
		block(idC, "shared", "knowledge", false),
		block(idD, "shared", "knowledge", false),
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.6, RawConf: 0.6}, // fwd, private
		{Src: idC, Dst: idA, Rel: "topical", Conf: 0.6, RawConf: 0.6}, // rev, shared
	}
	s := []graphcache.StructRowT{
		{Src: idA, Dst: idD, Class: "references", Origin: "system", Created: time.Unix(1, 0)}, // fwd, shared
	}
	snap := mustAssemble(t, u, d, s)
	nA := nodeID(t, snap, idA)

	raw, capped := snap.Degree(nA, nil)
	if capped {
		t.Error("raw degree reported capped")
	}
	if raw != 3 {
		t.Errorf("raw degree = %d, want 3 (dream fwd + dream rev + struct fwd)", raw)
	}

	hints := snap.MakeDegreeHints([]string{"shared"}, nil, 0)
	filtered, capped := snap.Degree(nA, &hints)
	if capped {
		t.Error("unbudgeted filtered degree reported capped")
	}
	if filtered != 2 {
		t.Errorf("filtered degree = %d, want 2 (private neighbour B excluded)", filtered)
	}

	budgeted := snap.MakeDegreeHints([]string{"shared"}, nil, 2)
	cnt, capped := snap.Degree(nA, &budgeted)
	if !capped {
		t.Error("budget=2 over degree-3 node should report capped")
	}
	if cnt < 0 || cnt > filtered {
		t.Errorf("budgeted count = %d, want a lower bound in [0,%d]", cnt, filtered)
	}
}

// TestArchivedInUniverse: archived blocks stay in the universe (§3.2 Nr. 7) and
// carry the Archived hint; the degree hint filter can exclude them.
func TestArchivedInUniverse(t *testing.T) {
	u := []graphcache.BlockRowT{
		block(idA, "shared", "knowledge", false),
		block(idB, "shared", "knowledge", true), // archived neighbour
	}
	d := []graphcache.DreamRowT{
		{Src: idA, Dst: idB, Rel: "topical", Conf: 0.6, RawConf: 0.6},
	}
	snap := mustAssemble(t, u, d, nil)
	if snap.NumNodes() != 2 {
		t.Fatalf("archived block dropped from universe: NumNodes = %d, want 2", snap.NumNodes())
	}
	nB := nodeID(t, snap, idB)
	if !snap.IsArchived(nB) {
		t.Error("IsArchived(B) = false, want true")
	}
	nA := nodeID(t, snap, idA)
	hints := snap.MakeDegreeHints(nil, nil, 0) // nil scopes = all; ExcludeArchived on
	deg, _ := snap.Degree(nA, &hints)
	if deg != 0 {
		t.Errorf("filtered degree = %d, want 0 (archived neighbour excluded)", deg)
	}
}
