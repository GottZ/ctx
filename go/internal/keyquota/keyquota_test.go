package keyquota_test

import (
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
)

// clock is a fake clock: it moves only when a test says so.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func seeded(c *clock) *keyquota.Meter {
	m := keyquota.New(c.now)
	m.MarkSeeded()
	return m
}

func chat(lane keyquota.Lane) keyquota.Subject {
	return keyquota.Subject{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpChat, Lane: lane}
}

// small is a chat estimate of a few tokens with a client maximum of 1.
var small = keyquota.Estimate{PromptRunes: 9, MaxCompletion: 1, ClientMax: true}

// runesFor returns a prompt rune count whose TokenEstimate is tokens.
func runesFor(tokens int) int {
	r := tokens * 9 / 5
	for promptguard.TokenEstimate(r) < tokens {
		r++
	}
	for r > 0 && promptguard.TokenEstimate(r-1) >= tokens {
		r--
	}
	return r
}

// TestOpMatchesTheRowVocabulary pins Op against the op vocabulary of the /v1
// rows (llmlog.GatewayOp*): the adapter hands the route's op string through.
func TestOpMatchesTheRowVocabulary(t *testing.T) {
	for _, s := range []string{llmlog.GatewayOpChat, llmlog.GatewayOpEmbed, llmlog.GatewayOpRerank} {
		op, ok := keyquota.ParseOp(s)
		if !ok || string(op) != s {
			t.Errorf("ParseOp(%q) = %q, %v — want the row vocabulary's op", s, op, ok)
		}
	}
	for _, s := range []string{"", "Chat", "embedding", "chat "} {
		if _, ok := keyquota.ParseOp(s); ok {
			t.Errorf("ParseOp(%q) accepted", s)
		}
	}
}

// TestGCRABurstAndRetryAfter: rpm L ⇒ a burst of exactly L at one instant
// (τ = W), the L+1-th refused with Retry-After T = W/L exactly; after T the
// next one passes.
func TestGCRABurstAndRetryAfter(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 2, TPM: 1000}
	for i, want := range []int64{1, 0} {
		res, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
		if !d.Allowed || res == nil {
			t.Fatalf("request %d refused: %+v", i+1, d)
		}
		if d.Headers.RemainingRequests != want || d.Headers.LimitRequests != 2 {
			t.Errorf("request %d: headers %+v, want remaining %d of 2", i+1, d.Headers, want)
		}
	}
	res, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
	if d.Allowed || res != nil || d.Reason != keyquota.ReasonRequests || !d.Retryable || d.RetryAfter != 30*time.Second {
		t.Fatalf("request 3: %+v, want rate_requests, retryable, Retry-After exactly 30s", d)
	}
	if d.Message != "Rate limit reached for requests (chat): 2/min." {
		t.Errorf("message %q", d.Message)
	}
	c.add(30*time.Second - time.Nanosecond)
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); d.Allowed || d.RetryAfter != time.Nanosecond {
		t.Errorf("1 ns early: %+v, want refused with Retry-After 1ns", d)
	}
	c.add(time.Nanosecond)
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); !d.Allowed {
		t.Errorf("after Retry-After: %+v, want admitted", d)
	}
}

// TestGCRAUnevenLimit: a limit that does not divide the window still allows
// its full burst (the emission interval rounds down) and the Retry-After is
// the exact excess.
func TestGCRAUnevenLimit(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 7, TPM: 1000}
	for i := range 7 {
		if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); !d.Allowed {
			t.Fatalf("request %d of the burst refused: %+v", i+1, d)
		}
	}
	_, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
	tick := time.Minute / 7
	if d.Allowed || d.RetryAfter != 8*tick-time.Minute {
		t.Errorf("request 8: %+v, want Retry-After %v", d, 8*tick-time.Minute)
	}
}

// TestLaneReserve is B-06-27 (L0): rpm 4, r 0.25 ⇒ three bulk requests pass,
// the fourth bulk one is refused, an urgent one of the same consumer still
// passes; the bulk lane's headers and message show its own share.
func TestLaneReserve(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 4, TPM: 1 << 20, XUReserve: 0.25}
	for i, want := range []int64{2, 1, 0} {
		_, d := m.Admit(chat(keyquota.LaneBulk), l, small)
		if !d.Allowed {
			t.Fatalf("bulk %d refused: %+v", i+1, d)
		}
		if d.Headers.LimitRequests != 3 || d.Headers.RemainingRequests != want {
			t.Errorf("bulk %d: headers %+v, want limit 3 remaining %d", i+1, d.Headers, want)
		}
	}
	_, d := m.Admit(chat(keyquota.LaneBulk), l, small)
	if d.Allowed || d.Reason != keyquota.ReasonRequests || d.RetryAfter != 15*time.Second {
		t.Fatalf("bulk 4: %+v, want refused with Retry-After 15s", d)
	}
	if !strings.Contains(d.Message, "bulk lane") || !strings.Contains(d.Message, "3/min") {
		t.Errorf("bulk 4 message %q, want the bulk lane's own limit 3/min", d.Message)
	}
	_, d = m.Admit(chat(keyquota.LaneUrgent), l, small)
	if !d.Allowed || d.Headers.LimitRequests != 4 || d.Headers.RemainingRequests != 0 {
		t.Fatalf("urgent after three bulk: %+v, want admitted, limit 4, remaining 0", d)
	}
}

// TestLaneReserveFloor: a limit too small to split leaves bulk at least one
// unit (no reserve, the named bound) — never a bulk request refused for good.
func TestLaneReserveFloor(t *testing.T) {
	c := newClock()
	m := seeded(c)
	for _, r := range []float64{0.25, 0.99, 1, 2} {
		l := keyquota.Limits{RPM: 1, TPM: 1000, XUReserve: r}
		s := keyquota.Subject{ConsumerID: "floor", TenantID: "t", Op: keyquota.OpChat, Lane: keyquota.LaneBulk}
		_, d := m.Admit(s, l, small)
		if !d.Allowed {
			t.Errorf("r=%v: a fresh bulk request at rpm 1 refused: %+v", r, d)
		}
		c.add(time.Minute)
	}
}

// TestCapAndDebt is B-06-28: the reservation is capped at the limit, so a
// request estimated above TPM is admitted on a full bucket; 45 000 real
// tokens settle, the follow-up waits the debt off; an overrun beyond the
// estimate is debt as well.
func TestCapAndDebt(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 100, TPM: 60000}
	big := keyquota.Estimate{PromptRunes: runesFor(61000), MaxCompletion: 1000, ClientMax: true}
	if got := big.Tokens(keyquota.OpChat, l); got <= 60000 {
		t.Fatalf("estimate %d, want > 60000", got)
	}
	res, d := m.Admit(chat(keyquota.LaneUrgent), l, big)
	if !d.Allowed {
		t.Fatalf("estimate above TPM on a full bucket: %+v, want admitted (capped)", d)
	}
	if d.Headers.RemainingTokens != 0 {
		t.Errorf("remaining tokens %d after a capped admission, want 0", d.Headers.RemainingTokens)
	}
	res.Settle(keyquota.Usage{PromptTokens: 44000, CompletionTokens: 1000})
	_, d = m.Admit(chat(keyquota.LaneUrgent), l, big)
	if d.Allowed || d.Reason != keyquota.ReasonTokens || d.RetryAfter != 45*time.Second {
		t.Fatalf("follow-up: %+v, want refused rate_tokens with Retry-After 45s (the 45000 settled tokens)", d)
	}
	if d.Message != "Rate limit reached for tokens (chat): 60000/min." {
		t.Errorf("message %q", d.Message)
	}
	c.add(45 * time.Second)
	res, d = m.Admit(chat(keyquota.LaneUrgent), l, small)
	if !d.Allowed {
		t.Fatalf("after the debt: %+v", d)
	}
	// Overrun: a few tokens reserved, 90 000 used ⇒ the bucket is 30 s in
	// debt beyond its window (1 ms per token at 60 000 tpm).
	res.Settle(keyquota.Usage{PromptTokens: 89000, CompletionTokens: 1000})
	_, d = m.Admit(chat(keyquota.LaneUrgent), l, small)
	want := 30*time.Second + time.Duration(small.Tokens(keyquota.OpChat, l))*time.Millisecond
	if d.Allowed || d.RetryAfter != want {
		t.Errorf("after an overrun of 90000: %+v, want refused with Retry-After %v", d, want)
	}
}

// TestBulkCapIsTheLaneCapacity (Fremd-Review 1 F-1): the cap of a bulk
// request is what the bulk lane can hold — at TPM 60 000 and r 0.25 a bulk
// estimate of 50 000 is admitted on a full bucket (charged at 45 000) and
// settles its real tokens. Capped at L instead, it could never pass: its
// cost (50 s) exceeds the lane's tolerance (45 s) on an empty bucket too,
// and every answer would be a retryable 429.
func TestBulkCapIsTheLaneCapacity(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 100, TPM: 60000, XUReserve: 0.25}
	e := keyquota.Estimate{PromptRunes: runesFor(49000), MaxCompletion: 1000, ClientMax: true}
	if got := e.Tokens(keyquota.OpChat, l); got != 50000 {
		t.Fatalf("estimate %d, want 50000", got)
	}
	res, d := m.Admit(chat(keyquota.LaneBulk), l, e)
	if !d.Allowed {
		t.Fatalf("bulk estimate 50000 on a full bucket: %+v, want admitted (charged at the lane's 45000)", d)
	}
	if d.Headers.LimitTokens != 45000 || d.Headers.RemainingTokens != 0 {
		t.Errorf("bulk headers %+v, want limit 45000, remaining 0", d.Headers)
	}
	res.Settle(keyquota.Usage{PromptTokens: 20000})
	// 20 000 real tokens: the urgent lane has 40 000 left, the bulk lane 25 000.
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{ClientMax: true}); d.Headers.RemainingTokens != 40000 {
		t.Errorf("urgent after the bulk settle: remaining %d, want 40000", d.Headers.RemainingTokens)
	}
}

// TestUsageSumSaturates: a usage that overflows int64 when summed books the
// largest debt, not a negative refund (Fremd-Review 1 F-5).
func TestUsageSumSaturates(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 100, TPM: 60000}
	res, _ := m.Admit(chat(keyquota.LaneUrgent), l, small)
	res.Settle(keyquota.Usage{PromptTokens: math.MaxInt, CompletionTokens: math.MaxInt})
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); d.Allowed || d.RetryAfter <= 0 {
		t.Errorf("after an absurd usage: %+v, want refused with a positive Retry-After", d)
	}
}

// TestOpBuckets is Finding 16 on the meter: an exhausted embed bucket leaves
// chat of the same consumer untouched.
func TestOpBuckets(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 100, TPM: 1000}
	emb := keyquota.Subject{ConsumerID: "c1", TenantID: "t1", Op: keyquota.OpEmbed}
	res, d := m.Admit(emb, l, keyquota.Estimate{InputRunes: runesFor(1000)})
	if !d.Allowed {
		t.Fatalf("embed: %+v", d)
	}
	res.Settle(keyquota.Usage{PromptTokens: 1000})
	if _, d := m.Admit(emb, l, keyquota.Estimate{InputRunes: 9}); d.Allowed {
		t.Fatalf("embed after its bucket is full: %+v, want refused", d)
	}
	_, d = m.Admit(chat(keyquota.LaneUrgent), l, small)
	if !d.Allowed || d.Headers.RemainingTokens != 1000-small.Tokens(keyquota.OpChat, l) {
		t.Errorf("chat of the same consumer: %+v, want admitted from its own bucket", d)
	}
}

// TestEmbedBatchFitsTheDefaults: a batch of 2048 inputs × 128 tokens under
// the registry defaults of embed (120 rpm, 500 000 tpm) is admitted.
func TestEmbedBatchFitsTheDefaults(t *testing.T) {
	m := seeded(newClock())
	l := keyquota.Limits{RPM: 120, TPM: 500000}
	e := keyquota.Estimate{InputRunes: runesFor(2048 * 128)}
	if got := e.Tokens(keyquota.OpEmbed, l); got != 2048*128 {
		t.Fatalf("batch estimate %d, want %d", got, 2048*128)
	}
	if _, d := m.Admit(keyquota.Subject{ConsumerID: "c", TenantID: "t", Op: keyquota.OpEmbed}, l, e); !d.Allowed {
		t.Errorf("2048 × 128 tokens under the defaults: %+v, want admitted", d)
	}
}

// TestSameEstimateBooksTheSame is the cache pin B-06-24 on the meter: the
// same embed input twice books the same tokens twice (no cheaper repeat).
func TestSameEstimateBooksTheSame(t *testing.T) {
	m := seeded(newClock())
	l := keyquota.Limits{RPM: 100, TPM: 100000}
	s := keyquota.Subject{ConsumerID: "c", TenantID: "t", Op: keyquota.OpEmbed}
	e := keyquota.Estimate{InputRunes: 900}
	_, d1 := m.Admit(s, l, e)
	_, d2 := m.Admit(s, l, e)
	first := l.TPM - d1.Headers.RemainingTokens
	second := d1.Headers.RemainingTokens - d2.Headers.RemainingTokens
	if !d1.Allowed || !d2.Allowed || first != second || first != e.Tokens(keyquota.OpEmbed, l) {
		t.Errorf("bookings %d and %d, want both %d", first, second, e.Tokens(keyquota.OpEmbed, l))
	}
}

// TestChatReservation is the chat formula of design/06 §4.2: prompt
// estimate + the client's clamped maximum, or min(reserve, clamp) without one.
func TestChatReservation(t *testing.T) {
	l := keyquota.Limits{CompletionReserve: 1024}
	p := promptguard.TokenEstimate(900)
	for _, tc := range []struct {
		e    keyquota.Estimate
		want int64
	}{
		{keyquota.Estimate{PromptRunes: 900, MaxCompletion: 300, ClientMax: true}, int64(p + 300)},
		{keyquota.Estimate{PromptRunes: 900, MaxCompletion: 4096}, int64(p + 1024)},
		{keyquota.Estimate{PromptRunes: 900, MaxCompletion: 512}, int64(p + 512)},
		{keyquota.Estimate{PromptRunes: 900}, int64(p + 1024)},
	} {
		if got := tc.e.Tokens(keyquota.OpChat, l); got != tc.want {
			t.Errorf("%+v: %d, want %d", tc.e, got, tc.want)
		}
	}
	if got := (keyquota.Estimate{InputRunes: 900}).Tokens(keyquota.OpRerank, l); got != int64(p) {
		t.Errorf("rerank: %d, want %d", got, p)
	}
}

// TestWarming: an unseeded meter refuses every request with warming, 5 s,
// retryable — and books nothing.
func TestWarming(t *testing.T) {
	c := newClock()
	m := keyquota.New(c.now)
	l := keyquota.Limits{RPM: 1, TPM: 1000}
	res, d := m.Admit(chat(keyquota.LaneUrgent), l, small)
	if d.Allowed || res != nil || d.Reason != keyquota.ReasonWarming || d.RetryAfter != 5*time.Second || !d.Retryable {
		t.Fatalf("unseeded: %+v, want warming, Retry-After 5s, retryable", d)
	}
	m.MarkSeeded()
	if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); !d.Allowed || d.Headers.RemainingRequests != 0 {
		t.Errorf("seeded: %+v, want admitted with the full bucket (warming booked nothing)", d)
	}
}

// TestInvalidSubjectFailsClosed: no consumer, no tenant, an unknown op or a
// limit below 1 is a wiring defect — refused, not retryable, nothing booked.
func TestInvalidSubjectFailsClosed(t *testing.T) {
	m := seeded(newClock())
	l := keyquota.Limits{RPM: 10, TPM: 1000}
	for name, tc := range map[string]struct {
		s keyquota.Subject
		l keyquota.Limits
	}{
		"no consumer": {keyquota.Subject{TenantID: "t", Op: keyquota.OpChat}, l},
		"no tenant":   {keyquota.Subject{ConsumerID: "c", Op: keyquota.OpChat}, l},
		"unknown op":  {keyquota.Subject{ConsumerID: "c", TenantID: "t", Op: "images"}, l},
		"bad lane":    {keyquota.Subject{ConsumerID: "c", TenantID: "t", Op: keyquota.OpChat, Lane: 7}, l},
		"rpm 0":       {chat(keyquota.LaneUrgent), keyquota.Limits{TPM: 1000}},
		"tpm 0":       {chat(keyquota.LaneUrgent), keyquota.Limits{RPM: 10}},
	} {
		res, d := m.Admit(tc.s, tc.l, small)
		if d.Allowed || res != nil || d.Reason != keyquota.ReasonInvalid || d.Retryable {
			t.Errorf("%s: %+v, want refused invalid, not retryable", name, d)
		}
	}
}

// TestReservationEndsOnce: Settle, Abort and Reject share one end; Abort
// and Reject (with or without a reason) give the tokens back and keep the
// request counted.
func TestReservationEndsOnce(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 10, TPM: 10000}
	e := keyquota.Estimate{PromptRunes: 900, MaxCompletion: 500, ClientMax: true}
	n := e.Tokens(keyquota.OpChat, l)
	for name, end := range map[string]func(*keyquota.Reservation){
		"abort":        func(r *keyquota.Reservation) { r.Abort() },
		"reject":       func(r *keyquota.Reservation) { r.Reject("server_busy", time.Second) },
		"reject empty": func(r *keyquota.Reservation) { r.Reject("", 0) },
	} {
		s := keyquota.Subject{ConsumerID: name, TenantID: "t", Op: keyquota.OpChat}
		r, d := m.Admit(s, l, e)
		if d.Headers.RemainingTokens != l.TPM-n {
			t.Fatalf("%s: remaining %d, want %d", name, d.Headers.RemainingTokens, l.TPM-n)
		}
		end(r)
		r.Settle(keyquota.Usage{PromptTokens: 5000}) // after the end: no effect
		_, d = m.Admit(s, l, keyquota.Estimate{})
		if d.Headers.RemainingTokens != l.TPM || d.Headers.RemainingRequests != l.RPM-2 {
			t.Errorf("%s: %+v, want the tokens back (%d) and both requests counted", name, d.Headers, l.TPM)
		}
	}
	s := keyquota.Subject{ConsumerID: "settle", TenantID: "t", Op: keyquota.OpChat}
	r, _ := m.Admit(s, l, e)
	r.Settle(keyquota.Usage{PromptTokens: 200, CompletionTokens: 100})
	r.Settle(keyquota.Usage{PromptTokens: 5000})
	r.Abort()
	if _, d := m.Admit(s, l, keyquota.Estimate{}); d.Headers.RemainingTokens != l.TPM-300 {
		t.Errorf("settle: remaining %d, want %d (300 real tokens, once)", d.Headers.RemainingTokens, l.TPM-300)
	}
}

// TestMixedEndsEqualTheSettles is gate Z2 on the meter: 1 000 reservations
// ended at random by Settle, Abort and Reject (some twice) leave the token
// bucket of the subject at exactly the sum of the settled tokens. The limit
// divides the window (1 µs per token), so tokens and bucket time convert
// without rounding; for any limit the bucket time is Σ ⌊actual·W/L⌋.
func TestMixedEndsEqualTheSettles(t *testing.T) {
	c := newClock()
	m := seeded(c)
	l := keyquota.Limits{RPM: 1 << 20, TPM: 60_000_000}
	s := chat(keyquota.LaneUrgent)
	rng := rand.New(rand.NewPCG(10, 6))
	var settled int64
	for range 1000 {
		r, d := m.Admit(s, l, keyquota.Estimate{PromptRunes: rng.IntN(5000), MaxCompletion: rng.IntN(2000), ClientMax: true})
		if !d.Allowed {
			t.Fatalf("refused: %+v", d)
		}
		switch rng.IntN(4) {
		case 0:
			u := keyquota.Usage{PromptTokens: rng.IntN(3000), CompletionTokens: rng.IntN(3000)}
			settled += int64(u.PromptTokens + u.CompletionTokens)
			r.Settle(u)
			r.Abort()
		case 1:
			r.Abort()
			r.Settle(keyquota.Usage{PromptTokens: 7})
		case 2:
			r.Reject("server_busy", time.Second)
		default:
			r.Reject("", 0)
			r.Abort()
		}
	}
	_, d := m.Admit(s, l, keyquota.Estimate{})
	if got := l.TPM - d.Headers.RemainingTokens; got != settled || d.Headers.ResetTokens != time.Duration(settled)*time.Microsecond {
		t.Errorf("token bucket holds %d (%v), want the settled sum %d", got, d.Headers.ResetTokens, settled)
	}
}

// TestConcurrentAdmitsNeverExceedTheLimit: 64 goroutines on one subject at
// one instant ⇒ exactly L admissions (≤ L per window), under -race.
func TestConcurrentAdmitsNeverExceedTheLimit(t *testing.T) {
	c := newClock()
	m := seeded(c)
	const limit = 50
	l := keyquota.Limits{RPM: limit, TPM: 1 << 30}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 20 {
				if r, d := m.Admit(chat(keyquota.LaneUrgent), l, small); d.Allowed {
					admitted.Add(1)
					r.Settle(keyquota.Usage{PromptTokens: 1})
				}
			}
		})
	}
	wg.Wait()
	if n := admitted.Load(); n != limit {
		t.Errorf("admitted %d of 1280 at one instant, want exactly %d", n, limit)
	}
	// One window later the bucket holds the limit again, no more.
	c.add(time.Minute)
	admitted.Store(0)
	for range 64 {
		wg.Go(func() {
			if _, d := m.Admit(chat(keyquota.LaneUrgent), l, small); d.Allowed {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if n := admitted.Load(); n != limit {
		t.Errorf("next window: admitted %d of 64, want %d", n, limit)
	}
}

// TestHeaderForm: the six x-ratelimit-* headers, reset as a Go duration.
func TestHeaderForm(t *testing.T) {
	m := seeded(newClock())
	l := keyquota.Limits{RPM: 2, TPM: 60000}
	r, d := m.Admit(chat(keyquota.LaneUrgent), l, keyquota.Estimate{PromptRunes: 9, MaxCompletion: 995, ClientMax: true})
	h := r.Headers()
	want := map[string]string{
		"x-ratelimit-limit-requests": "2", "x-ratelimit-remaining-requests": "1", "x-ratelimit-reset-requests": "30s",
		"x-ratelimit-limit-tokens": "60000", "x-ratelimit-remaining-tokens": "59000", "x-ratelimit-reset-tokens": "1s",
	}
	if len(h) != len(want) {
		t.Errorf("%d headers %v, want %d", len(h), h, len(want))
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q (decision %+v)", k, got, v, d.Headers)
		}
	}
}
