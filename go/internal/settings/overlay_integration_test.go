//go:build integration

// 06-C2 integration probe: the TenantOverlay closure end-to-end against a real
// PG18 testcontainer — it is the consumer that wires loadTenantOverrideRows +
// BuildFromRows into the config.TenantOverlay value the Store injects at 06-C3.
//
// Run with:
//
//	go test -tags=integration ./internal/settings/ -run TestTenantOverlay_Integration -count=1 -v
package settings

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantOverlay_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	resetEnv(t)

	base, baseIssues := envBuild(t)
	if config.HasErrors(baseIssues) {
		t.Fatalf("env base must validate: %v", baseIssues)
	}
	overlay := TenantOverlay(pool)

	const (
		key    = "rerank.blend_weight"
		tenant = "work"
		other  = "sales"
	)

	// The Store passes the live base to the overlay; the overlay returns it
	// VERBATIM when the tenant has no own rows (folding _global into base is
	// Reload's job, not the overlay's). Pointer identity is the "inherited base,
	// no tenant generation" signal — the Store then caches the base pointer
	// cheaply instead of a redundant full generation.
	t.Run("no rows at all ⇒ inherits base pointer", func(t *testing.T) {
		cfg, err := overlay(ctx, base, tenant)
		if err != nil {
			t.Fatalf("overlay: %v", err)
		}
		if cfg != base {
			t.Errorf("a tenant with no own rows must inherit the base pointer unchanged")
		}
	})

	t.Run("_global row but no tenant row ⇒ still inherits base", func(t *testing.T) {
		upsertScopeIT(t, pool, key, store.GlobalScope, `0.4`)
		cfg, err := overlay(ctx, base, tenant)
		if err != nil {
			t.Fatalf("overlay: %v", err)
		}
		if cfg != base {
			t.Errorf("a _global-only row must not create a tenant generation (hasScope keys on the tenant)")
		}
	})

	t.Run("own row ⇒ tenant generation, tenant wins, Source=tenant", func(t *testing.T) {
		upsertScopeIT(t, pool, key, tenant, `0.6`)
		cfg, err := overlay(ctx, base, tenant)
		if err != nil {
			t.Fatalf("overlay: %v", err)
		}
		if cfg == base {
			t.Fatalf("a tenant with own rows must get a distinct generation")
		}
		if cfg.Rerank.BlendWeight != 0.6 || cfg.Source(key) != config.SourceTenant {
			t.Errorf("tenant override must win + be attributed: got %v source %q, want 0.6/tenant",
				cfg.Rerank.BlendWeight, cfg.Source(key))
		}
	})

	t.Run("a different tenant is unaffected ⇒ inherits base", func(t *testing.T) {
		cfg, err := overlay(ctx, base, other)
		if err != nil {
			t.Fatalf("overlay: %v", err)
		}
		if cfg != base {
			t.Errorf("a tenant with no own rows must inherit base even when ANOTHER tenant has rows")
		}
	})
}

// TestStoreOverlayWiring_Integration is the 06-C3 gate: the boot wiring
// config.Store.SetOverlay(settings.TenantOverlay(pool)) (cmd/ctxd/main.go) end
// to end. With the real overlay installed, SnapshotForTenant resolves a tenant's
// context_settings row into a real per-tenant generation; SnapshotForRequest
// still returns base (the request-scope hook is wired in a later wave, C5); and
// an overlay over a dead pool fails safe to base without a panic (§5.6 base
// fallback / §10.6 fail-open interim, never a panic-on-error).
func TestStoreOverlayWiring_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	resetEnv(t)

	base, baseIssues := envBuild(t)
	if config.HasErrors(baseIssues) {
		t.Fatalf("env base must validate: %v", baseIssues)
	}
	const (
		key    = "rerank.blend_weight"
		tenant = "work"
	)
	upsertScopeIT(t, pool, key, tenant, `0.6`)
	if base.Rerank.BlendWeight == 0.6 {
		t.Fatalf("fixture: base must differ from the tenant value")
	}

	st := config.NewStore(base)
	st.SetOverlay(TenantOverlay(pool))

	cfg := st.SnapshotForTenant(ctx, tenant)
	if cfg.Rerank.BlendWeight != 0.6 || cfg.Source(key) != config.SourceTenant {
		t.Errorf("wired overlay must resolve the tenant row: got %v source %q, want 0.6/tenant",
			cfg.Rerank.BlendWeight, cfg.Source(key))
	}

	// The request path is NOT wired yet (no requestScopeHook until C5) ⇒ base.
	if got := st.SnapshotForRequest(ctx); got != base {
		t.Errorf("SnapshotForRequest must still return base until C5 wires the scope hook, got %p", got)
	}

	// A dead-pool overlay fails safe to base, never panics (§5.6).
	dead := config.NewStore(base)
	dead.SetOverlay(TenantOverlay(deadPool(t)))
	if got := dead.SnapshotForTenant(ctx, tenant); got != base {
		t.Errorf("overlay build error must fall back to base, got %p", got)
	}
}
