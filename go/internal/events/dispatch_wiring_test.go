package events

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
)

// TestDispatchSettingsMapping pins the total field mapping (design/01 §3.1:
// the reload owner maps config.DispatchConfig → dispatch.Settings). Every
// value is deliberately non-default so a dropped or crossed field cannot
// hide behind a shared default.
func TestDispatchSettingsMapping(t *testing.T) {
	in := config.DispatchConfig{
		Enabled:                      false,
		BackgroundQueueMax:           7,
		InteractiveQueuePerPrincipal: 3,
		InteractiveQueuePerTenant:    5,
		InteractiveQueueMax:          11,
		LeaseReapGrace:               13 * time.Second,
		LeaseMaxAge:                  17 * time.Second,
	}
	want := dispatch.Settings{
		Enabled:                      false,
		BackgroundQueueMax:           7,
		InteractiveQueuePerPrincipal: 3,
		InteractiveQueuePerTenant:    5,
		InteractiveQueueMax:          11,
		LeaseReapGrace:               13 * time.Second,
		LeaseMaxAge:                  17 * time.Second,
		Fairness:                     dispatch.FairnessFIFO,
	}
	if got := DispatchSettings(in); got != want {
		t.Fatalf("DispatchSettings = %+v, want %+v", got, want)
	}
}

// TestDispatchBackendRowsMapping pins the leaf-safe projection (design/01
// N9): Name/Scope/BaseURL/Limits and nothing else — including disabled rows
// (their declared slots stay in the K2 MIN merge, fail-closed).
func TestDispatchBackendRowsMapping(t *testing.T) {
	in := []backends.Backend{
		{
			Name:    "herbert-chat",
			Scope:   "_global",
			Host:    "http://HOST:8089/v1",
			Enabled: true,
			Limits:  map[string]any{"slots": float64(1)},
			APIKey:  "never-projected",
		},
		{
			Name:    "tenant-private",
			Scope:   "acme",
			Host:    "https://api.example.com",
			Enabled: false, // disabled rows are projected too
		},
	}
	want := []dispatch.BackendRow{
		{Name: "herbert-chat", Scope: "_global", BaseURL: "http://HOST:8089/v1", Limits: map[string]any{"slots": float64(1)}},
		{Name: "tenant-private", Scope: "acme", BaseURL: "https://api.example.com"},
	}
	if got := DispatchBackendRows(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("DispatchBackendRows = %+v, want %+v", got, want)
	}
}

// TestRefreshDispatchSettings_PushesSnapshot proves the reload-owner seam
// end to end on an observable: a snapshot with dispatch.enabled=false lands
// as Snapshot().Enabled==false on the dispatcher (the emergency stop is the
// one settings knob visible without a lease).
func TestRefreshDispatchSettings_PushesSnapshot(t *testing.T) {
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password-123")
	cc, issues := config.FromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cc.Dispatch.Enabled = false

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	if !d.Snapshot().Enabled {
		t.Fatal("precondition: default settings must be Enabled")
	}

	h := &SettingsWriteHandler{cfg: config.NewStore(cc), dispatcher: d}
	h.refreshDispatchSettings()
	if d.Snapshot().Enabled {
		t.Fatal("refreshDispatchSettings did not push dispatch.enabled=false into the dispatcher")
	}
}

// TestRefreshDispatchSettings_NilDispatcherInert pins the unwired state
// (tests, pre-wire boot): no panic, no effect.
func TestRefreshDispatchSettings_NilDispatcherInert(t *testing.T) {
	h := &SettingsWriteHandler{}
	h.refreshDispatchSettings()
	h.refreshDispatchPolicy()
}

// TestRefreshDispatchPolicy_DerivesFromPoolSnapshot proves the N9 seam end
// to end: pool snapshot → BackendRow → DerivePolicy → UpdatePolicy. The
// derived slots cap is observed through the dispatcher's own snapshot after
// one acquire materializes the target state (TargetSnapshot.Slots reads the
// live policy).
func TestRefreshDispatchPolicy_DerivesFromPoolSnapshot(t *testing.T) {
	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest([]backends.Backend{{
		Name:   "herbert-chat",
		Scope:  "_global",
		Host:   "http://HOST:8089/v1", // canonicalizes to http://host:8089
		Limits: map[string]any{"slots": float64(2)},
	}})

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	h := &SettingsWriteHandler{backendPool: pool, dispatcher: d}
	h.refreshDispatchPolicy()

	lease, _, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: "http://host:8089"},
		Class:  dispatch.ClassBackground,
		Role:   "test",
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release()

	snap := d.Snapshot()
	if len(snap.Targets) != 1 {
		t.Fatalf("snapshot targets = %d, want 1", len(snap.Targets))
	}
	ts := snap.Targets[0]
	if ts.Origin != "http://host:8089" {
		t.Fatalf("origin = %q, want http://host:8089 (normalization drift)", ts.Origin)
	}
	if ts.Slots != 2 {
		t.Fatalf("derived slots = %d, want 2 — NOTIFY policy seam broken", ts.Slots)
	}
	if ts.Held != 1 {
		t.Fatalf("held = %d, want 1 (declared policy must hold a slot, not pass through)", ts.Held)
	}
}
