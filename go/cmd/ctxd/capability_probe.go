// Boot of the backend capability probe (FOLGE-Z Z1/Z2): the pool asks each
// row's engine what it can do (vision today) — once now, after every pool
// reload and every pool.capability_probe_interval — and the model-alias
// registry resolves each alias's effective vision against those answers.
// Every pool reload and every probe round that changed a row triggers a
// registry reload (single-flight, off the caller), so an alias without
// policy.vision follows its chain without an operator step.

package main

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelalias"
)

// bootCapabilityProbe wires the probe to the registry and starts its
// cadence, bound to ctx (the process lifecycle): it ends with ctx, and the
// round in flight then records nothing.
//
// The registry's boot load ran before the view existed (bootModelAliases),
// so the view is installed and a reload triggered at once: from then on
// every pool reload and every probe change re-resolves the aliases.
func bootCapabilityProbe(ctx context.Context, pool *backends.Pool, reg *modelalias.Registry, cfgStore *config.Store) {
	reg.SetVisionView(aliasVisionView(pool, cfgStore))
	pool.OnChange(func() { reg.Trigger(ctx) })
	reg.Trigger(ctx)
	go pool.RunCapabilityProbe(ctx, capabilityProbeInterval(cfgStore))
}

// aliasVisionView is the pool under the /v1 export rule
// (gateway.exportable_roles, read per call — hot).
func aliasVisionView(pool *backends.Pool, cfgStore *config.Store) backends.ChainVisionView {
	return backends.ChainVisionView{Pool: pool, ExportableRoles: func() []string {
		return cfgStore.Snapshot().Gateway.ExportableRoles //nolint:forbidigo // MT 06 BLIND: gateway.exportable_roles is global-only, the server admin's export list for every tenant alias.
	}}
}

// capabilityProbeInterval reads pool.capability_probe_interval per wait (hot).
func capabilityProbeInterval(cfgStore *config.Store) func() time.Duration {
	return func() time.Duration {
		return cfgStore.Snapshot().Pool.CapabilityProbeInterval //nolint:forbidigo // MT 06 BLIND: the probe cadence is global-only; the pool is one process-wide good.
	}
}
