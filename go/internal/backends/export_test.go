package backends

import "time"

// Test seams for the external test package backends_test (quota integration
// tests), which cannot reach unexported state otherwise.

// SetClockForTest pins the accountant's clock: the TTL and the end of every
// spend window.
func (a *QuotaAccountant) SetClockForTest(now func() time.Time) { a.nowFn = now }

// SpendForTest is one tenant's rolling spend as the current generation holds it.
type SpendForTest struct {
	DayCalls                                       int
	DayCost, MonthCost                             float64
	DayCallsRelief, DayCostRelief, MonthCostRelief time.Time
}

// SpendForTest reports the spend of the tenant scope belongs to, false when
// the generation does not hold that tenant.
func (a *QuotaAccountant) SpendForTest(scope string) (SpendForTest, bool) {
	gen := a.cache.Load()
	if gen == nil {
		return SpendForTest{}, false
	}
	st, ok := gen.tenants[gen.scopeTenant[scope]]
	if !ok {
		return SpendForTest{}, false
	}
	return SpendForTest{
		DayCalls: st.dayCalls, DayCost: st.dayCost, MonthCost: st.monthCost,
		DayCallsRelief: st.dayCallsRelief, DayCostRelief: st.dayCostRelief, MonthCostRelief: st.monthCostRelief,
	}, true
}
