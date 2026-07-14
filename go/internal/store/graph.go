// Package store — ego-subgraph traversal over context_dream_links (F5-W1).
//
// EgoGraph implements the server side of the graph viewer: a BFS hop loop in
// Go (fetchNeighbors discipline, rrf/graph.go) with the canonical visibility
// triple applied IN every hop leg and IN the per-node cap legs — never only at
// the end. Rationale (design/05-graph-viewer.md §6.1/§6.2):
//
//   - Bridge leak: filtering only the final node list would traverse THROUGH
//     invisible blocks and deliver visible nodes that are reachable only via a
//     foreign private bridge — leaking the existence of private links.
//   - Cap starvation / counting channel: granting cap slots before visibility
//     lets invisible high-raw-confidence edges eat the per-node cap (hub yields
//     0 neighbors despite visible ones beyond the cap) and makes the count of
//     foreign private links probeable by varying per_node_cap.
//
// Deliberate, documented deviation from rrf.fetchNeighbors (which caps via
// seed_rn BEFORE the block join): the internal retrieval boost is not
// probeable (no cap param, no enumerated neighbor list, no degree badge);
// the viewer quantifies the signal, so slots must be granted visibility-clean.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Degree budgets: policy values with a single definition, passed to SQL as
// bind parameters (mechanism = code, policy = data). Deliberately NOT API
// parameters (design §3.3 Q3).
const (
	// DegreeScanBudget caps how many RAW link rows per direction PER CLASS
	// (dream/structural — four Q3 legs since GB4, design/01 §4.7: worst case
	// factor ≤2 over the dream-only baseline, ledgered in §6.3) the visible-
	// degree count scans BEFORE the block visibility join. Without it a
	// degree-10^4 hub whose neighborhood is mostly invisible to the caller
	// (normal after scope promotion, and the friend-tenant default at 1M+
	// scale) would pay a block PK lookup per raw edge just to report a tiny
	// degree — per node, per response, per click.
	DegreeScanBudget = 1000
	// DegreeHitCap caps the visible-neighbor count itself; a value of 201
	// renders client-side as "200+". It stops early in the visible-rich case.
	DegreeHitCap = 201
)

// GraphRels is the fixed relationship legend for the wire format: an edge's
// rel index points into this slice. The first four entries are the positive
// (traversable) types; supersedes is display-only and never traversed.
var GraphRels = []string{"topical", "factual", "causal", "recurrent", "supersedes"}

// graphRelIndex maps relationship name → index in GraphRels.
var graphRelIndex = func() map[string]int {
	m := make(map[string]int, len(GraphRels))
	for i, r := range GraphRels {
		m[r] = i
	}
	return m
}()

// ErrNotVisible is returned by EgoGraph when the focus block does not exist
// OR is not visible to the caller — deliberately ONE error for both cases so
// the handler answers an indistinguishable 404 (no existence oracle across
// scope boundaries, design §6.5).
var ErrNotVisible = errors.New("store: block not found or not visible")

// GraphNode is one node of the ego subgraph. Deliberately WITHOUT content
// (RAM + privacy — full content loads lazily via the scope-checked get API).
type GraphNode struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"` // server-side capped at 120 chars
	Category  string    `json:"category"`
	Scope     string    `json:"scope"`
	Degree    int       `json:"degree"` // visible degree, 201 = "200+"
	Hop       int       `json:"hop"`    // BFS distance from focus
	CreatedAt time.Time `json:"created_at"`
}

// GraphEdge marshals as the compact tuple [srcIdx, dstIdx, relIdx, conf].
// Indexes are response-local (into nodes / GraphRels).
type GraphEdge struct {
	Src, Dst, Rel int
	Conf          float64
}

// MarshalJSON renders the fixed-width tuple. Confidence is rounded to three
// decimals — REAL float32 marshaled raw would emit 0.8299999833106995-noise.
func (e GraphEdge) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "[%d,%d,%d,%.3f]", e.Src, e.Dst, e.Rel, e.Conf), nil
}

// StructGraphEdge is one structural fact edge as the all-integer wire tuple
// [srcIdx, dstIdx, classIdx, originIdx] — src/dst point into nodes (same
// response-local indexes as GraphEdge), classIdx into StructRels, originIdx
// into Origins. Deliberately NO confidence slot: structural links are 1.0 by
// definition (076), a positional slot for a constant would be dead wire
// weight at the 1M+ target. Tuple positions are only ever APPENDED, never
// reinterpreted — clients destructure prefixes (design/02 §4.1).
type StructGraphEdge struct {
	Src, Dst, Class, Origin int
}

// MarshalJSON renders the fixed-width integer tuple.
func (e StructGraphEdge) MarshalJSON() ([]byte, error) {
	return fmt.Appendf(nil, "[%d,%d,%d,%d]", e.Src, e.Dst, e.Class, e.Origin), nil
}

// EgoParams are the validated request parameters (ceilings enforced in the
// handler; out-of-range is a 400 there, never silently clamped).
type EgoParams struct {
	Focus         string
	Hops          int      // 1..3
	PerNodeCap    int      // 1..100
	Limit         int      // 1..1500 nodes (G39: ceiling lowered from 5000, see handler)
	EdgeLimit     int      // 1..20000 induced edges
	MinConfidence float64  // gate on weighted confidence (traversal + induced)
	LinkClasses   []string // dream classes: nil = all five; non-nil-EMPTY = none (§4.6)
	StructClasses []string // structural classes: nil = all; non-nil-EMPTY = none (§4.6)
	Categories    []string // nil = all
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
}

// EgoResult is the store-INTERNAL result. The wire envelope is built by the
// handler (egoResponse) — three places, ONE shape: design §3.1 example ==
// handler egoResponse == client EgoResponse.
type EgoResult struct {
	Focus string
	Rels  []string // legend for edge rel indexes
	// StructRels/Origins are the response-local legends for StructEdges
	// (delivered classes/origins only, dynamic — NOT the fixed dream five).
	// Empty until the structural traversal waves (GB1–GB3) populate them;
	// the handler marshals them as [] unconditionally (GA8).
	StructRels  []string
	Origins     []string
	Nodes       []GraphNode
	Edges       []GraphEdge
	StructEdges []StructGraphEdge
	Truncated   bool
}

// hopCandidate is one Q1 row: a visible, filter-passing neighbor with the
// confidence of its strongest edge into the frontier (ordering key only —
// it is not part of the node payload).
type hopCandidate struct {
	node GraphNode
	conf float64
}

// isGrantOnlyVisible reports whether a block that has ALREADY reached the node
// set is visible ONLY via a block-grant (its scope is not in readScopes). Such a
// block is a leaf: visible, but never a hop seed — traversing FROM it would let a
// row-level grant pull the grantee's OWN in-scope blocks (and the existence of
// foreign private link chains) into the response over the grant bridge.
//
// TENANT-DECISION(block-grant-graph-traversal): Grant-Block = Blatt (sichtbar, nicht
//   traversierbar) in allen drei Seed-Quellen — EgoGraph-frontier (Hop>=1),
//   EgoGraph-Hop-0-Focus (frontier leer wenn focus.Scope NOT IN readScopes),
//   GraphExpand-Seed (Seed-Set vor fetchNeighbors filtern). Alternative voller
//   Hop-Knoten, umentscheidbar weil der Ausschluss je ein Filter im Seed-/frontier-Set ist.
func isGrantOnlyVisible(scope string, readScopes []string) bool {
	return !slices.Contains(readScopes, scope)
}

// EgoGraph runs the BFS hop loop (design §3.2/§3.3):
//
//  1. Hydrate the focus + visibility check (miss → ErrNotVisible; the handler
//     answers 404 identical to "does not exist" and writes NO access_log).
//  2. Per hop ONE batched query (Q1) over the frontier — visibility, category
//     and created-window predicates INSIDE the LATERAL cap legs, before the
//     per-node LIMIT. The visited set lives in Go; the node budget is enforced
//     DURING traversal (truncation order: hop ascending, then confidence DESC,
//     then id).
//  3. Induced edges (Q2) over the final node set — scope-safe by construction
//     (both endpoints come from the visibility-checked node set).
//  4. Visible degrees (Q3), batched, double-budgeted (scan + hit cap).
//
// visibleTypes is the resolved retrieval allowlist (blocktype.Set
// .VisibleTypes, WF T6) — every visibility predicate in all four queries
// consumes it as a bind parameter. Fail-closed HARD: an empty/nil list is a
// Go error here (rrf.Search pattern) — SQL would return 0 rows, but a silent
// empty graph would mask a wiring bug.
func EgoGraph(ctx context.Context, pool *pgxpool.Pool, p EgoParams, readScopes []string, grantedBlockIDs []string, visibleTypes []string) (*EgoResult, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if len(visibleTypes) == 0 {
		return nil, errors.New("store: empty visible-types allowlist (block-type registry not wired?)")
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	normalizeClassFilters(&p)
	focus, err := hydrateFocus(ctx, pool, p.Focus, readScopes, grantedBlockIDs, visibleTypes)
	if err != nil {
		return nil, err
	}

	nodes := []GraphNode{*focus}
	visited := map[string]bool{focus.ID: true}
	// T41 (07-W3) hop-0 leaf protection: a grant-only focus (visible solely via the
	// block-grant OR-arm, its scope NOT in readScopes) stays a visible isolated node
	// but seeds NO traversal — frontier empty. Otherwise the hop loop would walk
	// FROM the granted block and deliver the grantee's own in-scope neighbours over
	// the grant bridge. An in-scope focus keeps the normal {focus} frontier.
	frontier := []string{focus.ID}
	if isGrantOnlyVisible(focus.Scope, readScopes) {
		frontier = []string{}
	}
	truncated := false

	// Symmetric class short circuits (design/01 §4.6, W4-G4): a non-nil-EMPTY
	// class filter means "none of this class" — the leg would bind ANY('{}'),
	// match nothing, and (worse) leave the M050/M104 early termination dead
	// while scanning to prefix exhaustion on every hub. Skip the roundtrip.
	// dreamSkip also covers the legacy `link_class=supersedes` case
	// (traversalClasses collapses it to non-nil-empty).
	dreamSkip := p.LinkClasses != nil && len(traversalClasses(p.LinkClasses)) == 0
	structSkip := p.StructClasses != nil && len(p.StructClasses) == 0

	for hop := 1; hop <= p.Hops; hop++ {
		if len(frontier) == 0 {
			break // natural exhaustion — nothing left to expand
		}
		if len(nodes) >= p.Limit {
			// Node budget exhausted while an unexpanded frontier remains:
			// the budget cut the traversal short.
			truncated = true
			break
		}
		var dreamCands, structCands []hopCandidate
		if !dreamSkip {
			var herr error
			dreamCands, herr = hopNeighbors(ctx, pool, frontier, readScopes, grantedBlockIDs, visibleTypes, p)
			if herr != nil {
				return nil, herr
			}
		}
		if !structSkip {
			var serr error
			structCands, serr = structuralHopNeighbors(ctx, pool, frontier, readScopes, grantedBlockIDs, visibleTypes, p)
			if serr != nil {
				return nil, serr
			}
		}
		added, hopTruncated := takeHopMerged(dreamCands, structCands, visited, p.Limit-len(nodes), hop)
		frontier = make([]string, 0, len(added))
		for i := range added {
			nodes = append(nodes, added[i])
			// T41 (07-W3) hop>=1 leaf protection: a grant-only neighbour (reached via
			// the T40a OR-arm, scope NOT in readScopes) joins the visible node set but
			// NOT the next frontier — the traversal does not continue THROUGH a granted
			// block into the grantee's own in-scope blocks behind it.
			if isGrantOnlyVisible(added[i].Scope, readScopes) {
				continue
			}
			frontier = append(frontier, added[i].ID)
		}
		if hopTruncated {
			truncated = true
			break
		}
	}

	ids := make([]string, len(nodes))
	index := make(map[string]int, len(nodes))
	for i := range nodes {
		ids[i] = nodes[i].ID
		index[nodes[i].ID] = i
	}

	edges, edgesTruncated, err := resolveDreamEdges(ctx, pool, ids, index, p)
	if err != nil {
		return nil, err
	}
	if edgesTruncated {
		truncated = true
	}

	edges, structEdges, structRels, origins, structTruncated, err := resolveStructEdges(ctx, pool, ids, index, p, edges)
	if err != nil {
		return nil, err
	}
	if structTruncated {
		truncated = true
	}

	if err := fillDegrees(ctx, pool, ids, readScopes, grantedBlockIDs, visibleTypes, nodes); err != nil {
		return nil, err
	}

	return &EgoResult{
		Focus:       focus.ID,
		Rels:        GraphRels,
		StructRels:  structRels,
		Origins:     origins,
		Nodes:       nodes,
		Edges:       edges,
		StructEdges: structEdges,
		Truncated:   truncated,
	}, nil
}

// takeHop applies the deterministic truncation order to one hop's candidates:
// confidence DESC, then id ASC. Already-visited candidates are skipped without
// consuming budget; at most budget NEW nodes are accepted (Hop stamped,
// visited updated). The second return is true iff a NEW candidate had to be
// dropped because the budget was exhausted. Pure (no DB) — unit-testable.
func takeHop(cands []hopCandidate, visited map[string]bool, budget, hop int) ([]GraphNode, bool) {
	return takeHopMerged(cands, nil, visited, budget, hop)
}

// takeHopMerged arbitrates one hop's node budget FAIRLY between the two data
// classes (E2 zipper, design/01 §4.3): dream candidates re-sort internally by
// conf DESC / id ASC (the takeHop legacy), the structural list is consumed in
// SQL order — Q1s' ORDER BY (link_created DESC, neighbor_id) IS the total
// order, there is deliberately NO Go re-sort (hopCandidate.conf carries no
// ordering content for structural, constant 1.0). The zipper alternates
// (dream starts — deterministic convention); a visited skip does not consume
// the side's turn (a node present in BOTH lists takes ONE budget slot and
// must not shift fairness); an exhausted side drains the other. Naively
// sorting structural conf=1.0 into the single takeHop list would push every
// fact ahead of every dream candidate at the budget cut — the displacement
// the user request names, moved from the per-node cap to the global budget.
// truncated turns true as soon as ANY unvisited candidate fails the budget
// (unchanged semantics). Pure — unit-tested including the zipper fairness
// and the empty-structural regression anchor (byte-identical to old takeHop).
func takeHopMerged(dream, structural []hopCandidate, visited map[string]bool, budget, hop int) ([]GraphNode, bool) {
	sort.SliceStable(dream, func(i, j int) bool {
		if dream[i].conf != dream[j].conf {
			return dream[i].conf > dream[j].conf
		}
		return dream[i].node.ID < dream[j].node.ID
	})

	out := make([]GraphNode, 0, len(dream)+len(structural))
	truncated := false
	var di, si int
	// next yields the side's first unvisited candidate — skips cost nothing.
	next := func(list []hopCandidate, i *int) (hopCandidate, bool) {
		for *i < len(list) {
			c := list[*i]
			*i++
			if !visited[c.node.ID] {
				return c, true
			}
		}
		return hopCandidate{}, false
	}
	dreamTurn := true
	for {
		var c hopCandidate
		var ok bool
		if dreamTurn {
			if c, ok = next(dream, &di); !ok {
				c, ok = next(structural, &si) // drain the other side
			}
		} else {
			if c, ok = next(structural, &si); !ok {
				c, ok = next(dream, &di)
			}
		}
		if !ok {
			break // both exhausted
		}
		if budget <= 0 {
			truncated = true
			break
		}
		n := c.node
		n.Hop = hop
		visited[n.ID] = true
		out = append(out, n)
		budget--
		dreamTurn = !dreamTurn
	}
	return out, truncated
}

// hydrateFocus loads the focus node (hop 0) under the canonical visibility
// triple. Zero rows — whether the block does not exist or is out of scope —
// collapse into the same ErrNotVisible.
func hydrateFocus(ctx context.Context, pool *pgxpool.Pool, id string, readScopes, grantedBlockIDs, visibleTypes []string) (*GraphNode, error) {
	// $1=focus id, $2=readScopes, $3=grantedBlockIDs (T40a OR-arm via the shared
	// VisibilityPredicate switch point — a granted block becomes a VISIBLE focus),
	// $4=visibleTypes (T6 registry allowlist).
	q := fmt.Sprintf(
		`SELECT b.id::text, left(b.title, 120), b.category, b.scope::text, b.created_at
		 FROM context_blocks b
		 WHERE b.id = $1::uuid AND %s`,
		VisibilityPredicate("b", "$4", "$2", "$3"),
	)
	n := GraphNode{Hop: 0}
	err := pool.QueryRow(ctx, q, id, readScopes, grantedBlockIDs, visibleTypes).
		Scan(&n.ID, &n.Title, &n.Category, &n.Scope, &n.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotVisible
	}
	if err != nil {
		return nil, fmt.Errorf("store: graph hydrate focus: %w", err)
	}
	return &n, nil
}

// hopNeighbors is Q1: ONE batched hop over the frontier. Per frontier node a
// LATERAL picks the top per_node_cap edges by raw_confidence DESC (M050 index
// order → early termination) — with ALL neighbor-side predicates (weighted-
// confidence gate, relationship filter, visibility triple, category, created
// window) inside the legs, BEFORE the LIMIT. Cap slots are only ever granted
// to visible, filter-passing edges (anti-starvation + no counting channel,
// design §6.2). DISTINCT ON keeps each neighbor once with its strongest edge.
func hopNeighbors(ctx context.Context, pool *pgxpool.Pool, frontier, readScopes, grantedBlockIDs, visibleTypes []string, p EgoParams) ([]hopCandidate, error) {
	// $9 = grantedBlockIDs (T40a): the shared VisibilityPredicate carries the
	// block-grant OR-arm into BOTH hop legs (a granted neighbor becomes VISIBLE).
	// $10 = visibleTypes (T6): registry allowlist, inside the legs BEFORE the cap.
	vis := VisibilityPredicate("b", "$10", "$2", "$9")
	q := fmt.Sprintf(`
WITH hop AS (
    SELECT DISTINCT ON (e.neighbor_id)
           e.neighbor_id, e.confidence, e.title, e.category, e.scope, e.created_at
    FROM unnest($1::uuid[]) AS f(seed_id)
    CROSS JOIN LATERAL (
        (SELECT l.target_block_id AS neighbor_id, l.confidence, l.raw_confidence,
                left(b.title, 120) AS title, b.category, b.scope::text AS scope, b.created_at
         FROM context_dream_links l
         JOIN context_blocks b ON b.id = l.target_block_id
         WHERE l.source_block_id = f.seed_id
           AND l.relationship <> 'supersedes'
           AND l.confidence >= $3
           AND ($4::text[] IS NULL OR l.relationship = ANY($4))
           AND %s
           AND ($6::text[] IS NULL OR b.category = ANY($6))
           AND ($7::timestamptz IS NULL OR b.created_at >= $7)
           AND ($8::timestamptz IS NULL OR b.created_at <  $8)
         ORDER BY l.raw_confidence DESC
         LIMIT $5)
        UNION ALL
        (SELECT l.source_block_id, l.confidence, l.raw_confidence,
                left(b.title, 120), b.category, b.scope::text, b.created_at
         FROM context_dream_links l
         JOIN context_blocks b ON b.id = l.source_block_id
         WHERE l.target_block_id = f.seed_id
           AND l.relationship <> 'supersedes'
           AND l.confidence >= $3
           AND ($4::text[] IS NULL OR l.relationship = ANY($4))
           AND %s
           AND ($6::text[] IS NULL OR b.category = ANY($6))
           AND ($7::timestamptz IS NULL OR b.created_at >= $7)
           AND ($8::timestamptz IS NULL OR b.created_at <  $8)
         ORDER BY l.raw_confidence DESC
         LIMIT $5)
        ORDER BY raw_confidence DESC
        LIMIT $5
    ) e
    ORDER BY e.neighbor_id, e.confidence DESC
)
SELECT h.neighbor_id::text, h.confidence, h.title, h.category, h.scope, h.created_at
FROM hop h
ORDER BY h.confidence DESC, h.neighbor_id`, vis, vis)

	rows, err := pool.Query(ctx, q,
		frontier,                        // $1
		readScopes,                      // $2
		p.MinConfidence,                 // $3
		traversalClasses(p.LinkClasses), // $4 NULL = all four positive types
		p.PerNodeCap,                    // $5
		nilIfEmpty(p.Categories),        // $6
		p.CreatedAfter,                  // $7
		p.CreatedBefore,                 // $8
		grantedBlockIDs,                 // $9 block-grant OR-arm (T40a)
		visibleTypes,                    // $10 registry type allowlist (T6)
	)
	if err != nil {
		return nil, fmt.Errorf("store: graph hop query: %w", err)
	}
	defer rows.Close()

	var cands []hopCandidate
	for rows.Next() {
		var c hopCandidate
		if err := rows.Scan(&c.node.ID, &c.conf, &c.node.Title, &c.node.Category, &c.node.Scope, &c.node.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: graph hop scan: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: graph hop rows: %w", err)
	}
	return cands, nil
}

// structuralHopSQL is the Q1s query text, factored out so the W2-G3 EXPLAIN
// gate proves the plan of EXACTLY the shipped SQL (no test-local copy drift).
func structuralHopSQL() string {
	// $8 = grantedBlockIDs, $9 = visibleTypes — same predicate discipline as Q1.
	vis := VisibilityPredicate("b", "$9", "$2", "$8")
	return fmt.Sprintf(`
WITH hop AS (
    SELECT DISTINCT ON (e.neighbor_id)
           e.neighbor_id, e.link_created, e.title, e.category, e.scope, e.created_at
    FROM unnest($1::uuid[]) AS f(seed_id)
    CROSS JOIN LATERAL (
        (SELECT sl.target_block_id AS neighbor_id, sl.created_at AS link_created,
                left(b.title, 120) AS title, b.category, b.scope::text AS scope, b.created_at
         FROM context_structural_links sl
         JOIN context_blocks b ON b.id = sl.target_block_id
         WHERE sl.source_block_id = f.seed_id
           AND ($3::text[] IS NULL OR sl.link_class = ANY($3))
           AND %s
           AND ($5::text[] IS NULL OR b.category = ANY($5))
           AND ($6::timestamptz IS NULL OR b.created_at >= $6)
           AND ($7::timestamptz IS NULL OR b.created_at <  $7)
         ORDER BY sl.created_at DESC
         LIMIT $4)
        UNION ALL
        (SELECT sl.source_block_id, sl.created_at,
                left(b.title, 120), b.category, b.scope::text, b.created_at
         FROM context_structural_links sl
         JOIN context_blocks b ON b.id = sl.source_block_id
         WHERE sl.target_block_id = f.seed_id
           AND ($3::text[] IS NULL OR sl.link_class = ANY($3))
           AND %s
           AND ($5::text[] IS NULL OR b.category = ANY($5))
           AND ($6::timestamptz IS NULL OR b.created_at >= $6)
           AND ($7::timestamptz IS NULL OR b.created_at <  $7)
         ORDER BY sl.created_at DESC
         LIMIT $4)
        ORDER BY link_created DESC
        LIMIT $4
    ) e
    ORDER BY e.neighbor_id, e.link_created DESC
)
SELECT h.neighbor_id::text, h.link_created, h.title, h.category, h.scope, h.created_at
FROM hop h
ORDER BY h.link_created DESC, h.neighbor_id`, vis, vis)
}

// structuralHopNeighbors is Q1s: ONE batched hop over the frontier on
// context_structural_links — the structural sibling of hopNeighbors (Q1),
// UNWIRED until the traversal-merge wave (GB3). Ordering key is created_at
// DESC ("newest reference first", E5) — structural links carry no confidence
// (1.0 by definition, 076), so there is deliberately NO MinConfidence bind:
// for every valid threshold in [0,1], 1.0 passes — omitting the gate IS the
// semantics, not a special case. All neighbor-side predicates (class filter
// $3 = displayClasses(StructClasses), visibility triple, category, created
// window) sit INSIDE the LATERAL legs BEFORE the per-node LIMIT — identical
// anti-starvation / no-counting-channel discipline as Q1: an invisible edge
// never consumes a cap slot. PerNodeCap is SHARED as a value but a SEPARATE
// slot pool (E10): up to cap dream candidates (Q1) plus up to cap structural
// candidates (Q1s) per frontier node; the global node budget arbitrates in Go
// (GB3). DISTINCT ON keeps each neighbor once with its newest edge; the
// returned slice is newest-first — order carries the ranking (conf is a
// constant 1), the GB3 merge consumes it positionally.
//
// Fail-closed entry is SELF-CONTAINED (RequireScopes + visible-types check +
// grant normalization live HERE — self-contained entry discipline): the SQL
// alone would return an EMPTY graph on empty scopes/types — a silent mask of
// a wiring bug, exactly what the EgoGraph entry refuses loudly.
func structuralHopNeighbors(ctx context.Context, pool *pgxpool.Pool,
	frontier, readScopes, grantedBlockIDs, visibleTypes []string, p EgoParams) ([]hopCandidate, error) {
	if err := RequireScopes(readScopes); err != nil {
		return nil, err
	}
	if len(visibleTypes) == 0 {
		return nil, errors.New("store: empty visible-types allowlist (block-type registry not wired?)")
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}

	rows, err := pool.Query(ctx, structuralHopSQL(),
		frontier,                        // $1
		readScopes,                      // $2
		displayClasses(p.StructClasses), // $3 nil = all classes; non-nil-empty = none (§4.6)
		p.PerNodeCap,                    // $4 shared value, separate slot pool (E10)
		nilIfEmpty(p.Categories),        // $5
		p.CreatedAfter,                  // $6
		p.CreatedBefore,                 // $7
		grantedBlockIDs,                 // $8 block-grant OR-arm
		visibleTypes,                    // $9 registry type allowlist
	)
	if err != nil {
		return nil, fmt.Errorf("store: graph structural hop query: %w", err)
	}
	defer rows.Close()

	var cands []hopCandidate
	for rows.Next() {
		var c hopCandidate
		var linkCreated time.Time // ordering key only — position in the slice carries it
		if err := rows.Scan(&c.node.ID, &linkCreated, &c.node.Title, &c.node.Category, &c.node.Scope, &c.node.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: graph structural hop scan: %w", err)
		}
		c.conf = 1 // facts are 1.0 by definition (076)
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: graph structural hop rows: %w", err)
	}
	return cands, nil
}

// inducedEdges is Q2: ALL edges whose BOTH endpoints are inside the delivered
// node set — including supersedes (display-only; rendered dashed client-side).
// Scope safety holds by construction: every endpoint already passed the
// visibility triple. The query reads one row beyond the edge budget so the
// truncation flag is exact; ordering is strongest-first with an id tiebreak
// for deterministic output.
func inducedEdges(ctx context.Context, pool *pgxpool.Pool, ids []string, index map[string]int, p EgoParams) ([]GraphEdge, bool, error) {
	const q = `
SELECT l.source_block_id::text, l.target_block_id::text, l.relationship, l.confidence
FROM context_dream_links l
WHERE l.source_block_id = ANY($1::uuid[])
  AND l.target_block_id = ANY($1::uuid[])
  AND l.confidence >= $2
  AND ($3::text[] IS NULL OR l.relationship = ANY($3))
ORDER BY l.confidence DESC, l.source_block_id, l.target_block_id
LIMIT $4`

	rows, err := pool.Query(ctx, q,
		ids,                           // $1
		p.MinConfidence,               // $2
		displayClasses(p.LinkClasses), // $3 nil = all five; non-nil-empty = none (§4.6)
		p.EdgeLimit+1,                 // $4 one extra row → exact truncation flag
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: graph induced edges query: %w", err)
	}
	defer rows.Close()

	var edges []GraphEdge
	for rows.Next() {
		var src, dst, rel string
		var conf float64
		if err := rows.Scan(&src, &dst, &rel, &conf); err != nil {
			return nil, false, fmt.Errorf("store: graph induced edges scan: %w", err)
		}
		si, sok := index[src]
		di, dok := index[dst]
		ri, rok := graphRelIndex[rel]
		if !sok || !dok || !rok {
			// Both endpoints matched ANY(ids); unknown values would mean a
			// relationship outside the legend — skip defensively.
			continue
		}
		edges = append(edges, GraphEdge{Src: si, Dst: di, Rel: ri, Conf: conf})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: graph induced edges rows: %w", err)
	}

	if len(edges) > p.EdgeLimit {
		return edges[:p.EdgeLimit], true, nil
	}
	return edges, false, nil
}

// resolveDreamEdges is the Q2 stage of EgoGraph with the GB5 skip: an empty
// dream partition (non-nil-EMPTY LinkClasses, §4.6 sentinel) costs NO
// roundtrip — without the skip `link_class=<structural>` would bind $3='{}',
// match nothing, and still scan (design/02 §4.2b). Symmetric to the Q2s skip
// in resolveStructEdges.
func resolveDreamEdges(ctx context.Context, pool *pgxpool.Pool, ids []string, index map[string]int, p EgoParams) ([]GraphEdge, bool, error) {
	if p.LinkClasses != nil && len(p.LinkClasses) == 0 {
		return nil, false, nil
	}
	return inducedEdges(ctx, pool, ids, index, p)
}

// resolveStructEdges is the Q2s stage of EgoGraph (GB2): structural facts
// between the ALREADY delivered nodes, budget-arbitrated against the dream
// edges (fact precedence with dream floor), index-mapped with post-truncation
// legends. Go short circuit on the §4.6 "none selected" sentinel: a
// non-nil-EMPTY StructClasses would bind ANY('{}') and match nothing — skip
// the roundtrip entirely. Returns the (possibly arbitration-cut) dream edges.
func resolveStructEdges(ctx context.Context, pool *pgxpool.Pool, ids []string, index map[string]int, p EgoParams, edges []GraphEdge) ([]GraphEdge, []StructGraphEdge, []string, []string, bool, error) {
	if p.StructClasses != nil && len(p.StructClasses) == 0 {
		return edges, nil, nil, nil, false, nil
	}
	structRows, structOverflow, err := inducedStructEdges(ctx, pool, ids, p)
	if err != nil {
		return nil, nil, nil, nil, false, err
	}
	var arbTruncated bool
	edges, structRows, arbTruncated = arbitrateEdgeBudget(edges, structRows, p.EdgeLimit)
	structEdges, structRels, origins := mapStructEdges(structRows, index)
	return edges, structEdges, structRels, origins, structOverflow || arbTruncated, nil
}

// structEdgeRow is one raw Q2s row before index mapping — endpoints as UUIDs,
// class/origin as names (legends are built AFTER budget arbitration from the
// delivered rows only, E3 dynamic).
type structEdgeRow struct {
	src, dst, class, origin string
}

// inducedStructEdges is Q2s: ALL structural edges whose BOTH endpoints are
// inside the delivered node set. Scope-safe by construction (every endpoint
// passed the visibility triple — the Q2 invariant, verbatim). No
// MinConfidence bind (facts are 1.0 by definition and pass every threshold).
// Parallel classes are correct and wanted: the PK carries link_class, so one
// pair may deliver references AND duplicate-of as separate tuples — plus a
// dream edge in Edges (separate arrays). Reads one row beyond the budget so
// the overflow flag is exact; newest-first (fact precedence order for the
// arbitration cut).
func inducedStructEdges(ctx context.Context, pool *pgxpool.Pool, ids []string, p EgoParams) ([]structEdgeRow, bool, error) {
	const q = `
SELECT sl.source_block_id::text, sl.target_block_id::text, sl.link_class, sl.origin
FROM context_structural_links sl
WHERE sl.source_block_id = ANY($1::uuid[])
  AND sl.target_block_id = ANY($1::uuid[])
  AND ($2::text[] IS NULL OR sl.link_class = ANY($2))
ORDER BY sl.created_at DESC, sl.source_block_id, sl.target_block_id, sl.link_class
LIMIT $3`

	rows, err := pool.Query(ctx, q,
		ids,                             // $1
		displayClasses(p.StructClasses), // $2 nil = all; non-nil-empty = none (§4.6)
		p.EdgeLimit+1,                   // $3 one extra row → exact overflow flag
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: graph induced struct edges query: %w", err)
	}
	defer rows.Close()

	var out []structEdgeRow
	for rows.Next() {
		var r structEdgeRow
		if err := rows.Scan(&r.src, &r.dst, &r.class, &r.origin); err != nil {
			return nil, false, fmt.Errorf("store: graph induced struct edges scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: graph induced struct edges rows: %w", err)
	}
	if len(out) > p.EdgeLimit {
		return out[:p.EdgeLimit], true, nil
	}
	return out, false, nil
}

// arbitrateEdgeBudget shares ONE EdgeLimit between both edge classes: fact
// precedence WITH a dream floor (design/01 §4.5 — the edge analogue of the E2
// zipper, coarser but starvation-free in both directions). If both lists fit,
// everything ships. Otherwise dream keeps min(|D|, EdgeLimit/2) slots (its
// list is conf-DESC, the cut hits the weakest edges by definition), structural
// takes min(|S|, EdgeLimit−floor) in newest-first order, dream fills the rest.
// Degenerates correctly: little dream → the unused floor flows to structural
// via min; empty structural → byte-identical to the pre-GB2 behavior. Pure —
// unit-tested including the degeneration arm (red against a floorless cut).
func arbitrateEdgeBudget(dream []GraphEdge, structRows []structEdgeRow, edgeLimit int) ([]GraphEdge, []structEdgeRow, bool) {
	if len(dream)+len(structRows) <= edgeLimit {
		return dream, structRows, false
	}
	dreamFloor := min(len(dream), edgeLimit/2)
	structSlots := min(len(structRows), edgeLimit-dreamFloor)
	dreamSlots := edgeLimit - structSlots
	return dream[:dreamSlots], structRows[:structSlots], true
}

// mapStructEdges resolves raw Q2s rows into wire tuples: legends are the
// sorted distinct class/origin sets of the DELIVERED rows (built after
// truncation — a legend must never describe an edge the response dropped),
// endpoints resolve through the node index (out-of-set endpoints cannot occur
// by the ANY(ids) predicate; skipped defensively like Q2).
func mapStructEdges(rows []structEdgeRow, index map[string]int) ([]StructGraphEdge, []string, []string) {
	if len(rows) == 0 {
		return nil, nil, nil
	}
	classSet := map[string]bool{}
	originSet := map[string]bool{}
	for _, r := range rows {
		classSet[r.class] = true
		originSet[r.origin] = true
	}
	classes := make([]string, 0, len(classSet))
	for c := range classSet {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	origins := make([]string, 0, len(originSet))
	for o := range originSet {
		origins = append(origins, o)
	}
	sort.Strings(origins)
	classIdx := make(map[string]int, len(classes))
	for i, c := range classes {
		classIdx[c] = i
	}
	originIdx := make(map[string]int, len(origins))
	for i, o := range origins {
		originIdx[o] = i
	}

	edges := make([]StructGraphEdge, 0, len(rows))
	for _, r := range rows {
		si, sok := index[r.src]
		di, dok := index[r.dst]
		if !sok || !dok {
			continue
		}
		edges = append(edges, StructGraphEdge{Src: si, Dst: di, Class: classIdx[r.class], Origin: originIdx[r.origin]})
	}
	return edges, classes, origins
}

// fillDegrees is Q3: the visible degree per node, batched, double-budgeted.
// The inner LIMIT (DegreeScanBudget) is a SCAN budget — it caps how many RAW
// link rows per direction PER CLASS ever reach the block join (cost ceiling
// in the visible-poor case; 4 legs × 1000 since GB4, ledgered §6.3). The outer LIMIT (DegreeHitCap) is a HIT budget — it
// stops after 201 VISIBLE neighbors (renders as "200+", visible-rich case).
// Degree counts link ROWS of BOTH data classes (GB4, K5 semantics: per
// direction, per class — NOT distinct neighbors, which would break the
// client badge math `degree - loadedDeg` on every collision pair): all five
// dream relationship types plus every structural class, four UNION-ALL legs.
// Every leg is scope-visible at the far endpoint — a raw count would leak
// the existence of foreign private links on shared blocks (design §6.3).
func fillDegrees(ctx context.Context, pool *pgxpool.Pool, ids, readScopes, grantedBlockIDs, visibleTypes []string, nodes []GraphNode) error {
	// $5 = grantedBlockIDs (T40a): a granted neighbor counts toward the VISIBLE
	// degree via the shared VisibilityPredicate OR-arm (all four UNION legs).
	// $6 = visibleTypes (T6): only allowlisted neighbors count.
	vis := VisibilityPredicate("nb", "$6", "$2", "$5")
	q := fmt.Sprintf(`
SELECT n.id::text,
       (SELECT count(*) FROM (
            SELECT 1
            FROM (SELECT l.target_block_id AS nb_id
                  FROM context_dream_links l
                  WHERE l.source_block_id = n.id
                  LIMIT $3) raw
            JOIN context_blocks nb ON nb.id = raw.nb_id
            WHERE %[1]s
            UNION ALL
            SELECT 1
            FROM (SELECT l.source_block_id AS nb_id
                  FROM context_dream_links l
                  WHERE l.target_block_id = n.id
                  LIMIT $3) raw
            JOIN context_blocks nb ON nb.id = raw.nb_id
            WHERE %[1]s
            UNION ALL
            SELECT 1
            FROM (SELECT sl.target_block_id AS nb_id
                  FROM context_structural_links sl
                  WHERE sl.source_block_id = n.id
                  LIMIT $3) raw
            JOIN context_blocks nb ON nb.id = raw.nb_id
            WHERE %[1]s
            UNION ALL
            SELECT 1
            FROM (SELECT sl.source_block_id AS nb_id
                  FROM context_structural_links sl
                  WHERE sl.target_block_id = n.id
                  LIMIT $3) raw
            JOIN context_blocks nb ON nb.id = raw.nb_id
            WHERE %[1]s
            LIMIT $4
        ) c)::int AS degree
FROM unnest($1::uuid[]) AS n(id)`, vis)

	rows, err := pool.Query(ctx, q, ids, readScopes, DegreeScanBudget, DegreeHitCap, grantedBlockIDs, visibleTypes)
	if err != nil {
		return fmt.Errorf("store: graph degree query: %w", err)
	}
	defer rows.Close()

	degrees := make(map[string]int, len(ids))
	for rows.Next() {
		var id string
		var deg int
		if err := rows.Scan(&id, &deg); err != nil {
			return fmt.Errorf("store: graph degree scan: %w", err)
		}
		degrees[id] = deg
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: graph degree rows: %w", err)
	}

	for i := range nodes {
		nodes[i].Degree = degrees[nodes[i].ID]
	}
	return nil
}

// traversalClasses intersects the requested link classes with the four
// positive (traversable) types. nil stays nil (SQL NULL = all four via the
// partial-index predicate); a non-nil result may be empty, which correctly
// matches nothing. supersedes is never traversed (display-only).
func traversalClasses(requested []string) []string {
	if requested == nil {
		return nil
	}
	out := make([]string, 0, len(requested))
	for _, r := range requested {
		if r != "supersedes" {
			out = append(out, r)
		}
	}
	return out
}

// nilIfEmpty maps an empty slice to nil so pgx binds SQL NULL ("no filter")
// instead of an empty array (which would match nothing). Kept for Categories,
// where empty stays "no filter" (egoCSV contract) — NEVER use it on class
// binds (see displayClasses).
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// displayClasses is the class-filter bind semantic (design/01 §4.6): nil binds
// SQL NULL ("no filter"); non-nil — INCLUDING empty — binds the array (empty
// matches nothing). Collapsing non-nil-empty to NULL (nilIfEmpty) would turn
// the filter "no dream classes selected" into "all five" — after the handler's
// vocabulary split, `link_class=<structural class>` would then show every
// dream edge (W1-G1 bug path, red-proven against the old binding).
func displayClasses(s []string) []string {
	return s
}

// normalizeClassFilters enforces the §4.6 input invariant on EgoGraph entry:
// a class filter is set all-or-nothing across BOTH vocabularies. If exactly
// one side is filtered (non-nil) and the other is nil, the nil side becomes
// non-nil-EMPTY ("none") — `link_class=topical` means ONLY topical, not
// "filtered dream plus ALL structural". The final handler sets both fields
// after its vocabulary split anyway (no-op there); this protects direct store
// consumers and pins the build-phase semantics while the split is unwired.
func normalizeClassFilters(p *EgoParams) {
	if p.LinkClasses != nil && p.StructClasses == nil {
		p.StructClasses = []string{}
	}
	if p.StructClasses != nil && p.LinkClasses == nil {
		p.LinkClasses = []string{}
	}
}

// LogGraphAccess writes the single telemetry row for a successful ego call:
// action='graph', block_id=NULL, metadata={focus,hops,limit,node_count}.
//
// block_id is NULL by design: the deployed M007/M008 gravity functions read
// access_log counts per block_id (without action filter) into a ranking-mass
// formula — with block_id=NULL graph browsing is constructively decoupled
// from every access-count-based ranking mechanic, whatever the temporal wave
// reactivates. The focus stays auditable via metadata. Callers invoke this
// AFTER the focus visibility check; the 404 path writes nothing, so foreign
// private blocks cannot be telemetrically "bumped" by UUID probing (the
// manage-get pre-check logging is the anti-pattern F5 deliberately avoids).
func LogGraphAccess(ctx context.Context, pool *pgxpool.Pool, apiKeyID, focus string, hops, limit, nodeCount, structEdgeCount int) error {
	meta, err := json.Marshal(map[string]any{
		"focus":             focus,
		"hops":              hops,
		"limit":             limit,
		"node_count":        nodeCount,
		"struct_edge_count": structEdgeCount, // GB3 telemetry (§4.9) — additive, same bucket
	})
	if err != nil {
		return fmt.Errorf("store: graph access metadata: %w", err)
	}

	var keyID any // NULL-safe: empty key id must not break the uuid cast
	if apiKeyID != "" {
		keyID = apiKeyID
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata, principal_id)
		 VALUES ($1::uuid, NULL, 'graph', $2::jsonb,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))`,
		keyID, meta,
	)
	if err != nil {
		return fmt.Errorf("store: log graph access: %w", err)
	}
	return nil
}
