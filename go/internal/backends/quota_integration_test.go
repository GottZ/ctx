//go:build integration

// Integration test for MT wave T36 (Achse 04-W4): the QuotaAccountant build/
// refresh SQL path + Gate enforcement against a real PG18 testcontainer. The
// accountant resolves a tenant scope → its keys → the rolling external cost SUM
// + attributed call COUNT over context_llm_log, caches it lock-free, and the
// Gate filters/blocks per the context_tenant_quota policy.
//
// External test package (backends_test): internal/testdb → internal/store →
// internal/backends, so an internal-package test importing testdb would cycle.
// It drives only the public surface (NewQuotaAccountant / RefreshNow / Gate).
//
//	go test -tags=integration ./internal/backends/ -run TestQuota -count=1 -v
//	go test -tags=integration -race ./internal/backends/ -run TestQuotaAccountant_Race -count=1
package backends_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedQuotaTenant wires a tenant + scope map + one key and returns (scope, keyID).
func seedQuotaTenant(t *testing.T, pool *pgxpool.Pool, slug, scope string) (string, string) {
	t.Helper()
	ctx := context.Background()
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&tenantID); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("map scope %s: %v", scope, err)
	}
	var keyID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id)
		 VALUES ($1,$2,$3,$4::uuid) RETURNING id::text`, "qk-"+scope, "qk-"+scope, scope, tenantID).Scan(&keyID); err != nil {
		t.Fatalf("insert key for %s: %v", scope, err)
	}
	return scope, keyID
}

func setQuota(t *testing.T, pool *pgxpool.Pool, scope string, dailyCost *float64, dailyCalls *int, onExceed string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_quota (scope, daily_cost_usd, daily_calls, on_exceed)
		 VALUES ($1,$2,$3,$4)`, scope, dailyCost, dailyCalls, onExceed); err != nil {
		t.Fatalf("set quota %s: %v", scope, err)
	}
}

func logCall(t *testing.T, pool *pgxpool.Pool, keyID string, cost float64, locality string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, cost_usd, backend_locality)
		 VALUES ('query-synthesize','m','h',10,$1::uuid,$2,$3)`, keyID, cost, locality); err != nil {
		t.Fatalf("log call: %v", err)
	}
}

func fpv(v float64) *float64 { return &v }
func ipv(v int) *int         { return &v }

func mixedChain() []backends.Backend {
	return []backends.Backend{
		{ID: "loc", Name: "local-gpu", Locality: "local"},
		{ID: "ext", Name: "cloud", Locality: backends.LocalityExternal},
	}
}

func hasBackend(c []backends.Backend, name string) bool {
	for _, b := range c {
		if b.Name == name {
			return true
		}
	}
	return false
}

func TestQuotaAccountant_Enforce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("cost_over_external_off_drops_external", func(t *testing.T) {
		scope, key := seedQuotaTenant(t, pool, "t36-eo", "t36-eo")
		setQuota(t, pool, scope, fpv(0.001), nil, "external_off")
		logCall(t, pool, key, 0.5, "external") // 0.5 >> 0.001 budget

		a := backends.NewQuotaAccountant(pool, time.Minute)
		if err := a.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		got, err := a.Gate(scope, mixedChain())
		if err != nil {
			t.Fatalf("external_off must not error: %v", err)
		}
		if hasBackend(got, "cloud") {
			t.Error("external backend should be dropped over cost budget")
		}
		if !hasBackend(got, "local-gpu") {
			t.Error("local backend must stay reachable")
		}
	})

	t.Run("cost_over_block_errors", func(t *testing.T) {
		scope, key := seedQuotaTenant(t, pool, "t36-blk", "t36-blk")
		setQuota(t, pool, scope, fpv(0.001), nil, "block")
		logCall(t, pool, key, 0.5, "external")

		a := backends.NewQuotaAccountant(pool, time.Minute)
		if err := a.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		_, err := a.Gate(scope, mixedChain())
		var qe *backends.ErrQuotaExceeded
		if err == nil || !errorsAs(err, &qe) || qe.Reason != "cost_budget" {
			t.Fatalf("block over budget should be ErrQuotaExceeded/cost_budget, got %v", err)
		}
	})

	t.Run("call_budget_errors_even_local", func(t *testing.T) {
		scope, key := seedQuotaTenant(t, pool, "t36-call", "t36-call")
		setQuota(t, pool, scope, nil, ipv(1), "external_off")
		logCall(t, pool, key, 0, "local") // one attributed call, local
		logCall(t, pool, key, 0, "local") // a second → over daily_calls=1

		a := backends.NewQuotaAccountant(pool, time.Minute)
		if err := a.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		_, err := a.Gate(scope, []backends.Backend{{ID: "loc", Name: "local-gpu", Locality: "local"}})
		var qe *backends.ErrQuotaExceeded
		if err == nil || !errorsAs(err, &qe) || qe.Reason != "daily_calls" {
			t.Fatalf("call budget should fire even local-only, got %v", err)
		}
	})

	t.Run("no_quota_row_fail_open", func(t *testing.T) {
		scope, key := seedQuotaTenant(t, pool, "t36-open", "t36-open")
		logCall(t, pool, key, 99, "external") // huge spend, but NO quota row

		a := backends.NewQuotaAccountant(pool, time.Minute)
		if err := a.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		got, err := a.Gate(scope, mixedChain())
		if err != nil || len(got) != 2 {
			t.Fatalf("no quota row must fail-open (full chain): got %d err %v", len(got), err)
		}
	})

	t.Run("under_budget_passes", func(t *testing.T) {
		scope, key := seedQuotaTenant(t, pool, "t36-under", "t36-under")
		setQuota(t, pool, scope, fpv(10.0), nil, "external_off")
		logCall(t, pool, key, 0.5, "external") // 0.5 < 10 budget

		a := backends.NewQuotaAccountant(pool, time.Minute)
		if err := a.RefreshNow(ctx); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		got, err := a.Gate(scope, mixedChain())
		if err != nil || len(got) != 2 {
			t.Fatalf("under budget should pass full chain: got %d err %v", len(got), err)
		}
	})
}

// TestQuotaAccountant_Race drives concurrent Gate reads against repeated
// RefreshNow swaps — the lock-free read path must hold under -race.
func TestQuotaAccountant_Race(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scope, key := seedQuotaTenant(t, pool, "t36-race", "t36-race")
	setQuota(t, pool, scope, fpv(0.001), nil, "external_off")
	logCall(t, pool, key, 0.5, "external")

	a := backends.NewQuotaAccountant(pool, time.Millisecond) // tiny TTL → frequent refresh
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = a.Gate(scope, mixedChain()) // triggers ensureFresh swaps
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = a.RefreshNow(ctx)
			}
		}()
	}
	wg.Wait()
}

// errorsAs wraps errors.As for the assertion sites.
func errorsAs(err error, target **backends.ErrQuotaExceeded) bool {
	return errors.As(err, target)
}
