package main

import (
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// gatewaySettingsGetter is the settings source of the OpenAI-compatible
// gateway (design/02 §3.3): internal/oai may not import config, so cmd/ctxd
// hands it this getter. Every call takes one snapshot of the live store, so
// the gateway reads each hot gateway.* key per request and a settings write
// applies from the next request on. The keys are global-only — one gateway
// per process — which is why the tenant-less Snapshot is the right read.
// Its consumer is the /v1 and /v2 group (W01-6 mounts it; until then it is
// reached only from tests).
func gatewaySettingsGetter(cfgStore *config.Store) func() oai.Settings {
	return func() oai.Settings {
		return cfgStore.Snapshot().GatewaySettings() //nolint:forbidigo // MT 06 BLIND: gateway.* is global-only (one gateway, one budget per process); the gate runs before any credential, so no tenant exists to resolve.
	}
}

// whoamiModelAPIGetter is the source of the whoami capability model_api
// (design/05 §4.6, W05-01): gateway.enabled, gateway.tenant_self_service and
// the app-key lifetime, rotation grace and expiry warning in whole seconds —
// the data the SPA derives viewModelApi/mintModelKeys from. One snapshot per
// call, so a settings write shows on the next whoami. The peer lifetime is not
// carried (the peer page, W05-14, owns it).
func whoamiModelAPIGetter(cfgStore *config.Store) func() handler.WhoamiModelAPI {
	return func() handler.WhoamiModelAPI {
		c := cfgStore.Snapshot() //nolint:forbidigo // MT 06 BLIND: gateway.enabled and the gateway.* consumer policy are global-only switches over a host-global good (design/01 §4.6) — the same _global read the manage handlers enforce with.
		g := c.Gateway.Consumers
		return handler.WhoamiModelAPI{
			Enabled:               c.Gateway.Enabled,
			SelfService:           g.TenantSelfService,
			KeyMaxLifetimeSeconds: int64(g.KeyMaxLifetimeApp / time.Second),
			RotationGraceSeconds:  int64(g.KeyRotationGrace / time.Second),
			ExpiryWarnSeconds:     int64(g.KeyExpiryWarn / time.Second),
		}
	}
}
