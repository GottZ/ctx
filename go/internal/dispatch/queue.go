package dispatch

import (
	"context"
	"time"
)

// waiter is one enqueued acquire waiting for admission. All fields are
// guarded by Dispatcher.mu except admitCh (closed exactly once under mu; the
// waiting goroutine only receives).
type waiter struct {
	seq       uint64
	class     Class
	principal Principal
	role      string
	enqueued  time.Time
	// reapRef is the reap reference captured at Acquire: the EARLIER of the
	// deadline hint and the ctx deadline; zero = lease_max_age fallback.
	reapRef time.Time
	// deadlineIn is the admission-anchored hint (Request.DeadlineIn); it
	// resolves to admitted+deadlineIn in admitLocked (rule V1).
	deadlineIn time.Duration
	runCtx     context.Context
	cancel     context.CancelCauseFunc
	admitCh    chan struct{}
	// lease is set by the admitting side (direct handoff) before admitCh
	// closes; the waiting goroutine reads it after <-admitCh.
	lease    *Lease
	admitted bool
	// warned dedupes the >5s interactive wait WARN across reaper ticks.
	warned bool
	// aged marks a background waiter admitted via the aging escape (MW25,
	// aging.go) — carried onto the lease so a later preempt of it is
	// distinguishable (the waste metric of the FA activation gate).
	aged bool
}

// classQueue is the interactive wait queue of one target: per-fairness-bucket
// FIFOs plus a ring (E-F2, built in MW1 so the fairness activation MW23 is a
// pure pick-order flip behind a data switch — no structural wave). Two count
// dimensions with two DIFFERENT keys (design/04 §4.2): the bucket order runs
// per fairKey (HomeScope), the Deckel-Staffel counts per ApiKeyID and
// TenantID — buckets and caps share the waiter DATA, not the key.
type classQueue struct {
	buckets map[string][]*waiter
	// ring holds bucket keys in join order; cursor is the next principal_rr
	// pick (pattern: scheduler tenantCursor). Only active buckets stay in
	// map+ring (eviction pattern of the webchat semaphore).
	ring   []string
	cursor int
	// byKey / byTenant / total are the Deckel-Staffel counters (waiting
	// acquires per ApiKeyID / per TenantID / per target).
	byKey    map[string]int
	byTenant map[string]int
	total    int
}

func newClassQueue() *classQueue {
	return &classQueue{
		buckets:  make(map[string][]*waiter),
		byKey:    make(map[string]int),
		byTenant: make(map[string]int),
	}
}

// push enqueues w into its fairness bucket and bumps the cap counters.
func (q *classQueue) push(w *waiter) {
	key := w.principal.fairKey()
	if _, ok := q.buckets[key]; !ok {
		q.ring = append(q.ring, key)
	}
	q.buckets[key] = append(q.buckets[key], w)
	q.byKey[w.principal.ApiKeyID]++
	if w.principal.TenantID != "" {
		q.byTenant[w.principal.TenantID]++
	}
	q.total++
}

// pick dequeues the next waiter under the given fairness mode, or nil.
// fifo: smallest seq over all bucket heads — byte-equal semantics to one
// global FIFO. principal_rr: head of the next non-empty bucket from cursor;
// INSIDE a bucket strictly FIFO (no intra-principal barging — the per-bucket
// reformulation of the no-barging probe). The queue depth is capped by the
// Deckel-Staffel (≤ interactive_queue_max), so the O(#buckets) fifo scan is
// bounded and irrelevant against wire latencies even at target scale.
func (q *classQueue) pick(mode FairnessMode) *waiter {
	if q.total == 0 {
		return nil
	}
	if mode == FairnessPrincipalRR && len(q.ring) > 0 {
		q.cursor %= len(q.ring)
		key := q.ring[q.cursor]
		q.cursor++
		return q.popHead(key)
	}
	// fifo: global arrival order via the monotone per-target seq.
	best := ""
	var bestSeq uint64
	for key, b := range q.buckets {
		if len(b) == 0 {
			continue
		}
		if best == "" || b[0].seq < bestSeq {
			best, bestSeq = key, b[0].seq
		}
	}
	if best == "" {
		return nil
	}
	return q.popHead(best)
}

// popHead removes and returns the head of one bucket, maintaining counters
// and evicting the bucket when it empties.
func (q *classQueue) popHead(key string) *waiter {
	b := q.buckets[key]
	w := b[0]
	b = b[1:]
	if len(b) == 0 {
		delete(q.buckets, key)
		q.dropRingKey(key)
	} else {
		q.buckets[key] = b
	}
	q.uncount(w)
	return w
}

// remove splices one canceled waiter out of its bucket (caller-ctx cancel
// vacates the wait slot — design/01 §4.2). Depth is Deckel-bounded, so the
// linear scan is O(≤64).
func (q *classQueue) remove(w *waiter) {
	key := w.principal.fairKey()
	b := q.buckets[key]
	for i, cand := range b {
		if cand != w {
			continue
		}
		b = append(b[:i], b[i+1:]...)
		if len(b) == 0 {
			delete(q.buckets, key)
			q.dropRingKey(key)
		} else {
			q.buckets[key] = b
		}
		q.uncount(w)
		return
	}
}

func (q *classQueue) uncount(w *waiter) {
	q.byKey[w.principal.ApiKeyID]--
	if q.byKey[w.principal.ApiKeyID] <= 0 {
		delete(q.byKey, w.principal.ApiKeyID)
	}
	if w.principal.TenantID != "" {
		q.byTenant[w.principal.TenantID]--
		if q.byTenant[w.principal.TenantID] <= 0 {
			delete(q.byTenant, w.principal.TenantID)
		}
	}
	q.total--
}

func (q *classQueue) dropRingKey(key string) {
	for i, k := range q.ring {
		if k != key {
			continue
		}
		q.ring = append(q.ring[:i], q.ring[i+1:]...)
		if q.cursor > i {
			q.cursor--
		}
		return
	}
}

// oldest returns the longest-waiting enqueue time, zero when empty.
func (q *classQueue) oldest() time.Time {
	var t time.Time
	for _, b := range q.buckets {
		for _, w := range b {
			if t.IsZero() || w.enqueued.Before(t) {
				t = w.enqueued
			}
		}
	}
	return t
}

// each visits every waiter (WARN scan of the reaper).
func (q *classQueue) each(fn func(*waiter)) {
	for _, b := range q.buckets {
		for _, w := range b {
			fn(w)
		}
	}
}

// bgQueue is the background wait queue of one target: a single FIFO —
// background consumers are the process's own scheduler arms, there are no
// competing principals inside the class (design/04 §4.2; the E-U2 outcome
// keeps the daily API path interactive-with-brake, so this holds).
type bgQueue struct {
	items []*waiter
}

func (q *bgQueue) push(w *waiter) { q.items = append(q.items, w) }

func (q *bgQueue) pop() *waiter {
	if len(q.items) == 0 {
		return nil
	}
	w := q.items[0]
	q.items = q.items[1:]
	return w
}

func (q *bgQueue) remove(w *waiter) {
	for i, cand := range q.items {
		if cand == w {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return
		}
	}
}

func (q *bgQueue) len() int { return len(q.items) }

func (q *bgQueue) oldest() time.Time {
	if len(q.items) == 0 {
		return time.Time{}
	}
	return q.items[0].enqueued
}
