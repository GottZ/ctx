// Package graphcache holds the in-memory CSR adjacency cache over BOTH edge
// classes (context_dream_links + context_structural_links) that Achse 05 uses
// to move the two live traversal engines (rrf.GraphExpand, store.EgoGraph) off
// per-request SQL hops onto O(1) neighbourhood lookups.
//
// W05.1 delivers ONLY the data layer: the immutable Snapshot type, the
// deterministic Builder, the double-buffer Store, and the read API
// (NodeID / Neighbours / Degree / InducedEdges). It ships NO consumer and NO
// scheduler — the rebuild job (W05.2), the NOTIFY invalidation (W05.3), the
// budget taxonomy (W05.4) and the engine cut-overs (W05.5–W05.7) are later
// waves. Every path here is dormant until a consumer reads Store.Current().
//
// Design: .project/plan-evokoa-cleanroom-2026-07-22/design/05-graph-track.md
// §3.2 (normative in-memory layout) + §4.1 (API) + §7 line W05.1 (gates).
//
// Source: https://github.com/GottZ/ctx
package graphcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphRels is the fixed dream relationship legend — an edge's Rel byte indexes
// into this slice. Kept byte-identical to store.GraphRels (store/graph.go:56)
// so the two layers share ONE legend; duplicated here rather than imported to
// keep graphcache free of a store dependency (store may later call into
// graphcache, not the reverse). The first four are the positive (traversable)
// types; supersedes is display-only and NEVER enters the CSR (§3.2 Nr. 3).
var GraphRels = []string{"topical", "factual", "causal", "recurrent", "supersedes"}

// supersededRel is the index of "supersedes" — edges of this relationship are
// excluded from the dream CSR (traversal contract: rrf/graph.go:391/400,
// M050 partial indexes WHERE relationship <> 'supersedes').
const supersededRel = "supersedes"

// Interning ceilings (§3.2 Nr. 1 + §6.4). scope/type intern into uint16; class/
// origin into uint8 with a hard cap of 254 (analogous to the pgGraph label
// concept). Exceeding either is a LOUD build error, never a silent wrap.
const (
	maxScopeType   = 1 << 16 // 65536 distinct values fit in uint16 (ids 0..65535)
	maxClassOrigin = 254     // u8 cap; ids 0..253
)

var (
	// ErrScopeOverflow / ErrTypeOverflow fire when the distinct scope/type name
	// count exceeds the uint16 interning range.
	ErrScopeOverflow = errors.New("graphcache: distinct scope count exceeds uint16 interning range")
	ErrTypeOverflow  = errors.New("graphcache: distinct type_name count exceeds uint16 interning range")
	// ErrClassOverflow / ErrOriginOverflow fire when the distinct structural
	// link_class / origin count exceeds the u8 cap (254) — §6.4 "Build-Fehler laut".
	ErrClassOverflow  = errors.New("graphcache: distinct structural link_class count exceeds u8 cap (254)")
	ErrOriginOverflow = errors.New("graphcache: distinct structural origin count exceeds u8 cap (254)")
)

// blockRow is one row of the block universe stream (id, scope, type_name,
// is_archived) — the Build query reads it WITHOUT ORDER BY; the total order is
// established in Go by sorting the UUID array (§4.1).
type blockRow struct {
	id       [16]byte
	scope    string
	typeName string
	archived bool
}

// dreamEdgeRow is one context_dream_links row (source, target, relationship,
// confidence, raw_confidence). scope/created_at are not cached — the CSR is
// pure topology + ordering keys.
type dreamEdgeRow struct {
	src, dst [16]byte
	rel      string
	conf     float64 // weighted confidence
	rawConf  float64 // raw_confidence (ordering key, §3.2 Nr. 1)
}

// structEdgeRow is one context_structural_links row (source, target,
// link_class, origin, created_at). Structural links carry NO confidence
// (1.0 by definition, 076) — created_at is the ordering key (M104).
type structEdgeRow struct {
	src, dst [16]byte
	class    string
	origin   string
	created  time.Time
}

// dreamEdge / structEdge are the resolved (NodeID-keyed) build-time edges after
// the universe join. Both endpoints are guaranteed present in the universe
// (dangling edges are dropped + counted before this stage).
type dreamEdge struct {
	src, dst uint32
	rel      uint8
	conf     uint16
	rawConf  uint16
}

type structEdge struct {
	src, dst uint32
	class    uint8
	origin   uint8
	created  uint32
}

// Build performs a full rebuild: three streams (block universe, dream links,
// structural links) read WITHOUT ORDER BY, assembled deterministically in Go.
// The Determinismus-Invariante holds by construction: the same DB yields a
// byte-equal Snapshot regardless of the (unordered) row arrival sequence —
// the UUID universe is sorted, all interning is over sorted distinct names,
// and every adjacency list is totally ordered (§3.2 Nr. 1). Only BuiltAt/Seq
// vary between builds; Fingerprint() covers exactly the deterministic content.
func Build(ctx context.Context, pool *pgxpool.Pool) (*Snapshot, error) {
	universe, err := readUniverse(ctx, pool)
	if err != nil {
		return nil, err
	}
	dream, err := readDreamEdges(ctx, pool)
	if err != nil {
		return nil, err
	}
	structs, err := readStructEdges(ctx, pool)
	if err != nil {
		return nil, err
	}
	snap, err := assemble(universe, dream, structs, true)
	if err != nil {
		return nil, err
	}
	snap.BuiltAt = time.Now()
	if d := snap.Stats.DreamDangling + snap.Stats.StructDangling; d > 0 {
		// Dangling = edge endpoint not in the universe. This is possible only
		// as a transient race (an edge inserted for a block deleted between the
		// universe read and the link read); skip + count, never panic (§4.1).
		slog.Warn("graphcache: dropped dangling edges during build",
			"dream_dangling", snap.Stats.DreamDangling,
			"struct_dangling", snap.Stats.StructDangling)
	}
	return snap, nil
}

// readUniverse streams the block universe WITHOUT ORDER BY (§4.1: an ORDER BY id
// @10M forces a full sort or 10M heap fetches; Go sorts the UUID array in ~1-2s).
func readUniverse(ctx context.Context, pool *pgxpool.Pool) ([]blockRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, scope::text, type_name, is_archived FROM context_blocks`)
	if err != nil {
		return nil, fmt.Errorf("graphcache: universe query: %w", err)
	}
	defer rows.Close()

	var out []blockRow
	for rows.Next() {
		var (
			id       pgtype.UUID
			br       blockRow
			typeName pgtype.Text // NULL-safe (pre-071 rows, defensive)
		)
		if err := rows.Scan(&id, &br.scope, &typeName, &br.archived); err != nil {
			return nil, fmt.Errorf("graphcache: universe scan: %w", err)
		}
		br.id = id.Bytes
		br.typeName = typeName.String
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphcache: universe rows: %w", err)
	}
	return out, nil
}

// readDreamEdges streams context_dream_links (no ORDER BY; supersedes is
// filtered later in assemble against GraphRels so the legend stays the sole
// source of truth for the exclusion).
func readDreamEdges(ctx context.Context, pool *pgxpool.Pool) ([]dreamEdgeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT source_block_id, target_block_id, relationship, confidence, raw_confidence
		 FROM context_dream_links`)
	if err != nil {
		return nil, fmt.Errorf("graphcache: dream query: %w", err)
	}
	defer rows.Close()

	var out []dreamEdgeRow
	for rows.Next() {
		var src, dst pgtype.UUID
		var r dreamEdgeRow
		if err := rows.Scan(&src, &dst, &r.rel, &r.conf, &r.rawConf); err != nil {
			return nil, fmt.Errorf("graphcache: dream scan: %w", err)
		}
		r.src, r.dst = src.Bytes, dst.Bytes
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphcache: dream rows: %w", err)
	}
	return out, nil
}

// readStructEdges streams context_structural_links (no ORDER BY).
func readStructEdges(ctx context.Context, pool *pgxpool.Pool) ([]structEdgeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT source_block_id, target_block_id, link_class, origin, created_at
		 FROM context_structural_links`)
	if err != nil {
		return nil, fmt.Errorf("graphcache: struct query: %w", err)
	}
	defer rows.Close()

	var out []structEdgeRow
	for rows.Next() {
		var src, dst pgtype.UUID
		var r structEdgeRow
		var created pgtype.Timestamptz
		if err := rows.Scan(&src, &dst, &r.class, &r.origin, &created); err != nil {
			return nil, fmt.Errorf("graphcache: struct scan: %w", err)
		}
		r.src, r.dst = src.Bytes, dst.Bytes
		r.created = created.Time
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphcache: struct rows: %w", err)
	}
	return out, nil
}

// assemble is the DB-free deterministic core — split out so every invariant is
// unit-testable without a database (the recall.validateMeta discipline). It
// takes the three raw streams in ANY order and produces the canonical Snapshot.
// sortAdj toggles the per-node adjacency sort: production passes true; the
// export_test unsorted seam passes false to prove the ordering-invariant check
// is non-vacuous (Fixture-Gate, §7 W05.1).
func assemble(universe []blockRow, dreamRows []dreamEdgeRow, structRows []structEdgeRow, sortAdj bool) (*Snapshot, error) {
	// (1) Total order over the universe: sort by raw UUID bytes (PK-unique ⇒
	// total). The index into this array is the NodeID everywhere else.
	slices.SortFunc(universe, func(a, b blockRow) int { return bytes.Compare(a.id[:], b.id[:]) })
	n := len(universe)

	uuids := make([][16]byte, n)
	idIndex := make(map[[16]byte]uint32, n)
	for i := range universe {
		uuids[i] = universe[i].id
		idIndex[universe[i].id] = uint32(i)
	}

	// (2) Intern scope + type names over SORTED distinct sets (order-independent
	// ⇒ deterministic). Overflow is a loud build error.
	scopeNames, scopeOf := internStrings(collectField(universe, func(b blockRow) string { return b.scope }))
	if len(scopeNames) > maxScopeType {
		return nil, ErrScopeOverflow
	}
	typeNames, typeOf := internStrings(collectField(universe, func(b blockRow) string { return b.typeName }))
	if len(typeNames) > maxScopeType {
		return nil, ErrTypeOverflow
	}

	scopeID := make([]uint16, n)
	typeID := make([]uint16, n)
	archived := newBitset(n)
	for i := range universe {
		scopeID[i] = uint16(scopeOf[universe[i].scope])
		typeID[i] = uint16(typeOf[universe[i].typeName])
		if universe[i].archived {
			archived.set(i)
		}
	}

	// (3) Intern structural class + origin (u8, cap 254).
	classNames, classOf := internStrings(collectStructField(structRows, func(r structEdgeRow) string { return r.class }))
	if len(classNames) > maxClassOrigin {
		return nil, ErrClassOverflow
	}
	originNames, originOf := internStrings(collectStructField(structRows, func(r structEdgeRow) string { return r.origin }))
	if len(originNames) > maxClassOrigin {
		return nil, ErrOriginOverflow
	}

	stats := BuildStats{Nodes: n}

	// (4) Resolve dream edges to NodeIDs. supersedes leaves the traversal CSR
	// (§3.2 Nr. 3) and enters the separate DISPLAY segment instead — Q2 shows it
	// and Q3 counts it, but no walk may ever reach it. The counter semantics of
	// SupersedesSkipped are unchanged (edges excluded from the traversal CSR);
	// SupersedesDisplay counts what made it into the display segment.
	dreamEdges := make([]dreamEdge, 0, len(dreamRows))
	supEdges := make([]dreamEdge, 0)
	for _, r := range dreamRows {
		if r.rel == supersededRel {
			stats.SupersedesSkipped++
			si, sok := idIndex[r.src]
			di, dok := idIndex[r.dst]
			if !sok || !dok {
				continue // dangling: not counted as DreamDangling (pre-W05.6 semantics)
			}
			supEdges = append(supEdges, dreamEdge{
				src: si, dst: di, rel: uint8(relIndex(supersededRel)),
				conf: ConfToFix(r.conf), rawConf: ConfToFix(r.rawConf),
			})
			continue
		}
		si, sok := idIndex[r.src]
		di, dok := idIndex[r.dst]
		if !sok || !dok {
			stats.DreamDangling++
			continue
		}
		ri := relIndex(r.rel)
		if ri < 0 {
			// Relationship outside the legend — skip defensively (same posture
			// as store.inducedEdges' unknown-rel guard). Counted as dangling.
			stats.DreamDangling++
			continue
		}
		dreamEdges = append(dreamEdges, dreamEdge{
			src: si, dst: di, rel: uint8(ri),
			conf: ConfToFix(r.conf), rawConf: ConfToFix(r.rawConf),
		})
	}
	stats.DreamEdges = len(dreamEdges)
	stats.SupersedesDisplay = len(supEdges)

	// (5) Resolve structural edges to NodeIDs, drop dangling.
	structEdges := make([]structEdge, 0, len(structRows))
	for _, r := range structRows {
		si, sok := idIndex[r.src]
		di, dok := idIndex[r.dst]
		if !sok || !dok {
			stats.StructDangling++
			continue
		}
		structEdges = append(structEdges, structEdge{
			src: si, dst: di,
			class:   uint8(classOf[r.class]),
			origin:  uint8(originOf[r.origin]),
			created: toUnix32(r.created),
		})
	}
	stats.StructEdges = len(structEdges)

	snap := &Snapshot{
		UUIDs:       uuids,
		ScopeID:     scopeID,
		TypeID:      typeID,
		Archived:    archived,
		scopeNames:  scopeNames,
		typeNames:   typeNames,
		classNames:  classNames,
		originNames: originNames,
		Dream: CSRPair{
			Fwd: buildDreamCSR(dreamEdges, n, false, sortAdj),
			Rev: buildDreamCSR(dreamEdges, n, true, sortAdj),
		},
		Struct: CSRPair{
			Fwd: buildStructCSR(structEdges, n, false, sortAdj),
			Rev: buildStructCSR(structEdges, n, true, sortAdj),
		},
		// Display segment (§3.2 Nr. 3): same CSR machinery, own arrays, no
		// neighbour accessor. Fwd carries Q2 (induced display edges), Rev exists
		// solely so Q3 counts the reverse dream rows the SQL degree legs count.
		supersedes: CSRPair{
			Fwd: buildDreamCSR(supEdges, n, false, sortAdj),
			Rev: buildDreamCSR(supEdges, n, true, sortAdj),
		},
		Stats: stats,
	}
	return snap, nil
}

// buildDreamCSR builds one directed dream CSR. reverse=false groups by source
// (targets = dst); reverse=true groups by dst (targets = src). When sortAdj is
// set, each node's adjacency is totally ordered by raw_confidence DESC, dst ASC
// (§3.2 Nr. 1 — mirrors idx_dream_links_graph_fwd/_rev, M050).
func buildDreamCSR(edges []dreamEdge, n int, reverse, sortAdj bool) CSR {
	key := func(e dreamEdge) uint32 {
		if reverse {
			return e.dst
		}
		return e.src
	}
	tgt := func(e dreamEdge) uint32 {
		if reverse {
			return e.src
		}
		return e.dst
	}

	offsets := make([]uint32, n+1)
	for _, e := range edges {
		offsets[key(e)+1]++
	}
	for i := 1; i <= n; i++ {
		offsets[i] += offsets[i-1]
	}

	m := len(edges)
	c := CSR{
		Offsets: offsets,
		Targets: make([]uint32, m),
		Rel:     make([]uint8, m),
		Conf:    make([]uint16, m),
		RawConf: make([]uint16, m),
	}
	cursor := make([]uint32, n)
	copy(cursor, offsets[:n])
	for _, e := range edges {
		k := key(e)
		p := cursor[k]
		cursor[k]++
		c.Targets[p] = tgt(e)
		c.Rel[p] = e.rel
		c.Conf[p] = e.conf
		c.RawConf[p] = e.rawConf
	}

	if sortAdj {
		for i := 0; i < n; i++ {
			sortDreamRange(c, offsets[i], offsets[i+1])
		}
	}
	return c
}

// sortDreamRange insertion-sorts [lo,hi) by RawConf DESC, then Targets ASC.
// Insertion sort is intentional: adjacency lists are tiny (mean < 6, §6.2),
// stable, and alloc-free. The dst tiebreak makes the order TOTAL (dream PK is
// (source,target) ⇒ dst unique per source), so the result is independent of
// the fill order — the Determinismus-Invariante.
func sortDreamRange(c CSR, lo, hi uint32) {
	for i := lo + 1; i < hi; i++ {
		for j := i; j > lo && dreamLess(c, j, j-1); j-- {
			c.Targets[j], c.Targets[j-1] = c.Targets[j-1], c.Targets[j]
			c.Rel[j], c.Rel[j-1] = c.Rel[j-1], c.Rel[j]
			c.Conf[j], c.Conf[j-1] = c.Conf[j-1], c.Conf[j]
			c.RawConf[j], c.RawConf[j-1] = c.RawConf[j-1], c.RawConf[j]
		}
	}
}

func dreamLess(c CSR, a, b uint32) bool {
	if c.RawConf[a] != c.RawConf[b] {
		return c.RawConf[a] > c.RawConf[b] // DESC
	}
	return c.Targets[a] < c.Targets[b] // total-order tiebreak
}

// buildStructCSR builds one directed structural CSR. Ordering (sortAdj): created
// DESC, dst ASC, class ASC (§3.2 Nr. 1 — mirrors idx_struct_links_graph_fwd/_rev,
// M104; the class tiebreak covers the (source,target,link_class) PK).
func buildStructCSR(edges []structEdge, n int, reverse, sortAdj bool) CSR {
	key := func(e structEdge) uint32 {
		if reverse {
			return e.dst
		}
		return e.src
	}
	tgt := func(e structEdge) uint32 {
		if reverse {
			return e.src
		}
		return e.dst
	}

	offsets := make([]uint32, n+1)
	for _, e := range edges {
		offsets[key(e)+1]++
	}
	for i := 1; i <= n; i++ {
		offsets[i] += offsets[i-1]
	}

	m := len(edges)
	c := CSR{
		Offsets:  offsets,
		Targets:  make([]uint32, m),
		ClassID:  make([]uint8, m),
		OriginID: make([]uint8, m),
		Created:  make([]uint32, m),
	}
	cursor := make([]uint32, n)
	copy(cursor, offsets[:n])
	for _, e := range edges {
		k := key(e)
		p := cursor[k]
		cursor[k]++
		c.Targets[p] = tgt(e)
		c.ClassID[p] = e.class
		c.OriginID[p] = e.origin
		c.Created[p] = e.created
	}

	if sortAdj {
		for i := 0; i < n; i++ {
			sortStructRange(c, offsets[i], offsets[i+1])
		}
	}
	return c
}

func sortStructRange(c CSR, lo, hi uint32) {
	for i := lo + 1; i < hi; i++ {
		for j := i; j > lo && structLess(c, j, j-1); j-- {
			c.Targets[j], c.Targets[j-1] = c.Targets[j-1], c.Targets[j]
			c.ClassID[j], c.ClassID[j-1] = c.ClassID[j-1], c.ClassID[j]
			c.OriginID[j], c.OriginID[j-1] = c.OriginID[j-1], c.OriginID[j]
			c.Created[j], c.Created[j-1] = c.Created[j-1], c.Created[j]
		}
	}
}

func structLess(c CSR, a, b uint32) bool {
	if c.Created[a] != c.Created[b] {
		return c.Created[a] > c.Created[b] // DESC
	}
	if c.Targets[a] != c.Targets[b] {
		return c.Targets[a] < c.Targets[b]
	}
	return c.ClassID[a] < c.ClassID[b] // total-order tiebreak (PK class dimension)
}

// relIndex maps a relationship name to its GraphRels index, or -1 if unknown.
func relIndex(rel string) int {
	for i, r := range GraphRels {
		if r == rel {
			return i
		}
	}
	return -1
}

// internStrings returns the sorted distinct set of names and a name→id map.
// Sorting makes the assignment order-independent — the interning determinism
// rule (§3.2, §4.1): the same distinct set always interns to the same ids.
func internStrings(all []string) ([]string, map[string]int) {
	seen := make(map[string]struct{}, len(all))
	for _, s := range all {
		seen[s] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for s := range seen {
		names = append(names, s)
	}
	slices.Sort(names)
	idOf := make(map[string]int, len(names))
	for i, s := range names {
		idOf[s] = i
	}
	return names, idOf
}

func collectField(rows []blockRow, f func(blockRow) string) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = f(rows[i])
	}
	return out
}

func collectStructField(rows []structEdgeRow, f func(structEdgeRow) string) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = f(rows[i])
	}
	return out
}

// toUnix32 clamps a timestamp to a uint32 Unix-second ordering key. created_at
// is only ever an ordering/merge key, never rendered from the cache, so the
// 2106 wrap and sub-second truncation are irrelevant to correctness. Negative
// (pre-1970) and overflow values clamp to the representable range.
func toUnix32(t time.Time) uint32 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	if s > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(s)
}
