package events

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// T38 (04-W6): the background path runs per iterated tenant with an
// entitlement-clamped read window (read_scopes ∩ TenantScopes), a tenant-visible
// Chain, and a per-tenant scope floor. This file pins the events-package seams:
// the two pure clamps (intersectWindow / effectiveHomeScope), the dream loop's
// read-window threading (runCycle observes the CLAMPED window, not raw
// read_scopes), and newRouter carrying the per-tenant floor.

// TestIntersectWindow pins the §4.4-(a) read-window clamp: read_scopes ∩ owned.
// A nil owned is the unbounded fallback (pre-MT / register error). A foreign
// read_scopes value (a tenant-overridable config the tenant does NOT own) is
// dropped — the cross-tenant-background-read gate. The default tenant's owned =
// {private,shared,work} intersected with the default read_scopes is byte-equal
// (AMENDMENT #3 invariance).
func TestIntersectWindow(t *testing.T) {
	cases := []struct {
		name       string
		readScopes []string
		owned      []string
		want       []string
	}{
		{
			name:       "nil_owned_is_unbounded_fallback",
			readScopes: []string{"private", "shared", "work"},
			owned:      nil,
			want:       []string{"private", "shared", "work"},
		},
		{
			name:       "default_invariant_byte_equal",
			readScopes: []string{"private", "shared", "work"},
			owned:      []string{"private", "shared", "work"},
			want:       []string{"private", "shared", "work"},
		},
		{
			name:       "foreign_read_scope_override_intersects_away",
			readScopes: []string{"tenant-a", "foreign-x"}, // override claims a foreign scope
			owned:      []string{"tenant-a"},
			want:       []string{"tenant-a"}, // foreign-x dropped, leak closed
		},
		{
			name:       "empty_intersection_fails_closed",
			readScopes: []string{"foreign-x"},
			owned:      []string{"tenant-a"},
			want:       []string{}, // reads nothing rather than a foreign scope
		},
		{
			name:       "order_follows_read_scopes",
			readScopes: []string{"work", "private", "shared"},
			owned:      []string{"private", "shared", "work"},
			want:       []string{"work", "private", "shared"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectWindow(tc.readScopes, tc.owned)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("intersectWindow(%v, %v) = %v, want %v", tc.readScopes, tc.owned, got, tc.want)
			}
		})
	}
}

// TestEffectiveHomeScope pins the authoritative home-scope mapping (§4.4): the
// config home_scope when the tenant owns it, else its identity scope (owned[0]).
// The default tenant stays on "private" (owned); a non-default tenant whose
// config home_scope ("private", the base default) it does NOT own falls back to
// its own identity scope, so the digest index / report is never written under a
// foreign scope.
func TestEffectiveHomeScope(t *testing.T) {
	cases := []struct {
		name       string
		configHome string
		owned      []string
		want       string
	}{
		{"nil_owned_verbatim", "private", nil, "private"},
		{"default_owns_private", "private", []string{"private", "shared", "work"}, "private"},
		{"nondefault_unowned_home_falls_back", "private", []string{"tenant-a"}, "tenant-a"},
		{"nondefault_owned_home_kept", "tenant-a", []string{"tenant-a", "tenant-a-dept"}, "tenant-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveHomeScope(tc.configHome, tc.owned); got != tc.want {
				t.Fatalf("effectiveHomeScope(%q, %v) = %q, want %q", tc.configHome, tc.owned, got, tc.want)
			}
		})
	}
}

// captureDreamWindow runs ONE dream cycle for the given iterated tenant and
// returns the read window runDreamLoop handed RunDreamCycle (the runCycle seam's
// 6th arg). The cfgFor overlay supplies the per-tenant config generation; a nil
// cfgFor leaves SnapshotForTenant on base. The loop runs in a goroutine; the
// seam holds the cycle open until the test has captured the window, then the
// loop is cancelled.
func captureDreamWindow(t *testing.T, base *config.Config, bt backgroundTenant, cfgFor func(scope string) *config.Config) []string {
	t.Helper()
	st := config.NewStore(base)
	if cfgFor != nil {
		st.SetOverlay(func(_ context.Context, _ *config.Config, scope string) (*config.Config, error) {
			if c := cfgFor(scope); c != nil {
				return c, nil
			}
			return nil, nil
		})
	}
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{dreamPoolRow("http://dream.example", "dream-model")})
	s := NewScheduler(deadPool(t), st, bpool, StartupConfig{})
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return []backgroundTenant{bt} }

	got := make(chan []string, 1)
	release := make(chan struct{})
	s.runCycle = func(_ context.Context, _ *pgxpool.Pool, _ *dream.Router, _ llm.Options, _ dream.BackoffConfig, readScopes []string, _ dream.Throttle) (int, error) {
		got <- readScopes
		<-release
		return 1, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runDreamLoop(ctx)
		close(done)
	}()

	var window []string
	select {
	case window = <-got:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("no dream cycle within 15s")
	}
	cancel()
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dream loop did not exit after cancel")
	}
	return window
}

// TestDreamLoopClampsReadWindowToEntitlements is the §4.4-(a) cross-tenant
// background-read gate: a tenant whose scheduler.read_scopes override claims a
// FOREIGN scope must NOT read it — the dream cycle's window is read_scopes ∩
// owned. RED arm (runDreamLoop passing raw cfg.Scheduler.ReadScopes instead of
// intersectWindow): the window would carry "foreign-x" — a cross-tenant leak.
func TestDreamLoopClampsReadWindowToEntitlements(t *testing.T) {
	base := captureTestConfig(t, 12)
	window := captureDreamWindow(t,
		base,
		backgroundTenant{scope: "tenant-a", owned: []string{"tenant-a"}},
		func(scope string) *config.Config {
			if scope != "tenant-a" {
				return nil
			}
			c := captureTestConfig(t, 12)
			c.Scheduler.ReadScopes = []string{"tenant-a", "foreign-x"} // override grabs a foreign scope
			return c
		},
	)
	if slices.Contains(window, "foreign-x") {
		t.Fatalf("dream read window = %v, must NOT contain the foreign scope (cross-tenant background-read leak)", window)
	}
	if !slices.Equal(window, []string{"tenant-a"}) {
		t.Fatalf("dream read window = %v, want [tenant-a] (read_scopes ∩ owned)", window)
	}
}

// TestDreamLoopDefaultWindowByteIdentical is the AMENDMENT-#3 invariance: at one
// tenant (default), the window stays {private,shared,work} — byte-identical to
// the pre-T13 global run. owned = TenantScopes(default) = {private,shared,work}
// (migration 059), read_scopes default = {private,shared,work}. RED arm (the
// DISTINCT-home_scope mistake, owned = {private}): shared+work would drop out of
// the dream/digest cadence.
func TestDreamLoopDefaultWindowByteIdentical(t *testing.T) {
	base := captureTestConfig(t, 12)
	base.Scheduler.ReadScopes = []string{"private", "shared", "work"}
	window := captureDreamWindow(t,
		base,
		// default tenant: config scope _global (SnapshotForTenant -> base), owned
		// is the real entitlement set 059 backfills.
		backgroundTenant{scope: store.GlobalScope, owned: []string{"private", "shared", "work"}},
		nil,
	)
	if !slices.Equal(window, []string{"private", "shared", "work"}) {
		t.Fatalf("default-tenant dream window = %v, want [private shared work] (byte-identical legacy cadence)", window)
	}
}

// TestNewRouterCarriesPerTenantFloor pins §4.4-(c): the router built for an
// iterated tenant carries THAT tenant's scope-sensitivity floor (from
// SnapshotForTenant), raising only that tenant's own blocks. A foreign-scope
// block is untouched (the floor is keyed by scope). RED arm (newRouter built
// from base Snapshot() rather than the per-tenant generation): the tenant floor
// is absent and the block is not raised.
func TestNewRouterCarriesPerTenantFloor(t *testing.T) {
	base := captureTestConfig(t, 12) // empty floor
	tcfg := captureTestConfig(t, 12)
	tcfg.Pool.ScopeSensitivityFloor = config.ScopeFloor{"tenant-a": backends.SensPersonal}

	st := config.NewStore(base)
	st.SetOverlay(func(_ context.Context, _ *config.Config, scope string) (*config.Config, error) {
		if scope == "tenant-a" {
			return tcfg, nil
		}
		return nil, nil
	})
	s := NewScheduler(deadPool(t), st, backends.NewPool(nil, nil), StartupConfig{})
	ctx := context.Background()

	// Per-tenant router: tenant-a's floor raises a tenant-a-scope block to personal.
	rt := s.newRouter(st.SnapshotForTenant(ctx, "tenant-a"), "tenant-a")
	if got := rt.FloorSens(backends.SensPublic, "tenant-a"); got != backends.SensPersonal {
		t.Errorf("tenant-a floor: FloorSens(public, tenant-a) = %q, want personal", got)
	}
	// The floor is scope-keyed: a foreign-scope block under tenant-a's router is NOT raised.
	if got := rt.FloorSens(backends.SensPublic, "tenant-b"); got != backends.SensPublic {
		t.Errorf("tenant-a floor must not raise a tenant-b block: got %q, want public", got)
	}
	if rt.Tenant != "tenant-a" {
		t.Errorf("newRouter tenant = %q, want tenant-a (the iterated egress scope)", rt.Tenant)
	}

	// RED arm: a router built from the BASE generation has the empty base floor.
	rb := s.newRouter(st.Snapshot(), store.GlobalScope)
	if got := rb.FloorSens(backends.SensPublic, "tenant-a"); got != backends.SensPublic {
		t.Errorf("base floor must be empty: FloorSens(public, tenant-a) = %q, want public", got)
	}
}
