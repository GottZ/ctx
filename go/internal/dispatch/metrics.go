package dispatch

// MW7 (D3-W3, design/03 §4.6.1): wait-time measurands per target × class.
// The starvation decision (D3-U2 / D3-W6 gate) is a DATA decision — it needs
// max_wait and wait-p95 per target × class from day 1, without new
// persistence. A counter pair cannot yield a p95 (review lens 2), so the
// estimator is pinned: a ring buffer over the last waitRingK admitted wait
// samples per target × class — O(1) insert in the hot path (one array store
// under the mutex admitLocked already holds), all quantile work deferred to
// the snapshot pull. int64 nanoseconds ⇒ 4 KiB per class, 8 KiB per
// targetState: memory is irrelevant even across many targets.

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
// waited).
type waitRing struct {
	// samples holds waits in nanoseconds (admitted − enqueued).
	samples [waitRingK]int64
	// pos is the next write index (wraps).
	pos int
	// count is the number of filled entries, ≤ waitRingK.
	count int
}

// add records one admitted wait — O(1), hot path (admitLocked).
func (r *waitRing) add(wait time.Duration) {
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
// ONLY on the snapshot pull, never in the admission path.
func (r *waitRing) statsInto(ws *WaitStats) {
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

// classIdx maps a class onto its ring slot. Everything that is not
// interactive counts as background — the same defensive reading the queue
// side uses (an out-of-range Class value must never panic the meter).
func classIdx(c Class) int {
	if c == ClassInteractive {
		return 0
	}
	return 1
}
