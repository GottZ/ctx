package dispatch

import (
	"fmt"
	"time"
)

// Test hooks of the A03-W4 gates (fairness_gates_test.go reaches them through
// interface assertions, so the gate file also runs against a tree without
// them). Precedent: ReapForTest / the per-instance warnAfter.

// setFairClockForTest installs the clock of the slot-time meter and the pick
// (fairClock) — the fake clock of the 30-min gates. Production keeps
// time.Now: the field is nil there.
func (d *Dispatcher) setFairClockForTest(now func() time.Time) {
	d.mu.Lock()
	d.fairNow = now
	for _, st := range d.targets {
		st.external.fair.viewAt = time.Time{} // the order's clock follows the new clock from the next event
	}
	d.mu.Unlock()
}

// fairAuditForTest recomputes the fairness bookkeeping of one target from the
// lease register (st.inflight) and the external queue under the mutex and
// returns the first difference ("" when none) and the number of fairness
// records left. Recomputed: heldBy (Σw of the held external leases) per
// consumer identity and per tenant, each consumer's activity (waiters +
// leases), its record binding (lease.fair), each tenant's active consumers
// and Σ fair_weight over them, the active tenant count, and the head-weight
// minimum of the queue.
func (d *Dispatcher) fairAuditForTest(origin string) (string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targets[canonicalOrigin(origin)]
	if st == nil {
		return "", 0
	}
	q, f := st.external, st.external.fair
	type agg struct {
		held, refs int
		tenant     string
	}
	cons := map[string]*agg{}
	get := func(key, tenant string) *agg {
		a, ok := cons[key]
		if !ok {
			a = &agg{tenant: tenant}
			cons[key] = a
		}
		return a
	}
	tenHeld := map[string]int{}
	for l := range st.inflight {
		if l.class != ClassExternal || !l.slotHeld {
			continue
		}
		key := l.principal.identityKey()
		a := get(key, l.principal.TenantID)
		a.held += l.weight
		a.refs++
		tenHeld[l.principal.TenantID] += l.weight
		if c := f.consumers[key]; c == nil || l.fair != c {
			return fmt.Sprintf("lease of %s not bound to its record", key), 0
		}
	}
	headMin := 0
	for key, b := range q.buckets {
		a := get(key, b[0].principal.TenantID)
		a.refs += len(b)
		if headMin == 0 || b[0].weight < headMin {
			headMin = b[0].weight
		}
	}
	for _, w := range q.cont {
		get(w.principal.identityKey(), w.principal.TenantID).refs++
	}
	if len(q.cont) > 0 {
		headMin = q.cont[0].weight // the class head (A03-W17)
	}
	for key, a := range cons {
		c := f.consumers[key]
		switch {
		case c == nil:
			return fmt.Sprintf("heldBy[%s]: no record (recomputed held %d, refs %d)", key, a.held, a.refs), 0
		case c.held != a.held:
			return fmt.Sprintf("heldBy[%s] = %d, recomputed %d", key, c.held, a.held), 0
		case c.refs != a.refs:
			return fmt.Sprintf("activity[%s] = %d, recomputed %d", key, c.refs, a.refs), 0
		case c.tenant != f.tenants[a.tenant]:
			return fmt.Sprintf("record %s bound to tenant %q, want %q", key, c.tenant.id, a.tenant), 0
		}
	}
	active := map[string]int{}
	sumFW := map[string]int{}
	for key, c := range f.consumers {
		if cons[key] == nil && (c.refs != 0 || c.held != 0) {
			return fmt.Sprintf("record %s: held %d refs %d without lease or waiter", key, c.held, c.refs), 0
		}
		if c.refs > 0 {
			active[c.tenant.id]++
			sumFW[c.tenant.id] += c.fw
		}
	}
	for id, t := range f.tenants {
		switch {
		case t.held != tenHeld[id]:
			return fmt.Sprintf("heldBy[_tenant:%s] = %d, recomputed %d", id, t.held, tenHeld[id]), 0
		case t.consumers != active[id]:
			return fmt.Sprintf("tenant %s active consumers = %d, recomputed %d", id, t.consumers, active[id]), 0
		case t.sumFW != sumFW[id]:
			return fmt.Sprintf("tenant %s Σ fair_weight = %d, recomputed %d", id, t.sumFW, sumFW[id]), 0
		}
	}
	for id, h := range tenHeld {
		if f.tenants[id] == nil {
			return fmt.Sprintf("heldBy[_tenant:%s] = %d without a record", id, h), 0
		}
	}
	if f.activeTenants != len(active) {
		return fmt.Sprintf("active tenants = %d, recomputed %d", f.activeTenants, len(active)), 0
	}
	if got := q.headMinWeight(); got != headMin {
		return fmt.Sprintf("head-weight minimum = %d, recomputed %d", got, headMin), 0
	}
	return "", len(f.consumers) + len(f.tenants)
}
