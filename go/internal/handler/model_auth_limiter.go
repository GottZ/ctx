package handler

import (
	"container/list"
	"context"
	"net/netip"
	"sync"
	"time"
)

// Mechanism constants of the model-key door (design/01 §4.9
// "Hardcoded-legitim"): sizes and a deadline, not policy. The policy values
// — failure limit and window, probe rate, DB concurrency — are the
// gateway.auth_* settings.
const (
	// modelAuthLimiterCap bounds the failure-lock map (the size of the
	// login limiter, ipLimiterMaxBuckets). A full map evicts its least
	// recently touched window instead of refusing new keys (B26).
	modelAuthLimiterCap = 8192
	// modelAuthKnownGoodCap bounds the hashes exempt from the probe token.
	modelAuthKnownGoodCap = 4096
	// modelAuthDBDeadline is the whole budget of the door's database step —
	// semaphore wait, pool acquire and ctx_model_auth — before a 503: well
	// under the SDKs' timeout; the throughput knob is the concurrency, not
	// this deadline.
	modelAuthDBDeadline = time.Second
	// modelAuthIPv6Bits is the IPv6 prefix of the lock key: one /64 is one
	// host's rotation space (B26).
	modelAuthIPv6Bits = 64
)

// failKey is the failure lock's key (design/01 §4.2 stage 5): the client
// network (IPv4 /32, IPv6 /64) and the first 8 bytes of SHA-256 of the key.
// Behind a shared address a stale key locks only itself (B27).
type failKey struct {
	prefix netip.Prefix
	hash   [8]byte
}

// limiterKey builds the key; an invalid address maps to the zero prefix, one
// shared bucket per key hash.
func limiterKey(ip netip.Addr, hashPrefix [8]byte) failKey {
	ip = ip.Unmap().WithZone("")
	var p netip.Prefix
	switch {
	case ip.Is4():
		p = netip.PrefixFrom(ip, 32)
	case ip.Is6():
		p, _ = ip.Prefix(modelAuthIPv6Bits)
	}
	return failKey{prefix: p, hash: hashPrefix}
}

type failWindow struct {
	key   failKey
	start time.Time
	count int
}

// failLimiter is the fixed-window failure lock with the normative semantics
// of design/01 §4.2 — deliberately NOT the ipLimiter policy (deny new keys
// on a full map, auth_sso.go): blocked is true only for an existing window
// that runs and reached the limit, so an unknown key and a full map never
// block; fail on a full map evicts the least recently touched window.
// Every operation is O(1) (map + container/list); there is no prune scan
// under the mutex every /v1 request passes.
type failLimiter struct {
	mu  sync.Mutex
	cap int
	now func() time.Time
	lru *list.List // front = most recently failed; values *failWindow
	idx map[failKey]*list.Element
}

func newFailLimiter(capacity int, now func() time.Time) *failLimiter {
	return &failLimiter{cap: capacity, now: now, lru: list.New(), idx: make(map[failKey]*list.Element, capacity)}
}

// blocked reports whether k is locked and, if so, how long the window still
// runs. An expired window is dropped on the way.
func (l *failLimiter) blocked(k failKey, limit int, window time.Duration) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.idx[k]
	if !ok {
		return false, 0
	}
	w := el.Value.(*failWindow)
	left := w.start.Add(window).Sub(l.now())
	if left <= 0 {
		l.lru.Remove(el)
		delete(l.idx, k)
		return false, 0
	}
	return w.count >= limit, left
}

// fail counts one failed check of k.
func (l *failLimiter) fail(k failKey, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if el, ok := l.idx[k]; ok {
		w := el.Value.(*failWindow)
		if now.Sub(w.start) >= window {
			w.start, w.count = now, 0
		}
		w.count++
		l.lru.MoveToFront(el)
		return
	}
	if l.lru.Len() >= l.cap {
		oldest := l.lru.Back()
		l.lru.Remove(oldest)
		delete(l.idx, oldest.Value.(*failWindow).key)
	}
	l.idx[k] = l.lru.PushFront(&failWindow{key: k, start: now, count: 1})
}

// knownGoodLRU holds the hashes of keys whose last check succeeded. It is NOT
// an auth cache (E-01-9): membership only exempts a request from the probe
// token, every request still runs ctx_model_auth. A hash whose check fails
// leaves the set at once, so a revoked key cannot keep bypassing the bucket.
type knownGoodLRU struct {
	mu  sync.Mutex
	cap int
	lru *list.List // values string (hash)
	idx map[string]*list.Element
}

func newKnownGood(capacity int) *knownGoodLRU {
	return &knownGoodLRU{cap: capacity, lru: list.New(), idx: make(map[string]*list.Element, capacity)}
}

func (k *knownGoodLRU) contains(hash string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.idx[hash]
	return ok
}

func (k *knownGoodLRU) add(hash string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if el, ok := k.idx[hash]; ok {
		k.lru.MoveToFront(el)
		return
	}
	if k.lru.Len() >= k.cap {
		oldest := k.lru.Back()
		k.lru.Remove(oldest)
		delete(k.idx, oldest.Value.(string))
	}
	k.idx[hash] = k.lru.PushFront(hash)
}

func (k *knownGoodLRU) remove(hash string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if el, ok := k.idx[hash]; ok {
		k.lru.Remove(el)
		delete(k.idx, hash)
	}
}

// probeBucket is the global token bucket of stage 6 (burst = rate, refill =
// rate per second). The rate is read per request (hot); a lowered rate caps
// the stored tokens at once.
type probeBucket struct {
	mu      sync.Mutex
	now     func() time.Time
	tokens  float64
	last    time.Time
	started bool
}

func newProbeBucket(now func() time.Time) *probeBucket { return &probeBucket{now: now} }

func (b *probeBucket) take(rate int) bool {
	if rate <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now, burst := b.now(), float64(rate)
	if !b.started {
		b.tokens, b.started = burst, true
	} else if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.tokens += dt * burst
	}
	b.last = now
	if b.tokens > burst {
		b.tokens = burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// authSemaphore bounds the door's concurrent database checks (stage 7). The
// limit is read per acquire (hot); waiters are served FIFO, a raised limit
// admits waiters at once, a lowered one drains as holders release.
type authSemaphore struct {
	mu      sync.Mutex
	held    int
	limit   int
	waiters *list.List // values chan struct{}
}

func newAuthSemaphore() *authSemaphore { return &authSemaphore{waiters: list.New()} }

// acquire waits for a slot until ctx ends; the returned release must be
// called exactly once.
func (s *authSemaphore) acquire(ctx context.Context, limit int) (func(), error) {
	s.mu.Lock()
	s.limit = limit
	ch := make(chan struct{})
	el := s.waiters.PushBack(ch)
	s.grantLocked()
	s.mu.Unlock()
	select {
	case <-ch:
		return s.release, nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	select {
	case <-ch: // granted while the context ended: hand the slot on
		s.held--
		s.grantLocked()
	default:
		s.waiters.Remove(el)
	}
	s.mu.Unlock()
	return nil, ctx.Err()
}

func (s *authSemaphore) release() {
	s.mu.Lock()
	s.held--
	s.grantLocked()
	s.mu.Unlock()
}

func (s *authSemaphore) grantLocked() {
	for s.held < s.limit && s.waiters.Len() > 0 {
		front := s.waiters.Front()
		s.waiters.Remove(front)
		s.held++
		close(front.Value.(chan struct{}))
	}
}
