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
// read path never touches the DB).
func seedQuota(states map[string]tenantState) *QuotaAccountant {
	a := NewQuotaAccountant(nil, time.Minute)
	fixed := time.Unix(1_700_000_000, 0)
	a.nowFn = func() time.Time { return fixed }
	gen := states
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

// TestQuotaGateFailOpen: a scope with no quota row, a disabled policy, or an
// empty/_global scope passes the chain through unchanged (fail-open — the
// fail-closed axis is egress visibility, not cost).
func TestQuotaGateFailOpen(t *testing.T) {
	a := seedQuota(map[string]tenantState{
		"disabled-t": {quota: TenantQuota{DailyCostUSD: fp(0.0), OnExceed: QuotaExceedBlock, Enabled: false}, dayCost: 99},
	})
	cases := []string{"unknown-tenant", "disabled-t", "", GlobalScope}
	for _, scope := range cases {
		got, err := a.Gate(scope, chainLocalExternal())
		if err != nil || len(got) != 2 {
			t.Errorf("scope %q should fail-open (full chain, no error): got %v err %v", scope, qNames(got), err)
		}
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
