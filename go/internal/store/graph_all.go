// FullGraph — the flat "load all" seed behind GET /api/graph/all (SPA
// load-all button). NO traversal: the node set is simply every visible block
// (canonical visibility triple + the ego node filters), newest-first with an
// id tiebreak so truncation is deterministic. Edges and degrees then run
// through the SHARED SQL stages (egoSQLEdges = Q2/Q2s/Q3 verbatim) — scope
// safety holds by the Q2 invariant (both endpoints passed the triple), and a
// second copy of any visibility check would be a second truth; there is none.
//
// The bridge-leak / cap-starvation machinery of EgoGraph does not apply here:
// nothing is traversed THROUGH, no per-node cap slot is granted — the node set
// is predicate-defined, not reachability-defined. Deliberately SQL-only: the
// W05.5 cache arm answers frontier walks, a flat listing gains nothing from it.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/visibility"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FullGraph returns up to p.Limit visible blocks (created_at DESC, id) plus
// the induced edges of both data classes and the visible degrees. p.Focus,
// p.Hops and p.PerNodeCap are ignored — EgoResult.Focus is "" (no focus
// exists). Truncated reports node OR edge budget exhaustion, exactly like
// EgoGraph.
func FullGraph(ctx context.Context, pool *pgxpool.Pool, p EgoParams, readScopes, grantedBlockIDs, visibleTypes []string) (*EgoResult, error) {
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

	// Flat seed: the ego NODE filters (category, created window) apply; edge
	// filters (min_confidence, link_class) belong to the edge stages below.
	// LIMIT reads one row beyond the budget so the truncation flag is exact.
	q := fmt.Sprintf(
		`SELECT b.id::text, left(b.title, 120), b.category, b.scope::text, b.created_at
		 FROM context_blocks b
		 WHERE %s
		   AND ($5::text[] IS NULL OR b.category = ANY($5))
		   AND ($6::timestamptz IS NULL OR b.created_at >= $6)
		   AND ($7::timestamptz IS NULL OR b.created_at <  $7)
		 ORDER BY b.created_at DESC, b.id
		 LIMIT $4`,
		visibility.Predicate("b", "$3", "$1", "$2"),
	)
	rows, err := pool.Query(ctx, q,
		readScopes,               // $1
		grantedBlockIDs,          // $2 block-grant OR-arm (T40a; nil-wired like ego)
		visibleTypes,             // $3 registry type allowlist (T6)
		p.Limit+1,                // $4 one extra row → exact truncation flag
		nilIfEmpty(p.Categories), // $5
		p.CreatedAfter,           // $6
		p.CreatedBefore,          // $7
	)
	if err != nil {
		return nil, fmt.Errorf("store: graph full seed query: %w", err)
	}
	defer rows.Close()

	var nodes []GraphNode
	for rows.Next() {
		// Hop 1 for every node: there is no hop-0 focus, and the client seeds
		// new nodes on a hop-scaled ring — hop 0 would stack the whole corpus
		// on one point before the layout worker untangles it.
		n := GraphNode{Hop: 1}
		if err := rows.Scan(&n.ID, &n.Title, &n.Category, &n.Scope, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: graph full seed scan: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: graph full seed rows: %w", err)
	}

	budget := graphcache.NewBudgetReport(graphcache.SourceSQL)
	truncated := false
	if len(nodes) > p.Limit {
		nodes = nodes[:p.Limit]
		truncated = true
		budget.Add(graphcache.TravNodeLimitReached)
	}

	ids := make([]string, len(nodes))
	index := make(map[string]int, len(nodes))
	for i := range nodes {
		ids[i] = nodes[i].ID
		index[nodes[i].ID] = i
	}

	sql := &egoSQLEdges{pool: pool, p: p, readScopes: readScopes, grantedBlockIDs: grantedBlockIDs, visibleTypes: visibleTypes}
	es, err := sql.edges(ctx, ids, index)
	if err != nil {
		return nil, err
	}
	if es.DreamTrunc {
		truncated = true
		budget.Add(graphcache.TravEdgeLimitReached)
	}
	if es.StructTrunc {
		truncated = true
		budget.Add(graphcache.TravEdgeLimitReached)
	}
	if err := sql.degrees(ctx, ids, nodes); err != nil {
		return nil, err
	}

	return &EgoResult{
		Focus:       "",
		Rels:        GraphRels,
		StructRels:  es.StructRels,
		Origins:     es.Origins,
		Nodes:       nodes,
		Edges:       es.Dream,
		StructEdges: es.Struct,
		Truncated:   truncated,
		Budget:      budget,
	}, nil
}
