// Boot of the limits registry of the model API's meter (wave W06-15,
// migration 168, design/06 §4.3): the rows of context_gateway_limits, loaded
// once here and kept hot by the scheduler's NOTIFY listener (entity
// context_gateway_limits and every backlog) and by the gateway-limits-*
// manage actions (synchronous reload after each committed write). The meter
// adapter (meterLimiter) and the manage actions read the same registry.

package main

import (
	"context"
	"log/slog"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// bootGatewayLimits builds the registry over the store and loads it. A failed
// load is not fatal: the registry stays empty — every consumer meters under
// the settings floor, the behaviour before W06-15 — and the listener's first
// backlog reload tries again.
func bootGatewayLimits(ctx context.Context, q pgxdb.Querier) *gatewaylimits.Registry {
	reg := gatewaylimits.NewRegistry(func(ctx context.Context) ([]gatewaylimits.Row, error) {
		return store.LoadGatewayLimits(ctx, q)
	})
	if err := reg.Load(ctx); err != nil {
		slog.Warn("gatewaylimits: initial load failed — the gateway.* settings hold for every consumer until a reload succeeds", "error", err)
	}
	return reg
}

// gatewayLimitDefaults is the settings floor per op of the gateway-limits-*
// manage actions (the ceiling of a tenant-admin write, the effective
// values): one snapshot of the live store per call, the same mapping the
// meter adapter uses (meterDefaults). The keys are global-only.
func gatewayLimitDefaults(cfgStore *config.Store) func(op keyquota.Op) gatewaylimits.Defaults {
	return func(op keyquota.Op) gatewaylimits.Defaults {
		return meterDefaults(cfgStore.Snapshot().GatewaySettings().Meter, op) //nolint:forbidigo // MT 06 BLIND: gateway.* is global-only (one meter per process); a tenant narrows through limits rows, never through a setting.
	}
}
