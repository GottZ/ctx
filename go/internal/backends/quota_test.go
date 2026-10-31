package backends

import (
	"errors"
	"testing"
	"time"
)

// MT T36 (04-W4): the QuotaAccountant Gate decision logic, tested DB-less by
// seeding the lock-free cache directly. The build/refresh SQL path is proved
// end-to-end against PG18 in quota_integration_test.go.

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

// seedQuota builds an accountant with a pre-populated (fresh) cache so Gate reads
// it without triggering a refresh (q is nil — a refresh would panic, proving the
// read path never touches the DB). Each key is a scope that is the only scope of
// its own tenant ("t/"+scope).
func seedQuota(states map[string]tenantState) *QuotaAccountant {
	gen := quotaGen{scopeTenant: map[string]string{}, tenants: map[string]tenantState{}}
	for scope, st := range states {
		gen.scopeTenant[scope] = "t/" + scope
		gen.tenants["t/"+scope] = st
	}
	return seedGen(gen)
}

// seedGen stores an explicit generation (scope→tenant map + tenant states).
func seedGen(gen quotaGen) *QuotaAccountant {
	a := NewQuotaAccountant(nil, time.Minute)
	fixed := time.Unix(1_700_000_000, 0)
	a.nowFn = func() time.Time { return fixed }
	a.cache.Store(&gen)
	a.cacheAt.Store(fixed.UnixNano())
	return a
}

func chainLocalExternal() []Backend {
	return []Backend{
		{ID: "loc", Name: "local-gpu", Locality: "local"},
		{ID: "ext", Name: "cloud", Locality: LocalityExternal},
	}
}

func qNames(c []Backend) []string {
	out := make([]string, len(c))
	for i, b := range c {
		out[i] = b.Name
	}
	return out
}

func hasName(c []Backend, name string) bool {
	for _, b := range c {
		if b.Name == name {
			return true
		}
	}
	return false
}

// TestQuotaGateCostExternalOff: cost over budget with external_off drops external
// but keeps local (cost never locks the own GPU). Under budget passes unchanged.
func TestQuotaGateCostExternalOff(t *testing.T) {
	a := seedQuota(map[string]tenantState{
		"tenant-a": {quota: TenantQuota{DailyCostUSD: fp(1.0), OnExceed: QuotaExceedExternalOff, Enabled: true}, dayCost: 2.0},
	})
	got, err := a.Gate("tenant-a", chainLocalExternal())
	if err != nil {
		t.Fatalf("external_off should not error: %v", err)
	}
	if hasName(got, "cloud") {
		t.Errorf("external backend should be dropped over budget: %v", qNames(got))
	}
	if !hasName(got, "local-gpu") {
		t.Errorf("local backend must stay reachable (cost never locks the GPU): %v", qNames(got))
	}
}

// TestQuotaGateCostBlock: cost over budget with on_exceed=block hard-errors.
func TestQuotaGateCostBlock(t *testing.T) {
	a := seedQuota(map[string]tenantState{
		"tenant-a": {quota: TenantQuota{MonthlyCostUSD: fp(10.0), OnExceed: QuotaExceedBlock, Enabled: true}, monthCost: 11.0},
	})
	_, err := a.Gate("tenant-a", chainLocalExternal())
	var qe *ErrQuotaExceeded
	if err == nil || !asQuota(err, &qe) || qe.Reason != "cost_budget" {
		t.Fatalf("block over cost budget should be ErrQuotaExceeded/cost_budget, got %v", err)
	}
}

// TestQuotaGateUnderBudget: under budget passes the full chain unchanged.
func TestQuotaGateUnderBudget(t *testing.T) {
	a := seedQuota(map[string]tenantState{
		"tenant-a": {quota: TenantQuota{DailyCostUSD: fp(5.0), OnExceed: QuotaExceedExternalOff, Enabled: true}, dayCost: 1.0},
	})
	got, err := a.Gate("tenant-a", chainLocalExternal())
	if err != nil || len(got) != 2 {
		t.Fatalf("under budget should pass full chain: got %v err %v", qNames(got), err)
	}
}

// TestQuotaGateCallBudget: the call budget hard-errors on EVERY backend, even a
// local-only chain (a call cap that skipped local would be toothless, §4.5).
func TestQuotaGateCallBudget(t *testing.T) {
	a := seedQuota(map[string]tenantState{
		"tenant-a": {quota: TenantQuota{DailyCalls: ip(1), OnExceed: QuotaExceedExternalOff, Enabled: true}, dayCalls: 1},
	})
	// local-only chain — the call budget must still fire.
	_, err := a.Gate("tenant-a", []Backend{{ID: "loc", Name: "local-gpu", Locality: "local"}})
	var qe *ErrQuotaExceeded
	if err == nil || !asQuota(err, &qe) || qe.Reason != "daily_calls" {
		t.Fatalf("call budget should be ErrQuotaExceeded/daily_calls even local-only, got %v", err)
	}
}

// TestQuotaGateFailOpen: a scope that maps to no tenant, a scope whose tenant
// has no quota row, a disabled policy, or an empty/_global scope passes the
// chain through unchanged (fail-open — the fail-closed axis is egress
// visibility, not cost).
func TestQuotaGateFailOpen(t *testing.T) {
	a := seedGen(quotaGen{
		scopeTenant: map[string]string{
			"disabled-t":  "tenant-d",
			"no-policy-t": "tenant-n", // mapped, but tenant-n has no state
		},
		tenants: map[string]tenantState{
			"tenant-d": {quota: TenantQuota{DailyCostUSD: fp(0.0), OnExceed: QuotaExceedBlock, Enabled: false}, dayCost: 99},
		},
	})
	cases := []string{"scope-without-tenant", "no-policy-t", "disabled-t", "", GlobalScope}
	for _, scope := range cases {
		got, err := a.Gate(scope, chainLocalExternal())
		if err != nil || len(got) != 2 {
			t.Errorf("scope %q should fail-open (full chain, no error): got %v err %v", scope, qNames(got), err)
		}
	}
}

// TestQuotaGateTenantWide is the GHSA 2026-10-31 §4 probe: the quota row sits
// on scope A, the caller's key is homed in scope B of the SAME tenant. The
// policy binds the tenant, so B is gated exactly like A. Before the fix Gate
// looked the policy up under B alone, found none and passed the full chain.
func TestQuotaGateTenantWide(t *testing.T) {
	gen := quotaGen{
		scopeTenant: map[string]string{"acme": "tenant-acme", "acme:side": "tenant-acme", "other": "tenant-other"},
		tenants: map[string]tenantState{
			"tenant-acme": {quota: TenantQuota{DailyCalls: ip(10), DailyCostUSD: fp(1.0), OnExceed: QuotaExceedBlock, Enabled: true}, dayCost: 2.0},
		},
	}
	a := seedGen(gen)
	for _, scope := range []string{"acme", "acme:side"} {
		_, err := a.Gate(scope, chainLocalExternal())
		var qe *ErrQuotaExceeded
		if err == nil || !asQuota(err, &qe) || qe.Reason != "cost_budget" {
			t.Errorf("scope %q of the over-budget tenant: want ErrQuotaExceeded/cost_budget, got %v", scope, err)
		}
	}
	// A different tenant without a policy is not touched by acme's row.
	if got, err := a.Gate("other", chainLocalExternal()); err != nil || len(got) != 2 {
		t.Errorf("foreign tenant must stay ungated: got %v err %v", qNames(got), err)
	}
}

// TestStrictestQuota: several rows of one tenant merge field-wise to the
// strictest limit; nil (unlimited) loses to any limit; block beats external_off.
func TestStrictestQuota(t *testing.T) {
	got := StrictestQuota(
		TenantQuota{DailyCostUSD: fp(5), DailyCalls: nil, OnExceed: QuotaExceedExternalOff, Enabled: true},
		TenantQuota{DailyCostUSD: fp(2), MonthlyCostUSD: fp(30), DailyCalls: ip(100), OnExceed: QuotaExceedBlock, Enabled: true},
		TenantQuota{DailyCostUSD: nil, DailyCalls: ip(50), OnExceed: QuotaExceedExternalOff, Enabled: true},
	)
	if got.DailyCostUSD == nil || *got.DailyCostUSD != 2 {
		t.Errorf("daily_cost_usd = %v, want 2", got.DailyCostUSD)
	}
	if got.MonthlyCostUSD == nil || *got.MonthlyCostUSD != 30 {
		t.Errorf("monthly_cost_usd = %v, want 30", got.MonthlyCostUSD)
	}
	if got.DailyCalls == nil || *got.DailyCalls != 50 {
		t.Errorf("daily_calls = %v, want 50", got.DailyCalls)
	}
	if got.OnExceed != QuotaExceedBlock || !got.Enabled {
		t.Errorf("on_exceed/enabled = %q/%v, want block/true", got.OnExceed, got.Enabled)
	}
	if z := StrictestQuota(); z.Enabled {
		t.Error("no policy must merge to a disabled (unlimited) zero value")
	}
}

// TestQuotaGateColdCache: a never-refreshed accountant fails open (it must never
// block before the first refresh lands).
func TestQuotaGateColdCache(t *testing.T) {
	a := NewQuotaAccountant(nil, time.Minute)
	a.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	// cache never stored; ensureFresh would CAS+spawn, but we only assert the
	// read result: nil cache → fail-open. Pre-set refresh=true so ensureFresh
	// does not spawn a goroutine against the nil querier.
	a.refresh.Store(true)
	got, err := a.Gate("tenant-a", chainLocalExternal())
	if err != nil || len(got) != 2 {
		t.Fatalf("cold cache must fail-open: got %v err %v", qNames(got), err)
	}
}

// TestQuotaCacheTTLNoRefetch: a Gate within the TTL window must NOT trigger a
// refresh — the cached generation is reused (no second cost SUM, §6.2). Proven
// by the unchanged cacheAt + the un-set refresh flag (ensureFresh returns before
// the CAS when fresh; nil querier here would panic if a refresh did spawn).
func TestQuotaCacheTTLNoRefetch(t *testing.T) {
	a := seedQuota(map[string]tenantState{
		"tenant-a": {quota: TenantQuota{DailyCostUSD: fp(1.0), OnExceed: QuotaExceedExternalOff, Enabled: true}, dayCost: 0.1},
	})
	at1 := a.cacheAt.Load()
	for i := 0; i < 5; i++ {
		_, _ = a.Gate("tenant-a", chainLocalExternal())
	}
	if a.cacheAt.Load() != at1 {
		t.Fatal("cacheAt changed: a refresh ran within the TTL window (second SUM)")
	}
	if a.refresh.Load() {
		t.Fatal("ensureFresh won a CAS within the TTL window (would re-SUM)")
	}
}

// asQuota wraps errors.As for the assertion sites.
func asQuota(err error, target **ErrQuotaExceeded) bool {
	return errors.As(err, target)
}
