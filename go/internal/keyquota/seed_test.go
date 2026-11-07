package keyquota_test

// The boot seed and the burst damping (wave W06-13 (c), (g); design/06 §4.2
// "Boot-Seed", "Burst-Dämpfung"): Seed loads what the consumers used in the
// running UTC day and month from the rollup into all three budget levels and
// opens the meter; a GCRA state born in the first window after the seed
// starts half full.

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

// TestSeedRestoresTheBudgets is gate (b), Finding 1, at the meter: a
// consumer that used its 1000 tokens today before the restart is still
// exhausted after it; the envelopes add the rows of all consumers of a
// tenant and of the instance up. Red against MarkSeeded: the budget is free
// again.
func TestSeedRestoresTheBudgets(t *testing.T) {
	c := newClock()
	m := keyquota.New(c.now)
	if _, d := m.Admit(consumerOf("c1", "t1"), budgetLimits(keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{}), tokens(1)); d.Reason != keyquota.ReasonWarming {
		t.Fatalf("before the seed: %+v, want warming", d)
	}
	rows := []keyquota.SeedRow{
		{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpChat, DayTokens: 1000, DayRequests: 3, MonthTokens: 5000},
		{ConsumerID: "c2", TenantID: "t2", Op: keyquota.OpChat, DayTokens: 600, MonthTokens: 600},
		{ConsumerID: "c3", TenantID: "t2", Op: keyquota.OpChat, DayTokens: 600, MonthTokens: 600},
		{ConsumerID: "c4", TenantID: "t2", Op: keyquota.OpEmbed, DayTokens: 9000, MonthTokens: 9000},
	}
	if err := m.Seed(c.now(), rows); err != nil {
		t.Fatal(err)
	}
	if !m.Seeded() {
		t.Fatal("not seeded after Seed")
	}
	c.add(time.Minute) // past the burst damping
	day := keyquota.Budget{DailyTokens: 1000}
	_, d := m.Admit(consumerOf("c1", "t1"), budgetLimits(day, keyquota.Budget{}, keyquota.Budget{}), tokens(1))
	refusedQuota(t, "seeded consumer", d, keyquota.ReasonDailyTokens, 12*time.Hour-time.Minute)
	_, d = m.Admit(consumerOf("c1", "t1"), budgetLimits(keyquota.Budget{DailyTokens: 1 << 40, DailyRequests: 3}, keyquota.Budget{}, keyquota.Budget{}), tokens(1))
	refusedQuota(t, "seeded requests", d, keyquota.ReasonDailyRequests, 12*time.Hour-time.Minute)
	_, d = m.Admit(consumerOf("c1", "t1"), budgetLimits(keyquota.Budget{DailyTokens: 1 << 40, MonthlyTokens: 5000}, keyquota.Budget{}, keyquota.Budget{}), tokens(1))
	refusedQuota(t, "seeded month", d, keyquota.ReasonMonthlyTokens, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Sub(c.now()))
	_, d = m.Admit(consumerOf("c5", "t2"), budgetLimits(day, keyquota.Budget{DailyTokens: 1200}, keyquota.Budget{}), tokens(1))
	refusedQuota(t, "tenant envelope from two seeded consumers", d, keyquota.ReasonTenantEnvelope, 12*time.Hour-time.Minute)
	_, d = m.Admit(consumerOf("c6", "t3"), budgetLimits(day, keyquota.Budget{}, keyquota.Budget{DailyTokens: 2200}), tokens(1))
	refusedQuota(t, "instance envelope from every seeded chat row", d, keyquota.ReasonInstanceEnvelope, 12*time.Hour-time.Minute)
	e := keyquota.Subject{ConsumerID: "c6", TenantID: "t3", Op: keyquota.OpEmbed}
	if _, d := m.Admit(e, budgetLimits(day, keyquota.Budget{}, keyquota.Budget{DailyTokens: 9001}), keyquota.Estimate{InputRunes: 1}); !d.Allowed {
		t.Errorf("embed under the instance envelope 9001 after 9000 seeded: %+v", d)
	}
	if err := m.Seed(c.now(), rows); err == nil {
		t.Error("a second Seed was accepted (it would count the rollup twice)")
	}
}

// TestSeedAcrossMidnight: a seed taken before 00:00 UTC whose meter admits
// after it counts the month but not the day that is over.
func TestSeedAcrossMidnight(t *testing.T) {
	c := newClock()
	at := time.Date(2026, 11, 5, 23, 59, 59, 0, time.UTC)
	m := keyquota.New(c.now)
	if err := m.Seed(at, []keyquota.SeedRow{{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpChat, DayTokens: 1000, MonthTokens: 1000}}); err != nil {
		t.Fatal(err)
	}
	c.add(12*time.Hour + time.Minute) // 2026-11-06 00:01 UTC
	l := budgetLimits(keyquota.Budget{DailyTokens: 1000, MonthlyTokens: 1500}, keyquota.Budget{}, keyquota.Budget{})
	admit(t, "the day after the seed", m, consumerOf("c1", "t1"), l, tokens(500)).Settle(keyquota.Usage{CompletionTokens: 500})
	_, d := m.Admit(consumerOf("c1", "t1"), l, tokens(1))
	refusedQuota(t, "the month still counts", d, keyquota.ReasonMonthlyTokens, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Sub(c.now()))
}

// TestSeedSkipsInvalidRows: a row without consumer, tenant or a known op
// counts nowhere (fail-closed for the subject, never a shared bucket).
func TestSeedSkipsInvalidRows(t *testing.T) {
	c := newClock()
	m := keyquota.New(c.now)
	if err := m.Seed(c.now(), []keyquota.SeedRow{
		{ConsumerID: "", TenantID: "t1", Op: keyquota.OpChat, DayTokens: 1000},
		{ConsumerID: "c1", TenantID: "", Op: keyquota.OpChat, DayTokens: 1000},
		{ConsumerID: "c1", TenantID: "t1", Op: "image", DayTokens: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	l := budgetLimits(keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{DailyTokens: 1000})
	admit(t, "nothing counted", m, consumerOf("c1", "t1"), l, tokens(1000))
}

// TestBurstDampingAfterBoot is gate (g): 30 consumers that were active this
// month arriving at once in the first minute after the seed get half the
// burst each (τ/2: 15 of RPM 30); one of them arriving a window after the
// seed gets the whole burst, and so does a consumer without a row in the
// month — it used nothing, its bucket was full before the restart too (the
// deviation of the W06-13 report). Red without the damping: 30 each.
func TestBurstDampingAfterBoot(t *testing.T) {
	c := newClock()
	m := keyquota.New(c.now)
	var rows []keyquota.SeedRow
	for i := range 31 {
		rows = append(rows, keyquota.SeedRow{ConsumerID: "boot-" + string(rune('A'+i)), TenantID: "t1", Op: keyquota.OpChat, DayTokens: 1})
	}
	if err := m.Seed(c.now(), rows); err != nil {
		t.Fatal(err)
	}
	l := keyquota.Limits{RPM: 30, TPM: 1 << 30, Concurrency: noL1}
	burst := func(id string) int {
		n := 0
		for range 60 {
			res, d := m.Admit(consumerOf(id, "t1"), l, small)
			if !d.Allowed {
				break
			}
			res.Abort()
			n++
		}
		return n
	}
	c.add(30 * time.Second)
	for i := range 30 {
		if n := burst("boot-" + string(rune('A'+i))); n != 15 {
			t.Fatalf("consumer %d in the first window: burst %d, want 15 (τ/2)", i, n)
		}
	}
	if n := burst("idle-this-month"); n != 30 {
		t.Errorf("a consumer without a row in the month: burst %d, want 30 (τ)", n)
	}
	c.add(30 * time.Second)
	if n := burst("boot-" + string(rune('A'+30))); n != 30 {
		t.Errorf("an active consumer born a window after the seed: burst %d, want 30 (τ)", n)
	}
}
