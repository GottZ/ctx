package handler

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// pickAdmits evaluates the classes of the backfill pick in Go, as its
// branches read them (store.PendingPickSQL): a Shared branch for every scope
// outside Dead, and the pairs.
func pickAdmits(cl store.PendingClasses, scope, sens string) bool {
	if !slices.Contains(cl.Dead, scope) && slices.Contains(cl.Shared, sens) {
		return true
	}
	for i := range cl.PairScopes {
		if cl.PairScopes[i] == scope && cl.PairLevels[i] == sens {
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
		if cl.Shared == nil || cl.Dead == nil || cl.PairScopes == nil || cl.PairLevels == nil {
			t.Fatalf("within %v: a nil slice in %+v", within, cl)
		}
		// A bound pick reads its scopes only: no Shared branch, and every
		// pair in one of them.
		if within != nil {
			if len(cl.Shared) != 0 {
				t.Errorf("within %v: Shared branches %v in a bound pick", within, cl.Shared)
			}
			for _, s := range cl.PairScopes {
				if !slices.Contains(within, s) {
					t.Errorf("within %v: a pair of scope %q", within, s)
				}
			}
		}
		check := scopes
		if within != nil {
			check = within
		}
		for _, scope := range check {
			for _, l := range backends.SensitivityLevels {
				_, err := p.Chain(backends.RoleEmbed, floor.Apply(l, scope), scope)
				if got, want := pickAdmits(cl, scope, string(l)), err == nil; got != want {
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
	if len(cl.Shared) != 0 || len(cl.PairScopes) != 0 || cl.Shared == nil || cl.Dead == nil || cl.PairScopes == nil || cl.PairLevels == nil {
		t.Errorf("empty pool: %+v", cl)
	}
}

// TestBackfillPickerNilReadScopes: a caller without read scopes is bound to
// nothing. nil read scopes must not become the unbound classes of every
// scope (fail-closed; before #239, scope = ANY(NULL) matched no row).
func TestBackfillPickerNilReadScopes(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		{ID: "g", Name: "shared-full", Scope: backends.GlobalScope, Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustFull},
	})
	pk := newBackfillPicker(nil, true, p.TrustCeilings(backends.RoleEmbed), nil, nil)
	if cl := pk.callerClasses; len(cl.Shared) != 0 || len(cl.PairScopes) != 0 {
		t.Errorf("caller classes of a caller without read scopes: %+v, want none", cl)
	}
}
