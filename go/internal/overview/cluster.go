// Package overview builds the F5-W6 graph "landkarte": a precomputed Louvain
// cluster supergraph over context_dream_links. The heavy compute (gonum,
// in-memory, single-threaded) runs offline in a scheduler goroutine and writes
// scope-PARTITIONED aggregate tables (migration 057); the read path
// (store.GraphOverview, F5-W6-W2) sums only the rows of the caller's readScopes.
// See design 07-graph-overview.md.
package overview

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

const (
	// louvainSeed1/2 are HARD-WIRED so Modularize is reproducible across runs
	// (math/rand/v2 PCG source). Determinism hinges on TWO axes: this fixed
	// seed AND a reproducible node/edge insertion order (loadNodes/loadEdges
	// use ORDER BY). Drift on either silently changes the partition — guarded
	// by the modularity Q-score smoke (graph_overview_meta) + the unit test.
	louvainSeed1 uint64 = 0x637478_6f766572  // "ctx over"
	louvainSeed2 uint64 = 0x6c6f757661696e21 // "louvain!"

	// overviewLockKey gates the rebuild tx with pg_try_advisory_xact_lock so two
	// ctxd instances (multi-tenant line) never overwrite the tables against each
	// other. First advisory lock in the repo.
	overviewLockKey int64 = 0x6f76727677 // "ovrvw"

	// memberBatch caps the per-INSERT unnest array size (1M+ members → batched,
	// not one giant bind parameter).
	memberBatch = 5000
)

// Stats is the result of one rebuild for logging/telemetry.
type Stats struct {
	Skipped      bool    // advisory lock held elsewhere → no-op
	NodeCount    int     // clustered blocks (members)
	ClusterCount int     // distinct communities
	EdgeRows     int     // rows written to graph_cluster_edge (scope-pair partitioned)
	Modularity   float64 // gonum Q-score of the partition
}

// rawEdge is a directed dream-link reduced to the fields the clustering needs.
type rawEdge struct {
	src, dst string
	weight   float64
}

// clustering is the pure (DB-free) output of the Louvain run.
type clustering struct {
	blockToCluster map[string]string // block uuid → cluster_id (= min member uuid)
	modularity     float64
	clusterCount   int
}

// Rebuild recomputes the cluster supergraph and replaces the 057 tables in one
// advisory-locked transaction. Never call from a request path — gonum loads the
// whole graph into RAM; this belongs in the scheduler (runOverviewRebuild).
func Rebuild(ctx context.Context, pool *pgxpool.Pool, resolution float64) (Stats, error) {
	if resolution <= 0 {
		resolution = 1.0
	}
	nodeUUIDs, err := loadNodes(ctx, pool)
	if err != nil {
		return Stats{}, fmt.Errorf("loading nodes: %w", err)
	}
	edges, err := loadEdges(ctx, pool)
	if err != nil {
		return Stats{}, fmt.Errorf("loading edges: %w", err)
	}
	cl := computeClustering(nodeUUIDs, edges, resolution)
	return persist(ctx, pool, cl, resolution)
}

// computeClustering is the pure core: build an undirected weighted graph from
// the visible node set + symmetrized links, run Louvain with the fixed seed,
// derive a content-stable cluster_id (min member uuid). DB-free → unit-testable.
func computeClustering(nodeUUIDs []string, edges []rawEdge, resolution float64) clustering {
	n := len(nodeUUIDs)
	if n == 0 {
		return clustering{blockToCluster: map[string]string{}}
	}

	idx := make(map[string]int64, n)
	for i, u := range nodeUUIDs {
		idx[u] = int64(i)
	}

	// Symmetrize: aggregate directed links into undirected edge weights. Skip
	// dangling endpoints (link to an archived/meta block not in the node set)
	// and self-loops (gonum simple graph forbids them).
	type pair struct{ a, b int64 }
	agg := make(map[pair]float64, len(edges))
	for _, e := range edges {
		ai, okA := idx[e.src]
		bi, okB := idx[e.dst]
		if !okA || !okB || ai == bi {
			continue
		}
		if ai > bi {
			ai, bi = bi, ai
		}
		agg[pair{ai, bi}] += e.weight
	}

	g := simple.NewWeightedUndirectedGraph(0, 0)
	for i := range nodeUUIDs {
		g.AddNode(simple.Node(int64(i))) // isolated nodes too → singleton clusters
	}
	// Deterministic edge insertion order (sorted keys) — map iteration is random
	// in Go; the sort makes the build reproducible independent of map layout.
	keys := make([]pair, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		return keys[i].b < keys[j].b
	})
	for _, k := range keys {
		w := agg[k]
		if w <= 0 {
			w = 1e-9 // Modularize panics on negative weight; 0 is meaningless
		}
		g.SetWeightedEdge(simple.WeightedEdge{F: simple.Node(k.a), T: simple.Node(k.b), W: w})
	}

	reduced := community.Modularize(g, resolution, rand.NewPCG(louvainSeed1, louvainSeed2))
	comms := reduced.Communities()

	b2c := make(map[string]string, n)
	for _, members := range comms {
		var minUUID string
		for _, node := range members {
			if u := nodeUUIDs[node.ID()]; minUUID == "" || u < minUUID {
				minUUID = u
			}
		}
		for _, node := range members {
			b2c[nodeUUIDs[node.ID()]] = minUUID
		}
	}

	q := community.Q(g, comms, resolution)
	if math.IsNaN(q) || math.IsInf(q, 0) {
		q = 0 // 0-edge graph → undefined Q; report 0 rather than NaN
	}
	return clustering{blockToCluster: b2c, modularity: q, clusterCount: len(comms)}
}

func loadNodes(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	// ORDER BY id = determinism axis 2: the int64 surrogate id is the load
	// position, so a stable order yields a stable partition under a fixed seed.
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM context_blocks
		 WHERE NOT is_archived AND block_role <> 'system-meta'
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func loadEdges(ctx context.Context, pool *pgxpool.Pool) ([]rawEdge, error) {
	// supersedes is a replacement relation, not a topical bond — excluded from
	// clustering (matches the ego traversal, which never walks supersedes).
	rows, err := pool.Query(ctx,
		`SELECT source_block_id::text, target_block_id::text, raw_confidence
		 FROM context_dream_links
		 WHERE relationship <> 'supersedes'
		 ORDER BY source_block_id, target_block_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rawEdge
	for rows.Next() {
		var e rawEdge
		if err := rows.Scan(&e.src, &e.dst, &e.weight); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// nodeAggSQL builds graph_cluster_node: two-level aggregation — inner per
// (cluster, scope, category) counts + best representative, outer rolls up to
// size + category_counts(jsonb) + the representative of the highest-quality
// category. All visibility filtering is the canonical block-set predicate
// (NOT is_archived AND block_role <> 'system-meta'); scope partitioning is the
// GROUP BY, so each row belongs to exactly one scope (design §3.3).
const nodeAggSQL = `
INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id, repr_title, repr_quality)
SELECT cluster_id, scope,
       sum(cat_cnt)::int,
       jsonb_object_agg(category, cat_cnt),
       (array_agg(repr_id    ORDER BY cat_max_q DESC, repr_id))[1],
       (array_agg(repr_title ORDER BY cat_max_q DESC, repr_id))[1],
       max(cat_max_q)
FROM (
    SELECT m.cluster_id, b.scope, b.category,
           count(*)::int        AS cat_cnt,
           max(b.quality_score) AS cat_max_q,
           (array_agg(b.id              ORDER BY b.quality_score DESC, b.id))[1] AS repr_id,
           (array_agg(left(b.title,120) ORDER BY b.quality_score DESC, b.id))[1] AS repr_title
    FROM graph_cluster_member m
    JOIN context_blocks b ON b.id = m.block_id
       AND NOT b.is_archived AND b.block_role <> 'system-meta'
    GROUP BY m.cluster_id, b.scope, b.category
) per_cat
GROUP BY cluster_id, scope`

// edgeAggSQL builds graph_cluster_edge: inter-cluster links aggregated per
// (cluster-pair, scope-pair). cluster_a < cluster_b normalizes the undirected
// meta-edge; scope_s/scope_t are the source/target BLOCK scopes (a meta-edge is
// visible iff both are in readScopes — design §2). raw_confidence (CHECK >= 0)
// is the weight, never the unconstrained confidence column.
const edgeAggSQL = `
INSERT INTO graph_cluster_edge (cluster_a, cluster_b, scope_s, scope_t, link_count, weight_sum)
SELECT LEAST(ms.cluster_id, mt.cluster_id),
       GREATEST(ms.cluster_id, mt.cluster_id),
       bs.scope, bt.scope,
       count(*)::int, sum(l.raw_confidence)::real
FROM context_dream_links l
JOIN graph_cluster_member ms ON ms.block_id = l.source_block_id
JOIN graph_cluster_member mt ON mt.block_id = l.target_block_id
JOIN context_blocks bs ON bs.id = l.source_block_id AND NOT bs.is_archived AND bs.block_role <> 'system-meta'
JOIN context_blocks bt ON bt.id = l.target_block_id AND NOT bt.is_archived AND bt.block_role <> 'system-meta'
WHERE l.relationship <> 'supersedes' AND ms.cluster_id <> mt.cluster_id
GROUP BY 1, 2, bs.scope, bt.scope`

const metaUpsertSQL = `
INSERT INTO graph_overview_meta (singleton, computed_at, modularity, cluster_n, node_n, edge_n, resolution)
VALUES (true, now(), $1, $2, $3, $4, $5)
ON CONFLICT (singleton) DO UPDATE SET
    computed_at = EXCLUDED.computed_at, modularity = EXCLUDED.modularity,
    cluster_n = EXCLUDED.cluster_n, node_n = EXCLUDED.node_n,
    edge_n = EXCLUDED.edge_n, resolution = EXCLUDED.resolution`

// persist replaces the three 057 tables in one advisory-locked transaction.
func persist(ctx context.Context, pool *pgxpool.Pool, cl clustering, resolution float64) (Stats, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Stats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, overviewLockKey).Scan(&locked); err != nil {
		return Stats{}, fmt.Errorf("advisory lock: %w", err)
	}
	if !locked {
		slog.Info("overview: rebuild skipped — advisory lock held by another instance")
		return Stats{Skipped: true}, nil
	}

	if _, err := tx.Exec(ctx, `TRUNCATE graph_cluster_member, graph_cluster_node, graph_cluster_edge`); err != nil {
		return Stats{}, fmt.Errorf("truncate: %w", err)
	}

	// Batched member insert (deterministic order; unnest text[]→uuid[] cast).
	blocks := make([]string, 0, len(cl.blockToCluster))
	for b := range cl.blockToCluster {
		blocks = append(blocks, b)
	}
	sort.Strings(blocks)
	clusters := make([]string, len(blocks))
	clusterSet := make(map[string]struct{})
	for i, b := range blocks {
		clusters[i] = cl.blockToCluster[b]
		clusterSet[cl.blockToCluster[b]] = struct{}{}
	}
	for i := 0; i < len(blocks); i += memberBatch {
		end := min(i+memberBatch, len(blocks))
		if _, err := tx.Exec(ctx,
			`INSERT INTO graph_cluster_member (block_id, cluster_id)
			 SELECT b::uuid, c::uuid FROM unnest($1::text[], $2::text[]) AS t(b, c)`,
			blocks[i:end], clusters[i:end]); err != nil {
			return Stats{}, fmt.Errorf("insert members: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, nodeAggSQL); err != nil {
		return Stats{}, fmt.Errorf("node aggregation: %w", err)
	}
	edgeTag, err := tx.Exec(ctx, edgeAggSQL)
	if err != nil {
		return Stats{}, fmt.Errorf("edge aggregation: %w", err)
	}

	stats := Stats{
		NodeCount:    len(cl.blockToCluster),
		ClusterCount: len(clusterSet),
		EdgeRows:     int(edgeTag.RowsAffected()),
		Modularity:   cl.modularity,
	}

	if _, err := tx.Exec(ctx, metaUpsertSQL,
		stats.Modularity, stats.ClusterCount, stats.NodeCount, stats.EdgeRows, resolution); err != nil {
		return Stats{}, fmt.Errorf("meta upsert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Stats{}, fmt.Errorf("commit: %w", err)
	}
	return stats, nil
}
