package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// TestScopeFloorSettingKeyIsTheRegistryKey pins scopeFloorSettingKey to the
// registry key of the floor and to its tenancy: backfillFloor reads the rows
// on that key, and only a tenant-overridable key has rows outside _global.
func TestScopeFloorSettingKeyIsTheRegistryKey(t *testing.T) {
	k, ok := config.KeyByName(scopeFloorSettingKey)
	if !ok {
		t.Fatalf("%s is not a registry key", scopeFloorSettingKey)
	}
	if k.Type != "scope_floor" || k.Tenancy != "tenant-overridable" {
		t.Errorf("%s: type %q tenancy %q, want scope_floor, tenant-overridable", scopeFloorSettingKey, k.Type, k.Tenancy)
	}
}

// ownfGeneration is a configuration config.Store.Replace accepts, with floor
// as its pool.scope_sensitivity_floor.
func ownfGeneration(floor config.ScopeFloor) *config.Config {
	c := config.Defaults()
	c.Server.DBPass = "test-password" // Replace validates the whole config, boot-required keys included
	c.Pool.ScopeSensitivityFloor = floor
	return c
}

// TestBackfillFloorOfSeesTheFallbackOfItsOwnCall: whether an owner's
// configuration with a floor row answered with its own generation or with
// config.Store's fallback to the base is read from the call that returned it.
// A base read at another time is not that signal: a settings reload
// (config.Store.Replace) between the two reads makes the fallback one base
// and the comparison another, and the old base's floor then counted as the
// owner's.
//
// RED before the fix (floorOf compared the snapshot with a base it read
// itself): reload_during_the_failed_build and reload_between_two_owners
// (no error, the floor of a base), overlay_answered_with_the_base (an error
// for a generation the overlay answered). The two controls pass before and
// after.
func TestBackfillFloorOfSeesTheFallbackOfItsOwnCall(t *testing.T) {
	ctx := context.Background()
	const owner, other = "acme", "beta"
	baseFloor1 := config.ScopeFloor{owner: backends.SensPublic}
	baseFloor2 := config.ScopeFloor{owner: backends.SensInternal}
	ownerFloor := config.ScopeFloor{owner: backends.SensCredentials}
	broken := errors.New("probe: the owner's rows cannot be read")
	floors := func(cs ConfigStore) *ownerFloors {
		return &ownerFloors{cfg: cs, ctx: ctx, floors: map[string]config.ScopeFloor{}, out: config.ScopeFloor{}}
	}
	reload := func(t *testing.T, cs *config.Store, next *config.Config) {
		t.Helper()
		if err := cs.Replace(next); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}

	// The reload lands while the owner's build runs and fails: the call
	// answers with the base it started on, the store holds another by then.
	t.Run("reload_during_the_failed_build", func(t *testing.T) {
		cs := config.NewStore(ownfGeneration(baseFloor1))
		next := ownfGeneration(baseFloor2)
		cs.SetOverlay(func(context.Context, *config.Config, string) (*config.Config, error) {
			if err := cs.Replace(next); err != nil {
				t.Errorf("reload: %v", err)
			}
			return nil, broken
		})
		f, err := floors(cs).floorOf(owner, true)
		if !errors.Is(err, errOwnerConfigFallback) {
			t.Fatalf("floorOf = %v, %v; want errOwnerConfigFallback (the configuration did not build)", f, err)
		}
	})

	// One owner builds, a reload lands, the next owner's build fails: the
	// fallback is the new base, any base read with the first owner the old.
	t.Run("reload_between_two_owners", func(t *testing.T) {
		cs := config.NewStore(ownfGeneration(baseFloor1))
		cs.SetOverlay(func(_ context.Context, _ *config.Config, scope string) (*config.Config, error) {
			if scope == owner {
				return ownfGeneration(ownerFloor), nil
			}
			return nil, broken
		})
		r := floors(cs)
		if f, err := r.floorOf(owner, true); err != nil || f[owner] != backends.SensCredentials {
			t.Fatalf("premise: the first owner's floor = %v, %v; want its own", f, err)
		}
		reload(t, cs, ownfGeneration(baseFloor2))
		f, err := r.floorOf(other, true)
		if !errors.Is(err, errOwnerConfigFallback) {
			t.Fatalf("floorOf = %v, %v; want errOwnerConfigFallback (the configuration did not build)", f, err)
		}
	})

	// settings.TenantOverlay answers a tenant without rows of its own with
	// the base it was given: that is the tenant's generation (it inherits),
	// not a fallback, although it is the base pointer.
	t.Run("overlay_answered_with_the_base", func(t *testing.T) {
		cs := config.NewStore(ownfGeneration(baseFloor1))
		cs.SetOverlay(func(_ context.Context, base *config.Config, _ string) (*config.Config, error) {
			return base, nil
		})
		f, err := floors(cs).floorOf(owner, true)
		if err != nil || f[owner] != backends.SensPublic {
			t.Fatalf("floorOf = %v, %v; want the inherited floor of the base", f, err)
		}
	})

	// Controls: a failed build without a reload, and a built generation.
	t.Run("control_failed_build", func(t *testing.T) {
		cs := config.NewStore(ownfGeneration(baseFloor1))
		cs.SetOverlay(func(context.Context, *config.Config, string) (*config.Config, error) {
			return nil, broken
		})
		if f, err := floors(cs).floorOf(owner, true); !errors.Is(err, errOwnerConfigFallback) {
			t.Fatalf("floorOf = %v, %v; want errOwnerConfigFallback", f, err)
		}
	})
	t.Run("control_built_generation", func(t *testing.T) {
		cs := config.NewStore(ownfGeneration(baseFloor1))
		cs.SetOverlay(func(context.Context, *config.Config, string) (*config.Config, error) {
			return ownfGeneration(ownerFloor), nil
		})
		if f, err := floors(cs).floorOf(owner, true); err != nil || f[owner] != backends.SensCredentials {
			t.Fatalf("floorOf = %v, %v; want the owner's own floor", f, err)
		}
	})
}
