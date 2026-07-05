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
	"hash/fnv"
	"math/rand/v2"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"

	"github.com/GottZ/ctx/internal/visibility"
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
	// other. First advisory lock in the repo. Since B-W4 this is the BASE key:
	// a global run (nil ScopeFilter) locks exactly this value, a partition run
	// XORs a process-stable hash of its scope set on top (lockKeyForScopes).
	overviewLockKey int64 = 0x6f76727677 // "ovrvw"

	// memberBatch caps the per-INSERT unnest array size (1M+ members → batched,
	// not one giant bind parameter).
	memberBatch = 5000
)

// Stats is the result of one rebuild for logging/telemetry.
type Stats struct {
	Skipped      bool    // no-op run — SkipReason says why
	SkipReason   string  // "advisory-lock" | "node-cap" (empty when not skipped)
	NodeCount    int     // clustered blocks (members)
	ClusterCount int     // distinct communities
	EdgeRows     int     // rows written to graph_cluster_edge (scope-pair partitioned)
	Modularity   float64 // gonum Q-score of the partition
}

// Options bundles the rebuild inputs (B-W1). The per-tenant scope filter
// (B-W6) lands here so call sites change once.
type Options struct {
	// Resolution is the Louvain resolution parameter (<=0 → 1.0).
	Resolution float64
	// VisibleTypes is the resolved retrieval allowlist (T6); feeds the node cut
	// and the aggregation re-checks. Empty = wiring bug, fails loudly.
	VisibleTypes []string
	// OverviewTypes is the overview.include allowlist (T6); node cut only.
	// Empty = wiring bug, fails loudly.
	OverviewTypes []string
	// MaxNodes is the load-bearing liveness guard (B2-C1): Louvain is one
	// opaque gonum call that cannot be interrupted, and past ~200k nodes it
	// hits a convergence wall (bench 019ec56f: 400k=465s, 1M never converges).
	// A node set larger than this cap skips the rebuild fail-safe (logged,
	// Stats.SkipReason="node-cap"). 0 = uncapped.
	MaxNodes int
	// ScopeFilter is the per-tenant partition filter (B-W3, decision B-E3).
	// Non-nil: teardown AND aggregation are scoped to exactly these scopes,
	// ATOMICALLY — never one without the other (B1-C1: a scoped DELETE with an
	// unscoped re-aggregation resurrects foreign-partition rows and hits the
	// (cluster_id, scope) PK ⇒ 23505 from tenant #2 on). nil: global run,
	// behaviour-identical to the pre-B full replace (TRUNCATE + unscoped
	// aggregation). INPUT scoping (loadNodes/loadEdges) lands in B-W6 — until
	// then a non-nil filter requires an input already cut to these scopes;
	// persist fails loudly on any input block outside the filter.
	ScopeFilter []string
}

// lockKeyForScopes returns the advisory lock key for one persist run (B-W4).
//
// Empty/nil filter (the global run) returns overviewLockKey BYTE-IDENTICALLY:
// during a mixed-version deploy or rollback window an old binary (which only
// knows the base key) and the new one keep serializing against each other.
//
// A partition run hashes its scope set with FNV-64a — seedless and therefore
// process-stable — and XORs it onto the base key. hash/maphash is NOT usable
// here (B1-M1): its seed is per-process, so two ctxd instances on the same DB
// would compute DIFFERENT keys for the same tenant and stop excluding each
// other. The input is sorted and deduplicated (a tenant's owned set is a SET —
// order or duplicates must never change the key), scopes joined with a 0x00
// separator (scope values are VARCHAR/TEXT and cannot contain NUL, so the
// framing is unambiguous).
//
// Collision note (B2-m2(i), deliberately unhandled): two distinct scope sets
// colliding in the 64-bit space merely serialize their rebuilds — the losing
// run skips and retries next tick; a delayed rebuild, never a correctness
// problem.
func lockKeyForScopes(scopeFilter []string) int64 {
	if len(scopeFilter) == 0 {
		return overviewLockKey
	}
	set := make([]string, 0, len(scopeFilter))
	seen := make(map[string]struct{}, len(scopeFilter))
	for _, s := range scopeFilter {
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			set = append(set, s)
		}
	}
	sort.Strings(set)
	h := fnv.New64a()
	for _, s := range set {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return overviewLockKey ^ int64(h.Sum64()) //nolint:gosec // deliberate wrap-around: the lock key is an opaque 64-bit identity, not a quantity
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
//
// Type policy (WF T6, design/01 §6.8/§7-T6): visibleTypes is the resolved
// retrieval allowlist (blocktype.Set.VisibleTypes — the registry successor of
// the former `type_name <> 'system-meta'` literal); overviewTypes is the
// overview.include allowlist (Set.OverviewTypes — the digest.include pendant
// against issue flooding). The NODE set is cut by the INTERSECTION of both
// (a type must be retrieval-visible AND overview-included to become a Louvain
// node); the aggregation re-checks use visibleTypes alone (pure visibility
// semantics — members are already the cut set). Both lists come from the BASE
// snapshot: the rebuild is ONE global run TODAY; the per-tenant rebuild loop
// (B-W6) threads a scope filter through Options (§9.6 caveat, T12 boundary —
// under active migration on the B line). Empty lists are a wiring bug and
// fail loudly (rrf.Search pattern).
//
// Liveness (B-W1): opts.MaxNodes is the load-bearing guard (fail-safe skip);
// ctx cancellation/timeout is the secondary guard — it aborts BETWEEN the
// load/compute/persist phases and via clusterWithCtx during compute (with the
// documented goroutine leak: a running Modularize cannot be interrupted).
func Rebuild(ctx context.Context, pool *pgxpool.Pool, opts Options) (Stats, error) {
	if opts.Resolution <= 0 {
		opts.Resolution = 1.0
	}
	if len(opts.VisibleTypes) == 0 || len(opts.OverviewTypes) == 0 {
		return Stats{}, fmt.Errorf("overview: empty type allowlist (visible=%d, overview=%d) — block-type registry not wired?",
			len(opts.VisibleTypes), len(opts.OverviewTypes))
	}
	nodeUUIDs, nodeScopes, err := loadNodes(ctx, pool, intersect(opts.VisibleTypes, opts.OverviewTypes))
	if err != nil {
		return Stats{}, fmt.Errorf("loading nodes: %w", err)
	}
	if opts.MaxNodes > 0 && len(nodeUUIDs) > opts.MaxNodes {
		slog.Warn("overview: rebuild skipped — node count exceeds cap (Louvain wall)",
			"nodes", len(nodeUUIDs), "max_nodes", opts.MaxNodes)
		return Stats{Skipped: true, SkipReason: "node-cap"}, nil
	}
	edges, err := loadEdges(ctx, pool)
	if err != nil {
		return Stats{}, fmt.Errorf("loading edges: %w", err)
	}
	cl, err := clusterWithCtx(ctx, nodeUUIDs, edges, opts.Resolution)
	if err != nil {
		return Stats{}, err
	}
	return persist(ctx, pool, cl, opts, nodeScopes)
}

// clusterWithCtx runs computeClustering in its own goroutine so a ctx
// timeout/cancel lets the CALLER move on. Modularize is one opaque gonum call
// and cannot observe ctx — on abort the goroutine keeps computing until
// Louvain converges and is then discarded (documented leak; bounded by
// Options.MaxNodes, which keeps the input below the convergence wall). The
// buffered channel lets the orphaned goroutine exit instead of blocking
// forever.
func clusterWithCtx(ctx context.Context, nodeUUIDs []string, edges []rawEdge, resolution float64) (clustering, error) {
	done := make(chan clustering, 1)
	go func() {
		done <- computeClustering(nodeUUIDs, edges, resolution)
	}()
	select {
	case <-ctx.Done():
		return clustering{}, fmt.Errorf("clustering aborted (goroutine keeps running until convergence): %w", ctx.Err())
	case cl := <-done:
		return cl, nil
	}
}

// intersect returns the sorted intersection of two string slices. Used for
// the node-set cut (visible ∩ overview.include); n is registry-sized (tens),
// never corpus-sized.
func intersect(a, b []string) []string {
	in := make(map[string]struct{}, len(b))
	for _, s := range b {
		in[s] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if _, ok := in[s]; ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
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

func loadNodes(ctx context.Context, pool *pgxpool.Pool, nodeTypes []string) ([]string, map[string]string, error) {
	// ORDER BY id = determinism axis 2: the int64 surrogate id is the load
	// position, so a stable order yields a stable partition under a fixed seed.
	// nodeTypes = visible ∩ overview.include (T6): the shared type-visibility
	// fragment plus the overview sieve, folded into ONE bind parameter.
	// The scope rides along (B-W2): persist denormalizes it onto the member
	// row, so the written partition is the one the clustering RAN in — never
	// a re-read that a concurrent scope-move could skew.
	rows, err := pool.Query(ctx,
		fmt.Sprintf(`SELECT cb.id::text, cb.scope FROM context_blocks cb
		 WHERE %s
		 ORDER BY cb.id`, visibility.TypeVisible("cb", "$1")),
		nodeTypes)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []string
	scopes := make(map[string]string)
	for rows.Next() {
		var s, scope string
		if err := rows.Scan(&s, &scope); err != nil {
			return nil, nil, err
		}
		out = append(out, s)
		scopes[s] = scope
	}
	return out, scopes, rows.Err()
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
// category. All visibility filtering is the canonical type-visibility
// fragment (visibility.TypeVisible, $1 = registry allowlist — T6); scope
// partitioning is the GROUP BY, so each row belongs to exactly one scope
// (design §3.3).
// nodeAggTemplate has two slots: the type-visibility fragment ($1) and the
// B-W3 scope filter ("" = global run, or a WHERE on the DENORMALIZED
// m.scope ($2) — the member column exists exactly so the scoped aggregation
// never joins context_blocks for partition membership, 087 header).
const nodeAggTemplate = `
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
       AND %s
    %s
    GROUP BY m.cluster_id, b.scope, b.category
) per_cat
GROUP BY cluster_id, scope`

var nodeAggSQL = fmt.Sprintf(nodeAggTemplate, visibility.TypeVisible("b", "$1"), "")

// nodeAggScopedSQL is the B-W3 partition-scoped variant ($2 = scope filter).
// It MUST only ever run after the equally scoped teardown DELETE (persist
// keeps the two in one branch — B1-C1).
var nodeAggScopedSQL = fmt.Sprintf(nodeAggTemplate, visibility.TypeVisible("b", "$1"),
	"WHERE m.scope = ANY($2)")

// edgeAggSQL builds graph_cluster_edge: inter-cluster links aggregated per
// (cluster-pair, scope-pair). cluster_a < cluster_b normalizes the undirected
// meta-edge; scope_s/scope_t are the source/target BLOCK scopes (a meta-edge is
// visible iff both are in readScopes — design §2). raw_confidence (CHECK >= 0)
// is the weight, never the unconstrained confidence column. Both endpoint
// re-checks use the shared type-visibility fragment ($1 = registry allowlist).
// edgeAggTemplate: slots = two type-visibility fragments ($1) and the B-W3
// scope filter. The scoped variant restricts BOTH endpoint member scopes
// (AND, not OR): a partition's meta-edges are exactly those the tenant's
// owned-disjoint Louvain input can produce (both endpoints owned — matches
// the B-W6 loadEdges contract and the read path's "both scopes readable"
// rule, 057 header). Cross-partition mixed edges are deliberately NOT torn
// down by a scoped run — the B-W5/B-W6 transition owns their lifecycle.
const edgeAggTemplate = `
INSERT INTO graph_cluster_edge (cluster_a, cluster_b, scope_s, scope_t, link_count, weight_sum)
SELECT LEAST(ms.cluster_id, mt.cluster_id),
       GREATEST(ms.cluster_id, mt.cluster_id),
       bs.scope, bt.scope,
       count(*)::int, sum(l.raw_confidence)::real
FROM context_dream_links l
JOIN graph_cluster_member ms ON ms.block_id = l.source_block_id
JOIN graph_cluster_member mt ON mt.block_id = l.target_block_id
JOIN context_blocks bs ON bs.id = l.source_block_id AND %s
JOIN context_blocks bt ON bt.id = l.target_block_id AND %s
WHERE l.relationship <> 'supersedes' AND ms.cluster_id <> mt.cluster_id%s
GROUP BY 1, 2, bs.scope, bt.scope`

var edgeAggSQL = fmt.Sprintf(edgeAggTemplate,
	visibility.TypeVisible("bs", "$1"), visibility.TypeVisible("bt", "$1"), "")

// edgeAggScopedSQL is the B-W3 partition-scoped variant ($2 = scope filter,
// both endpoints). Only ever runs after the scoped teardown (B1-C1).
var edgeAggScopedSQL = fmt.Sprintf(edgeAggTemplate,
	visibility.TypeVisible("bs", "$1"), visibility.TypeVisible("bt", "$1"),
	"\n  AND ms.scope = ANY($2) AND mt.scope = ANY($2)")

const metaUpsertSQL = `
INSERT INTO graph_overview_meta (singleton, computed_at, modularity, cluster_n, node_n, edge_n, resolution)
VALUES (true, now(), $1, $2, $3, $4, $5)
ON CONFLICT (singleton) DO UPDATE SET
    computed_at = EXCLUDED.computed_at, modularity = EXCLUDED.modularity,
    cluster_n = EXCLUDED.cluster_n, node_n = EXCLUDED.node_n,
    edge_n = EXCLUDED.edge_n, resolution = EXCLUDED.resolution`

// teardown clears the 057 tables for one persist run: global = TRUNCATE (the
// pre-B path), scoped = per-partition DELETEs (B-W3) — the atomic partner of
// the equally scoped aggregation in persist; never ships without it (B1-C1).
// Members and nodes belong to exactly one scope; edges use AND on both
// endpoint scopes (partition semantics, see edgeAggTemplate).
func teardown(ctx context.Context, tx pgx.Tx, scoped bool, scopeFilter []string) error {
	if !scoped {
		if _, err := tx.Exec(ctx, `TRUNCATE graph_cluster_member, graph_cluster_node, graph_cluster_edge`); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
		return nil
	}
	for _, del := range []struct{ label, sql string }{
		{"member", `DELETE FROM graph_cluster_member WHERE scope = ANY($1)`},
		{"node", `DELETE FROM graph_cluster_node WHERE scope = ANY($1)`},
		{"edge", `DELETE FROM graph_cluster_edge WHERE scope_s = ANY($1) AND scope_t = ANY($1)`},
	} {
		if _, err := tx.Exec(ctx, del.sql, scopeFilter); err != nil {
			return fmt.Errorf("scoped %s teardown: %w", del.label, err)
		}
	}
	return nil
}

// persist replaces the 057 tables in one advisory-locked transaction —
// globally (nil ScopeFilter: TRUNCATE + unscoped aggregation, the pre-B
// behaviour) or for ONE partition (B-W3: scoped DELETE + scoped aggregation,
// ATOMICALLY paired — a scoped DELETE with the unscoped aggregation
// resurrects foreign-partition rows into the (cluster_id, scope) PK ⇒ 23505,
// the verified B1-C1 breakage). opts.VisibleTypes feeds the aggregation
// re-checks (T6, $1). nodeScopes is the Louvain-input scope per block
// (loadNodes) — denormalized onto the member row (B-W2, migration 087).
func persist(ctx context.Context, pool *pgxpool.Pool, cl clustering, opts Options, nodeScopes map[string]string) (Stats, error) {
	scoped := len(opts.ScopeFilter) > 0
	filterSet := make(map[string]struct{}, len(opts.ScopeFilter))
	for _, s := range opts.ScopeFilter {
		filterSet[s] = struct{}{}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Stats{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	// B-W4: the lock is per-partition — two different tenants persist in
	// parallel, the same tenant (and the global run against old binaries)
	// keeps serializing. See lockKeyForScopes.
	lockKey := lockKeyForScopes(opts.ScopeFilter)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lockKey).Scan(&locked); err != nil {
		return Stats{}, fmt.Errorf("advisory lock: %w", err)
	}
	if !locked {
		slog.Info("overview: rebuild skipped — advisory lock held by another instance",
			"lock_key", lockKey, "scope_filter", opts.ScopeFilter)
		return Stats{Skipped: true, SkipReason: "advisory-lock"}, nil
	}

	if err := teardown(ctx, tx, scoped, opts.ScopeFilter); err != nil {
		return Stats{}, err
	}

	// Batched member insert (deterministic order; unnest text[]→uuid[] cast).
	// scope is denormalized from the Louvain input (B-W2, 087) — the member
	// row records the partition the clustering RAN in. block_id stays the
	// SOLE PK: the overview input is strictly owned-disjoint (no grants), so
	// no block is a member under two scopes — load-bearing invariant, the
	// input-purity assert lands in B-W6 (087 header).
	blocks := make([]string, 0, len(cl.blockToCluster))
	for b := range cl.blockToCluster {
		blocks = append(blocks, b)
	}
	sort.Strings(blocks)
	clusters := make([]string, len(blocks))
	scopes := make([]string, len(blocks))
	clusterSet := make(map[string]struct{})
	for i, b := range blocks {
		clusters[i] = cl.blockToCluster[b]
		clusterSet[cl.blockToCluster[b]] = struct{}{}
		scope, ok := nodeScopes[b]
		if !ok || scope == "" {
			// Fail loud: a member without an input scope would either violate
			// NOT NULL or silently land in the wrong partition (B-W3 poison).
			return Stats{}, fmt.Errorf("insert members: block %s has no Louvain-input scope", b)
		}
		if scoped {
			// Input-purity guard (B-W3): a scoped run whose input carries a
			// block outside the filter would collide with the surviving
			// foreign partition (block_id PK) or poison it. Input scoping
			// itself (loadNodes/loadEdges) is B-W6 — until then callers must
			// hand persist an already-cut input; fail loud, never trim.
			if _, in := filterSet[scope]; !in {
				return Stats{}, fmt.Errorf("insert members: block %s scope %q outside ScopeFilter %v — input not partition-cut (B-W6 wires scoped loading)", b, scope, opts.ScopeFilter)
			}
		}
		scopes[i] = scope
	}
	for i := 0; i < len(blocks); i += memberBatch {
		end := min(i+memberBatch, len(blocks))
		if _, err := tx.Exec(ctx,
			`INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
			 SELECT b::uuid, c::uuid, s FROM unnest($1::text[], $2::text[], $3::text[]) AS t(b, c, s)`,
			blocks[i:end], clusters[i:end], scopes[i:end]); err != nil {
			return Stats{}, fmt.Errorf("insert members: %w", err)
		}
	}

	// Aggregation — ALWAYS the variant matching the teardown above (B1-C1:
	// scoped DELETE + unscoped aggregation = resurrected foreign rows ⇒ 23505).
	var edgeTag pgconn.CommandTag
	if !scoped {
		if _, err := tx.Exec(ctx, nodeAggSQL, opts.VisibleTypes); err != nil {
			return Stats{}, fmt.Errorf("node aggregation: %w", err)
		}
		edgeTag, err = tx.Exec(ctx, edgeAggSQL, opts.VisibleTypes)
		if err != nil {
			return Stats{}, fmt.Errorf("edge aggregation: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, nodeAggScopedSQL, opts.VisibleTypes, opts.ScopeFilter); err != nil {
			return Stats{}, fmt.Errorf("scoped node aggregation: %w", err)
		}
		edgeTag, err = tx.Exec(ctx, edgeAggScopedSQL, opts.VisibleTypes, opts.ScopeFilter)
		if err != nil {
			return Stats{}, fmt.Errorf("scoped edge aggregation: %w", err)
		}
	}

	stats := Stats{
		NodeCount:    len(cl.blockToCluster),
		ClusterCount: len(clusterSet),
		EdgeRows:     int(edgeTag.RowsAffected()),
		Modularity:   cl.modularity,
	}

	// Meta stays the 057 singleton until B-W5 (scope-PK migration 088): a
	// scoped run must not overwrite the global stats row with one partition's
	// numbers — B-W5 replaces this with per-scope rows in the same tx.
	if !scoped {
		if _, err := tx.Exec(ctx, metaUpsertSQL,
			stats.Modularity, stats.ClusterCount, stats.NodeCount, stats.EdgeRows, opts.Resolution); err != nil {
			return Stats{}, fmt.Errorf("meta upsert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Stats{}, fmt.Errorf("commit: %w", err)
	}
	return stats, nil
}
