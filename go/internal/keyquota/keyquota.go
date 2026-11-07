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
// The day and month budgets (wave W06-13, design/06 §4.2, §4.3) are
// calendar-UTC windows at three levels — the consumer × op, its tenant × op
// (the tenant envelope, counting only consumer tokens) and the op over all
// consumers (the instance envelope): per window the tokens and requests used,
// and per level the window-independent tokens the open reservations hold. A
// request passes when used + reserved + its own cost fits each bounded
// dimension of each level; a refusal waits until the window resets and is
// not retryable (insufficient_quota).
//
// State lives per (consumer, op) for the life of the process and is never
// removed when a key is revoked, rotated or its consumer disabled (B-06-19):
// a consumer cannot reset its own window by cycling keys. Only the budget
// counters of a level without an open reservation are dropped at the change
// of the UTC month, once their month is over. Single process: one meter per
// instance, like the dispatcher (design/06 §4.2 "Single-Process-Annahme").
package keyquota

import (
	"context"
	"errors"
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
	// Consumer, Tenant and Instance are the calendar-UTC budgets of the
	// subject's op (wave W06-13, design/06 §4.3): the consumer's own
	// (gateway.<op>_daily_tokens_default, consumer_monthly_tokens_default,
	// consumer_daily_requests_default), the envelope of its tenant over all
	// its consumers (gateway.tenant_envelope_factor × the consumer's) and the
	// instance's over all consumers (gateway.instance_envelope_factor × the
	// consumer's). A zero dimension is unbounded.
	Consumer, Tenant, Instance Budget
}

// Budget is one level's calendar-UTC budgets of an op (design/06 §4.2 rows
// "Tag/Monat", "Tenant-Hülle", "Instanz-Hülle"): tokens per UTC day, tokens
// per UTC month and requests per UTC day. 0 = the dimension is unbounded —
// the registry's "off" of the month and request budgets (design/06 §4.3);
// the daily token defaults are at least 1 (V50), so every op has a day
// budget at every level.
type Budget struct {
	DailyTokens, MonthlyTokens, DailyRequests int64
}

// Scale is b with every bounded dimension multiplied by f, floored and
// saturated: an envelope as a factor of the consumer's budget (design/06
// §4.3, tenant_env = factor × registry). An unbounded dimension stays
// unbounded. A factor below 1 (V50 refuses it) or not a number counts as 1:
// an envelope is never unbounded and never below the consumer's own budget.
func (b Budget) Scale(f float64) Budget {
	if !(f >= 1) {
		f = 1
	}
	return Budget{DailyTokens: scaleLimit(b.DailyTokens, f), MonthlyTokens: scaleLimit(b.MonthlyTokens, f),
		DailyRequests: scaleLimit(b.DailyRequests, f)}
}

func scaleLimit(l int64, f float64) int64 {
	if l <= 0 {
		return 0
	}
	p := math.Floor(float64(l) * f)
	if !(p < math.MaxInt64) {
		return math.MaxInt64
	}
	return max(int64(p), l)
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

// promptTokens is the prompt share of e for op: TokenEstimate of the chat
// prompt runes, of the input runes for embed and rerank — the prompt term of
// a forced settle's estimate (design/06 §4.2 "Usage-Quellen beim Settle").
func (e Estimate) promptTokens(op Op) int64 {
	if op == OpChat {
		return int64(promptguard.TokenEstimate(e.PromptRunes))
	}
	return int64(promptguard.TokenEstimate(e.InputRunes))
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

// The refusals of the day and month budgets (W06-13): the consumer's own
// dimension, or the envelope of its tenant or of the instance that bound.
// They are the ledger details of reason "quota" (design/06 §4.1 step 8).
const (
	ReasonDailyTokens      Reason = "daily_tokens"
	ReasonMonthlyTokens    Reason = "monthly_tokens"
	ReasonDailyRequests    Reason = "daily_requests"
	ReasonTenantEnvelope   Reason = "tenant_envelope"
	ReasonInstanceEnvelope Reason = "instance_envelope"
)

// Quota reports whether r is a refusal of a day or month budget: 429
// insufficient_quota, not retryable, Retry-After until the window resets
// (design/06 §4.4).
func (r Reason) Quota() bool {
	switch r {
	case ReasonDailyTokens, ReasonMonthlyTokens, ReasonDailyRequests, ReasonTenantEnvelope, ReasonInstanceEnvelope:
		return true
	}
	return false
}

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
	// pass if nothing else arrives (WarmingRetryAfter while unseeded); for
	// a budget refusal the time until its window resets — 00:00 UTC, or the
	// first of the next month at 00:00 UTC.
	RetryAfter time.Duration
	// Retryable is false for ReasonInvalid and the budget refusals
	// (Reason.Quota): x-should-retry false.
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
	// seededAt is the meter's clock at Seed and damp the (consumer, op) keys
	// the seed loaded (both under mu): a GCRA state of such a key born within
	// one window after the seed starts half full (burst damping); damp is
	// dropped once that window is over.
	seededAt int64
	damp     map[stateKey]struct{}

	mu    sync.Mutex
	state map[stateKey]*buckets
	// budgets are the day and month counters of every level (W06-13);
	// month is the UTC month the meter last saw (the sweep's trigger).
	budgets map[budgetKey]*usage
	month   int64
}

type stateKey struct {
	consumer string
	op       Op
}

// budgetLevel is one of the three levels of the day and month budgets.
type budgetLevel uint8

const (
	levelConsumer budgetLevel = iota
	levelTenant
	levelInstance
)

// budgetKey names one level's counters: the consumer, the tenant or the
// instance ("") with the op.
type budgetKey struct {
	level budgetLevel
	id    string
	op    Op
}

// usage are one level's counters (W06-13): the tokens and requests used in
// the UTC day day and the tokens used in the UTC month month (the calendar
// keys of calendarOf), and the tokens the level's open reservations hold —
// window-independent, they end with their reservation whatever the window.
// open counts those reservations: counters with an open reservation are
// never dropped.
type usage struct {
	day, month                          int64
	dayTokens, dayRequests, monthTokens int64
	reserved                            int64
	open                                int64
}

// roll moves u into the windows of day and month: a window that is over
// starts empty.
func (u *usage) roll(day, month int64) {
	if u.month != month {
		u.month, u.monthTokens = month, 0
	}
	if u.day != day {
		u.day, u.dayTokens, u.dayRequests = day, 0, 0
	}
}

// check is the admission of a request of n tokens on lane against b (each
// bounded dimension as the lane sees it, laneLimit): used + reserved + n_d ≤
// L with n_d = min(n, L) — a reservation above the budget is charged at it,
// so it passes an empty budget and is never refused for good (B-06-28) —
// and requests used + 1 ≤ L. The dimension that refuses with the longest
// wait names the refusal: the month before the day.
func (u *usage) check(b Budget, lane Lane, r float64, n int64) (Reason, bool) {
	if l := laneLimit(b.MonthlyTokens, lane, r); l > 0 && satAdd(satAdd(u.monthTokens, u.reserved), min(n, l)) > l {
		return ReasonMonthlyTokens, false
	}
	if l := laneLimit(b.DailyTokens, lane, r); l > 0 && satAdd(satAdd(u.dayTokens, u.reserved), min(n, l)) > l {
		return ReasonDailyTokens, false
	}
	if l := laneLimit(b.DailyRequests, lane, r); l > 0 && satAdd(u.dayRequests, 1) > l {
		return ReasonDailyRequests, false
	}
	return "", true
}

// laneLimit is a budget limit as lane sees it: the whole limit for the urgent
// lane, ⌊L·(1 − r)⌋ (at least 1) for the bulk lane (design/06 §4.2 "XB ≤ (1 −
// r)·L"); 0 stays unbounded. r is taken to the millionth like bulkSlots.
func laneLimit(l int64, lane Lane, r float64) int64 {
	if l <= 0 || lane != LaneBulk {
		return max(l, 0)
	}
	return bulkSlots(l, r)
}

// calendarOf are the calendar keys of t: the UTC day (days since the epoch)
// and the UTC month (year·12 + month).
func calendarOf(t time.Time) (day, month int64) {
	u := t.UTC()
	y, mo, d := u.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Unix() / 86400, int64(y)*12 + int64(mo)
}

// untilReset is the time from t to the reset of the window of reason r: the
// first of the next UTC month at 00:00 for the month, the next 00:00 UTC for
// the day.
func untilReset(t time.Time, r Reason) time.Duration {
	u := t.UTC()
	y, mo, d := u.Date()
	next := time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
	if r == ReasonMonthlyTokens {
		next = time.Date(y, mo+1, 1, 0, 0, 0, 0, time.UTC)
	}
	return next.Sub(u)
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

// New returns an unseeded meter over the clock now: until Seed every Admit
// answers warming (design/06 §4.1 step 7).
func New(now func() time.Time) *Meter {
	return &Meter{now: now, base: now(), state: map[stateKey]*buckets{}, budgets: map[budgetKey]*usage{}}
}

// clock is the meter's time: ns since base on the monotonic clock when now
// carries one.
func (m *Meter) clock() int64 { return int64(m.now().Sub(m.base)) }

// MarkSeeded opens the meter without the boot seed and without the burst
// damping: a meter that has been running for a while, the test seam of the
// minute buckets and L1. ctxd opens its meter with Seed (W06-13).
func (m *Meter) MarkSeeded() { m.seeded.Store(true) }

// SeedRow is what one consumer used of one op in the running UTC day and
// month before this process started (llmusage.SeedConsumers over the rollup,
// design/06 §4.2 "Boot-Seed"): its tokens and requests of the day and its
// tokens of the month.
type SeedRow struct {
	ConsumerID, TenantID                string
	Op                                  Op
	DayTokens, DayRequests, MonthTokens int64
}

// ErrSeeded refuses a second seed: it would count the rollup twice.
var ErrSeeded = errors.New("keyquota: meter already seeded")

// Seed loads rows, read at at, into the three budget levels — each row into
// its consumer, its tenant's envelope and the instance's — and opens the
// meter (design/06 §4.2 "Boot-Seed", fail-closed until it ran: the caller
// retries a failed read and the meter answers warming meanwhile). A row
// counts into the UTC day and month of at: a seed read before 00:00 UTC
// whose meter admits after it keeps the month and loses the day that is
// over, exactly like any window. A row without consumer, tenant or a known
// op counts nowhere. Because the seed loads every consumer with rollup rows
// in the running month, a consumer without a row has used exactly 0 — no
// lazy seed per subject (B-06-19). From the seed on, a GCRA state of a
// (consumer, op) the seed loaded that is born within one window starts at
// tat = now + τ/2 (burst damping): after a restart no consumer that was
// active gets the whole burst at once again. A (consumer, op) without a row
// in the running month used nothing in it — its buckets were full before
// the restart too, and it keeps them (deviation from design/06 §4.2 "jeder
// GCRA-Zustand", W06-13 report). Exactly once: a second call is ErrSeeded
// and changes nothing.
func (m *Meter) Seed(at time.Time, rows []SeedRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seeded.Load() {
		return ErrSeeded
	}
	day, month := calendarOf(at)
	m.damp = map[stateKey]struct{}{}
	for _, row := range rows {
		if _, ok := ParseOp(string(row.Op)); !ok || row.ConsumerID == "" || row.TenantID == "" {
			continue
		}
		m.damp[stateKey{row.ConsumerID, row.Op}] = struct{}{}
		for _, key := range [3]budgetKey{
			{levelConsumer, row.ConsumerID, row.Op}, {levelTenant, row.TenantID, row.Op}, {levelInstance, "", row.Op},
		} {
			u := m.budgets[key]
			if u == nil {
				u = &usage{}
				m.budgets[key] = u
			}
			u.roll(day, month)
			u.dayTokens = satAdd(u.dayTokens, max(row.DayTokens, 0))
			u.dayRequests = satAdd(u.dayRequests, max(row.DayRequests, 0))
			u.monthTokens = satAdd(u.monthTokens, max(row.MonthTokens, 0))
		}
	}
	m.month = month
	m.seededAt = m.clock()
	m.seeded.Store(true)
	return nil
}

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
	wall := m.now()
	now := int64(wall.Sub(m.base))
	if m.damp != nil && now-m.seededAt >= window {
		m.damp = nil
	}
	st := m.state[key]
	if st == nil {
		st = &buckets{}
		if _, ok := m.damp[key]; ok {
			// Burst damping (design/06 §4.2): a state of a key the seed
			// loaded, born in the first window after it, starts half full.
			st.req, st.tok = now+window/2, now+window/2
		}
	}
	levels := m.levels(s, l, wall)
	// A budget refusal comes first: its window outlasts any minute bucket and
	// a retry cannot help before it resets (design/06 §4.4).
	for _, lv := range levels {
		if reason, ok := lv.u.check(lv.b, s.Lane, l.XUReserve, n); !ok {
			return nil, quotaRefused(s, lv, reason, l, wall)
		}
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
	var held [3]*usage
	for i, lv := range levels {
		m.budgets[lv.key] = lv.u
		lv.u.reserved = satAdd(lv.u.reserved, n)
		lv.u.dayRequests = satAdd(lv.u.dayRequests, 1)
		lv.u.open++
		held[i] = lv.u
	}
	st.raise(now, nextTok-max(st.tok, now))
	st.req, st.tok = nextReq, nextTok
	h := RateHeaders{
		LimitRequests: req.cap, RemainingRequests: req.remaining(st.req, now), ResetRequests: time.Duration(backlog(st.req, now)),
		LimitTokens: tok.cap, RemainingTokens: tok.remaining(st.tok, now), ResetTokens: time.Duration(backlog(st.tok, now)),
	}
	r := &Reservation{m: m, key: key, lane: s.Lane, tpm: l.TPM, charged: tok.cost(n), held: n, shadow: now, headers: h, budget: held}
	r.promptEst, r.maxCompletion = e.promptTokens(s.Op), int64(max(e.MaxCompletion, 0))
	st.add(r)
	return r, Decision{Allowed: true, Retryable: true, Headers: h}
}

// budgetLevelOf is one level of a request's budgets: its counters (rolled
// into the request's windows, not yet stored for a new key) and its limits.
type budgetLevelOf struct {
	key   budgetKey
	u     *usage
	b     Budget
	level budgetLevel
}

// levels are the three budget levels of s under l at wall, in the order the
// admission checks them: the consumer, its tenant, the instance. Called
// under m.mu; a key seen for the first time gets fresh counters, stored only
// when the request is admitted (a refusal changes no state). The first
// request of a new UTC month drops the counters of the months before that
// hold no open reservation.
func (m *Meter) levels(s Subject, l Limits, wall time.Time) [3]budgetLevelOf {
	day, month := calendarOf(wall)
	if month != m.month {
		m.sweep(month)
	}
	out := [3]budgetLevelOf{
		{key: budgetKey{levelConsumer, s.ConsumerID, s.Op}, b: l.Consumer, level: levelConsumer},
		{key: budgetKey{levelTenant, s.TenantID, s.Op}, b: l.Tenant, level: levelTenant},
		{key: budgetKey{levelInstance, "", s.Op}, b: l.Instance, level: levelInstance},
	}
	for i := range out {
		u := m.budgets[out[i].key]
		if u == nil {
			u = &usage{}
		}
		u.roll(day, month)
		out[i].u = u
	}
	return out
}

// sweep drops the budget counters of the months before month that hold no
// open reservation (design/06 §4.2 "Zustands-Lebensdauer": removed only at
// the change of the UTC month, when the key has no activity in the new
// month — a key that comes back starts at 0 used, which is what it used in
// the new month). O(keys), once per month. Called under m.mu.
func (m *Meter) sweep(month int64) {
	for k, u := range m.budgets {
		if u.month < month && u.open == 0 {
			delete(m.budgets, k)
		}
	}
	m.month = month
}

// quotaRefused is the decision of a budget refusal: insufficient quota, not
// retryable, Retry-After until the window resets. The message names the
// consumer's own limit as its lane sees it; an envelope refusal names no
// value — the tenant's sum is not the consumer's own limit, the instance's
// is the deployment's (B6 doctrine, design/06 §4.4).
func quotaRefused(s Subject, lv budgetLevelOf, dim Reason, l Limits, wall time.Time) Decision {
	what, unit, limit := "Daily token", "tokens per UTC day", lv.b.DailyTokens
	switch dim {
	case ReasonMonthlyTokens:
		what, unit, limit = "Monthly token", "tokens per UTC month", lv.b.MonthlyTokens
	case ReasonDailyRequests:
		what, unit, limit = "Daily request", "requests per UTC day", lv.b.DailyRequests
	}
	d := Decision{Reason: dim, RetryAfter: untilReset(wall, dim)}
	switch lv.level {
	case levelTenant:
		d.Reason = ReasonTenantEnvelope
		d.Message = fmt.Sprintf("%s quota of your tenant reached (%s).", what, s.Op)
	case levelInstance:
		d.Reason = ReasonInstanceEnvelope
		d.Message = fmt.Sprintf("Capacity quota for this deployment reached (%s).", s.Op)
	default:
		lane := ""
		if s.Lane == LaneBulk {
			lane = ", bulk lane"
		}
		d.Message = fmt.Sprintf("%s quota reached (%s%s): %d %s.", what, s.Op, lane, laneLimit(limit, s.Lane, l.XUReserve), unit)
	}
	return d
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
	// budget are the counters of the request's three budget levels: each
	// holds held tokens of it in reserved until the reservation ends.
	budget [3]*usage
	once   sync.Once
	// The forced settle's inputs (W06-13 (d)): the prompt share of the
	// admission's estimate, the request's completion maximum (0 = none),
	// the runes the route handed on (AddStreamed) and the route's writer
	// (OnForcedSettle); forced reports that ForceSettleAll ended it.
	promptEst, maxCompletion int64
	streamed                 atomic.Int64
	finalizer                atomic.Pointer[func(Usage)]
	forced                   atomic.Bool
}

// AddStreamed counts runes a stream handed on to its client (design/06 §4.2:
// the completion term of an estimate). Safe from the relay's goroutine; a
// negative count counts nothing.
func (r *Reservation) AddStreamed(runes int) {
	if runes > 0 {
		r.streamed.Add(int64(runes))
	}
}

// OnForcedSettle registers fn as the writer of the request's one row should
// the shutdown's ForceSettleAll end the reservation: fn gets the estimate the
// meter booked and runs at most once, and only when the forced settle — not
// the route's own end — ended the reservation (Forced). The last
// registration wins.
func (r *Reservation) OnForcedSettle(fn func(Usage)) {
	if fn != nil {
		r.finalizer.Store(&fn)
	}
}

// Forced reports whether ForceSettleAll ended the reservation: its writer
// then wrote (or writes) the row, and the route writes none.
func (r *Reservation) Forced() bool { return r.forced.Load() }

// ForceSettleAll ends every reservation still open with usage=estimated and
// calls its writer once (design/06 §4.2 "Shutdown-Reihenfolge" step 2, B-06-9:
// after srv.Shutdown, before the rejection ledger's flush and the llmlog
// settle, so the rows of requests the HTTP drain left running reach the
// rollup the next boot seeds from). The estimate is the one of a stream cut
// short (B-06-8): prompt = TokenEstimate of the admission's prompt or input
// runes, completion = TokenEstimate of the runes handed on (AddStreamed), at
// most the request's maximum; the meter books it like a route books a
// stream's estimate — below what the reservation holds Settle, else Keep. A
// reservation that ends meanwhile through its route ends once all the same
// (one sync.Once): exactly one of the two ends it, and the writer runs only
// for the forced one. A request a revoked key ended (W01-8) is no different.
// It stops before the next reservation when ctx is done and returns the
// number it ended.
func (m *Meter) ForceSettleAll(ctx context.Context) int {
	m.mu.Lock()
	var open []*Reservation
	for _, st := range m.state {
		open = append(open, st.open...)
	}
	m.mu.Unlock()
	n := 0
	for _, r := range open {
		if ctx.Err() != nil {
			break
		}
		if r.forceSettle() {
			n++
		}
	}
	return n
}

// forceSettle ends r with its estimate unless it ended already; it reports
// whether it did. The writer runs after the meter booked, outside the once.
func (r *Reservation) forceSettle() bool {
	var u Usage
	ran := false
	r.once.Do(func() {
		completion := int64(promptguard.TokenEstimate(int(min(r.streamed.Load(), math.MaxInt32))))
		if r.maxCompletion > 0 {
			completion = min(completion, r.maxCompletion)
		}
		u = Usage{PromptTokens: int(min(r.promptEst, math.MaxInt32)), CompletionTokens: int(completion)}
		r.forced.Store(true)
		ran = true
		actual := satAdd(r.promptEst, completion)
		if actual >= r.held {
			r.m.end(r, 0, r.held)
			return
		}
		r.m.end(r, dim{limit: r.tpm}.cost(actual)-r.charged, actual)
	})
	if ran {
		if fn := r.finalizer.Load(); fn != nil {
			(*fn)(u)
		}
	}
	return ran
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
		r.m.end(r, dim{limit: r.tpm}.cost(actual)-r.charged, actual)
	})
}

// Keep ends a reservation keeping its whole charge: a wire call was made but
// its usage is unknown (an upstream error, an answer without usage, a panic
// or a gone client after the wire) — after wire contact nothing is given
// back.
func (r *Reservation) Keep() {
	r.once.Do(func() { r.m.end(r, 0, r.held) })
}

// Abort ends a reservation without a wire call: its charge goes back as far
// as the bucket still holds it (all of it unless the bucket drained below it
// meanwhile), the request stays counted (design/06 §4.2).
func (r *Reservation) Abort() {
	r.once.Do(func() { r.m.end(r, -r.charged, 0) })
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
// capacity of the lane's token bucket (B-06-28) — what Keep books. It is
// set at the admission; Keep, Settle and Abort move the bucket, never it. A
// route compares what it would book with it instead of rebuilding it (lead
// decision 2026-11-06, one source): what it weighs its booking against is
// then what Keep books by construction, in every lane and under every cap.
// A rebuild from the admission's own headers misses only a limiter that
// reports no capacity; one from the settings also moves with a reload
// between the admission and the end of the request.
func (r *Reservation) Held() int64 { return r.held }

// end closes r, books used tokens into the day and month windows of its
// budget levels (where the reservation's hold ends) and moves the token
// bucket by delta ns. A debt is a new
// charge at now — booked from max(tat, now) on (the overrun was used, its
// moment inside the request is unknown, and booking it from the settle
// never counts too little; H-2) and into every open shadow. A refund of d
// takes min(d, max(tat, now) − max(shadow, now)) out of the bucket, and out
// of the shadow of every older open reservation the same way (r's charge is
// one of the charges after their admission) — never below now.
func (m *Meter) end(r *Reservation, delta, used int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.budget[0] != nil {
		day, month := calendarOf(m.now())
		for _, u := range r.budget {
			u.roll(day, month)
			u.reserved = max(u.reserved-r.held, 0)
			u.open--
			u.dayTokens = satAdd(u.dayTokens, used)
			u.monthTokens = satAdd(u.monthTokens, used)
		}
	}
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
