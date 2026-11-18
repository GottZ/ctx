package handler

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// scopeFloorSettingKey is the registry key of config.PoolConfig's
// ScopeSensitivityFloor (pinned by TestScopeFloorSettingKeyIsTheRegistryKey).
const scopeFloorSettingKey = "pool.scope_sensitivity_floor"

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
// Where a floor can come from: the _global base and every configuration
// with a row of its own on the key (store.SettingRowsForKeys); any other
// tenant's configuration inherits the base's value. The scopes those floors
// name are resolved to their owner's configuration scope
// (store.ScopeOwnerConfigScopes, the scheduler's rule), and a floor counts
// only in its owner's configuration; a scope no tenant owns takes the base's.
// Cost: one read of the settings rows on the key (unique index on key,
// scope) per request, plus one read of the owners of the floored scopes when
// any floor is set; both are the size of the configuration, not of the
// corpus. The snapshots are config.Store's cached generations.
//
// SnapshotForTenant on the request path: the scopes it is called with come
// from the settings table and from context_tenant_scopes, never from the
// request, and only the floor is read from what it returns (ConfigStore).
//
// An error leaves the caller nothing to apply; the backfill is then skipped
// for the request (fail-closed: no block leaves on a floor that could not be
// read).
func (h *QueryHandler) backfillFloor(ctx context.Context, callerFloor config.ScopeFloor) (config.ScopeFloor, error) {
	refs, err := store.SettingRowsForKeys(ctx, h.pool, []string{scopeFloorSettingKey})
	if err != nil {
		return nil, fmt.Errorf("backfill floor: settings rows: %w", err)
	}
	floors := map[string]config.ScopeFloor{}
	floorOf := func(configScope string) config.ScopeFloor {
		f, ok := floors[configScope]
		if !ok {
			f = h.cfg.SnapshotForTenant(ctx, configScope).Pool.ScopeSensitivityFloor
			floors[configScope] = f
		}
		return f
	}
	floorOf(store.GlobalScope)
	for _, r := range refs {
		floorOf(r.Scope)
	}
	var named []string
	for _, f := range floors {
		for s := range f {
			named = append(named, s)
		}
	}
	if len(named) == 0 {
		return callerFloor, nil
	}
	slices.Sort(named)
	named = slices.Compact(named)
	owners, err := store.ScopeOwnerConfigScopes(ctx, h.pool, named)
	if err != nil {
		return nil, fmt.Errorf("backfill floor: %w", err)
	}
	out := maps.Clone(callerFloor)
	if out == nil {
		out = config.ScopeFloor{}
	}
	for _, s := range named {
		configScope, ok := owners[s]
		if !ok {
			configScope = store.GlobalScope
		}
		level, ok := floorOf(configScope)[s]
		if !ok {
			continue
		}
		if mine, ok := out[s]; ok {
			level = backends.MaxSensitivity(mine, level)
		}
		out[s] = level
	}
	return out, nil
}
