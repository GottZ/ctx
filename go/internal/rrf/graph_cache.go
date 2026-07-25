// Package rrf — the W05.7 GraphExpand cache arm (design/05 §4.2/§4.4/§5.1).
//
// What moves to the snapshot: the EDGE FETCH of fetchNeighbors — the edge_dir
// UNION, the per-type raw_confidence gate, the per-seed cap and the hub-damping
// degree. What does NOT move, by design:
//
//   - The HYDRATE stays SQL, PER HOP: the snapshot is a topology accelerator,
//     not a visibility authority (§4.4). It runs the SAME visibility.Predicate
//     the SQL arm's JOIN carries, so a candidate only becomes a graphEdge after
//     the LIVE row admitted it.
//   - Seed selection, the hop>=2 frontier with its T41 leaf check and visited
//     set, and the fusion are the SHARED traversal body (expandWith). A second
//     copy of a visibility or leaf check would be a second truth; there is none.
//
// Three structural properties carry the safety of this arm:
//
//  1. GA5 isolation is STRUCTURAL, not conventional: the arm's snapshot field
//     has type dreamGraph — a three-method interface over the DREAM adjacency
//     only. StructNeighbors, InducedEdges, Degree and the unexported supersedes
//     segment are not reachable through it, so "structural links never inject
//     into retrieval" holds by construction and not by a filter someone could
//     forget. The narrowing happens exactly once, in newExpandCacheArm.
//
//  2. Hop synchrony (§4.2 Punkt 3, normative for HopDepth>=2): one fetch call
//     is walk(hop n) -> hydrate(hop n). expandWith builds the frontier for hop
//     n+1 from the returned edges, i.e. from DB-CONFIRMED neighbours, and runs
//     the T41 leaf check on the HYDRATED scope. A multi-hop walk before the
//     hydrate would reproduce break path §5.1 Nr. 2 (traversal THROUGH a
//     foreign-scope or grant-only bridge) on the expand path.
//
//  3. The hub-damping degree is counted IN THE WALK over the gate-passing,
//     seed-incident edge set — never from raw CSR offsets. That is exact parity
//     with the SQL window COUNT(*) OVER (PARTITION BY neighbor_id) inside the
//     `ranked` CTE (which sits over `gated`, BEFORE the visibility JOIN): the
//     value is caller-local, derived from the caller's own seeds. A raw CSR
//     degree would be a cross-scope channel — the injection magnitude, and thus
//     the response, would become an observable function of foreign private link
//     counts on a shared hub (the very thing store/graph.go:823-825 forbids).
package rrf

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/visibility"
)

// ExpandCache is the OPTIONAL W05.7 cache arm handed in by the caller
// (design/05 §4.2/§4.6). A zero value (Snapshot nil) is the SQL path,
// byte-identical to pre-W05.7 — the permanent fallback AND the differential
// oracle. The caller (handler/query.go) decides whether the arm may run at all:
// it passes a Snapshot ONLY when the cache state is Fresh AND
// graph_cache.serve_expand is on. This type carries no policy, only the seam —
// rrf imports neither config nor events (SelectorPolicy pattern).
type ExpandCache struct {
	// Snapshot is the live CSR snapshot; nil = SQL path.
	Snapshot *graphcache.Snapshot
	// Age is the snapshot's age, reported as BudgetReport.CacheAge.
	Age time.Duration
}

// dreamGraph is the ENTIRE snapshot surface the expand arm can reach — the GA5
// seam (§3.2 rationale c). Three methods: entry (NodeID), rendering (NodeUUID)
// and the DREAM adjacency. Adding a structural or supersedes accessor here is
// the only way this arm could ever surface a non-dream edge, which makes the
// isolation invariant reviewable in one place instead of at every walk site.
type dreamGraph interface {
	NodeID(id [16]byte) (uint32, bool)
	NodeUUID(n uint32) ([16]byte, bool)
	DreamNeighbors(n uint32, dir graphcache.Direction) graphcache.EdgeSlice
}

// *graphcache.Snapshot is the only production implementation.
var _ dreamGraph = (*graphcache.Snapshot)(nil)

// errExpandCacheStale is the INTERNAL signal that the cache arm cannot answer
// this request (a seed or frontier block is not in the snapshot — younger than
// the last build). It never escapes graphExpandDispatch: the request restarts
// COMPLETELY on the SQL arm (§4.2 — no partial fallback, no merge special
// cases), and the report records TravCacheStale.
var errExpandCacheStale = errors.New("rrf: expand cache snapshot stale for this request")

// expandCacheArm is the snapshot arm of neighborFetcher.
type expandCacheArm struct {
	dream dreamGraph
	pool  *pgxpool.Pool

	readScopes      []string
	grantedBlockIDs []string
	visibleTypes    []string
	cfg             GraphConfig

	// u16 gate thresholds, precomputed once per request. CEIL side of the
	// §3.2 Nr. 2 rule (edge weights FLOOR, thresholds CEIL) — the cache gate is
	// at least as strict as SQL's `raw_confidence >= $3`, never laxer.
	minConf    uint16
	minConfRec uint16
}

// newExpandCacheArm narrows the snapshot to the dream adjacency (the GA5 cut)
// and precomputes the request-constant gates.
func newExpandCacheArm(snap *graphcache.Snapshot, pool *pgxpool.Pool,
	readScopes, grantedBlockIDs, visibleTypes []string, cfg GraphConfig) *expandCacheArm {
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL (T40a)
	}
	return &expandCacheArm{
		dream:           snap, // the ONLY widening-to-narrowing assignment in this arm
		pool:            pool,
		readScopes:      readScopes,
		grantedBlockIDs: grantedBlockIDs,
		visibleTypes:    visibleTypes,
		cfg:             cfg,
		minConf:         graphcache.ThresholdToFix(cfg.MinConfidence),
		minConfRec:      graphcache.ThresholdToFix(cfg.MinConfidenceRecurrent),
	}
}

// expandWalkEdge is one capped, gate-passing seed edge before the hydrate.
type expandWalkEdge struct {
	seed int    // index into the seedIDs of this hop
	node uint32 // neighbour NodeID
	rel  uint8  // index into graphcache.GraphRels
}

// fetch is the neighborFetcher implementation: walk -> hydrate -> edges.
//
// Fail-closed entry is SELF-CONTAINED (§5.2, the discipline fetchNeighbors and
// structuralHopNeighbors both repeat): an empty type allowlist is a loud Go
// error here too, never a silently empty hop. The caller's fail-open mantle
// turns it into "keep the pre-expansion results" plus a warning, exactly as on
// the SQL arm.
func (a *expandCacheArm) fetch(ctx context.Context, seedIDs []string, hopDecay float64) ([]graphEdge, error) {
	if len(a.visibleTypes) == 0 {
		return nil, fmt.Errorf("rrf: empty visible-types allowlist (block-type registry not wired?)")
	}
	seeds := make([]uint32, 0, len(seedIDs))
	for _, id := range seedIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			// Not the arm's error to report: the SQL arm's uuid[] cast produces
			// the authoritative message, so decline and let the fallback run.
			return nil, errExpandCacheStale
		}
		n, ok := a.dream.NodeID(u)
		if !ok {
			// A seed the snapshot does not know (block younger than the last
			// build): the arm cannot answer this request. NO partial fallback —
			// the whole request restarts on SQL (§4.2).
			return nil, errExpandCacheStale
		}
		seeds = append(seeds, n)
	}

	capped, degree := a.walk(seeds)
	if len(capped) == 0 {
		return nil, nil
	}
	ids, err := a.candidateIDs(capped)
	if err != nil {
		return nil, err
	}
	hydrated, err := a.hydrate(ctx, ids)
	if err != nil {
		return nil, err
	}

	edges := make([]graphEdge, 0, len(capped))
	for _, c := range capped {
		nb, ok := hydrated[c.node]
		if !ok {
			continue // the recheck rejected it — archived / foreign / type-invisible / gone
		}
		edges = append(edges, graphEdge{
			seedID:         seedIDs[c.seed],
			relationship:   graphcache.GraphRels[c.rel],
			neighbor:       *nb,
			neighborDegree: degree[c.node],
			hopDecay:       hopDecay,
		})
	}
	return edges, nil
}

// walk reproduces the `edge_dir` -> `gated` -> `ranked` chain of fetchNeighbors
// on the CSR, and returns (a) the per-seed capped candidates in seed order and
// (b) the hub-damping degree per neighbour.
//
// Two properties are load-bearing and easy to break:
//
//   - The cap window is "the first k AFTER the gate" over the RawConf-DESC
//     merge of the walked legs — and NOTHING else. In particular there is NO
//     scope/type hint pre-filter here (unlike the ego arm, where the refill loop
//     makes hints a pure pruning device): the SQL cap is a ROW_NUMBER over the
//     gated set BEFORE the visibility JOIN, so a hint that skipped a candidate
//     would let a LOWER-confidence edge slide into the cap window that SQL never
//     delivers. Hints would change the answer, not just the work.
//   - The degree counts the FULL gated, seed-incident edge set — the walk
//     therefore continues past a filled cap. That is the SQL window COUNT: it
//     partitions `gated`, not the capped rows, so edges beyond a seed's cap and
//     edges to invisible neighbours DO count, while foreign edges that touch no
//     caller seed do NOT (§4.2 Punkt 2 — caller-local by construction).
func (a *expandCacheArm) walk(seeds []uint32) ([]expandWalkEdge, map[uint32]int) {
	var capped []expandWalkEdge
	degree := make(map[uint32]int)
	for si, n := range seeds {
		c := &dreamCursor{a: a, fwd: a.dream.DreamNeighbors(n, graphcache.Forward)}
		if !a.cfg.Directed {
			// Undirected = the inbound leg of the SQL UNION ALL, re-oriented to
			// (seed, neighbour) — the reverse CSR IS that re-orientation.
			c.rev = a.dream.DreamNeighbors(n, graphcache.Reverse)
		}
		taken := 0
		for {
			cand, ok := c.next()
			if !ok {
				break
			}
			degree[cand.node]++
			if taken < a.cfg.PerSeedCap {
				capped = append(capped, expandWalkEdge{seed: si, node: cand.node, rel: cand.rel})
				taken++
			}
		}
	}
	return capped, degree
}

// candidateIDs renders the distinct capped candidates back to UUID text for the
// hydrate, preserving first-seen order (determinism, not a contract).
func (a *expandCacheArm) candidateIDs(capped []expandWalkEdge) ([]string, error) {
	seen := make(map[uint32]bool, len(capped))
	ids := make([]string, 0, len(capped))
	for _, c := range capped {
		if seen[c.node] {
			continue
		}
		seen[c.node] = true
		raw, ok := a.dream.NodeUUID(c.node)
		if !ok {
			return nil, errExpandCacheStale // defensive: NodeID gave us this id
		}
		ids = append(ids, uuid.UUID(raw).String())
	}
	return ids, nil
}

// hydrate is the per-hop batched visibility recheck — the ONLY authority over
// what may become a graphEdge (§4.4). It carries EXACTLY the neighbour-side
// predicate of the SQL arm's JOIN: THE shared fragment visibility.Predicate
// (NOT archived, type_name = ANY(visibleTypes) registry allowlist, and the
// parenthesised scope/grant group), gated on context_blocks.scope — never on
// context_dream_links.scope. Ids the query does not return are invisible
// (fail-closed direction).
func (a *expandCacheArm) hydrate(ctx context.Context, ids []string) (map[uint32]*SearchResult, error) {
	q := `
SELECT cb.id::text,
       cb.title::text,
       cb.category,
       cb.tags,
       cb.content,
       cb.scope::text,
       cb.updated_at
FROM context_blocks cb
WHERE cb.id = ANY($1::uuid[])
  AND ` + visibility.Predicate("cb", "$4", "$2", "$3")

	rows, err := a.pool.Query(ctx, q,
		ids,               // $1 candidate ids
		a.readScopes,      // $2
		a.grantedBlockIDs, // $3 block-grant OR-arm (T40a, neighbour side)
		a.visibleTypes,    // $4 registry type allowlist (T6)
	)
	if err != nil {
		return nil, fmt.Errorf("hydrate dream neighbors: %w", err)
	}
	defer rows.Close()

	out := make(map[uint32]*SearchResult, len(ids))
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ID, &r.Title, &r.Category, &r.Tags, &r.Content, &r.Scope, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan dream neighbor row: %w", err)
		}
		u, perr := uuid.Parse(r.ID)
		if perr != nil {
			return nil, fmt.Errorf("parse dream neighbor id: %w", perr)
		}
		n, ok := a.dream.NodeID(u)
		if !ok {
			continue // defensive: we asked with ids the snapshot produced
		}
		row := r
		out[n] = &row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dream neighbor rows: %w", err)
	}
	return out, nil
}

// dreamCand is one gate-passing candidate of the adjacency merge.
type dreamCand struct {
	node uint32
	rel  uint8
}

// dreamCursor walks ONE seed's forward (+ reverse when undirected) dream
// adjacency in RawConf-DESC order, lazily, applying the per-type confidence
// gate. Both CSR sides are pre-sorted by RawConf DESC (§3.2 Nr. 1), so the
// merge yields exactly the order the SQL window function ranks by
// (`ORDER BY raw_confidence DESC` over the UNION) — "take the first k" IS the
// per-seed cap. Lazy matters: a 10^4-degree hub must not be materialised.
type dreamCursor struct {
	a        *expandCacheArm
	fwd, rev graphcache.EdgeSlice
	fi, ri   int
}

// next yields the next gate-passing candidate, or ok=false at exhaustion.
func (c *dreamCursor) next() (dreamCand, bool) {
	for {
		fk, fok := keyAt(c.fwd, c.fi)
		rk, rok := keyAt(c.rev, c.ri)
		if !fok && !rok {
			return dreamCand{}, false
		}
		es, i := c.fwd, c.fi
		if !fok || (rok && rk > fk) {
			es, i = c.rev, c.ri
			c.ri++
		} else {
			c.fi++
		}
		if cand, ok := c.a.admit(es, i); ok {
			return cand, true
		}
	}
}

// keyAt reads the RawConf ordering key at position i, or ok=false past the end.
func keyAt(es graphcache.EdgeSlice, i int) (uint16, bool) {
	if i >= len(es.Targets) {
		return 0, false
	}
	return es.RawConf[i], true
}

// admit applies the per-type raw_confidence gate — the `gated` CTE.
//
// The supersedes exclusion is STRUCTURAL here, not a filter: the dream CSR
// holds no supersedes edges at all (they live in the snapshot's unexported
// display segment, §3.2 Nr. 3), and this arm cannot reach that segment through
// dreamGraph. The bounds check below is defensive against a legend/CSR skew,
// not a relationship policy.
func (a *expandCacheArm) admit(es graphcache.EdgeSlice, i int) (dreamCand, bool) {
	rel := es.Rel[i]
	if int(rel) >= len(graphcache.GraphRels) {
		return dreamCand{}, false
	}
	name := graphcache.GraphRels[rel]
	if name == "supersedes" {
		return dreamCand{}, false // unreachable by construction; kept fail-closed
	}
	gate := a.minConf
	if name == "recurrent" {
		gate = a.minConfRec
	}
	if es.RawConf[i] < gate {
		return dreamCand{}, false
	}
	return dreamCand{node: es.Targets[i], rel: rel}, true
}
