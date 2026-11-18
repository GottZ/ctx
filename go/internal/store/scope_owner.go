package store

import (
	"context"
	"fmt"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ScopeOwnerConfigScopes maps each of scopes that a tenant owns
// (context_tenant_scopes, one scope = one tenant) to the scope that tenant's
// configuration resolves under in the background pipeline: the _global base
// for the default tenant, and for any other tenant its first owned scope in
// scope order. That is the rule of events.backgroundTenantFor (the scheduler's
// owned[0] of TenantScopes, ORDER BY scope; min() reads the same collation);
// a change to one has to follow in the other. A scope no tenant owns is
// absent from the map: its blocks resolve under _global, as the scheduler's
// unbounded legacy pass does.
//
// The caller is the query path's embed backfill (issue #253), which applies
// the sensitivity floor of the tenant that owns a block, not only the one of
// the tenant whose request triggered the embed. An empty scopes list reads
// nothing.
func ScopeOwnerConfigScopes(ctx context.Context, db pgxdb.Querier, scopes []string) (map[string]string, error) {
	out := make(map[string]string, len(scopes))
	if len(scopes) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx,
		`SELECT ts.scope,
		        CASE WHEN ts.tenant_id = $2::uuid THEN $3
		             ELSE (SELECT min(o.scope) FROM context_tenant_scopes o WHERE o.tenant_id = ts.tenant_id)
		        END
		   FROM context_tenant_scopes ts
		  WHERE ts.scope = ANY($1::text[])`,
		scopes, DefaultTenantID, GlobalScope)
	if err != nil {
		return nil, fmt.Errorf("store: scope owner config scopes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, configScope string
		if err := rows.Scan(&scope, &configScope); err != nil {
			return nil, fmt.Errorf("store: scope owner config scopes scan: %w", err)
		}
		out[scope] = configScope
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scope owner config scopes rows: %w", err)
	}
	return out, nil
}
