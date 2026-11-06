package keyquota_test

// Hardening after the second review of W06-10 (Fremd-Review 2) and the
// delta review 3: the refund of a reservation is what booking its real
// usage at the admission would have left — min(charged − cost(actual),
// backlog − its shadow's backlog), the shadow being the bucket of the
// charges after its admission (H-1, made exact by the shadow rule in H2-1;
// oracle_internal_test.go holds the differential), a debt is booked from
// the settle on (H-2), the reset header rounds up (H-5, R9) and the meter
// does not follow a step of the wall clock (H-6).

import (
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/GottZ/ctx/internal/keyquota"
)

// TestHeldDecoysBuyNoBurst is H-1 (review 2 F-1): k reservations of the
// whole TPM bucket are held one minute each while their charges run off,
// then a request books the whole bucket and uses it, and each held decoy
// ends with usage 0 followed by another whole-bucket request. The refund of
// an expired charge must not erase the charge of a later request: within
// the short window at most L real tokens pass, never (k+1)·L — whichever
// end the decoys take: Abort (a client gone), Settle with no usage, Reject
// (a refusal after the dispatcher wait, the realistic path; review 3 F-7).
func TestHeldDecoysBuyNoBurst(t *testing.T) {
	ends := map[string]func(*keyquota.Reservation){
		"abort":  func(r *keyquota.Reservation) { r.Abort() },
		"settle": func(r *keyquota.Reservation) { r.Settle(keyquota.Usage{}) },
		"reject": func(r *keyquota.Reservation) { r.Reject("server_busy", 20*time.Second) },
	}
	for name, end := range ends {
		t.Run(name, func(t *testing.T) { heldDecoys(t, end) })
	}
}

func heldDecoys(t *testing.T, end func(*keyquota.Reservation)) {
	const L = 60000
	l := keyquota.Limits{Concurrency: noL1, RPM: 30, TPM: L, CompletionReserve: 1024}
	whole := keyquota.Estimate{MaxCompletion: L, ClientMax: true} // capped at L
	for _, decoys := range []int{1, 2, 9} {
		c := newClock()
		m := seeded(c)
		s := chat(keyquota.LaneUrgent)
		var held []*keyquota.Reservation
		for i := range decoys {
			r, d := m.Admit(s, l, whole)
			if !d.Allowed {
				t.Fatalf("decoys=%d: decoy %d refused: %+v", decoys, i, d)
			}
			held = append(held, r)
			c.add(time.Minute)
		}
		start := c.now()
		real, served := 0, 0
		var lastRefusal keyquota.Decision
		if r, d := m.Admit(s, l, whole); d.Allowed {
			r.Settle(keyquota.Usage{PromptTokens: L})
			real += L
			served++
		}
		for _, h := range held {
			c.add(10 * time.Millisecond)
			end(h)
			r, d := m.Admit(s, l, whole)
			if d.Allowed {
				r.Settle(keyquota.Usage{PromptTokens: L})
				real += L
				served++
				continue
			}
			lastRefusal = d
		}
		t.Logf("decoys=%d held up to %v: %d requests admitted with %d real tokens within %v (limit %d/min, ratio %.1f×L); last refusal Retry-After %v",
			decoys, time.Duration(decoys)*time.Minute, served, real, c.now().Sub(start), L, float64(real)/L, lastRefusal.RetryAfter)
		if real > L {
			t.Errorf("decoys=%d: %d real tokens within %v at TPM %d — the expired decoys' refunds erased a later charge", decoys, real, c.now().Sub(start), L)
		}
		if lastRefusal.RetryAfter < 59*time.Second {
			t.Errorf("decoys=%d: the last follow-up's Retry-After %v, want ≥ 59 s (the whole bucket is still owed)", decoys, lastRefusal.RetryAfter)
		}
	}
}

// TestSettleBeforeExpiryRefundsTheUnexpiredPart is the counter-check of the
// refund rule: A books 1 042 tokens at t = 0 (1 ms per token at 60 000 tpm),
// B books 1 000 behind it at the same instant; A settles at 1 s with 14
// real tokens. The bucket never drained below 1.042 s while A was open, so
// none of A's inflation ran off: all 1 028 unused tokens come back, as if A
// had booked its 14 at the admission — the bucket holds 14 ms (59 986
// remaining). Until hardening 2 this gate pinned the FIFO approximation of
// H-1 (only A's "unexpired" 42 ms back, 59 000 remaining), 986 tokens
// stricter than the oracle for up to 1 s (review 3 F-2 (b)).
func TestSettleBeforeExpiryRefundsTheUnexpiredPart(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{Concurrency: noL1, RPM: 100, TPM: 60000}
	s := chat(keyquota.LaneUrgent)
	a, _ := m.Admit(s, l, keyquota.Estimate{MaxCompletion: 1042, ClientMax: true})
	if _, d := m.Admit(s, l, keyquota.Estimate{MaxCompletion: 1000, ClientMax: true}); !d.Allowed || d.Headers.ResetTokens != 2042*time.Millisecond {
		t.Fatalf("B: %+v, want admitted behind A (bucket 2.042 s)", d)
	}
	c.add(time.Second)
	a.Settle(keyquota.Usage{PromptTokens: 14})
	_, d := m.Admit(s, l, keyquota.Estimate{ClientMax: true})
	if d.Headers.ResetTokens != 14*time.Millisecond || d.Headers.RemainingTokens != 59986 {
		t.Errorf("after A settled 14 at 1 s: bucket %v, remaining %d — want 14ms and 59986 (the oracle: A's 14 and B's 1 000 booked at 0, 1 s run off)",
			d.Headers.ResetTokens, d.Headers.RemainingTokens)
	}
}

// TestAbortRightAfterAdmissionRefundsInFull: an aborted reservation whose
// charge the bucket still holds (it never drained below 40 s while the
// reservation was open) gets its whole charge back — the oracle's value:
// the first charge (20 000, never ended) ran 10 s, the aborted one booked 0.
func TestAbortRightAfterAdmissionRefundsInFull(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{Concurrency: noL1, RPM: 100, TPM: 60000}
	s := chat(keyquota.LaneUrgent)
	if _, d := m.Admit(s, l, keyquota.Estimate{MaxCompletion: 20000, ClientMax: true}); !d.Allowed {
		t.Fatal(d)
	}
	r, _ := m.Admit(s, l, keyquota.Estimate{MaxCompletion: 30000, ClientMax: true})
	c.add(10 * time.Second)
	r.Abort()
	_, d := m.Admit(s, l, keyquota.Estimate{ClientMax: true})
	if d.Headers.ResetTokens != 10*time.Second || d.Headers.RemainingTokens != 50000 {
		t.Errorf("after the abort: bucket %v, remaining %d — want 10 s and 50000 (the first charge ran 10 s of its 20 s, the aborted one came back whole)",
			d.Headers.ResetTokens, d.Headers.RemainingTokens)
	}
}

// TestDebtIsBookedFromTheSettle is H-2: an overrun is booked from the settle
// on (max(tat, now)), also when the request's own charge ran off long ago —
// the overrun was used, its moment inside the request is unknown, and
// booking it from the settle never counts too little. A small request
// settled two minutes after its admission with 90 000 real tokens leaves the
// next one 30 s of debt to wait off at 60 000 tpm.
func TestDebtIsBookedFromTheSettle(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{Concurrency: noL1, RPM: 100, TPM: 60000}
	s := chat(keyquota.LaneUrgent)
	r, _ := m.Admit(s, l, small)
	c.add(2 * time.Minute)
	r.Settle(keyquota.Usage{PromptTokens: 89000, CompletionTokens: 1000})
	_, d := m.Admit(s, l, small)
	if d.Allowed || d.Reason != keyquota.ReasonTokens || d.RetryAfter != 30*time.Second {
		t.Errorf("after an overrun settled late: %+v, want refused rate_tokens with Retry-After exactly 30s", d)
	}
}

// TestResetRoundsUp is H-5 (review 2 R9): a reset with a fraction of a
// millisecond is rounded up — at 7 rpm one request leaves 8.571428571 s.
func TestResetRoundsUp(t *testing.T) {
	m := seeded(newClock())
	r, _ := m.Admit(chat(keyquota.LaneUrgent), keyquota.Limits{Concurrency: noL1, RPM: 7, TPM: 60000}, small)
	if got := r.Headers().Get("x-ratelimit-reset-requests"); got != "8.572s" {
		t.Errorf("x-ratelimit-reset-requests %q, want 8.572s (8.571428571 s rounded up)", got)
	}
}

// TestWallClockStepLeavesTheMeterAlone is H-6 (review 2 F-7, taken over
// from its probe): a time.Time whose wall reading steps by ±1 h while its
// monotonic reading does not (built over the field layout of time.Time,
// Go 1.25) leaves the meter untouched — no consumer locked out for an hour,
// no bucket refilled.
func TestWallClockStepLeavesTheMeterAlone(t *testing.T) {
	type rawTime struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	const hasMonotonic = 1 << 63
	const nsecShift = 30
	stepWall := func(tm time.Time, d time.Duration) time.Time {
		r := (*rawTime)(unsafe.Pointer(&tm))
		if r.wall&hasMonotonic == 0 {
			t.Fatal("no monotonic reading")
		}
		secs := int64(r.wall<<1>>(nsecShift+1)) + int64(d/time.Second)
		r.wall = hasMonotonic | uint64(secs)<<nsecShift | (r.wall & (1<<nsecShift - 1))
		return tm
	}
	for _, step := range []time.Duration{-time.Hour, time.Hour} {
		var mu sync.Mutex
		wallStep := time.Duration(0)
		now := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			tm := time.Now()
			if wallStep != 0 {
				tm = stepWall(tm, wallStep)
			}
			return tm
		}
		probe := now()
		stepped := stepWall(probe, step)
		if stepped.Sub(probe) != 0 || stepped.Round(0).Sub(probe.Round(0)) != step {
			t.Fatalf("construction: monotonic diff %v, wall diff %v", stepped.Sub(probe), stepped.Round(0).Sub(probe.Round(0)))
		}
		m := keyquota.New(now)
		m.MarkSeeded()
		l := keyquota.Limits{Concurrency: noL1, RPM: 2, TPM: 60000, CompletionReserve: 1024}
		_, d1 := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{})
		mu.Lock()
		wallStep = step
		mu.Unlock()
		_, d2 := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{})
		_, d3 := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{})
		t.Logf("wall step %v (monotonic unchanged): first %v; after the step: second %v, third %v Retry-After %v",
			step, d1.Allowed, d2.Allowed, d3.Allowed, d3.RetryAfter)
		if !d1.Allowed || !d2.Allowed || d3.Allowed || d3.RetryAfter > 31*time.Second || d3.RetryAfter < 29*time.Second {
			t.Errorf("wall step %v changed the meter: %+v / %+v / %+v", step, d1, d2, d3)
		}
	}
}

// TestAbsurdUsageLocksTheOpNamedLimit pins the named limit of review 2 F-2
// (W06-10 hardening H-3, not built): the debt has no bound. A usage report
// of 2³¹ tokens at 60 000 tpm books 2 147 483.648 s of debt on (consumer,
// op) — 24 h later the same request is still refused with Retry-After
// 572h30m24.728s (the debt minus the day, plus its own 1 080 tokens, minus
// the burst tolerance of one minute). Nothing resets it short of a process
// restart: the state is never removed and there is no admin action. The
// lock is narrow: the other ops of the same consumer and other consumers
// stay free. A byte bound on the settled usage is refuted by measurement
// (tool requests on SGLang and llama.cpp report more tokens than the body
// has bytes, logs/h3-usage-bound.log); this pin turns red when a usage
// bound, a debt cap or a reset lands — then rewrite it into that gate.
func TestAbsurdUsageLocksTheOpNamedLimit(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{Concurrency: noL1, RPM: 30, TPM: 60000, CompletionReserve: 1024}
	e := keyquota.Estimate{PromptRunes: 100} // 56 prompt tokens + 1 024 reserve
	locked := keyquota.Subject{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpChat}
	r, d := m.Admit(locked, l, e)
	if !d.Allowed {
		t.Fatalf("first request refused: %+v", d)
	}
	r.Settle(keyquota.Usage{PromptTokens: 1 << 31})
	c.add(24 * time.Hour)

	_, d = m.Admit(locked, l, e)
	const want = 572*time.Hour + 30*time.Minute + 24*time.Second + 728*time.Millisecond
	if d.Allowed || d.Reason != keyquota.ReasonTokens || d.RetryAfter != want || !d.Retryable {
		t.Fatalf("24 h after a usage of 2^31 tokens: %+v, want refused rate_tokens with Retry-After %v — the named limit changed; rewrite this pin into the gate of the usage bound", d, want)
	}
	for _, free := range []keyquota.Subject{
		{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpEmbed},
		{ConsumerID: "c2", TenantID: "t1", Op: keyquota.OpChat},
	} {
		if _, d := m.Admit(free, l, e); !d.Allowed {
			t.Fatalf("%s/%s after c1/chat's absurd usage: %+v, want admitted — the lock reaches beyond (consumer, op)", free.ConsumerID, free.Op, d)
		}
	}
	t.Logf("named limit: a usage of 2^31 tokens locks c1/chat, 24 h later Retry-After %v; c1/embed and c2/chat stay free", d.RetryAfter)
}
