package dispatch

// White-box gates of A03-W4 (they read the fairness internals and therefore
// do not compile against a tree without them — that compile error is their
// red run): the share formula at the gate fixtures, the d2 background floor by
// exhaustive enumeration, the pick-consistency differential trace (h) and the
// pick-cost scaling probe (i).

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestExternalShareFormulaAtTheGateFixtures recomputes the shares the gates
// rely on (design/03 §4.5 no. 2): gate (a) E = 6, T_act = 2 ⇒ tenantShare 3,
// ten consumers of fw 1 ⇒ 1 each, one consumer ⇒ 3; gate (d) E = 4, one
// tenant, fw 3 and 1 ⇒ 3 and 1; gate (b) E = 5, two equal consumers ⇒ 3;
// the floor max(1, …) for a consumer whose exact share is below one unit.
func TestExternalShareFormulaAtTheGateFixtures(t *testing.T) {
	for _, tc := range []struct {
		name           string
		n, num, den, w int
	}{
		{"(a) tenant share E=6 T=2", 6, 1, 2, 3},
		{"(a) consumer of ten, fw 1", 3, 1, 10, 1},
		{"(a) single consumer", 3, 1, 1, 3},
		{"(d) fw 3 of Σ4", 4, 3, 4, 3},
		{"(d) fw 1 of Σ4", 4, 1, 4, 1},
		{"(b) E=5 one tenant, fw 10 of Σ20", 5, 10, 20, 3},
		{"ceil 7/3", 7, 1, 3, 3},
		{"floor of one unit", 1, 1, 100, 1},
		{"den 0 reads as 1", 2, 1, 0, 2},
	} {
		if got := shareOf(tc.n, tc.num, tc.den); got != tc.w {
			t.Errorf("%s: shareOf(%d, %d, %d) = %d, want %d", tc.name, tc.n, tc.num, tc.den, got, tc.w)
		}
	}
	for fw, want := range map[int]int{-3: 1, 0: 1, 1: 1, 100: 100, maxLimitCount + 1: maxLimitCount} {
		if got := fairWeightOf(Principal{FairWeight: fw}); got != want {
			t.Errorf("fairWeightOf(%d) = %d, want %d", fw, got, want)
		}
	}
}

// TestExternalReservationKeepsTheBackgroundFloor enumerates d2 over every
// small partition (S 2..9, R 1..S−1, every split of the held units into
// interactive, background and external, a waiting head of every weight
// within E, o1/o2), the heads queued for real: the reservation never
// refuses a background unit inside the floor (heldInt + 1 ≤ R ⇒ not
// reserved), refuses one exactly when the head the freed units serve first
// fits E, is not kept out by interactive alone (review 2 F-2, aligned with
// the backfill bound by review 3 F3-1: heldI + need ≤ S) and would no
// longer fit S after the background unit, and never acts in o2. The need (F-1 (i)): with a second, lighter
// and younger head of another consumer the fairness-first head is the
// older one — its weight is the need while it fits or binds the backfill,
// the lightest head's while interactive keeps it out (heldI + h1 > S).
func TestExternalReservationKeepsTheBackgroundFloor(t *testing.T) {
	cases, refused := 0, 0
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		s := DefaultSettings()
		s.RankOrder = mustRankOrder(t, order)
		s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
		for slots := 2; slots <= 9; slots++ {
			for r := 1; r < slots; r++ {
				d, _ := newTestDispatcher(t, s, partitionPolicy(slots, r))
				e := slots - r
				d.mu.Lock()
				st := d.targetLocked(testOrigin)
				for heldExt := 0; heldExt <= e; heldExt++ {
					for heldI := 0; heldI+heldExt <= slots; heldI++ {
						for heldB := 0; heldI+heldB+heldExt <= slots; heldB++ {
							for h1 := 1; h1 <= e; h1++ {
								for _, h2 := range []int{0, 1} {
									got, want, label := reservationCase(t, d, st, order, slots, e, heldI, heldB, heldExt, h1, h2)
									cases++
									if got {
										refused++
									}
									switch {
									case got != want:
										t.Errorf("%s S=%d R=%d %s: reserved=%v, want %v", order, slots, r, label, got, want)
									case got && heldI+heldB+1 <= r:
										t.Errorf("%s S=%d R=%d %s: reserved inside the background floor", order, slots, r, label)
									}
								}
							}
						}
					}
				}
				d.mu.Unlock()
			}
		}
	}
	t.Logf("%d states enumerated, %d with the background unit reserved", cases, refused)
}

// reservationCase builds one d2 state on st under d.mu — the held units set
// directly (heldB background leases in cancelable), the heads h1 (older) and
// h2 (0 = none) queued through enqueueLocked for two consumers that hold
// nothing — reads externalReservedLocked for one background unit and tears
// the state down. want is the rule written out (need per F-1 (i)).
func reservationCase(t *testing.T, d *Dispatcher, st *targetState, order string, slots, e, heldI, heldB, heldExt, h1, h2 int) (got, want bool, label string) {
	t.Helper()
	st.held, st.heldExt = heldI+heldB+heldExt, heldExt
	for i := 0; i < heldB; i++ {
		st.cancelable[&Lease{}] = nil
	}
	head := func(consumer string, w int) *waiter {
		hw := &waiter{class: ClassExternal, principal: xuPrincipal(consumer, "tenant-h"), weight: w, admitCh: make(chan struct{})}
		if err := d.enqueueLocked(st, hw); err != nil {
			t.Fatalf("enqueue %s: %v", consumer, err)
		}
		return hw
	}
	heads := []*waiter{head("h1", h1)}
	if h2 > 0 {
		heads = append(heads, head("h2", h2))
	}
	got = d.externalReservedLocked(st, 1, slots)
	for _, hw := range heads {
		st.external.remove(hw)
	}
	clear(st.cancelable)
	st.held, st.heldExt = 0, 0

	need := h1
	fits := heldI+heldB+heldExt+h1 <= slots && heldExt+h1 <= e
	if !fits && heldI+h1 > slots && h2 > 0 {
		need = min(h1, h2) // interactive keeps the fairness-first head out: the lightest head
	}
	held := heldI + heldB + heldExt
	want = order == ExternalOrderO1 && heldExt+need <= e && held+1+need > slots && heldI+need <= slots
	label = fmt.Sprintf("heldI=%d heldB=%d heldExt=%d heads=%d,%d need=%d", heldI, heldB, heldExt, h1, h2, need)
	return got, want, label
}

// TestExternalNewcomerYieldsToAFittingHead pins the guard of the external
// newcomer for the window between a policy store and its wakeAll (review
// F5/R11): E = 5 is full (w = 5), consumer c1 waits with w = 2; the policy
// grows E to 7 without the wake, so c1's head fits now. A newcomer of
// another consumer that would fit too is not admitted past it (TryAcquire ⇒
// ErrWouldBlock); the wake then admits c1.
func TestExternalNewcomerYieldsToAFittingHead(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	occ := mustAcquire(t, d, xctx(bg, "c0", "tenant-a"), weightedReq(weightE))
	defer occ.Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "c1", weightedReq(2), ch)
	waitFor(t, "c1 queued", func() bool { return waitingExternal(d) == 1 })
	grown := partitionPolicy(weightSlots+2, weightReserved)
	d.policy.Store(&grown) // the store half of UpdatePolicy, without its wakeAll
	l, _, err := d.TryAcquire(xctx(bg, "c2", "tenant-b"), weightedReq(1))
	if l != nil {
		l.Release()
		t.Fatalf("a newcomer of another consumer was admitted past c1's head, which fits after the swap — want ErrWouldBlock")
	}
	if !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("newcomer: %v, want ErrWouldBlock", err)
	}
	d.wakeAll()
	got := recvAdmission(t, ch, "c1 after the wake")
	if got.err != nil {
		t.Fatalf("c1: %v", got.err)
	}
	got.lease.Release()
}

// TestExternalHeadBeyondThePolicyReservesNothing pins the policy filter of
// the fairness-first head (F-1 (i)): E = 5, an occupant of tenant-z holds
// one unit, consumer c1 (tenant-a) waits with w = 5; the policy shrinks E to
// 3 WITHOUT the wake (the store half of UpdatePolicy), so c1's head can
// never fit any more — it ends in the overweight drain of the next wake and
// must not reserve units until then. A newcomer of another tenant that fits
// (w = 1; it loses the tie on arrival against c1) is admitted; the wake
// then ends c1 with ErrExternalNotOffered.
func TestExternalHeadBeyondThePolicyReservesNothing(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	occ := mustAcquire(t, d, xctx(bg, "c0", "tenant-z"), weightedReq(1))
	defer occ.Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "c1", weightedReq(weightE), ch)
	waitFor(t, "c1 queued", func() bool { return waitingExternal(d) == 1 })
	shrunk := partitionPolicy(weightSlots, weightSlots-3)
	d.policy.Store(&shrunk) // the store half of UpdatePolicy, without its wakeAll
	l, _, err := d.TryAcquire(xctx(bg, "c2", "tenant-b"), weightedReq(1))
	if err != nil || l == nil {
		t.Fatalf("newcomer next to a head beyond the policy: (lease %v, err %v), want an admission — a head that can never fit reserved units", l != nil, err)
	}
	l.Release()
	d.wakeAll()
	if got := recvAdmission(t, ch, "c1 after the wake"); !errors.Is(got.err, ErrExternalNotOffered) {
		t.Fatalf("c1 after the wake: %v, want ErrExternalNotOffered (overweight drain)", got.err)
	}
}

// traceRun is the state of one pick-consistency trace: its own record of
// every lease and waiter, the slot time it settled itself (the reference
// does not read the dispatcher's meter), and the fake clock.
type traceRun struct {
	t       *testing.T
	d       *Dispatcher
	rng     *rand.Rand
	clk     atomic.Int64
	slots   int
	reports chan traceReport
	nextID  int
	waiting map[int]traceWaiter
	xu      map[*Lease]traceLease
	bg      []*Lease
	served  map[string]float64 // settled slot time per consumer key and per "_tenant:<id>"
	pending bool               // a shrink left held above S or E (attrition) — the bound check waits
	o2      bool               // dispatch.external_order=o2: background ranks above the external class

	// probe statistics; bound counts the probes whose fairness-first head
	// did not fit and bound the backfill (review 2 F-1 (i)).
	probes, choices, decided, bound int
}

type traceReport struct {
	id    int
	lease *Lease
	err   error
}

type traceWaiter struct {
	cancel   context.CancelFunc
	consumer int
	weight   int
	external bool
}

type traceLease struct {
	consumer, weight int
	start            time.Time
}

const (
	traceReserved  = 2
	traceConsumers = 5
)

func traceConsumerKey(c int) string { return consumerKeyPrefix + fmt.Sprintf("c%d", c) }
func traceTenant(c int) string      { return fmt.Sprintf("t%d", c%3) }

// traceFW is distinct for the two consumers of a tenant (c and c+3), so the
// consumer order inside a tenant depends on the fair weight (review F3).
func traceFW(c int) int { return 1 + c }

// refKey is the reference's own order of the pick candidates (spec: under
// share first — a consumer or tenant holding nothing is within its share,
// review 2 F-1 (ii) — then the tenant with the least slot time, then the
// consumer with the least slot time per fair weight, then arrival) —
// deliberately not the production pickKey.less (review F3).
type refKey struct {
	over             bool
	tenant, consumer float64
	seq              uint64
}

func refLess(a, b refKey) bool {
	if a.over != b.over {
		return b.over
	}
	if a.tenant != b.tenant {
		return a.tenant < b.tenant
	}
	if a.consumer != b.consumer {
		return a.consumer < b.consumer
	}
	return a.seq < b.seq
}

func (r *traceRun) now() time.Time { return time.Unix(0, r.clk.Load()) }

func (r *traceRun) policy(slots int) Policy {
	return partitionPolicy(slots, traceReserved)
}

// settle drains the acquire reports until every outstanding acquire either
// holds a recorded lease or waits in a queue.
func (r *traceRun) settle() {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
	drain:
		for {
			select {
			case rep := <-r.reports:
				w := r.waiting[rep.id]
				delete(r.waiting, rep.id)
				switch {
				case rep.err != nil:
				case w.external:
					r.xu[rep.lease] = traceLease{consumer: w.consumer, weight: w.weight, start: r.now()}
				default:
					r.bg = append(r.bg, rep.lease)
				}
			default:
				break drain
			}
		}
		ts := target(r.t, r.d)
		if ts.External.Waiting+ts.Background.Waiting == len(r.waiting) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("trace did not settle: %d+%d waiting, %d recorded waiters", ts.External.Waiting, ts.Background.Waiting, len(r.waiting))
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func (r *traceRun) acquire(external bool) {
	c := r.rng.Intn(traceConsumers)
	w := 1 + r.rng.Intn(2) // mostly light, so a probe sees several heads that fit
	if r.rng.Intn(10) < 3 {
		w = 1 + r.rng.Intn(r.slots-traceReserved)
	}
	id := r.nextID
	r.nextID++
	ctx, cancel := context.WithCancel(context.Background())
	req := backgroundReq()
	if external {
		ctx = withPrincipal(ctx, Principal{ConsumerID: fmt.Sprintf("c%d", c), TenantID: traceTenant(c), ConsumerKind: "app", FairWeight: traceFW(c)})
		req = weightedReq(w)
	}
	r.waiting[id] = traceWaiter{cancel: cancel, consumer: c, weight: w, external: external}
	go func() {
		l, _, err := r.d.Acquire(ctx, req)
		r.reports <- traceReport{id: id, lease: l, err: err}
	}()
	r.settle()
}

func (r *traceRun) release() {
	if len(r.xu)+len(r.bg) == 0 {
		return
	}
	i := r.rng.Intn(len(r.xu) + len(r.bg))
	if i < len(r.bg) {
		l := r.bg[i]
		r.bg = slices.Delete(r.bg, i, i+1)
		l.Release()
	} else {
		keys := make([]*Lease, 0, len(r.xu))
		for l := range r.xu {
			keys = append(keys, l)
		}
		// Acquire order (enqueue instants strictly increase: one acquire per
		// settled step) — the selection is reproducible per seed.
		slices.SortFunc(keys, func(a, b *Lease) int { return a.enqueued.Compare(b.enqueued) })
		l := keys[i-len(r.bg)]
		tl := r.xu[l]
		delete(r.xu, l)
		v := r.now().Sub(tl.start).Seconds() * float64(tl.weight)
		r.served[traceConsumerKey(tl.consumer)] += v
		r.served["_tenant:"+traceTenant(tl.consumer)] += v
		l.Release()
	}
	r.settle()
}

func (r *traceRun) cancelOne() {
	if len(r.waiting) == 0 {
		return
	}
	ids := make([]int, 0, len(r.waiting))
	for id := range r.waiting {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	r.waiting[ids[r.rng.Intn(len(ids))]].cancel()
	r.settle()
}

// reference is the spec transcription of the pick (design/03 §4.5 no. 2/3,
// d1 and the review-2 hardening F-1), recomputed from the trace's own
// records: T_act, Σ fw, heldBy and the slot time (settled by the trace,
// running from its own admission times) — none of it read from the
// dispatcher's fairness bookkeeping. The fairness-first head is the first in
// order among the heads within the policy (w ≤ S, w ≤ E); it is taken when it
// fits, else the first head in order that fits with its weight plus the
// fairness-first head's weight (review 2's wording; while the first does not
// fit no such head exists — review 3 F3-5 — so this is "nobody", kept in
// this form as an independent statement of the rule) — unless the in-house classes ranked above
// the external class keep that head out on their own (o1: interactive, of
// which the trace has none, so the reservation always binds; o2: background
// too, from the trace's own background leases). It returns the head that
// must be taken and how many heads fit now.
func (r *traceRun) reference(st *targetState, slots int) (*waiter, int) {
	capacity := slots - traceReserved
	if st.external.total == 0 {
		return nil, 0
	}
	now := r.now()
	heldC, heldT := map[string]int{}, map[string]int{}
	slotC, slotT := map[string]float64{}, map[string]float64{}
	activeC := map[int]bool{}
	for _, tl := range r.xu {
		k, tk := traceConsumerKey(tl.consumer), "_tenant:"+traceTenant(tl.consumer)
		heldC[k] += tl.weight
		heldT[tk] += tl.weight
		run := now.Sub(tl.start).Seconds() * float64(tl.weight)
		slotC[k] += run
		slotT[tk] += run
		activeC[tl.consumer] = true
	}
	for key := range st.external.buckets {
		var c int
		if _, err := fmt.Sscanf(strings.TrimPrefix(key, consumerKeyPrefix), "c%d", &c); err != nil {
			r.t.Fatalf("bucket key %q", key)
		}
		activeC[c] = true
	}
	activeT, sumFW := map[string]bool{}, map[string]int{}
	for c := range activeC {
		activeT[traceTenant(c)] = true
		sumFW[traceTenant(c)] += traceFW(c)
	}
	tShare := max(1, (capacity+len(activeT)-1)/len(activeT))
	keyOf := func(key string, h *waiter) refKey {
		var c int
		_, _ = fmt.Sscanf(strings.TrimPrefix(key, consumerKeyPrefix), "c%d", &c)
		tk := "_tenant:" + traceTenant(c)
		cShare := max(1, (tShare*traceFW(c)+sumFW[traceTenant(c)]-1)/sumFW[traceTenant(c)])
		return refKey{
			over:     (heldC[key] > 0 && heldC[key]+h.weight > cShare) || (heldT[tk] > 0 && heldT[tk]+h.weight > tShare),
			tenant:   r.served[tk] + slotT[tk],
			consumer: (r.served[key] + slotC[key]) / float64(traceFW(c)),
			seq:      h.seq,
		}
	}
	fitsW := func(w int) bool { return st.held+w <= slots && st.heldExt+w <= capacity }
	var first *waiter
	var fk refKey
	fits := 0
	for key, b := range st.external.buckets {
		h := b[0]
		if fitsW(h.weight) {
			fits++
		}
		if h.weight > slots || h.weight > capacity {
			continue
		}
		if k := keyOf(key, h); first == nil || refLess(k, fk) {
			first, fk = h, k
		}
	}
	if first == nil || fitsW(first.weight) {
		return first, fits
	}
	reserve := first.weight
	above := st.held - st.heldExt - len(r.bg) // interactive (the trace has none)
	if r.o2 {
		above += len(r.bg) // o2: background ranks above the external class
	}
	if above+first.weight > slots {
		reserve = 0
	} else {
		r.bound++
	}
	var best *waiter
	var bk refKey
	for key, b := range st.external.buckets {
		h := b[0]
		if !fitsW(h.weight + reserve) {
			continue
		}
		if k := keyOf(key, h); best == nil || refLess(k, bk) {
			best, bk = h, k
		}
	}
	return best, fits
}

// probe is the pre-wake check: a policy swap to slots is stored WITHOUT the
// wake (the window between UpdatePolicy's store and its wakeAll), then —
// under the mutex — the head the rank scan checks (headAdmissibleLocked),
// the head the wake takes first (pickAdmissibleLocked; in o1 without
// interactive waiters the external class is the first one the scan asks) and
// the reference must agree, the taken head must fit, and the rest of the wake
// runs as usual.
func (r *traceRun) probe(slots int) {
	r.t.Helper()
	r.d.mu.Lock()
	st := r.d.targetLocked(testOrigin)
	p := r.policy(slots)
	r.d.policy.Store(&p)
	if st.held > slots || st.heldExt > slots-traceReserved {
		r.pending = true // a shrink below the held units drains by attrition
	}
	r.slots = slots
	seen := r.d.headAdmissibleLocked(st, ClassExternal, slots)
	ref, fits := r.reference(st, slots)
	taken := r.d.pickAdmissibleLocked(st, ClassExternal, FairnessFIFO, slots)
	r.probes++
	if fits > 1 {
		r.choices++
	}
	var msg string
	switch {
	case seen != (taken != nil):
		msg = fmt.Sprintf("headAdmissibleLocked = %v, but the wake takes %v — the rank scan checks another head than the wake takes", seen, taken != nil)
	case taken != ref:
		msg = fmt.Sprintf("the wake takes %s, the reference pick is %s (%d heads fit)", waiterLabel(taken), waiterLabel(ref), fits)
	case taken != nil && (st.held+taken.weight > slots || st.heldExt+taken.weight > slots-traceReserved):
		msg = fmt.Sprintf("the taken head (w=%d) does not fit: held %d heldExt %d S %d", taken.weight, st.held, st.heldExt, slots)
	}
	if taken != nil {
		r.decided++
		r.d.handoffLocked(st, taken, true)
	}
	r.d.wakeLocked(st)
	r.d.mu.Unlock()
	if msg != "" {
		r.t.Fatalf("probe %d: %s", r.probes, msg)
	}
	r.settle()
}

func waiterLabel(w *waiter) string {
	if w == nil {
		return "nobody"
	}
	return fmt.Sprintf("%s(w=%d seq=%d)", w.principal.identityKey(), w.weight, w.seq)
}

// check is the quiescent check after every operation: the bookkeeping audit,
// the wake invariant (no external head that fits is left waiting) and the
// bounds (every handoff was admissible) unless a shrink is still draining.
func (r *traceRun) check(step int) {
	r.t.Helper()
	if diff, _ := r.d.fairAuditForTest(testOrigin); diff != "" {
		r.t.Fatalf("step %d: %s", step, diff)
	}
	r.d.mu.Lock()
	st := r.d.targetLocked(testOrigin)
	left, _ := r.d.externalPickLocked(st, r.slots)
	held, heldExt := st.held, st.heldExt
	r.d.mu.Unlock()
	if left != nil {
		r.t.Fatalf("step %d: %s fits and still waits after the wake", step, waiterLabel(left))
	}
	if r.pending && held <= r.slots && heldExt <= r.slots-traceReserved {
		r.pending = false
	}
	if !r.pending && (held > r.slots || heldExt > r.slots-traceReserved) {
		r.t.Fatalf("step %d: held %d heldExt %d past S %d / E %d — an inadmissible head was handed off", step, held, heldExt, r.slots, r.slots-traceReserved)
	}
}

// TestExternalPickConsistencyTrace is gate (h): 6 seeds × 300 random
// operations (external acquires of five consumers in three tenants with
// fair weights 1..3 and weights 1..E, background acquires, releases, caller
// cancels, and pre-wake probes after a policy swap S ∈ 7..12, R = 2) on
// a fake clock in whole seconds (exact slot-time arithmetic). At every probe
// the head the rank scan checks, the head the wake takes and the spec
// reference agree; after every operation the bookkeeping audit is clean, no
// fitting external head waits, and no handoff overran S or E.
func TestExternalPickConsistencyTrace(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		for seed := int64(1); seed <= 6; seed++ {
			t.Run(fmt.Sprintf("%s/%d", order, seed), func(t *testing.T) { externalPickTrace(t, order, seed) })
		}
	}
}

func externalPickTrace(t *testing.T, order string, seed int64) {
	s := DefaultSettings()
	s.RankOrder = mustRankOrder(t, order)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	s.ExternalMaxWait = 10 * time.Minute
	s.BackgroundQueueMax = 0
	r := &traceRun{t: t, rng: rand.New(rand.NewSource(seed)), slots: 7, reports: make(chan traceReport, 1024),
		waiting: map[int]traceWaiter{}, xu: map[*Lease]traceLease{}, served: map[string]float64{}, o2: order == ExternalOrderO2}
	r.d, _ = newTestDispatcher(t, s, r.policy(r.slots))
	r.clk.Store(time.Now().Truncate(time.Second).UnixNano())
	r.d.setFairClockForTest(r.now)
	defer func() {
		for _, w := range r.waiting {
			w.cancel()
		}
		r.settle()
		for l := range r.xu {
			l.Release()
		}
		for _, l := range r.bg {
			l.Release()
		}
	}()
	for step := 0; step < 300; step++ {
		r.clk.Add(int64(r.rng.Intn(4)) * int64(time.Second))
		switch op := r.rng.Intn(20); {
		case op < 7 && len(r.waiting) < 24:
			r.acquire(true)
		case op < 10 && len(r.waiting) < 24:
			r.acquire(false)
		case op < 16:
			r.release()
		case op < 18:
			r.cancelOne()
		default:
			r.probe(7 + r.rng.Intn(6))
		}
		r.check(step)
	}
	t.Logf("%s seed %d: %d probes, %d with ≥ 2 fitting heads, %d decided a handoff, %d with the backfill bound by a fairness-first head that did not fit", order, seed, r.probes, r.choices, r.decided, r.bound)
	if r.choices == 0 {
		t.Fatalf("seed %d: no probe saw two fitting heads — the trace never exercised the pick order", seed)
	}
	if r.bound == 0 {
		t.Fatalf("seed %d: no probe saw a fairness-first head that did not fit and bound the backfill — the trace never exercised F-1 (i)", seed)
	}
}

// TestExternalPickCostScales is the complexity half of gate (i): the pick is
// one pass over the buckets, so its median cost grows linearly from 64 to 512
// buckets (factor 8); a pick quadratic in the buckets grows by 64. Medians
// over 2 000 picks are robust against a descheduled section; the bound 22 is
// the geometric middle of the two growth factors. Every head fits (E = 1 is
// free, every waiter weighs 1), so every pick evaluates every bucket fully —
// share, slot time and order — and returns a head without dequeuing it.
func TestExternalPickCostScales(t *testing.T) {
	med := func(n int) time.Duration {
		s := DefaultSettings()
		s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
		d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.targetLocked(testOrigin)
		for i := 0; i < n; i++ {
			w := &waiter{class: ClassExternal, principal: xuPrincipal(fmt.Sprintf("cost-%d", i), fmt.Sprintf("tenant-%d", i%8)),
				weight: 1, admitCh: make(chan struct{}), cancel: func(error) {}}
			if err := d.enqueueLocked(st, w); err != nil {
				t.Fatalf("enqueue %d: %v", i, err)
			}
		}
		samples := make([]time.Duration, 2000)
		for i := range samples {
			start := time.Now()
			if w, _ := d.externalPickLocked(st, 2); w == nil {
				t.Fatalf("no head picked although every head fits")
			}
			samples[i] = time.Since(start)
		}
		for st.external.total > 0 {
			st.external.pick(FairnessFIFO)
		}
		slices.Sort(samples)
		return samples[len(samples)/2]
	}
	small, large := med(64), med(512)
	ratio := float64(large) / float64(small)
	t.Logf("median pick: %v at 64 buckets, %v at 512 buckets — growth %.1f (linear 8, quadratic 64)", small, large, ratio)
	if ratio >= 22 {
		t.Fatalf("pick cost grows by %.1f from 64 to 512 buckets, want < 22 (one pass over the buckets)", ratio)
	}
	if small >= opWarnThreshold {
		t.Fatalf("median pick %v at 64 buckets, want far below %v", small, opWarnThreshold)
	}
}
