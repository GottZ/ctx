//go:build integration

// Integration test for Multi-Tenant wave T32 (03-W6 scope-carried lazy cache
// invalidation). Two gates against a real PG18 testcontainer:
//
//	(1) Migration 065 — notify_settings_write() carries the changed row's scope
//	    in the ctx_settings_write NOTIFY payload. RED before 065: the 051 payload
//	    is {entity,key,op} with no scope, so the LISTEN probe sees scope absent.
//
//	(2) listener dispatch — SettingsWriteHandler routes by scope: a tenant-scope
//	    write drops ONLY that tenant's cached config generation
//	    (config.Store.InvalidateTenant), while a _global / reserved / absent
//	    scope does the full settings.Reload (base rebuild + Replace-wipe of every
//	    tenant generation). RED before the dispatch: today EVERY write goes
//	    through settings.Reload, so a tenant-A write Replace-wipes tenant-B's
//	    cached generation too (tenant-B then rebuilds on its next access — the
//	    probe catches the extra build).
//
// The "0 synchronous builds in the NOTIFY handler" gate (design 03 §6.3) is
// enforced by a counting TenantOverlay: neither branch calls the overlay
// synchronously inside HandleNotification (Replace wipes O(1), InvalidateTenant
// deletes O(1); both rebuild lazily on the next per-tenant Snapshot).
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestSettingsNotifyScope -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// envBaseConfig builds a valid env-only base generation so the _global Reload
// path's Replace (which re-validates) succeeds — mirrors the env reset of
// reload_integration_test.go.
func envBaseConfig(t *testing.T) *config.Config {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	c, issues := config.FromEnv()
	issues = append(issues, config.Validate(c)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	return c
}

// settingsNotify crafts a ctx_settings_write payload for the given scope as the
// 065 trigger would emit it (the conn arg of HandleNotification is unused on the
// settings path, so the test passes nil).
func settingsNotify(scope string) *pgconn.Notification {
	payload, _ := json.Marshal(map[string]string{
		"entity": "context_settings",
		"key":    "query.score_threshold",
		"scope":  scope,
		"op":     "UPDATE",
	})
	return &pgconn.Notification{Channel: channelSettingsWrite, Payload: string(payload)}
}

func TestSettingsNotifyScope_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	base := envBaseConfig(t)

	// (1) Migration 065: the NOTIFY payload carries the row's scope.
	t.Run("payload_carries_scope", func(t *testing.T) {
		lc, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire LISTEN conn: %v", err)
		}
		defer lc.Release()
		if _, err := lc.Exec(ctx, "LISTEN "+channelSettingsWrite); err != nil {
			t.Fatalf("LISTEN: %v", err)
		}

		const tenantScope = "t32-acme"
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3)`,
			"query.score_threshold", tenantScope, json.RawMessage(`"0.5"`)); err != nil {
			t.Fatalf("insert tenant setting: %v", err)
		}

		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := lc.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("WaitForNotification: %v", err)
		}
		var p struct{ Entity, Key, Scope, Op string }
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", n.Payload, err)
		}
		if p.Scope != tenantScope { // RED pre-065: scope absent → ""
			t.Errorf("NOTIFY payload scope = %q, want %q (payload=%s)", p.Scope, tenantScope, n.Payload)
		}
		if p.Entity != "context_settings" {
			t.Errorf("entity = %q, want context_settings", p.Entity)
		}
	})

	// (2) listener dispatch. One counting overlay; prime two tenant generations.
	var builds atomic.Int64
	cfg := config.NewStore(base)
	cfg.SetOverlay(func(_ context.Context, b *config.Config, _ string) (*config.Config, error) {
		builds.Add(1)
		c := *b // a distinct generation per build, derived from the live base
		return &c, nil
	})
	const scopeA, scopeB = "t32-tenant-a", "t32-tenant-b"
	cfg.SnapshotForTenant(ctx, scopeA)
	cfg.SnapshotForTenant(ctx, scopeB)
	if got := builds.Load(); got != 2 {
		t.Fatalf("priming: builds = %d, want 2", got)
	}
	h := NewSettingsWriteHandler(pool, cfg, nil)

	t.Run("tenant_write_drops_only_that_tenant", func(t *testing.T) {
		before := builds.Load()
		if err := h.HandleNotification(ctx, settingsNotify(scopeA), nil); err != nil {
			t.Fatalf("HandleNotification(tenant-A): %v", err)
		}
		// 0 synchronous builds inside the handler.
		if got := builds.Load() - before; got != 0 {
			t.Fatalf("synchronous builds in handler = %d, want 0", got)
		}
		// tenant B SURVIVES: its Snapshot is a cache HIT (no rebuild). RED today:
		// settings.Reload Replace-wiped B, so this access rebuilds (+1).
		cfg.SnapshotForTenant(ctx, scopeB)
		if got := builds.Load() - before; got != 0 {
			t.Errorf("tenant-B rebuilt after a tenant-A write (delta %d) — A's write wiped B", got)
		}
		// tenant A was invalidated: its Snapshot MISSES and rebuilds (+1).
		cfg.SnapshotForTenant(ctx, scopeA)
		if got := builds.Load() - before; got != 1 {
			t.Errorf("tenant-A not invalidated: build delta = %d, want 1", got)
		}
	})

	t.Run("global_write_reloads_base_and_wipes_all", func(t *testing.T) {
		// Re-prime both tenant generations (the previous subtest left A rebuilt,
		// B as-was; force a known-good two-entry cache at the current baseGen).
		cfg.SnapshotForTenant(ctx, scopeA)
		cfg.SnapshotForTenant(ctx, scopeB)
		// A _global override the Reload must pick up into the base generation.
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3)
			   ON CONFLICT (key, scope) DO UPDATE SET value = EXCLUDED.value`,
			"rerank.blend_weight", store.GlobalScope, json.RawMessage(`"0.33"`)); err != nil {
			t.Fatalf("insert _global setting: %v", err)
		}
		before := builds.Load()
		if err := h.HandleNotification(ctx, settingsNotify(store.GlobalScope), nil); err != nil {
			t.Fatalf("HandleNotification(_global): %v", err)
		}
		// 0 synchronous per-tenant builds: Replace wipes O(1); the base rebuild is
		// config.Build, not the overlay (design 03 §6.3 — no eager N-fold rebuild).
		if got := builds.Load() - before; got != 0 {
			t.Fatalf("synchronous overlay builds on _global write = %d, want 0", got)
		}
		// The base WAS reloaded: the _global override is now live in the base.
		// RED if a _global write only invalidated _global without rebuilding base.
		if got := cfg.Snapshot().Rerank.BlendWeight; got != 0.33 {
			t.Errorf("base not reloaded on _global write: blend_weight = %v, want 0.33", got)
		}
		// ALL tenant generations were wiped: each Snapshot MISSES (+1 build each).
		cfg.SnapshotForTenant(ctx, scopeA)
		cfg.SnapshotForTenant(ctx, scopeB)
		if got := builds.Load() - before; got != 2 {
			t.Errorf("not all tenant generations wiped on _global write: delta = %d, want 2", got)
		}
	})
}
