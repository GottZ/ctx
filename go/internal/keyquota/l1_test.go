package keyquota_test

// L1 of the meter (wave W06-11, design/06 §4.2 row "L1 Concurrency", §4.4
// row concurrency, B-06-27): C requests per (consumer, op) open at once —
// admitted and not ended — the bulk lane at most max(1, ⌊C·(1 − r)⌋) of them;
// one more is refused at once with Retry-After 1 s, and a refusal books
// nothing.

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

// noL1 keeps L1 out of the way of a test about L0: every reservation fits.
const noL1 = math.MaxInt64

// TestL1RefusesOneMoreThanC: with C open reservations of (consumer, op) the
// next request is refused — reason concurrency, Retry-After exactly 1 s,
// retryable, the message naming C and nothing else — and the refusal books
// no request; each of the four ends frees its slot; other ops and other
// consumers keep their own slots.
func TestL1RefusesOneMoreThanC(t *testing.T) {
	for _, c := range []int64{1, 2, 5} {
		m := seeded(newClock())
		l := keyquota.Limits{RPM: 1000, TPM: 1 << 30, Concurrency: c}
		var open []*keyquota.Reservation
		for i := range c {
			r, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
			if !d.Allowed {
				t.Fatalf("C=%d: request %d of C refused: %+v", c, i+1, d)
			}
			open = append(open, r)
		}
		_, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
		want := "Concurrency limit reached for requests (chat): " + strconv.FormatInt(c, 10) + " at a time."
		if d.Allowed || d.Reason != keyquota.ReasonConcurrency || d.RetryAfter != time.Second || !d.Retryable || d.Message != want {
			t.Fatalf("C=%d: request C+1: %+v, want refused concurrency, Retry-After 1s, retryable, message %q", c, d, want)
		}
		if d.RetryAfter != keyquota.ConcurrencyRetryAfter {
			t.Errorf("C=%d: Retry-After %v, want the named constant %v", c, d.RetryAfter, keyquota.ConcurrencyRetryAfter)
		}
		// Another op and another consumer are untouched.
		embed := keyquota.Subject{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpEmbed}
		other := keyquota.Subject{ConsumerID: "c2", TenantID: "t1", Op: keyquota.OpChat}
		for _, s := range []keyquota.Subject{embed, other} {
			r, d := m.Admit(s, l, small)
			if !d.Allowed {
				t.Errorf("C=%d: %s/%s refused next to a full chat L1 of c1: %+v", c, s.ConsumerID, s.Op, d)
				continue
			}
			r.Abort()
		}
		// Each end frees one slot: the next request passes, and the refused
		// ones booked no request (requests counted: C, then one per pass).
		for i, end := range []func(*keyquota.Reservation){
			func(r *keyquota.Reservation) { r.Settle(keyquota.Usage{PromptTokens: 1}) },
			(*keyquota.Reservation).Keep,
			(*keyquota.Reservation).Abort,
			func(r *keyquota.Reservation) { r.Reject("server_busy", time.Second) },
		} {
			end(open[0])
			end(open[0]) // a second end frees nothing more
			r, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
			if !d.Allowed {
				t.Fatalf("C=%d: after end %d the next request was refused: %+v", c, i, d)
			}
			if got, want := d.Headers.RemainingRequests, 1000-(c+int64(i)+1); got != want {
				t.Errorf("C=%d: after end %d remaining-requests %d, want %d (an L1 refusal books no request)", c, i, got, want)
			}
			open[0] = r
			if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); d.Allowed || d.Reason != keyquota.ReasonConcurrency {
				t.Fatalf("C=%d: after end %d and one pass, the next: %+v, want refused concurrency", c, i, d)
			}
		}
	}
}

// TestL1LaneReserve is B-06-27 for L1: the bulk lane holds at most
// max(1, ⌊C·(1 − r)⌋) slots — refused beyond that with its share in the
// message — and the urgent lane of the same consumer keeps the rest; the
// bulk count is the bulk lane's own (an urgent request in flight does not
// shrink it), and every request counts against C. At C = 1 there is no
// reserve (the named bound); at r = 0 the bulk lane may hold all of C.
func TestL1LaneReserve(t *testing.T) {
	type step struct {
		lane  keyquota.Lane
		allow bool
		msg   string // the refusal's message
	}
	xu, xb := keyquota.LaneUrgent, keyquota.LaneBulk
	for _, tc := range []struct {
		name  string
		c     int64
		r     float64
		steps []step
	}{
		{"C=2 r=0.25: one bulk, the urgent lane keeps one", 2, 0.25, []step{
			{xb, true, ""},
			{xb, false, "Concurrency limit reached for requests (chat, bulk lane): 1 at a time."},
			{xu, true, ""},
			{xu, false, "Concurrency limit reached for requests (chat): 2 at a time."},
			{xb, false, "Concurrency limit reached for requests (chat): 2 at a time."},
		}},
		{"C=2 r=0.25: an urgent request in flight leaves the bulk share", 2, 0.25, []step{
			{xu, true, ""},
			{xb, true, ""},
			{xb, false, "Concurrency limit reached for requests (chat): 2 at a time."},
		}},
		{"C=4 r=0.25: three bulk", 4, 0.25, []step{
			{xb, true, ""}, {xb, true, ""}, {xb, true, ""},
			{xb, false, "Concurrency limit reached for requests (chat, bulk lane): 3 at a time."},
			{xu, true, ""},
			{xu, false, "Concurrency limit reached for requests (chat): 4 at a time."},
		}},
		{"C=1: no reserve", 1, 0.25, []step{
			{xb, true, ""},
			{xu, false, "Concurrency limit reached for requests (chat): 1 at a time."},
		}},
		{"C=3 r=0.5: one bulk", 3, 0.5, []step{
			{xb, true, ""},
			{xb, false, "Concurrency limit reached for requests (chat, bulk lane): 1 at a time."},
			{xu, true, ""}, {xu, true, ""},
		}},
		{"C=2 r=0: the bulk lane may hold all", 2, 0, []step{
			{xb, true, ""}, {xb, true, ""},
			{xu, false, "Concurrency limit reached for requests (chat): 2 at a time."},
		}},
	} {
		m := seeded(newClock())
		l := keyquota.Limits{RPM: 1000, TPM: 1 << 30, XUReserve: tc.r, Concurrency: tc.c}
		for i, s := range tc.steps {
			_, d := m.Admit(chat(s.lane), l, small)
			if d.Allowed != s.allow || (!s.allow && (d.Reason != keyquota.ReasonConcurrency || d.Message != s.msg || d.RetryAfter != time.Second)) {
				t.Errorf("%s: step %d (lane %d): %+v, want allowed %v %q", tc.name, i+1, s.lane, d, s.allow, s.msg)
			}
		}
	}
}

// TestL1AfterL0: a request L0 refuses names L0's reason even when L1 is full
// too (design/06 §4.1: step 8 before step 9), and an L1 refusal books
// neither a request nor its tokens.
func TestL1AfterL0(t *testing.T) {
	m := seeded(newClock())
	l := keyquota.Limits{RPM: 1, TPM: 60000, Concurrency: 1}
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); d.Reason != keyquota.ReasonRequests {
		t.Errorf("second at 1 rpm with L1 full: %+v, want rate_requests (L0 first)", d)
	}

	m = seeded(newClock())
	l = keyquota.Limits{RPM: 1000, TPM: 60000, Concurrency: 1}
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	big := keyquota.Estimate{PromptRunes: runesFor(30000), MaxCompletion: 1, ClientMax: true}
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, big); d.Reason != keyquota.ReasonConcurrency {
		t.Fatalf("a 30000-token request with L1 full: %+v, want concurrency", d)
	}
	l.Concurrency = 2
	_, d := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{ClientMax: true})
	if want := 60000 - small.Tokens(keyquota.OpChat, l); !d.Allowed || d.Headers.RemainingTokens != want || d.Headers.RemainingRequests != 1000-2 {
		t.Errorf("probe after the L1 refusal: %+v, want remaining-tokens %d and remaining-requests 998 (the refused request booked nothing)", d, want)
	}
}

// TestL1IsNeverUnbounded: a concurrency limit below 1 is a wiring defect —
// refused as invalid, not retryable, nothing booked (design/04 R-06-4).
func TestL1IsNeverUnbounded(t *testing.T) {
	m := seeded(newClock())
	for _, c := range []int64{0, -1, math.MinInt64} {
		res, d := m.Admit(chat(keyquota.LaneUrgent), keyquota.Limits{RPM: 10, TPM: 1000, Concurrency: c}, small)
		if res != nil || d.Allowed || d.Reason != keyquota.ReasonInvalid || d.Retryable {
			t.Errorf("concurrency %d: %+v, want refused invalid, not retryable", c, d)
		}
	}
	if _, d := m.Admit(chat(keyquota.LaneUrgent), keyquota.Limits{RPM: 10, TPM: 1000, Concurrency: 1}, small); !d.Allowed || d.Headers.RemainingRequests != 9 {
		t.Errorf("after the invalid ones: %+v, want admitted with 9 requests left (nothing booked)", d)
	}
}

// TestL1BoundsTheBulkDebt is the lane gate with an overrun (Fremd-Review 2 of
// W06-10, F-4, handed to W06-11): the XU reserve of the token bucket guards
// the admission, not the bucket — every concurrent bulk request may settle
// far above its reservation. The review's probe (TPM 60 000, r 0.25, a bulk
// reservation of 1 074 tokens, 4 050 real) admitted 41 concurrent bulk
// requests whose debt then refused the urgent lane for 1m48s. Under L1 at
// the default C = 2 the bulk lane holds one slot: one bulk request is
// admitted, the 40 others are refused by L1, and after its overrun the
// urgent lane still passes.
func TestL1BoundsTheBulkDebt(t *testing.T) {
	m := seeded(newClock())
	l := keyquota.Limits{RPM: 1000, TPM: 60000, CompletionReserve: 1024, XUReserve: 0.25, Concurrency: 2}
	bulkEst := keyquota.Estimate{PromptRunes: runesFor(50), MaxCompletion: 4096}
	if n := bulkEst.Tokens(keyquota.OpChat, l); n != 1074 {
		t.Fatalf("bulk reservation %d, want the probe's 1074", n)
	}
	var admitted []*keyquota.Reservation
	refusedL1 := 0
	for range 41 {
		r, d := m.Admit(chat(keyquota.LaneBulk), l, bulkEst)
		switch {
		case d.Allowed:
			admitted = append(admitted, r)
		case d.Reason == keyquota.ReasonConcurrency:
			refusedL1++
		default:
			t.Fatalf("bulk refused by L0: %+v", d)
		}
	}
	for _, r := range admitted {
		r.Settle(keyquota.Usage{PromptTokens: 50, CompletionTokens: 4000})
	}
	_, d := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{PromptRunes: runesFor(50), MaxCompletion: 4096})
	t.Logf("bulk admitted %d concurrent, refused by L1 %d; urgent after the settles: allowed=%v reason=%s Retry-After %v remaining-tokens %d",
		len(admitted), refusedL1, d.Allowed, d.Reason, d.RetryAfter, d.Headers.RemainingTokens)
	if len(admitted) != 1 || refusedL1 != 40 {
		t.Errorf("bulk admitted %d, refused by L1 %d — want 1 and 40 (the bulk lane's share of C = 2)", len(admitted), refusedL1)
	}
	if !d.Allowed {
		t.Errorf("urgent after the bulk overrun: %+v, want admitted (the reserve holds with one bulk request in flight)", d)
	}
}
