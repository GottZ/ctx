// Cluster read path of the Cluster-Topic-Map (Achse 03, design/03 §4.1). Wave
// C1 lands the batch membership read and nothing else — no consumer, no wire
// field, no behaviour change. The later waves (ego annotation C2, RRF boost C3,
// facet C6, route C7) all reach the membership through THIS function, so the
// scope conjunction has exactly one site to be got right at.

package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/clustersql"
)

// ClusterMembership maps block id → cluster id for the blocks the caller may
// see, scope-pure. Blocks without a VISIBLE membership row are simply absent
// from the result — not clustered, grant-only, or created after the last
// rebuild are indistinguishable to the caller, deliberately: distinguishing
// them would be the existence oracle the axis is built to avoid.
//
// RequireScopes is the FIRST statement (T07 fail-closed, pattern
// store/overview.go GraphOverview). Skipping it would not fail loudly: PostgreSQL
// evaluates `scope = ANY('{}')` as a deterministic FALSE, so an unresolved scope
// set would come back as an EMPTY MAP — visually identical to "nothing found",
// and a resolver bug would hide as a quiet loss of signal.
//
// An empty block set short-circuits WITHOUT a roundtrip, but only AFTER the
// scope check: a fail-closed guard that a caller can skip by passing no ids is
// not a guard.
func ClusterMembership(ctx context.Context, pool *pgxpool.Pool, blockIDs, readScopes []string) (map[string]string, error) {
	if err := RequireScopes(readScopes); err != nil {
		return nil, err
	}
	if len(blockIDs) == 0 {
		return map[string]string{}, nil
	}

	rows, err := pool.Query(ctx, clustersql.MembershipQuery, blockIDs, readScopes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string, len(blockIDs))
	for rows.Next() {
		var blockID, clusterID string
		if err := rows.Scan(&blockID, &clusterID); err != nil {
			return nil, err
		}
		out[blockID] = clusterID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClusterAnnotationEntry is one cluster the delivered ego nodes sit in, already
// scope-pure aggregated. ClusterID stays store-internal — the handler emits a
// per-request ordinal instead (design/03 §5.1: cluster_id is the smallest member
// UUID and context_blocks.id is uuidv7, so a raw cluster_id would tell a caller
// that an invisible block exists AND roughly when it was created).
type ClusterAnnotationEntry struct {
	ClusterID     string
	Size          int      // Σ size over the caller's visible partitions
	TopCategories []string // merged category_counts of those partitions, top 3
	ScopeMix      []string // the visible partitions of this cluster ⊆ readScopes
	InResponse    int      // how many of the passed blocks sit in this cluster
	// Topics are the STABLE handles of this cluster's visible partitions (C5),
	// ordered by partition size desc — element 0 is the primary. A handle is
	// scope-bound by construction (Masterplan K2 / A03-2: "ein Handle = ein
	// scope-reines Thema"), while this entry is keyed by CLUSTER, so a cluster
	// spanning two visible scopes carries two handles here and the caller learns
	// which one names the bigger half without a second query.
	//
	// EMPTY is a normal state, never an error: a partition the identity layer has
	// not reached yet (pre-W3, or the mid-rollout window) simply has no handle,
	// and the entry keeps its size and categories. Unlike cluster_id these values
	// are emittable — gen_random_uuid v4, no block reference, no timestamp
	// component (§5.1).
	Topics []string
	// Label is the PRIMARY partition's label, "" when unlabelled or unidentified.
	// It accompanies the map's other captions rather than replacing anything
	// (E6-01); the label PROVENANCE (label_source/label_model) deliberately stays
	// off this path and lives on the C7 detail route only (E4-02).
	Label string
}

// ClusterAnnotationResult is the scope-pure cluster projection of a node set.
// Clusters is ordered InResponse desc, ClusterID asc — deterministic, and the
// handler's ordinal assignment inherits that order.
//
// MemberOf may name clusters that are ABSENT from Clusters: a membership row
// whose aggregate partition is not visible (or not yet rebuilt) has no size to
// report. The handler resolves such a block to -1, which is why a dangling
// ordinal is structurally impossible here rather than merely tested for.
type ClusterAnnotationResult struct {
	Clusters []ClusterAnnotationEntry
	MemberOf map[string]string // block id → cluster id (scope-pure, C1)
}

// ClusterAnnotation is the scope-pure cluster projection for a node set
// (design/03 §4.2, wave C2). Two reads: the C1 membership probe, then the
// visible-size aggregation restricted to the clusters that probe actually hit.
//
// RequireScopes runs first — inside ClusterMembership, whose contract is that
// the scope check precedes even the empty-input short circuit.
//
// TRUNCATION IS AN ERROR HERE, NOT A FLAG (design/03 §4.2, Linse 2 / B10). The
// landkarte's aggregate runs with min_cluster_size=1 and node_limit=500 and
// reports truncation to a human. This path hands ordinals to a POSITIONAL array:
// a dropped entry would leave cluster_of[i] pointing at nothing, and no client
// could resolve it. The size query therefore carries no HAVING and no LIMIT, and
// getting back more rows than clusters were asked for is a wiring bug we surface
// loudly instead of trimming.
func ClusterAnnotation(ctx context.Context, pool *pgxpool.Pool, blockIDs, readScopes []string) (*ClusterAnnotationResult, error) {
	memberOf, err := ClusterMembership(ctx, pool, blockIDs, readScopes)
	if err != nil {
		return nil, err
	}
	empty := &ClusterAnnotationResult{Clusters: []ClusterAnnotationEntry{}, MemberOf: memberOf}
	if len(memberOf) == 0 {
		return empty, nil
	}

	inResponse := make(map[string]int, len(memberOf))
	clusterIDs := make([]string, 0, len(memberOf))
	for _, cid := range memberOf {
		if inResponse[cid] == 0 {
			clusterIDs = append(clusterIDs, cid)
		}
		inResponse[cid]++
	}
	sort.Strings(clusterIDs) // deterministic parameter order, deterministic plan

	aggs, err := clusterVisibleSizes(ctx, pool, clusterIDs, readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: cluster annotation sizes: %w", err)
	}
	if len(aggs) > len(clusterIDs) {
		return nil, fmt.Errorf("store: cluster annotation returned %d clusters for %d probed ids", len(aggs), len(clusterIDs))
	}
	if len(aggs) == 0 {
		return empty, nil
	}
	// fillTopCategories works on OverviewNode values and keys them by cluster
	// (byCluster=true), so the aggregate rows are projected onto that shape for
	// the one call instead of teaching the shared filler a second row type.
	nodes := make([]OverviewNode, len(aggs))
	for i := range aggs {
		nodes[i] = OverviewNode{ClusterID: aggs[i].ClusterID, Size: aggs[i].Size, ScopeMix: aggs[i].ScopeMix}
	}
	// fillTopCategories is REUSED verbatim from the landkarte read path: one
	// definition of "the visible categories of a cluster", so ego annotation and
	// overview cannot drift into two answers to the same question (§5.6).
	// byCluster=true: the ego annotation reports per CLUSTER (its wire carries
	// cluster ordinals, not topics), so the per-scope partials are folded the
	// way they always were.
	if err := fillTopCategories(ctx, pool, nodes, readScopes, true); err != nil {
		return nil, fmt.Errorf("store: cluster annotation categories: %w", err)
	}

	out := make([]ClusterAnnotationEntry, len(aggs))
	for i, a := range aggs {
		out[i] = ClusterAnnotationEntry{
			ClusterID:     a.ClusterID,
			Size:          a.Size,
			TopCategories: nodes[i].TopCategories,
			ScopeMix:      a.ScopeMix,
			InResponse:    inResponse[a.ClusterID],
			Topics:        a.Topics,
			Label:         a.Label,
		}
	}
	// Order = descending hit count in the delivered node set, tiebreak cluster_id
	// asc (§4.2). Slice, not SliceStable — the tiebreak is total.
	sort.Slice(out, func(i, j int) bool {
		if out[i].InResponse != out[j].InResponse {
			return out[i].InResponse > out[j].InResponse
		}
		return out[i].ClusterID < out[j].ClusterID
	})
	return &ClusterAnnotationResult{Clusters: out, MemberOf: memberOf}, nil
}

// clusterVisibleAgg is one row of clustersql.VisibleSizeQuery: a CLUSTER with
// its scope-pure size, its visible partitions and (C5) their stable handles.
// Deliberately its own type rather than OverviewNode: an OverviewNode is one
// MAP node — on the identity path exactly one (cluster, scope) partition with
// exactly one topic — while this row aggregates over partitions and can carry
// several handles. Reusing the map type here would have made "TopicID" mean two
// different things in one package.
type clusterVisibleAgg struct {
	ClusterID string
	Size      int
	ScopeMix  []string
	Topics    []string
	Label     string
}

// clusterVisibleSizes runs clustersql.VisibleSizeQuery — THE shared definition
// of scope-pure cluster size (§5.6) and, since C5, of the handles that name its
// visible partitions.
func clusterVisibleSizes(ctx context.Context, pool *pgxpool.Pool, clusterIDs, readScopes []string) ([]clusterVisibleAgg, error) {
	rows, err := pool.Query(ctx, clustersql.VisibleSizeQuery, clusterIDs, readScopes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]clusterVisibleAgg, 0, len(clusterIDs))
	for rows.Next() {
		var a clusterVisibleAgg
		if err := rows.Scan(&a.ClusterID, &a.Size, &a.ScopeMix, &a.Topics, &a.Label); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
