package backends

// The egress and cost rule of /v1 chains (wave W06-14, design/06 §4.5
// table "Egress-/Kostenregel"): a row is free (local, lan, or a ctx peer),
// billable and cost-reporting (external openrouter) or billable without a
// cost report (every other external row); the consumer's policy keeps or
// drops each class.

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func egressRow(name, locality, provider string) Backend {
	return Backend{Name: name, Locality: locality, ProviderClass: provider}
}

// TestWithoutBillableMatrix is the classification table with file:line in
// the report: every class × every policy, the chain order kept.
func TestWithoutBillableMatrix(t *testing.T) {
	chain := []Backend{
		egressRow("local", LocalityLocal, "llamacpp"),
		egressRow("lan", LocalityLAN, "generic"),
		egressRow("peer", LocalityExternal, ProviderCtx),
		egressRow("openrouter", LocalityExternal, ProviderOpenRouter),
		egressRow("generic-ext", LocalityExternal, "generic"),
		egressRow("unknown-locality", "", "generic"),
	}
	for _, tc := range []struct {
		name string
		p    EgressPolicy
		want []string
	}{
		{"no permission", EgressPolicy{}, []string{"local", "lan", "peer"}},
		{"permission", EgressPolicy{AllowExternal: true}, []string{"local", "lan", "peer", "openrouter"}},
		{"permission, cost used up", EgressPolicy{AllowExternal: true, CostExhausted: true}, []string{"local", "lan", "peer"}},
		{"permission and unmetered", EgressPolicy{AllowExternal: true, AllowUnmetered: true}, []string{"local", "lan", "peer", "openrouter", "generic-ext", "unknown-locality"}},
		{"unmetered without permission", EgressPolicy{AllowUnmetered: true}, []string{"local", "lan", "peer"}},
		{"unmetered, cost used up", EgressPolicy{AllowExternal: true, AllowUnmetered: true, CostExhausted: true}, []string{"local", "lan", "peer", "generic-ext", "unknown-locality"}},
	} {
		var got []string
		for _, b := range WithoutBillable(chain, tc.p) {
			got = append(got, b.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: kept %v, want %v", tc.name, got, tc.want)
		}
	}
	if len(chain) != 6 || chain[3].Name != "openrouter" {
		t.Error("WithoutBillable changed the shared chain in place")
	}
	for _, tc := range []struct {
		b    Backend
		want Billing
	}{
		{chain[0], BillingFree}, {chain[1], BillingFree}, {chain[2], BillingFree},
		{chain[3], BillingMetered}, {chain[4], BillingUnmetered}, {chain[5], BillingUnmetered},
		{egressRow("openrouter-lan", LocalityLAN, ProviderOpenRouter), BillingFree},
	} {
		if got := BillingOf(tc.b); got != tc.want {
			t.Errorf("BillingOf(%s/%s/%s) = %v, want %v", tc.b.Name, tc.b.Locality, tc.b.ProviderClass, got, tc.want)
		}
	}
}

// TestGateTenantSetsTheRelief is gate W06-14 (c) at the accountant: a
// refusal carries the earliest relief of the figure that tripped —
// daily_calls the call relief, the cost budget under block the relief of
// the window over its limit (both over: the later one, both must drop).
func TestGateTenantSetsTheRelief(t *testing.T) {
	a := NewQuotaAccountant(nil, 0)
	calls, dayCost, monthCost := 3, 1.0, 5.0
	day := mustTime("2026-11-08T10:00:00Z")
	month := mustTime("2026-11-20T00:00:00Z")
	callRelief := mustTime("2026-11-08T09:30:00Z")
	gen := func(st tenantState) {
		g := quotaGen{scopeTenant: map[string]string{}, tenants: map[string]tenantState{"t": st}}
		a.ttl = 1 << 62
		a.cache.Store(&g)
		a.cacheAt.Store(a.nowFn().UnixNano())
	}
	gen(tenantState{quota: TenantQuota{Enabled: true, DailyCalls: &calls}, dayCalls: 3, dayCallsRelief: callRelief})
	_, err := a.GateTenant("t", nil)
	var qe *ErrQuotaExceeded
	if !errors.As(err, &qe) || qe.Reason != "daily_calls" || !qe.ReliefAt.Equal(callRelief) {
		t.Fatalf("daily_calls: %v, want ReliefAt %v", err, callRelief)
	}
	block := TenantQuota{Enabled: true, DailyCostUSD: &dayCost, MonthlyCostUSD: &monthCost, OnExceed: QuotaExceedBlock}
	for _, tc := range []struct {
		name               string
		dayUsed, monthUsed float64
		want               string
	}{
		{"day over", 1, 1, "2026-11-08T10:00:00Z"},
		{"month over", 0.5, 5, "2026-11-20T00:00:00Z"},
		{"both over", 2, 6, "2026-11-20T00:00:00Z"},
	} {
		gen(tenantState{quota: block, dayCost: tc.dayUsed, monthCost: tc.monthUsed, dayCostRelief: day, monthCostRelief: month})
		_, err := a.GateTenant("t", nil)
		if !errors.As(err, &qe) || qe.Reason != "cost_budget" || !qe.ReliefAt.Equal(mustTime(tc.want)) {
			t.Errorf("%s: %v, want cost_budget with ReliefAt %s", tc.name, err, tc.want)
		}
	}
	// Both over, the month's relief unknown: the relief is unknown, never
	// the day's earlier one.
	gen(tenantState{quota: block, dayCost: 2, monthCost: 6, dayCostRelief: day})
	if _, err := a.GateTenant("t", nil); !errors.As(err, &qe) || !qe.ReliefAt.IsZero() {
		t.Errorf("both over, month relief unknown: %v, want a zero ReliefAt", err)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
