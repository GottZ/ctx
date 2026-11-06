package oai

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
)

// requestReserveFactor is the request share of the in-flight budget per body
// byte (design/02 §4.2.1): the body, its RawMessage copy and the upstream
// body exist at the same time. A derived constant of the handler's memory
// shape, not a policy knob — the policy is gateway.inflight_budget.
const requestReserveFactor = 3

// scanReserveFactor and scanReserveBase are the heap the route books before
// the sensitivity scan of a request below credentials — factor × body +
// base (FOLGE-P hardening H-8; federation.ScanRaise, internal/federation/
// policy.go, is not changed here). Measured, not chosen: TotalAlloc of one
// scan call (the scanner's matches and the garbage it leaves, pattern pools
// cold) per body byte at 256 B … 8 MiB — prose, text with spaces, many
// messages, escaped text 3.0–3.8 at 1 MiB; tokens in tool_calls up to 17.8;
// a text of short secret-like assignments ("apikey:x ", each one a pattern
// match the scanner collects) 22.23 at 2 MiB (22.21 at 1 MiB, 22.06 at
// 4 MiB), the largest found (bau report FOLGE-P §Härtung H-8, §Härtung 2).
// The factor is the ceiling of that maximum, the base the largest remainder
// of a small body (47 334 bytes at 2 KiB: the patterns' matchers, allocated
// anew after a collection) rounded up. TestScanReserveIsTheMeasuredOne fails
// when a form needs more (the booking would no longer be true) or when the
// largest needs a whole byte less per body byte (it would cost requests for
// nothing).
//
// What the factor binds is what one scan call holds on the heap at once. One
// form is scanned twice in a row (Delta-Review 2 F-1): one key of the whole
// body with a scalar value, read as a key and again in the probe
// key + ": " + value — 40.4 (256 KiB) … 42.4 (1 MiB) bytes per body byte in
// total, but the first call's garbage is dead before the second begins: the
// live heap the collector marks during both is one scan's (9–11.6 bytes per
// body byte, as for the key scanned once), and over the route at 8 MiB the
// heap grew 142–150 MiB against the gate's 200.3 MiB (Delta-Review 2 §5b).
// TestScanTwiceHoldsOneScanAtOnce pins the form: at most two bookings in
// total, at most one held at once.
//
// The promise "the heap holds no more than the bookings" is made under a set
// process memory limit (GOMEMLIMIT; the image sets 200MiB, go/Dockerfile) —
// a bare ctxd without one does not get it. A request's booking ends with
// the request, its scan's garbage stays on the heap until the next
// collection, and the next request books the budget the first one freed;
// without a limit the pacer lets that garbage grow to GOGC × the live heap.
// Hostile clients in a closed loop (no Retry-After, 40 × 1 MiB or 3 × 8 MiB
// of this form for 20–30 s, Delta-Review 3 R3-1): without GOMEMLIMIT the
// heap passed the budget's peak plus the harness load in 7 of 13 runs, up to
// 294.0 MiB against 248.8 MiB (the form scanned once: 1 of 7); with
// GOMEMLIMIT=200MiB in none (151.5–198.1 MiB). The heap gate's sustained
// case runs it under that limit (chat_parse_mem_test.go; bau report
// §Härtung 3): 9 of 9 runs within the bound, 183.7–202.8 MiB; without the
// limit 4 of 6 over it (up to 281.4 MiB).
const (
	scanReserveFactor = 23
	scanReserveBase   = 64 << 10
)

// RequestReserveFactor, ScanReserveFactor and ScanReserveBase are the
// constants above as the configuration's budget floor reads them (V39,
// config/validate.go gatewayBudgetFloor: one request at every cap must fit
// gateway.inflight_budget). config imports oai, never the reverse (depguard
// config-layering), so the values are defined here once and read there.
const (
	RequestReserveFactor = requestReserveFactor
	ScanReserveFactor    = scanReserveFactor
	ScanReserveBase      = scanReserveBase
)

// ErrBudgetExhausted is the read error of a body without a declared length
// once its running reservation would pass the in-flight budget. A handler
// that sees it answers WriteOverloaded.
var ErrBudgetExhausted = errors.New("oai: in-flight budget exhausted")

// Budget is the process-wide in-flight memory budget of the gateway
// (gateway.inflight_budget, design/02 §4.2.1, B9): the sum of every running
// request's reservation — the request share at entry and, from the handler,
// the response share. One Budget per process, shared by the /v1 and /v2
// groups; the zero value is ready to use. It never waits (L5): a
// reservation that does not fit is refused at once.
type Budget struct {
	used atomic.Int64
}

// InUse reports the bytes currently reserved.
func (b *Budget) InUse() int64 { return b.used.Load() }

// tryAdd takes n bytes if used+n stays within limit.
func (b *Budget) tryAdd(n, limit int64) bool {
	if n < 0 {
		return false
	}
	for {
		cur := b.used.Load()
		if n > limit-cur {
			return false
		}
		if b.used.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// NewReservation opens an empty reservation against limit (the budget value
// of the request's settings snapshot).
func (b *Budget) NewReservation(limit int64) *Reservation {
	return &Reservation{b: b, limit: limit}
}

// Reservation is one request's share of the Budget. Grow adds to it, Release
// returns all of it and seals it; both are safe for concurrent use. A Grow
// after (or racing with) Release takes nothing: a late writer — a relay
// goroutine still sizing its response share after the handler returned —
// must not leave bytes behind that no one will ever return, or the budget
// would shrink until every gateway request is refused.
type Reservation struct {
	b        *Budget
	limit    int64
	mu       sync.Mutex
	held     int64
	released bool
}

// Grow reserves n more bytes. It returns false — and reserves nothing — when
// the budget cannot hold them or the reservation is already released.
func (r *Reservation) Grow(n int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released || !r.b.tryAdd(n, r.limit) {
		return false
	}
	r.held += n
	return true
}

// Release returns everything the reservation holds and seals it.
// Idempotent.
func (r *Reservation) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	r.b.used.Add(-r.held)
	r.held = 0
}

type reservationKey struct{}

// ReservationFrom returns the request's reservation, nil outside the budget
// stage. Handlers grow it by their response share (design/02 §4.2.1) and
// answer WriteOverloaded when that fails.
func ReservationFrom(ctx context.Context) *Reservation {
	r, _ := ctx.Value(reservationKey{}).(*Reservation)
	return r
}

// InflightBudget is stage 5 of the gateway stack (design/02 §4.2): it
// reserves the request share before the handler runs — 3 × Content-Length
// for a declared body, counted while reading for one without a declared
// length — and releases the whole reservation (request and response shares)
// by defer when the handler returns, panics included. A declared body that
// does not fit is refused at once with WriteOverloaded, before a byte of it
// is read and without the handler; an undeclared one fails its read with
// ErrBudgetExhausted.
func InflightBudget(b *Budget, get func() Settings) func(http.Handler) http.Handler {
	if b == nil || get == nil {
		panic("oai.InflightBudget: nil budget or settings getter")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res := b.NewReservation(get().InflightBudget)
			defer res.Release()
			switch cl := r.ContentLength; {
			case cl > 0:
				if cl > math.MaxInt64/requestReserveFactor || !res.Grow(requestReserveFactor*cl) {
					WriteOverloaded(w)
					return
				}
			case cl < 0 && r.Body != nil && r.Body != http.NoBody:
				r.Body = &budgetedBody{ReadCloser: r.Body, res: res}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reservationKey{}, res)))
		})
	}
}

// budgetedBody reserves 3 × every chunk it hands out.
type budgetedBody struct {
	io.ReadCloser
	res *Reservation
	err error
}

func (b *budgetedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.res.Grow(requestReserveFactor*int64(n)) {
		b.err = ErrBudgetExhausted
		return 0, b.err
	}
	return n, err
}
