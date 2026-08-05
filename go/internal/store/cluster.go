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

	nodes, err := clusterVisibleSizes(ctx, pool, clusterIDs, readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: cluster annotation sizes: %w", err)
	}
	if len(nodes) > len(clusterIDs) {
		return nil, fmt.Errorf("store: cluster annotation returned %d clusters for %d probed ids", len(nodes), len(clusterIDs))
	}
	if len(nodes) == 0 {
		return empty, nil
	}
	// fillTopCategories is REUSED verbatim from the landkarte read path: one
	// definition of "the visible categories of a cluster", so ego annotation and
	// overview cannot drift into two answers to the same question (§5.6).
	if err := fillTopCategories(ctx, pool, nodes, readScopes); err != nil {
		return nil, fmt.Errorf("store: cluster annotation categories: %w", err)
	}

	out := make([]ClusterAnnotationEntry, len(nodes))
	for i, n := range nodes {
		out[i] = ClusterAnnotationEntry{
			ClusterID:     n.ClusterID,
			Size:          n.Size,
			TopCategories: n.TopCategories,
			ScopeMix:      n.ScopeMix,
			InResponse:    inResponse[n.ClusterID],
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

// clusterVisibleSizes runs clustersql.VisibleSizeQuery — THE shared definition
// of scope-pure cluster size (§5.6). It returns OverviewNode values so
// fillTopCategories consumes them unchanged; ReprID/ReprTitle stay empty, the
// ego annotation carries no representative on the wire.
func clusterVisibleSizes(ctx context.Context, pool *pgxpool.Pool, clusterIDs, readScopes []string) ([]OverviewNode, error) {
	rows, err := pool.Query(ctx, clustersql.VisibleSizeQuery, clusterIDs, readScopes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]OverviewNode, 0, len(clusterIDs))
	for rows.Next() {
		var n OverviewNode
		if err := rows.Scan(&n.ClusterID, &n.Size, &n.ScopeMix); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
