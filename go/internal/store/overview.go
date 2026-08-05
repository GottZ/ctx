// GraphOverview is the read path of the F5-W6 landkarte (GET /api/graph/overview).
// It reads the scope-PARTITIONED aggregate tables (migration 057, built offline
// by internal/overview) and sums ONLY the rows of the caller's readScopes — so
// every size/weight is scope-pure by construction, never aggregated across scope
// boundaries. cluster_id stays store-internal; the handler maps it to a
// per-request ordinal (no existence oracle, design §6.1). See 07-graph-overview.md.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OverviewNode is one meta-node (cluster), already scope-pure aggregated.
// ClusterID is internal — the handler emits a per-request ordinal instead.
type OverviewNode struct {
	ClusterID     string
	Size          int      // Σ size over the caller's visible scopes
	TopCategories []string // merged from category_counts of the visible scopes
	ReprID        string   // representative block (highest repr_quality, visible scope)
	ReprTitle     string
	ScopeMix      []string // contributing scopes ⊆ readScopes
}

// OverviewEdge is an aggregated inter-cluster meta-edge (undirected, scope-pure).
type OverviewEdge struct {
	A, B      string // internal cluster_ids (handler maps to ordinals)
	LinkCount int
	Weight    float64
}

// OverviewParams are the validated request parameters (ceilings in the handler).
type OverviewParams struct {
	MinClusterSize int
	MinInterWeight float64
	NodeLimit      int
	EdgeLimit      int
}

// OverviewResult is the store-internal result; the handler builds the wire
// envelope (overviewResponse) and assigns ordinals.
type OverviewResult struct {
	Nodes      []OverviewNode
	Edges      []OverviewEdge
	ComputedAt time.Time // graph_overview_meta.computed_at; zero = never built
	Truncated  bool
}

// GraphOverview assembles the scope-pure cluster supergraph for the caller.
// Three small reads over the precomputed aggregate tables — no gonum, no
// context_blocks/context_dream_links touch, bounded by cluster(-pair) count,
// not corpus size.
func GraphOverview(ctx context.Context, pool *pgxpool.Pool, p OverviewParams, readScopes []string) (*OverviewResult, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	nodes, truncNodes, err := overviewNodes(ctx, pool, p, readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: overview nodes: %w", err)
	}
	if len(nodes) > 0 {
		if err := fillTopCategories(ctx, pool, nodes, readScopes); err != nil {
			return nil, fmt.Errorf("store: overview categories: %w", err)
		}
	}

	clusterIDs := make([]string, len(nodes))
	for i := range nodes {
		clusterIDs[i] = nodes[i].ClusterID
	}
	edges, truncEdges, err := overviewEdges(ctx, pool, p, readScopes, clusterIDs)
	if err != nil {
		return nil, fmt.Errorf("store: overview edges: %w", err)
	}

	var computedAt time.Time
	// Per-scope meta rows (B-W5, migration 088): freshness is the newest
	// rebuild among the CALLER's scopes — never an unscoped row pick, which
	// would leak a foreign partition's computed_at as our own (B1-m1). No
	// matching row (never built) leaves computedAt zero — not an error.
	var computedAtP *time.Time
	_ = pool.QueryRow(ctx,
		`SELECT max(computed_at) FROM graph_overview_meta WHERE scope = ANY($1)`,
		readScopes).Scan(&computedAtP)
	if computedAtP != nil {
		computedAt = *computedAtP
	}

	return &OverviewResult{
		Nodes:      nodes,
		Edges:      edges,
		ComputedAt: computedAt,
		Truncated:  truncNodes || truncEdges,
	}, nil
}

// overviewNodes sums per-(cluster,scope) partial rows over the visible scopes.
// SCOPE-PURE: WHERE scope = ANY(readScopes) — a node never counts a member of an
// invisible scope. HAVING drops clusters with no visible member (vector 4).
func overviewNodes(ctx context.Context, pool *pgxpool.Pool, p OverviewParams, readScopes []string) ([]OverviewNode, bool, error) {
	rows, err := pool.Query(ctx, `
		SELECT cluster_id::text,
		       sum(size)::int                                                       AS visible_size,
		       (array_agg(repr_block_id ORDER BY repr_quality DESC, repr_block_id))[1]::text AS repr_id,
		       (array_agg(repr_title    ORDER BY repr_quality DESC, repr_block_id))[1]       AS repr_title,
		       array_agg(DISTINCT scope ORDER BY scope)                             AS scope_mix
		FROM graph_cluster_node
		WHERE scope = ANY($1::text[])
		GROUP BY cluster_id
		HAVING sum(size) >= $2
		ORDER BY sum(size) DESC, cluster_id
		LIMIT $3`,
		readScopes, p.MinClusterSize, p.NodeLimit+1) // +1 detects truncation
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []OverviewNode
	for rows.Next() {
		var n OverviewNode
		if err := rows.Scan(&n.ClusterID, &n.Size, &n.ReprID, &n.ReprTitle, &n.ScopeMix); err != nil {
			return nil, false, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	truncated := len(out) > p.NodeLimit
	if truncated {
		out = out[:p.NodeLimit]
	}
	return out, truncated, nil
}

// fillTopCategories merges the per-scope category_counts of the returned clusters
// over the visible scopes and keeps the top 3 per cluster. Scope-pure (same
// readScopes filter); a foreign-scope category can never appear.
func fillTopCategories(ctx context.Context, pool *pgxpool.Pool, nodes []OverviewNode, readScopes []string) error {
	ids := make([]string, len(nodes))
	idx := make(map[string]int, len(nodes))
	for i := range nodes {
		ids[i] = nodes[i].ClusterID
		idx[nodes[i].ClusterID] = i
	}

	rows, err := pool.Query(ctx, `
		SELECT cluster_id::text, kv.key, sum((kv.value)::int)::int
		FROM graph_cluster_node, jsonb_each(category_counts) kv
		WHERE scope = ANY($1::text[]) AND cluster_id = ANY($2::uuid[])
		GROUP BY cluster_id, kv.key`,
		readScopes, ids)
	if err != nil {
		return err
	}
	defer rows.Close()

	type catCount struct {
		cat string
		cnt int
	}
	per := make(map[string][]catCount, len(nodes))
	for rows.Next() {
		var cid, cat string
		var cnt int
		if err := rows.Scan(&cid, &cat, &cnt); err != nil {
			return err
		}
		per[cid] = append(per[cid], catCount{cat, cnt})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for cid, ccs := range per {
		sort.Slice(ccs, func(i, j int) bool {
			if ccs[i].cnt != ccs[j].cnt {
				return ccs[i].cnt > ccs[j].cnt
			}
			return ccs[i].cat < ccs[j].cat // deterministic tiebreak
		})
		top := make([]string, 0, 3)
		for i := 0; i < len(ccs) && i < 3; i++ {
			top = append(top, ccs[i].cat)
		}
		nodes[idx[cid]].TopCategories = top
	}
	return nil
}

// overviewEdges sums per-(cluster-pair, scope-pair) partial rows where BOTH
// endpoint scopes are visible (the meta-edge analogue of inducedEdges' two-
// endpoint rule). Restricted to the returned clusters so every edge maps to a
// node ordinal.
func overviewEdges(ctx context.Context, pool *pgxpool.Pool, p OverviewParams, readScopes, clusterIDs []string) ([]OverviewEdge, bool, error) {
	// EdgeLimit <= 0 short-circuits WITHOUT touching graph_cluster_edge (W-B):
	// the root map renders cluster lines only, and asking for zero edges should
	// cost zero queries. Unreachable from GET /api/graph/overview — its parser
	// enforces edge_limit >= 1 (handler/overview.go) — so the wire envelope and
	// its golden test are untouched by this branch.
	if len(clusterIDs) == 0 || p.EdgeLimit <= 0 {
		return nil, false, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT cluster_a::text, cluster_b::text, sum(link_count)::int, sum(weight_sum)::float8
		FROM graph_cluster_edge
		WHERE scope_s = ANY($1::text[]) AND scope_t = ANY($1::text[])
		  AND cluster_a = ANY($2::uuid[]) AND cluster_b = ANY($2::uuid[])
		GROUP BY cluster_a, cluster_b
		HAVING sum(weight_sum) >= $3
		ORDER BY sum(link_count) DESC, cluster_a, cluster_b
		LIMIT $4`,
		readScopes, clusterIDs, p.MinInterWeight, p.EdgeLimit+1) // +1 detects truncation
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []OverviewEdge
	for rows.Next() {
		var e OverviewEdge
		if err := rows.Scan(&e.A, &e.B, &e.LinkCount, &e.Weight); err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	truncated := len(out) > p.EdgeLimit
	if truncated {
		out = out[:p.EdgeLimit]
	}
	return out, truncated, nil
}

// LogGraphOverviewAccess writes the single telemetry row for a successful
// overview call: action='graph-overview', block_id=NULL (decoupled from every
// access-count ranking mechanic, like LogGraphAccess). Called only after success.
func LogGraphOverviewAccess(ctx context.Context, pool *pgxpool.Pool, apiKeyID string, nodeCount, edgeCount int) error {
	meta, err := json.Marshal(map[string]any{"node_count": nodeCount, "edge_count": edgeCount})
	if err != nil {
		return fmt.Errorf("store: overview access metadata: %w", err)
	}
	var keyID any // NULL-safe: empty key id must not break the uuid cast
	if apiKeyID != "" {
		keyID = apiKeyID
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata, principal_id)
		 VALUES ($1::uuid, NULL, 'graph-overview', $2::jsonb,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))`,
		keyID, meta,
	); err != nil {
		return fmt.Errorf("store: log graph overview access: %w", err)
	}
	return nil
}
