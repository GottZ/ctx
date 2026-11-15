package events

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
)

// TestBackfillClassesMatchChain holds the classes of the background arm's
// pick against the chain backfillStep resolves per block: for every scope
// and sensitivity the classes admit the pair exactly when
// router.EmbedChain(router.FloorSens(sensitivity, scope)) returns a backend.
// The pools cover a shared no-credentials embed backend, a tenant's own
// full-trust one, a dream-embed row that takes over the role for its tenant,
// and no backend at all; the floors lie above, at and below the ceilings.
func TestBackfillClassesMatchChain(t *testing.T) {
	floor := config.ScopeFloor{
		"lifted": backends.SensCredentials,
		"mild":   backends.SensInternal,
		"t-own":  backends.SensPersonal,
	}
	cfg := &config.Config{}
	cfg.Pool.ScopeSensitivityFloor = floor
	embedRow := func(id, scope, role string, trust backends.Trust) backends.Backend {
		return backends.Backend{ID: id, Name: id, Scope: scope, Roles: []string{role}, Enabled: true, Trust: trust}
	}
	pools := map[string][]backends.Backend{
		"shared_nocred_and_own_full": {
			embedRow("g", backends.GlobalScope, backends.RoleEmbed, backends.TrustNoCredentials),
			embedRow("o", "t-own", backends.RoleEmbed, backends.TrustFull),
		},
		"dream_embed_takes_the_role": {
			embedRow("g", backends.GlobalScope, backends.RoleEmbed, backends.TrustFull),
			embedRow("d", backends.GlobalScope, backends.RoleDreamEmbed, backends.TrustPublic),
		},
		"no_embed_backend": nil,
	}
	scopes := []string{"t-own", "shared", "lifted", "mild", "other"}
	for name, rows := range pools {
		p := backends.NewPool(nil, nil)
		p.SeedSnapshotForTest(rows)
		for _, tenant := range []string{"t-own", store.GlobalScope} {
			router := &dream.Router{Pool: p, Tenant: tenant, Floor: floor.Apply}
			for _, owned := range [][]string{nil, {"t-own", "lifted"}, {"shared", "mild", "shared"}} {
				cl := backfillClasses(router, cfg, owned, router.EmbedCeiling())
				if cl.Shared == nil || cl.Dead == nil || cl.PairScopes == nil || cl.PairLevels == nil {
					t.Fatalf("%s/%s/%v: a nil slice in %+v", name, tenant, owned, cl)
				}
				check := scopes
				if owned != nil {
					check = owned
					if len(cl.Shared) != 0 {
						t.Errorf("%s/%s/%v: Shared branches %v in a bound pick", name, tenant, owned, cl.Shared)
					}
					for _, s := range cl.PairScopes {
						if !slices.Contains(owned, s) {
							t.Errorf("%s/%s/%v: a pair of scope %q outside the owned scopes", name, tenant, owned, s)
						}
					}
				}
				for _, scope := range check {
					for _, l := range backfillSensLevels {
						_, _, err := router.EmbedChain(router.FloorSens(l, scope))
						if got, want := classAdmits(cl, scope, string(l)), err == nil; got != want {
							t.Errorf("%s/%s/%v scope %q level %s: classes admit %v, EmbedChain resolves %v (%+v)",
								name, tenant, owned, scope, l, got, want, cl)
						}
					}
				}
			}
		}
	}
}

// classAdmits evaluates the classes as the branches of store.PendingPickSQL
// read them.
func classAdmits(cl store.PendingClasses, scope, sens string) bool {
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
