package handler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// scopeFloorSettingKey is the registry key of config.PoolConfig's
// ScopeSensitivityFloor (pinned by TestScopeFloorSettingKeyIsTheRegistryKey).
const scopeFloorSettingKey = "pool.scope_sensitivity_floor"

// ownerConfigBuildTimeout bounds the build of another tenant's configuration
// that backfillFloor can trigger (config.Store builds a generation the first
// time it is asked for after a change). The build runs on a context of its
// own: config.Store builds a cold generation once for every waiter, on the
// context of the first, so a request that is not the owner's must not end
// the owner's build when its client goes away. A fixed bound, the one the
// detached memo writes of backfillPending use.
const ownerConfigBuildTimeout = 5 * time.Second

// errOwnerConfigFallback: config.Store answered with its fallback, the base
// generation, for a configuration that has a floor row of its own: the
// overlay's build failed or declined, and the base's floor is not the
// owner's. The store says so from the call that returned the generation
// (SnapshotForTenantResolved); a base read at another time cannot tell, a
// settings reload in between makes the fallback a different base.
var errOwnerConfigFallback = errors.New("backfill floor: an owner's configuration did not build")

// backfillFloor is the sensitivity floor the query path's embed backfill
// applies to the blocks it may pick (issue #253): per scope, the higher of
// the floor the caller's configuration sets (callerFloor, today's rule) and
// the floor the configuration of the tenant that OWNS the scope sets. The
// pick is not bound to the caller's tenant (a read scope another tenant
// granted, and with the background arm off the old end of every scope), and
// pool.scope_sensitivity_floor is tenant-overridable: a floor only the owner
// set was not applied, and the owner's block could reach an embed backend
// below it. The background arm resolves the floor per owner
// (SnapshotForTenant of the iterated tenant); this is the same floor for the
// request path. The caller's own floor stays in the fold so that the change
// only raises: a block the request did not send before it does not send now.
//
// A tenant's configuration is the one the background resolves for it: the
// _global base for the default tenant, its first owned scope for any other
// (store.ScopeOwnerConfigScopes, the rule of events.backgroundTenantFor); a
// scope no tenant owns takes the base's floor. A configuration without a row
// of its own on the key inherits the base's floor.
//
// Which scopes are resolved is what the pick can reach. With the background
// arm on (everyScope false) that is readScopes, the caller's read scopes:
// one statement for their owners (primary key of context_tenant_scopes) and,
// when an owner is not the default tenant, one for whether its configuration
// has a floor row (unique index on key, scope); both the size of the key's
// read scopes. With the arm off (everyScope true) the old end reaches every
// scope: one statement for the configurations with a floor row, one for the
// scopes each of them owns, and the base's floor for every other scope. A
// configuration's floor is read for the scopes its tenant owns only, so the
// cost follows the owners' scope lists (each capped by max_scopes), not what
// a floor map names. The snapshots are config.Store's cached generations.
//
// SnapshotForTenantResolved on the request path: the scopes it is called
// with come from the settings table and from context_tenant_scopes, never
// from the request, and only the floor and whether the store fell back to
// the base are read from what it returns (ConfigStore).
//
// An error leaves the caller nothing to apply; backfillPending then skips
// the backfill for the request (fail-closed: no block leaves on a floor that
// could not be read). That includes an owner's configuration that did not
// build (errOwnerConfigFallback).
func (h *QueryHandler) backfillFloor(ctx context.Context, callerFloor config.ScopeFloor, readScopes []string, everyScope bool) (config.ScopeFloor, error) {
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ownerConfigBuildTimeout)
	defer cancel()
	r := &ownerFloors{cfg: h.cfg, buildCtx: buildCtx, floors: map[string]config.ScopeFloor{}, out: maps.Clone(callerFloor)}
	if r.out == nil {
		r.out = config.ScopeFloor{}
	}
	var err error
	if everyScope {
		err = r.foldEveryScope(ctx, h.pool)
	} else {
		err = r.foldReadScopes(ctx, h.pool, readScopes)
	}
	if err != nil {
		return nil, err
	}
	return r.out, nil
}

// ownerFloors is one backfillFloor call: the configurations' floors read so
// far and the fold of them into the caller's.
type ownerFloors struct {
	cfg      ConfigStore
	buildCtx context.Context
	floors   map[string]config.ScopeFloor
	out      config.ScopeFloor
}

// floorOf is the floor of the configuration at configScope. hasRow says the
// configuration has a floor row of its own, so it must not resolve to
// config.Store's fallback to the base generation (errOwnerConfigFallback).
// The base answering for _global is not a fallback: it is that
// configuration.
func (r *ownerFloors) floorOf(configScope string, hasRow bool) (config.ScopeFloor, error) {
	if f, ok := r.floors[configScope]; ok {
		return f, nil
	}
	snap, resolved := r.cfg.SnapshotForTenantResolved(r.buildCtx, configScope)
	if hasRow && !resolved && configScope != store.GlobalScope {
		return nil, fmt.Errorf("%w (%s)", errOwnerConfigFallback, configScope)
	}
	r.floors[configScope] = snap.Pool.ScopeSensitivityFloor
	return r.floors[configScope], nil
}

// fold raises the floor of scope to level where level is higher.
func (r *ownerFloors) fold(scope string, level backends.Sensitivity) {
	if mine, ok := r.out[scope]; ok {
		level = backends.MaxSensitivity(mine, level)
	}
	r.out[scope] = level
}

// foldReadScopes folds the owners' floors of the caller's read scopes (the
// arm runs, so the pick reaches nothing else).
func (r *ownerFloors) foldReadScopes(ctx context.Context, db pgxdb.Querier, readScopes []string) error {
	scopes := slices.Clone(readScopes)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	if len(scopes) == 0 {
		return nil
	}
	owners, err := store.ScopeOwnerConfigScopes(ctx, db, scopes)
	if err != nil {
		return fmt.Errorf("backfill floor: %w", err)
	}
	var tenantConfigs []string
	for _, cs := range owners {
		if cs != store.GlobalScope {
			tenantConfigs = append(tenantConfigs, cs)
		}
	}
	withRow, err := store.SettingScopesWithRow(ctx, db, scopeFloorSettingKey, tenantConfigs)
	if err != nil {
		return fmt.Errorf("backfill floor: %w", err)
	}
	for _, s := range scopes {
		cs, ok := owners[s]
		if !ok {
			cs = store.GlobalScope
		}
		f, err := r.floorOf(cs, withRow[cs])
		if err != nil {
			return err
		}
		if level, ok := f[s]; ok {
			r.fold(s, level)
		}
	}
	return nil
}

// foldEveryScope folds the owners' floors of every scope (the arm is off,
// so the old end of the pick reaches every scope): the floor of each
// configuration with a row of its own for the scopes its tenant owns, and
// the base's for every other scope.
func (r *ownerFloors) foldEveryScope(ctx context.Context, db *pgxpool.Pool) error {
	refs, err := store.SettingRowsForKeys(ctx, db, []string{scopeFloorSettingKey})
	if err != nil {
		return fmt.Errorf("backfill floor: settings rows: %w", err)
	}
	var rowScopes []string
	for _, ref := range refs {
		if ref.Scope != store.GlobalScope {
			rowScopes = append(rowScopes, ref.Scope)
		}
	}
	owned, err := store.ConfigScopeOwnedScopes(ctx, db, rowScopes)
	if err != nil {
		return fmt.Errorf("backfill floor: %w", err)
	}
	// The scopes whose owner's configuration has a floor row of its own:
	// that configuration's floor replaces the base's for them.
	covered := map[string]bool{}
	for configScope, scopes := range owned {
		f, err := r.floorOf(configScope, true)
		if err != nil {
			return err
		}
		for _, s := range scopes {
			covered[s] = true
			if level, ok := f[s]; ok {
				r.fold(s, level)
			}
		}
	}
	baseFloor, err := r.floorOf(store.GlobalScope, false)
	if err != nil {
		return err
	}
	for s, level := range baseFloor {
		if !covered[s] {
			r.fold(s, level)
		}
	}
	return nil
}
