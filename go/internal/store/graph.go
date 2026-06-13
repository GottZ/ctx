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
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Degree budgets: policy values with a single definition, passed to SQL as
// bind parameters (mechanism = code, policy = data). Deliberately NOT API
// parameters (design §3.3 Q3).
const (
	// DegreeScanBudget caps how many RAW link rows per direction the visible-
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

// EgoParams are the validated request parameters (ceilings enforced in the
// handler; out-of-range is a 400 there, never silently clamped).
type EgoParams struct {
	Focus         string
	Hops          int      // 1..3
	PerNodeCap    int      // 1..100
	Limit         int      // 1..1500 nodes (G39: ceiling lowered from 5000, see handler)
	EdgeLimit     int      // 1..20000 induced edges
	MinConfidence float64  // gate on weighted confidence (traversal + induced)
	LinkClasses   []string // nil = all five
	Categories    []string // nil = all
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
}

// EgoResult is the store-INTERNAL result. The wire envelope is built by the
// handler (egoResponse) — three places, ONE shape: design §3.1 example ==
// handler egoResponse == client EgoResponse.
type EgoResult struct {
	Focus     string
	Rels      []string // legend for edge rel indexes
	Nodes     []GraphNode
	Edges     []GraphEdge
	Truncated bool
}

// hopCandidate is one Q1 row: a visible, filter-passing neighbor with the
// confidence of its strongest edge into the frontier (ordering key only —
// it is not part of the node payload).
type hopCandidate struct {
	node GraphNode
	conf float64
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
func EgoGraph(ctx context.Context, pool *pgxpool.Pool, p EgoParams, readScopes []string) (*EgoResult, error) {
	focus, err := hydrateFocus(ctx, pool, p.Focus, readScopes)
	if err != nil {
		return nil, err
	}

	nodes := []GraphNode{*focus}
	visited := map[string]bool{focus.ID: true}
	frontier := []string{focus.ID}
	truncated := false

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
		cands, herr := hopNeighbors(ctx, pool, frontier, readScopes, p)
		if herr != nil {
			return nil, herr
		}
		added, hopTruncated := takeHop(cands, visited, p.Limit-len(nodes), hop)
		frontier = make([]string, 0, len(added))
		for i := range added {
			nodes = append(nodes, added[i])
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

	edges, edgesTruncated, err := inducedEdges(ctx, pool, ids, index, p)
	if err != nil {
		return nil, err
	}
	if edgesTruncated {
		truncated = true
	}

	if err := fillDegrees(ctx, pool, ids, readScopes, nodes); err != nil {
		return nil, err
	}

	return &EgoResult{
		Focus:     focus.ID,
		Rels:      GraphRels,
		Nodes:     nodes,
		Edges:     edges,
		Truncated: truncated,
	}, nil
}

// takeHop applies the deterministic truncation order to one hop's candidates:
// confidence DESC, then id ASC. Already-visited candidates are skipped without
// consuming budget; at most budget NEW nodes are accepted (Hop stamped,
// visited updated). The second return is true iff a NEW candidate had to be
// dropped because the budget was exhausted. Pure (no DB) — unit-testable.
func takeHop(cands []hopCandidate, visited map[string]bool, budget, hop int) ([]GraphNode, bool) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].conf != cands[j].conf {
			return cands[i].conf > cands[j].conf
		}
		return cands[i].node.ID < cands[j].node.ID
	})

	out := make([]GraphNode, 0, len(cands))
	truncated := false
	for _, c := range cands {
		if visited[c.node.ID] {
			continue
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
	}
	return out, truncated
}

// hydrateFocus loads the focus node (hop 0) under the canonical visibility
// triple. Zero rows — whether the block does not exist or is out of scope —
// collapse into the same ErrNotVisible.
func hydrateFocus(ctx context.Context, pool *pgxpool.Pool, id string, readScopes []string) (*GraphNode, error) {
	q := fmt.Sprintf(
		`SELECT b.id::text, left(b.title, 120), b.category, b.scope::text, b.created_at
		 FROM context_blocks b
		 WHERE b.id = $1::uuid AND %s`,
		VisibilityPredicate("b", "$2"),
	)
	n := GraphNode{Hop: 0}
	err := pool.QueryRow(ctx, q, id, readScopes).
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
func hopNeighbors(ctx context.Context, pool *pgxpool.Pool, frontier, readScopes []string, p EgoParams) ([]hopCandidate, error) {
	vis := VisibilityPredicate("b", "$2")
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
		ids,                       // $1
		p.MinConfidence,           // $2
		nilIfEmpty(p.LinkClasses), // $3 NULL = all five (display set)
		p.EdgeLimit+1,             // $4 one extra row → exact truncation flag
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

// fillDegrees is Q3: the visible degree per node, batched, double-budgeted.
// The inner LIMIT (DegreeScanBudget) is a SCAN budget — it caps how many RAW
// link rows per direction ever reach the block join (cost ceiling in the
// visible-poor case). The outer LIMIT (DegreeHitCap) is a HIT budget — it
// stops after 201 VISIBLE neighbors (renders as "200+", visible-rich case).
// Degree counts all five relationship types; the count is scope-visible —
// a raw count would leak the existence of foreign private links on shared
// blocks (design §6.3, decision §7.2).
func fillDegrees(ctx context.Context, pool *pgxpool.Pool, ids, readScopes []string, nodes []GraphNode) error {
	vis := VisibilityPredicate("nb", "$2")
	q := fmt.Sprintf(`
SELECT n.id::text,
       (SELECT count(*) FROM (
            SELECT 1
            FROM (SELECT l.target_block_id AS nb_id
                  FROM context_dream_links l
                  WHERE l.source_block_id = n.id
                  LIMIT $3) raw
            JOIN context_blocks nb ON nb.id = raw.nb_id
            WHERE %s
            UNION ALL
            SELECT 1
            FROM (SELECT l.source_block_id AS nb_id
                  FROM context_dream_links l
                  WHERE l.target_block_id = n.id
                  LIMIT $3) raw
            JOIN context_blocks nb ON nb.id = raw.nb_id
            WHERE %s
            LIMIT $4
        ) c)::int AS degree
FROM unnest($1::uuid[]) AS n(id)`, vis, vis)

	rows, err := pool.Query(ctx, q, ids, readScopes, DegreeScanBudget, DegreeHitCap)
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
// instead of an empty array (which would match nothing).
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
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
func LogGraphAccess(ctx context.Context, pool *pgxpool.Pool, apiKeyID, focus string, hops, limit, nodeCount int) error {
	meta, err := json.Marshal(map[string]any{
		"focus":      focus,
		"hops":       hops,
		"limit":      limit,
		"node_count": nodeCount,
	})
	if err != nil {
		return fmt.Errorf("store: graph access metadata: %w", err)
	}

	var keyID any // NULL-safe: empty key id must not break the uuid cast
	if apiKeyID != "" {
		keyID = apiKeyID
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata)
		 VALUES ($1::uuid, NULL, 'graph', $2::jsonb)`,
		keyID, meta,
	)
	if err != nil {
		return fmt.Errorf("store: log graph access: %w", err)
	}
	return nil
}
