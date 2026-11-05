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
	// The per-class split the rank-order terms read (A03-W9 classHeld):
	// the same state, held by class.
	st.classHeld[ClassInteractive], st.classHeld[ClassBackground], st.classHeld[ClassExternal] = heldI, heldB, heldExt
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
	st.classHeld = [numClasses]int{}

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
//
// Two random streams (A03-W9 hardening H-6, review 2 F2-4): rng is the main
// stream — exactly the draws of the trace before A03-W9 (clock, operation,
// consumer, weight, the release and cancel choice, the probe size), over the
// records it drew them from in the base (waiting, xu, bg: XU and background
// only). xbRng is the XB stream: every XB operation (xbOp: an XB acquire,
// the release of an XB lease, the cancel of an XB waiter) and its draws, on
// its own records (waitingXB, xb), one XB operation after every main step.
// Each seed runs twice from an empty state: the base run with the XB stream
// silent — the main stream then draws the base trace's operation sequence
// on the base trace's state, step for step — and the mixed run with the XB
// stream from step 0. The runs are separate because the classes share one
// state: an XB lease holds units of the partition, an XB waiter or lease
// joins T_act and Σ fw, an XB event re-reads the order — from the first XB
// event on the XU path is no longer the base's, whatever stream XB draws
// from (H-6 measurement: in the mixed run the XU probe statistics scatter
// around the base per seed, their sum stays). agingRng drives the
// interactive demand phases of the aging run (nil outside it).
type traceRun struct {
	t                    *testing.T
	d                    *Dispatcher
	rng, xbRng, agingRng *rand.Rand
	clk                  atomic.Int64
	slots                int
	reports              chan traceReport
	nextID               int
	waiting              map[int]traceWaiter   // the waiting XU and background acquires (main stream)
	waitingXB            map[int]traceWaiter   // the waiting XB acquires (XB stream)
	xu                   map[*Lease]traceLease // the XU leases (main stream)
	xb                   map[*Lease]traceLease // the XB leases (XB stream, A03-W9)
	bg                   []*Lease
	served               map[string]float64 // settled slot time per consumer key and per "_tenant:<id>", both external classes
	pending              bool               // a shrink left held above S or E (attrition) — the bound check waits
	o2                   bool               // dispatch.external_order=o2: background ranks above the external class
	demandDone           func()             // the open interactive demand of the aging run (nil: none open)
	// viewRef is the trace's own clock of the fairness order (review 5
	// FR5-2): the step time of the last operation that, by the documented
	// rule, re-reads the order — a wake or the start or end of an external
	// lease. After every operation the dispatcher's view time must equal it.
	viewRef time.Time

	// probe statistics; bound counts the probes whose fairness-first head
	// did not fit and bound the backfill (review 2 F-1 (i)); for XB (A03-W9)
	// boundXB the same in the XB queue and claimed the probes whose XB pick
	// was bound by the claim of a waiting XU head. escapes is the number of
	// background admissions via the aging escape (the aging run).
	probes, choices, decided, bound int
	boundXB, claimed, decidedXB     int
	escapes                         int64
	admittedXB                      int // XB acquires that got a lease (record)
	// granted is the weight admitted per external class (0 XU, 1 XB) and
	// consumer — the trace's own count of the aging rule (A03-W8a);
	// aged counts the reference keys of aged heads per class (0 XU, 1 XB)
	// — the trace exercised the rule.
	granted [2][traceConsumers]uint64
	aged    [2]int
	// The age base of the aging rule (review 2 H-1), kept from the trace's
	// own lease records: zeroAt is the step time when a consumer last
	// dropped to zero held external units (both classes) and zeroOthers
	// what its classes had admitted to others then. warmP50 is the L_p50 of
	// the lease windows the warm run seeds and freezes (0: cold windows);
	// agedTime counts the reference keys aged by the time threshold alone.
	zeroAt     [traceConsumers]time.Time
	zeroOthers [traceConsumers][2]uint64
	warmP50    time.Duration
	agedTime   int
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
	bulk     bool // an external waiter of XB (A03-W9)
	// base is the weight its class had admitted to other consumers when
	// it arrived and pushAt the step time of its arrival (the aging rule,
	// A03-W8a; external waiters only).
	base   uint64
	pushAt time.Time
}

type traceLease struct {
	consumer, weight int
	start            time.Time
}

// The acquire kinds of the trace.
const (
	traceBackground = iota
	traceXU
	traceXB
)

const (
	traceReserved  = 2
	traceConsumers = 5
	traceSteps     = 300 // main steps per run
)

// The XB stream (H-6): per main step one draw out of traceXBOps —
// traceXBAcquire of them acquire XB (the same consumers and weight
// distribution as XU), traceXBRelease release one XB lease, traceXBCancel
// cancel one XB waiter, the rest do nothing; at most traceXBWaitMax XB
// acquires wait. The seed of the stream is the trace seed plus
// traceXBSeedOffset (the main stream keeps the trace seed). The mix is the
// one of the measured variants (H-6 review 3 F3-1) with the most XB
// admissions per seed that keeps cancels and binds XB by its own head in
// every seed: XB is admitted only beyond the claim of a waiting XU head, and
// the main stream keeps the base trace's full XU load — its XU queue is
// empty after 6..57 of 300 base steps (A03-W9's trace: 28..154) — so a
// denser XB stream mostly adds waiters, not leases.
const (
	traceXBOps        = 40
	traceXBAcquire    = 20
	traceXBRelease    = 16
	traceXBCancel     = 1
	traceXBWaitMax    = 32
	traceXBSeedOffset = 1000
)

// The aging run (H-6): background_aging_after on the real clock (the escape
// reads time.Now(), aging.go agingEscapeLocked) — 1 ns ages every queued
// background head past it by the next wake —, and the interactive demand
// (Dispatcher.InteractiveArrived) open from the start, flipped by one draw
// out of traceDemandFlip per step (seed: the trace seed plus
// traceAgingSeedOffset).
const (
	traceAgingAfter      = time.Nanosecond
	traceDemandFlip      = 8
	traceAgingSeedOffset = 2000
)

// traceBaseFloors are the XU probe statistics of the base trace (the main
// stream without XB): per order and seed {choices, decided, bound}. The XB
// stream must not thin the XU part of the trace below them. Measured at
// A03-W8a after review 2 (age base: the later of the push and the last drop
// to zero held units; identical over 20 runs): the aging rule reorders heads
// of the same draws (the reference agrees at every probe), so the base run
// reaches other statistics than before it — sums o1 49/49/108, o2 51/41/96
// (A03-W8a before review 2: o1 50/48/106, o2 56/44/99, per seed o1 {8,7,20}
// {11,9,19} {8,9,25} {7,7,10} {10,9,17} {6,7,15}, o2 {7,6,19} {12,8,13}
// {8,8,20} {11,7,12} {10,9,17} {8,6,18}) against 53/44/106 and 46/46/88
// of the base trace before A03-W9 (commit 6fd6bea4; review 2 F2-4, log
// subA/logs/trace_base.log: o1 {8,6,21} {11,8,16} {9,9,24} {8,5,12}
// {12,9,17} {5,7,16}, o2 {6,5,19} {9,10,5} {7,8,21} {6,7,10} {9,9,16}
// {9,7,17}).
var traceBaseFloors = map[string][6][3]int{
	ExternalOrderO1: {{8, 6, 21}, {11, 9, 19}, {7, 9, 24}, {6, 8, 13}, {12, 10, 16}, {5, 7, 15}},
	ExternalOrderO2: {{6, 5, 19}, {12, 9, 12}, {7, 8, 21}, {8, 4, 12}, {10, 10, 15}, {8, 5, 17}},
}

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
	aged             bool // A03-W8a: an aged head goes first inside its tenant
	seq              uint64
}

func refLess(a, b refKey) bool {
	if a.over != b.over {
		return b.over
	}
	if a.tenant != b.tenant {
		return a.tenant < b.tenant
	}
	if a.aged != b.aged {
		return a.aged
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
// holds a recorded lease or waits in a queue, every lease the dispatcher
// handed out has reported (review 5 FR5-1: a wake inside an enqueue or a
// drop can admit ANOTHER waiter — the waiting count alone stays equal, the
// late report would book the lease start a step too late), and the acquire
// gone (≥ 0: a canceled one) has ended.
func (r *traceRun) settle() { r.settleGone(-1) }

func (r *traceRun) settleGone(gone int) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.drainReports()
		ts := target(r.t, r.d)
		r.d.mu.Lock()
		leases := len(r.d.targetLocked(testOrigin).inflight)
		r.d.mu.Unlock()
		recorded := len(r.waiting) + len(r.waitingXB)
		if ts.External.Waiting+ts.ExternalBulk.Waiting+ts.Background.Waiting == recorded && leases == len(r.xu)+len(r.xb)+len(r.bg) && !r.isWaiting(gone) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("trace did not settle: %d+%d+%d waiting, %d recorded waiters", ts.External.Waiting, ts.ExternalBulk.Waiting, ts.Background.Waiting, recorded)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// drainReports records every acquire report that has arrived.
func (r *traceRun) drainReports() {
	for {
		select {
		case rep := <-r.reports:
			r.record(rep)
		default:
			return
		}
	}
}

// record books one acquire report into the records of its stream.
func (r *traceRun) record(rep traceReport) {
	w, ok := r.waiting[rep.id]
	if ok {
		delete(r.waiting, rep.id)
	} else {
		w = r.waitingXB[rep.id]
		delete(r.waitingXB, rep.id)
	}
	switch {
	case rep.err != nil:
	case w.bulk:
		r.xb[rep.lease] = traceLease{consumer: w.consumer, weight: w.weight, start: r.now()}
		r.admittedXB++
		r.granted[1][w.consumer] += uint64(w.weight)
	case w.external:
		r.xu[rep.lease] = traceLease{consumer: w.consumer, weight: w.weight, start: r.now()}
		r.granted[0][w.consumer] += uint64(w.weight)
	default:
		r.bg = append(r.bg, rep.lease)
	}
}

// grantedToOthers is the weight the trace saw admitted in the external class
// (XB when bulk) to consumers other than c.
func (r *traceRun) grantedToOthers(bulk bool, c int) uint64 {
	k := 0
	if bulk {
		k = 1
	}
	var n uint64
	for o, g := range r.granted[k] {
		if o != c {
			n += g
		}
	}
	return n
}

// agedRef is the reference's own statement of the aging rule (A03-W8a,
// review 2 H-1; stated independently of fairView.aged, from the trace's own
// records): the head of a consumer — its oldest waiting acquire of the class
// — has aged once, counted from the later of its arrival and the step at
// which the consumer last dropped to zero held units, either the view clock
// moved on by one L_p50 (warm run only) or the units its class admitted to
// OTHER consumers reach the external capacity E (one turnover).
func (r *traceRun) agedRef(bulk bool, c, capacity int, now time.Time) bool {
	waiting := r.waiting
	if bulk {
		waiting = r.waitingXB
	}
	oldest := -1
	for id, tw := range waiting {
		if tw.external && tw.bulk == bulk && tw.consumer == c && (oldest < 0 || id < oldest) {
			oldest = id
		}
	}
	if oldest < 0 || capacity <= 0 {
		return false
	}
	k := 0
	if bulk {
		k = 1
	}
	head := waiting[oldest]
	from, base := head.pushAt, head.base
	if r.zeroAt[c].After(from) {
		from = r.zeroAt[c]
	}
	base = max(base, r.zeroOthers[c][k])
	byCount := r.grantedToOthers(bulk, c)-base >= uint64(capacity)
	byTime := r.warmP50 > 0 && !now.Before(from.Add(r.warmP50))
	if byTime && !byCount {
		r.agedTime++
	}
	return byTime || byCount
}

// isWaiting reports whether the acquire id waits (in either stream).
func (r *traceRun) isWaiting(id int) bool {
	_, main := r.waiting[id]
	_, bulk := r.waitingXB[id]
	return main || bulk
}

// acquire starts one acquire of kind; its consumer and weight come from the
// stream of its kind (XB: the XB stream, else the main stream — the same
// draws as before A03-W9).
func (r *traceRun) acquire(kind int) {
	external, bulk := kind != traceBackground, kind == traceXB
	rng := r.rng
	if bulk {
		rng = r.xbRng
	}
	c := rng.Intn(traceConsumers)
	w := 1 + rng.Intn(2) // mostly light, so a probe sees several heads that fit
	if rng.Intn(10) < 3 {
		w = 1 + rng.Intn(r.slots-traceReserved)
	}
	id := r.nextID
	r.nextID++
	ctx, cancel := context.WithCancel(context.Background())
	req := backgroundReq()
	if external {
		ctx = withPrincipal(ctx, Principal{ConsumerID: fmt.Sprintf("c%d", c), TenantID: traceTenant(c), ConsumerKind: "app", FairWeight: traceFW(c)})
		req = weightedReq(w)
		if bulk {
			req.Class = ClassExternalBulk
		}
	}
	xuWaiting, xbWaiting := r.externalWaiting()
	tw := traceWaiter{cancel: cancel, consumer: c, weight: w, external: external, bulk: bulk, base: r.grantedToOthers(bulk, c), pushAt: r.now()}
	if bulk {
		r.waitingXB[id] = tw
	} else {
		r.waiting[id] = tw
	}
	go func() {
		l, _, err := r.d.Acquire(ctx, req)
		r.reports <- traceReport{id: id, lease: l, err: err}
	}()
	r.settle()
	queued := r.isWaiting(id)
	switch {
	case external:
		r.viewRef = r.now() // an external lease starts, or the enqueue wakes
	case !queued && (xbWaiting > 0 || r.o2 && xuWaiting > 0):
		// A background admission wakes while the heads of an external class
		// ranked below background wait: XB in both orders, XU in o2.
		r.viewRef = r.now()
	}
}

// externalWaiting is the number of XU and XB acquires the trace has queued.
func (r *traceRun) externalWaiting() (xu, xb int) {
	for _, w := range r.waiting {
		if w.external {
			xu++
		}
	}
	return xu, len(r.waitingXB)
}

// sortedLeases is the leases of m in acquire order (enqueue instants
// strictly increase: one acquire per settled step) — a selection by index
// is reproducible per seed.
func sortedLeases(m map[*Lease]traceLease) []*Lease {
	keys := make([]*Lease, 0, len(m))
	for l := range m {
		keys = append(keys, l)
	}
	slices.SortFunc(keys, func(a, b *Lease) int { return a.enqueued.Compare(b.enqueued) })
	return keys
}

// endExternal releases the external lease l and settles its slot time in
// the trace's own record (consumer and tenant; one record set for both
// external classes).
func (r *traceRun) endExternal(l *Lease, tl traceLease) {
	v := r.now().Sub(tl.start).Seconds() * float64(tl.weight)
	r.served[traceConsumerKey(tl.consumer)] += v
	r.served["_tenant:"+traceTenant(tl.consumer)] += v
	// The caller dropped tl from its map: does the consumer hold nothing
	// now? Then its heads age from here (the age base of the reference).
	holds := false
	for _, leases := range []map[*Lease]traceLease{r.xu, r.xb} {
		for _, o := range leases {
			holds = holds || o.consumer == tl.consumer
		}
	}
	if !holds {
		r.zeroAt[tl.consumer] = r.now()
		r.zeroOthers[tl.consumer] = [2]uint64{r.grantedToOthers(false, tl.consumer), r.grantedToOthers(true, tl.consumer)}
	}
	l.Release()
}

// release ends one lease of the main stream (XU or background).
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
		l := sortedLeases(r.xu)[i-len(r.bg)]
		tl := r.xu[l]
		delete(r.xu, l)
		r.endExternal(l, tl)
	}
	r.settle()
	r.viewRef = r.now() // every release wakes
}

// releaseXB ends one XB lease (XB stream).
func (r *traceRun) releaseXB() {
	if len(r.xb) == 0 {
		return
	}
	l := sortedLeases(r.xb)[r.xbRng.Intn(len(r.xb))]
	tl := r.xb[l]
	delete(r.xb, l)
	r.endExternal(l, tl)
	r.settle()
	r.viewRef = r.now() // every release wakes
}

// cancelOne cancels one waiting acquire of the main stream.
func (r *traceRun) cancelOne() {
	if len(r.waiting) == 0 {
		return
	}
	id := sortedIDs(r.waiting)[r.rng.Intn(len(r.waiting))]
	w := r.waiting[id]
	w.cancel()
	r.settleGone(id)
	if w.external {
		r.viewRef = r.now() // the drop of an external waiter wakes (a background drop does not)
	}
}

// cancelXB cancels one waiting XB acquire (XB stream).
func (r *traceRun) cancelXB() {
	if len(r.waitingXB) == 0 {
		return
	}
	id := sortedIDs(r.waitingXB)[r.xbRng.Intn(len(r.waitingXB))]
	r.waitingXB[id].cancel()
	r.settleGone(id)
	r.viewRef = r.now() // the drop of an external waiter wakes
}

func sortedIDs(m map[int]traceWaiter) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// mainOp is one operation of the main stream — the operation mix of the
// trace before A03-W9, with the same draws.
func (r *traceRun) mainOp() {
	switch op := r.rng.Intn(20); {
	case op < 7 && len(r.waiting) < 24:
		r.acquire(traceXU)
	case op < 10 && len(r.waiting) < 24:
		r.acquire(traceBackground)
	case op < 16:
		r.release()
	case op < 18:
		r.cancelOne()
	default:
		r.probe(7 + r.rng.Intn(6))
	}
}

// xbOp is one operation of the XB stream (between two main steps).
func (r *traceRun) xbOp() {
	switch op := r.xbRng.Intn(traceXBOps); {
	case op < traceXBAcquire:
		if len(r.waitingXB) < traceXBWaitMax {
			r.acquire(traceXB)
		}
	case op < traceXBAcquire+traceXBRelease:
		r.releaseXB()
	case op < traceXBAcquire+traceXBRelease+traceXBCancel:
		r.cancelXB()
	}
}

// demandOp is one step of the demand phases of the aging run: a flip opens
// the interactive demand (0 → 1 wakes nothing: the herald term only shuts
// background out) or closes it (→ 0 wakes every target, InteractiveArrived's
// done — the order is re-read).
func (r *traceRun) demandOp() {
	if r.agingRng.Intn(traceDemandFlip) != 0 {
		return
	}
	if r.demandDone == nil {
		r.demandDone = r.d.InteractiveArrived()
		return
	}
	r.demandDone()
	r.demandDone = nil
	r.settle()
	r.viewRef = r.now()
}

// reference is the spec transcription of the pick (design/03 §4.5 no. 2/3,
// d1 and the review-2 hardening F-1) for the external class c, recomputed
// from the trace's own records: T_act, Σ fw, heldBy and the slot time over
// BOTH external classes (A03-W9: one record set — settled by the trace,
// running from its own admission times) — none of it read from the
// dispatcher's fairness bookkeeping. The fairness-first head is the first in
// order among the heads within the policy (w ≤ S, w ≤ E); it is taken when it
// fits, else the first head in order that fits with its weight plus the
// fairness-first head's weight (review 2's wording; while the first does not
// fit no such head exists — review 3 F3-5 — so this is "nobody", kept in
// this form as an independent statement of the rule) — unless the classes
// ranked above c keep that head out on their own (XU: o1 interactive, of
// which the trace has none, so the reservation always binds; o2 background
// too, from the trace's own background leases; XB: background and the units
// XU holds, in both orders). For XB every candidate fits only beyond the
// claim of XU (refClaim). It returns the head that must be taken and how
// many heads fit now.
func (r *traceRun) reference(st *targetState, slots int, c Class) (*waiter, int) {
	capacity := slots - traceReserved
	q := st.extQueue(c)
	if q.total == 0 {
		return nil, 0
	}
	keyOf := r.refOrder(st, slots)
	claim := 0
	if c == ClassExternalBulk {
		claim = r.refClaim(st, slots, keyOf)
	}
	fitsW := func(w int) bool { return st.held+w+claim <= slots && st.heldExt+w+claim <= capacity }
	first, fits := refFirst(q, slots, capacity, keyOf, fitsW)
	if first == nil || fitsW(first.weight) {
		return first, fits
	}
	reserve := first.weight
	aboveIn, aboveExt := 0, 0
	switch {
	case c == ClassExternalBulk:
		aboveIn, aboveExt = len(r.bg), r.heldXU() // background and XU rank above XB in o1 and o2
	case r.o2:
		aboveIn = len(r.bg) // o2: background ranks above XU
	}
	if aboveIn+aboveExt+first.weight > slots || aboveExt+first.weight > capacity {
		reserve = 0
	} else if c == ClassExternalBulk {
		r.boundXB++
	} else {
		r.bound++
	}
	var best *waiter
	var bk refKey
	for key, b := range q.buckets {
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

// refOrder is the reference's fairness order over both external classes:
// the order key of a bucket head, from the trace's own records.
func (r *traceRun) refOrder(st *targetState, slots int) func(string, *waiter) refKey {
	capacity := slots - traceReserved
	// The clock of the order is the trace's own view clock (review 4 FR4-2:
	// frozen between events; review 5 FR5-2: kept by the trace from the
	// documented rule, check compares it with the dispatcher's).
	now := r.viewRef
	if now.IsZero() {
		now = r.now() // no event yet: the dispatcher reads the clock
	}
	heldC, heldT := map[string]int{}, map[string]int{}
	slotC, slotT := map[string]float64{}, map[string]float64{}
	activeC := map[int]bool{}
	for _, leases := range []map[*Lease]traceLease{r.xu, r.xb} {
		for _, tl := range leases {
			k, tk := traceConsumerKey(tl.consumer), "_tenant:"+traceTenant(tl.consumer)
			heldC[k] += tl.weight
			heldT[tk] += tl.weight
			run := now.Sub(tl.start).Seconds() * float64(tl.weight)
			slotC[k] += run
			slotT[tk] += run
			activeC[tl.consumer] = true
		}
	}
	for _, q := range []*classQueue{st.external, st.externalBulk} {
		for key := range q.buckets {
			activeC[traceConsumerOf(r.t, key)] = true
		}
	}
	activeT, sumFW := map[string]bool{}, map[string]int{}
	for c := range activeC {
		activeT[traceTenant(c)] = true
		sumFW[traceTenant(c)] += traceFW(c)
	}
	tShare := max(1, (capacity+len(activeT)-1)/max(len(activeT), 1))
	for _, k := range []Class{ClassExternal, ClassExternalBulk} {
		if lv := st.externalLeaseView(k); lv.warm != (r.warmP50 > 0) || (lv.warm && lv.p50 != r.warmP50) {
			r.t.Fatalf("%s lease window warm %v (p50 %v), the trace seeded %v — the reference's time threshold would not match", k, lv.warm, lv.p50, r.warmP50)
		}
	}
	return func(key string, h *waiter) refKey {
		c := traceConsumerOf(r.t, key)
		tk := "_tenant:" + traceTenant(c)
		cShare := max(1, (tShare*traceFW(c)+sumFW[traceTenant(c)]-1)/sumFW[traceTenant(c)])
		over := (heldC[key] > 0 && heldC[key]+h.weight > cShare) || (heldT[tk] > 0 && heldT[tk]+h.weight > tShare)
		// A03-W8a: the head of a consumer holding nothing ages after one
		// turnover; it then weighs one unit in its tenant's share and goes
		// first inside its tenant.
		bulk := h.class == ClassExternalBulk
		aged := heldC[key] == 0 && r.agedRef(bulk, c, capacity, now)
		if aged {
			over = heldT[tk] > 0 && heldT[tk]+1 > tShare
			if bulk {
				r.aged[1]++
			} else {
				r.aged[0]++
			}
		}
		return refKey{
			over:     over,
			tenant:   r.served[tk] + slotT[tk],
			consumer: (r.served[key] + slotC[key]) / float64(traceFW(c)),
			aged:     aged,
			seq:      h.seq,
		}
	}
}

// refFirst is the reference's fairness-first head of q among the heads
// within the policy, and how many heads fit now (fitsW).
func refFirst(q *classQueue, slots, capacity int, keyOf func(string, *waiter) refKey, fitsW func(int) bool) (*waiter, int) {
	var first *waiter
	var fk refKey
	fits := 0
	for key, b := range q.buckets {
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
	return first, fits
}

// refClaim is the reference's claim of XU on XB (A03-W9, written out): the
// weight of XU's head the freed units serve first — its fairness-first head
// while it fits or holds (nothing above XU keeps it out: o1 no interactive,
// o2 background), else its lightest head — unless the classes ranked above
// XU keep that head out on their own; 0 without an XU waiter.
func (r *traceRun) refClaim(st *targetState, slots int, keyOf func(string, *waiter) refKey) int {
	capacity := slots - traceReserved
	q := st.external
	if q.total == 0 {
		return 0
	}
	plain := func(w int) bool { return st.held+w <= slots && st.heldExt+w <= capacity }
	first, _ := refFirst(q, slots, capacity, keyOf, plain)
	if first == nil {
		return 0
	}
	above := 0
	if r.o2 {
		above = len(r.bg)
	}
	need := first.weight
	if !plain(first.weight) && above+first.weight > slots {
		need = 0
		for _, b := range q.buckets {
			if need == 0 || b[0].weight < need {
				need = b[0].weight
			}
		}
	}
	if above+need > slots || need > capacity {
		return 0
	}
	return need
}

// heldXU is the weight the trace's XU leases hold.
func (r *traceRun) heldXU() int {
	n := 0
	for _, tl := range r.xu {
		n += tl.weight
	}
	return n
}

// traceConsumerOf parses the consumer number out of a bucket key.
func traceConsumerOf(t *testing.T, key string) int {
	var c int
	if _, err := fmt.Sscanf(strings.TrimPrefix(key, consumerKeyPrefix), "c%d", &c); err != nil {
		t.Fatalf("bucket key %q", key)
	}
	return c
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
	ref, fits := r.reference(st, slots, ClassExternal)
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
		// The XB half reads the state after this handoff: the trace records
		// the new XU lease now (its report, settled after the probe, writes
		// the same record again), and the lease start re-reads the order —
		// the documented rule (fairness.go extFair.leaseEnter: touch(start)),
		// so the XB half reads it at this step (H-6: a reference left at the
		// last event's instant mis-ranks XU's heads and with them the claim).
		r.xu[taken.lease] = traceLease{consumer: traceConsumerOf(r.t, taken.principal.identityKey()), weight: taken.weight, start: r.now()}
		r.viewRef = r.now()
	}
	if msg == "" {
		msg = r.probeBulkLocked(st, slots)
	}
	r.d.wakeLocked(st)
	r.d.mu.Unlock()
	r.viewRef = r.now() // the probe's wake
	if msg != "" {
		r.t.Fatalf("probe %d: %s", r.probes, msg)
	}
	r.settle()
}

// probeBulkLocked is the XB half of the probe (A03-W9), on the state the XU
// handoff left: the head the rank scan checks, XB's pick (not dequeued — the
// wake that follows serves the classes in rank order) and the reference must
// agree, and the pick must fit together with XU's claim. "" when they do.
func (r *traceRun) probeBulkLocked(st *targetState, slots int) string {
	seen := r.d.headAdmissibleLocked(st, ClassExternalBulk, slots)
	ref, _ := r.reference(st, slots, ClassExternalBulk)
	pick, _ := r.d.externalPickLocked(st, ClassExternalBulk, slots)
	if st.externalBulk.total > 0 && r.refClaim(st, slots, r.refOrder(st, slots)) > 0 {
		r.claimed++
	}
	switch {
	case seen != (pick != nil):
		return fmt.Sprintf("XB: headAdmissibleLocked = %v, but the pick is %v", seen, waiterLabel(pick))
	case pick != ref:
		return fmt.Sprintf("XB: the pick is %s, the reference pick is %s", waiterLabel(pick), waiterLabel(ref))
	case pick != nil && (st.held+pick.weight > slots || st.heldExt+pick.weight > slots-traceReserved):
		return fmt.Sprintf("XB: the pick (w=%d) does not fit: held %d heldExt %d S %d", pick.weight, st.held, st.heldExt, slots)
	}
	if pick != nil {
		r.decidedXB++
	}
	return ""
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
	left, _ := r.d.externalPickLocked(st, ClassExternal, r.slots)
	leftXB, _ := r.d.externalPickLocked(st, ClassExternalBulk, r.slots)
	// Frozen between events (review 4 FR4-2): the clock alone never changes
	// the pick — 1000 s later, under the same mutex section, it is the same.
	r.clk.Add(int64(1000 * time.Second))
	later, _ := r.d.externalPickLocked(st, ClassExternal, r.slots)
	laterXB, _ := r.d.externalPickLocked(st, ClassExternalBulk, r.slots)
	r.clk.Add(-int64(1000 * time.Second))
	held, heldExt := st.held, st.heldExt
	viewAt := st.external.fair.viewAt
	r.d.mu.Unlock()
	if later != left || laterXB != leftXB {
		r.t.Fatalf("step %d: the picks are %s/%s now and %s/%s 1000 s later without an event — the order moved with the clock",
			step, waiterLabel(left), waiterLabel(leftXB), waiterLabel(later), waiterLabel(laterXB))
	}
	if !viewAt.Equal(r.viewRef) {
		r.t.Fatalf("step %d: the dispatcher's fairness view is at %v, the trace's rule says %v — the order's clock moved without its event (or missed one)", step, viewAt.UnixNano(), r.viewRef.UnixNano())
	}
	if left != nil || leftXB != nil {
		r.t.Fatalf("step %d: %s / %s (XU / XB) fits and still waits after the wake", step, waiterLabel(left), waiterLabel(leftXB))
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
// cancels, and pre-wake probes after a policy swap S ∈ 7..12, R = 2) on a
// fake clock in whole seconds (exact slot-time arithmetic), each seed in two
// runs: base — the trace before A03-W9 draw for draw — and xb — the same
// main stream plus one operation of the XB stream (A03-W9; H-6: its own
// random stream) after every main step: XB acquires of the same consumers,
// XB releases and cancels. At every probe the head the rank scan checks,
// the head the wake takes and the spec reference agree — for XU, and for XB
// on the state the XU handoff left (its own order over the shared records
// and XU's claim, written out in the reference); after every operation the
// bookkeeping audit is clean, no fitting head of either external class
// waits, and no handoff overran S or E. Floors per seed: the base run
// reaches the base trace's XU probe statistics (traceBaseFloors); the mixed
// run exercises every XU probe kind, binds XB by its own head and by the XU
// claim, and admits XB at least traceXBAdmitFloors times.
func TestExternalPickConsistencyTrace(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		for seed := int64(1); seed <= 6; seed++ {
			t.Run(fmt.Sprintf("%s/%d/base", order, seed), func(t *testing.T) {
				externalPickTrace(t, order, seed, traceMode{}).baseFloors(order, seed)
			})
			t.Run(fmt.Sprintf("%s/%d/xb", order, seed), func(t *testing.T) {
				externalPickTrace(t, order, seed, traceMode{xb: true}).mixedFloors(order, seed)
			})
		}
	}
	// The aging rule in the XB queue: since its age counts from the last
	// drop to zero held units (review 2 H-1), the cold mixed runs barely
	// reach it (XB heads mostly wait behind the XU claim, their consumers
	// hold units); the warm run exercises it in every seed
	// (TestExternalPickConsistencyTraceWarm, aged XB keys per seed).
}

// traceXBAdmitFloors are the XB admissions per order and seed the mixed run
// must reach — XB leases: heldBy and slot time over both classes,
// releaseXB, heldExt through XB (review 3 F3-1). The target is the trace at
// A03-W9 (commit d0ec4399, a third of the external acquires XB on the main
// stream): o1 11/18/3/17/20/14, o2 15/17/3/16/20/14. The mixed run reaches
// it for seed 1 in both orders (floor = the d0ec4399 number); for the other
// seeds the floor is the measured count of the mixed run (the XU claim
// under the base trace's XU load caps it, see the XB stream mix) — an open
// point of H-6, not the target. A03-W8a: the aging rule reorders XU heads of
// the same draws, the mixed run admits o1 13/16/2/11/9/9 and o2
// 26/16/2/11/10/6 — o2 seed 6 re-measured (8 at A03-W9), the sums (o1 60,
// o2 71) stay above the A03-W9 floors (58, 61); unchanged by the age base of
// review 2 (identical over 20 runs).
var traceXBAdmitFloors = map[string][6]int{
	ExternalOrderO1: {11, 16, 2, 11, 9, 9},
	ExternalOrderO2: {15, 16, 2, 11, 9, 6},
}

// baseFloors: the base run reaches the base trace's XU probe statistics
// (traceBaseFloors — the same draw sequence, so a shortfall means the trace
// no longer runs the base's XU part).
func (r *traceRun) baseFloors(order string, seed int64) {
	r.t.Helper()
	base := traceBaseFloors[order][seed-1]
	if r.choices < base[0] || r.decided < base[1] || r.bound < base[2] {
		r.t.Fatalf("seed %d: base run XU probes %d/%d/%d (≥ 2 fitting heads / handoffs / bound backfill), base trace %d/%d/%d — the trace thinned the XU part",
			seed, r.choices, r.decided, r.bound, base[0], base[1], base[2])
	}
	if r.aged[0] == 0 {
		r.t.Fatalf("seed %d: no XU reference key of an aged head — the trace never exercised the aging rule (A03-W8a)", seed)
	}
}

// mixedFloors: the mixed run exercises the XU pick next to XB (every probe
// kind), binds XB by its own head and by the XU claim, and admits XB at
// least traceXBAdmitFloors times.
func (r *traceRun) mixedFloors(order string, seed int64) {
	r.t.Helper()
	if r.choices == 0 || r.decided == 0 || r.bound == 0 {
		r.t.Fatalf("seed %d: mixed run XU probes %d/%d/%d — the trace never exercised the XU pick next to XB", seed, r.choices, r.decided, r.bound)
	}
	if r.boundXB == 0 || r.claimed == 0 {
		r.t.Fatalf("seed %d: XB never bound by its own head (%d) or by the XU claim (%d) — the trace never exercised the XB pick", seed, r.boundXB, r.claimed)
	}
	if want := traceXBAdmitFloors[order][seed-1]; r.admittedXB < want {
		r.t.Fatalf("seed %d: %d XB admissions, floor %d — XB leases (heldBy, slot time, heldExt over both classes) thinned out", seed, r.admittedXB, want)
	}
	if r.aged[0] == 0 {
		r.t.Fatalf("seed %d: no XU reference key of an aged head next to XB — the trace never exercised the aging rule (A03-W8a)", seed)
	}
}

// TestExternalPickConsistencyTraceAging is the mixed run of gate (h) with
// the aging escape armed (H-6): background_aging_after = traceAgingAfter
// and the interactive demand in phases (demandOp), so while the demand is
// open background is admitted past the herald term only via the escape
// (rank.go backgroundEscapesLocked, taken by the wake's rank scan in
// pickAdmissibleLocked) — while every check of the trace runs unchanged.
// Escape admissions happen only inside wakes, which the trace's view rule
// already books; demandOp books the one new wake (demand → 0). Per seed at
// least one escape admission. The newcomer half of the escape
// (headAdmissibleLocked) is pinned by the deterministic escape gates
// (TestEscapeBeforeExternalUnderO2, TestExternalBulkEscapeBeforeXB and the
// yield gates), not by this trace.
func TestExternalPickConsistencyTraceAging(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		for seed := int64(1); seed <= 6; seed++ {
			t.Run(fmt.Sprintf("%s/%d", order, seed), func(t *testing.T) {
				r := externalPickTrace(t, order, seed, traceMode{xb: true, aging: traceAgingAfter})
				if r.escapes == 0 {
					t.Fatalf("seed %d: no background admission via the aging escape — the trace never exercised the escape", seed)
				}
			})
		}
	}
}

// traceWarmP50 is the L_p50 the warm run seeds into both external lease
// windows: the trace's clock moves 0–3 s per main step, so heads that wait a
// few steps age by the time threshold (A03-W8a review 2 H-3).
const traceWarmP50 = 4 * time.Second

// traceWarmFloors are the probe statistics of the warm run per order and
// seed — {choices, decided, bound, reference keys aged by the time threshold
// alone} — measured identical over 20 runs (the run is deterministic: fake
// clock, frozen windows); the floor is the measured value.
var traceWarmFloors = map[string][6][4]int{
	ExternalOrderO1: {{11, 10, 17, 95}, {12, 9, 17, 92}, {9, 7, 25, 119}, {7, 7, 12, 100}, {12, 7, 19, 109}, {4, 3, 20, 116}},
	ExternalOrderO2: {{8, 5, 19, 64}, {11, 7, 7, 152}, {8, 8, 21, 165}, {8, 7, 9, 88}, {10, 9, 15, 107}, {8, 4, 20, 120}},
}

// TestExternalPickConsistencyTraceWarm is the mixed run of gate (h) on warm
// lease windows (review 2 H-3: the cold runs exclude the time threshold of
// the aging rule by construction): both external windows seeded with
// L_p50 = traceWarmP50 and frozen; the reference states the time threshold
// from its own records (arrival or the last drop to zero held units, plus
// one L_p50 on the trace's view clock). Every check of the trace runs
// unchanged; per seed the rule is exercised, and the floors pin the probe
// statistics and the keys aged by the time threshold alone.
func TestExternalPickConsistencyTraceWarm(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		for seed := int64(1); seed <= 6; seed++ {
			t.Run(fmt.Sprintf("%s/%d", order, seed), func(t *testing.T) {
				r := externalPickTrace(t, order, seed, traceMode{xb: true, warm: traceWarmP50})
				want := traceWarmFloors[order][seed-1]
				if r.choices < want[0] || r.decided < want[1] || r.bound < want[2] || r.agedTime < want[3] {
					t.Fatalf("seed %d: warm run probes %d/%d/%d, keys aged by time %d; floors %d/%d/%d/%d", seed, r.choices, r.decided, r.bound, r.agedTime, want[0], want[1], want[2], want[3])
				}
				if r.agedTime == 0 || r.aged[1] == 0 {
					t.Fatalf("seed %d: reference keys aged by the time threshold alone %d, aged XB keys %d — the warm run never exercised the time threshold or the XB queue", seed, r.agedTime, r.aged[1])
				}
			})
		}
	}
}

// traceMode is the variant of one trace run: xb runs the XB stream, aging
// > 0 is the aging run.
type traceMode struct {
	xb    bool
	aging time.Duration
	// warm > 0 seeds both external lease windows with L_p50 = warm before
	// the run (frozen: the loops are closed) — the time threshold of the
	// aging rule (A03-W8a review 2 H-3).
	warm time.Duration
}

// externalPickTrace runs one trace and returns it for the floors.
func externalPickTrace(t *testing.T, order string, seed int64, mode traceMode) *traceRun {
	s := DefaultSettings()
	s.RankOrder = mustRankOrder(t, order)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	s.ExternalMaxWait = 10 * time.Minute
	s.BackgroundQueueMax = 0
	s.BackgroundAgingAfter = mode.aging
	r := &traceRun{t: t, rng: rand.New(rand.NewSource(seed)), xbRng: rand.New(rand.NewSource(seed + traceXBSeedOffset)),
		slots: 7, reports: make(chan traceReport, 1024), waiting: map[int]traceWaiter{}, waitingXB: map[int]traceWaiter{},
		xu: map[*Lease]traceLease{}, xb: map[*Lease]traceLease{}, served: map[string]float64{}, o2: order == ExternalOrderO2}
	r.d, _ = newTestDispatcher(t, s, r.policy(r.slots))
	if mode.warm > 0 {
		r.d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 4*leaseColdSamples, mode.warm)
		r.d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 4*leaseColdSamples, mode.warm)
		r.warmP50 = mode.warm
	}
	r.clk.Store(time.Now().Truncate(time.Second).UnixNano())
	r.d.setFairClockForTest(r.now)
	// Close stops only the loops (reaper, publish); Acquire and Release do
	// not use them. With the escape armed the reaper tick wakes every target
	// (dispatcher.go reapNow) — asynchronously, on the real clock, outside
	// the trace's event rule; and the publish loop would change the lease
	// windows on the real clock mid-run, arming or moving the time threshold
	// of the aging rule (A03-W8a) asynchronously — the trace runs on cold
	// windows, the warm run on the windows it seeded.
	r.d.Close()
	if mode.aging > 0 {
		r.agingRng = rand.New(rand.NewSource(seed + traceAgingSeedOffset))
		r.demandDone = r.d.InteractiveArrived()
	}
	defer r.cleanup()
	for step := 0; step < traceSteps; step++ {
		r.clk.Add(int64(r.rng.Intn(4)) * int64(time.Second))
		r.mainOp()
		r.check(step)
		if mode.xb {
			r.xbOp()
			r.check(step)
		}
		if r.agingRng != nil {
			r.demandOp()
			r.check(step)
		}
	}
	r.d.mu.Lock()
	r.escapes = r.d.targetLocked(testOrigin).preempt.agedAdmits
	r.d.mu.Unlock()
	t.Logf("%s seed %d xb=%v: %d probes, %d with ≥ 2 fitting heads, %d decided a handoff, %d with the backfill bound by a fairness-first head that did not fit; XB: %d admissions, %d picks, %d bound by its own head, %d bound by the XU claim; %d aging escapes; reference keys of an aged head (A03-W8a) XU %d, XB %d, by the time threshold alone %d",
		order, seed, mode.xb, r.probes, r.choices, r.decided, r.bound, r.admittedXB, r.decidedXB, r.boundXB, r.claimed, r.escapes, r.aged[0], r.aged[1], r.agedTime)
	return r
}

// cleanup ends every waiter and lease of the trace (and its open demand).
func (r *traceRun) cleanup() {
	for _, m := range []map[int]traceWaiter{r.waiting, r.waitingXB} {
		for _, w := range m {
			w.cancel()
		}
	}
	r.settle()
	if r.demandDone != nil {
		r.demandDone()
	}
	for _, m := range []map[*Lease]traceLease{r.xu, r.xb} {
		for l := range m {
			l.Release()
		}
	}
	for _, l := range r.bg {
		l.Release()
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
			if w, _ := d.externalPickLocked(st, ClassExternal, 2); w == nil {
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
