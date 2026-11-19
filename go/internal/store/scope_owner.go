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
// a change to one has to follow in the other, and
// events.TestBackgroundTenantRuleIsTheScopeOwnerRule holds the two against
// each other for every scope of every tenant. A scope no tenant owns is
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

// ConfigScopeOwnedScopes maps each of configScopes that is the configuration
// scope of a tenant other than the default one (its first owned scope in
// scope order, the rule of ScopeOwnerConfigScopes) to every scope that tenant
// owns. A scope that is no tenant's configuration scope is absent: the
// default tenant's configuration is _global, and a settings row at any other
// scope of a tenant is not the configuration the background resolves for it.
// The result is the size of the owners' scope lists (each capped by its
// tenant's max_scopes), not of what the configurations name. An empty
// configScopes list reads nothing.
func ConfigScopeOwnedScopes(ctx context.Context, db pgxdb.Querier, configScopes []string) (map[string][]string, error) {
	out := make(map[string][]string, len(configScopes))
	if len(configScopes) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx,
		`SELECT c.scope, o.scope
		   FROM context_tenant_scopes c
		   JOIN context_tenant_scopes o ON o.tenant_id = c.tenant_id
		  WHERE c.scope = ANY($1::text[])
		    AND c.tenant_id <> $2::uuid
		    AND c.scope = (SELECT min(m.scope) FROM context_tenant_scopes m WHERE m.tenant_id = c.tenant_id)
		  ORDER BY c.scope, o.scope`,
		configScopes, DefaultTenantID)
	if err != nil {
		return nil, fmt.Errorf("store: config scope owned scopes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var configScope, owned string
		if err := rows.Scan(&configScope, &owned); err != nil {
			return nil, fmt.Errorf("store: config scope owned scopes scan: %w", err)
		}
		out[configScope] = append(out[configScope], owned)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: config scope owned scopes rows: %w", err)
	}
	return out, nil
}

// SettingScopesWithRow reports which of scopes hold a context_settings row
// of their own on key (unique index on key, scope). An empty scopes list
// reads nothing.
func SettingScopesWithRow(ctx context.Context, db pgxdb.Querier, key string, scopes []string) (map[string]bool, error) {
	out := make(map[string]bool, len(scopes))
	if len(scopes) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx,
		`SELECT scope FROM context_settings WHERE key = $1 AND scope = ANY($2::text[])`, key, scopes)
	if err != nil {
		return nil, fmt.Errorf("store: setting scopes with row: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: setting scopes with row scan: %w", err)
		}
		out[s] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: setting scopes with row rows: %w", err)
	}
	return out, nil
}
