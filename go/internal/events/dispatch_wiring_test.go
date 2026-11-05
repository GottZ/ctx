package events

import (
	"context"
	"log/slog"
	"math"
	"reflect"
	"strings"
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
		PreemptReleaseTimeout:        19 * time.Second,
		BackgroundAgingAfter:         23 * time.Second,
		// A03-W3: the external order and the external cap ladder.
		ExternalOrder:            "o2",
		ExternalQueuePerConsumer: 29,
		ExternalQueuePerTenant:   31,
		ExternalQueueMax:         37,
		ExternalLeaseMax:         41 * time.Second,
		// A03-W5: the external wait budget.
		ExternalMaxWait: 43 * time.Second,
		// A03-W6: the Retry-After steps of the external class.
		ExternalRetryAfterSteps: "3,7",
	}
	o2, ok := dispatch.RankOrderFor(dispatch.ExternalOrderO2)
	if !ok {
		t.Fatal("named order o2 missing")
	}
	want := dispatch.Settings{
		Enabled:                      false,
		BackgroundQueueMax:           7,
		InteractiveQueuePerPrincipal: 3,
		InteractiveQueuePerTenant:    5,
		InteractiveQueueMax:          11,
		LeaseReapGrace:               13 * time.Second,
		LeaseMaxAge:                  17 * time.Second,
		PreemptReleaseTimeout:        19 * time.Second,
		BackgroundAgingAfter:         23 * time.Second,
		Fairness:                     dispatch.FairnessFIFO,
		RankOrder:                    o2,
		ExternalQueuePerConsumer:     29,
		ExternalQueuePerTenant:       31,
		ExternalQueueMax:             37,
		ExternalLeaseMax:             41 * time.Second,
		ExternalMaxWait:              43 * time.Second,
		ExternalRetryAfterSteps:      []time.Duration{3 * time.Second, 7 * time.Second},
	}
	if got := DispatchSettings(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("DispatchSettings = %+v, want %+v", got, want)
	}
	// o1 is the default reading; an empty value (a literal Config) reads as
	// o1 too — the zero value never selects a different order.
	for _, name := range []string{"o1", ""} {
		in.ExternalOrder = name
		if got := DispatchSettings(in).RankOrder; got != dispatch.DefaultRankOrder() {
			t.Fatalf("external_order %q maps to %v, want the default o1", name, got)
		}
	}
	// An empty or malformed step list — only a literal Config bypassing
	// Validate (V46) can carry one — maps to the default steps.
	for _, raw := range []string{"", "5,2", "a"} {
		in.ExternalRetryAfterSteps = raw
		if got := DispatchSettings(in).ExternalRetryAfterSteps; !reflect.DeepEqual(got, dispatch.DefaultSettings().ExternalRetryAfterSteps) {
			t.Fatalf("external_retry_after_steps %q maps to %v, want the default steps", raw, got)
		}
	}
}

// TestDispatchBackendRowsMapping pins the leaf-safe projection (design/01
// N9): Name/Scope/BaseURL/Limits and nothing else — including disabled rows
// (their declared slots stay in the K2 MIN merge, fail-closed).
func TestDispatchBackendRowsMapping(t *testing.T) {
	in := []backends.Backend{
		{
			Name:          "herbert-chat",
			Scope:         "_global",
			Host:          "http://HOST:8089/v1",
			ProviderClass: "llamacpp",
			Locality:      backends.LocalityLocal,
			Roles:         []string{"chat", "dream"},
			Enabled:       true,
			Limits:        map[string]any{"slots": float64(1)},
			APIKey:        "never-projected",
		},
		{
			Name:          "tenant-private",
			Scope:         "acme",
			Host:          "https://api.example.com",
			ProviderClass: "openrouter",
			Locality:      backends.LocalityExternal,
			Enabled:       false, // disabled rows are projected too
		},
		{
			// A03-W3: a peer ctx instance on the LAN is self-admitting too.
			Name:          "peer-ctx",
			Scope:         "_global",
			Host:          "http://peer:8077",
			ProviderClass: backends.ProviderCtx,
			Locality:      backends.LocalityLAN,
			Enabled:       true,
		},
	}
	// The projection is the cross-wave seam: Roles/External feed the MW14
	// Enforcing coverage term, ProviderClass feeds the MW18 external guard.
	// A field silently dropped here degrades those gates without any test
	// in internal/dispatch noticing — this pin is their owner-side anchor.
	want := []dispatch.BackendRow{
		{Name: "herbert-chat", Scope: "_global", BaseURL: "http://HOST:8089/v1", ProviderClass: "llamacpp", Limits: map[string]any{"slots": float64(1)}, Roles: []string{"chat", "dream"}},
		{Name: "tenant-private", Scope: "acme", BaseURL: "https://api.example.com", ProviderClass: "openrouter", External: true, SelfAdmitting: true},
		{Name: "peer-ctx", Scope: "_global", BaseURL: "http://peer:8077", ProviderClass: "ctx", SelfAdmitting: true},
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

// TestDispatchExternalLimitsWritePathAndDerivationAgree pins the two halves
// of the three external limits keys against each other (A03-W3): the backend
// write path refuses a malformed value of exactly the keys the derivation
// reads, the derivation does not offer on the same malformed value (a row
// that bypassed the write path), and the well-formed values projected
// through DispatchBackendRows + DerivePolicy offer the external class.
func TestDispatchExternalLimitsWritePathAndDerivationAgree(t *testing.T) {
	base := func(limits map[string]any, b backends.Backend) backends.Backend {
		b.Name, b.Scope, b.Protocol, b.Trust, b.Enabled, b.Limits = "row", "_global", backends.ProtocolOpenAI, backends.TrustFull, true, limits
		b.Roles = []string{"chat"}
		return b
	}
	lan := backends.Backend{Host: "http://gpu:8089", ProviderClass: backends.ProviderLlamaCpp, Locality: backends.LocalityLAN}
	cloud := backends.Backend{Host: "https://api.example.com", ProviderClass: backends.ProviderGeneric, Locality: backends.LocalityExternal}
	for _, bad := range []struct {
		key string
		val any
	}{
		{"internal_reserved_slots", "two"}, {"internal_reserved_slots", float64(0)}, {"internal_reserved_slots", 1.5},
		{"external_offer", "yes"},
		{"external_max_inflight", float64(0)}, {"external_max_inflight", -1.0},
		// Review 2 F-B, the other direction: a value the derivation refuses
		// above math.MaxInt32 is refused on the write path as well.
		{"internal_reserved_slots", 1e19}, {"internal_reserved_slots", float64(3000000000)},
		{"external_max_inflight", float64(math.MaxInt32 + 1)},
	} {
		b := base(map[string]any{bad.key: bad.val}, lan)
		_, errs := backends.ValidateBackend(&b)
		found := false
		for _, e := range errs {
			if e.Field == "limits."+bad.key {
				found = true
			}
		}
		if !found {
			t.Errorf("write path accepts limits.%s=%v: %v", bad.key, bad.val, errs)
		}
		// The same value on a row that otherwise offers — slot-managed with R
		// for R, self-admitting with the offer for the other two keys.
		row := base(map[string]any{"slots": float64(4), "internal_reserved_slots": float64(2), bad.key: bad.val}, lan)
		origin := "http://gpu:8089"
		if bad.key != "internal_reserved_slots" {
			row = base(map[string]any{"external_offer": true, bad.key: bad.val}, cloud)
			origin = "https://api.example.com"
		}
		d := dispatch.New(nil, dispatch.DefaultSettings())
		d.UpdatePolicy(dispatch.DerivePolicy(DispatchBackendRows([]backends.Backend{row}), slog.New(slog.DiscardHandler)))
		if d.ExternalAdmissible(origin) {
			t.Errorf("derivation offers on limits.%s=%v, which the write path refuses", bad.key, bad.val)
		}
		d.Close()
	}
	l := base(map[string]any{"slots": float64(4), "internal_reserved_slots": float64(2)}, lan)
	c := base(map[string]any{"external_offer": true, "external_max_inflight": float64(3)}, cloud)
	for _, b := range []backends.Backend{l, c} {
		if _, errs := backends.ValidateBackend(&b); len(errs) != 0 {
			for _, e := range errs {
				if strings.HasPrefix(e.Field, "limits.") {
					t.Fatalf("write path refuses well-formed %s: %v", e.Field, errs)
				}
			}
		}
	}
	pol := dispatch.DerivePolicy(DispatchBackendRows([]backends.Backend{l, c}), nil)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(pol)
	if !d.ExternalAdmissible("http://gpu:8089") || !d.ExternalAdmissible("https://api.example.com") {
		t.Fatalf("well-formed external limits do not offer: lan=%v cloud=%v",
			d.ExternalAdmissible("http://gpu:8089"), d.ExternalAdmissible("https://api.example.com"))
	}
}
