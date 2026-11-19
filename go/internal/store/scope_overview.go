package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ScopeOverview is one per-scope aggregate row for the server-admin
// `scope-overview` action (MT 04-W6/A0, design/04 §3): the block + key counts and
// the owning tenant for a single scope. It carries COUNTS ONLY — never any block
// title, body or other content — so a server-admin can read the whole scope
// landscape (the GROUP BY is deliberately unscoped) without a cross-tenant content
// leak. TenantID is a pointer: a scope that maps to NO tenant (a system scope like
// '_global', or a scope not yet assigned in context_tenant_scopes) serializes as
// null rather than "".
//
// The JSON field names are a FROZEN contract with the frontend ScopeOverview type
// (design/04 §3) — do NOT rename scope/block_count/key_count/tenant_id.
type ScopeOverview struct {
	Scope      string  `json:"scope"`
	BlockCount int     `json:"block_count"`
	KeyCount   int     `json:"key_count"`
	TenantID   *string `json:"tenant_id"`
}

// ScopeOverviews returns one ScopeOverview per scope across the whole store, for
// the server-admin scope-overview read (design/04 §3, A0). The shape is the
// ListCategories GROUP-BY (blocks.go:765) generalised over scope and merged with
// the (small) key + tenant-scope tables.
//
// It takes NO readScopes parameter ON PURPOSE: this is a server-admin-only global
// aggregate (the tierServerAdmin gate at the manage dispatch is the
// authorisation), and only per-scope COUNTS plus the scope→tenant mapping leave
// the store — never block content. That is why the deliberate omission of the
// `scope = ANY($1)` filter that ListCategories carries is NOT a cross-tenant leak
// (design/04 §3 A0). The block counts sum the block counters of migration 170
// (scopeOverviewBlockCountsSQL, issue #271), so the read costs the number of
// counter rows, not a pass over every block.
//
// The scope universe is the UNION of every scope that appears in context_blocks,
// context_api_keys (home_scope ∪ allowed_scopes) or context_tenant_scopes, so a
// scope with zero blocks but an owning tenant (a freshly assigned scope) or a
// key-only scope still appears. Rows are returned ordered by scope for a
// deterministic, testable result; an empty store yields an empty (non-nil) slice.
func ScopeOverviews(ctx context.Context, pool *pgxpool.Pool) ([]ScopeOverview, error) {
	overviews := map[string]*ScopeOverview{}
	get := func(scope string) *ScopeOverview {
		if o, ok := overviews[scope]; ok {
			return o
		}
		o := &ScopeOverview{Scope: scope}
		overviews[scope] = o
		return o
	}

	// 1. Per-scope block counts — the block counters WITHOUT a readScopes
	//    filter (see the doc comment for why that is safe here).
	if err := scanScopeCounts(ctx, pool, scopeOverviewBlockCountsSQL,
		func(scope string, n int) { get(scope).BlockCount = n }); err != nil {
		return nil, fmt.Errorf("store: scope overview block counts: %w", err)
	}

	// 2. Per-scope key counts — a key counts toward a scope if that scope is its
	//    home_scope OR appears in allowed_scopes; count DISTINCT id so a key whose
	//    scope is both home and allowed is not double-counted. Small table (a key
	//    register), so the unnest + group is cheap regardless of the corpus size.
	if err := scanScopeCounts(ctx, pool,
		`SELECT scope, count(DISTINCT id)::int FROM (
		     SELECT id, home_scope AS scope FROM context_api_keys
		     UNION ALL
		     SELECT id, unnest(allowed_scopes) AS scope FROM context_api_keys
		 ) ks GROUP BY scope`,
		func(scope string, n int) { get(scope).KeyCount = n }); err != nil {
		return nil, fmt.Errorf("store: scope overview key counts: %w", err)
	}

	// 3. scope → tenant mapping (Modell C: one scope = one tenant). Small table.
	//    A scope absent here keeps a nil TenantID (system / unassigned → null).
	tenantRows, err := pool.Query(ctx,
		`SELECT scope, tenant_id::text FROM context_tenant_scopes`)
	if err != nil {
		return nil, fmt.Errorf("store: scope overview tenant map: %w", err)
	}
	defer tenantRows.Close()
	for tenantRows.Next() {
		var scope, tenantID string
		if err := tenantRows.Scan(&scope, &tenantID); err != nil {
			return nil, fmt.Errorf("store: scope overview tenant map scan: %w", err)
		}
		tid := tenantID
		get(scope).TenantID = &tid
	}
	if err := tenantRows.Err(); err != nil {
		return nil, fmt.Errorf("store: scope overview tenant map rows: %w", err)
	}

	scopes := make([]string, 0, len(overviews))
	for s := range overviews {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	results := make([]ScopeOverview, 0, len(scopes))
	for _, s := range scopes {
		results = append(results, *overviews[s])
	}
	return results, nil
}

// scopeOverviewBlockCountsSQL is the per-scope block count of ScopeOverviews:
// every block, archived or not, summed over the block counters (folded rows
// plus pending deltas, blockCountRowsSQL; issue #271). Its cost is the number
// of counter rows of the store (categories × guard states × archived per
// scope, plus a few seconds of deltas), not the number of blocks. A scope
// whose counters sum to zero has no blocks and is left out, as the count left
// it out. The numbers are the counters', so a drift the triggers cannot see
// (block_counts.go) shows here until the reconcile corrects it, as in stats.
const scopeOverviewBlockCountsSQL = `SELECT scope, sum(n)::int
	  FROM ` + blockCountRowsSQL + ` k
	 GROUP BY scope
	HAVING sum(n) <> 0`

// scanScopeCounts runs a (scope, count) aggregate query and feeds each row to set.
// Shared by the block-count and key-count passes of ScopeOverviews so both close
// their result set before the next query runs on the pool.
func scanScopeCounts(ctx context.Context, pool *pgxpool.Pool, query string, set func(scope string, n int)) error {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var n int
		if err := rows.Scan(&scope, &n); err != nil {
			return err
		}
		set(scope, n)
	}
	return rows.Err()
}
