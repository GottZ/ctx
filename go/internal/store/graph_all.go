// FullGraph — the flat "load all" seed behind GET /api/graph/all (SPA
// load-all button). NO traversal: the node set is simply every visible block
// (canonical visibility triple + the ego node filters, read as the triple's
// scope arm and grant arm, fullGraphSeedSQL), newest-first with an id
// tiebreak so truncation is deterministic. Edges and degrees then run
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fullGraphSeedFilters are the ego node filters of the flat seed, over b:
// the type allowlist and the archive guard of the visibility triple
// (visibility.TypeVisible), the category set and the created window.
var fullGraphSeedFilters = visibility.TypeVisible("b", "$3") + `
		            AND ($5::text[] IS NULL OR b.category = ANY($5))
		            AND ($6::timestamptz IS NULL OR b.created_at >= $6)
		            AND ($7::timestamptz IS NULL OR b.created_at <  $7)`

// fullGraphSeedSQL is FullGraph's flat seed: the newest visible blocks
// (created_at DESC, id), at most $4. The visibility triple's scope-or-grant
// disjunction (visibility.Predicate) is read as its two arms, each a page of
// its own, merged and cut again (issue #536): one LATERAL per read scope ($1)
// with the scope stated as a range and leading the ORDER BY — the device of
// statsSQL (#512) — and the granted blocks ($2) by primary key. With the
// disjunction in one WHERE the planner walked idx_context_created from the
// newest block of the corpus and filtered scope and grant row by row until
// $4 rows qualified: on the generic plan at both random_page_cost values, and
// on the custom plan when a granted block was newer than the read scopes'
// blocks, a read scope older than the rest of the corpus read every newer
// block of every other scope (34 392 buffers at 100 000 other blocks). A
// scope's arm orders by exactly the key of idx_context_scope_active (scope,
// created_at DESC) WHERE NOT is_archived, so the walk of that index needs no
// sort and stops after the page, and takes the rows that tie with its last
// one (FETCH FIRST … WITH TIES), so the merge orders a created_at that
// several blocks share by id as the statement before did. The planner
// estimates a scope stated through the LATERAL as a range of the table's
// average size, and its custom plan at random_page_cost 4 still read the
// whole range of a large read scope through idx_context_scope and sorted it
// (5 657 buffers for a page of a scope of 100 000 blocks), so the statement
// runs with sorts off (readNoSort), where the walk is the arm's only path
// without a sort (37 buffers). UNION
// drops a block that is in a read scope and granted, and a scope named twice;
// the range admits exactly the scope's rows (scope is NOT NULL, a database's
// collation deterministic). What stays linear is inside a read scope: the
// blocks of one created_at at the end of its page are read whole (one
// statement writing many of them), and a category or type filter that none of
// its newest blocks match reads on through its range.
var fullGraphSeedSQL = `SELECT u.id::text, left(u.title, 120), u.category, u.scope::text, u.created_at
		 FROM (
		   (SELECT x.id, x.title, x.category, x.scope, x.created_at
		      FROM unnest($1::text[]) AS s(scope)
		      CROSS JOIN LATERAL (
		        SELECT b.id, b.title, b.category, b.scope, b.created_at
		          FROM context_blocks b
		         WHERE b.scope >= s.scope AND b.scope <= s.scope
		           AND ` + fullGraphSeedFilters + `
		         ORDER BY b.scope, b.created_at DESC
		         FETCH FIRST $4 ROWS WITH TIES) x)
		   UNION
		   (SELECT b.id, b.title, b.category, b.scope, b.created_at
		      FROM context_blocks b
		     WHERE b.id = ANY($2::uuid[])
		       AND ` + fullGraphSeedFilters + `
		     ORDER BY b.created_at DESC, b.id
		     LIMIT $4)
		 ) u
		 ORDER BY u.created_at DESC, u.id
		 LIMIT $4`

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
	// The seed runs in a read-only transaction with sorts off (readNoSortTx):
	// each read scope's arm then walks idx_context_scope_active and stops
	// after its page.
	var nodes []GraphNode
	if err := readNoSortTx(ctx, pool, "store: graph full seed", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, fullGraphSeedSQL,
			readScopes,               // $1
			grantedBlockIDs,          // $2 block-grant OR-arm (T40a; nil-wired like ego)
			visibleTypes,             // $3 registry type allowlist (T6)
			p.Limit+1,                // $4 one extra row → exact truncation flag
			nilIfEmpty(p.Categories), // $5
			p.CreatedAfter,           // $6
			p.CreatedBefore,          // $7
		)
		if err != nil {
			return fmt.Errorf("store: graph full seed query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			// Hop 1 for every node: there is no hop-0 focus, and the client seeds
			// new nodes on a hop-scaled ring — hop 0 would stack the whole corpus
			// on one point before the layout worker untangles it.
			n := GraphNode{Hop: 1}
			if err := rows.Scan(&n.ID, &n.Title, &n.Category, &n.Scope, &n.CreatedAt); err != nil {
				return fmt.Errorf("store: graph full seed scan: %w", err)
			}
			nodes = append(nodes, n)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: graph full seed rows: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
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
