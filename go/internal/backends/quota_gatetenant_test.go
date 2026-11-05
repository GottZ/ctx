package backends

import (
	"errors"
	"testing"
	"time"
)

// W02-07 G8 (masterplan K17, design/02 §5 B21): GateTenant is Gate keyed by
// the tenant id. An empty tenant is ErrNoTenant and never the chain; a tenant
// is gated without any scope mapping; fail-open otherwise exactly like Gate.
// Gate(scope) itself is unchanged (control).

// gateTenantGen is a fresh generation holding tenant states keyed by tenant
// id only — no scope maps to them, so Gate(scope) cannot reach them.
func gateTenantGen(states map[string]tenantState) *QuotaAccountant {
	return seedGen(quotaGen{scopeTenant: map[string]string{}, tenants: states})
}

func TestGateTenantEmptyTenantIsErrNoTenant(t *testing.T) {
	// An enabled tenant without any limit would pass the chain — "" must not.
	a := gateTenantGen(map[string]tenantState{"": {quota: TenantQuota{Enabled: true}}})
	got, err := a.GateTenant("", chainLocalExternal())
	if !errors.Is(err, ErrNoTenant) || got != nil {
		t.Fatalf(`GateTenant("") = %v, %v; want nil, ErrNoTenant`, qNames(got), err)
	}
	// Cold accountant (nil querier, no refresh pre-set): "" returns before
	// the cache is touched — a refresh against the nil querier would panic.
	cold := NewQuotaAccountant(nil, time.Minute)
	if got, err := cold.GateTenant("", chainLocalExternal()); !errors.Is(err, ErrNoTenant) || got != nil {
		t.Fatalf(`cold GateTenant("") = %v, %v; want nil, ErrNoTenant`, qNames(got), err)
	}
	if cold.refresh.Load() || cold.cacheAt.Load() != 0 {
		t.Fatal(`GateTenant("") started a refresh`)
	}
}

func TestGateTenantColdCacheFailOpen(t *testing.T) {
	a := NewQuotaAccountant(nil, time.Minute)
	a.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	a.refresh.Store(true) // no refresh goroutine against the nil querier
	got, err := a.GateTenant("t1", chainLocalExternal())
	if err != nil || len(got) != 2 {
		t.Fatalf("cold cache must fail-open like Gate: got %v err %v", qNames(got), err)
	}
}

func TestGateTenantWithoutPolicyPassesChain(t *testing.T) {
	a := gateTenantGen(map[string]tenantState{
		"other":    {quota: TenantQuota{Enabled: true, DailyCalls: ip(0)}},
		"disabled": {quota: TenantQuota{Enabled: false, DailyCalls: ip(0)}, dayCalls: 9},
	})
	for _, tenant := range []string{"t-none", "disabled"} {
		got, err := a.GateTenant(tenant, chainLocalExternal())
		if err != nil || len(got) != 2 {
			t.Fatalf("GateTenant(%q) = %v, %v; want the chain unchanged", tenant, qNames(got), err)
		}
	}
}

func TestGateTenantDailyCallsReached(t *testing.T) {
	a := gateTenantGen(map[string]tenantState{
		"t1": {quota: TenantQuota{Enabled: true, DailyCalls: ip(5)}, dayCalls: 5},
		"t2": {quota: TenantQuota{Enabled: true, DailyCalls: ip(5)}, dayCalls: 4},
	})
	got, err := a.GateTenant("t1", chainLocalExternal())
	var qe *ErrQuotaExceeded
	if !errors.As(err, &qe) || qe.Reason != "daily_calls" || got != nil {
		t.Fatalf("GateTenant(t1) at daily_calls = %v, %v; want *ErrQuotaExceeded daily_calls", qNames(got), err)
	}
	// Control: one call below the budget passes the whole chain.
	if got, err := a.GateTenant("t2", chainLocalExternal()); err != nil || len(got) != 2 {
		t.Fatalf("GateTenant(t2) under budget = %v, %v; want the chain", qNames(got), err)
	}
	// The same tenant through Gate(scope) — no scope maps to it — is ungated:
	// the difference GateTenant exists for (B21).
	if got, err := a.Gate("t1", chainLocalExternal()); err != nil || len(got) != 2 {
		t.Fatalf("Gate(t1) (no scope row) = %v, %v; want the ungated chain", qNames(got), err)
	}
}

func TestGateScopeUnchangedByGateTenant(t *testing.T) {
	a := seedQuota(map[string]tenantState{"acme:main": {quota: TenantQuota{Enabled: true, DailyCalls: ip(1)}, dayCalls: 1}})
	for _, scope := range []string{"", GlobalScope} {
		got, err := a.Gate(scope, chainLocalExternal())
		if err != nil || len(got) != 2 {
			t.Fatalf("Gate(%q) = %v, %v; want the ungated chain (sentinel callers)", scope, qNames(got), err)
		}
	}
	var qe *ErrQuotaExceeded
	if _, err := a.Gate("acme:main", chainLocalExternal()); !errors.As(err, &qe) || qe.Reason != "daily_calls" {
		t.Fatalf("Gate(acme:main) = %v; want *ErrQuotaExceeded daily_calls", err)
	}
}
