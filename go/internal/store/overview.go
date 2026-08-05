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

	"github.com/GottZ/ctx/internal/clustersql"
)

// OverviewNode is one meta-node, already scope-pure aggregated.
//
// ClusterID is internal — it is a BLOCK uuid (the smallest member id of the
// community) and must never leave the server. The handler emits a per-request
// ordinal instead, and since W7 the stable TopicID next to it.
//
// TopicID/Label are empty on the legacy path (see GraphOverview): a database
// that has not yet run a rebuild with the identity layer answers exactly as it
// did before, byte for byte.
type OverviewNode struct {
	ClusterID string
	// TopicID is the stable, scope-bound identity (graph_cluster_topic,
	// gen_random_uuid v4 — deliberately NOT block-derived and deliberately
	// without the uuidv7 timestamp, which would be a side channel on when a
	// rebuild first saw this community).
	TopicID string
	// Label is the topic's name. It ACCOMPANIES ReprTitle, it does not replace
	// it (decision E6-01): the only interaction path of today's map is the
	// drill-down on ReprID, and ReprTitle is its caption.
	Label         string
	Size          int      // Σ size over the caller's visible scopes
	TopCategories []string // merged from category_counts of the visible scopes
	// TopCatCounts are the counts of TopCategories, same order and length
	// (W-D). fillTopCategories has always computed them and threw them away;
	// the root map prints "learnings 50 · decisions 30" rather than a bare list
	// of names, and a mix without magnitudes says nothing about the cluster.
	// Additive and unused by the wire envelope (handler/overview.go reads
	// TopCategories), so no response byte moves.
	TopCatCounts []int
	ReprID       string // representative block (highest repr_quality, visible scope)
	ReprTitle    string
	ScopeMix     []string // contributing scopes ⊆ readScopes
}

// Key is the identifier the handler builds its per-request ordinal map on, and
// the space OverviewEdge endpoints live in.
//
// One rule, no flag: a node with an identity is keyed by its identity, a node
// without one by its cluster. The two never mix inside a response, because the
// legacy decision is taken for the WHOLE request.
func (n OverviewNode) Key() string {
	if n.TopicID != "" {
		return n.TopicID
	}
	return n.ClusterID
}

// OverviewEdge is an aggregated inter-cluster meta-edge (undirected, scope-pure).
//
// A/B live in the same identifier space as OverviewNode.Key: topic ids on the
// identity path, cluster ids on the legacy one. The handler maps them to
// ordinals either way and neither value reaches the wire.
type OverviewEdge struct {
	A, B string
	// ScopeA/ScopeB are the endpoint BLOCK scopes of the aggregate row, filled
	// on the identity path only. They are what makes the endpoint resolvable: a
	// topic is a (cluster, scope) partition, so a cluster pair alone cannot name
	// which half of a scope-crossing cluster an edge touches. Internal — they
	// never reach the wire, and they are empty after the topic projection.
	ScopeA, ScopeB string
	LinkCount      int
	Weight         float64
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
	legacy, err := overviewLegacy(ctx, pool, readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: overview legacy probe: %w", err)
	}
	nodes, truncNodes, err := overviewNodes(ctx, pool, p, readScopes, legacy)
	if err != nil {
		return nil, fmt.Errorf("store: overview nodes: %w", err)
	}
	if len(nodes) > 0 {
		if err := fillTopCategories(ctx, pool, nodes, readScopes, legacy); err != nil {
			return nil, fmt.Errorf("store: overview categories: %w", err)
		}
	}

	clusterIDs := make([]string, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		if _, dup := seen[nodes[i].ClusterID]; dup {
			continue // a scope-crossing cluster contributes two nodes, one id
		}
		seen[nodes[i].ClusterID] = struct{}{}
		clusterIDs = append(clusterIDs, nodes[i].ClusterID)
	}
	edges, truncEdges, err := overviewEdges(ctx, pool, p, readScopes, clusterIDs, legacy)
	if err != nil {
		return nil, fmt.Errorf("store: overview edges: %w", err)
	}
	if !legacy {
		edges = projectEdgesOntoTopics(edges, nodes)
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

// overviewLegacy decides, per REQUEST, whether the identity path can be used.
//
// The criterion is deliberately pessimistic in the direction of a COMPLETE map,
// not of a complete identity: the legacy path is taken as soon as ONE readable
// node row lacks a topic_id. A map without topic ids is exactly today's map and
// lossless for a client; a map with missing NODES would be a silent data loss.
//
// The mixed state is real and it lasts: the rebuild serves one tenant per tick
// at a 6 h cadence, so with N tenants full coverage takes N × 6 h, during which
// early-served scopes carry identities and later ones do not. A caller whose
// read scopes span both partitions — the shared-scope case is exactly that —
// would otherwise get a silently halved map from the fail-closed JOIN.
//
// After the rollout the probe is a constant false and costs one index lookup.
func overviewLegacy(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (bool, error) {
	var legacy bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM graph_cluster_node n
		                WHERE `+clustersql.NodeVisible("n", "$1")+` AND n.topic_id IS NULL)`,
		readScopes).Scan(&legacy)
	return legacy, err
}

// overviewNodesTopicSQL is the identity path. With one topic per (cluster,
// scope) — and uq_gcn_scope_topic enforcing it — there is exactly ONE node row
// per topic, so the historical GROUP BY cluster_id collapses to the identity
// and disappears. A scope-crossing cluster (structurally possible, live not
// instantiated) therefore surfaces as TWO map nodes with disjoint scope_mix
// instead of one merged node, which is the whole point of the scope-bound
// identity.
//
// AND t.scope = n.scope is the second half of the B1b closure. It is redundant
// to a correct assignment and stands there precisely for that reason: if the
// matching ever handed a foreign-scope topic to a node row, the row falls out
// here instead of serving another scope's label. It is the line the W7 scope
// gate removes to go red.
var overviewNodesTopicSQL = `
	SELECT n.cluster_id::text, n.topic_id::text, COALESCE(t.label, ''), n.size,
	       n.repr_block_id::text, n.repr_title, ARRAY[n.scope]
	FROM graph_cluster_node n
	JOIN graph_cluster_topic t ON t.topic_id = n.topic_id AND t.scope = n.scope
	WHERE ` + clustersql.NodeVisible("n", "$1") + `
	  AND n.size >= $2
	ORDER BY n.size DESC, n.topic_id
	LIMIT $3`

// overviewNodesLegacySQL is the pre-W3 shape, byte-identical to what it was.
var overviewNodesLegacySQL = `
	SELECT n.cluster_id::text,
	       sum(n.size)::int                                                         AS visible_size,
	       (array_agg(n.repr_block_id ORDER BY n.repr_quality DESC, n.repr_block_id))[1]::text AS repr_id,
	       (array_agg(n.repr_title    ORDER BY n.repr_quality DESC, n.repr_block_id))[1]       AS repr_title,
	       array_agg(DISTINCT n.scope ORDER BY n.scope)                             AS scope_mix
	FROM graph_cluster_node n
	WHERE ` + clustersql.NodeVisible("n", "$1") + `
	GROUP BY n.cluster_id
	HAVING sum(n.size) >= $2
	ORDER BY sum(n.size) DESC, n.cluster_id
	LIMIT $3`

// overviewNodes reads the visible meta-nodes. SCOPE-PURE on both paths:
// WHERE scope = ANY(readScopes) — a node never counts a member of an invisible
// scope.
func overviewNodes(ctx context.Context, pool *pgxpool.Pool, p OverviewParams, readScopes []string, legacy bool) ([]OverviewNode, bool, error) {
	sql := overviewNodesTopicSQL
	if legacy {
		sql = overviewNodesLegacySQL
	}
	rows, err := pool.Query(ctx, sql, readScopes, p.MinClusterSize, p.NodeLimit+1) // +1 detects truncation
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []OverviewNode
	for rows.Next() {
		var n OverviewNode
		if legacy {
			err = rows.Scan(&n.ClusterID, &n.Size, &n.ReprID, &n.ReprTitle, &n.ScopeMix)
		} else {
			err = rows.Scan(&n.ClusterID, &n.TopicID, &n.Label, &n.Size, &n.ReprID, &n.ReprTitle, &n.ScopeMix)
		}
		if err != nil {
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

// projectEdgesOntoTopics maps the cluster-keyed meta-edges onto topic ids.
//
// IN GO, not in SQL. The SQL variant would have to join graph_cluster_node
// twice and could then only apply its filter POST-join, which removes the
// prefilter on graph_cluster_edge — the one table of this family that grows
// with the dream-link count rather than the cluster count. The Go form costs
// zero extra joins.
//
// LEAST/GREATEST re-normalisation is required, not cosmetic: the 057 CHECK
// guarantees cluster_a < cluster_b, which says nothing about the order of the
// topics they map to. Pairs that collapse onto the same topic pair are summed.
//
// The endpoint is resolved by (cluster, SCOPE), not by cluster alone. That is
// the whole reason overviewEdgesTopicSQL keeps the scope pair: a scope-crossing
// cluster is two topics, and a cluster-only lookup would either have to guess
// or drop the edge. An endpoint whose partition is not in the result — below
// min_cluster_size, past the node limit — has no ordinal, so its edge is
// dropped, exactly as before.
//
// An edge that collapses onto ONE topic is dropped too: after the projection it
// is a self-loop, and the map has never drawn those.
func projectEdgesOntoTopics(edges []OverviewEdge, nodes []OverviewNode) []OverviewEdge {
	type endpoint struct{ cluster, scope string }
	topicOf := make(map[endpoint]string, len(nodes))
	for _, n := range nodes {
		scope := ""
		if len(n.ScopeMix) > 0 {
			scope = n.ScopeMix[0] // identity nodes are single-scope by construction
		}
		topicOf[endpoint{n.ClusterID, scope}] = n.TopicID
	}

	merged := make(map[[2]string]*OverviewEdge, len(edges))
	order := make([][2]string, 0, len(edges))
	for _, e := range edges {
		a, okA := topicOf[endpoint{e.A, e.ScopeA}]
		b, okB := topicOf[endpoint{e.B, e.ScopeB}]
		if !okA || !okB || a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		key := [2]string{a, b}
		if cur, ok := merged[key]; ok {
			cur.LinkCount += e.LinkCount
			cur.Weight += e.Weight
			continue
		}
		merged[key] = &OverviewEdge{A: a, B: b, LinkCount: e.LinkCount, Weight: e.Weight}
		order = append(order, key)
	}

	out := make([]OverviewEdge, 0, len(order))
	for _, key := range order {
		out = append(out, *merged[key])
	}
	return out
}

// fillTopCategories merges the per-scope category_counts of the returned nodes
// and keeps the top 3 each. Scope-pure (same readScopes filter); a foreign-scope
// category can never appear.
//
// The KEY differs by path and that is load-bearing. The legacy node is one
// cluster summed over its scopes, so its categories are too. An identity node
// is one (cluster, scope) partition, and a scope-crossing cluster produces two
// of them: keying by cluster_id alone would let one of the two overwrite the
// other in the index map — one node would get no categories at all and the
// other the merged counts of both scope halves. The query therefore projects
// and groups by scope as well; the cluster_id = ANY(...) prefilter and the
// scope filter are untouched.
func fillTopCategories(ctx context.Context, pool *pgxpool.Pool, nodes []OverviewNode, readScopes []string, byCluster bool) error {
	ids := make([]string, 0, len(nodes))
	idx := make(map[string]int, len(nodes))
	catKey := func(cluster, scope string) string {
		if byCluster {
			return cluster
		}
		return cluster + "\x00" + scope
	}
	for i := range nodes {
		ids = append(ids, nodes[i].ClusterID)
		scope := ""
		if !byCluster && len(nodes[i].ScopeMix) > 0 {
			scope = nodes[i].ScopeMix[0]
		}
		idx[catKey(nodes[i].ClusterID, scope)] = i
	}

	rows, err := pool.Query(ctx, `
		SELECT n.cluster_id::text, n.scope, kv.key, sum((kv.value)::int)::int
		FROM graph_cluster_node n, jsonb_each(n.category_counts) kv
		WHERE `+clustersql.NodeVisible("n", "$1")+` AND n.cluster_id = ANY($2::uuid[])
		GROUP BY n.cluster_id, n.scope, kv.key`,
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
		var cid, scope, cat string
		var cnt int
		if err := rows.Scan(&cid, &scope, &cat, &cnt); err != nil {
			return err
		}
		key := catKey(cid, scope)
		// The cluster key drops the scope, so two partial rows of one cluster
		// have to be folded here instead of by the GROUP BY.
		if byCluster {
			merged := per[key]
			found := false
			for i := range merged {
				if merged[i].cat == cat {
					merged[i].cnt += cnt
					found = true
					break
				}
			}
			if !found {
				merged = append(merged, catCount{cat, cnt})
			}
			per[key] = merged
			continue
		}
		per[key] = append(per[key], catCount{cat, cnt})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for key, ccs := range per {
		// A partition whose node fell out of the result (min size, node limit)
		// still comes back from the query, because the prefilter is on
		// cluster_id. Without this check the zero value of the index map would
		// hand its categories to the FIRST node of the response.
		at, ok := idx[key]
		if !ok {
			continue
		}
		sort.Slice(ccs, func(i, j int) bool {
			if ccs[i].cnt != ccs[j].cnt {
				return ccs[i].cnt > ccs[j].cnt
			}
			return ccs[i].cat < ccs[j].cat // deterministic tiebreak
		})
		top := make([]string, 0, 3)
		counts := make([]int, 0, 3)
		for i := 0; i < len(ccs) && i < 3; i++ {
			top = append(top, ccs[i].cat)
			counts = append(counts, ccs[i].cnt)
		}
		nodes[at].TopCategories = top
		nodes[at].TopCatCounts = counts
	}
	return nil
}

// overviewEdgesLegacySQL is the pre-W7 aggregation, byte-identical.
const overviewEdgesLegacySQL = `
	SELECT cluster_a::text, cluster_b::text, '', '', sum(link_count)::int, sum(weight_sum)::float8
	FROM graph_cluster_edge
	WHERE scope_s = ANY($1::text[]) AND scope_t = ANY($1::text[])
	  AND cluster_a = ANY($2::uuid[]) AND cluster_b = ANY($2::uuid[])
	GROUP BY cluster_a, cluster_b
	HAVING sum(weight_sum) >= $3
	ORDER BY sum(link_count) DESC, cluster_a, cluster_b
	LIMIT $4`

// overviewEdgesTopicSQL keeps the SCOPE PAIR in the grouping.
//
// The prefilter — the reason design/01 §4.7 insisted the topic mapping happen
// in Go rather than in SQL — is untouched: graph_cluster_edge is still narrowed
// by (scope_s, scope_t, cluster_a, cluster_b) BEFORE any join, and there is
// still no join. What changes is the granularity of the aggregate, and that is
// what makes the endpoint resolvable at all: a topic is a (cluster, SCOPE)
// partition, so an edge row grouped by cluster pair alone cannot say WHICH half
// of a scope-crossing cluster it belongs to. Dropping such edges would be the
// only honest alternative, and it would drop a real, visible connection.
//
// At the live shape the two forms are identical — every cluster is single-scope,
// so the scope pair adds no rows.
const overviewEdgesTopicSQL = `
	SELECT cluster_a::text, cluster_b::text, scope_s, scope_t, sum(link_count)::int, sum(weight_sum)::float8
	FROM graph_cluster_edge
	WHERE scope_s = ANY($1::text[]) AND scope_t = ANY($1::text[])
	  AND cluster_a = ANY($2::uuid[]) AND cluster_b = ANY($2::uuid[])
	GROUP BY cluster_a, cluster_b, scope_s, scope_t
	HAVING sum(weight_sum) >= $3
	ORDER BY sum(link_count) DESC, cluster_a, cluster_b, scope_s, scope_t
	LIMIT $4`

// overviewEdges sums per-(cluster-pair, scope-pair) partial rows where BOTH
// endpoint scopes are visible (the meta-edge analogue of inducedEdges' two-
// endpoint rule). Restricted to the returned clusters so every edge maps to a
// node ordinal.
func overviewEdges(ctx context.Context, pool *pgxpool.Pool, p OverviewParams, readScopes, clusterIDs []string, legacy bool) ([]OverviewEdge, bool, error) {
	// EdgeLimit <= 0 short-circuits WITHOUT touching graph_cluster_edge (W-B):
	// the root map renders cluster lines only, and asking for zero edges should
	// cost zero queries. Unreachable from GET /api/graph/overview — its parser
	// enforces edge_limit >= 1 (handler/overview.go) — so the wire envelope and
	// its golden test are untouched by this branch.
	if len(clusterIDs) == 0 || p.EdgeLimit <= 0 {
		return nil, false, nil
	}
	sql := overviewEdgesTopicSQL
	if legacy {
		sql = overviewEdgesLegacySQL
	}
	rows, err := pool.Query(ctx, sql,
		readScopes, clusterIDs, p.MinInterWeight, p.EdgeLimit+1) // +1 detects truncation
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []OverviewEdge
	for rows.Next() {
		var e OverviewEdge
		if err := rows.Scan(&e.A, &e.B, &e.ScopeA, &e.ScopeB, &e.LinkCount, &e.Weight); err != nil {
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
