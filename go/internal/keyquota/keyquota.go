// Package keyquota is the per-consumer meter of the model API, levels L0
// and L1 (design/06 §4.2, waves W06-10 and W06-11): a GCRA bucket for
// requests per minute and one for tokens per minute per (consumer, op),
// lane-aware with the XU reserve, with the reservation capped at the limit
// and the overrun booked as debt when the request settles; and the
// concurrency limit C per (consumer, op), counted from Admit to the
// reservation's end — waiting in L2 and running alike — with the same XU
// reserve. The meter refuses at once and never waits (design/06 §4.1 steps 8
// and 9: "L0/L1 lehnen ab, NUR L2 wartet"); only the dispatcher's L2 waits.
//
// A leaf by design (design/06 §4.2, masterplan K-Meter): it imports neither
// config nor handler nor a database — the limits arrive per call (Limits,
// from the hot gateway.* keys), the subject from the model-key door
// (Subject), and cmd/ctxd adapts the meter to the oai.Limiter hook of the
// /v1 routes. The one ctx import is promptguard.TokenEstimate, the house
// rule that turns runes into a token estimate (depguard keyquota-leaf).
//
// State lives per (consumer, op) for the life of the process and is never
// removed when a key is revoked, rotated or its consumer disabled (B-06-19):
// a consumer cannot reset its own window by cycling keys. Single process: one
// meter per instance, like the dispatcher (design/06 §4.2
// "Single-Process-Annahme").
package keyquota

import (
	"fmt"
	"math"
	"math/bits"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/promptguard"
)

// Op is the operation class of a request; its values are the op vocabulary
// of the /v1 rows (llmlog.GatewayOp*, pinned by TestOpMatchesTheRowVocabulary).
type Op string

// The ops of the model API: each has its own pair of buckets (Finding 16 —
// an exhausted embed bucket leaves chat of the same consumer untouched).
const (
	OpChat   Op = "chat"
	OpEmbed  Op = "embed"
	OpRerank Op = "rerank"
)

// ParseOp maps s onto an op; false for anything else.
func ParseOp(s string) (Op, bool) {
	switch op := Op(s); op {
	case OpChat, OpEmbed, OpRerank:
		return op, true
	}
	return "", false
}

// Lane is the admission lane of a request (design/03 §4.4, resolved by
// handler.ResolveLane): urgent (XU) or bulk (XB).
type Lane uint8

// The lanes. The zero value is urgent; a bulk request fills at most the
// share 1 − r of each bucket (B-06-27).
const (
	LaneUrgent Lane = iota
	LaneBulk
)

// Subject is who a request is charged to.
type Subject struct {
	// ConsumerID and TenantID come from the model consumer of the door
	// (auth.ConsumerAuth); either one empty ⇒ Admit refuses (fail-closed).
	ConsumerID, TenantID string
	Op                   Op
	Lane                 Lane
}

// Limits are the effective limits of one request (until W06-15 the
// registry defaults for every consumer).
type Limits struct {
	// RPM and TPM are the requests and tokens per minute of the subject's
	// op (gateway.<op>_rpm_default, gateway.<op>_tpm_default); each must be
	// at least 1 (V47), a smaller value is refused as a wiring defect.
	RPM, TPM int64
	// CompletionReserve is the completion share of a chat reservation when
	// the client named no maximum (gateway.tpm_reserve_default).
	CompletionReserve int64
	// XUReserve is r (gateway.xu_reserve_ratio, [0, 1)): bulk requests are
	// admitted only up to the share 1 − r of each bucket and of the
	// concurrency slots.
	XUReserve float64
	// Concurrency is C, L1 (gateway.consumer_concurrency_default): the
	// requests of the subject's op that may be open at once — admitted and
	// not ended, waiting in L2 or running. At least 1 (V48): L1 is never
	// unbounded (design/04 R-06-4), a smaller value is refused as a wiring
	// defect.
	Concurrency int64
}

// Estimate is what a request is expected to cost (design/06 §4.2
// "Reservierung je op").
type Estimate struct {
	// PromptRunes are the runes of a chat prompt as it goes upstream.
	PromptRunes int
	// MaxCompletion is the effective completion maximum of a chat request
	// after the route's clamp (design/02 §4.5); ClientMax reports whether
	// the client named one (max_tokens / max_completion_tokens). Without
	// one, the completion share is min(CompletionReserve, MaxCompletion).
	MaxCompletion int
	ClientMax     bool
	// InputRunes are Σ input runes of an embedding request, Σ (query +
	// document) runes of a rerank request.
	InputRunes int
}

// Tokens is the reservation of e for op under l: chat = TokenEstimate(prompt
// runes) + completion share, embed and rerank = TokenEstimate(input runes).
// The same estimate always books the same tokens (B-06-24: no cache hit is
// ever cheaper).
func (e Estimate) Tokens(op Op, l Limits) int64 {
	switch op {
	case OpChat:
		c := int64(max(e.MaxCompletion, 0))
		if !e.ClientMax && (c == 0 || l.CompletionReserve < c) {
			c = max(l.CompletionReserve, 0)
		}
		return int64(promptguard.TokenEstimate(e.PromptRunes)) + c
	case OpEmbed, OpRerank:
		return int64(promptguard.TokenEstimate(e.InputRunes))
	}
	return 0
}

// Usage is what a request really cost (the backend's usage object).
type Usage struct {
	PromptTokens, CompletionTokens int
}

// Reason is why Admit refused (the ledger reasons of design/06 §4.1).
type Reason string

// The refusal reasons of L0 and L1. ReasonInvalid is a wiring defect (no
// consumer, no tenant, an unknown op, a limit below 1) — never a client's
// fault.
const (
	ReasonRequests    Reason = "rate_requests"
	ReasonTokens      Reason = "rate_tokens"
	ReasonConcurrency Reason = "concurrency"
	ReasonWarming     Reason = "warming"
	ReasonInvalid     Reason = "invalid"
)

// WarmingRetryAfter is the Retry-After of an unseeded meter: the retry
// cadence of the boot seed (design/06 §4.2 "Retry mit Backoff 5 s", §4.4
// row warming; lead decision O-10-1 (a): a named constant, not a setting).
const WarmingRetryAfter = 5 * time.Second

// ConcurrencyRetryAfter is the Retry-After of an L1 refusal: the honest lower
// bound — the meter cannot know when one of the open requests ends
// (design/06 §4.2 row "L1 Concurrency", §4.4 row concurrency: 1 / 1000).
const ConcurrencyRetryAfter = time.Second

// window is W of design/06 §4.2: the limits are per minute by definition,
// the emission interval of a limit L is W/L and the burst tolerance τ = W.
const window = int64(time.Minute)

// Decision is the meter's answer for one request.
type Decision struct {
	Allowed bool
	Reason  Reason
	// RetryAfter is GCRA-exact: the earliest time the same request could
	// pass if nothing else arrives (WarmingRetryAfter while unseeded).
	RetryAfter time.Duration
	// Retryable is false only for ReasonInvalid.
	Retryable bool
	// Message names only the caller's own limit (B6 doctrine): the op, the
	// dimension and the limit as the request's lane sees it.
	Message string
	// Headers is the state of the op's buckets right after an admission,
	// as the request's lane sees it (zero for a refusal).
	Headers RateHeaders
}

// RateHeaders are the x-ratelimit-* values of one admission (design/06
// §4.4, extern-recherche §1.6): the bucket of the requested op as the lane
// sees it — the limit is the lane's capacity, the remaining units fit now,
// the reset is the time until the bucket is full again.
type RateHeaders struct {
	LimitRequests, RemainingRequests int64
	ResetRequests                    time.Duration
	LimitTokens, RemainingTokens     int64
	ResetTokens                      time.Duration
}

// Header renders h as the six x-ratelimit-* headers; reset as a Go duration
// string rounded up to the millisecond ("1m0s", "1.5s", "17ms").
func (h RateHeaders) Header() http.Header {
	out := make(http.Header, 6)
	out.Set("x-ratelimit-limit-requests", strconv.FormatInt(h.LimitRequests, 10))
	out.Set("x-ratelimit-remaining-requests", strconv.FormatInt(h.RemainingRequests, 10))
	out.Set("x-ratelimit-reset-requests", ceilMs(h.ResetRequests).String())
	out.Set("x-ratelimit-limit-tokens", strconv.FormatInt(h.LimitTokens, 10))
	out.Set("x-ratelimit-remaining-tokens", strconv.FormatInt(h.RemainingTokens, 10))
	out.Set("x-ratelimit-reset-tokens", ceilMs(h.ResetTokens).String())
	return out
}

func ceilMs(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return (d + time.Millisecond - 1) / time.Millisecond * time.Millisecond
}

// Meter is the L0 meter of one process. Its clock is the time since the
// meter was built (now() − base): time.Now carries Go's monotonic reading, so
// a step of the wall clock (NTP, an operator) neither locks consumers out nor
// refills their buckets.
type Meter struct {
	now    func() time.Time
	base   time.Time
	seeded atomic.Bool

	mu    sync.Mutex
	state map[stateKey]*buckets
}

type stateKey struct {
	consumer string
	op       Op
}

// buckets are the theoretical arrival times (GCRA tat, ns since the meter's
// base) of one (consumer, op): requests and tokens (0 = never used, an
// empty bucket), and the reservations of the key that have not ended yet,
// each with its shadow (the refund rule, Reservation.Settle). The open set
// is also L1's count: len(open) requests in flight, bulk of them on the bulk
// lane (kept by add and drop, the only two places the set changes).
type buckets struct {
	req, tok int64
	open     []*Reservation
	bulk     int64
	seq      uint64 // admission order of the key's reservations
}

// raise books a charge of c ns at now into every open reservation's shadow:
// each shadow is a bucket that started empty at its reservation's admission
// and sees every later charge (admissions and debts).
func (st *buckets) raise(now, c int64) {
	for _, r := range st.open {
		r.shadow = satAdd(max(r.shadow, now), c)
	}
}

// add enters r into the open set.
func (st *buckets) add(r *Reservation) {
	st.seq++
	r.seq = st.seq
	r.slot = len(st.open)
	st.open = append(st.open, r)
	if r.lane == LaneBulk {
		st.bulk++
	}
}

// drop takes r out of the open set (swap with the last).
func (st *buckets) drop(r *Reservation) {
	if r.slot < 0 || r.slot >= len(st.open) || st.open[r.slot] != r {
		return
	}
	last := len(st.open) - 1
	st.open[r.slot] = st.open[last]
	st.open[r.slot].slot = r.slot
	st.open[last] = nil
	st.open = st.open[:last]
	r.slot = -1
	if r.lane == LaneBulk {
		st.bulk--
	}
}

// l1 is L1 of design/06 §4.2 for a request on lane: the key's open
// reservations stay below C, and those of the bulk lane below its share
// bulkSlots(C, r), so the urgent lane of the same consumer keeps the rest of
// the slots and reaches L2. It returns the limit that refuses (false) — the
// lane's share when the bulk count is the bound.
func (st *buckets) l1(lane Lane, l Limits) (limit int64, bulkBound, ok bool) {
	if int64(len(st.open)) >= l.Concurrency {
		return l.Concurrency, false, false
	}
	if lane == LaneBulk {
		if share := bulkSlots(l.Concurrency, l.XUReserve); st.bulk >= share {
			return share, true, false
		}
	}
	return 0, false, true
}

// bulkSlots is the bulk lane's share of C: ⌊C·(1 − r)⌋, at least one slot —
// a bulk request is never refused for good; at C = 1 (or r = 0, or no r at
// all, like newDim) there is no urgent reserve, the named bound of design/06
// §4.2 "Bei C = 1 ist kein Reservat möglich". r is taken to the millionth and
// the floor counted in integers: in float64, 1 − r of a decimal r can lie
// just under its exact value and the truncation then lands one slot short
// (Fremd-Review 1 F-1: C 10 at r 0.8 gave 1, not 2).
func bulkSlots(c int64, r float64) int64 {
	if !(r > 0) {
		return c
	}
	keep := reserveScale - int64(math.Round(min(r, 1)*reserveScale))
	return max(mulDiv(c, keep, reserveScale), 1)
}

// reserveScale is the resolution of r in the bulk share: the millionth.
const reserveScale = 1_000_000

// New returns an unseeded meter over the clock now: until MarkSeeded every
// Admit answers warming (design/06 §4.1 step 7).
func New(now func() time.Time) *Meter {
	return &Meter{now: now, base: now(), state: map[stateKey]*buckets{}}
}

// clock is the meter's time: ns since base on the monotonic clock when now
// carries one.
func (m *Meter) clock() int64 { return int64(m.now().Sub(m.base)) }

// MarkSeeded opens the meter. RPM and TPM windows need no history, so until
// W06-13 seeds the day and month budgets from the rollup, ctxd marks the
// meter seeded at start.
func (m *Meter) MarkSeeded() { m.seeded.Store(true) }

// Seeded reports whether the meter admits at all.
func (m *Meter) Seeded() bool { return m.seeded.Load() }

// Admit is design/06 §4.1 steps 7 to 9 for one request: seeded?, then the
// request and token buckets of (consumer, op), then L1 — all pass or nothing
// is booked. O(open reservations of the key) under one mutex (the admission
// raises their shadows; L1 bounds the open set at C); a refusal changes no
// state.
func (m *Meter) Admit(s Subject, l Limits, e Estimate) (*Reservation, Decision) {
	if s.ConsumerID == "" || s.TenantID == "" || l.RPM < 1 || l.TPM < 1 || l.Concurrency < 1 || s.Lane > LaneBulk {
		return nil, invalid()
	}
	if _, ok := ParseOp(string(s.Op)); !ok {
		return nil, invalid()
	}
	if !m.Seeded() {
		return nil, Decision{Reason: ReasonWarming, RetryAfter: WarmingRetryAfter, Retryable: true,
			Message: "The rate limiter is starting up; retry shortly."}
	}
	req := newDim(l.RPM, s.Lane, l.XUReserve)
	tok := newDim(l.TPM, s.Lane, l.XUReserve)
	// The cap (B-06-28): a reservation above what the lane's bucket can
	// hold is charged at that capacity, so an overestimated request is
	// admitted on a full bucket and settles the rest as debt — never refused
	// for good.
	n := min(max(e.Tokens(s.Op, l), 0), tok.cap)
	key := stateKey{s.ConsumerID, s.Op}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	st := m.state[key]
	if st == nil {
		st = &buckets{}
	}
	nextReq, waitReq, okReq := req.admit(st.req, now, 1)
	nextTok, waitTok, okTok := tok.admit(st.tok, now, n)
	if !okReq || !okTok {
		return nil, refused(s, req, tok, waitReq, waitTok, okReq)
	}
	if limit, bulkBound, ok := st.l1(s.Lane, l); !ok {
		return nil, concurrencyRefused(s, limit, bulkBound)
	}
	m.state[key] = st
	st.raise(now, nextTok-max(st.tok, now))
	st.req, st.tok = nextReq, nextTok
	h := RateHeaders{
		LimitRequests: req.cap, RemainingRequests: req.remaining(st.req, now), ResetRequests: time.Duration(backlog(st.req, now)),
		LimitTokens: tok.cap, RemainingTokens: tok.remaining(st.tok, now), ResetTokens: time.Duration(backlog(st.tok, now)),
	}
	r := &Reservation{m: m, key: key, lane: s.Lane, tpm: l.TPM, charged: tok.cost(n), held: n, shadow: now, headers: h}
	st.add(r)
	return r, Decision{Allowed: true, Retryable: true, Headers: h}
}

// concurrencyRefused is the decision of an L1 refusal: Retry-After is the
// honest lower bound, the message names the limit that refused — C, or the
// bulk lane's share of it.
func concurrencyRefused(s Subject, limit int64, bulkBound bool) Decision {
	lane := ""
	if bulkBound {
		lane = ", bulk lane"
	}
	return Decision{Reason: ReasonConcurrency, RetryAfter: ConcurrencyRetryAfter, Retryable: true,
		Message: fmt.Sprintf("Concurrency limit reached for requests (%s%s): %d at a time.", s.Op, lane, limit)}
}

func invalid() Decision {
	return Decision{Reason: ReasonInvalid, Message: "The server could not complete the request."}
}

// refused is the decision of a refusal: the dimension that needs the longer
// wait names the reason, the Retry-After is the longer wait (after it both
// buckets would pass if nothing else arrives).
func refused(s Subject, req, tok dim, waitReq, waitTok int64, okReq bool) Decision {
	reason, d, what := ReasonTokens, tok, "tokens"
	if !okReq && waitReq >= waitTok {
		reason, d, what = ReasonRequests, req, "requests"
	}
	lane := ""
	if s.Lane == LaneBulk {
		lane = ", bulk lane"
	}
	return Decision{Reason: reason, RetryAfter: time.Duration(max(waitReq, waitTok)), Retryable: true,
		Message: fmt.Sprintf("Rate limit reached for %s (%s%s): %d/min.", what, s.Op, lane, d.cap)}
}

// Reservation is one admitted request's share of the meter, its L1 slot
// included. It ends exactly once — Settle, Keep, Abort and Reject share one
// sync.Once, and each frees the slot — and every later end is a no-op.
type Reservation struct {
	m       *Meter
	key     stateKey
	lane    Lane  // the lane of the admission: which L1 count the slot is in
	tpm     int64 // the TPM the admission was charged under
	charged int64 // ns booked on the token bucket at admission
	held    int64 // the tokens charged at admission (Held)
	// shadow is the tat (ns) of a bucket that started empty at this
	// admission and saw only the charges after it, corrected by the refunds
	// of younger reservations — kept by the meter under its mutex while the
	// reservation is open; seq is its admission order, slot its place in the
	// key's open set (-1 once ended).
	shadow  int64
	seq     uint64
	slot    int
	headers RateHeaders
	once    sync.Once
}

// Settle books the request's real tokens: the token bucket moves by
// cost(actual) − cost(charged). An overrun is debt from now on, the next
// request waits it off (design/06 §4.2 TPM-Settle). A refund leaves the
// bucket exactly where it would stand had the real usage been booked at
// the admission (the oracle; W06-10 hardening 2, H2-1). The bucket is a
// max-plus sum: tat = max(A, B), where A are the terms that contain this
// reservation's charge and B = its shadow, the bucket of the charges after
// it alone; taking d = charged − cost(actual) out of the charge gives
// max(A − d, B), a refund of min(d, tat − shadow). A held reservation that
// ends with little usage therefore cannot erase the charge of requests
// admitted after it (Fremd-Review 2 F-1), and one that ends early loses no
// credit (Fremd-Review 3 F-2). The request stays counted.
func (r *Reservation) Settle(u Usage) {
	r.once.Do(func() {
		actual := satAdd(int64(max(u.PromptTokens, 0)), int64(max(u.CompletionTokens, 0)))
		r.m.end(r, dim{limit: r.tpm}.cost(actual)-r.charged)
	})
}

// Keep ends a reservation keeping its whole charge: a wire call was made but
// its usage is unknown (an upstream error, an answer without usage, a panic
// or a gone client after the wire) — after wire contact nothing is given
// back.
func (r *Reservation) Keep() {
	r.once.Do(func() { r.m.end(r, 0) })
}

// Abort ends a reservation without a wire call: its charge goes back as far
// as the bucket still holds it (all of it unless the bucket drained below it
// meanwhile), the request stays counted (design/06 §4.2).
func (r *Reservation) Abort() {
	r.once.Do(func() { r.m.end(r, -r.charged) })
}

// Reject ends a reservation the dispatcher refused after Admit. In this
// wave it is Abort: the reason and the wait are the ledger's (W06-12), so an
// empty reason (the client went away during the refusal) changes nothing.
func (r *Reservation) Reject(reason string, waited time.Duration) {
	_, _ = reason, waited
	r.Abort()
}

// Headers are the x-ratelimit-* headers of the admission (design/06 §4.4):
// the route writes them on its successful answer before the first byte.
func (r *Reservation) Headers() http.Header { return r.headers.Header() }

// Held is the tokens the admission charged: the op's estimate, at most the
// capacity of the lane's token bucket (B-06-28) — what Keep books. A route
// compares what it would book with it instead of rebuilding it from the
// estimate and the limits (lead decision 2026-11-06, one source: a settings
// reload between the admission and the end of a request moves a rebuilt
// value, not this one).
func (r *Reservation) Held() int64 { return r.held }

// end closes r and moves the token bucket by delta ns. A debt is a new
// charge at now — booked from max(tat, now) on (the overrun was used, its
// moment inside the request is unknown, and booking it from the settle
// never counts too little; H-2) and into every open shadow. A refund of d
// takes min(d, max(tat, now) − max(shadow, now)) out of the bucket, and out
// of the shadow of every older open reservation the same way (r's charge is
// one of the charges after their admission) — never below now.
func (m *Meter) end(r *Reservation, delta int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[r.key]
	if st == nil {
		return
	}
	now := m.clock()
	st.drop(r)
	switch {
	case delta > 0:
		st.tok = satAdd(max(st.tok, now), delta)
		st.raise(now, delta)
	case delta < 0:
		from := max(r.shadow, now)
		st.tok = refund(st.tok, now, from, -delta)
		for _, o := range st.open {
			if o.seq < r.seq {
				o.shadow = refund(o.shadow, now, from, -delta)
			}
		}
	}
}

// refund takes d out of the bucket tat as far as the part of it that
// precedes the bucket from (the charges after the refunded reservation)
// allows: max(tat, now) − min(d, max(tat, now) − from), never below from.
func refund(tat, now, from, d int64) int64 {
	cur := max(tat, now)
	return cur - min(d, max(cur-from, 0))
}

// dim is one GCRA bucket of a limit as a lane sees it.
type dim struct {
	limit int64 // units per window
	tol   int64 // the lane's burst tolerance in ns (τ, or τ·(1 − r) for bulk)
	cap   int64 // units the lane's tolerance holds: ⌊tol·limit/W⌋, at least 1
}

func newDim(limit int64, lane Lane, r float64) dim {
	d := dim{limit: limit, tol: window}
	if lane == LaneBulk && r > 0 {
		// τ·(1 − r), at least one unit: a bulk request is never refused for
		// good (with r ≥ 1, or a limit too small to split, there is no
		// reserve — the named bound of design/06 §4.2 "C = 1").
		d.tol = max(int64(float64(window)*(1-r)), d.cost(1))
	}
	d.cap = max(mulDiv(d.tol, limit, window), 1)
	return d
}

// cost is the emission time of n units: ⌊n·W/limit⌋ ns, at least 1 for n ≥ 1.
func (d dim) cost(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return max(mulDiv(n, window, d.limit), 1)
}

// admit is GCRA for n units at now: admitted iff max(tat, now) + cost(n) −
// now ≤ tol; the wait of a refusal is that excess.
func (d dim) admit(tat, now, n int64) (next, wait int64, ok bool) {
	base := max(tat, now)
	need := satAdd(base-now, d.cost(n))
	if need > d.tol {
		return tat, need - d.tol, false
	}
	return satAdd(base, d.cost(n)), 0, true
}

// remaining are the units that fit now.
func (d dim) remaining(tat, now int64) int64 {
	free := d.tol - backlog(tat, now)
	if free <= 0 {
		return 0
	}
	return min(mulDiv(free, d.limit, window), d.cap)
}

func backlog(tat, now int64) int64 { return max(tat, now) - now }

// mulDiv is ⌊a·b/c⌋ for a, b ≥ 0 and c > 0 over 128 bits, saturated at
// math.MaxInt64.
func mulDiv(a, b, c int64) int64 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return math.MaxInt64
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// satAdd is a + b saturated at the int64 range.
func satAdd(a, b int64) int64 {
	s := a + b
	switch {
	case b > 0 && s < a:
		return math.MaxInt64
	case b < 0 && s > a:
		return math.MinInt64
	}
	return s
}
