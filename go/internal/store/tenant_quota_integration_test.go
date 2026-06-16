//go:build integration

// Integration test for MT wave T33b (Achse 04-W1, second half): the per-tenant
// quota/budget schema (migration 063) — the policy table + accounting/rate-limit
// access paths that the quota enforcement (04-W4) and cost-attribution (04-W3)
// waves consume. No consumer yet: a missing quota row means "no limit"
// (fail-OPEN by design — the fail-CLOSED axis is egress visibility, not the cost
// budget), so applying the table changes nothing.
//
// pgCode is declared in tenants_hybrid_integration_test.go (same store_test
// package) and reused.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantQuota -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantQuotaMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Table + its UNIQUE scope constraint.
	var hasTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('context_tenant_quota') IS NOT NULL`).Scan(&hasTable); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if !hasTable {
		t.Fatal("migration 063 did not create context_tenant_quota")
	}
	var hasUq bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='uq_tenant_quota_scope')`).Scan(&hasUq); err != nil {
		t.Fatalf("check uq: %v", err)
	}
	if !hasUq {
		t.Error("uq_tenant_quota_scope missing")
	}

	// The accounting / rate-limit indices.
	for _, idx := range []string{"idx_llm_log_apikey", "idx_llm_log_cost", "idx_access_log_ratelimit"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname=$1)`, idx).Scan(&exists); err != nil {
			t.Fatalf("check index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("migration 063 index %s missing", idx)
		}
	}

	// The NOTIFY trigger (hot-reload of quota policy on the settings channel).
	var hasTrigger bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='trg_tenant_quota_notify')`).Scan(&hasTrigger); err != nil {
		t.Fatalf("check trigger: %v", err)
	}
	if !hasTrigger {
		t.Error("trg_tenant_quota_notify missing")
	}

	// 2x-idempotent: a second migration run is a no-op (per-version skip).
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations (idempotency): %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 63`).Scan(&count); err != nil {
		t.Fatalf("count migration 63: %v", err)
	}
	if count != 1 {
		t.Errorf("_migrations version 63 rows = %d, want exactly 1 (idempotent)", count)
	}
}

func TestTenantQuota_Constraints(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// A row with only a scope is valid: NULL budgets = unlimited (fail-open).
	if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_quota (scope) VALUES ('q-a')`); err != nil {
		t.Fatalf("insert minimal quota (NULL budgets should be fine): %v", err)
	}

	// on_exceed is CHECK-constrained.
	_, err := pool.Exec(ctx, `INSERT INTO context_tenant_quota (scope, on_exceed) VALUES ('q-bad', 'nope')`)
	if code := pgCode(err); code != "23514" {
		t.Fatalf("invalid on_exceed: code = %q, want 23514 (CHECK)", code)
	}

	// One quota row per tenant (UNIQUE scope).
	_, err = pool.Exec(ctx, `INSERT INTO context_tenant_quota (scope) VALUES ('q-a')`)
	if code := pgCode(err); code != "23505" {
		t.Fatalf("duplicate scope: code = %q, want 23505 (uq_tenant_quota_scope)", code)
	}
}
