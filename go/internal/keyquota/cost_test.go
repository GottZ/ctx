package keyquota_test

// The cost dimension of the meter (wave W06-14 (b), design/06 §4.3
// cost_budget, §4.5): the provider-reported cost a consumer's requests
// booked in the running UTC month, per consumer × op and per tenant
// envelope × op; it never refuses an admission — the chain rule drops the
// cost-reporting rows once it is used up (a degrade, never a 429).

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

func costLimits(consumer, tenant float64) keyquota.Limits {
	return budgetLimits(keyquota.Budget{DailyTokens: 1 << 40, MonthlyCostUSD: consumer}, keyquota.Budget{DailyTokens: 1 << 40, MonthlyCostUSD: tenant}, keyquota.Budget{})
}

// TestCostBudgetIsUsedUp: AddCost books on the reservation's end into the
// consumer and its tenant envelope; the budget is used up at >= the limit;
// an aborted request (no wire call) books no cost; the month resets it; the
// tenant envelope adds the consumers of the tenant up; the seed's month cost
// counts.
func TestCostBudgetIsUsedUp(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := costLimits(1.0, 1.5)
	s := consumerOf("c1", "t1")
	r := admit(t, "first", m, s, l, tokens(1))
	r.AddCost(0.6)
	r.Settle(keyquota.Usage{CompletionTokens: 1})
	if m.CostExhausted(s, l) {
		t.Fatal("0.6 of 1.0 reported as used up")
	}
	a := admit(t, "aborted", m, s, l, tokens(1))
	a.AddCost(5)
	a.Abort()
	if m.CostExhausted(s, l) {
		t.Fatal("the cost of an aborted request (no wire call) was booked")
	}
	r = admit(t, "second", m, s, l, tokens(1))
	r.AddCost(0.4)
	r.Keep()
	if !m.CostExhausted(s, l) {
		t.Fatal("1.0 of 1.0 not used up")
	}
	if m.CostExhausted(consumerOf("c2", "t2"), l) {
		t.Fatal("another tenant's consumer sees c1's cost")
	}
	r = admit(t, "tenant mate", m, consumerOf("c3", "t1"), l, tokens(1))
	r.AddCost(0.5)
	r.Settle(keyquota.Usage{CompletionTokens: 1})
	if !m.CostExhausted(consumerOf("c4", "t1"), l) {
		t.Fatal("tenant envelope 1.5 with 1.5 booked by two consumers: c4 not exhausted")
	}
	if m.CostExhausted(consumerOf("c4", "t1"), costLimits(1.0, 0)) {
		t.Fatal("without a tenant cost envelope c4 (0 booked) is exhausted")
	}
	c.add(26 * 24 * time.Hour) // 2026-12-01 12:00 UTC
	if m.CostExhausted(s, l) {
		t.Fatal("the cost survived the change of the UTC month")
	}

	seededM := keyquota.New(c.now)
	if err := seededM.Seed(c.now(), []keyquota.SeedRow{{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpChat, MonthCostUSD: 1.0}}); err != nil {
		t.Fatal(err)
	}
	if !seededM.CostExhausted(s, l) {
		t.Fatal("the seed's month cost does not count")
	}
}

// TestCostScales: the tenant envelope of the cost is the factor × the
// consumer's budget; 0 stays 0 (no cost dimension).
func TestCostScales(t *testing.T) {
	if b := (keyquota.Budget{MonthlyCostUSD: 1.0}).Scale(5); b.MonthlyCostUSD != 5 {
		t.Errorf("cost × 5 = %v", b.MonthlyCostUSD)
	}
	if b := (keyquota.Budget{}).Scale(5); b.MonthlyCostUSD != 0 {
		t.Errorf("no cost budget scaled to %v", b.MonthlyCostUSD)
	}
}
