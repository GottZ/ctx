// FullGraph — the flat "load all" seed behind GET /api/graph/all (SPA
// load-all button). NO traversal: the node set is simply every visible block
// (canonical visibility triple + the ego node filters, read as the triple's
// scope arms and grant arm, fullGraphSeed), newest-first with an id
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
	"strings"

	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/visibility"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fullGraphSeedFilters are the ego node filters of the flat seed's planned
// form, over b: the type allowlist and the archive guard of the visibility
// triple (visibility.TypeVisible), the category set and the created window.
var fullGraphSeedFilters = visibility.TypeVisible("b", "$3") + `
		            AND ($5::text[] IS NULL OR b.category = ANY($5))
		            AND ($6::timestamptz IS NULL OR b.created_at >= $6)
		            AND ($7::timestamptz IS NULL OR b.created_at <  $7)`

// fullGraphSeedMergeScopes is the most distinct read scopes FullGraph reads
// one arm each for (fullGraphSeedMergeSQL); a reader of more scopes, and a
// seed filtered by category, is read by fullGraphSeedPlannedSQL (issue #536).
// Fixed by measurement, not policy. The merge form reads at most the page,
// a descent per arm and the ties at the page's end, wherever the read
// scopes' blocks sit in the corpus — its cost is bounded by the limit and the
// scope count, not by the corpus. What grows with the scope count: its plan
// (built in 0.7 ms for one scope, 8-10 ms for 20-32 at 1 000 000 rows; 46-52
// ms for 200, 230-240 ms for 500 at 200 000) and the descents, and where the
// read scopes hold most of the corpus's newest blocks, interleaved with each
// other on the heap, the walk of idx_context_created reads fewer buffers
// than one walk per scope: 1.3-1.8 times for 20-32 of 200 scopes, 1 629
// against 90 for 32 of 40 at 1 000 000 rows, 2 302 against 944 for all 200
// of 200 at 200 000 — beyond the bound the planned form keeps the walk the
// planner picks for such a reader.
const fullGraphSeedMergeScopes = 32

// fullGraphSeedMergeSQL is FullGraph's flat seed for scopes distinct read
// scopes ($1, 1 <= scopes <= fullGraphSeedMergeScopes) without a category
// filter ($5 is NULL): the newest visible blocks (created_at DESC, id), at
// most $4, read as one arm per read scope and one for the granted blocks ($2)
// outside them, merged in their order (issue #536). Each scope's arm walks
// idx_context_scope_active (scope, created_at DESC) WHERE NOT is_archived
// newest first; a MergeAppend pulls from the arm whose head is newest and
// stops when the page is full, so a page reads its own rows plus a descent per
// arm (and an incremental sort on id), wherever the read scopes' blocks sit
// among the corpus — the statement before 536 walked idx_context_created from
// the newest block of the corpus (340 633 buffers on the generic plan at
// random_page_cost 4 for a read scope of 300 behind 1 000 000 newer blocks),
// and the per-scope LATERAL of the first form of 536 read a page per read
// scope whether the merge kept its rows or not (302 998 buffers for 200
// scopes at 1 000 000 rows). The arm states its scope as an equality, so the
// arm's order reduces to created_at and the arms merge, and again as a range
// of a parameter, which the generic plan (fullGraphSeedMergePlan) estimates at
// 0.5 % of the scope's share: a walk of idx_context_created that filters the
// scope then costs the planner more than the table, and the arm walks its
// scope's range. It takes the rows that tie with its last one (FETCH FIRST …
// WITH TIES), so the merge orders a created_at that several blocks share by
// id. The created window is stated only when it is set (after, before), as a
// bound of the walk: the generic plan cannot read a bound out of "$6 IS NULL
// OR …" and would filter the scope's newer blocks row by row instead. The
// granted arm's created_at is an expression of the column, which no index
// orders: its rows are found by primary key and sorted (the one sort every
// plan of the statement has), where a walk of idx_context_created would
// deliver their order without a sort and read the corpus to find them. A
// block of a read scope that is also granted is read by its scope's arm only
// (the granted arm excludes the read scopes). What stays linear: an arm reads
// on through its range while its newest blocks are not of a visible type, and
// reads the blocks of one created_at at the end of its page whole.
func fullGraphSeedMergeSQL(scopes int, after, before bool) string {
	filters := visibility.TypeVisible("b", "$3") + `
		            AND $5::text[] IS NULL`
	if after {
		filters += `
		            AND b.created_at >= $6`
	} else {
		filters += `
		            AND $6::timestamptz IS NULL`
	}
	if before {
		filters += `
		            AND b.created_at < $7`
	} else {
		filters += `
		            AND $7::timestamptz IS NULL`
	}
	var b strings.Builder
	b.WriteString(`SELECT u.id::text, left(u.title, 120), u.category, u.scope::text, u.created_at
		 FROM (
		   (SELECT s.id, s.title, s.category, s.scope, s.created_at
		      FROM (`)
	for i := 1; i <= scopes; i++ {
		if i > 1 {
			b.WriteString(`
		        UNION ALL`)
		}
		fmt.Fprintf(&b, `
		        (SELECT b.id, b.title, b.category, b.scope, b.created_at
		           FROM context_blocks b
		          WHERE b.scope = ($1::text[])[%[1]d]
		            AND b.scope >= ($1::text[])[%[1]d] AND b.scope <= ($1::text[])[%[1]d]
		            AND %[2]s
		          ORDER BY b.created_at DESC
		          FETCH FIRST $4 ROWS WITH TIES)`, i, filters)
	}
	b.WriteString(`) s
		     ORDER BY s.created_at DESC, s.id
		     LIMIT $4)
		   UNION ALL
		   (SELECT b.id, b.title, b.category, b.scope, b.created_at + interval '0' AS created_at
		      FROM context_blocks b
		     WHERE b.id = ANY($2::uuid[]) AND NOT (b.scope = ANY($1::text[]))
		       AND ` + filters + `
		     ORDER BY 5 DESC, 1
		     LIMIT $4)
		 ) u
		 ORDER BY u.created_at DESC, u.id
		 LIMIT $4`)
	return b.String()
}

// fullGraphSeedMergePlan are the planner settings of fullGraphSeedMergeSQL,
// in its own read-only transaction. Full sorts off (incremental sorts stay
// on: the merge needs one for its id tiebreak): a sort of every arm's rows is
// then never cheaper than the merge. The generic plan: a custom plan
// estimates the arm's scope from its statistics, and for a scope that holds a
// large share of the table it weighed the walk of idx_context_created as
// cheap — at random_page_cost 4 an arm of a scope of half the corpus, older
// than the other half, walked the other half (4 895 buffers against 77 at
// 200 000 rows). The generic plan is the same for every reader of as many
// scopes, built once per connection (fullGraphSeedMergeMode).
const fullGraphSeedMergePlan = "SET LOCAL enable_sort = off; SET LOCAL plan_cache_mode = force_generic_plan"

// fullGraphSeedMergeMode sends fullGraphSeedMergeSQL as a prepared
// statement, whose generic plan the connection keeps; a pool without a
// statement cache sends it unnamed (planned generic on every execution).
// Explicit, so a pool whose default mode interpolates the parameters as
// literals still plans the statement without the reader's values.
func fullGraphSeedMergeMode(pool *pgxpool.Pool) pgx.QueryExecMode {
	if pool.Config().ConnConfig.StatementCacheCapacity > 0 {
		return pgx.QueryExecModeCacheStatement
	}
	return pgx.QueryExecModeDescribeExec
}

// fullGraphSeedPlannedSQL is FullGraph's flat seed for a reader of more than
// fullGraphSeedMergeScopes scopes, or with a category filter: the statement
// before issue #536 with its scope-or-grant disjunction read as two arms (the
// read scopes $1, the granted blocks $2), merged by UNION, sent with
// CustomPlanMode — planned with the reader's scopes on every execution, so
// the planner weighs the walk of idx_context_created against a bitmap of the
// read scopes with their statistics. The generic plan a pooled connection
// switched to from its sixth execution cannot: it walked idx_context_created
// for a read scope of 300 behind 1 000 000 newer blocks (340 633 buffers),
// read 44 510 for 5 000 scopes where the walk read 77; the disjunction in one
// WHERE made the custom plan walk too when a granted block was newer than the
// read scopes. A category filter goes here because the planner can read the
// category's selectivity (a category none of a scope's blocks carries: 4
// buffers through idx_blocks_scope_category_updated, where an arm of the merge
// reads its scope's range). What stays: the walk the custom plan chooses for
// read scopes it believes spread over the corpus reads every newer block when
// they are older than it.
var fullGraphSeedPlannedSQL = `SELECT u.id::text, left(u.title, 120), u.category, u.scope::text, u.created_at
		 FROM (
		   (SELECT b.id, b.title, b.category, b.scope, b.created_at
		      FROM context_blocks b
		     WHERE b.scope = ANY($1::text[])
		       AND ` + fullGraphSeedFilters + `
		     ORDER BY b.created_at DESC, b.id
		     LIMIT $4)
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

// distinctScopes returns scopes without repeats, in their first order: the
// merge form reads one arm per scope.
func distinctScopes(scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// fullGraphSeed reads FullGraph's flat seed: fullGraphSeedMergeSQL in a
// read-only transaction with fullGraphSeedMergePlan for a reader of at most
// fullGraphSeedMergeScopes scopes and no category filter, else
// fullGraphSeedPlannedSQL, planned with its parameters on every execution
// (CustomPlanMode).
func fullGraphSeed(ctx context.Context, pool *pgxpool.Pool, p EgoParams, readScopes, grantedBlockIDs, visibleTypes []string) ([]GraphNode, error) {
	scopes := distinctScopes(readScopes)
	merge := len(scopes) <= fullGraphSeedMergeScopes && len(p.Categories) == 0
	mode := CustomPlanMode(pool)
	if merge {
		mode = fullGraphSeedMergeMode(pool)
	}
	args := []any{
		mode,
		scopes,                   // $1
		grantedBlockIDs,          // $2 block-grant arm (T40a; nil-wired like ego)
		visibleTypes,             // $3 registry type allowlist (T6)
		p.Limit + 1,              // $4 one extra row → exact truncation flag
		nilIfEmpty(p.Categories), // $5
		p.CreatedAfter,           // $6
		p.CreatedBefore,          // $7
	}
	var nodes []GraphNode
	read := func(rows pgx.Rows, err error) error {
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
	}
	if !merge {
		if err := read(pool.Query(ctx, fullGraphSeedPlannedSQL, args...)); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	sql := fullGraphSeedMergeSQL(len(scopes), p.CreatedAfter != nil, p.CreatedBefore != nil)
	if err := readTx(ctx, pool, "store: graph full seed", fullGraphSeedMergePlan, func(tx pgx.Tx) error {
		return read(tx.Query(ctx, sql, args...))
	}); err != nil {
		return nil, err
	}
	return nodes, nil
}

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
	nodes, err := fullGraphSeed(ctx, pool, p, readScopes, grantedBlockIDs, visibleTypes)
	if err != nil {
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
