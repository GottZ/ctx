package main

import (
	"github.com/GottZ/ctx/internal/config"
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
