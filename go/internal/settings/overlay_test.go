package settings

// 06-C2 unit gates for the per-tenant config overlay (overlay.go) — the consumer
// that ties loadTenantOverrideRows + BuildFromRows into the config.TenantOverlay
// value the Store injects at 06-C3. The DB-bound closure is covered end-to-end by
// overlay_integration_test.go; here the pure decision logic and the build-level
// admission gate run WITHOUT a pool.

import (
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// TestHasScope pins the "inherit base vs build a tenant generation" decision:
// the two-scope load ALWAYS returns the _global base rows, so only a row OWNED
// by the tenant means "this tenant has its own settings". RED against a check
// that keys on row presence (any row) rather than the tenant scope — that would
// build a redundant generation for every tenant that merely inherits _global
// (the §10.2 footprint blow-up at N tenants).
func TestHasScope(t *testing.T) {
	const tenant = "work"
	cases := []struct {
		name string
		rows []store.SettingOverride
		want bool
	}{
		{"no rows", nil, false},
		{"_global only", []store.SettingOverride{row("rerank.blend_weight", `0.4`)}, false},
		{"tenant row present alongside _global", []store.SettingOverride{
			row("rerank.blend_weight", `0.4`),
			tenantRow("rerank.blend_weight", tenant, `0.6`),
		}, true},
		{"tenant row only", []store.SettingOverride{tenantRow("chat.model", tenant, `"m"`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasScope(tc.rows, tenant); got != tc.want {
				t.Errorf("hasScope = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTenantRestartKeyGated is the §8.2 admission check at the build level: a
// tenant override on a restart/coupled key (server.db_host — restart, and so
// global-only) must NOT take effect. The global-only gate (T28) front-runs the
// mut-gate for tenant rows — every restart/coupled key is also global-only — so
// the override is dropped before config.Build; the effective value stays the
// env/default base and the Source is never "tenant". A WARN names the dropped
// key. RED against an overlay that lets a tenant flip a restart-class key.
func TestTenantRestartKeyGated(t *testing.T) {
	resetEnv(t)
	const key = "server.db_host"
	if !config.IsGlobalOnly(key) {
		t.Fatalf("test premise: %s must be global-only (restart class)", key)
	}

	base, _ := buildWith(nil, nil, globalAndTenant)
	cfg, issues := buildWith([]store.SettingOverride{
		tenantRow(key, testTenant, `"db.tenant.example"`),
	}, nil, globalAndTenant)

	if cfg.Server.DBHost != base.Server.DBHost {
		t.Errorf("tenant must not override a restart key: got %q, want base %q",
			cfg.Server.DBHost, base.Server.DBHost)
	}
	if cfg.Source(key) == config.SourceTenant {
		t.Errorf("a gated tenant override must never be attributed to the tenant")
	}
	var warned bool
	for _, is := range issues {
		if is.Field == key && is.Severity == config.SeverityWarn {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a WARN on the gated tenant %s override, got %+v", key, issues)
	}
}

// TestTenantOverlayInheritsBaseLabel pins the source-attribution contrast at the
// merge level: a key won by _global keeps Source "settings", a key won by the
// tenant carries Source "tenant" — in the SAME build. RED against threading the
// tenant scope into the sources map for every override (which would mislabel the
// operator's _global value as tenant-sourced).
func TestTenantOverlayInheritsBaseLabel(t *testing.T) {
	resetEnv(t)
	cfg, _ := buildWith([]store.SettingOverride{
		row("chat.model", `"global-model"`),               // _global only ⇒ "settings"
		tenantRow("rerank.blend_weight", testTenant, `0.6`), // tenant ⇒ "tenant"
	}, nil, globalAndTenant)

	if cfg.Chat.Model != "global-model" || cfg.Source("chat.model") != config.SourceSettings {
		t.Errorf("_global-won key: got %q source %q, want global-model/settings",
			cfg.Chat.Model, cfg.Source("chat.model"))
	}
	if cfg.Rerank.BlendWeight != 0.6 || cfg.Source("rerank.blend_weight") != config.SourceTenant {
		t.Errorf("tenant-won key: got %v source %q, want 0.6/tenant",
			cfg.Rerank.BlendWeight, cfg.Source("rerank.blend_weight"))
	}
}
