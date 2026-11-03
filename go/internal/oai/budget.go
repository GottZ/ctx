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
