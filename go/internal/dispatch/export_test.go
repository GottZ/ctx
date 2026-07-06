package dispatch

import "time"

// ReapForTest exposes one reaper pass to the external K1 wire test (the
// production cadence is the lazy reapLoop ticker).
func (d *Dispatcher) ReapForTest(now time.Time) { d.reapNow(now) }

// SeedWaitSampleForTest stages one admitted wait sample into a target × class
// wait window (MW7 ring) — lets the RetryAfterHint gate pin the estimate on a
// known snapshot without racing a live queue. origin is normalized like a real
// acquire; unparseable origins fall back to the raw key.
func (d *Dispatcher) SeedWaitSampleForTest(origin string, class Class, wait time.Duration) {
	norm, err := NormalizeOrigin(origin)
	if err != nil {
		norm = origin
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targetLocked(norm)
	st.waits[classIdx(class)].add(wait)
}

// SeedInteractiveDepthForTest raises the interactive queue depth counter of a
// target to n (the peers-ahead factor of the B1 estimate) without staging real
// waiters — the depth is all RetryAfterHint reads.
func (d *Dispatcher) SeedInteractiveDepthForTest(origin string, n int) {
	norm, err := NormalizeOrigin(origin)
	if err != nil {
		norm = origin
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targetLocked(norm)
	st.interactive.total = n
}
