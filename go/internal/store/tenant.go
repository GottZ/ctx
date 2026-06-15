package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoScopes is the fail-closed sentinel: a read path resolved an EMPTY scope
// set. It is an error, never silently "all scopes". Callers gate on it via
// RequireScopes (design/01 §5.4).
var ErrNoScopes = errors.New("store: no scopes resolved (fail-closed)")

// RequireScopes enforces fail-closed: an empty scope slice is an error, NOT
// "all scopes". It mirrors the guard that today lives only in rrf.Search
// (search.go:65-67) so the later fail-closed-hardening wave (T07) can spread it
// to SearchBlocks + the manage reads as a per-path second line of defence behind
// the auth choke point (design/01 §5.0/§5.4). T04 only DEFINES the helper; the
// wiring into the read call-sites is T07.
func RequireScopes(scopes []string) error {
	if len(scopes) == 0 {
		return ErrNoScopes
	}
	return nil
}

// TenantScopes returns every scope OWNED by the given tenant (Modell C: a lookup
// on context_tenant_scopes WHERE tenant_id = $1). This is the per-TENANT scope
// set — distinct from a single KEY's read_scopes (built in ctx_auth, migration
// 060): it is the data foundation ("which scopes belong to whom") the visibility
// axis consumes, plus the cross-tenant isolation guarantee that a tenant id NEVER
// resolves to a foreign scope (the PK on context_tenant_scopes.scope ⇒ one scope
// = one tenant, no multi-mapping). It does NOT change how visibility works today
// (design/01 §4.2): SearchBlocks/ctx_rrf already gate on `scope = ANY(...)`; this
// supplies the per-tenant list those paths consume.
//
// An unknown or empty tenant yields an empty slice (NOT an error) — callers MUST
// pass the result through RequireScopes (or rely on the auth choke point) so that
// empty fails CLOSED rather than collapsing to "all scopes" via the PG
// `scope = ANY('{}')` artefact (design/01 §5.0/§5.2). An empty tenantID short-
// circuits before the query (an empty string is not a valid UUID and would raise
// 22P02; semantically it is "no tenant" → no scopes). The result is ORDER BY
// scope for a deterministic, testable order (TenantScopes is a set; the wire-
// ordered read_scopes[0]=home invariant belongs to ctx_auth, not here).
func TenantScopes(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]string, error) {
	if tenantID == "" {
		return nil, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT scope FROM context_tenant_scopes WHERE tenant_id = $1::uuid ORDER BY scope`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: tenant scopes query: %w", err)
	}
	defer rows.Close()

	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: tenant scopes scan: %w", err)
		}
		scopes = append(scopes, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tenant scopes rows: %w", err)
	}
	return scopes, nil
}
