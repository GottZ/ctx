package main

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/handler"
)

// TestGatewaySettingsGetterIsHot: the getter cmd/ctxd hands to internal/oai
// (design/02 §3.3 — oai never imports config) reads one snapshot per call,
// so a settings write that replaces the config generation reaches the next
// request without a restart.
func TestGatewaySettingsGetterIsHot(t *testing.T) {
	store := config.NewStore(config.Defaults())
	get := gatewaySettingsGetter(store)
	if s := get(); s.Enabled || s.MaxRequestBytes != 8<<20 || s.InflightBudget != 256<<20 {
		t.Fatalf("defaults: %+v, want enabled=false, 8 MiB request cap, 256 MiB budget", s)
	}
	next := config.Defaults()
	next.Server.DBPass = "test-password" // V11: Replace validates the whole generation
	next.Gateway.Enabled = true
	next.Gateway.MaxRequestBytes = 2 << 20
	if err := store.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if s := get(); !s.Enabled || s.MaxRequestBytes != 2<<20 {
		t.Errorf("after replace: %+v, want enabled=true and the 2 MiB cap", s)
	}
}

// TestWhoamiModelAPIGetter: the whoami model_api capability (design/05 §4.6,
// W05-01) is the _global gateway policy in whole seconds — gateway.enabled,
// gateway.tenant_self_service, the APP key lifetime (the peer lifetime belongs
// to the peer page, W05-14), the rotation grace and the expiry warning. One
// snapshot per call, so a settings write reaches the next whoami.
func TestWhoamiModelAPIGetter(t *testing.T) {
	store := config.NewStore(config.Defaults())
	get := whoamiModelAPIGetter(store)
	want := handler.WhoamiModelAPI{
		Enabled:               false,
		SelfService:           false,
		KeyMaxLifetimeSeconds: 31536000, // gateway.key_max_lifetime_app default (365 d), not the peer's 0
		RotationGraceSeconds:  86400,
		ExpiryWarnSeconds:     1209600, // gateway.key_expiry_warn default (336 h)
	}
	if got := get(); got != want {
		t.Fatalf("defaults: %+v, want %+v", got, want)
	}
	next := config.Defaults()
	next.Server.DBPass = "test-password" // V11: Replace validates the whole generation
	next.Gateway.Enabled = true
	next.Gateway.Consumers.TenantSelfService = true
	next.Gateway.Consumers.KeyMaxLifetimeApp = 30 * 24 * time.Hour
	next.Gateway.Consumers.KeyMaxLifetimePeer = 7 * 24 * time.Hour
	next.Gateway.Consumers.KeyRotationGrace = 2 * time.Hour
	next.Gateway.Consumers.KeyExpiryWarn = 48 * time.Hour
	if err := store.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}
	want = handler.WhoamiModelAPI{
		Enabled:               true,
		SelfService:           true,
		KeyMaxLifetimeSeconds: 2592000,
		RotationGraceSeconds:  7200,
		ExpiryWarnSeconds:     172800,
	}
	if got := get(); got != want {
		t.Errorf("after replace: %+v, want %+v", got, want)
	}
	// The two switches travel independently (review F-1): a swapped or a
	// folded mapping (self_service := enabled && tenant_self_service) fails
	// one of the asymmetric generations.
	for _, sw := range []struct{ enabled, selfService bool }{{true, false}, {false, true}} {
		gen := config.Defaults()
		gen.Server.DBPass = "test-password"
		gen.Gateway.Enabled = sw.enabled
		gen.Gateway.Consumers.TenantSelfService = sw.selfService
		if err := store.Replace(gen); err != nil {
			t.Fatalf("replace: %v", err)
		}
		if got := get(); got.Enabled != sw.enabled || got.SelfService != sw.selfService {
			t.Errorf("gateway.enabled=%v tenant_self_service=%v: got Enabled=%v SelfService=%v",
				sw.enabled, sw.selfService, got.Enabled, got.SelfService)
		}
	}
}
