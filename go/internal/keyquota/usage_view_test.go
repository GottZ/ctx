package keyquota_test

// RC-D cluster C2 gates on the meter (usage_view.go):
//   - W06-18 soft threshold: gateway.soft_ratio lists the day/month
//     dimensions at or over the share in Decision.Warnings, 0 = off, never on
//     a refusal; the warning header rides in Reservation.Headers() next to
//     x-ratelimit-*; envelopes without values (B6).
//   - W06-16 Snapshot: the counters of the three levels, the binding level,
//     the live part — no statement, read under the meter's lock.

import (
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

func softLimits(ratio float64, consumer, tenant, instance keyquota.Budget) keyquota.Limits {
	l := budgetLimits(consumer, tenant, instance)
	l.SoftRatio = ratio
	return l
}

// TestSoftRatioWarnsAtTheShare: daily budget 1000, ratio 0.8 — 700 used is
// below the share, a reservation that brings used + reserved to 850 is at
// it: the admission lists daily_tokens of the consumer level with the
// window's start (00:00 UTC of the day). A monthly budget far away does not
// warn. Ratio 0 never warns, not even at 99 %.
func TestSoftRatioWarnsAtTheShare(t *testing.T) {
	c := newClock() // 2026-11-05 12:00 UTC
	m := seeded(c)
	s := consumerOf("c1", "t1")
	l := softLimits(0.8, keyquota.Budget{DailyTokens: 1000, MonthlyTokens: 1_000_000}, keyquota.Budget{}, keyquota.Budget{})
	res, d := m.Admit(s, l, tokens(700))
	if !d.Allowed {
		t.Fatalf("first admission refused: %+v", d)
	}
	if len(d.Warnings) != 0 {
		t.Errorf("W06-18 gate (soft ratio): 700 of 1000 at ratio 0.8 warns %+v, want none", d.Warnings)
	}
	res.Settle(keyquota.Usage{PromptTokens: 700})
	_, d = m.Admit(s, l, tokens(150))
	if !d.Allowed {
		t.Fatalf("second admission refused: %+v", d)
	}
	want := keyquota.Warning{Level: keyquota.LevelConsumer, Dimension: keyquota.ReasonDailyTokens, Used: 850, Limit: 1000,
		WindowStart: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)}
	if len(d.Warnings) != 1 || d.Warnings[0] != want {
		t.Fatalf("W06-18 gate (soft ratio): 850 of 1000 at ratio 0.8 warns %+v, want exactly %+v", d.Warnings, want)
	}

	off := seeded(newClock())
	l0 := softLimits(0, keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	_, d0 := off.Admit(s, l0, tokens(990))
	if !d0.Allowed || len(d0.Warnings) != 0 {
		t.Errorf("W06-18 gate (soft ratio 0 = off): 990 of 1000 gives %+v, want admitted without warning", d0)
	}
}

// TestSoftRatioNeverOnARefusal: a refused admission (over the day budget)
// carries no warning — the refusal is the hard event, not a soft one.
func TestSoftRatioNeverOnARefusal(t *testing.T) {
	c := newClock()
	m := seeded(c)
	s := consumerOf("c1", "t1")
	l := softLimits(0.5, keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	res, d := m.Admit(s, l, tokens(600))
	if !d.Allowed || len(d.Warnings) != 1 {
		t.Fatalf("W06-18 gate (soft ratio): 600 of 1000 at ratio 0.5 gives %+v, want admitted with one warning", d)
	}
	res.Settle(keyquota.Usage{PromptTokens: 600})
	_, d = m.Admit(s, l, tokens(500))
	if d.Allowed || len(d.Warnings) != 0 {
		t.Errorf("W06-18 gate (no warning on refusal): %+v, want refused without warnings", d)
	}
}

// TestSoftRatioEnvelopesWithoutValues: the tenant envelope at its share
// warns as level tenant; the header names it without a value, the
// consumer's own dimension with pct — in the fixed order.
func TestSoftRatioEnvelopesWithoutValues(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := softLimits(0.8, keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{DailyTokens: 1500}, keyquota.Budget{})
	a := admit(t, "a", m, consumerOf("a", "t1"), l, tokens(600))
	a.Settle(keyquota.Usage{PromptTokens: 600})
	res, d := m.Admit(consumerOf("b", "t1"), l, tokens(850))
	if !d.Allowed {
		t.Fatalf("b refused: %+v", d)
	}
	var levels []keyquota.BudgetLevel
	for _, w := range d.Warnings {
		levels = append(levels, w.Level)
	}
	if len(d.Warnings) != 2 || d.Warnings[0].Level != keyquota.LevelConsumer || d.Warnings[1].Level != keyquota.LevelTenant {
		t.Fatalf("W06-18 gate (envelope warning): levels %v (%+v), want [consumer tenant] — b at 850/1000, tenant at 1450/1500", levels, d.Warnings)
	}
	if got, want := res.Headers().Get(keyquota.QuotaWarningHeader), "daily_tokens;pct=85, tenant_envelope"; got != want {
		t.Errorf("W06-18 gate (warning header): %s = %q, want %q (envelope without a value)", keyquota.QuotaWarningHeader, got, want)
	}
}

// TestWarningHeaderFormat pins the header's form: RFC 8941 list, fixed
// order, pct = floor of the used percentage, envelopes bare; no warning ⇒ "".
func TestWarningHeaderFormat(t *testing.T) {
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	ws := []keyquota.Warning{
		{Level: keyquota.LevelInstance, Dimension: keyquota.ReasonDailyTokens, WindowStart: day},
		{Level: keyquota.LevelConsumer, Dimension: keyquota.ReasonDailyRequests, Used: 99, Limit: 100, WindowStart: day},
		{Level: keyquota.LevelConsumer, Dimension: keyquota.ReasonDailyTokens, Used: 8_333, Limit: 10_000, WindowStart: day},
		{Level: keyquota.LevelConsumer, Dimension: keyquota.ReasonMonthlyTokens, Used: 910, Limit: 1000, WindowStart: day},
		{Level: keyquota.LevelTenant, Dimension: keyquota.ReasonMonthlyTokens, WindowStart: day},
	}
	want := "monthly_tokens;pct=91, daily_tokens;pct=83, daily_requests;pct=99, tenant_envelope, instance_envelope"
	if got := keyquota.WarningHeader(ws); got != want {
		t.Errorf("W06-18 gate (header form): %q, want %q", got, want)
	}
	if got := keyquota.WarningHeader(nil); got != "" {
		t.Errorf("no warning renders %q, want \"\"", got)
	}
	if strings.Contains(want, "tenant_envelope;") {
		t.Fatal("fixture: an envelope must not carry a parameter")
	}
}

// TestSnapshotBindingAndLive is W06-16's meter view: after 900 tokens of
// consumer a against a day budget of 1000 under a tenant envelope of 5000,
// the consumer level binds (100 left vs 4100); after b used 4050 in the same
// tenant and a holds an open reservation of 10, the tenant envelope binds
// for a (40 left of 5000 vs 90 of 1000). The live part counts the open
// reservation; the day resets at 00:00 UTC.
func TestSnapshotBindingAndLive(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := budgetLimits(keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{DailyTokens: 5000}, keyquota.Budget{})
	l.RPM, l.TPM = 100, 100_000
	sa := consumerOf("a", "t1")
	admit(t, "a", m, sa, l, tokens(900)).Settle(keyquota.Usage{PromptTokens: 900})
	snap, ok := m.Snapshot(sa, l)
	if !ok {
		t.Fatal("W06-16 gate (meter view): Snapshot of a seeded meter and a valid subject answered ok = false")
	}
	if snap.Binding != keyquota.LevelConsumer || snap.Consumer.DayTokens != 900 || snap.Tenant.DayTokens != 900 {
		t.Errorf("W06-16 gate (binding): %+v, want binding consumer, 900 used at both levels", snap)
	}
	b := l
	b.Consumer = keyquota.Budget{DailyTokens: 10_000}
	admit(t, "b", m, consumerOf("b", "t1"), b, tokens(4050)).Settle(keyquota.Usage{PromptTokens: 4050})
	open := admit(t, "a open", m, sa, l, tokens(10))
	snap, _ = m.Snapshot(sa, l)
	if snap.Binding != keyquota.LevelTenant {
		t.Errorf("W06-16 gate (binding): %s, want tenant (tenant 4960 of 5000 vs consumer 910 of 1000)", snap.Binding)
	}
	if snap.Inflight != 1 || snap.Consumer.Open != 1 || snap.Consumer.Reserved != 10 {
		t.Errorf("W06-16 gate (live): inflight %d, open %d, reserved %d — want 1, 1, 10", snap.Inflight, snap.Consumer.Open, snap.Consumer.Reserved)
	}
	if want := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC); !snap.DayReset.Equal(want) {
		t.Errorf("W06-16 gate (reset): day reset %v, want %v (UTC)", snap.DayReset, want)
	}
	open.Abort()
}
