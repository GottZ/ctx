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
	// err is the waiter's error channel (A03-W3, design/03 §4.3): set under
	// Dispatcher.mu BEFORE admitCh closes when the waiter ends WITHOUT an
	// admission — an external waiter whose target stopped offering the
	// external class gets ErrExternalNotOffered instead of a pass-through
	// handoff (X9). nil on every admission.
	err error
	// weight is the number of slot units the lease will hold (A03-W7,
	// requestWeight): Request.Weight for the external class, always 1 for
	// the in-house classes.
	weight int
	// cont marks a continuation (Request.Continuation of an external
	// request, A03-W17): it waits in classQueue.cont, the head of the class,
	// never in a bucket. Always false in-house.
	cont bool
	// fair is the consumer's fairness record while the waiter is queued in
	// the external queue (A03-W4, extFair.waiterEnter) — the pick reads it
	// without a map lookup; nil in-house.
	fair *fairConsumer
	// fwBefore is the fair weight of the consumer's record before this
	// waiter's push when the record was active then (0: it was not): a
	// refusal predicted after the push writes it back (A03-W6 review 4
	// FR4-2, predictAfterPushLocked).
	fwBefore int
	// passBase is the weight its external class had admitted to other
	// consumers at its push (extFair.others, A03-W8a): the head has aged
	// once that count grew by externalAgingTurnovers · E_eff since. 0
	// in-house.
	passBase uint64
	// pushAt is its push on the view clock and agingDur externalAgingRounds
	// · L_p50 of its class read at the push when the class's lease window
	// was warm, else 0 (enqueueExternalLocked; the time threshold of the
	// aging rule, A03-W8a).
	pushAt   time.Time
	agingDur time.Duration
}

// classQueue is a per-principal wait queue of one target: per-bucket FIFOs
// plus a ring (E-F2, built in MW1 so the fairness activation MW23 is a pure
// pick-order flip behind a data switch — no structural wave). The
// interactive queue has two count dimensions with two DIFFERENT keys
// (design/04 §4.2): the bucket order runs per fairKey (HomeScope), the
// Deckel-Staffel counts per ApiKeyID and TenantID — buckets and caps share
// the waiter DATA, not the key. The external queue (A03-W3, design/03 §4.1
// C7 (5)) keys bucket AND cap on the consumer identity _consumer:<id>
// (identityKey) and counts its own tenant dimension: two consumers are two
// buckets with two caps, never one "" bucket.
type classQueue struct {
	// external selects the identity keys of an external queue; class is its
	// external class (XU or XB, A03-W9 — the index of the per-class waiting
	// counters in the shared fairness records), the zero value on the
	// interactive queue.
	external bool
	class    Class
	buckets  map[string][]*waiter
	// ring holds bucket keys in join order; cursor is the next principal_rr
	// pick (pattern: scheduler tenantCursor). Only active buckets stay in
	// map+ring (eviction pattern of the webchat semaphore).
	ring   []string
	cursor int
	// byKey / byTenant / total are the Deckel-Staffel counters (waiting
	// acquires per cap key — ApiKeyID, or the consumer identity in the
	// external queue — / per TenantID / per target); weight is the sum of
	// the waiting weights (A03-W9: the units a lower-ranked external class
	// waits behind, prediction.go aheadLocked).
	byKey    map[string]int
	byTenant map[string]int
	total    int
	weight   int
	// maxWeight is an upper bound of the waiting weights (A03-W7): raised on
	// push, reset when the queue empties, recomputed by the overweight drain
	// — the O(1) guard that keeps the drain off the release path while every
	// waiter fits.
	maxWeight int
	// cont is the continuation FIFO of the external queue (A03-W17, masterplan
	// K21): the HEAD of the class. Invariant for every bucket pick (the
	// fairness pick of A03-W4 included): each view of the class head
	// (peekFIFO) and each dequeue (pick) consults cont BEFORE any bucket,
	// under every fairness mode — a continuation never waits for its
	// bucket's turn. A continuation is counted like every waiter (total,
	// byKey, byTenant, maxWeight: the depth ahead of a newcomer includes it)
	// but never checked against a cap (enqueueLocked). Empty in the
	// interactive queue.
	cont []*waiter
	// fair is the fairness bookkeeping of the external queue (A03-W4,
	// fairness.go; nil on the interactive queue); headMin is the smallest
	// bucket-head weight, recomputed lazily once headMinStale (headMinWeight).
	fair         *extFair
	headMin      int
	headMinStale bool
}

func newClassQueue() *classQueue {
	return &classQueue{
		buckets:  make(map[string][]*waiter),
		byKey:    make(map[string]int),
		byTenant: make(map[string]int),
	}
}

// newExternalQueue builds the wait queue of the external class c of one
// target on the target's fairness records fair — ONE record set for both
// external classes (A03-W9, design/03 §4.5 no. 2: T_act, heldBy and Σ
// fair_weight count XU and XB together; A03-W4 handover).
func newExternalQueue(c Class, fair *extFair) *classQueue {
	q := newClassQueue()
	q.external = true
	q.class = c
	q.fair = fair
	return q
}

// bucketKey is the bucket of p: fairKey in the interactive queue, the
// consumer identity in the external one.
func (q *classQueue) bucketKey(p Principal) string {
	if q.external {
		return p.identityKey()
	}
	return p.fairKey()
}

// capKey is the per-principal cap counter key of p: ApiKeyID in the
// interactive queue (B9), the consumer identity in the external one.
func (q *classQueue) capKey(p Principal) string {
	if q.external {
		return p.identityKey()
	}
	return p.ApiKeyID
}

// push enqueues w into its bucket and bumps the cap counters.
func (q *classQueue) push(w *waiter) {
	key := q.bucketKey(w.principal)
	if _, ok := q.buckets[key]; !ok {
		q.ring = append(q.ring, key)
		q.headJoined(w)
	}
	q.buckets[key] = append(q.buckets[key], w)
	if q.fair != nil {
		q.fair.waiterEnter(key, w)
	}
	q.count(w)
}

// pushCont enqueues a continuation at the class head (behind the waiting
// continuations, before every bucket) and counts it like any waiter.
func (q *classQueue) pushCont(w *waiter) {
	q.cont = append(q.cont, w)
	if q.fair != nil {
		q.fair.waiterEnter(q.bucketKey(w.principal), w)
	}
	q.count(w)
}

// count bumps the cap counters and the weight bound for one queued waiter.
func (q *classQueue) count(w *waiter) {
	if w.weight > q.maxWeight {
		q.maxWeight = w.weight
	}
	q.byKey[q.capKey(w.principal)]++
	if w.principal.TenantID != "" {
		q.byTenant[w.principal.TenantID]++
	}
	q.total++
	q.weight += w.weight
}

// pick dequeues the next waiter under the given fairness mode, or nil. A
// waiting continuation comes first under every mode (A03-W17, cont). Then
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
	if len(q.cont) > 0 {
		return q.popCont()
	}
	if mode == FairnessPrincipalRR && len(q.ring) > 0 {
		q.cursor %= len(q.ring)
		key := q.ring[q.cursor]
		q.cursor++
		return q.popHead(key)
	}
	// fifo: global arrival order via the monotone per-target seq.
	best := q.fifoHeadKey()
	if best == "" {
		return nil
	}
	return q.popHead(best)
}

// fifoHeadKey is the bucket whose head has the smallest seq ("" when
// empty): the bucket pick(FairnessFIFO) dequeues from.
func (q *classQueue) fifoHeadKey() string {
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
	return best
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
	q.headLeft()
	if q.fair != nil {
		q.fair.waiterLeave(key, w)
	}
	return w
}

// popCont dequeues the oldest continuation.
func (q *classQueue) popCont() *waiter {
	w := q.cont[0]
	q.cont[0] = nil // no stale reference in the backing array
	q.cont = q.cont[1:]
	q.uncount(w)
	if q.fair != nil {
		q.fair.waiterLeave(q.bucketKey(w.principal), w)
	}
	return w
}

// remove splices one canceled waiter out of its bucket — a continuation out
// of cont (caller-ctx cancel vacates the wait slot — design/01 §4.2). Depth
// is Deckel-bounded, so the linear scan is O(≤64).
func (q *classQueue) remove(w *waiter) {
	if w.cont {
		for i, cand := range q.cont {
			if cand == w {
				// Shift in place and clear the vacated tail slot, so the
				// backing array keeps no reference to the canceled waiter
				// (the same hygiene as popCont).
				last := len(q.cont) - 1
				copy(q.cont[i:], q.cont[i+1:])
				q.cont[last] = nil
				q.cont = q.cont[:last]
				q.uncount(w)
				if q.fair != nil {
					q.fair.waiterLeave(q.bucketKey(w.principal), w)
				}
				return
			}
		}
		return
	}
	key := q.bucketKey(w.principal)
	b := q.buckets[key]
	for i, cand := range b {
		if cand != w {
			continue
		}
		// Shift in place and clear the vacated tail slot, the hygiene of
		// the cont path: a refusal predicted after its push (A03-W6) leaves
		// the newest waiter of a bucket, and the backing array must keep no
		// reference to it until the next push into that bucket.
		last := len(b) - 1
		copy(b[i:], b[i+1:])
		b[last] = nil
		b = b[:last]
		if len(b) == 0 {
			delete(q.buckets, key)
			q.dropRingKey(key)
		} else {
			q.buckets[key] = b
		}
		q.uncount(w)
		if i == 0 {
			q.headLeft()
		}
		if q.fair != nil {
			q.fair.waiterLeave(key, w)
		}
		return
	}
}

func (q *classQueue) uncount(w *waiter) {
	ck := q.capKey(w.principal)
	q.byKey[ck]--
	if q.byKey[ck] <= 0 {
		delete(q.byKey, ck)
	}
	if w.principal.TenantID != "" {
		q.byTenant[w.principal.TenantID]--
		if q.byTenant[w.principal.TenantID] <= 0 {
			delete(q.byTenant, w.principal.TenantID)
		}
	}
	q.total--
	q.weight -= w.weight
	if q.total == 0 {
		q.maxWeight = 0
	}
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
	q.each(func(w *waiter) {
		if t.IsZero() || w.enqueued.Before(t) {
			t = w.enqueued
		}
	})
	return t
}

// each visits every waiter, the continuations included (WARN scan of the
// reaper, overweight drain).
func (q *classQueue) each(fn func(*waiter)) {
	for _, w := range q.cont {
		fn(w)
	}
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
