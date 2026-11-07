package keyquota_test

// The day and month budgets of the meter (wave W06-13, design/06 §4.2 rows
// "Tag/Monat Konsument", "Tenant-Hülle", "Instanz-Hülle", §4.3, §4.4):
// calendar-UTC windows per (consumer, op), per (tenant, op) and per op, used
// per window plus the window-independent reserved tokens of the open
// reservations; a refusal is not retryable and waits until the window resets.

import (
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

// budgetLimits are limits whose minute buckets never bind: only the budgets
// under test do.
func budgetLimits(consumer, tenant, instance keyquota.Budget) keyquota.Limits {
	return keyquota.Limits{RPM: 1 << 30, TPM: 1 << 40, Concurrency: noL1, XUReserve: 0.25,
		Consumer: consumer, Tenant: tenant, Instance: instance}
}

// tokens is a chat estimate of exactly n tokens (no prompt, a client
// maximum of n).
func tokens(n int) keyquota.Estimate { return keyquota.Estimate{MaxCompletion: n, ClientMax: true} }

func consumerOf(id, tenant string) keyquota.Subject {
	return keyquota.Subject{ConsumerID: id, TenantID: tenant, Op: keyquota.OpChat}
}

func admit(t *testing.T, what string, m *keyquota.Meter, s keyquota.Subject, l keyquota.Limits, e keyquota.Estimate) *keyquota.Reservation {
	t.Helper()
	res, d := m.Admit(s, l, e)
	if !d.Allowed || res == nil {
		t.Fatalf("%s: %+v, want admitted", what, d)
	}
	return res
}

func refusedQuota(t *testing.T, what string, d keyquota.Decision, reason keyquota.Reason, retry time.Duration) {
	t.Helper()
	if d.Allowed || d.Reason != reason || d.Retryable || d.RetryAfter != retry {
		t.Fatalf("%s: %+v, want refused %s, not retryable, Retry-After %v", what, d, reason, retry)
	}
}

// TestDailyBudgetGate is gate (a): chat_daily_tokens 1000, 800 real tokens,
// a follow-up reservation of 400 ⇒ insufficient quota, not retryable,
// Retry-After = the time until 00:00 UTC; at midnight the window is fresh.
func TestDailyBudgetGate(t *testing.T) {
	c := newClock() // 2026-11-05 12:00 UTC
	m := seeded(c)
	l := budgetLimits(keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	s := consumerOf("c1", "t1")
	admit(t, "first", m, s, l, tokens(100)).Settle(keyquota.Usage{PromptTokens: 500, CompletionTokens: 300})
	_, d := m.Admit(s, l, tokens(400))
	refusedQuota(t, "800 used + 400", d, keyquota.ReasonDailyTokens, 12*time.Hour)
	if want := "Daily token quota reached (chat): 1000 tokens per UTC day."; d.Message != want {
		t.Errorf("message %q, want %q", d.Message, want)
	}
	admit(t, "800 used + 200 fits exactly", m, s, l, tokens(200)).Abort()
	c.add(12*time.Hour - time.Second)
	_, d = m.Admit(s, l, tokens(400))
	refusedQuota(t, "one second before midnight", d, keyquota.ReasonDailyTokens, time.Second)
	c.add(time.Second)
	admit(t, "at 00:00 UTC", m, s, l, tokens(1000))
}

// TestMonthlyAndRequestBudgets: the month window resets on the 1st at 00:00
// UTC; the request budget counts an admitted request even when it ends
// without a wire call (design/06 §4.2: the request stays counted).
func TestMonthlyAndRequestBudgets(t *testing.T) {
	c := newClock()
	m := seeded(c)
	s := consumerOf("c1", "t1")
	month := budgetLimits(keyquota.Budget{DailyTokens: 1 << 40, MonthlyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	admit(t, "month first", m, s, month, tokens(10)).Settle(keyquota.Usage{CompletionTokens: 1000})
	c.add(24 * time.Hour) // the day rolled, the month did not
	_, d := m.Admit(s, month, tokens(1))
	refusedQuota(t, "month exhausted", d, keyquota.ReasonMonthlyTokens, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Sub(c.now()))

	req := budgetLimits(keyquota.Budget{DailyTokens: 1 << 40, DailyRequests: 2}, keyquota.Budget{}, keyquota.Budget{})
	r := consumerOf("c2", "t1")
	admit(t, "request 1", m, r, req, tokens(1)).Abort()
	admit(t, "request 2", m, r, req, tokens(1)).Keep()
	_, d = m.Admit(r, req, tokens(1))
	refusedQuota(t, "request 3 of 2", d, keyquota.ReasonDailyRequests, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC).Sub(c.now()))
}

// TestReservedCountsAndTheCap: an open reservation holds its tokens against
// the budget until it ends (used + reserved + n_d ≤ L); a reservation above
// the budget is charged at it (n_d = min(n, L)) and admitted on an empty
// budget — never refused for good; the bulk lane fills at most (1 − r)·L and
// its own cost is capped at that share.
func TestReservedCountsAndTheCap(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := budgetLimits(keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	s := consumerOf("c1", "t1")
	open := admit(t, "600 open", m, s, l, tokens(600))
	_, d := m.Admit(s, l, tokens(600))
	refusedQuota(t, "600 + 600 reserved", d, keyquota.ReasonDailyTokens, 12*time.Hour)
	open.Abort()
	admit(t, "after the abort", m, s, l, tokens(600)).Abort()

	big := consumerOf("c-big", "t1")
	admit(t, "5000 on an empty budget of 1000", m, big, l, tokens(5000)).Settle(keyquota.Usage{CompletionTokens: 1200})
	_, d = m.Admit(big, l, tokens(1))
	refusedQuota(t, "after a real overrun", d, keyquota.ReasonDailyTokens, 12*time.Hour)

	bulk := keyquota.Subject{ConsumerID: "c-bulk", TenantID: "t1", Op: keyquota.OpChat, Lane: keyquota.LaneBulk}
	urgent := keyquota.Subject{ConsumerID: "c-bulk", TenantID: "t1", Op: keyquota.OpChat}
	admit(t, "bulk 5000 on an empty budget", m, bulk, l, tokens(5000)).Settle(keyquota.Usage{CompletionTokens: 700})
	_, d = m.Admit(bulk, l, tokens(100))
	refusedQuota(t, "bulk over (1 − r)·L = 750", d, keyquota.ReasonDailyTokens, 12*time.Hour)
	admit(t, "urgent keeps the reserve", m, urgent, l, tokens(300))
}

// TestEnvelopeBindsBeforeTheSum is the masterplan gate "Hülle bindet vor
// Summe" (B-06-6, gate (f)): three consumers of one tenant with a daily
// default of 1000 under the tenant envelope factor 2 stop at 2000 together,
// though their own budgets add up to 3000; ten consumers under an instance
// envelope of 5000 stop at 5000, and that refusal names no value.
func TestEnvelopeBindsBeforeTheSum(t *testing.T) {
	c := newClock()
	m := seeded(c)
	own := keyquota.Budget{DailyTokens: 1000}
	l := budgetLimits(own, own.Scale(2), own.Scale(1000))
	var total int
	var last keyquota.Decision
	for i := range 3 {
		s := consumerOf("c"+string(rune('a'+i)), "t-env")
		for {
			res, d := m.Admit(s, l, tokens(100))
			if !d.Allowed {
				last = d
				break
			}
			res.Settle(keyquota.Usage{CompletionTokens: 100})
			total += 100
		}
	}
	if total != 2000 {
		t.Fatalf("three consumers used %d, want 2000 (the tenant envelope, not Σ 3000)", total)
	}
	refusedQuota(t, "third consumer", last, keyquota.ReasonTenantEnvelope, 12*time.Hour)
	if strings.ContainsAny(last.Message, "0123456789") {
		t.Errorf("tenant envelope message %q names a value", last.Message)
	}

	m = seeded(c) // a fresh instance: the envelope counts every consumer of the op
	inst := budgetLimits(own, own.Scale(1000), keyquota.Budget{DailyTokens: 5000})
	total = 0
	for i := range 10 {
		s := consumerOf("i"+string(rune('a'+i)), "t-inst-"+string(rune('a'+i%3)))
		res, d := m.Admit(s, inst, tokens(600))
		if !d.Allowed {
			last = d
			continue
		}
		res.Settle(keyquota.Usage{CompletionTokens: 600})
		total += 600
	}
	if total != 4800 {
		t.Fatalf("ten consumers used %d, want 4800 (eight of 600 under the instance envelope 5000)", total)
	}
	refusedQuota(t, "instance envelope", last, keyquota.ReasonInstanceEnvelope, 12*time.Hour)
	if want := "Capacity quota for this deployment reached (chat)."; last.Message != want {
		t.Errorf("instance message %q, want %q (no value, B6)", last.Message, want)
	}
}

// TestEnvelopeCountsOnlyItsOp: the budgets are per op — an exhausted chat
// budget leaves embed of the same consumer and tenant alone.
func TestEnvelopeCountsOnlyItsOp(t *testing.T) {
	c := newClock()
	m := seeded(c)
	own := keyquota.Budget{DailyTokens: 100}
	l := budgetLimits(own, own.Scale(1), own.Scale(1))
	s := consumerOf("c1", "t1")
	admit(t, "chat", m, s, l, tokens(10)).Settle(keyquota.Usage{CompletionTokens: 100})
	if _, d := m.Admit(s, l, tokens(1)); d.Allowed {
		t.Fatal("chat over its budget admitted")
	}
	e := keyquota.Subject{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpEmbed}
	admit(t, "embed of the same consumer", m, e, l, keyquota.Estimate{InputRunes: 9})
}

// TestBudgetScale: a factor scales every bounded dimension and leaves an
// unbounded one (0) unbounded; the product saturates.
func TestBudgetScale(t *testing.T) {
	b := keyquota.Budget{DailyTokens: 2_000_000, MonthlyTokens: 0, DailyRequests: 7}.Scale(5)
	if b.DailyTokens != 10_000_000 || b.MonthlyTokens != 0 || b.DailyRequests != 35 {
		t.Errorf("scale 5: %+v", b)
	}
	if b := (keyquota.Budget{DailyTokens: 1 << 62}).Scale(10); b.DailyTokens != 1<<63-1 {
		t.Errorf("saturation: %d", b.DailyTokens)
	}
	if b := (keyquota.Budget{DailyTokens: 3}).Scale(1.5); b.DailyTokens != 4 {
		t.Errorf("scale 1.5 of 3: %d, want 4 (floor)", b.DailyTokens)
	}
}

// TestBudgetSurvivesRotation is gate (e) at the meter (B-06-19): the meter's
// key is the consumer, never the model key — a rotated or reactivated key of
// the same consumer finds the budget its predecessor used.
func TestBudgetSurvivesRotation(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := budgetLimits(keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	s := consumerOf("c-rot", "t1")
	admit(t, "80 %", m, s, l, tokens(10)).Settle(keyquota.Usage{CompletionTokens: 800})
	if _, d := m.Admit(s, l, tokens(400)); d.Allowed || d.Reason != keyquota.ReasonDailyTokens {
		t.Fatalf("after rotation: %+v, want daily_tokens", d)
	}
}
