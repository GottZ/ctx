package handler

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// admits evaluates the pick's class conjunct in Go, as the SQL states it.
func (cl embedClasses) admits(scope, sens string) bool {
	if !slices.Contains(cl.except, scope) && slices.Contains(cl.levels, sens) {
		return true
	}
	for i := range cl.pairScopes {
		if cl.pairScopes[i] == scope && cl.pairLevels[i] == sens {
			return true
		}
	}
	return false
}

// TestEmbedServableClassesMatchChain holds the class conjunct of the backfill
// pick against the chain the loop resolves per block: for every scope and
// sensitivity of the fixture the conjunct admits the pair exactly when
// Chain(RoleEmbed, floor.Apply(sensitivity, scope), scope) returns a backend.
// The fixture has a shared no-credentials backend, two scopes with a
// full-trust backend of their own (one of them floored as well, one not, so
// that the backend alone is what makes the scope differ), one with a lower
// one, and floors above, at and below the shared ceiling; within runs
// unbound, narrowed, with a duplicate, and empty.
func TestEmbedServableClassesMatchChain(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		{ID: "g", Name: "shared-nocred", Scope: backends.GlobalScope, Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustNoCredentials},
		{ID: "a", Name: "alpha-full", Scope: "alpha", Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustFull},
		{ID: "b", Name: "beta-full", Scope: "beta", Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustFull},
		{ID: "c", Name: "gamma-public", Scope: "gamma", Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustPublic},
	})
	floor := config.ScopeFloor{
		"lifted":  backends.SensCredentials, // above the shared ceiling: nothing servable
		"alpha":   backends.SensPersonal,    // lifted, but its own backend takes everything
		"mild":    backends.SensInternal,    // lifted below the ceiling
		"_global": backends.SensPersonal,
	}
	scopes := []string{"private", "shared", "alpha", "beta", "gamma", "lifted", "mild", "_global", "nobody"}
	c := p.TrustCeilings(backends.RoleEmbed)
	for _, within := range [][]string{nil, {"private", "lifted"}, {"alpha", "alpha", "mild"}, {"beta", "private"}, {}} {
		cl := embedServableClasses(c, floor, within)
		if cl.except == nil || cl.levels == nil || cl.pairScopes == nil || cl.pairLevels == nil {
			t.Fatalf("within %v: a nil slice in %+v", within, cl)
		}
		check := scopes
		if within != nil {
			check = within
		}
		for _, scope := range check {
			for _, l := range backends.SensitivityLevels {
				_, err := p.Chain(backends.RoleEmbed, floor.Apply(l, scope), scope)
				if got, want := cl.admits(scope, string(l)), err == nil; got != want {
					t.Errorf("within %v scope %q level %s: conjunct admits %v, Chain resolves %v", within, scope, l, got, want)
				}
			}
		}
		t.Logf("within %v: %+v", within, cl)
	}
	// No embed backend at all, nil floor: nothing is servable, nothing is nil.
	empty := backends.NewPool(nil, nil)
	empty.SeedSnapshotForTest(nil)
	cl := embedServableClasses(empty.TrustCeilings(backends.RoleEmbed), nil, nil)
	if len(cl.levels) != 0 || cl.levels == nil || cl.except == nil || cl.pairScopes == nil || cl.pairLevels == nil {
		t.Errorf("empty pool: %+v", cl)
	}
}
