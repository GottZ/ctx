// Boot of the model-alias registry (W02-03, design/02 §4.4): the /v1 model
// namespace, loaded once here and kept hot by the scheduler's NOTIFY listener
// (entity context_model_aliases, every backlog, and — for the tenant ceiling —
// every _global settings write). W02-04 adds the ceilings: one getter over the
// gateway.* keys, shared by the registry (reload clamp of tenant aliases) and
// the model-alias-* manage actions (write-time 422).

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// bootModelAliases builds the registry, installs the ceiling getter and
// loads it. A failed load is not fatal: the registry stays empty — nothing
// offered, fail-closed — and the listener's first backlog reload (pgxlisten
// runs it on every connect, the first included) tries again.
func bootModelAliases(ctx context.Context, q pgxdb.Querier, ceilings func() modelalias.Ceilings) *modelalias.Registry {
	reg := modelalias.NewRegistry(q)
	reg.SetCeilings(ceilings)
	if err := reg.Reload(ctx); err != nil {
		slog.Error("modelalias: initial reload failed — no model alias is offered until a reload succeeds", "error", err)
	}
	return reg
}

// modelAliasCeilings is the setting source of the alias policy ceilings
// (design/02 §3.2, §3.3): every call takes one snapshot of the live store, so
// a hot write of a gateway.* key applies from the next write check and the
// next registry reload on. The keys are global-only — one ceiling set per
// process — which is why the tenant-less Snapshot is the right read.
func modelAliasCeilings(cfgStore *config.Store) func() modelalias.Ceilings {
	return func() modelalias.Ceilings {
		// GatewaySettings is the one config → gateway mapping (W02-02); this
		// maps its ceiling part onto the leaf package's types.
		g := cfgStore.Snapshot().GatewaySettings() //nolint:forbidigo // MT 06 BLIND: gateway.* is global-only; the ceilings cap aliases of every tenant alike (the tenant ceiling is the server admin's, never a tenant's override).
		t := g.TenantAliasCeiling
		return modelalias.Ceilings{
			MaxOutputTokens:    g.MaxOutputTokensCeiling,
			MaxRequestBytes:    g.MaxRequestBytes,
			EmbeddingMaxFloats: g.EmbeddingMaxFloats,
			Tenant: modelalias.TenantCeiling{
				MaxOutputTokens:    t.MaxOutputTokens,
				MaxN:               t.MaxN,
				TimeoutS:           int(t.Timeout / time.Second),
				EmbeddingMaxInputs: t.EmbeddingMaxInputs,
				RerankMaxDocuments: t.RerankMaxDocuments,
				ToolsMaxCount:      t.ToolsMaxCount,
			},
		}
	}
}
