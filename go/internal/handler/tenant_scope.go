package handler

import (
	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// The two-write-worlds scope resolution for the settings/secrets API (03-W5
// §4.1a/§4.5). The handler NEVER reads a scope from the request body; it derives
// the target scope from the authenticated role + tenant. A tenant-admin can only
// ever address its OWN tenant namespace, an operator addresses _global — there is
// no path to a foreign tenant scope by construction (§5.1).

// mutationScope resolves the scope a MUTATION targets: a server-admin (operator)
// writes the server-wide _global config; a tenant-admin writes ONLY its own
// tenant namespace (its HomeScope, the scope string — NOT the tenant UUID,
// §11.1). A nil/invalid ar is fail-closed to "" (the store layer then rejects the
// write with "scope is required").
func mutationScope(ar *auth.AuthResult) string {
	if ar == nil {
		return ""
	}
	if ar.IsServerAdmin() {
		return store.GlobalScope
	}
	return ar.HomeScope
}

// readScopes resolves the EFFECTIVE-view scopes a GET renders over: the operator
// sees _global (the level it administers); a tenant-admin sees the effective
// tenant > _global pair. Exactly two scopes at most (never an N-array): the
// resolution stays O(rows for these scopes) at 1M×N. A tenant whose HomeScope is
// empty or reserved degrades to _global (defensive).
func readScopes(ar *auth.AuthResult) []string {
	if ar == nil || ar.IsServerAdmin() {
		return []string{store.GlobalScope}
	}
	if ar.HomeScope == "" || ar.HomeScope == store.GlobalScope {
		return []string{store.GlobalScope}
	}
	return []string{store.GlobalScope, ar.HomeScope}
}

// resolveScopes resolves the scopes a secret_ref EXISTENCE check must consult so
// the write gate (checkSecretRef) follows the resolver exactly (§4.5 / D2): an
// operator resolves at _global; a tenant-admin resolves at its own scope plus —
// ONLY with the tenant's allow_shared_secrets opt-in active — _global. Without
// the opt-in a tenant referencing a _global-only secret is rejected (strict
// isolation), matching tenantSecretResolver's gated fallback.
func resolveScopes(ar *auth.AuthResult, allowShared bool) []string {
	if ar == nil || ar.IsServerAdmin() {
		return []string{store.GlobalScope}
	}
	if ar.HomeScope == "" || ar.HomeScope == store.GlobalScope {
		return []string{store.GlobalScope}
	}
	scopes := []string{ar.HomeScope}
	if allowShared {
		scopes = append(scopes, store.GlobalScope)
	}
	return scopes
}

// appliedFromDB reports whether a settings source means "the override took effect
// from the DB layer" — either the operator's _global ("settings") or a tenant's
// own scope ("tenant"). The PUT candidate build accepts BOTH: a tenant write wins
// the precedence merge as SourceTenant, not SourceSettings, so a tenant-only
// check would 422 a valid tenant override.
func appliedFromDB(source string) bool {
	return source == config.SourceSettings || source == config.SourceTenant
}
