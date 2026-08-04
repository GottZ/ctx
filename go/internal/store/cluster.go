// Cluster read path of the Cluster-Topic-Map (Achse 03, design/03 §4.1). Wave
// C1 lands the batch membership read and nothing else — no consumer, no wire
// field, no behaviour change. The later waves (ego annotation C2, RRF boost C3,
// facet C6, route C7) all reach the membership through THIS function, so the
// scope conjunction has exactly one site to be got right at.

package store

import (
	"context"

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
