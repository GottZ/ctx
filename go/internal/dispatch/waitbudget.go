package dispatch

import "time"

// The wait budget of the external classes (design/03 §4.9, wave A03-W5;
// masterplan K6: ONE wait budget, in the dispatcher — saturation is a 429,
// never a hang). An external acquire that cannot be admitted at once waits at
// most MaxWait_eff; when that runs out it ends with ErrWaitExpired, a
// rejection. The in-house classes have no wait budget (V3): their wait ends
// with an admission or with the caller's own ctx, as before.

// serveMargin is the serve margin m of the wait budget (design/03 §3.3, a
// mechanism constant, not a tuning value): the network round trip and answer
// path between the end of a lease and the requester's ServeBy (precedent:
// design/04 hopMargin).
const serveMargin = 500 * time.Millisecond

// externalWaitBudget is MaxWait_eff of an external acquire that would have to
// wait (design/03 §4.9): the minimum of the setting, Request.MaxWait when > 0
// and ServeBy − now − ŝ − serveMargin when ServeBy is set. Request values can
// only shorten the setting (I-03-5). ŝ, the p90 lease duration of the target ×
// class, is 0 until the lease-duration ring exists (A03-W6; the cold-ring rule
// of §4.9). A setting ≤ 0 is a budget of 0: the external class never waits
// (fail-closed, never "unbounded").
func externalWaitBudget(set Settings, req Request, now time.Time) time.Duration {
	budget := max(set.ExternalMaxWait, 0)
	if req.MaxWait > 0 && req.MaxWait < budget {
		budget = req.MaxWait
	}
	if !req.ServeBy.IsZero() {
		budget = min(budget, req.ServeBy.Sub(now)-serveMargin)
	}
	return budget
}

// waitBudgetLocked resolves the wait budget of an acquire that has to wait —
// called after the immediate admission failed and BEFORE the enqueue: 0 for
// the in-house classes (no budget, no timer), MaxWait_eff (> 0) for an
// external class, or ErrWaitExpired when that budget is already ≤ 0 (reason
// predicted, counted; the acquire is never queued and never touches the cap
// ladder).
func (d *Dispatcher) waitBudgetLocked(st *targetState, class Class, req Request, now time.Time) (time.Duration, error) {
	if !class.External() {
		return 0, nil
	}
	budget := externalWaitBudget(d.currentSettings(), req, now)
	if budget <= 0 {
		st.extRejects.Predicted++
		return 0, ErrWaitExpired
	}
	return budget, nil
}

// waitTimer arms the wait-budget timer of one queued acquire: for budget > 0
// the channel fires when the budget, counted from the acquire's arrival
// (the instant its ServeBy term and its censored sample are measured from),
// runs out — time spent on the dispatcher mutex counts against it; a budget
// already used up by then fires at once. Without a budget (the in-house
// classes) the channel is nil, so its select case never fires. stop is the
// timer stop of every return path of the wait (Acquire defers it).
func waitTimer(budget time.Duration, arrived time.Time) (<-chan time.Time, func()) {
	if budget <= 0 {
		return nil, func() {}
	}
	t := time.NewTimer(max(budget-time.Since(arrived), 0))
	return t.C, func() { t.Stop() }
}

// expireWaiter is the timer branch of Acquire's wait (design/03 §4.9). Under
// the mutex the dispatcher may have ended the wait first: a direct handoff
// that won the race (w.admitted) returns its lease — the slot is already this
// waiter's, the wait ended in an admission (already sampled by admitLocked) —
// and a waiter the dispatcher ended without an admission (w.err, a withdrawn
// offer) returns that error, counted once where it was set. Otherwise the
// waiter leaves its queue (dropWaiterLocked corrects byKey/byTenant/total),
// its waited time goes into its class's wait window as a CENSORED sample
// (else the external p95 measures only the survivors), Expired counts it, and
// the answer is ErrWaitExpired.
func (d *Dispatcher) expireWaiter(st *targetState, w *waiter) (*Lease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if w.admitted {
		return w.lease, nil
	}
	if w.err != nil {
		return nil, w.err
	}
	d.dropWaiterLocked(st, w)
	st.ring(w.class).add(time.Since(w.enqueued))
	st.extRejects.Expired++
	return nil, ErrWaitExpired
}
