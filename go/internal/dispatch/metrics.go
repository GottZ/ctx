package dispatch

// MW7 (D3-W3, design/03 §4.6.1): wait-time measurands per target × class.
// The starvation decision (D3-U2 / D3-W6 gate) is a DATA decision — it needs
// max_wait and wait-p95 per target × class from day 1, without new
// persistence. A counter pair cannot yield a p95 (review lens 2), so the
// estimator is pinned: a ring buffer over the last waitRingK admitted wait
// samples per target × class — O(1) insert in the hot path (one array store
// under the mutex admitLocked already holds), all quantile work deferred to
// the snapshot pull. int64 nanoseconds ⇒ 4 KiB per class, numClasses × 4 KiB
// per targetState: memory is irrelevant even across many targets.
//
// Publication (A03-W17, masterplan K21, design/02 §9.2 A03-9): the
// Retry-After hint is on the /v1 hot path — every 429 of an external request
// asks for it — so it may not sort the window under the dispatcher mutex.
// The publisher goroutine (publishLoop, started by New, ended by Close)
// re-reads every window that took samples since its last pass at most once
// per waitPublishInterval: it copies the window under the mutex, sorts the
// copy OUTSIDE it and stores p95/max/samples atomically (waitRing.pub). The
// hint reads only that published value. Snapshot keeps computing exactly
// (statsInto, the server-admin pull).
//
// Lease durations (A03-W6, design/03 §4.9): the same fixed window type holds,
// per target × class, the last waitRingK lease durations (finish − admitted,
// one sample per lease end in finishLocked); the same publication pass
// publishes their p50/p90 next to the wait aggregates — for the external
// classes only (A03-W9 lead decision P2, A03-W6 review 1 F-8: publish only
// what is read; the in-house windows keep sampling until a reader exists).
// The prediction, ŝ and the relational cap of each external class read only
// its own published value — never a sort under the dispatcher mutex. design/03 §4.9 recomputes "every
// 32 samples"; the publication of A03-W17 (K21: at most once per
// waitPublishInterval, outside the mutex) replaces that cadence.

import (
	"slices"
	"sync/atomic"
	"time"
)

// waitRingK is the rolling window size per target × class (design/03 §4.6.1).
const waitRingK = 512

// waitPublishInterval is the staleness bound of the published wait aggregates
// (design/02 §9.2 A03-9: "at most once per second, outside the mutex"): a
// mechanism constant, not a tuning value. A window republishes at most once
// per interval, and a published value is at most one interval (plus the pass
// itself) older than the window it summarizes.
const waitPublishInterval = time.Second

// waitRing is the fixed rolling window over the last waitRingK admitted wait
// samples of one target × class (guarded by Dispatcher.mu like every other
// targetState field). Sample waitRingK+1 displaces sample 1. Every admission
// samples — immediate and pass-through admissions record their ~0 wait, so
// the window is the true per-acquire distribution, not just the queued
// subset (the D3-W6 before/after comparison must see admissions that never
// waited). The external window also holds CENSORED samples (A03-W5,
// expireWaiter): an external waiter whose wait budget ran out adds the time
// it waited — without them the external p95 would measure only the survivors.
type waitRing struct {
	// samples holds waits in nanoseconds (admitted − enqueued).
	samples [waitRingK]int64
	// pos is the next write index (wraps).
	pos int
	// count is the number of filled entries, ≤ waitRingK.
	count int
	// gen counts the samples ever added and copied is gen at the last
	// publication copy — a window republishes only when they differ.
	gen, copied uint64
	// pub is the last published aggregate (nil before the first
	// publication) — written by the publication pass, read lock-free.
	pub atomic.Pointer[waitAgg]
}

// waitAgg is one published aggregate of a window: the wait aggregates p95 and
// max, and the lease-duration aggregates p50 and p90 (A03-W6) — one sort
// yields all four, each window reads the ones it means.
type waitAgg struct {
	p50, p90, p95, max time.Duration
	samples            int
}

// add records one admitted wait — O(1), hot path (admitLocked). A nil ring
// (targetState.ring of a class outside the class table) is a no-op.
func (r *waitRing) add(wait time.Duration) {
	if r == nil {
		return
	}
	r.samples[r.pos] = int64(wait)
	r.pos = (r.pos + 1) % waitRingK
	if r.count < waitRingK {
		r.count++
	}
	r.gen++
}

// statsInto fills the window aggregates of one class view: p95 (nearest-rank
// over the window), max and the SAMPLE COUNT the aggregates were computed
// from — window honesty per design/03 §4.6.1: the D3-W6 before/after gate
// needs the window size next to the quantile so comparisons are not
// contaminated by a since-boot mixture. Sorting the ≤ waitRingK copy happens
// ONLY on the snapshot pull, never in the admission path. A nil ring leaves
// ws untouched (no window, no samples).
func (r *waitRing) statsInto(ws *WaitStats) {
	if r == nil {
		return
	}
	ws.Samples = r.count
	if r.count == 0 {
		return
	}
	buf := make([]int64, r.count)
	copy(buf, r.samples[:r.count])
	a := aggregate(buf)
	ws.MaxWait, ws.P95Wait = a.max, a.p95
}

// aggregate sorts buf in place and returns its window aggregates — the ONE
// formula of the exact snapshot and of the publication: max and the
// nearest-rank quantiles, the ⌈q·n⌉-th smallest sample.
func aggregate(buf []int64) waitAgg {
	n := len(buf)
	if n == 0 {
		return waitAgg{}
	}
	slices.Sort(buf)
	return waitAgg{
		p50: nearestRank(buf, 50), p90: nearestRank(buf, 90), p95: nearestRank(buf, 95),
		max: time.Duration(buf[n-1]), samples: n,
	}
}

// nearestRank is the nearest-rank pct-th percentile of the sorted, non-empty
// buf: the ⌈pct·n/100⌉-th smallest sample.
func nearestRank(buf []int64, pct int) time.Duration {
	return time.Duration(buf[(len(buf)*pct+99)/100-1])
}

// leaseStatsInto fills the lease-duration view of one class (A03-W6): p50,
// p90 and the sample count, exact like statsInto (the server-admin pull, never
// the admission path). A nil ring leaves ls untouched.
func (r *waitRing) leaseStatsInto(ls *LeaseDurStats) {
	if r == nil {
		return
	}
	ls.Samples = r.count
	if r.count == 0 {
		return
	}
	buf := make([]int64, r.count)
	copy(buf, r.samples[:r.count])
	a := aggregate(buf)
	ls.P50, ls.P90 = a.p50, a.p90
}

// published returns the last published aggregate of the window — zero before
// the first publication and for a nil ring. Lock-free, allocation-free.
func (r *waitRing) published() waitAgg {
	if r == nil {
		return waitAgg{}
	}
	if a := r.pub.Load(); a != nil {
		return *a
	}
	return waitAgg{}
}

// publishLoop runs a publication pass every waitPublishInterval until Close.
func (d *Dispatcher) publishLoop() {
	defer d.loops.Done()
	t := time.NewTicker(waitPublishInterval)
	defer t.Stop()
	for {
		select {
		case <-d.stopLoops:
			return
		case <-t.C:
			d.publishWaits()
		}
	}
}

// publishWaits is one publication pass: under the mutex it collects the
// windows — wait and lease-duration windows alike — that took samples since
// their last copy, then per window it copies
// the samples under the mutex (one short measured section each), sorts the
// copy outside it and stores the aggregate atomically. Passes are serialized
// (pubMu), so a published value never goes back in time.
func (d *Dispatcher) publishWaits() {
	d.pubMu.Lock()
	defer d.pubMu.Unlock()
	if d.pubBuf == nil {
		d.pubBuf = make([]int64, waitRingK)
	}
	d.mu.Lock()
	opStart := time.Now()
	rings := d.pubRings[:0]
	for _, st := range d.targets {
		for c := range st.waits {
			if r := &st.waits[c]; r.gen != r.copied {
				rings = append(rings, r)
			}
			if r := &st.leases[c]; Class(c).External() && r.gen != r.copied {
				rings = append(rings, r)
			}
		}
	}
	d.opDone(opStart)
	d.mu.Unlock()
	for _, r := range rings {
		d.mu.Lock()
		opStart = time.Now()
		n := copy(d.pubBuf, r.samples[:r.count])
		r.copied = r.gen
		d.opDone(opStart)
		d.mu.Unlock()
		a := aggregate(d.pubBuf[:n])
		r.pub.Store(&a)
	}
	clear(rings)
	d.pubRings = rings[:0]
}

// ring returns class c's wait window — the ONLY index into targetState.waits
// (one window per class below numClasses). It is nil for a value without a
// named class-table row, and both window methods are nil-safe no-ops, so no
// caller can form a negative or out-of-range index, and none can panic while
// holding d.mu (A03-W1 review 2 F2: the former -1 index sentinel made every
// unguarded caller panic, and a panic under d.mu without defer leaves the
// mutex locked — a dispatcher-wide deadlock). An out-of-table value is never
// folded into another class's window either, as the pre-A03-W1 classIdx did
// onto background's ring (K9).
func (st *targetState) ring(c Class) *waitRing {
	if !c.valid() {
		return nil
	}
	return &st.waits[c]
}

// leaseRing returns class c's lease-duration window (A03-W6) — the only index
// into targetState.leases, nil outside the class table like ring.
func (st *targetState) leaseRing(c Class) *waitRing {
	if !c.valid() {
		return nil
	}
	return &st.leases[c]
}
