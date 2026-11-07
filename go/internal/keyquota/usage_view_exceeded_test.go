package keyquota_test

// W06-18 S2 hardening (Fremd-Review S2 F7/P5b): the hard_limit key of a
// monthly refusal is the month's window — the 1st of the month 00:00 UTC —
// not the day's; a day window there would write up to 30 hard_limit rows a
// month (B-06-21).

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

func TestExceededNamesTheWindow(t *testing.T) {
	c := newClock() // 2026-11-05 12:00 UTC
	m := seeded(c)
	s := consumerOf("c1", "t1")
	l := budgetLimits(keyquota.Budget{DailyTokens: 10_000, MonthlyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	admit(t, "first", m, s, l, tokens(900)).Settle(keyquota.Usage{PromptTokens: 900})
	_, d := m.Admit(s, l, tokens(200))
	want := keyquota.Warning{Level: keyquota.LevelConsumer, Dimension: keyquota.ReasonMonthlyTokens, Used: 900, Limit: 1000,
		WindowStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if d.Allowed || d.Exceeded == nil || *d.Exceeded != want {
		t.Fatalf("W06-18 gate (hard_limit window): %+v (exceeded %+v), want refused with %+v", d, d.Exceeded, want)
	}
	day := keyquota.Budget{DailyTokens: 1000}
	l2 := budgetLimits(day, keyquota.Budget{}, keyquota.Budget{})
	s2 := consumerOf("c2", "t1")
	admit(t, "day", m, s2, l2, tokens(900)).Settle(keyquota.Usage{PromptTokens: 900})
	_, d = m.Admit(s2, l2, tokens(200))
	if d.Exceeded == nil || !d.Exceeded.WindowStart.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) || d.Exceeded.Dimension != keyquota.ReasonDailyTokens {
		t.Errorf("W06-18 gate (hard_limit window): day refusal exceeded %+v, want daily_tokens of 2026-11-05", d.Exceeded)
	}
}
