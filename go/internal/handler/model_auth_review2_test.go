package handler

// Review-2 gates of W01-6 (reports/review/W01-6-fremdreview-2.md): the
// spellings of CTX_TRUSTED_PROXY (FR2-1 / Copilot on PR #98), the lock's
// Retry-After as the rest of the window end to end (FR2-2, mutant M15) and the
// semaphore under cancellation stress (FR2-3, mutant M23). Unit level: the
// lazy pool of newUnitDoor answers every database step with 503
// auth_unavailable.

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/modelkey"
)

// FR2-1: every spelling an operator may give CTX_TRUSTED_PROXY — plain,
// padded, with a port, bracketed, bracketed with a port, upper case,
// IPv4-mapped — names the same proxy; a value that is no address at all is
// logged and treated as unset, so a request through the proxy answers 503
// proxy_misconfigured (loud) instead of taking the proxy for the client
// (silent B22 state: allowlists would check the proxy's address).
func TestModelAuthTrustedProxySpellings(t *testing.T) {
	client := netip.MustParseAddr("198.51.100.7")
	cases := []struct {
		trusted, remote string
	}{
		{"192.0.2.10", "192.0.2.10:1"},
		{" 192.0.2.10 ", "192.0.2.10:1"},
		{"192.0.2.10:443", "192.0.2.10:1"},
		{"[2001:db8::10]", "[2001:db8::10]:1"},
		{"[2001:db8::10]:443", "[2001:db8::10]:1"},
		{"2001:DB8::10", "[2001:db8::10]:1"},
		{"::ffff:192.0.2.10", "192.0.2.10:1"},
	}
	for _, c := range cases {
		e := newUnitDoor(t, c.trusted)
		r := unitReq(func(r *http.Request) {
			r.RemoteAddr = c.remote
			r.Header.Add("X-Forwarded-For", client.String())
			r.Header.Add("X-Forwarded-Proto", "https")
		})
		ip, secure := e.door.origin(r)
		if ip != client || !secure {
			t.Errorf("trusted %q, peer %q: origin = %v secure=%v, want %v secure=true (the proxy is trusted)", c.trusted, c.remote, ip, secure, client)
		}
		if got := e.logs.String(); strings.Contains(got, "not an IP address") {
			t.Errorf("trusted %q: logged as invalid: %s", c.trusted, got)
		}
	}
	for _, bad := range []string{"reversed", "192.0.2.10/32", "http://192.0.2.10"} {
		e := newUnitDoor(t, bad)
		if !strings.Contains(e.logs.String(), "not an IP address") {
			t.Errorf("trusted %q: no ERROR about the value, logs: %q", bad, e.logs.String())
		}
		k, _ := modelkey.Generate()
		rec := e.serve(unitReq(func(r *http.Request) {
			r.RemoteAddr = "192.0.2.10:1"
			r.Header.Set("X-Forwarded-For", client.String())
			r.Header.Set("Authorization", "Bearer "+k.Plaintext())
		}))
		if rec.Code != http.StatusServiceUnavailable || e.door.Stats().ProxyMisconfigured != 1 {
			t.Errorf("trusted %q + X-Forwarded-For: %d, proxy_misconfigured=%d — want 503 counted once (treated as unset, loud)", bad, rec.Code, e.door.Stats().ProxyMisconfigured)
		}
		// Without X-Forwarded-For the direct peer is the client, as unset.
		if ip, secure := e.door.origin(unitReq(func(r *http.Request) { r.RemoteAddr = "192.0.2.10:1" })); ip != netip.MustParseAddr("192.0.2.10") || secure {
			t.Errorf("trusted %q, no X-Forwarded-For: origin = %v secure=%v, want the peer, not secure", bad, ip, secure)
		}
	}
}

// FR2-2 (reviewer probe FR2-U1): Retry-After of the lock is the REST of the
// window, end to end through the door (stage 5 is before the database), and
// the window expires; the lock is per (address, key).
func TestModelAuthLockRetryAfterIsTheRest(t *testing.T) {
	e := newUnitDoor(t, "")
	clk := newFakeClock()
	e.door.limiter = newFailLimiter(modelAuthLimiterCap, clk.now)
	s := e.door.settings().Auth
	k1, _ := modelkey.Generate()
	k2, _ := modelkey.Generate()
	ip := netip.MustParseAddr("192.0.2.1")
	lk := limiterKey(ip, k1.HashPrefix())
	for i := 0; i < s.FailureLimit; i++ {
		e.door.limiter.fail(lk, s.FailureWindow)
	}
	do := func(k modelkey.Key, remote string) *http.Request {
		return unitReq(func(r *http.Request) {
			r.RemoteAddr = remote
			r.Header.Set("Authorization", "Bearer "+k.Plaintext())
		})
	}
	steps := []struct {
		adv  time.Duration
		code int
		ra   string
	}{
		{0, http.StatusTooManyRequests, "60"},
		{44*time.Second + 500*time.Millisecond, http.StatusTooManyRequests, "16"},
		{15*time.Second + 499*time.Millisecond, http.StatusTooManyRequests, "1"},
		{time.Millisecond, http.StatusServiceUnavailable, ""},
	}
	for i, st := range steps {
		clk.add(st.adv)
		rec := e.serve(do(k1, "192.0.2.1:40000"))
		if rec.Code != st.code || rec.Header().Get("Retry-After") != st.ra {
			t.Errorf("step %d (+%v): %d Retry-After=%q, want %d %q", i, st.adv, rec.Code, rec.Header().Get("Retry-After"), st.code, st.ra)
		}
	}
	for i := 0; i < s.FailureLimit; i++ {
		e.door.limiter.fail(lk, s.FailureWindow)
	}
	if rec := e.serve(do(k2, "192.0.2.1:40000")); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("K2 from the same address: %d, want 503 (reached the DB step — the lock is per key)", rec.Code)
	}
	if rec := e.serve(do(k1, "192.0.2.2:40000")); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("K1 from another address: %d, want 503 (reached the DB step — the lock is per address)", rec.Code)
	}
}

// FR2-3 (reviewer probe FR2-U6): the semaphore under cancellation stress with
// hot limits, under -race: no leaked slot, no stranded waiter, never more
// holders than the largest limit — a slot granted to a context that ended in
// the same instant is handed on (mutant M23 strands it).
func TestAuthSemaphoreStress(t *testing.T) {
	s := newAuthSemaphore()
	var cur, peak, granted, cancelled atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 96; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				limit := 1 + rand.IntN(5)
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.IntN(300))*time.Microsecond)
				rel, err := s.acquire(ctx, limit)
				cancel()
				if err != nil {
					cancelled.Add(1)
					continue
				}
				granted.Add(1)
				n := cur.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(time.Duration(rand.IntN(50)) * time.Microsecond)
				cur.Add(-1)
				rel()
			}
		}()
	}
	wg.Wait()
	if s.holders() != 0 || s.waiting() != 0 || peak.Load() > 5 {
		t.Fatalf("leak/overrun after the storm: holders=%d waiting=%d peak=%d (granted %d, cancelled %d)", s.holders(), s.waiting(), peak.Load(), granted.Load(), cancelled.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rel, err := s.acquire(ctx, 1)
	if err != nil {
		t.Fatalf("acquire after the storm at limit 1: %v", err)
	}
	rel()
}
