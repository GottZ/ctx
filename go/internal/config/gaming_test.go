package config_test

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestGamingStateDefault pins the F3-P6 defaults: gaming is OFF and the
// disabled list is the herbert GPU backends (so a flip frees the GPU while the
// CPU/external rows stay in as failover). gaming.* is env:"-", so the value is
// always the default here regardless of the test environment's env vars.
func TestGamingStateDefault(t *testing.T) {
	cfg, issues := config.FromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("FromEnv had errors: %+v", issues)
	}
	gs := cfg.GamingState()
	if gs.Active {
		t.Error("default gaming.active = true, want false")
	}
	want := []string{"herbert-chat", "herbert-rerank"}
	if !reflect.DeepEqual(gs.DisabledBackends, want) {
		t.Errorf("default gaming.disabled_backends = %v, want %v", gs.DisabledBackends, want)
	}
}

// TestGamingStateMapping pins the PoolConfig → backends.GamingState mapping:
// the chain-time exclusion is exactly what the settings snapshot holds (the
// pool carries no policy — backends/pool.go decoupling).
func TestGamingStateMapping(t *testing.T) {
	cfg := &config.Config{Pool: config.PoolConfig{
		GamingActive:           true,
		GamingDisabledBackends: []string{"herbert-chat", "herbert-rerank"},
	}}
	gs := cfg.GamingState()
	if !gs.Active {
		t.Error("GamingState().Active = false, want true")
	}
	if !reflect.DeepEqual(gs.DisabledBackends, []string{"herbert-chat", "herbert-rerank"}) {
		t.Errorf("GamingState().DisabledBackends = %v", gs.DisabledBackends)
	}
	// The exclusion only bites when active — the zero/false state never drops a
	// backend (mirrors GamingState.disables, the pool's contract).
	off := &config.Config{Pool: config.PoolConfig{GamingActive: false, GamingDisabledBackends: []string{"herbert-chat"}}}
	if off.GamingState().Active {
		t.Error("inactive gaming reports Active = true")
	}
}
