package main

import (
	"testing"

	"github.com/GottZ/ctx/internal/config"
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
