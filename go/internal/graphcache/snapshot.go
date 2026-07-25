package graphcache

import (
	"bytes"
	"slices"
	"time"
)

// Snapshot is the immutable, atomically published cache state (§3.2). Every
// field is set once at Build and never mutated afterwards — publication is a
// pointer swap (store.go), readers run lock-free on whatever pointer they
// loaded. Two separate CSR pairs (one per edge class) keep the GA5 isolation
// invariant STRUCTURAL: a consumer handed only Dream can never surface a
// structural edge (§3.2 rationale c).
type Snapshot struct {
	BuiltAt time.Time // diagnostic only (§3.2) — NOT the staleness anchor (that is dirty-age, W05.2)
	Seq     uint64    // monotone, stamped by Store.Swap; for status/diff

	// Node universe: ALL context_blocks ids (incl. archived — see Archived),
	// sorted by raw UUID bytes. The index into UUIDs is the NodeID (uint32).
	UUIDs    [][16]byte
	ScopeID  []uint16 // interned over scopeNames; len == NumNodes
	TypeID   []uint16 // interned over typeNames; len == NumNodes
	Archived bitset   // 1 bit/node — a pruning HINT, never the final authority (§4.4)

	scopeNames  []string // sorted distinct scope legend
	typeNames   []string // sorted distinct type_name legend
	classNames  []string // sorted distinct structural link_class legend
	originNames []string // sorted distinct structural origin legend

	Dream  CSRPair // dream links (topical/factual/causal/recurrent; supersedes excluded)
	Struct CSRPair // structural links (facts, conf 1.0 by definition)

	// supersedes is the W05.6 DISPLAY segment (§3.2 Nr. 3): the supersedes dream
	// edges, held in their OWN CSR pair, structurally apart from the traversal
	// adjacency above. EgoGraph shows supersedes via Q2 (induced, rendered
	// dashed) and counts it in Q3 (fillDegrees has no relationship filter), so a
	// cache that dropped it could serve neither stage — but NO traversal may ever
	// walk it (rrf/graph.go:391/400, M050 partial indexes).
	//
	// The separation is STRUCTURAL, not conventional, exactly like the Dream/
	// Struct split (§3.2 rationale c): the field is UNEXPORTED and there is NO
	// neighbour accessor for it. DreamNeighbors/StructNeighbors — the only
	// adjacency entry points any traversal consumer has — cannot reach it, so
	// "supersedes is never traversed" holds by construction and not by a filter
	// someone could forget. The two in-package readers are InducedEdges (forward
	// only: display) and Degree (both directions: row count parity with Q3).
	supersedes CSRPair

	Stats BuildStats // build diagnostics (§4.6 status block, W05.2 consumer)
}

// CSRPair holds the forward + reverse adjacency for one edge class.
type CSRPair struct{ Fwd, Rev CSR }

// CSR is one directed compressed-sparse-row adjacency with parallel attribute
// arrays (struct-of-arrays, alignment-free + cache-friendly, §3.2). Adjacency
// of node i is [Offsets[i], Offsets[i+1]). Which attribute arrays are populated
// depends on the edge class: dream fills Rel/Conf/RawConf; struct fills
// ClassID/OriginID/Created. The unused set is nil.
type CSR struct {
	Offsets []uint32 // len == NumNodes+1
	Targets []uint32 // NodeIDs

	// dream attributes:
	Rel     []uint8  // index into GraphRels
	Conf    []uint16 // weighted confidence, u16 fixpoint (ConfToFix)
	RawConf []uint16 // raw_confidence, u16 fixpoint — the adjacency ordering key

	// structural attributes:
	ClassID  []uint8  // index into classNames
	OriginID []uint8  // index into originNames
	Created  []uint32 // created_at Unix seconds — the adjacency ordering key
}

// BuildStats are per-build diagnostics (deterministic — part of Fingerprint).
type BuildStats struct {
	Nodes             int
	DreamEdges        int // directed dream edges in the CSR (supersedes + dangling excluded)
	StructEdges       int // directed structural edges in the CSR (dangling excluded)
	DreamDangling     int // dream edges dropped: endpoint missing or rel off-legend
	StructDangling    int // structural edges dropped: endpoint missing
	SupersedesSkipped int // supersedes dream edges excluded from the traversal CSR (§3.2 Nr. 3)
	SupersedesDisplay int // supersedes edges in the display segment (≤ Skipped: dangling dropped)
}

// Direction selects the forward (source→target) or reverse (target→source)
// adjacency of a node.
type Direction uint8

const (
	Forward Direction = iota
	Reverse
)

// EdgeSlice is a zero-copy view of one node's adjacency: every field is a
// SUBSLICE of the underlying CSR arrays, so no allocation and no copy happens
// on a neighbourhood lookup. All slices share one length (the node's degree).
// Callers MUST NOT mutate these slices — the Snapshot is immutable.
type EdgeSlice struct {
	Targets []uint32
	// dream:
	Rel     []uint8
	Conf    []uint16
	RawConf []uint16
	// struct:
	ClassID  []uint8
	OriginID []uint8
	Created  []uint32
}

// NumNodes is the size of the node universe.
func (s *Snapshot) NumNodes() int { return len(s.UUIDs) }

// NodeID resolves a UUID to its NodeID by binary search over the sorted UUID
// array (§3.2 Nr. 6: lookups happen only at entry points, never per edge, so a
// map's @10M ~0.5 GB overhead buys nothing; binary search is ≤24 probes).
func (s *Snapshot) NodeID(id [16]byte) (uint32, bool) {
	i, found := slices.BinarySearchFunc(s.UUIDs, id, func(a, b [16]byte) int {
		return bytes.Compare(a[:], b[:])
	})
	if !found {
		return 0, false
	}
	return uint32(i), true
}

// DreamNeighbors returns the zero-copy dream adjacency of node n in the given
// direction, pre-sorted by RawConf DESC (§3.2 Nr. 1 — "take the first k" IS the
// per-node cap). Out-of-range n yields an empty slice (defensive; NodeID is the
// validated entry).
func (s *Snapshot) DreamNeighbors(n uint32, dir Direction) EdgeSlice {
	c := s.Dream.Fwd
	if dir == Reverse {
		c = s.Dream.Rev
	}
	lo, hi, ok := edgeRange(c, n)
	if !ok {
		return EdgeSlice{}
	}
	return EdgeSlice{
		Targets: c.Targets[lo:hi],
		Rel:     c.Rel[lo:hi],
		Conf:    c.Conf[lo:hi],
		RawConf: c.RawConf[lo:hi],
	}
}

// StructNeighbors returns the zero-copy structural adjacency of node n in the
// given direction, pre-sorted by Created DESC (§3.2 Nr. 1, M104).
func (s *Snapshot) StructNeighbors(n uint32, dir Direction) EdgeSlice {
	c := s.Struct.Fwd
	if dir == Reverse {
		c = s.Struct.Rev
	}
	lo, hi, ok := edgeRange(c, n)
	if !ok {
		return EdgeSlice{}
	}
	return EdgeSlice{
		Targets:  c.Targets[lo:hi],
		ClassID:  c.ClassID[lo:hi],
		OriginID: c.OriginID[lo:hi],
		Created:  c.Created[lo:hi],
	}
}

// edgeRange returns the [lo,hi) adjacency range of node n, or ok=false if n is
// out of range.
func edgeRange(c CSR, n uint32) (uint32, uint32, bool) {
	if int(n)+1 >= len(c.Offsets) {
		return 0, 0, false
	}
	return c.Offsets[n], c.Offsets[n+1], true
}

// rawDegree is the O(1) offset-difference degree of node n in one CSR.
func rawDegree(c CSR, n uint32) int {
	lo, hi, ok := edgeRange(c, n)
	if !ok {
		return 0
	}
	return int(hi - lo)
}

// DegreeHints filters the hint-degree walk (§4.1, E-05-3a). All three predicates
// are RAM reads against the Snapshot's hint arrays. AllowScope/AllowType are
// indexed by interned id; a nil slice means "all allowed" (no filter). The walk
// is a HINT, never the visibility authority — the value carries the declared
// §4.4 residual delta (stale hints ≤ MaxStaleness, missing grant neighbours).
type DegreeHints struct {
	AllowScope      []bool // indexed by ScopeID; nil = all allowed
	AllowType       []bool // indexed by TypeID; nil = all allowed
	ExcludeArchived bool
	WalkBudget      int // 0 = unlimited; else stop after N examined edges (§4.1)
	// HitCap mirrors the SQL hit budget (store.DegreeHitCap): stop counting after
	// N passing neighbours. 0 = uncapped. Policy lives with the consumer; this is
	// only the early-exit mechanism, so the cache walk stops exactly where the Q3
	// outer LIMIT stops and the "200+" display contract is identical.
	HitCap int
}

// MakeDegreeHints resolves request-scope read scopes + visible types into the
// interned-id bitsets a Degree walk consumes. A nil input slice stays "all
// allowed" (nil bitset); an unknown name simply contributes no allowed id.
func (s *Snapshot) MakeDegreeHints(readScopes, visibleTypes []string, walkBudget int) DegreeHints {
	h := DegreeHints{ExcludeArchived: true, WalkBudget: walkBudget}
	if readScopes != nil {
		h.AllowScope = make([]bool, len(s.scopeNames))
		for _, name := range readScopes {
			if id, ok := s.scopeIDByName(name); ok {
				h.AllowScope[id] = true
			}
		}
	}
	if visibleTypes != nil {
		h.AllowType = make([]bool, len(s.typeNames))
		for _, name := range visibleTypes {
			if id, ok := s.typeIDByName(name); ok {
				h.AllowType[id] = true
			}
		}
	}
	return h
}

// Degree returns the degree of node n. With hints == nil it is the RAW degree:
// O(1) from the offset differences across all six legs (dream fwd/rev + struct
// fwd/rev + the supersedes display segment fwd/rev), summed. The raw degree is a
// SERVER-INTERNAL number — it must never leave the process (a raw count leaks
// foreign private links on shared blocks, store/graph.go:823-825).
//
// With hints set it is the HINT-FILTERED degree: O(degree) RAM reads that keep
// only neighbours passing the scope/type/archived hints, capped by
// hints.WalkBudget (walk budget) and hints.HitCap (hit budget). The bool return
// is "capped": true means the walk hit the WALK budget and the returned count is
// a LOWER BOUND (identical "200+" display contract as fillDegrees' scan budget).
//
// Counting semantics are the Q3 semantics (store/graph.go fillDegrees, K5): link
// ROWS per direction per class, NOT distinct neighbours — which is why the same
// neighbour reached by two edges counts twice, and why the supersedes segment
// participates (the Q3 legs carry no relationship filter).
//
// Declared residual delta vs. the SQL degree (E-05-3(2), the ONE documented
// exception to the §4.4 rule): (i) stale scope/archived hints count wrongly until
// the next rebuild, bounded by MaxStaleness since the attribute-flip trigger
// (§3.1 Nr. 5); (ii) grant-only neighbours are MISSING here — the snapshot holds
// no grants (§3.2 Nr. 5), so the count is an UNDER-count, the fail-closed
// direction. Both are declared, neither is a leak.
func (s *Snapshot) Degree(n uint32, hints *DegreeHints) (int, bool) {
	if int(n) >= s.NumNodes() {
		return 0, false
	}
	if hints == nil {
		return rawDegree(s.Dream.Fwd, n) + rawDegree(s.Dream.Rev, n) +
			rawDegree(s.Struct.Fwd, n) + rawDegree(s.Struct.Rev, n) +
			rawDegree(s.supersedes.Fwd, n) + rawDegree(s.supersedes.Rev, n), false
	}

	count := 0
	examined := 0
	budget := hints.WalkBudget
	capped := false

	visit := func(targets []uint32) bool {
		for _, t := range targets {
			if hints.HitCap > 0 && count >= hints.HitCap {
				return true // hit budget reached — the count is exact AT the cap
			}
			if budget > 0 && examined >= budget {
				capped = true
				return true // stop
			}
			examined++
			if s.hintAllows(t, hints) {
				count++
			}
		}
		return false
	}

	// Walk all six legs; the far endpoint of each edge is the neighbour whose
	// hints decide the count. The supersedes legs are read through the
	// unexported field, NOT through a neighbour accessor — no traversal
	// consumer has a path to them (§3.2 Nr. 3).
	if visit(s.DreamNeighbors(n, Forward).Targets) ||
		visit(s.DreamNeighbors(n, Reverse).Targets) ||
		visit(s.StructNeighbors(n, Forward).Targets) ||
		visit(s.StructNeighbors(n, Reverse).Targets) ||
		visit(csrTargets(s.supersedes.Fwd, n)) ||
		visit(csrTargets(s.supersedes.Rev, n)) {
		return count, capped
	}
	return count, capped
}

// csrTargets is the package-internal target slice of node n in an arbitrary CSR.
// It exists so Degree/InducedEdges can read the supersedes segment WITHOUT that
// segment gaining an exported neighbour accessor (§3.2 Nr. 3).
func csrTargets(c CSR, n uint32) []uint32 {
	lo, hi, ok := edgeRange(c, n)
	if !ok {
		return nil
	}
	return c.Targets[lo:hi]
}

// hintAllows applies the scope/type/archived hint predicates to a neighbour.
func (s *Snapshot) hintAllows(t uint32, h *DegreeHints) bool {
	if h.ExcludeArchived && s.Archived.test(int(t)) {
		return false
	}
	if h.AllowScope != nil {
		sid := int(s.ScopeID[t])
		if sid >= len(h.AllowScope) || !h.AllowScope[sid] {
			return false
		}
	}
	if h.AllowType != nil {
		tid := int(s.TypeID[t])
		if tid >= len(h.AllowType) || !h.AllowType[tid] {
			return false
		}
	}
	return true
}

// InducedDreamEdge / InducedStructEdge are the resolved induced edges (both
// endpoints inside the queried set). Attributes come straight from the CSR
// (u16/u8 fixpoint / interned ids); the consumer wave (W05.6) maps them to the
// wire tuples.
type InducedDreamEdge struct {
	Src, Dst uint32
	Rel      uint8
	Conf     uint16
}

type InducedStructEdge struct {
	Src, Dst uint32
	ClassID  uint8
	OriginID uint8
	Created  uint32
}

// InducedResult holds both classes of induced edges.
type InducedResult struct {
	Dream  []InducedDreamEdge
	Struct []InducedStructEdge
}

// Membership probes for InducedEdges. Both are SPARSE — a dense [NumNodes]bool
// is ruled out on allocation grounds, not probe cost (§4.1: ~1.25 MB alloc +
// zeroing @10M per request for ≤1500 members, with the zeroing dominating the
// intersection work).
//
// Which of the two sparse arms is wired was MEASURED in W05.6, not assumed
// (snapshot_membership_bench_test.go, 200k-node universe, degree 6, Xeon
// E-2176G):
//
//	set=100    sorted 22.9–28.7 µs   map 23.0–29.9 µs   (a wash)
//	set=500    sorted  255   µs      map  124–129 µs    (map ~2.0x faster)
//	set=1500   sorted  911–988 µs    map  452–512 µs    (map ~2.0x faster)
//
// mapMembership is therefore the production arm. The binary search loses at the
// sizes that matter because its ~11 probes per lookup are random accesses into
// a 6 KB array — cache misses — while the map costs one hash and one bucket
// touch. The extra allocation it buys is 22 KB at the 1500-node ceiling
// (9 KB → 22 KB), three orders below the dense arm the design rejected.
//
// sortedMembership stays as the permanent comparison arm so the choice remains
// re-measurable after future changes (W05.8 covers it in the 1M bench).

// mapMembership is the CHOSEN probe: a hash set over the response node ids.
func mapMembership(set []uint32) func(uint32) bool {
	m := make(map[uint32]struct{}, len(set))
	for _, n := range set {
		m[n] = struct{}{}
	}
	return func(x uint32) bool {
		_, ok := m[x]
		return ok
	}
}

// sortedMembership is the comparison arm: a sorted copy of the set + binary
// search. Fewer allocations, ~2x slower at 500+ members.
func sortedMembership(set []uint32) func(uint32) bool {
	sorted := make([]uint32, len(set))
	copy(sorted, set)
	slices.Sort(sorted)
	return func(x uint32) bool {
		_, ok := slices.BinarySearch(sorted, x)
		return ok
	}
}

// InducedEdges returns all edges whose BOTH endpoints lie in set — the cache
// equivalent of Q2/Q2s (store/graph.go inducedEdges / inducedStructEdges).
//
// Only forward adjacency is walked (each directed edge appears exactly once,
// matching the SQL `source = ANY(ids) AND target = ANY(ids)` row semantics).
// The dream result carries the supersedes DISPLAY segment as well: Q2 selects
// every relationship including supersedes (display-only, rendered dashed
// client-side), so leaving it out would make the cache arm's edge set a strict
// subset of the SQL arm's. It is read from the unexported segment, so no
// traversal path gains access to it (§3.2 Nr. 3).
func (s *Snapshot) InducedEdges(set []uint32) InducedResult {
	return s.inducedEdgesWith(set, mapMembership(set))
}

// inducedEdgesWith is the membership-agnostic body (the micro-bench arm seam).
func (s *Snapshot) inducedEdgesWith(set []uint32, member func(uint32) bool) InducedResult {
	var res InducedResult
	for _, n := range set {
		if int(n) >= s.NumNodes() {
			continue
		}
		de := s.DreamNeighbors(n, Forward)
		for i, t := range de.Targets {
			if member(t) {
				res.Dream = append(res.Dream, InducedDreamEdge{
					Src: n, Dst: t, Rel: de.Rel[i], Conf: de.Conf[i],
				})
			}
		}
		if lo, hi, ok := edgeRange(s.supersedes.Fwd, n); ok {
			sup := s.supersedes.Fwd
			for i := lo; i < hi; i++ {
				if t := sup.Targets[i]; member(t) {
					res.Dream = append(res.Dream, InducedDreamEdge{
						Src: n, Dst: t, Rel: sup.Rel[i], Conf: sup.Conf[i],
					})
				}
			}
		}
		se := s.StructNeighbors(n, Forward)
		for i, t := range se.Targets {
			if member(t) {
				res.Struct = append(res.Struct, InducedStructEdge{
					Src: n, Dst: t, ClassID: se.ClassID[i],
					OriginID: se.OriginID[i], Created: se.Created[i],
				})
			}
		}
	}
	return res
}

// Interning accessors: legends are snapshot-internal, exposed read-only.

// IsArchived reports the archived hint of node n (§4.4: a hint, never authority).
func (s *Snapshot) IsArchived(n uint32) bool {
	if int(n) >= s.NumNodes() {
		return false
	}
	return s.Archived.test(int(n))
}

func (s *Snapshot) ScopeName(id uint16) string {
	if int(id) >= len(s.scopeNames) {
		return ""
	}
	return s.scopeNames[id]
}

func (s *Snapshot) TypeName(id uint16) string {
	if int(id) >= len(s.typeNames) {
		return ""
	}
	return s.typeNames[id]
}

func (s *Snapshot) ClassName(id uint8) string {
	if int(id) >= len(s.classNames) {
		return ""
	}
	return s.classNames[id]
}

func (s *Snapshot) OriginName(id uint8) string {
	if int(id) >= len(s.originNames) {
		return ""
	}
	return s.originNames[id]
}

func (s *Snapshot) scopeIDByName(name string) (uint16, bool) {
	i, ok := slices.BinarySearch(s.scopeNames, name)
	if !ok {
		return 0, false
	}
	return uint16(i), true
}

func (s *Snapshot) typeIDByName(name string) (uint16, bool) {
	i, ok := slices.BinarySearch(s.typeNames, name)
	if !ok {
		return 0, false
	}
	return uint16(i), true
}

// bitset is a plain word-packed bit array (the Archived hint).

type bitset struct {
	words []uint64
}

func newBitset(n int) bitset {
	return bitset{words: make([]uint64, (n+63)/64)}
}

func (b bitset) set(i int) {
	b.words[i>>6] |= 1 << (uint(i) & 63)
}

func (b bitset) test(i int) bool {
	if i < 0 || i>>6 >= len(b.words) {
		return false
	}
	return b.words[i>>6]&(1<<(uint(i)&63)) != 0
}
