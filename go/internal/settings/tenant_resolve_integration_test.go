//go:build integration

// MT3-W3 integration probe (internal package — exercises the unexported
// loadTenantOverrideRows) against a real PG18 testcontainer: seed a _global
// and a tenant-scope row of the same key, prove loadTenantOverrideRows reads
// BOTH scopes in one query, and that the Go merge resolves the tenant value as
// the winner.
//
// Run with:
//
//	go test -tags=integration ./internal/settings/ -run TestLoadTenantOverrideRows -count=1 -v
package settings

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func upsertScopeIT(t *testing.T, pool *pgxpool.Pool, key, scope, jsonVal string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, key, scope, json.RawMessage(jsonVal), nil); err != nil {
		t.Fatalf("upsert %s/%s: %v", key, scope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestLoadTenantOverrideRows_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	resetEnv(t)

	const (
		key    = "rerank.blend_weight" // float in [0,1], no cross-field constraint
		tenant = "work"                // a real (non-'_'-prefixed) home_scope shape
	)
	upsertScopeIT(t, pool, key, store.GlobalScope, `0.4`)
	upsertScopeIT(t, pool, key, tenant, `0.6`)

	rows, err := loadTenantOverrideRows(ctx, pool, tenant)
	if err != nil {
		t.Fatalf("loadTenantOverrideRows: %v", err)
	}

	// Both scopes come back in one query.
	scopes := map[string]bool{}
	for _, r := range rows {
		if r.Key == key {
			scopes[r.Scope] = true
		}
	}
	if !scopes[store.GlobalScope] || !scopes[tenant] {
		t.Fatalf("loadTenantOverrideRows must return both scopes for %q, got rows %+v", key, rows)
	}

	// The Go merge with {_global, tenant} resolves tenant as the winner.
	cfg, issues := buildWith(rows, nil, []string{store.GlobalScope, tenant})
	if config.HasErrors(issues) {
		t.Fatalf("build must stay error-free: %v", issues)
	}
	if cfg.Rerank.BlendWeight != 0.6 || cfg.Source(key) != "settings" {
		t.Errorf("tenant value must win: got %v source %q, want 0.6/settings",
			cfg.Rerank.BlendWeight, cfg.Source(key))
	}
}
