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
// lease register (st.inflight) and both external queues (A03-W9: one record
// set for XU and XB) under the mutex and returns the first difference (""
// when none) and the number of fairness records left. Recomputed: heldBy
// (Σw of the held external leases of both classes) per consumer identity and
// per tenant, each consumer's activity (waiters + leases of both classes),
// its record binding (lease.fair), each tenant's active consumers and Σ
// fair_weight over them, the active tenant count, the head-weight minimum of
// each queue, the waiting weight of each queue, the waiting position
// counters of A03-W6 per external class (waiting and Σw per consumer,
// waiting consumers and their Σ fair_weight per tenant, the waiting tenant
// count), and the per-class held units of A03-W9 (classHeld against the
// register; heldExt and held as their sums, background as len(cancelable)).
func (d *Dispatcher) fairAuditForTest(origin string) (string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targets[canonicalOrigin(origin)]
	if st == nil {
		return "", 0
	}
	f := st.external.fair
	if st.externalBulk.fair != f {
		return "the external queues do not share one record set", 0
	}
	type agg struct {
		held, refs     int
		waiting, waitW [numClasses]int
		tenant         string
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
	var classHeld [numClasses]int
	for l := range st.inflight {
		if !l.slotHeld {
			continue
		}
		classHeld[l.class] += l.weight
		if !l.class.External() {
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
	if classHeld != st.classHeld {
		return fmt.Sprintf("classHeld = %v, recomputed %v", st.classHeld, classHeld), 0
	}
	sum := 0
	for _, w := range classHeld {
		sum += w
	}
	switch {
	case sum != st.held:
		return fmt.Sprintf("held = %d, recomputed Σ classHeld %d", st.held, sum), 0
	case classHeld[ClassExternal]+classHeld[ClassExternalBulk] != st.heldExt:
		return fmt.Sprintf("heldExt = %d, recomputed %d", st.heldExt, classHeld[ClassExternal]+classHeld[ClassExternalBulk]), 0
	case classHeld[ClassBackground] != len(st.cancelable):
		return fmt.Sprintf("background held %d, cancelable %d", classHeld[ClassBackground], len(st.cancelable)), 0
	}
	for _, q := range st.extQueues() {
		k := q.class
		headMin, weight := 0, 0
		for key, b := range q.buckets {
			a := get(key, b[0].principal.TenantID)
			a.refs += len(b)
			a.waiting[k] += len(b)
			for _, w := range b {
				a.waitW[k] += w.weight
				weight += w.weight
			}
			if headMin == 0 || b[0].weight < headMin {
				headMin = b[0].weight
			}
		}
		for _, w := range q.cont {
			a := get(w.principal.identityKey(), w.principal.TenantID)
			a.refs++
			a.waiting[k]++
			a.waitW[k] += w.weight
			weight += w.weight
		}
		if len(q.cont) > 0 {
			headMin = q.cont[0].weight // the class head (A03-W17)
		}
		if got := q.headMinWeight(); got != headMin {
			return fmt.Sprintf("%s head-weight minimum = %d, recomputed %d", k, got, headMin), 0
		}
		if q.weight != weight {
			return fmt.Sprintf("%s waiting weight = %d, recomputed %d", k, q.weight, weight), 0
		}
		// The aging count of every queued waiter (A03-W8a) never runs back:
		// the units its class admitted to other consumers since its push.
		for _, b := range q.buckets {
			for _, w := range b {
				if w.fair == nil || f.others(w.fair, k) < w.passBase {
					return fmt.Sprintf("%s waiter of %s: aging count ran back (base %d)", k, w.principal.identityKey(), w.passBase), 0
				}
			}
		}
	}
	var ownAdmitted [numClasses]uint64
	for _, c := range f.consumers {
		for k, n := range c.admitted {
			ownAdmitted[k] += n
		}
	}
	for k := range ownAdmitted {
		if ownAdmitted[k] > f.admitted[k] {
			return fmt.Sprintf("%s admitted %d, the records count %d", Class(k), f.admitted[k], ownAdmitted[k]), 0
		}
	}
	var waitingCons, waitFW [numClasses]map[string]int
	for k := range waitingCons {
		waitingCons[k], waitFW[k] = map[string]int{}, map[string]int{}
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
		case c.waiting != a.waiting || c.waitW != a.waitW:
			return fmt.Sprintf("waiting[%s] = %v (Σw %v), recomputed %v (Σw %v)", key, c.waiting, c.waitW, a.waiting, a.waitW), 0
		case c.tenant != f.tenants[a.tenant]:
			return fmt.Sprintf("record %s bound to tenant %q, want %q", key, c.tenant.id, a.tenant), 0
		}
	}
	active := map[string]int{}
	sumFW := map[string]int{}
	for key, c := range f.consumers {
		if cons[key] == nil && (c.refs != 0 || c.held != 0 || c.waiting != [numClasses]int{} || c.waitW != [numClasses]int{}) {
			return fmt.Sprintf("record %s: held %d refs %d waiting %v without lease or waiter", key, c.held, c.refs, c.waiting), 0
		}
		for k, n := range c.waiting {
			if n > 0 {
				waitingCons[k][c.tenant.id]++
				waitFW[k][c.tenant.id] += c.fw
			}
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
		for k := range t.waitingConsumers {
			if t.waitingConsumers[k] != waitingCons[k][id] || t.waitFW[k] != waitFW[k][id] {
				return fmt.Sprintf("tenant %s %s waiting consumers = %d (Σfw %d), recomputed %d (Σfw %d)",
					id, Class(k), t.waitingConsumers[k], t.waitFW[k], waitingCons[k][id], waitFW[k][id]), 0
			}
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
	for k := range f.waitingTenants {
		if f.waitingTenants[k] != len(waitingCons[k]) {
			return fmt.Sprintf("%s waiting tenants = %d, recomputed %d", Class(k), f.waitingTenants[k], len(waitingCons[k])), 0
		}
	}
	return "", len(f.consumers) + len(f.tenants)
}
