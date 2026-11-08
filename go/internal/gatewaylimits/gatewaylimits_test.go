package gatewaylimits

// Gates of wave W06-15 on the resolution rule (design/06 §4.3, §5 B-06-5,
// B-06-19, B-06-20; masterplan §3.1 "Tenant-Zeile weitet ⇒ 400"): the chain
// consumer → tenant → instance → settings per dimension, ∞ semantics,
// max_concurrency never unbounded, the egress permissions apart from the
// budget, the envelopes, the narrowing ceiling of a tenant-admin write and
// the registry's snapshot life.

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func bp(v bool) *bool        { return &v }

// floor is the settings floor of chat in these tests.
var floor = Defaults{RPM: 30, TPM: 60000, Concurrency: 2, DailyTokens: 2000000, MonthlyTokens: 0, DailyRequests: 0,
	MonthlyCostUSD: 1.0, AllowExternal: false, TenantEnvelopeFactor: 5, InstanceEnvelopeFactor: 10, ConcurrencyCap: 8}

func consumerRow(id string, tier Tier, v Values) Row {
	return Row{ID: "r-c-" + id + "-" + string(tier), Kind: SubjectConsumer, ConsumerID: id, Tier: tier, Op: keyquota.OpChat, Values: v}
}

func tenantRow(id string, tier Tier, v Values) Row {
	return Row{ID: "r-t-" + id + "-" + string(tier), Kind: SubjectTenant, TenantID: id, Tier: tier, Op: keyquota.OpChat, Values: v}
}

func instanceRow(v Values) Row {
	return Row{ID: "r-i", Kind: SubjectInstance, Tier: TierServer, Op: keyquota.OpChat, Values: v}
}

func envelopeRow(id string, v Values) Row {
	return Row{ID: "r-e-" + id, Kind: SubjectTenantEnvelope, TenantID: id, Tier: TierServer, Op: keyquota.OpChat, Values: v}
}

// dimension reads one numeric dimension of the consumer level of e.
type dimension struct {
	name string
	set  func(v *Values, n int64)
	get  func(e Effective) int64
}

var dimensions = []dimension{
	{"rpm", func(v *Values, n int64) { v.RPM = i64(n) }, func(e Effective) int64 { return e.RPM }},
	{"tpm", func(v *Values, n int64) { v.TPM = i64(n) }, func(e Effective) int64 { return e.TPM }},
	{"max_concurrency", func(v *Values, n int64) { v.MaxConcurrency = i64(n) }, func(e Effective) int64 { return e.Concurrency }},
	{"daily_tokens", func(v *Values, n int64) { v.DailyTokens = i64(n) }, func(e Effective) int64 { return e.Consumer.DailyTokens }},
	{"monthly_tokens", func(v *Values, n int64) { v.MonthlyTokens = i64(n) }, func(e Effective) int64 { return e.Consumer.MonthlyTokens }},
	{"daily_requests", func(v *Values, n int64) { v.DailyRequests = i64(n) }, func(e Effective) int64 { return e.Consumer.DailyRequests }},
}

func valuesWith(d dimension, n int64) Values {
	var v Values
	d.set(&v, n)
	return v
}

// TestResolveChainPerDimension is gate (f) of W06-15 (dispatch C1 "Auflösungskette"):
// per dimension a consumer row beats the tenant row beats the instance row
// beats the settings floor — each level alone and stacked.
func TestResolveChainPerDimension(t *testing.T) {
	for _, d := range dimensions {
		t.Run(d.name, func(t *testing.T) {
			cases := []struct {
				name string
				rows []Row
				want int64
			}{
				{"instance over settings", []Row{instanceRow(valuesWith(d, 7))}, 7},
				{"tenant over instance", []Row{instanceRow(valuesWith(d, 7)), tenantRow("t1", TierServer, valuesWith(d, 5))}, 5},
				{"tenant raises over instance (server tier is free)", []Row{instanceRow(valuesWith(d, 3)), tenantRow("t1", TierServer, valuesWith(d, 6))}, 6},
				{"consumer over tenant", []Row{instanceRow(valuesWith(d, 7)), tenantRow("t1", TierServer, valuesWith(d, 5)),
					consumerRow("c1", TierServer, valuesWith(d, 4))}, 4},
				{"consumer server raises over tenant", []Row{tenantRow("t1", TierServer, valuesWith(d, 2)), consumerRow("c1", TierServer, valuesWith(d, 6))}, 6},
				{"tenant-tier consumer row narrows", []Row{consumerRow("c1", TierServer, valuesWith(d, 6)), consumerRow("c1", TierTenant, valuesWith(d, 3))}, 3},
				{"tenant-tier tenant row narrows", []Row{tenantRow("t1", TierServer, valuesWith(d, 6)), tenantRow("t1", TierTenant, valuesWith(d, 4))}, 4},
				{"tenant-tier row never widens", []Row{consumerRow("c1", TierServer, valuesWith(d, 3)), consumerRow("c1", TierTenant, valuesWith(d, 6))}, 3},
				{"other consumer's row does not apply", []Row{consumerRow("c2", TierServer, valuesWith(d, 1)), instanceRow(valuesWith(d, 7))}, 7},
				{"other tenant's row does not apply", []Row{tenantRow("t2", TierServer, valuesWith(d, 1)), instanceRow(valuesWith(d, 7))}, 7},
			}
			for _, tc := range cases {
				got := d.get(NewSnapshot(tc.rows).Resolve("c1", "t1", keyquota.OpChat, floor))
				if got != tc.want {
					t.Errorf("%s: %s = %d, want %d (chain consumer → tenant → instance → settings)", tc.name, d.name, got, tc.want)
				}
			}
		})
	}
}

// TestResolveOtherOpUntouched: a row is per op — a chat row leaves embed at
// its floor.
func TestResolveOtherOpUntouched(t *testing.T) {
	s := NewSnapshot([]Row{consumerRow("c1", TierServer, Values{RPM: i64(1)})})
	if got := s.Resolve("c1", "t1", keyquota.OpChat, floor).RPM; got != 1 {
		t.Errorf("chat rpm = %d, want the row's 1", got)
	}
	if got := s.Resolve("c1", "t1", keyquota.OpEmbed, Defaults{RPM: 120, TPM: 1, Concurrency: 2}).RPM; got != 120 {
		t.Errorf("embed rpm = %d, want the embed floor 120 (a chat row says nothing about embed)", got)
	}
}

// TestResolveInfinityAndConcurrency: settings 0 = ∞ for monthly_tokens and
// daily_requests; a row bounds it; max_concurrency is never ∞ and never
// above the V48 cap per row (W06-11 handover).
func TestResolveInfinityAndConcurrency(t *testing.T) {
	e := NewSnapshot(nil).Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Consumer.MonthlyTokens != 0 || e.Consumer.DailyRequests != 0 {
		t.Fatalf("settings 0 must stay unbounded: %+v", e.Consumer)
	}
	e = NewSnapshot([]Row{consumerRow("c1", TierTenant, Values{MonthlyTokens: i64(500)})}).Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Consumer.MonthlyTokens != 500 {
		t.Errorf("tenant-tier row under an unbounded floor: monthly_tokens = %d, want 500 (min(∞, 500))", e.Consumer.MonthlyTokens)
	}
	e = NewSnapshot([]Row{consumerRow("c1", TierServer, Values{MaxConcurrency: i64(30)})}).Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Concurrency != floor.ConcurrencyCap {
		t.Errorf("max_concurrency row 30 over the cap %d: effective %d, want the cap (V48 per row)", floor.ConcurrencyCap, e.Concurrency)
	}
	e = NewSnapshot([]Row{consumerRow("c1", TierServer, Values{MaxConcurrency: i64(5)})}).Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Concurrency != 5 {
		t.Errorf("max_concurrency row 5: effective %d, want 5 (the row replaces the registry default)", e.Concurrency)
	}
	if src := e.Sources["max_concurrency"]; src != SourceConsumerServer {
		t.Errorf("source of max_concurrency = %q, want %q", src, SourceConsumerServer)
	}
	if src := e.Sources["rpm"]; src != SourceSettings {
		t.Errorf("source of rpm without a row = %q, want %q", src, SourceSettings)
	}
}

// TestResolveEgress: the permissions apart from the budget (B-06-13,
// B-06-20, B-06-29): server tier grants, tenant tier only withdraws,
// unmetered only with allow_external, cost budget from the chain.
func TestResolveEgress(t *testing.T) {
	cases := []struct {
		name                    string
		rows                    []Row
		wantExternal, wantUnmet bool
		wantCost                float64
	}{
		{"settings floor", nil, false, false, 1.0},
		{"consumer server grants", []Row{consumerRow("c1", TierServer, Values{AllowExternal: bp(true)})}, true, false, 1.0},
		{"tenant default grants", []Row{tenantRow("t1", TierServer, Values{AllowExternal: bp(true)})}, true, false, 1.0},
		{"instance grants", []Row{instanceRow(Values{AllowExternal: bp(true)})}, true, false, 1.0},
		{"consumer server withdraws a tenant grant", []Row{tenantRow("t1", TierServer, Values{AllowExternal: bp(true)}),
			consumerRow("c1", TierServer, Values{AllowExternal: bp(false)})}, false, false, 1.0},
		{"tenant tier withdraws", []Row{consumerRow("c1", TierServer, Values{AllowExternal: bp(true)}),
			consumerRow("c1", TierTenant, Values{AllowExternal: bp(false)})}, false, false, 1.0},
		{"tenant-tier tenant row withdraws for every consumer", []Row{instanceRow(Values{AllowExternal: bp(true)}),
			tenantRow("t1", TierTenant, Values{AllowExternal: bp(false)})}, false, false, 1.0},
		{"unmetered with permission", []Row{consumerRow("c1", TierServer, Values{AllowExternal: bp(true), AllowUnmeteredExternal: bp(true)})}, true, true, 1.0},
		{"unmetered without permission stays off", []Row{consumerRow("c1", TierServer, Values{AllowUnmeteredExternal: bp(true)})}, false, false, 1.0},
		{"cost budget row", []Row{consumerRow("c1", TierServer, Values{MonthlyCostUSD: f64(25)})}, false, false, 25},
		{"cost budget tenant-tier narrows", []Row{consumerRow("c1", TierServer, Values{MonthlyCostUSD: f64(25)}),
			consumerRow("c1", TierTenant, Values{MonthlyCostUSD: f64(3)})}, false, false, 3},
	}
	for _, tc := range cases {
		e := NewSnapshot(tc.rows).Resolve("c1", "t1", keyquota.OpChat, floor)
		if e.AllowExternal != tc.wantExternal || e.AllowUnmetered != tc.wantUnmet || e.Consumer.MonthlyCostUSD != tc.wantCost {
			t.Errorf("%s: allow_external %v unmetered %v cost %v, want %v %v %v", tc.name, e.AllowExternal, e.AllowUnmetered,
				e.Consumer.MonthlyCostUSD, tc.wantExternal, tc.wantUnmet, tc.wantCost)
		}
	}
}

// TestResolveEnvelopes: the tenant envelope row replaces factor × settings,
// the instance envelope scales the settings floor — an instance row lowers
// every consumer's default, never the envelopes (S2 decision after
// Fremd-Review 1 M-2: S1 asserted factor × instance row, A8 withdrawn).
func TestResolveEnvelopes(t *testing.T) {
	e := NewSnapshot([]Row{envelopeRow("t1", Values{DailyTokens: i64(123), MonthlyCostUSD: f64(9)}),
		instanceRow(Values{DailyTokens: i64(1000)})}).Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Tenant.DailyTokens != 123 || e.Tenant.MonthlyCostUSD != 9 {
		t.Errorf("tenant envelope = %+v, want the row's 123 tokens / 9 USD", e.Tenant)
	}
	if e.Instance.DailyTokens != floor.DailyTokens*10 {
		t.Errorf("instance envelope daily tokens = %d, want factor 10 × the settings floor (not × the instance row 1000)", e.Instance.DailyTokens)
	}
	if e.Consumer.DailyTokens != 1000 {
		t.Errorf("consumer daily tokens = %d, want the instance row 1000", e.Consumer.DailyTokens)
	}
	// Fremd-Review 1 M-2: a low default for all plus a high grant for one.
	e = NewSnapshot([]Row{instanceRow(Values{DailyTokens: i64(1000)}), consumerRow("c1", TierServer, Values{DailyTokens: i64(1000000)})}).
		Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Consumer.DailyTokens != 1000000 || e.Tenant.DailyTokens != floor.DailyTokens*5 || e.Instance.DailyTokens != floor.DailyTokens*10 {
		t.Errorf("instance row 1000 + grant 1 000 000: consumer %d tenant env %d instance env %d, want 1000000, 5 × floor, 10 × floor",
			e.Consumer.DailyTokens, e.Tenant.DailyTokens, e.Instance.DailyTokens)
	}
	e = NewSnapshot([]Row{envelopeRow("t2", Values{DailyTokens: i64(1)})}).Resolve("c1", "t1", keyquota.OpChat, floor)
	if e.Tenant.DailyTokens != floor.DailyTokens*5 {
		t.Errorf("tenant envelope of another tenant applied: %d, want factor 5 × floor", e.Tenant.DailyTokens)
	}
}

// TestCeilingNarrowing is gate (a) of W06-15 and the dispatch's "Verengung":
// a tenant-tier tenant row above the instance default widens ⇒ refused with
// the field; a tenant-tier consumer row above the tenant default ⇒ refused;
// a missing server row means the settings value, never unbounded (05
// V-06-e); at or under the ceiling passes; a tenant-tier permission is
// refused (B-06-20).
func TestCeilingNarrowing(t *testing.T) {
	s := NewSnapshot([]Row{instanceRow(Values{RPM: i64(20)}), tenantRow("t1", TierServer, Values{TPM: i64(1000)})})
	ceil, err := s.Ceiling(SubjectTenant, "", "t1", keyquota.OpChat, floor)
	if err != nil {
		t.Fatalf("Ceiling(tenant): %v", err)
	}
	if fe := CheckNarrowing(Values{RPM: i64(21)}, ceil); fe == nil || fe.Field != "rpm" || fe.Code != CodeWidens {
		t.Errorf("tenant row rpm 21 over the instance default 20: %+v, want field rpm %s", fe, CodeWidens)
	}
	if fe := CheckNarrowing(Values{TPM: i64(1001)}, ceil); fe == nil || fe.Field != "tpm" {
		t.Errorf("tenant row tpm 1001 over the server tenant row 1000: %+v, want field tpm", fe)
	}
	if fe := CheckNarrowing(Values{RPM: i64(20), TPM: i64(999)}, ceil); fe != nil {
		t.Errorf("tenant row at/under the ceiling refused: %+v", fe)
	}
	if fe := CheckNarrowing(Values{DailyTokens: i64(floor.DailyTokens + 1)}, ceil); fe == nil || fe.Field != "daily_tokens" {
		t.Errorf("no server row for daily_tokens: the settings value bounds it, got %+v", fe)
	}
	if fe := CheckNarrowing(Values{MonthlyTokens: i64(1 << 40)}, ceil); fe != nil {
		t.Errorf("monthly_tokens under an unbounded floor refused: %+v", fe)
	}
	cs := NewSnapshot([]Row{tenantRow("t1", TierServer, Values{RPM: i64(10)}), tenantRow("t1", TierTenant, Values{RPM: i64(6)}),
		consumerRow("c1", TierTenant, Values{RPM: i64(4)})})
	ceil, err = cs.Ceiling(SubjectConsumer, "c1", "t1", keyquota.OpChat, floor)
	if err != nil {
		t.Fatalf("Ceiling(consumer): %v", err)
	}
	if fe := CheckNarrowing(Values{RPM: i64(7)}, ceil); fe == nil || fe.Field != "rpm" || fe.Code != CodeWidens {
		t.Errorf("consumer row rpm 7 over the tenant default 6: %+v, want field rpm %s", fe, CodeWidens)
	}
	if fe := CheckNarrowing(Values{RPM: i64(6)}, ceil); fe != nil {
		t.Errorf("consumer row rpm 6 (its own tenant row 4 must not count): %+v", fe)
	}
	if fe := CheckNarrowing(Values{AllowExternal: bp(true)}, ceil); fe == nil || fe.Field != "allow_external" {
		t.Errorf("tenant tier allow_external=true: %+v, want field allow_external", fe)
	}
	if fe := CheckNarrowing(Values{AllowExternal: bp(false)}, ceil); fe != nil {
		t.Errorf("tenant tier allow_external=false (withdraw) refused: %+v", fe)
	}
	if fe := CheckNarrowing(Values{AllowUnmeteredExternal: bp(false)}, ceil); fe == nil || fe.Field != "allow_unmetered_external" {
		t.Errorf("tenant tier allow_unmetered_external: %+v, want field allow_unmetered_external", fe)
	}
}

// TestValidateValues: the row shape per (kind, tier) — no 0 sentinel, the
// envelope's dimensions, server-only kinds, the V48 cap per row.
func TestValidateValues(t *testing.T) {
	cases := []struct {
		name      string
		kind      SubjectKind
		tier      Tier
		v         Values
		wantField string // "" = valid
	}{
		{"consumer server valid", SubjectConsumer, TierServer, Values{RPM: i64(5), AllowExternal: bp(true), MonthlyCostUSD: f64(2)}, ""},
		{"zero rpm", SubjectConsumer, TierServer, Values{RPM: i64(0)}, "rpm"},
		{"negative tpm", SubjectTenant, TierServer, Values{TPM: i64(-1)}, "tpm"},
		{"zero cost (B-06-20)", SubjectConsumer, TierServer, Values{MonthlyCostUSD: f64(0)}, "monthly_cost_usd"},
		{"concurrency over the cap", SubjectConsumer, TierServer, Values{MaxConcurrency: i64(9)}, "max_concurrency"},
		{"envelope with rpm", SubjectTenantEnvelope, TierServer, Values{RPM: i64(5)}, "rpm"},
		{"envelope with concurrency", SubjectTenantEnvelope, TierServer, Values{MaxConcurrency: i64(1)}, "max_concurrency"},
		{"envelope with unmetered", SubjectTenantEnvelope, TierServer, Values{AllowUnmeteredExternal: bp(false)}, "allow_unmetered_external"},
		{"envelope valid", SubjectTenantEnvelope, TierServer, Values{DailyTokens: i64(5), MonthlyCostUSD: f64(1)}, ""},
		{"instance tenant tier", SubjectInstance, TierTenant, Values{RPM: i64(1)}, "subject_kind"},
		{"envelope tenant tier", SubjectTenantEnvelope, TierTenant, Values{DailyTokens: i64(1)}, "subject_kind"},
		{"tenant tier valid", SubjectConsumer, TierTenant, Values{RPM: i64(1), AllowExternal: bp(false)}, ""},
	}
	for _, tc := range cases {
		fe := ValidateValues(tc.kind, tc.tier, tc.v, floor)
		switch {
		case tc.wantField == "" && fe != nil:
			t.Errorf("%s: refused %+v, want valid", tc.name, fe)
		case tc.wantField != "" && (fe == nil || fe.Field != tc.wantField):
			t.Errorf("%s: %+v, want a refusal at field %q", tc.name, fe, tc.wantField)
		}
	}
}

// TestRegistryLoadKeepsPreviousOnFailure: Load publishes the rows; a failed
// load keeps the previous snapshot.
func TestRegistryLoadKeepsPreviousOnFailure(t *testing.T) {
	fail := false
	r := NewRegistry(func(context.Context) ([]Row, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return []Row{consumerRow("c1", TierServer, Values{RPM: i64(3)})}, nil
	})
	if err := r.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := r.Snapshot().Resolve("c1", "t1", keyquota.OpChat, floor).RPM; got != 3 {
		t.Fatalf("after Load: rpm %d, want the row's 3", got)
	}
	fail = true
	if err := r.Load(context.Background()); err == nil {
		t.Fatal("failed load reported no error")
	}
	if got := len(r.Snapshot().Rows()); got != 1 {
		t.Errorf("after a failed load: %d rows, want the previous snapshot's 1", got)
	}
}

// TestRegistryTriggerLoads: a NOTIFY trigger reaches a load without the
// caller blocking, and a burst is coalesced (at most two loads for ten
// triggers while one runs).
func TestRegistryTriggerLoads(t *testing.T) {
	var loads atomic.Int64
	release := make(chan struct{})
	r := NewRegistry(func(context.Context) ([]Row, error) {
		if loads.Add(1) == 1 {
			<-release
		}
		return []Row{consumerRow("c1", TierServer, Values{RPM: i64(9)})}, nil
	})
	for range 10 {
		r.Trigger(context.Background())
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for r.Snapshot().Resolve("c1", "t1", keyquota.OpChat, floor).RPM != 9 {
		if time.Now().After(deadline) {
			t.Fatalf("trigger never loaded (loads=%d)", loads.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := loads.Load(); n > 2 {
		t.Errorf("10 triggers during a running load: %d loads, want at most 2 (coalesced)", n)
	}
}

// TestConsumerTiers is the interface to C2 (W06-16 /api/usage/consumers):
// the stored server and tenant rows per asked consumer and op; consumers
// not asked for, consumers without rows and tenant rows stay out.
func TestConsumerTiers(t *testing.T) {
	s := NewSnapshot([]Row{consumerRow("c1", TierServer, Values{RPM: i64(5)}), consumerRow("c1", TierTenant, Values{RPM: i64(3)}),
		consumerRow("c2", TierServer, Values{TPM: i64(9)}), tenantRow("t1", TierServer, Values{RPM: i64(1)})})
	got := s.ConsumerTiers([]string{"c1", "c3"})
	p, ok := got["c1"][keyquota.OpChat]
	if !ok || p.Server == nil || p.Server.RPM == nil || *p.Server.RPM != 5 || p.Tenant == nil || p.Tenant.RPM == nil || *p.Tenant.RPM != 3 {
		t.Errorf("c1 chat = %+v (present %v), want the server row rpm 5 and the tenant row rpm 3", p, ok)
	}
	if _, ok := got["c2"]; ok {
		t.Error("c2 was not asked for, but is in the answer")
	}
	if _, ok := got["c3"]; ok {
		t.Error("c3 has no rows, but is in the answer")
	}
}

// TestRegistryTriggerDuringLoadPublishesNewest: a trigger that arrives while
// a load runs is not lost — the drain loads once more, and the snapshot ends
// at the rows committed after the first read (a NOTIFY during a reload must
// reach the meter).
func TestRegistryTriggerDuringLoadPublishesNewest(t *testing.T) {
	var loads atomic.Int64
	inFirst, release := make(chan struct{}), make(chan struct{})
	r := NewRegistry(func(context.Context) ([]Row, error) {
		if loads.Add(1) == 1 {
			close(inFirst)
			<-release
			return []Row{consumerRow("c1", TierServer, Values{RPM: i64(1)})}, nil
		}
		return []Row{consumerRow("c1", TierServer, Values{RPM: i64(2)})}, nil
	})
	r.Trigger(context.Background())
	<-inFirst
	r.Trigger(context.Background()) // the write committed during the first read
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for r.Snapshot().Resolve("c1", "t1", keyquota.OpChat, floor).RPM != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("snapshot rpm %d after a trigger during the load (loads=%d), want 2 — the pending reload was lost",
				r.Snapshot().Resolve("c1", "t1", keyquota.OpChat, floor).RPM, loads.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestValidateValuesColumnBounds: a value at its column's maximum is valid,
// one above is refused at the write (never SQLSTATE 22003 from the
// database): INTEGER for rpm/tpm/daily_requests, SMALLINT for
// max_concurrency (without a V48 cap), BIGINT for the token budgets,
// NUMERIC(14,8) and finite for the cost.
func TestValidateValuesColumnBounds(t *testing.T) {
	noCap := floor
	noCap.ConcurrencyCap = 0
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name      string
		v         Values
		wantField string
	}{
		{"rpm at INTEGER max", Values{RPM: i64(2147483647)}, ""},
		{"rpm above INTEGER max", Values{RPM: i64(2147483648)}, "rpm"},
		{"daily_requests above INTEGER max", Values{DailyRequests: i64(2147483648)}, "daily_requests"},
		{"max_concurrency at SMALLINT max", Values{MaxConcurrency: i64(32767)}, ""},
		{"max_concurrency above SMALLINT max", Values{MaxConcurrency: i64(32768)}, "max_concurrency"},
		{"daily_tokens at BIGINT max", Values{DailyTokens: i64(9223372036854775807)}, ""},
		{"cost at NUMERIC(14,8) max", Values{MonthlyCostUSD: f(999999.99999999)}, ""},
		{"cost above NUMERIC(14,8) max", Values{MonthlyCostUSD: f(1000000)}, "monthly_cost_usd"},
		{"cost NaN", Values{MonthlyCostUSD: f(math.NaN())}, "monthly_cost_usd"},
		{"cost +Inf", Values{MonthlyCostUSD: f(math.Inf(1))}, "monthly_cost_usd"},
	}
	for _, tc := range cases {
		fe := ValidateValues(SubjectConsumer, TierServer, tc.v, noCap)
		switch {
		case tc.wantField == "" && fe != nil:
			t.Errorf("%s: refused %+v, want valid", tc.name, fe)
		case tc.wantField != "" && (fe == nil || fe.Field != tc.wantField):
			t.Errorf("%s: %+v, want a refusal at %q", tc.name, fe, tc.wantField)
		}
	}
}

// TestResolveCostDimension (Fremd-Review 1 L-3): monthly_cost_usd through
// the chain — consumer server over tenant server over instance over the
// setting, tenant-tier rows only narrow, the ceiling and the narrowing check
// of a tenant-admin cost; the cost's column resolution (M-1); the source of
// an unset unmetered permission is the fixed default, not a setting (N-3).
func TestResolveCostDimension(t *testing.T) {
	cost := func(rows ...Row) Effective { return NewSnapshot(rows).Resolve("c1", "t1", keyquota.OpChat, floor) }
	cases := []struct {
		name string
		e    Effective
		want float64
		src  Source
	}{
		{"setting", cost(), 1.0, SourceSettings},
		{"instance", cost(instanceRow(Values{MonthlyCostUSD: f64(2)})), 2, SourceInstance},
		{"tenant server over instance", cost(instanceRow(Values{MonthlyCostUSD: f64(2)}), tenantRow("t1", TierServer, Values{MonthlyCostUSD: f64(3)})), 3, SourceTenantServer},
		{"consumer server over tenant", cost(tenantRow("t1", TierServer, Values{MonthlyCostUSD: f64(3)}), consumerRow("c1", TierServer, Values{MonthlyCostUSD: f64(5)})), 5, SourceConsumerServer},
		{"tenant-tier tenant narrows", cost(consumerRow("c1", TierServer, Values{MonthlyCostUSD: f64(5)}), tenantRow("t1", TierTenant, Values{MonthlyCostUSD: f64(4)})), 4, SourceTenantTenant},
		{"tenant-tier consumer narrows", cost(consumerRow("c1", TierServer, Values{MonthlyCostUSD: f64(5)}), consumerRow("c1", TierTenant, Values{MonthlyCostUSD: f64(0.5)})), 0.5, SourceConsumerTenant},
		{"tenant-tier never widens", cost(consumerRow("c1", TierServer, Values{MonthlyCostUSD: f64(5)}), consumerRow("c1", TierTenant, Values{MonthlyCostUSD: f64(9)})), 5, SourceConsumerServer},
	}
	for _, tc := range cases {
		if tc.e.Consumer.MonthlyCostUSD != tc.want || tc.e.Sources["monthly_cost_usd"] != tc.src {
			t.Errorf("%s: cost %v (source %s), want %v (%s)", tc.name, tc.e.Consumer.MonthlyCostUSD, tc.e.Sources["monthly_cost_usd"], tc.want, tc.src)
		}
	}
	s := NewSnapshot([]Row{tenantRow("t1", TierServer, Values{MonthlyCostUSD: f64(3)}), tenantRow("t1", TierTenant, Values{MonthlyCostUSD: f64(2)})})
	ceil, err := s.Ceiling(SubjectConsumer, "c1", "t1", keyquota.OpChat, floor)
	if err != nil || ceil.MonthlyCostUSD == nil || *ceil.MonthlyCostUSD != 2 {
		t.Fatalf("cost ceiling of a consumer = %v (%v), want 2 (the tenant's tenant-tier cap)", ceil.MonthlyCostUSD, err)
	}
	if fe := CheckNarrowing(Values{MonthlyCostUSD: f64(2.5)}, ceil); fe == nil || fe.Field != "monthly_cost_usd" || fe.Code != CodeWidens {
		t.Errorf("tenant-admin cost 2.5 over the ceiling 2: %+v, want monthly_cost_usd %s", fe, CodeWidens)
	}
	if fe := CheckNarrowing(Values{MonthlyCostUSD: f64(2)}, ceil); fe != nil {
		t.Errorf("tenant-admin cost at the ceiling refused: %+v", fe)
	}
	for _, tc := range []struct {
		usd   float64
		valid bool
	}{{0.00000001, true}, {0.000000004, false}, {0.000000001, false}} {
		fe := ValidateValues(SubjectConsumer, TierServer, Values{MonthlyCostUSD: f64(tc.usd)}, floor)
		if (fe == nil) != tc.valid {
			t.Errorf("cost %g: refusal %+v, want valid=%v (NUMERIC(14,8) stores below 0.000000005 as 0, Fremd-Review 1 M-1)", tc.usd, fe, tc.valid)
		}
	}
	if src := cost().Sources["allow_unmetered_external"]; src != SourceDefault {
		t.Errorf("source of an unset unmetered permission = %q, want %q (there is no setting)", src, SourceDefault)
	}
}

// TestRegistryLoadSerialised (Fremd-Review 1 L-3): a load that starts after
// another publishes after it — a slower older read never overwrites the
// newer snapshot (loadMu).
func TestRegistryLoadSerialised(t *testing.T) {
	var calls atomic.Int64
	inFirst, release := make(chan struct{}), make(chan struct{})
	r := NewRegistry(func(context.Context) ([]Row, error) {
		if calls.Add(1) == 1 {
			close(inFirst)
			<-release
			return []Row{consumerRow("c1", TierServer, Values{RPM: i64(1)})}, nil
		}
		return []Row{consumerRow("c1", TierServer, Values{RPM: i64(2)})}, nil
	})
	done := make(chan struct{}, 2)
	go func() { _ = r.Load(context.Background()); done <- struct{}{} }()
	<-inFirst
	go func() { _ = r.Load(context.Background()); done <- struct{}{} }()
	time.Sleep(20 * time.Millisecond) // the second load is started (blocked on loadMu, or — unserialised — done)
	close(release)
	<-done
	<-done
	if got := r.Snapshot().Resolve("c1", "t1", keyquota.OpChat, floor).RPM; got != 2 {
		t.Errorf("after an older slow load and a newer one: rpm %d, want the newer 2", got)
	}
}
