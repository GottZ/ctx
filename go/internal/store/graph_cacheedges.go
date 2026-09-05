// graph_cacheedges.go — the W05.6 post-traversal cache stages: Q2/Q2s (induced edges)
// and Q3 (degrees) served from the CSR snapshot (design/05 §3.2 Nr. 3, §4.2,
// §8 E-05-3).
//
// What this file does NOT contain, deliberately:
//
//   - No second copy of a visibility check. Both stages run over the node set
//     runEgoTraversal already built from DB-CONFIRMED rows; an induced edge is
//     by construction an edge between two blocks the live predicate admitted, so
//     the Q2 invariant ("scope-safe by construction — every endpoint passed the
//     visibility triple") holds verbatim on this arm.
//   - No second copy of the budget arbitration or the legend construction: the
//     cache edges are handed to the SAME arbitrateEdgeBudget and mapStructEdges
//     the SQL arm uses. Only the row SOURCE differs.
//
// The two documented, bounded deviations from the SQL stages (both fail-closed
// in direction, both asserted by the W05.6 gates):
//
//  1. ORDER granularity. Q2 orders by the exact float `confidence DESC`, Q2s by
//     `created_at DESC` at microsecond resolution. The cache orders by the u16
//     confidence fixpoint and by created_at truncated to Unix SECONDS. Two edges
//     that differ only below those resolutions may swap places, which can change
//     WHICH edge falls off at p.EdgeLimit. The delivered COUNT and the
//     truncation flag are unaffected.
//  2. Degree (E-05-3(2)) — see egoCacheHops.degrees.
package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/graphcache"
)

// edges is the egoHopFetcher Q2/Q2s stage on the snapshot arm.
//
// One InducedEdges pass over the delivered node set yields BOTH classes plus the
// supersedes display segment; everything after that is the SQL arm's own code
// path (gates → order → EdgeLimit → arbitrateEdgeBudget → mapStructEdges).
func (f *egoCacheHops) edges(_ context.Context, ids []string, index map[string]int) (egoEdgeSet, error) {
	nodeIDs, err := f.nodeIDs(ids)
	if err != nil {
		// A delivered node the snapshot does not know: decline the WHOLE request
		// (§4.2, no mixed form) — EgoGraphCached re-runs it on SQL.
		return egoEdgeSet{}, err
	}
	induced := f.snap.InducedEdges(nodeIDs)

	dream, dreamTrunc := f.inducedDreamEdges(induced.Dream, index)
	if f.structSkip {
		// §4.6 sentinel: no structural classes selected. Byte-identical to
		// resolveStructEdges' short circuit — the dream edges pass through
		// UNARBITRATED (arbitrateEdgeBudget never runs), legends stay nil.
		return egoEdgeSet{Dream: dream, DreamTrunc: dreamTrunc}, nil
	}

	structRows, structOverflow := f.inducedStructRows(induced.Struct)
	dream, structRows, arbTruncated := arbitrateEdgeBudget(dream, structRows, f.p.EdgeLimit)
	structEdges, structRels, origins := mapStructEdges(structRows, index)
	return egoEdgeSet{
		Dream: dream, Struct: structEdges, StructRels: structRels, Origins: origins,
		DreamTrunc: dreamTrunc, StructTrunc: structOverflow || arbTruncated,
	}, nil
}

// inducedDreamEdges is the cache Q2: gate, order, cut at p.EdgeLimit, map to
// response-local indexes.
//
// The class gate is displayRelAllow (ALL FIVE relationships incl. supersedes),
// NOT the traversal gate — Q2's bind is displayClasses(p.LinkClasses). The
// confidence gate is the u16 comparison with the §3.2 Nr. 2 rule (edge FLOOR vs.
// threshold CEIL): at least as strict as SQL's `confidence >= $2`, never laxer.
// The order reproduces `ORDER BY confidence DESC, source_block_id,
// target_block_id`: NodeIDs are the index into the byte-sorted UUID array, so
// NodeID ASC IS uuid ASC (PostgreSQL compares uuid as 16 raw bytes).
func (f *egoCacheHops) inducedDreamEdges(in []graphcache.InducedDreamEdge, index map[string]int) ([]GraphEdge, bool) {
	if f.dreamDisplaySkip {
		return nil, false
	}
	kept := make([]graphcache.InducedDreamEdge, 0, len(in))
	for _, e := range in {
		if e.Conf < f.minConf {
			continue
		}
		if f.displayRelAllow != nil {
			r := int(e.Rel)
			if r >= len(graphcache.GraphRels) || !f.displayRelAllow[graphcache.GraphRels[r]] {
				continue
			}
		}
		kept = append(kept, e)
	}
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Conf != b.Conf {
			return a.Conf > b.Conf
		}
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		return a.Dst < b.Dst
	})

	truncated := false
	if len(kept) > f.p.EdgeLimit {
		kept = kept[:f.p.EdgeLimit]
		truncated = true
	}

	out := make([]GraphEdge, 0, len(kept))
	for _, e := range kept {
		si, sok := index[f.uuidOf(e.Src)]
		di, dok := index[f.uuidOf(e.Dst)]
		if !sok || !dok {
			continue // defensive, mirroring the SQL mapper: both endpoints are in the set
		}
		out = append(out, GraphEdge{
			Src: si, Dst: di, Rel: int(e.Rel),
			Conf: graphcache.FixToConf(e.Conf),
		})
	}
	return out, truncated
}

// inducedStructRows is the cache Q2s: class gate, newest-first order, cut at
// p.EdgeLimit. It returns the SQL arm's own row type so arbitrateEdgeBudget and
// mapStructEdges consume it unchanged — the legends are therefore built by the
// one existing implementation (sorted distinct sets of the DELIVERED rows).
//
// The order reproduces `ORDER BY created_at DESC, source_block_id,
// target_block_id, link_class`: ClassID is the index into the sorted distinct
// class-name array, so ClassID ASC IS link_class ASC.
func (f *egoCacheHops) inducedStructRows(in []graphcache.InducedStructEdge) ([]structEdgeRow, bool) {
	kept := make([]graphcache.InducedStructEdge, 0, len(in))
	for _, e := range in {
		if !f.structClassAllowed(e.ClassID) {
			continue
		}
		kept = append(kept, e)
	}
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Created != b.Created {
			return a.Created > b.Created
		}
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		return a.ClassID < b.ClassID
	})

	truncated := false
	if len(kept) > f.p.EdgeLimit {
		kept = kept[:f.p.EdgeLimit]
		truncated = true
	}

	rows := make([]structEdgeRow, 0, len(kept))
	for _, e := range kept {
		rows = append(rows, structEdgeRow{
			src:    f.uuidOf(e.Src),
			dst:    f.uuidOf(e.Dst),
			class:  f.snap.ClassName(e.ClassID),
			origin: f.snap.OriginName(e.OriginID),
		})
	}
	return rows, truncated
}

// degrees is the egoHopFetcher Q3 stage on the snapshot arm (E-05-3(a)).
//
// The value is the HINT-FILTERED degree: Snapshot.Degree walks the six adjacency
// legs and keeps neighbours whose scope/type/archived HINTS pass, with the same
// counting semantics as fillDegrees (link ROWS per direction per class, not
// distinct neighbours — the client badge math `degree - loadedDeg` depends on
// it) and the same DegreeHitCap=201 contract that renders as "200+".
//
// The three E-05-3 hardenings, all wired here:
//
//  1. MakeDegreeHints filters TypeID against the PER-REQUEST allowlist, not just
//     scope — fillDegrees applies the full VisibilityPredicate per leg including
//     the type conjunct, so without it the hint degree would count type-invisible
//     neighbours (retrieval=excluded checkpoint blocks, M107).
//  2. The residual delta is DECLARED and BOUNDED, never equated with the SQL
//     value: (i) stale scope/archived hints miscount until the next rebuild,
//     capped by MaxStaleness since the attribute-flip trigger (§3.1 Nr. 5);
//     (ii) grant-only neighbours are missing entirely — the snapshot holds no
//     grants (§3.2 Nr. 5) — so the count UNDER-reports, the fail-closed
//     direction. This is the one declared exception to the §4.4 rule.
//  3. The walk is capped by DegreeWalkBudget (config, hot): past it the degree is
//     a lower bound, exactly as DegreeScanBudget already makes the SQL degree one.
//
// What never happens: the RAW degree (offset differences, no hints) leaving the
// process. It is a cross-scope existence oracle — the raw degree of a shared hub
// counts foreign private links (store/graph.go fillDegrees header). The gate
// TestEgoCache_DegreeOracle_ForeignEdgesInvisible pins that, red against the raw
// value.
func (f *egoCacheHops) degrees(_ context.Context, _ []string, nodes []GraphNode) error {
	hints := f.snap.MakeDegreeHints(f.readScopes, f.visibleTypes, f.degreeWalkBudget)
	hints.HitCap = DegreeHitCap
	for i := range nodes {
		n, err := f.nodeID(nodes[i].ID)
		if err != nil {
			return err
		}
		deg, _ := f.snap.Degree(n, &hints)
		nodes[i].Degree = deg
	}
	return nil
}

// nodeID resolves ONE delivered node id to its NodeID; an id the snapshot does
// not know yields errEgoCacheStale (complete SQL fallback, §4.2).
func (f *egoCacheHops) nodeID(id string) (uint32, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return 0, fmt.Errorf("store: ego cache node id %q: %w", id, err)
	}
	n, ok := f.snap.NodeID(u)
	if !ok {
		return 0, errEgoCacheStale
	}
	return n, nil
}

// nodeIDs resolves the delivered node set.
func (f *egoCacheHops) nodeIDs(ids []string) ([]uint32, error) {
	out := make([]uint32, 0, len(ids))
	for _, id := range ids {
		n, err := f.nodeID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
