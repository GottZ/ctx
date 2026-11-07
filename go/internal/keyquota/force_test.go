package keyquota_test

// Settle with an estimate and the shutdown's forced settle (wave W06-13 (d),
// (e); design/06 §4.2 "Usage-Quellen beim Settle", "Shutdown-Reihenfolge"
// step 2, B-06-8, B-06-9): AddStreamed counts the runes a stream handed on,
// OnForcedSettle registers the row writer, and ForceSettleAll ends every
// reservation still open with usage=estimated — each exactly once, whatever
// ended first.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/promptguard"
)

// TestForceSettleAllEndsEachOpenReservationOnce is gate (d) at the meter:
// five reservations open at the shutdown (streams that handed on N runes,
// no usage chunk) are each settled once with the estimate — prompt =
// TokenEstimate(prompt runes), completion = TokenEstimate(streamed runes),
// at most the request's maximum — and call their writer once with it; the
// budget books the estimate (at most what the reservation holds, like
// Keep); a reservation that ended before is left alone; a later end and a
// second ForceSettleAll change nothing; the L1 slots are free again.
func TestForceSettleAllEndsEachOpenReservationOnce(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := budgetLimits(keyquota.Budget{DailyTokens: 1 << 40}, keyquota.Budget{}, keyquota.Budget{})
	l.Concurrency = 5
	est := keyquota.Estimate{PromptRunes: runesFor(100), MaxCompletion: 1000, ClientMax: true}
	type call struct {
		n int
		u keyquota.Usage
	}
	calls := make([]call, 6)
	var open []*keyquota.Reservation
	for i := range 5 {
		r := admit(t, "open stream", m, consumerOf("c-force", "t1"), l, est)
		r.AddStreamed(90 * (i + 1))
		r.OnForcedSettle(func(u keyquota.Usage) { calls[i].n++; calls[i].u = u })
		open = append(open, r)
	}
	if _, d := m.Admit(consumerOf("c-force", "t1"), l, est); d.Allowed || d.Reason != keyquota.ReasonConcurrency {
		t.Fatalf("sixth while five open: %+v, want L1", d)
	}
	done := admit(t, "ended before", m, consumerOf("c-done", "t1"), l, est)
	done.OnForcedSettle(func(keyquota.Usage) { calls[5].n++ })
	done.Settle(keyquota.Usage{PromptTokens: 7})

	if n := m.ForceSettleAll(context.Background()); n != 5 {
		t.Fatalf("ForceSettleAll forced %d, want 5", n)
	}
	var booked int64
	for i, r := range open {
		want := keyquota.Usage{PromptTokens: promptguard.TokenEstimate(runesFor(100)), CompletionTokens: promptguard.TokenEstimate(90 * (i + 1))}
		if calls[i].n != 1 || calls[i].u != want || !r.Forced() {
			t.Errorf("reservation %d: writer called %d× with %+v (forced %v), want once with %+v", i, calls[i].n, calls[i].u, r.Forced(), want)
		}
		booked += min(int64(want.PromptTokens+want.CompletionTokens), r.Held())
		r.Settle(keyquota.Usage{CompletionTokens: 1 << 30}) // no-op: ended
	}
	if calls[5].n != 0 || done.Forced() {
		t.Errorf("a reservation that ended before: writer %d×, forced %v", calls[5].n, done.Forced())
	}
	if n := m.ForceSettleAll(context.Background()); n != 0 {
		t.Errorf("second ForceSettleAll forced %d, want 0", n)
	}
	// The day booked the estimates, never the huge late Settle.
	day := budgetLimits(keyquota.Budget{DailyTokens: booked + 1}, keyquota.Budget{}, keyquota.Budget{})
	day.Concurrency = 5
	admit(t, "one token left", m, consumerOf("c-force", "t1"), day, tokens(1)).Abort()
	day.Consumer.DailyTokens = booked
	if _, d := m.Admit(consumerOf("c-force", "t1"), day, tokens(1)); d.Allowed {
		t.Errorf("budget of exactly the booked estimates admitted one more token: %+v", d)
	}
	for range 5 {
		admit(t, "slot free after the forced settle", m, consumerOf("c-force", "t1"), l, est)
	}
}

// TestForceSettleRacesTheRoute: a route's own end and the forced settle
// race; exactly one of them ends each reservation — the writer runs iff the
// forced settle won (Forced), never both, never neither. Pinned with -race
// and -count=200.
func TestForceSettleRacesTheRoute(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 1 << 30, TPM: 1 << 40, Concurrency: noL1}
	const n = 64
	res := make([]*keyquota.Reservation, n)
	var writes [n]atomic.Int64
	for i := range res {
		res[i] = admit(t, "open", m, consumerOf("c-race", "t1"), l, small)
		res[i].AddStreamed(10)
		res[i].OnForcedSettle(func(keyquota.Usage) { writes[i].Add(1) })
	}
	var wg sync.WaitGroup
	for i, r := range res {
		wg.Go(func() {
			switch i % 3 {
			case 0:
				r.Settle(keyquota.Usage{PromptTokens: 1})
			case 1:
				r.Keep()
			default:
				r.Abort()
			}
		})
	}
	wg.Go(func() { m.ForceSettleAll(context.Background()) })
	wg.Wait()
	for i, r := range res {
		if w := writes[i].Load(); (w == 1) != r.Forced() || w > 1 {
			t.Fatalf("reservation %d: writer %d×, forced %v — want the writer exactly when forced", i, w, r.Forced())
		}
	}
}

// TestForceSettleAllHonoursTheContext: a done context stops the forced
// settle before the next reservation (the shutdown budget bounds it).
func TestForceSettleAllHonoursTheContext(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 1 << 30, TPM: 1 << 40, Concurrency: noL1}
	for range 3 {
		admit(t, "open", m, consumerOf("c-ctx", "t1"), l, small)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n := m.ForceSettleAll(ctx); n != 0 {
		t.Errorf("ForceSettleAll under a done context forced %d, want 0", n)
	}
}
