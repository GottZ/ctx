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

import (
	"slices"
	"time"
)

// waitRingK is the rolling window size per target × class (design/03 §4.6.1).
const waitRingK = 512

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
	slices.Sort(buf)
	ws.MaxWait = time.Duration(buf[r.count-1])
	// Nearest-rank p95: the ⌈0.95·n⌉-th smallest sample of the window.
	ws.P95Wait = time.Duration(buf[(r.count*95+99)/100-1])
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
