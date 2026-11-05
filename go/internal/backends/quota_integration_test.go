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
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
		 )
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 SELECT $1,$2,$3,$4::uuid, p.id FROM p RETURNING id::text`, "qk-"+scope, "qk-"+scope, scope, tenantID).Scan(&keyID); err != nil {
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

// TestQuotaAccountant_TenantWide is the GHSA 2026-10-31 §4 path a): the
// operator set the quota on the tenant's first scope; a tenant-admin minted a
// second scope (scope-create) and a key homed there (api-key-create). The
// second key's calls count towards the tenant's spend — and its scope is now
// gated by the same policy. Before the fix Gate looked the policy up under the
// second scope alone, found no row and passed the full chain.
func TestQuotaAccountant_TenantWide(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	scopeA, _ := seedQuotaTenant(t, pool, "s4-acme", "s4-acme")
	setQuota(t, pool, scopeA, fpv(0.001), nil, "block")
	var tenantID string
	if err := pool.QueryRow(ctx, `SELECT tenant_id::text FROM context_tenant_scopes WHERE scope = $1`, scopeA).Scan(&tenantID); err != nil {
		t.Fatalf("tenant of %s: %v", scopeA, err)
	}
	const scopeB = "s4-acme:side"
	if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scopeB, tenantID); err != nil {
		t.Fatalf("map %s: %v", scopeB, err)
	}
	var keyB string
	if err := pool.QueryRow(ctx,
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
		 )
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 SELECT $1,$2,$3,$4::uuid, p.id FROM p RETURNING id::text`, "qk-side", "qk-side", scopeB, tenantID).Scan(&keyB); err != nil {
		t.Fatalf("key in %s: %v", scopeB, err)
	}
	logCall(t, pool, keyB, 0.5, "external") // spent through the side key

	// An unrelated tenant without a quota row, to prove the policy does not
	// leak across the tenant boundary.
	scopeO, _ := seedQuotaTenant(t, pool, "s4-other", "s4-other")

	a := backends.NewQuotaAccountant(pool, time.Minute)
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, scope := range []string{scopeA, scopeB} {
		_, err := a.Gate(scope, mixedChain())
		var qe *backends.ErrQuotaExceeded
		if err == nil || !errorsAs(err, &qe) || qe.Reason != "cost_budget" {
			t.Errorf("scope %q of the over-budget tenant: want ErrQuotaExceeded/cost_budget, got %v", scope, err)
		}
	}
	if got, err := a.Gate(scopeO, mixedChain()); err != nil || len(got) != 2 {
		t.Errorf("tenant without a quota row must stay ungated: got %d err %v", len(got), err)
	}

	// A second row on the side scope (UNIQUE(scope) allows it): the strictest
	// limit per field binds the whole tenant.
	if _, err := pool.Exec(ctx, `UPDATE context_tenant_quota SET daily_cost_usd = 100, on_exceed = 'external_off' WHERE scope = $1`, scopeA); err != nil {
		t.Fatalf("relax A: %v", err)
	}
	setQuota(t, pool, scopeB, nil, ipv(1), "external_off")
	logCall(t, pool, keyB, 0, "local") // second attributed call → over daily_calls=1
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	for _, scope := range []string{scopeA, scopeB} {
		_, err := a.Gate(scope, mixedChain())
		var qe *backends.ErrQuotaExceeded
		if err == nil || !errorsAs(err, &qe) || qe.Reason != "daily_calls" {
			t.Errorf("scope %q: the side row's daily_calls must bind the tenant, got %v", scope, err)
		}
	}
}

// TestQuotaAccountant_ConsumerRowsNeverCount (W06-4 gate d, K17, B-06-18):
// the internal tenant spend counts knowledge-key rows only. n calls of a /v1
// consumer of the tenant — consumer_id set, api_key_id NULL, external and
// costly — leave daily_calls and the cost budget untouched, so they can
// never lock the tenant's people out of /api/query and the web chat. The
// control half makes the pin non-vacuous: the same n calls on the tenant's
// knowledge key do exhaust daily_calls.
func TestQuotaAccountant_ConsumerRowsNeverCount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const n = 5
	scope, key := seedQuotaTenant(t, pool, "w064-asym", "w064-asym")
	setQuota(t, pool, scope, fpv(0.001), ipv(n), "block")
	const consumer = "0196f0a0-0000-7000-8000-0000000000c1" // fictitious consumer of this tenant
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, consumer_id, cost_usd, backend_locality)
			 VALUES ('gateway-chat','m','h',10,$1::uuid,0.5,'external')`, consumer); err != nil {
			t.Fatalf("consumer call: %v", err)
		}
	}

	a := backends.NewQuotaAccountant(pool, time.Minute)
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	sp, ok := a.SpendForTest(scope)
	if !ok || sp.DayCalls != 0 || sp.DayCost != 0 || sp.MonthCost != 0 {
		t.Errorf("consumer rows counted into the tenant spend: %+v (found %v)", sp, ok)
	}
	if got, err := a.Gate(scope, mixedChain()); err != nil || len(got) != 2 {
		t.Fatalf("consumer traffic must not gate the tenant: got %d backends, err %v", len(got), err)
	}

	for i := 0; i < n; i++ {
		logCall(t, pool, key, 0, "local")
	}
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if sp, _ := a.SpendForTest(scope); sp.DayCalls != n {
		t.Fatalf("control: %d knowledge-key calls counted as %d", n, sp.DayCalls)
	}
	_, err := a.Gate(scope, mixedChain())
	var qe *backends.ErrQuotaExceeded
	if err == nil || !errorsAs(err, &qe) || qe.Reason != "daily_calls" {
		t.Fatalf("control: %d knowledge-key calls must exhaust daily_calls=%d, got %v", n, n, err)
	}
}

// TestQuotaAccountant_Relief (W06-4 gate e): with the accountant's clock
// pinned, each spend figure carries its earliest relief — the oldest row it
// counts + its window — to the microsecond, also when that row sits in a
// rollup hour rather than in the window's raw edge.
func TestQuotaAccountant_Relief(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scope, key := seedQuotaTenant(t, pool, "w064-relief", "w064-relief")
	setQuota(t, pool, scope, fpv(100), ipv(1000), "external_off")
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04:05.999999", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := at("2026-11-03 10:20:00") // since 24 h = 11-02 10:20, since 30 d = 10-04 10:20
	for _, r := range []struct {
		at, cost, locality string
	}{
		{"2026-10-04 10:10:00", "0.3", "external"},        // before the 30-day window
		{"2026-10-04 10:30:00.000005", "0.2", "external"}, // oldest cost of the month, in its raw edge
		{"2026-11-02 10:19:00", "0", "local"},             // before the 24-hour window
		{"2026-11-02 11:07:30.25", "0", "local"},          // oldest call of the day, in a rollup hour
		{"2026-11-02 16:00:00", "0", "external"},          // external without a cost
		{"2026-11-03 08:00:00", "0.1", "external"},        // oldest cost of the day
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, api_key_id, cost_usd, backend_locality)
			 VALUES ($1,'query-synthesize','m','h',10,$2::uuid,$3::numeric,$4)`, at(r.at), key, r.cost, r.locality); err != nil {
			t.Fatalf("log %s: %v", r.at, err)
		}
	}

	a := backends.NewQuotaAccountant(pool, time.Minute)
	a.SetClockForTest(func() time.Time { return now })
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	sp, ok := a.SpendForTest(scope)
	if !ok {
		t.Fatal("tenant missing from the generation")
	}
	want := backends.SpendForTest{
		DayCalls: 3, DayCost: 0.1, MonthCost: 0.3,
		DayCallsRelief:  at("2026-11-03 11:07:30.25"),
		DayCostRelief:   at("2026-11-04 08:00:00"),
		MonthCostRelief: at("2026-11-03 10:30:00.000005"),
	}
	if sp.DayCalls != want.DayCalls || sp.DayCost != want.DayCost || sp.MonthCost != want.MonthCost ||
		!sp.DayCallsRelief.Equal(want.DayCallsRelief) || !sp.DayCostRelief.Equal(want.DayCostRelief) ||
		!sp.MonthCostRelief.Equal(want.MonthCostRelief) {
		t.Fatalf("spend = %+v\nwant    %+v", sp, want)
	}
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
