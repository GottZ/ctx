package dispatch

// sectionsOf is the number of dispatcher mutex sections (Snapshot.OpsTotal)
// that f's calls on d take, with d's loops stopped first so that no reaper
// or publication pass adds a section while f runs (d.Close; Acquire,
// TryAcquire and Release keep working on a closed dispatcher).
//
// It is the measure of a refusal "at once": a predicted refusal is the
// arrival's own section — the push, the prediction and the removal
// (predictAfterPushLocked), or a budget spent before the enqueue
// (waitBudgetLocked) — and returns before any wait; a refusal its timer
// ended takes a second section (expireWaiter), and so does an admission a
// release handed over (the wait a caller's ctx ended ends with the ctx's
// error, not ErrWaitExpired). The tests pinned "at once" with a wall-clock
// bound of 50 ms on the same call, which a loaded host's scheduler moves
// (issue #369); the section count it cannot move.
func sectionsOf(d *Dispatcher, f func()) int64 {
	d.Close()
	before := d.Snapshot().OpsTotal
	f()
	return d.Snapshot().OpsTotal - before
}
