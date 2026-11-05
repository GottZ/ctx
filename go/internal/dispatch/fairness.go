package dispatch

import "time"

// External fairness (design/03 §4.5, wave A03-W4; E-03-14, X15): the pick of
// the external queues of one target and their bookkeeping — a mechanism, not
// data (no setting; the interactive dispatch.fairness stays pinned to fifo).
// Since A03-W9 two external classes share it: ONE record set per target
// (extFair) for XU and XB — the shares, heldBy and the slot-time meter count
// both classes together (design/03 §4.5 no. 2) — while the pick runs per
// class in its own queue, XU strictly before XB (rank order, decision E2):
// an XB admission never takes a unit a waiting XU head claims
// (externalClaimLocked — the hold and d2 bind XB).
//
//   - Shares per target over both external classes: T_act = the tenants with
//     at least one external lease or waiter on the target, tenantShare =
//     max(1, ⌈E_eff / T_act⌉), and inside tenant t consumerShare(c) =
//     max(1, ⌈tenantShare · fw(c) / Σ fw(active consumers of t)⌉) with
//     fw = Principal.FairWeight (0 ⇒ 1). A target without an external cap
//     (self-admitting without external_max_inflight) has no shares, only the
//     pick order.
//   - Pick (one function for the wake, the head check of the rank scan and
//     the external newcomer): a waiting continuation is the class head
//     (A03-W17). Otherwise the bucket heads are ordered: stage (i) are the
//     buckets whose consumer and tenant stay within their shares after the
//     admission — a consumer or tenant holding nothing is within its share
//     whatever the weight (review 2 F-1 (ii)) — stage (ii) all others (work
//     conservation: one consumer may fill E_eff). Inside the first non-empty
//     stage the tenant with the least slot time goes first, inside the
//     tenant the consumer with the least slot time per fair weight; equal
//     slot times keep arrival order (seq). Inside a bucket strictly FIFO.
//   - The fairness-first head holds the class (review 2 F-1 (i)): it is
//     taken when it fits now; when it does not, no other head and no
//     newcomer of the class takes a unit (a newcomer that ranks before it is
//     the fairness-first one), so its units accumulate — except while the
//     in-house classes ranked above the external class keep it out on their
//     own; then any head that fits goes (d1: a head that does not fit never
//     blocks another bucket).
//   - Slot-time meter: Σ lease duration × weight per consumer and per tenant
//     in a fixed window of usageWindowDur (the M6 idiom of usage.go), booked
//     when a lease settles, plus the running time of the held leases at pick
//     time (O(1) per bucket from a maintained running sum). Keyed on the
//     identity (_consumer:<id>) and the tenant — never on fairKey, which is
//     "" for every consumer.
//   - d2 (the A03-W7 F-A guard, an addition to the work-conservation rule of
//     §4.2): in o1, while the external head the freed units serve first
//     fits the external partition (heldExt + w ≤ E_eff), is not kept out by
//     interactive alone (review 2 F-2, aligned with the backfill bound in
//     review 3 F3-1) and would no longer fit the slots after the background
//     unit, background takes no unit the head needs — externalReservedLocked.

// slotMeter is the Σw and slot-time state of one consumer or tenant on one
// target (guarded by Dispatcher.mu).
type slotMeter struct {
	// held is the sum of the weights of the held external leases (heldBy).
	held int
	// base anchors runSum; runSum is Σ w·(slotStart − base) in seconds over
	// the held leases, so their running slot time at now is
	// held·(now − base) − runSum. Both reset when held returns to 0.
	base   time.Time
	runSum float64
	// winStart / served are the fixed window of the settled slot time
	// (unit-seconds); a zero winStart is no window.
	winStart time.Time
	served   float64
}

// add books one admitted lease of weight w that started at start.
func (m *slotMeter) add(w int, start time.Time) {
	if m.held == 0 {
		m.base, m.runSum = start, 0
	}
	m.held += w
	m.runSum += float64(w) * start.Sub(m.base).Seconds()
}

// settle books the end of one lease of weight w that started at start: out
// of held, its slot time into the window at now (a new window when none is
// open or it ran out — the lease is charged in full to the window it ends
// in, the accepted fixed-window distortion of usage.go).
func (m *slotMeter) settle(w int, start, now time.Time, win time.Duration) {
	m.held -= w
	if m.held == 0 {
		m.runSum = 0
	} else {
		m.runSum -= float64(w) * start.Sub(m.base).Seconds()
	}
	if m.winStart.IsZero() || now.Sub(m.winStart) >= win {
		m.winStart, m.served = now, 0
	}
	m.served += float64(w) * max(now.Sub(start), 0).Seconds()
}

// slotTime is the slot time at now: the settled slot time of the live window
// plus the running slot time of the held leases.
func (m *slotMeter) slotTime(now time.Time, win time.Duration) float64 {
	var s float64
	if !m.winStart.IsZero() && now.Sub(m.winStart) < win {
		s = m.served
	}
	if m.held > 0 {
		s += float64(m.held)*now.Sub(m.base).Seconds() - m.runSum
	}
	return s
}

// idle reports whether the meter holds nothing and has no live window.
func (m *slotMeter) idle(now time.Time, win time.Duration) bool {
	return m.held == 0 && (m.winStart.IsZero() || now.Sub(m.winStart) >= win)
}

// fairConsumer is the record of one consumer identity (bucket key
// _consumer:<id>) on one target.
type fairConsumer struct {
	slotMeter
	tenant *fairTenant
	// fw is the effective fair weight (fairWeightOf) — the latest one seen
	// while active; refs counts the consumer's waiters plus held leases, the
	// consumer is active while refs > 0.
	fw   int
	refs int
	// waiting / waitW are the consumer's queued waiters (continuations
	// included) and the sum of their weights — the own position of the
	// external prediction and Retry-After (A03-W6, prediction.go) — per
	// external class (indexed by Class, A03-W9: a position counts only the
	// waiters of its own class; the in-house rows stay zero).
	waiting [numClasses]int
	waitW   [numClasses]int
}

// fairTenant is the record of one tenant on one target.
type fairTenant struct {
	slotMeter
	id string
	// consumers counts its active consumers (the tenant is active while > 0)
	// and sumFW their fair weights; records counts the consumer records that
	// point here (the tenant record outlives none of them).
	consumers int
	sumFW     int
	records   int
	// waitingConsumers counts its consumers with a queued waiter and waitFW
	// their fair weights (A03-W6): the own tenant's part of the share in the
	// external prediction (prediction.go) — per external class (A03-W9).
	waitingConsumers [numClasses]int
	waitFW           [numClasses]int
}

// extFair is the fairness bookkeeping of one target's external queue. A
// record outlives its activity until its slot-time window ran out (the
// reaper's sweep), so a consumer that pauses keeps its history.
type extFair struct {
	consumers     map[string]*fairConsumer
	tenants       map[string]*fairTenant
	activeTenants int
	// waitingTenants counts the tenants with at least one queued waiter —
	// the tenant term of the share in the external prediction (A03-W6) —
	// per external class (A03-W9).
	waitingTenants [numClasses]int
	// viewAt is the clock of the fairness order (review 4 FR4-2): the
	// running slot time of the held leases is read at viewAt, which moves
	// only at events — every wake (wakeLocked) and every external lease
	// start and end — never by the mere passing of time between them. So
	// the pick, and with it "the fairness-first head holds the class", is
	// a function of the state; a waiting head never becomes the pick
	// without the wake that hands it its units.
	viewAt time.Time
}

// touch moves the clock of the fairness order to now (never back).
func (f *extFair) touch(now time.Time) {
	if now.After(f.viewAt) {
		f.viewAt = now
	}
}

func newExtFair() *extFair {
	return &extFair{consumers: make(map[string]*fairConsumer), tenants: make(map[string]*fairTenant)}
}

// fairWeightOf is the effective fair weight of p: Principal.FairWeight, 0 or
// less ⇒ 1, bounded by the representation bound maxLimitCount so that
// tenantShare · fw cannot overflow (the stored value is 1..100).
func fairWeightOf(p Principal) int {
	return min(max(p.FairWeight, 1), maxLimitCount)
}

// consumer returns (or creates) the record of the consumer identity key of
// p. A consumer id belongs to one tenant (context_model_consumers.tenant_id),
// so the tenant binds at creation.
func (f *extFair) consumer(key string, p Principal) *fairConsumer {
	if c, ok := f.consumers[key]; ok {
		return c
	}
	t, ok := f.tenants[p.TenantID]
	if !ok {
		t = &fairTenant{id: p.TenantID}
		f.tenants[p.TenantID] = t
	}
	t.records++
	c := &fairConsumer{tenant: t}
	f.consumers[key] = c
	return c
}

// enter counts one waiter or lease of c with fair weight fw.
func (f *extFair) enter(c *fairConsumer, fw int) {
	t := c.tenant
	if c.refs == 0 {
		if t.consumers == 0 {
			f.activeTenants++
		}
		t.consumers++
		c.fw = fw
		t.sumFW += fw
	} else {
		c.reweigh(fw)
	}
	c.refs++
}

// reweigh moves the fair weight of an active record to fw, its tenant's Σ
// fair_weight and waiting Σ fair_weight with it (enter, and the undo of a
// refused push, A03-W6 review 4 FR4-2).
func (c *fairConsumer) reweigh(fw int) {
	if c.fw == fw {
		return
	}
	t := c.tenant
	t.sumFW += fw - c.fw
	for k, n := range c.waiting {
		if n > 0 {
			t.waitFW[k] += fw - c.fw
		}
	}
	c.fw = fw
}

// leave uncounts one waiter or lease of c.
func (f *extFair) leave(c *fairConsumer) {
	c.refs--
	if c.refs > 0 {
		return
	}
	t := c.tenant
	t.sumFW -= c.fw
	t.consumers--
	if t.consumers == 0 {
		f.activeTenants--
	}
}

// waiterEnter / waiterLeave keep the activity in step with the external
// queues (classQueue.push and its two exits, popHead and remove); the
// waiting counters move in the waiter's own class (A03-W9).
func (f *extFair) waiterEnter(key string, w *waiter) {
	c := f.consumer(key, w.principal)
	w.fair = c
	f.enter(c, fairWeightOf(w.principal))
	k := w.class
	if c.waiting[k] == 0 {
		t := c.tenant
		if t.waitingConsumers[k] == 0 {
			f.waitingTenants[k]++
		}
		t.waitingConsumers[k]++
		t.waitFW[k] += c.fw
	}
	c.waiting[k]++
	c.waitW[k] += w.weight
}

func (f *extFair) waiterLeave(key string, w *waiter) {
	c := w.fair
	if c == nil {
		if c = f.consumers[key]; c == nil {
			return
		}
	}
	f.leave(c)
	k := w.class
	c.waiting[k]--
	c.waitW[k] -= w.weight
	if c.waiting[k] == 0 {
		t := c.tenant
		t.waitFW[k] -= c.fw
		t.waitingConsumers[k]--
		if t.waitingConsumers[k] == 0 {
			f.waitingTenants[k]--
		}
	}
}

// leaseEnter books an admitted external lease (admitLocked): activity, heldBy
// of its consumer and tenant, and the start of its slot time. c is the
// record the waiter carried out of the queue, nil for an immediate admission.
// The fair weight comes from the principal only on an immediate admission; a
// handed-off waiter keeps the record's current weight (A03-W6, the PR #132
// bot-review hand-over): a newer waiter of the same consumer may have
// brought a newer fair_weight, and the older waiter's stale value must not
// reset the record and its tenant's Σ fair_weight.
func (f *extFair) leaseEnter(l *Lease, c *fairConsumer, start time.Time) {
	fw := fairWeightOf(l.principal)
	if c == nil {
		c = f.consumer(l.principal.identityKey(), l.principal)
	} else {
		fw = c.fw
	}
	f.enter(c, fw)
	c.add(l.weight, start)
	c.tenant.add(l.weight, start)
	l.fair, l.slotStart = c, start
	f.touch(start) // a lease never starts after the view (no negative running time)
}

// leaseLeave settles an external lease (finishLocked — release, reaper,
// cancel race and expiry all end there): heldBy down, its slot time into the
// windows of its consumer and tenant, activity down.
func (f *extFair) leaseLeave(l *Lease, now time.Time, win time.Duration) {
	c := l.fair
	c.settle(l.weight, l.slotStart, now, win)
	c.tenant.settle(l.weight, l.slotStart, now, win)
	f.leave(c)
	f.touch(now)
}

// sweep drops the records without activity whose window ran out (reaper
// cadence, like sweepUsageLocked): the maps never outgrow the identities of
// the last window.
func (f *extFair) sweep(now time.Time, win time.Duration) {
	for key, c := range f.consumers {
		if c.refs == 0 && c.idle(now, win) {
			c.tenant.records--
			delete(f.consumers, key)
		}
	}
	for id, t := range f.tenants {
		if t.records == 0 && t.consumers == 0 && t.idle(now, win) {
			delete(f.tenants, id)
		}
	}
}

// fairClock is the clock of the slot-time meter: now in production (the
// instant the caller already read); a test seam (setFairClockForTest) drives
// it for the fake-clock gates. Read under Dispatcher.mu only.
func (d *Dispatcher) fairClock(now time.Time) time.Time {
	if d.fairNow != nil {
		return d.fairNow()
	}
	return now
}

// shareOf is max(1, ⌈n·num / den⌉), the share formula of both levels:
// tenantShare = shareOf(E_eff, 1, T_act), consumerShare(c) =
// shareOf(tenantShare, fw(c), Σ fw). den < 1 reads as 1 (unreachable: a
// bucket head's consumer is active, so its tenant's Σ fw ≥ 1).
func shareOf(n, num, den int) int {
	den = max(den, 1)
	return max(1, (n*num+den-1)/den)
}

// overShare is the share term of one level for a head of weight w (F-1 (ii),
// review 2): a consumer or tenant that holds no unit is within its share —
// the floor is one request in flight, whatever its weight (without it a head
// heavier than its share stays over it for good and every lighter head
// passes it); one that holds units is over once held + w exceeds the share.
func overShare(held, w, share int) bool {
	return held > 0 && held+w > share
}

// pickKey orders the candidate heads of the pick: under share first, then
// the tenant's slot time, the consumer's slot time per fair weight, seq.
type pickKey struct {
	over     bool
	tenant   float64
	consumer float64
	seq      uint64
}

func (k pickKey) less(o pickKey) bool {
	switch {
	case k.over != o.over:
		return !k.over
	case k.tenant != o.tenant:
		return k.tenant < o.tenant
	case k.consumer != o.consumer:
		return k.consumer < o.consumer
	}
	return k.seq < o.seq
}

// externalFits is the external admission term for weight w against the
// target's resolved external capacity (0 = no external cap): the slot term
// on a slot-managed target and the partition term (rank.go
// externalAdmissibleLocked, the same terms after the offer check).
func externalFits(st *targetState, w, slots, capacity int) bool {
	return (slots <= 0 || st.held+w <= slots) && (capacity == 0 || st.heldExt+w <= capacity)
}

// extFit is the admission bound one external pick reads once (A03-W9): the
// target's slots and resolved external capacity, and claim — the units the
// class leaves to the waiting heads of the external classes ranked above it
// (externalClaimLocked; 0 for XU, which no external class outranks). A head
// of weight w fits when w plus the claim fits: the higher-ranked head still
// fits after this admission.
type extFit struct{ slots, capacity, claim int }

func (b extFit) fits(st *targetState, w int) bool {
	return externalFits(st, w+b.claim, b.slots, b.capacity)
}

// fairView is what one pick reads once: the clock of the fairness order
// (extFair.viewAt — frozen between events, review 4 FR4-2), the slot-time
// window and the tenant share (0: no external cap — no shares, only the
// order).
type fairView struct {
	now         time.Time
	win         time.Duration
	tenantShare int
}

func (d *Dispatcher) fairViewLocked(f *extFair, capacity int) fairView {
	v := fairView{now: f.viewAt, win: d.usageWindowDur()}
	if v.now.IsZero() {
		v.now = d.fairClock(time.Now()) // no event on the target yet
	}
	if capacity > 0 && f.activeTenants > 0 {
		v.tenantShare = shareOf(capacity, 1, f.activeTenants)
	}
	return v
}

// key is the fairness order of a head of weight w and arrival seq of the
// consumer c with the fair weight fw.
func (v fairView) key(c *fairConsumer, fw, w int, seq uint64) pickKey {
	k := pickKey{
		over:     v.tenantShare == 0,
		tenant:   c.tenant.slotTime(v.now, v.win),
		consumer: c.slotTime(v.now, v.win) / float64(max(fw, 1)),
		seq:      seq,
	}
	if v.tenantShare > 0 {
		k.over = overShare(c.held, w, shareOf(v.tenantShare, fw, c.tenant.sumFW)) ||
			overShare(c.tenant.held, w, v.tenantShare)
	}
	return k
}

// headRecord is the fairness record a queued head carries (the record
// travels on the waiter); the map lookup is the unreachable fallback.
func headRecord(f *extFair, key string, h *waiter) *fairConsumer {
	if h.fair != nil {
		return h.fair
	}
	return f.consumer(key, h.principal)
}

// externalPickLocked is THE pick of the external class c's queue: the waiter
// the wake dequeues next, the head headAdmissibleLocked checks, and the
// reference of the external newcomer — or nil (also when the target does not
// offer the external classes). Every candidate fits only beyond the claim of
// the external classes ranked above c (extFit, externalClaimLocked; A03-W9:
// XB never takes a unit a waiting XU head needs — 0 for XU). A waiting
// continuation is the head of its class (A03-W17, K21): it goes before every
// bucket and, while it does not fit with its weight, blocks them all (its
// weight is the head minimum, so the O(1) early exit returns nil). Otherwise
// the fairness-first bucket head (externalFirst) goes when it fits. When it
// does not fit, it holds the class (F-1 (i), review 2): no other bucket head
// takes a unit, so the units it needs accumulate as leases end — a head that
// fits with its weight PLUS the first head's need cannot exist while the
// first does not fit (review 3 F3-5), so the reservation is no backfill at
// all. The exception: while the classes ranked above c keep it out on their
// own (externalHeadHoldsLocked false), the backfill takes the first head in
// fairness order that fits (d1). O(1) when not even the lightest head fits,
// else one pass over the buckets, two in the d1 case; O(1) per bucket — for
// XB plus the pass over XU's bucket heads that resolves the claim.
func (d *Dispatcher) externalPickLocked(st *targetState, c Class, slots int) (*waiter, string) {
	q := st.extQueue(c)
	if q.total == 0 {
		return nil, ""
	}
	offered, capacity := d.externalOffer(st.origin)
	if !offered || !externalFits(st, q.headMinWeight(), slots, capacity) {
		return nil, ""
	}
	fit := extFit{slots: slots, capacity: capacity, claim: d.externalClaimLocked(st, c, slots, capacity)}
	if !fit.fits(st, q.headMinWeight()) {
		return nil, ""
	}
	if len(q.cont) > 0 {
		return q.cont[0], ""
	}
	v := d.fairViewLocked(q.fair, capacity)
	first, key, _ := externalFirst(q, v, slots, capacity)
	switch {
	case first == nil || fit.fits(st, first.weight):
		return first, key
	case d.externalHeadHoldsLocked(st, c, first, fit):
		return nil, ""
	}
	return externalBackfill(st, q, v, fit)
}

// externalFirst is the fairness-first bucket head of the external queue q:
// the first head in fairness order among the heads within the policy (w ≤ S
// on a slot-managed target, w ≤ E_eff under an external cap — a head beyond
// them ends in the overweight drain of the next wake and holds nothing),
// whether it fits now or not; nil without one. One pass, O(1) per bucket.
func externalFirst(q *classQueue, v fairView, slots, capacity int) (first *waiter, key string, fk pickKey) {
	for k, b := range q.buckets {
		h := b[0]
		if (slots > 0 && h.weight > slots) || (capacity > 0 && h.weight > capacity) {
			continue
		}
		c := headRecord(q.fair, k, h)
		hk := v.key(c, c.fw, h.weight, h.seq)
		if first == nil || hk.less(fk) {
			first, key, fk = h, k, hk
		}
	}
	return first, key, fk
}

// externalBackfill is d1: the pick of q while its fairness-first head does
// not fit and does not hold the class — the first head in fairness order that
// fits with its weight (beyond the claim, fit); a head that does not fit
// blocks no other bucket. Inside a bucket strictly FIFO (only bucket heads
// are candidates).
func externalBackfill(st *targetState, q *classQueue, v fairView, fit extFit) (*waiter, string) {
	var best *waiter
	var bestKey string
	var bk pickKey
	for key, b := range q.buckets {
		h := b[0]
		if !fit.fits(st, h.weight) {
			continue
		}
		c := headRecord(q.fair, key, h)
		k := v.key(c, c.fw, h.weight, h.seq)
		if best == nil || k.less(bk) {
			best, bestKey, bk = h, key, k
		}
	}
	return best, bestKey
}

// externalHeadHoldsLocked reports whether the fairness-first head first of
// the external class c, which does not fit now (fit), holds its class for
// itself (F-1 (i)): true while it would fit once the leases of the classes
// ranked at or below c drained — its own class's leases (no backfill refills
// them), the external classes below it (the claim binds them, A03-W9) and,
// in o1 for XU, background (which d2 binds); false while the classes ranked
// ABOVE c keep it out on their own (keptOutLocked: interactive, background
// in o2 for XU; every in-house class and XU for XB — "above" per rank order,
// A03-W4 handover): a hold the head cannot use would only idle the units
// (the principle of review 2 F-2). O(#classes).
func (d *Dispatcher) externalHeadHoldsLocked(st *targetState, c Class, first *waiter, fit extFit) bool {
	if fit.fits(st, first.weight) {
		return false
	}
	return !d.keptOutLocked(st, c, first.weight, fit.slots, fit.capacity)
}

// keptOutLocked reports whether the classes ranked above the external class
// c keep a head of weight w out on their own (A03-W9, review 2 F-2): with
// every class at or below c drained, w still exceeds the slots (on a
// slot-managed target) or, beside the external units the classes above c
// hold, the external capacity. For XU no external class ranks above it, so
// only the slot term reads — interactive in o1, interactive and background
// in o2 (the A03-W4 terms).
func (d *Dispatcher) keptOutLocked(st *targetState, c Class, w, slots, capacity int) bool {
	inHouse, ext := d.aboveLocked(st, c)
	return (slots > 0 && inHouse+ext+w > slots) || (capacity > 0 && ext+w > capacity)
}

// aboveLocked sums the units held by the classes ranked above c in the rank
// table in effect, split into the in-house and the external part (classHeld,
// A03-W9). O(#classes).
func (d *Dispatcher) aboveLocked(st *targetState, c Class) (inHouse, external int) {
	for _, h := range d.currentSettings().RankOrder {
		switch {
		case h == c:
			return inHouse, external
		case !h.valid():
			continue // only the unvalidated test hook stores such an order
		case h.External():
			external += st.classHeld[h]
		default:
			inHouse += st.classHeld[h]
		}
	}
	return inHouse, external
}

// externalEligibleNowLocked is the immediate-admission half of the rank scan
// for an external newcomer of class c = w.class: it fits with its weight
// (for XB beyond the claim of a waiting XU head, admissibleLocked), no
// continuation of its class waits (the class head goes first, A03-W17), its
// own bucket is empty (FIFO inside a bucket — never past its own consumer's
// waiters), no head of its class that fits waits (that one goes first,
// through the pick; after every wake none does, so this guards the swap
// window before wakeAll), no waiting fairness-first head holds the class
// unless the newcomer ranks before it (F-1 (i),
// externalNewcomerBackfillsLocked), and no class ranked above has an
// admissible head. A waiting head that does not fit and does not hold the
// class is passed (d1).
func (d *Dispatcher) externalEligibleNowLocked(st *targetState, w *waiter, slots int) bool {
	c := w.class
	if !d.admissibleLocked(st, c, w.weight, slots) {
		return false
	}
	if q := st.extQueue(c); q.total > 0 {
		if len(q.cont) > 0 || len(q.buckets[q.bucketKey(w.principal)]) > 0 || d.headAdmissibleLocked(st, c, slots) {
			return false
		}
		if !d.externalNewcomerBackfillsLocked(st, w, slots) {
			return false
		}
	}
	return d.rankClearLocked(st, c, slots)
}

// externalNewcomerBackfillsLocked is F-1 (i) for the external newcomer w (no
// waiting head of its class fits — checked before): while the waiting
// fairness-first head of its class holds the class, w is admitted only when
// it ranks before that head in the fairness order (then it is the
// fairness-first one). One pass over the buckets (plus the claim's pass for
// XB).
func (d *Dispatcher) externalNewcomerBackfillsLocked(st *targetState, w *waiter, slots int) bool {
	q := st.extQueue(w.class)
	_, capacity := d.externalOffer(st.origin)
	v := d.fairViewLocked(q.fair, capacity)
	first, _, fk := externalFirst(q, v, slots, capacity)
	if first == nil {
		return true
	}
	fit := extFit{slots: slots, capacity: capacity, claim: d.externalClaimLocked(st, w.class, slots, capacity)}
	if !d.externalHeadHoldsLocked(st, w.class, first, fit) {
		return true
	}
	return q.fair.newcomerKey(v, w).less(fk)
}

// newcomerKey is the fairness order of an external newcomer: its consumer's
// record (heldBy, slot time; a fresh one under its tenant's record without a
// record of its own) and the newest arrival — it loses every tie against a
// waiting head. The fair weight is the record's while the consumer is
// active (the one Σ fw counts), else the principal's.
func (f *extFair) newcomerKey(v fairView, w *waiter) pickKey {
	fw := fairWeightOf(w.principal)
	c, ok := f.consumers[w.principal.identityKey()]
	switch {
	case !ok:
		t := f.tenants[w.principal.TenantID]
		if t == nil {
			t = &fairTenant{id: w.principal.TenantID}
		}
		c = &fairConsumer{tenant: t}
	case c.refs > 0:
		fw = c.fw
	}
	return v.key(c, fw, w.weight, ^uint64(0))
}

// externalReservedLocked is d2 (A03-W7 F-A; design/03 §4.2 addition): true
// when the w units a background acquire would take are reserved for a
// waiting external head — only for an external class ranked above
// background (XU in o1; o2 is unchanged, and XB ranks below background in
// both), on a slot-managed target, for the head the freed units serve first
// (externalNeedLocked, weight need), while it fits the external partition
// (heldExt + need ≤ E_eff), is not kept out by the classes ranked above its
// class alone (keptOutLocked — for XU in o1: heldI + need ≤ S, review 2 F-2;
// the external leases are not counted, they drain because the backfill
// reservation binds the external classes too — the same bound as
// externalHeadHoldsLocked, review 3 F3-1), and would no longer fit after the
// background admission (held + w + need > S). Background keeps every unit
// beyond the head's need, in particular its floor R − heldI (heldInt + w ≤ R
// implies held + w + need ≤ S for every head that fits E_eff), and the aging
// escape does not overrule it (the reservation is class order, not herald).
// O(1) while no waiting weight can tip the slots (held + w + maxWeight ≤ S),
// else O(#buckets) for the need.
func (d *Dispatcher) externalReservedLocked(st *targetState, w, slots int) bool {
	if slots <= 0 {
		return false
	}
	for _, h := range d.currentSettings().RankOrder {
		switch {
		case h == ClassBackground:
			return false
		case !h.External():
			continue
		}
		q := st.extQueue(h)
		if q.total == 0 || st.held+w+q.maxWeight <= slots {
			continue
		}
		offered, capacity := d.externalOffer(st.origin)
		if !offered {
			return false
		}
		need := d.externalNeedLocked(st, h, slots, capacity)
		if need > 0 && st.held+w+need > slots &&
			(capacity == 0 || st.heldExt+need <= capacity) &&
			!d.keptOutLocked(st, h, need, slots, capacity) {
			return true
		}
	}
	return false
}

// externalClaimLocked is the bind of the external classes ranked above the
// external class c on its admissions (A03-W9; A03-W4 handovers: the hold and
// d2 bind XB): the sum, over each such class h with a waiting head, of the
// weight of the head the freed units serve first (externalNeedLocked: its
// continuation, its fairness-first head while it fits or holds, else its
// lightest head) — unless the classes ranked above h keep that head out on
// their own (keptOutLocked; review 2 F-2: no claim for a head that cannot
// use it). A class-c admission leaves these units free (extFit): it never
// takes a unit a higher-ranked head needs — neither while that head holds
// its class (F-1 (i)) nor while it waits for its units (d2), in o1 and o2.
// Unlike d2 for background, the claim also stands for a head that does not
// fit E_eff now: XB takes units of E, which that head waits for. 0 for XU
// (no external class ranks above it in o1 or o2). The needs are read
// without the claims of classes above h — with two external classes no
// such class exists. O(#buckets of the classes above c).
//
// Named limit (A03-W9 review 2 F2-2): the claim and the rank order starve
// XB under sustained XU load above E — while an XU head waits at every
// release, an XB acquire never gets a unit; it ends at its wait budget
// (dispatch.external_max_wait) with ErrWaitExpired (reason expired) and a
// Retry-After, never waits past it, and leaves no goroutine behind
// (TestExternalBulkStarvationEndsAtTheBudget). An aging rule for XB
// (external_bulk_aging_after, design/03 §4.8) is named, not built — A03-W8a.
func (d *Dispatcher) externalClaimLocked(st *targetState, c Class, slots, capacity int) int {
	claim := 0
	for _, h := range d.currentSettings().RankOrder {
		if h == c {
			return claim
		}
		if !h.External() || st.extQueue(h).total == 0 {
			continue
		}
		if need := d.externalNeedLocked(st, h, slots, capacity); need > 0 && !d.keptOutLocked(st, h, need, slots, capacity) {
			claim += need
		}
	}
	return claim
}

// externalClaimBlocksLocked reports whether an admission of w units in the
// external class c would take units the external classes ranked above it
// claim (externalClaimLocked): w plus the claim no longer fits. False
// without a claim — always for XU.
func (d *Dispatcher) externalClaimBlocksLocked(st *targetState, c Class, w, slots int) bool {
	_, capacity := d.externalOffer(st.origin)
	claim := d.externalClaimLocked(st, c, slots, capacity)
	return claim > 0 && !externalFits(st, w+claim, slots, capacity)
}

// externalNeedLocked is the weight of the head of the external class h the
// freed units serve first — the need of d2 and of the claim: the waiting
// continuation (the class head), else the fairness-first bucket head while
// it fits or holds the class (F-1 (i)), else (the classes ranked above h
// keep it out; the backfill runs per d1) the lightest bucket head, the first
// one the freed units can serve. 0 without a head within the policy.
// O(#buckets).
func (d *Dispatcher) externalNeedLocked(st *targetState, h Class, slots, capacity int) int {
	q := st.extQueue(h)
	if len(q.cont) > 0 {
		return q.cont[0].weight
	}
	first, _, _ := externalFirst(q, d.fairViewLocked(q.fair, capacity), slots, capacity)
	fit := extFit{slots: slots, capacity: capacity}
	switch {
	case first == nil:
		return 0
	case !fit.fits(st, first.weight) && !d.externalHeadHoldsLocked(st, h, first, fit):
		return q.headMinWeight()
	}
	return first.weight
}

// headMinWeight is the smallest weight among the bucket heads of the
// external queue (0 when it is empty), recomputed lazily after a head left —
// O(1) per read while the heads are unchanged, O(#buckets) after a change.
// While a continuation waits it is that continuation's weight: the class
// head (A03-W17) the freed units serve first, so the pick's O(1) early exit
// blocks every bucket head while it does not fit.
func (q *classQueue) headMinWeight() int {
	if len(q.cont) > 0 {
		// The oldest continuation is the head of the class (A03-W17): it
		// is the one the freed units serve first.
		return q.cont[0].weight
	}
	if q.headMinStale {
		q.headMin = 0
		for _, b := range q.buckets {
			if w := b[0].weight; q.headMin == 0 || w < q.headMin {
				q.headMin = w
			}
		}
		q.headMinStale = false
	}
	return q.headMin
}

// headJoined / headLeft keep the head-weight minimum and the fairness
// activity of the external queue in step (push into a new bucket; popHead or
// remove of a head). No-ops on the interactive queue.
func (q *classQueue) headJoined(w *waiter) {
	if q.fair != nil && !q.headMinStale && (q.headMin == 0 || w.weight < q.headMin) {
		q.headMin = w.weight
	}
}

func (q *classQueue) headLeft() {
	if q.fair != nil {
		q.headMinStale = true
	}
}
