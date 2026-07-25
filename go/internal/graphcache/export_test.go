package graphcache

import "time"

// Test seams for the W05.1 gates. The pure assemble core is driven directly
// with in-memory fixtures (no DB) so every invariant — ordering, determinism,
// u16 fixpoint, supersedes exclusion, interning overflow — is a unit test.
//
// AssembleForTest is the PRODUCTION path (sorted adjacency). AssembleUnsorted-
// ForTest is the deliberately unsorted seam the Fixture-Gate runs the ordering
// checker against, to prove the ordering test is not vacuously green.

// BlockRowT mirrors the unexported blockRow for fixtures.
type BlockRowT struct {
	ID       [16]byte
	Scope    string
	TypeName string
	Archived bool
}

// DreamRowT mirrors the unexported dreamEdgeRow.
type DreamRowT struct {
	Src, Dst [16]byte
	Rel      string
	Conf     float64
	RawConf  float64
}

// StructRowT mirrors the unexported structEdgeRow.
type StructRowT struct {
	Src, Dst [16]byte
	Class    string
	Origin   string
	Created  time.Time
}

func toBlockRows(in []BlockRowT) []blockRow {
	out := make([]blockRow, len(in))
	for i, r := range in {
		out[i] = blockRow{id: r.ID, scope: r.Scope, typeName: r.TypeName, archived: r.Archived}
	}
	return out
}

func toDreamRows(in []DreamRowT) []dreamEdgeRow {
	out := make([]dreamEdgeRow, len(in))
	for i, r := range in {
		out[i] = dreamEdgeRow{src: r.Src, dst: r.Dst, rel: r.Rel, conf: r.Conf, rawConf: r.RawConf}
	}
	return out
}

func toStructRows(in []StructRowT) []structEdgeRow {
	out := make([]structEdgeRow, len(in))
	for i, r := range in {
		out[i] = structEdgeRow{src: r.Src, dst: r.Dst, class: r.Class, origin: r.Origin, created: r.Created}
	}
	return out
}

// AssembleForTest runs the production assemble (sorted adjacency).
func AssembleForTest(u []BlockRowT, d []DreamRowT, s []StructRowT) (*Snapshot, error) {
	return assemble(toBlockRows(u), toDreamRows(d), toStructRows(s), true)
}

// AssembleUnsortedForTest is the permanent unsorted seam for the Fixture-Gate.
func AssembleUnsortedForTest(u []BlockRowT, d []DreamRowT, s []StructRowT) (*Snapshot, error) {
	return assemble(toBlockRows(u), toDreamRows(d), toStructRows(s), false)
}

// CheckOrdering exposes the internal ordering invariant checker.
func CheckOrdering(s *Snapshot) error { return checkOrdering(s) }

// SupersedesSegmentSizeForTest reports how many directed edges the W05.6 display
// segment holds per direction. Test-only: production has no accessor for the
// segment at all, which is exactly what keeps it un-traversable (§3.2 Nr. 3).
func SupersedesSegmentSizeForTest(s *Snapshot) (fwd, rev int) {
	return len(s.supersedes.Fwd.Targets), len(s.supersedes.Rev.Targets)
}

// InducedEdgesMapMembershipForTest runs InducedEdges with the map membership arm
// — the WINNER of the W05.6 micro-bench and the production choice, named
// explicitly so the bench compares like for like.
func InducedEdgesMapMembershipForTest(s *Snapshot, set []uint32) InducedResult {
	return s.inducedEdgesWith(set, mapMembership(set))
}

// InducedEdgesSortedMembershipForTest runs the sorted-slice comparison arm, kept
// permanently so the choice stays re-measurable.
func InducedEdgesSortedMembershipForTest(s *Snapshot, set []uint32) InducedResult {
	return s.inducedEdgesWith(set, sortedMembership(set))
}
