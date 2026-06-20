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
