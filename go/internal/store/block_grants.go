package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GrantedBlockIDs resolves the set of block IDs row-level-granted TO a tenant
// (Strategy A, design/07 §4.1): one lookup per request, the result flows as a
// bound uuid[] into the visibility OR-arm. An empty/whitespace tenantID or no
// grants returns a NON-NIL empty slice, so the caller binds '{}'::uuid[] and the
// OR-arm `id = ANY('{}')` is a deterministic FALSE — a byte-identical no-op to
// the scope-only state. Index-backed by idx_block_grants_grantee (067).
//
// TENANT-DECISION(block-grant-resolution): Strategy A (resolved []string bound
// param) for the Go ID/abruf paths in T40a — Alternative B (correlated subquery
// `id IN (SELECT block_id FROM context_block_grants WHERE grantee_tenant=$T)`),
// umentscheidbar weil die VisibilityPredicate-Signatur einen uuid[]-Param traegt
// und der OR-Arm hinter dem Switch-Point gekapselt ist; ctx_rrf (T40b) ist HART
// auf A festgelegt. design/07 §4.1.
//
// TENANT-DECISION(block-grant-empty-scope): konservativ — RequireScopes bleibt aktiv
//   (leeres readScopes wird abgelehnt, auch mit Grants). Alternative: Grant-only-Sicht
//   (RequireScopesOrGrants relaxen) — design/07 §5.3.6 Default-Empfehlung; umentscheidbar
//   weil es ein einzelner Guard am Eingang ist. Gewaehlt: fail-closed + minimal, gueltige
//   Grantees tragen ihren home_scope ohnehin. design/07 §5.3.6 / G5.
func GrantedBlockIDs(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return []string{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT block_id::text FROM context_block_grants WHERE grantee_tenant = $1::uuid`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: granted block ids: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan granted block id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
