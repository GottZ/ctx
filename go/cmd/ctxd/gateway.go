package main

import (
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/settings"
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

// federationMaxDepthGetter is the federation.max_depth source of the
// LoopGuards (F-W8, design/04 §4.6 step 3): one snapshot per call, so the
// hot key applies from the next request on. Global-only like gateway.*: the
// guard runs before any tenant is resolved on /v1, and the depth of a chain
// is a property of the federation topology, not of a tenant.
func federationMaxDepthGetter(cfgStore *config.Store) func() int {
	return func() int {
		return cfgStore.Snapshot().Federation.MaxDepth //nolint:forbidigo // MT 06 BLIND: federation.max_depth is global-only (one federation topology per process); the guard runs before any tenant is resolved.
	}
}

// gatewayIngressAssert is the boot half of "no /v1 without its ingress
// protection" (F-W8, design/04 §4.6, §7.2 no. 1): gateway.enabled
// effectively true — from CTX_GATEWAY_ENABLED or a settings override, the
// snapshot does not care which — while the router's federation ingress is
// not registered refuses the boot. The settings write of the key is the
// other half (handler/settings.go, 409); a psql write is caught here at the
// next start.
//
// The remedy names the layer that set the value: the settings API is not
// reachable while the boot refuses, and an env value does not beat a
// settings row (Fremd-Review FR-2).
func gatewayIngressAssert(c *config.Config, in *fedhop.Ingress) error {
	if !c.Gateway.Enabled || in.Registered() {
		return nil
	}
	src := c.Source("gateway.enabled")
	remedy := "unset CTX_GATEWAY_ENABLED or set it to false"
	if src == "settings" {
		remedy = "remove the settings row (psql: DELETE FROM context_settings WHERE key = 'gateway.enabled'), or start once with " +
			settings.EnvDisable + "=1 and DELETE /api/settings/gateway.enabled"
	}
	return fmt.Errorf("gateway.enabled is true (source: %s) but no model group with its federation LoopGuard is mounted — the model API cannot be enabled before its federation ingress is mounted; %s, or run a build that mounts /v1",
		src, remedy)
}
