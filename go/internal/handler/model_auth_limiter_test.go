package handler

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// Test-only views of the door's state (kept out of the production files so
// the deadcode ratchet sees no test-only method there).
func (l *failLimiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lru.Len()
}

func (l *failLimiter) has(k failKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.idx[k]
	return ok
}

func (k *knownGoodLRU) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lru.Len()
}

func (s *authSemaphore) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiters.Len()
}

func (s *authSemaphore) holders() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// fakeClock is a settable clock for the limiter, bucket and window tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)} }

func fk(i int) failKey {
	ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
	return limiterKey(ip, [8]byte{byte(i), 1})
}

// TestFailLimiterSemantics pins the normative lock semantics of design/01
// §4.2: an unknown key is never blocked, a full map never blocks, a bucket
// blocks once its count reaches the limit inside the window, and the window
// runs out.
func TestFailLimiterSemantics(t *testing.T) {
	clk := newFakeClock()
	l := newFailLimiter(4, clk.now)
	const limit, window = 3, time.Minute

	if b, _ := l.blocked(fk(1), limit, window); b {
		t.Fatal("an unknown key is blocked")
	}
	for i := 0; i < limit-1; i++ {
		l.fail(fk(1), window)
	}
	if b, _ := l.blocked(fk(1), limit, window); b {
		t.Fatalf("blocked after %d < %d failures", limit-1, limit)
	}
	l.fail(fk(1), window)
	b, retry := l.blocked(fk(1), limit, window)
	if !b || retry != window {
		t.Fatalf("after %d failures: blocked=%v retry=%v, want true/%v", limit, b, retry, window)
	}
	clk.add(40 * time.Second)
	if b, retry := l.blocked(fk(1), limit, window); !b || retry != 20*time.Second {
		t.Fatalf("40 s in: blocked=%v retry=%v, want true/20s", b, retry)
	}
	clk.add(20 * time.Second)
	if b, _ := l.blocked(fk(1), limit, window); b {
		t.Fatal("still blocked after the window ran out")
	}
	// A failure after the window opens a fresh window with count 1.
	l.fail(fk(1), window)
	if b, _ := l.blocked(fk(1), 2, window); b {
		t.Fatal("the expired window's count survived")
	}

	// Full map: an unknown key is still not blocked (B26), and fail on a
	// full map evicts the least recently touched window.
	for i := 2; i <= 4; i++ {
		l.fail(fk(i), window)
	}
	if l.len() != 4 {
		t.Fatalf("len %d, want 4", l.len())
	}
	if b, _ := l.blocked(fk(99), limit, window); b {
		t.Fatal("a full map blocks an unknown key — the deny-new policy of ipLimiter")
	}
	l.fail(fk(2), window) // touch 2: LRU order is now 1, 3, 4, 2
	l.fail(fk(5), window) // evicts 1
	if l.len() != 4 || l.has(fk(1)) || !l.has(fk(2)) || !l.has(fk(5)) {
		t.Fatalf("eviction: len=%d has1=%v has2=%v has5=%v, want 4/false/true/true", l.len(), l.has(fk(1)), l.has(fk(2)), l.has(fk(5)))
	}
	l.fail(fk(6), window) // evicts 3
	if l.has(fk(3)) || !l.has(fk(4)) {
		t.Fatalf("second eviction took the wrong window: has3=%v has4=%v", l.has(fk(3)), l.has(fk(4)))
	}
}

// TestLimiterKeyPrefixes pins the key's network part: IPv4 /32, IPv6 /64,
// IPv4-mapped IPv6 as IPv4, an invalid address in one shared bucket.
func TestLimiterKeyPrefixes(t *testing.T) {
	hp := [8]byte{1, 2, 3}
	same := func(a, b string) bool {
		return limiterKey(netip.MustParseAddr(a), hp) == limiterKey(netip.MustParseAddr(b), hp)
	}
	if same("10.0.0.1", "10.0.0.2") {
		t.Error("two IPv4 hosts share a bucket")
	}
	if !same("2001:db8::1", "2001:db8::ffff:1") {
		t.Error("two hosts of one IPv6 /64 have separate buckets")
	}
	if same("2001:db8::1", "2001:db8:0:1::1") {
		t.Error("two IPv6 /64s share a bucket")
	}
	if !same("::ffff:10.0.0.1", "10.0.0.1") {
		t.Error("IPv4-mapped IPv6 is not the IPv4 host")
	}
	if limiterKey(netip.MustParseAddr("10.0.0.1"), hp) == limiterKey(netip.MustParseAddr("10.0.0.1"), [8]byte{9}) {
		t.Error("two keys from one address share a bucket (B27)")
	}
	if inv := limiterKey(netip.Addr{}, hp); inv.prefix != (netip.Prefix{}) || inv == limiterKey(netip.MustParseAddr("0.0.0.0"), hp) {
		t.Errorf("an invalid address keys on %v, want the zero prefix (one shared bucket per key, apart from every real address)", inv.prefix)
	}
}

// TestKnownGoodLRU pins the capacity and LRU order of the probe-token
// exemption.
func TestKnownGoodLRU(t *testing.T) {
	k := newKnownGood(3)
	for _, h := range []string{"a", "b", "c"} {
		k.add(h)
	}
	k.add("a") // order: b, c, a
	k.add("d") // evicts b
	if k.contains("b") || !k.contains("a") || !k.contains("c") || !k.contains("d") || k.len() != 3 {
		t.Fatalf("LRU: b=%v a=%v c=%v d=%v len=%d", k.contains("b"), k.contains("a"), k.contains("c"), k.contains("d"), k.len())
	}
	k.remove("c")
	k.remove("zz")
	if k.contains("c") || k.len() != 2 {
		t.Fatalf("remove: c=%v len=%d", k.contains("c"), k.len())
	}
}

// TestProbeBucket pins the token bucket: burst = rate, refill at rate per
// second, a hot rate change takes effect at once.
func TestProbeBucket(t *testing.T) {
	clk := newFakeClock()
	b := newProbeBucket(clk.now)
	n := 0
	for b.take(5) {
		n++
		if n > 100 {
			t.Fatal("bucket never empties")
		}
	}
	if n != 5 {
		t.Fatalf("burst %d, want 5", n)
	}
	clk.add(200 * time.Millisecond) // +1 token
	if !b.take(5) || b.take(5) {
		t.Fatal("refill after 200 ms at 5/s is not exactly one token")
	}
	clk.add(10 * time.Second) // refill caps at the burst
	n = 0
	for b.take(5) {
		n++
	}
	if n != 5 {
		t.Fatalf("burst after a long pause %d, want 5 (capped)", n)
	}
	clk.add(time.Second) // at rate 2 the bucket holds at most 2
	n = 0
	for b.take(2) {
		n++
	}
	if n != 2 {
		t.Fatalf("after lowering the rate to 2: %d tokens, want 2", n)
	}
	if b.take(0) {
		t.Fatal("rate 0 hands out a token")
	}
}

// TestAuthSemaphore pins the DB semaphore: at most limit holders, FIFO
// waiters, a deadline ends the wait, a raised limit admits waiters at once.
func TestAuthSemaphore(t *testing.T) {
	s := newAuthSemaphore()
	ctx := context.Background()
	r1, err := s.acquire(ctx, 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := s.acquire(short, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire over the limit: err=%v, want DeadlineExceeded", err)
	}
	if s.waiting() != 0 {
		t.Fatalf("a timed-out waiter stays queued: %d", s.waiting())
	}

	// FIFO: three waiters are admitted in arrival order.
	order := make(chan int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel, err := s.acquire(ctx, 1)
			if err != nil {
				t.Errorf("waiter %d: %v", i, err)
				return
			}
			order <- i
			rel()
		}(i)
		for s.waiting() != i+1 {
			time.Sleep(time.Millisecond)
		}
	}
	r1()
	wg.Wait()
	close(order)
	got := fmt.Sprint(drain(order))
	if got != "[0 1 2]" {
		t.Fatalf("waiter order %s, want [0 1 2]", got)
	}

	// A raised limit admits a waiter without a release.
	rA, _ := s.acquire(ctx, 1)
	done := make(chan func())
	go func() {
		rel, _ := s.acquire(ctx, 1)
		done <- rel
	}()
	for s.waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	rB, err := s.acquire(ctx, 3) // limit 3: this caller and the waiter both get in
	if err != nil {
		t.Fatalf("acquire at the raised limit: %v", err)
	}
	select {
	case rel := <-done:
		rel()
	case <-time.After(2 * time.Second):
		t.Fatal("the raised limit did not admit the queued waiter")
	}
	rA()
	rB()
	if h := s.holders(); h != 0 {
		t.Fatalf("%d holders after every release", h)
	}
}

func drain(ch chan int) []int {
	var out []int
	for v := range ch {
		out = append(out, v)
	}
	return out
}
