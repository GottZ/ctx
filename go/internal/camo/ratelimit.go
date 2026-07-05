package camo

import (
	"sync"
	"time"
)

// rateLimiter is a per-key fixed-window counter guarding the sign endpoint
// against signature-oracle abuse (design §5, D2b restrisiko). It is in-memory
// and stateless-across-restarts by design: the sign endpoint is auth-gated and
// the fetch endpoint is SSRF-bounded, so the limiter's job is only to blunt a
// burst, not to be a durable quota. A background-free lazy sweep keeps the map
// from growing unbounded at 1M+ keys — expired windows are dropped on access.
type rateLimiter struct {
	mu       sync.Mutex
	limit    int
	window   time.Duration
	windows  map[string]*window
	lastweep time.Time
	now      func() time.Time // injectable clock for tests
}

type window struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, w time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  w,
		windows: make(map[string]*window),
		now:     time.Now,
	}
}

// Allow records one hit for key and reports whether it is within budget. A new
// or expired window resets the count.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	rl.sweep(now)

	wnd := rl.windows[key]
	if wnd == nil || now.Sub(wnd.start) >= rl.window {
		rl.windows[key] = &window{start: now, count: 1}
		return true
	}
	if wnd.count >= rl.limit {
		return false
	}
	wnd.count++
	return true
}

// sweep drops expired windows. It runs at most once per window duration (guarded
// by lastweep) so a hot path does not walk the whole map on every call.
func (rl *rateLimiter) sweep(now time.Time) {
	if now.Sub(rl.lastweep) < rl.window {
		return
	}
	rl.lastweep = now
	for k, wnd := range rl.windows {
		if now.Sub(wnd.start) >= rl.window {
			delete(rl.windows, k)
		}
	}
}
