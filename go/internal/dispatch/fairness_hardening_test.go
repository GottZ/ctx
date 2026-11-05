package dispatch

// Hardening gates of A03-W4 (review 2, F-1/F-2/F-3/F-4), all through the
// stock API — they compile against the tree before the hardening, which is
// their red run:
//   - F-1: a heavy head of another tenant (or of the same tenant, within its
//     tenant share) under a stream of light leases of another consumer is
//     admitted within one light hold time — while the fairness-first head
//     does not fit it holds the class (no backfill), and a consumer or
//     tenant that holds nothing is within its share; a same-tenant head with
//     w + (s − 1) > tenantShare next to a busy consumer of share s is the
//     named limit (pinned, it still starves);
//   - d2 follows the head the freed units serve first (the fairness-first
//     head, not the lightest one);
//   - the backfill reservation never binds while the in-house classes
//     ranked above the external class alone keep the head out — interactive,
//     and background in o2 (the F-2 principle applied to F-1 (i));
//   - F-2: d2 reserves only while interactive alone does not keep the head
//     out (the same bound as the backfill reservation, review 3 F3-1);
//   - F-3: a continuation that does not fit blocks the bucket heads, the
//     newcomer and background (o1);
//   - F-4: the slot-time order holds on the production clock (no seam).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lightStream runs n clients of one consumer that acquire w = 1, hold for
// hold and acquire again at once; stop ends them and returns the number of
// admissions.
func lightStream(d *Dispatcher, n int, hold time.Duration, consumer, tenant string) (stop func() int64) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var served atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req := weightedReq(1)
				req.MaxWait = 5 * time.Second
				l, _, err := d.Acquire(xctx(ctx, consumer, tenant), req)
				if err != nil {
					continue
				}
				served.Add(1)
				select {
				case <-time.After(hold):
				case <-ctx.Done():
				}
				l.Release()
			}
		}()
	}
	return func() int64 {
		cancel()
		wg.Wait()
		return served.Load()
	}
}

// heavyUnderLightStream starts the light stream of consumer "light"
// (tenant-a), lets it run 110 ms, then times one acquire of weight w of
// consumer "heavy" of tenant with the wait budget maxWait.
func heavyUnderLightStream(t *testing.T, clients, w int, tenant string, maxWait time.Duration) (took time.Duration, served int64, err error) {
	t.Helper()
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	stop := lightStream(d, clients, headGateHold, "light", "tenant-a")
	// 110 ms: off the 20-ms cycle of the light leases (admitted together at
	// the start), so the head arrives while they run and has to wait for
	// their units.
	time.Sleep(110 * time.Millisecond)
	req := weightedReq(w)
	req.MaxWait = maxWait
	l, took, err := timedAcquire(d, "heavy", tenant, req)
	if l != nil {
		l.Release()
	}
	return took, stop(), err
}

// TestExternalHeavyHeadUnderLightStream is the F-1 gate (review 2 P8):
// S = 7, R = 2 (E = 5), no background; n light clients of consumer "light"
// (tenant-a) hold 20 ms and ask again at once; 110 ms later one heavy head
// arrives. Before the hardening the head is passed by every freed unit (it
// never fits while a light lease runs, d1) and ends with ErrWaitExpired;
// with it the head is the fairness-first one (its tenant has no slot time;
// it holds nothing, so it is within its share) and holds the class: the
// light stream takes no unit until it fits — it is admitted within one
// light hold time
// (+ 50 ms). Cases: another tenant at w = E with 3 and 6 light clients, at
// w = 3 (= tenantShare with T_act = 2) and the same tenant at w = 3 (within
// tenantShare 5). The w = 2 case is a regression check (green before too).
func TestExternalHeavyHeadUnderLightStream(t *testing.T) {
	for _, tc := range []struct {
		clients, w int
		tenant     string
	}{
		{3, weightE, "tenant-b"},
		{6, weightE, "tenant-b"},
		{6, 3, "tenant-b"},
		{6, 3, "tenant-a"},
		{4, 2, "tenant-b"},
	} {
		t.Run(fmt.Sprintf("light%d_w%d_%s", tc.clients, tc.w, tc.tenant), func(t *testing.T) {
			took, served, err := heavyUnderLightStream(t, tc.clients, tc.w, tc.tenant, 500*time.Millisecond)
			t.Logf("heavy w=%d (%s) under %d light clients of tenant-a: err=%v after %v (bound %v; light leases served %d)",
				tc.w, tc.tenant, tc.clients, err, took, headGateHold+headGateSlack, served)
			if err != nil || took > headGateHold+headGateSlack {
				t.Fatalf("heavy w=%d: err=%v after %v, want admitted within one light hold time + %v — the light stream backfills the units the fairness-first head needs", tc.w, err, took, headGateSlack)
			}
		})
	}
}

// TestExternalHeavyHeadSameTenantNamedLimit pins the named limit of F-1
// (review 2; review 3 F3-2, F3-6; handoff "aging rule before A03-W8") as a
// scripted, deterministic sequence on the fake clock: a heavy head of weight
// w of the SAME tenant as a busy consumer c with consumer share s stays over
// its tenant share while w + (s − 1) exceeds the tenant share — c ranks
// first up to s − 1 held units and keeps the tenant at s − 1 or more. One
// active tenant, E = 5 (tenantShare 5), equal fair weights (s = ⌈5·10/20⌉ =
// 3): c holds 2, the head queues, c's third request ranks before it; then
// six times c queues one more request and one of its leases ends — every
// freed unit goes to c, the head (w = 5 and w = 4) keeps waiting. Control:
// w = 3 (3 + 2 ≤ 5) is admitted on arrival. This pin turns red when an
// aging rule lands (then rewrite it into its gate).
func TestExternalHeavyHeadSameTenantNamedLimit(t *testing.T) {
	for _, w := range []int{weightE, weightE - 1, weightE - 2} {
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) { sameTenantLimit(t, w) })
	}
}

func sameTenantLimit(t *testing.T, w int) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	light := []*Lease{
		mustAcquire(t, d, xctx(bg, "light", "tenant-a"), weightedReq(1)),
		mustAcquire(t, d, xctx(bg, "light", "tenant-a"), weightedReq(1)),
	}
	defer func() {
		for _, l := range light {
			l.Release()
		}
	}()
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "heavy", "tenant-a"), d, "heavy", weightedReq(w), heavyCh)
	if w+2 <= weightE {
		a := recvAdmission(t, heavyCh, "control head")
		if a.err != nil {
			t.Fatalf("w=%d: %v, want admitted on arrival (w + 2 ≤ tenantShare)", w, a.err)
		}
		a.lease.Release()
		return
	}
	waitQueued(t, d, heavyCh, 1, "heavy head queued")
	l3, _, err := d.TryAcquire(xctx(bg, "light", "tenant-a"), weightedReq(1))
	if err != nil || l3 == nil {
		t.Fatalf("light's third request (within its share 3, the head over the tenant share): (lease %v, err %v), want an admission", l3 != nil, err)
	}
	light = append(light, l3)
	ch := make(chan admission, 1)
	for cycle := 0; cycle < 6; cycle++ {
		startWaiter(xctx(ctx, "light", "tenant-a"), d, "light", weightedReq(1), ch)
		waitFor(t, "light request queued", func() bool { return waitingExternal(d) == 2 })
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
		a := recvAdmission(t, ch, "light after the release")
		if a.err != nil {
			t.Fatalf("cycle %d: light: %v", cycle, a.err)
		}
		light = append(light, a.lease)
		if n := waitingExternal(d); n != 1 {
			t.Fatalf("cycle %d: external waiting %d, want 1 (the heavy head)", cycle, n)
		}
	}
	select {
	case a := <-heavyCh:
		t.Fatalf("w=%d head ended (err %v) — the named limit changed; rewrite this pin into the gate of the aging rule", w, a.err)
	default:
	}
	t.Logf("named limit: same-tenant w=%d head still waits after 6 releases, each went to the busy consumer (share 3)", w)
	cancel()
	if a := recvAdmission(t, heavyCh, "heavy canceled"); !errors.Is(a.err, context.Canceled) {
		t.Fatalf("heavy after cancel: %v, want context.Canceled", a.err)
	}
}

// TestExternalReservationServesTheFairnessFirstHead (F-1 (i) × d2): under
// 16 background workers a light head of consumer "light" (tenant-a, 30 ms of
// slot time from an earlier lease) waits before the w = E head of "heavy"
// (tenant-b, no slot time). The heavy head is the fairness-first one and
// holds the class (the light head takes no unit), so d2 must reserve the
// heavy head's need against background (not the lightest head's): the heavy
// head is admitted within one background hold time (+ 50 ms), the light
// head after it. Before the hardening the light head takes the first freed
// unit and keeps it, the heavy head never fits the partition and ends with
// ErrWaitExpired; with the reservation for the lightest head the light head
// may not backfill and background fills every unit but one.
func TestExternalReservationServesTheFairnessFirstHead(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	warm := mustAcquire(t, d, xctx(bg, "light", "tenant-a"), weightedReq(1))
	time.Sleep(30 * time.Millisecond)
	warm.Release()
	stop := backgroundLoad(d, headGateBG, headGateHold, nil)
	defer stop()
	waitFor(t, "background saturates the target", func() bool { return target(t, d).HeldWeight == weightSlots })
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	lightCh := make(chan admission, 1)
	lreq := weightedReq(1)
	lreq.MaxWait = 5 * time.Second
	startWaiter(xctx(ctx, "light", "tenant-a"), d, "light", lreq, lightCh)
	waitFor(t, "light head queued", func() bool { return waitingExternal(d) == 1 })
	req := weightedReq(weightE)
	req.MaxWait = time.Second
	l, took, err := timedAcquire(d, "heavy", "tenant-b", req)
	t.Logf("fairness-first w=E head behind a waiting light head under %d background workers: err=%v after %v (bound %v)", headGateBG, err, took, headGateHold+headGateSlack)
	if l != nil {
		l.Release()
	}
	a := recvAdmission(t, lightCh, "light head")
	if a.lease != nil {
		a.lease.Release()
	}
	if err != nil || took > headGateHold+headGateSlack {
		t.Fatalf("heavy w=E head: err=%v after %v, want admitted within one background hold time + %v — the reservation must follow the fairness-first head", err, took, headGateSlack)
	}
	if a.err != nil {
		t.Fatalf("light head: %v, want admitted after the heavy head", a.err)
	}
}

// TestExternalNewcomerLosesTheTieAgainstTheFairnessFirstHead (F-1 (i) for
// the newcomer): E = 5, an occupant of tenant-z holds one unit, so the
// w = E head of c1 (tenant-a, nothing held, no slot time) does not fit. A
// newcomer of a fresh tenant ties with it on share and slot time and loses
// on arrival — it would-blocks instead of taking a unit the head needs
// (without the rule every first request of a new consumer or tenant passes
// the head). Released, the occupant's unit goes to the head.
func TestExternalNewcomerLosesTheTieAgainstTheFairnessFirstHead(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	occ := mustAcquire(t, d, xctx(bg, "c0", "tenant-z"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "c1", weightedReq(weightE), ch)
	waitFor(t, "c1 queued", func() bool { return waitingExternal(d) == 1 })
	if l, _, err := d.TryAcquire(xctx(bg, "c2", "tenant-b"), weightedReq(1)); !errors.Is(err, ErrWouldBlock) || l != nil {
		if l != nil {
			l.Release()
		}
		t.Fatalf("newcomer tied with the waiting fairness-first head: (lease %v, err %v), want ErrWouldBlock — it takes a unit the head needs", l != nil, err)
	}
	occ.Release()
	if got := recvAdmission(t, ch, "c1 after the release"); got.err != nil {
		t.Fatalf("c1: %v, want admitted", got.err)
	} else {
		got.lease.Release()
	}
}

// TestExternalReservationHoldsWhileExternalLeasesDrain (review 3 F3-1, X1):
// S = 7, R = 2 (E = 5); interactive holds 3 (> R), k0 one external unit,
// the w = 4 head of another tenant waits (fits E, not S: 4 + 4 > 7).
// Interactive alone does not keep it out (3 + 4 ≤ 7), so the units are
// reserved against background as well as against the backfill — a
// background try would-blocks; when k0's lease ends the head is admitted at
// once (≤ 50 ms). Counting the draining external lease as staying (the
// first F-2 term, held − heldB + need ≤ S) let background take the units in
// this window and the head waited for the background leases.
func TestExternalReservationHoldsWhileExternalLeasesDrain(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	var ileases []*Lease
	for i := 0; i < 3; i++ {
		ileases = append(ileases, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	defer func() {
		for _, l := range ileases {
			l.Release()
		}
	}()
	k0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-z"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	hreq := weightedReq(weightE - 1)
	hreq.MaxWait = 2 * time.Second
	startWaiter(xctx(ctx, "heavy", "tenant-b"), d, "heavy", hreq, heavyCh)
	waitFor(t, "heavy head queued", func() bool { return waitingExternal(d) == 1 })
	if l, _, err := d.TryAcquire(bg, backgroundReq()); !errors.Is(err, ErrWouldBlock) || l != nil {
		if l != nil {
			l.Release()
		}
		t.Fatalf("background try while the external lease drains for the head (o1): (lease %v, err %v), want ErrWouldBlock — the units are reserved", l != nil, err)
	}
	rel := time.Now()
	k0.Release()
	a := recvAdmission(t, heavyCh, "heavy head after the external release")
	lag := time.Since(rel)
	if a.lease != nil {
		a.lease.Release()
	}
	t.Logf("heavy w=%d admitted %v after the external release (bound 50ms), err=%v", weightE-1, lag, a.err)
	if a.err != nil || lag > 50*time.Millisecond {
		t.Fatalf("heavy head: err=%v after %v, want admitted ≤ 50ms after the external lease ended", a.err, lag)
	}
}

// TestExternalNewcomerOfAKnownTenantCarriesItsSlotTime (review 3 F3-3, R04):
// the first request of a NEW consumer of a busy tenant ranks with its
// tenant's record, not a fresh one. Fake clock, E = 5: tenant-b served 1 s
// (kb), tenant-a's ka holds a unit for 10 s; kb's w = E head waits (1 + 5 >
// E). A first request of ka2 (tenant-a, w = 1) fits but would take units
// the head needs and its tenant has more slot time (10 s vs 1 s): it
// would-blocks; ka's release admits the head.
func TestExternalNewcomerOfAKnownTenantCarriesItsSlotTime(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	kb := mustAcquire(t, d, xctx(bg, "kb", "tenant-b"), weightedReq(1))
	clk.Add(int64(time.Second))
	kb.Release()
	ka := mustAcquire(t, d, xctx(bg, "ka", "tenant-a"), weightedReq(1))
	clk.Add(int64(10 * time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "kb", "tenant-b"), d, "kb-heavy", weightedReq(weightE), ch)
	waitFor(t, "kb's heavy head queued", func() bool { return waitingExternal(d) == 1 })
	if l, _, err := d.TryAcquire(xctx(bg, "ka2", "tenant-a"), weightedReq(1)); !errors.Is(err, ErrWouldBlock) || l != nil {
		if l != nil {
			l.Release()
		}
		t.Fatalf("first request of a new consumer of tenant-a (10 s of slot time) next to tenant-b's waiting head (1 s): (lease %v, err %v), want ErrWouldBlock — it ranked with a fresh tenant record", l != nil, err)
	}
	ka.Release()
	if got := recvAdmission(t, ch, "kb's heavy head after ka's release"); got.err != nil {
		t.Fatalf("kb heavy: %v, want admitted", got.err)
	} else {
		got.lease.Release()
	}
}

// TestExternalBackfillRunsWhileInteractiveKeepsTheHeadOut pins the bound of
// the backfill reservation (F-1 (i) with the F-2 principle): interactive
// holds 4 of S = 7, so the w = E head (tenant-b) cannot fit until
// interactive releases — the reservation would only idle the free units.
// A light newcomer of another consumer is admitted at once (as before the
// hardening, d1), and so is the light head of a consumer that waits.
func TestExternalBackfillRunsWhileInteractiveKeepsTheHeadOut(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	var ileases []*Lease
	for i := 0; i < 4; i++ {
		ileases = append(ileases, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	defer func() {
		for _, l := range ileases {
			l.Release()
		}
	}()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	hreq := weightedReq(weightE)
	hreq.MaxWait = time.Second
	startWaiter(xctx(ctx, "heavy", "tenant-b"), d, "heavy", hreq, heavyCh)
	waitFor(t, "heavy head queued", func() bool { return waitingExternal(d) == 1 })
	l, _, err := d.TryAcquire(xctx(bg, "light", "tenant-a"), weightedReq(1))
	if err != nil || l == nil {
		t.Fatalf("light newcomer while interactive keeps the w=E head out: (lease %v, err %v), want an admission — a reservation the head cannot use idles the units", l != nil, err)
	}
	l2, _, err := d.TryAcquire(xctx(bg, "light2", "tenant-a"), weightedReq(2))
	if err != nil || l2 == nil {
		t.Fatalf("second light newcomer (w=2): (lease %v, err %v), want an admission", l2 != nil, err)
	}
	l.Release()
	l2.Release()
	for _, il := range ileases {
		il.Release()
	}
	ileases = nil
	if a := recvAdmission(t, heavyCh, "heavy head after interactive released"); a.err != nil {
		t.Fatalf("heavy head: %v, want admitted once interactive released", a.err)
	} else {
		a.lease.Release()
	}
}

// TestExternalBackfillRunsWhileBackgroundRanksAboveUnderO2 is the o2 half
// of the bound: under dispatch.external_order=o2 background ranks above the
// external class, so its held units are not the backfill's to wait for.
// Background holds 4 of S = 7, the w = E head waits (4 + 5 > 7); a light
// newcomer of another consumer is admitted at once.
func TestExternalBackfillRunsWhileBackgroundRanksAboveUnderO2(t *testing.T) {
	s := DefaultSettings()
	s.RankOrder = mustRankOrder(t, ExternalOrderO2)
	d, _ := newTestDispatcher(t, s, weightPolicy())
	bg := context.Background()
	var bleases []*Lease
	for i := 0; i < 4; i++ {
		bleases = append(bleases, mustAcquire(t, d, bg, backgroundReq()))
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	hreq := weightedReq(weightE)
	hreq.MaxWait = time.Second
	startWaiter(xctx(ctx, "heavy", "tenant-b"), d, "heavy", hreq, heavyCh)
	waitFor(t, "heavy head queued", func() bool { return waitingExternal(d) == 1 })
	l, _, err := d.TryAcquire(xctx(bg, "light", "tenant-a"), weightedReq(1))
	if err != nil || l == nil {
		t.Fatalf("o2: light newcomer while background (ranked above) keeps the w=E head out: (lease %v, err %v), want an admission", l != nil, err)
	}
	l.Release()
	for _, bl := range bleases {
		bl.Release()
	}
	if a := recvAdmission(t, heavyCh, "heavy head after background released"); a.err != nil {
		t.Fatalf("heavy head: %v, want admitted once background released", a.err)
	} else {
		a.lease.Release()
	}
}

// TestExternalInteractiveAdmissionLiftsTheReservationAtOnce (review 3, the
// trace under o2): units an in-house class ranked above the external class
// takes can lift the reservation for the fairness-first head — the units it
// held back must go to the waiters at once, not at the next release.
// S = 7, R = 2 (E = 5): interactive holds 2, an occupant of tenant-z one
// external unit; the w = E head of tenant-b waits (1 + 5 > E) and binds the
// backfill (2 + 5 ≤ S), so a light acquire of tenant-a queues behind it. An
// interactive newcomer takes a third unit: now interactive alone keeps the
// head out (3 + 5 > S), and the light waiter is admitted by that admission's
// wake (≤ 50 ms).
func TestExternalInteractiveAdmissionLiftsTheReservationAtOnce(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	var ileases []*Lease
	for i := 0; i < 2; i++ {
		ileases = append(ileases, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	occ := mustAcquire(t, d, xctx(bg, "occ", "tenant-z"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	hreq := weightedReq(weightE)
	hreq.MaxWait = 2 * time.Second
	startWaiter(xctx(ctx, "heavy", "tenant-b"), d, "heavy", hreq, ch)
	waitQueued(t, d, ch, 1, "heavy head queued")
	startWaiter(xctx(ctx, "light", "tenant-a"), d, "light", weightedReq(1), ch)
	waitQueued(t, d, ch, 2, "light acquire queued behind the reservation")
	start := time.Now()
	ileases = append(ileases, mustAcquire(t, d, ictx(principal("i2")), interactiveReq()))
	got := recvAdmission(t, ch, "light after the interactive admission")
	lag := time.Since(start)
	t.Logf("%s admitted %v after the interactive admission (bound 50ms), err=%v", got.label, lag, got.err)
	if got.err != nil || got.label != "light" || lag > 50*time.Millisecond {
		t.Fatalf("after the interactive admission: %s err=%v after %v, want light admitted ≤ 50ms — the lifted reservation waited for the next release", got.label, got.err, lag)
	}
	got.lease.Release()
	cancel()
	recvAdmission(t, ch, "heavy canceled")
	occ.Release()
	for _, l := range ileases {
		l.Release()
	}
}

// TestExternalReservationOnlyWhileTheHeadCouldFit is the F-2 gate (review 2
// P9): S = 7, R = 2; interactive holds 4, background 3, a w = E head waits
// (fits E, never S while interactive holds 4: 4 + 5 > 7) and one background
// waiter queues. One background lease ends: the head could not use the unit,
// so background takes it at once (≤ 50 ms). Before the hardening d2 kept the
// unit idle until the head's budget ran out (≈ 295 ms of 300 ms).
func TestExternalReservationOnlyWhileTheHeadCouldFit(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	var ileases, bleases []*Lease
	for i := 0; i < 4; i++ {
		ileases = append(ileases, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	for i := 0; i < 3; i++ {
		bleases = append(bleases, mustAcquire(t, d, context.Background(), backgroundReq()))
	}
	defer func() {
		for _, l := range append(ileases, bleases[1:]...) {
			l.Release()
		}
	}()
	headDone := make(chan error, 1)
	go func() {
		req := weightedReq(weightE)
		req.MaxWait = 300 * time.Millisecond
		l, _, err := d.Acquire(xctx(context.Background(), "heavy", "tenant-a"), req)
		if l != nil {
			l.Release()
		}
		headDone <- err
	}()
	waitFor(t, "head queued", func() bool { return waitingExternal(d) == 1 })
	bgAdm := make(chan time.Time, 1)
	go func() {
		l, _, err := d.Acquire(context.Background(), backgroundReq())
		if err == nil {
			bgAdm <- time.Now()
			l.Release()
		}
	}()
	waitFor(t, "background waiter queued", func() bool { return target(t, d).Background.Waiting == 1 })
	rel := time.Now()
	bleases[0].Release()
	var lag time.Duration
	select {
	case at := <-bgAdm:
		lag = at.Sub(rel)
	case <-time.After(2 * time.Second):
		t.Fatal("background never admitted")
	}
	herr := <-headDone
	t.Logf("background admitted %v after the release (bound 50ms); head: %v", lag, herr)
	if lag > 50*time.Millisecond {
		t.Fatalf("background admitted %v after the release, want ≤ 50ms — d2 kept a unit the head could not use (interactive holds 4 > S − w)", lag)
	}
	if !errors.Is(herr, ErrWaitExpired) {
		t.Fatalf("head: %v, want ErrWaitExpired (it never fits while interactive holds 4)", herr)
	}
}

// TestContinuationThatDoesNotFitBlocksTheClass pins the lead decision of the
// rebase onto A03-W17 (review 2 F-3 gap 1): S = 5, R = 2 (E = 3); background
// holds 3, k0 one external unit. k1's continuation (w = 2) fits E (1 + 2) but
// not S (4 + 2): it stays the head of the class and blocks — a bucket head
// of k2 that would fit (w = 1) is not admitted by a wake, a newcomer try of
// k3 would-blocks, and in o1 a background try would-blocks (d2 reserves
// the continuation's need). The next background release admits the
// continuation first, k2 after it.
func TestContinuationThatDoesNotFitBlocksTheClass(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(5, 2))
	bg := context.Background()
	var bleases []*Lease
	for i := 0; i < 3; i++ {
		bleases = append(bleases, mustAcquire(t, d, bg, backgroundReq()))
	}
	k0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "k1-cont", contReq(2), ch)
	waitQueued(t, d, ch, 1, "k1's continuation queued")
	if l, _, err := d.TryAcquire(bg, backgroundReq()); !errors.Is(err, ErrWouldBlock) || l != nil {
		if l != nil {
			l.Release()
		}
		t.Fatalf("background try while only the continuation waits for its units (o1): (lease %v, err %v), want ErrWouldBlock — d2 reserves its need", l != nil, err)
	}
	startWaiter(xctx(ctx, "k2", "tenant-b"), d, "k2", weightedReq(1), ch)
	waitQueued(t, d, ch, 2, "k2 queued behind the continuation")
	if l, _, err := d.TryAcquire(xctx(bg, "k3", "tenant-c"), weightedReq(1)); !errors.Is(err, ErrWouldBlock) || l != nil {
		if l != nil {
			l.Release()
		}
		t.Fatalf("newcomer try behind a continuation that does not fit: (lease %v, err %v), want ErrWouldBlock", l != nil, err)
	}
	if l, _, err := d.TryAcquire(bg, backgroundReq()); !errors.Is(err, ErrWouldBlock) || l != nil {
		if l != nil {
			l.Release()
		}
		t.Fatalf("background try while the continuation waits for its units (o1): (lease %v, err %v), want ErrWouldBlock — d2 reserves them", l != nil, err)
	}
	d.wakeAll()
	select {
	case a := <-ch:
		t.Fatalf("%s admitted while the continuation that does not fit waits (err %v) — a bucket head passed the class head", a.label, a.err)
	case <-time.After(20 * time.Millisecond):
	}
	bleases[0].Release()
	got := recvAdmission(t, ch, "handoff after the background release")
	if got.err != nil || got.label != "k1-cont" {
		t.Fatalf("handoff = %s (err %v), want k1-cont", got.label, got.err)
	}
	got.lease.Release()
	next := recvAdmission(t, ch, "k2 after the continuation")
	if next.err != nil || next.label != "k2" {
		t.Fatalf("second handoff = %s (err %v), want k2", next.label, next.err)
	}
	next.lease.Release()
	k0.Release()
	for _, l := range bleases[1:] {
		l.Release()
	}
}

// TestExternalPickServesLeastSlotTimeOnTheProductionClock is the F-4 pin:
// no clock seam. E = 1 (S = 3, R = 2): k1 (tenant-a) holds the unit 30 ms;
// meanwhile k1's second acquire queues, then k2's (tenant-b). At the release
// the unit goes to k2 — k1's tenant has 30 ms of slot time on the real
// clock, k2's none. A frozen production clock makes every slot time 0 and
// hands it to k1's older waiter.
func TestExternalPickServesLeastSlotTimeOnTheProductionClock(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(3, 2))
	bg := context.Background()
	first := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "k1-second", weightedReq(1), ch)
	waitQueued(t, d, ch, 1, "k1's second acquire queued")
	startWaiter(xctx(ctx, "k2", "tenant-b"), d, "k2", weightedReq(1), ch)
	waitQueued(t, d, ch, 2, "k2 queued")
	time.Sleep(30 * time.Millisecond)
	first.Release()
	got := recvAdmission(t, ch, "handoff after the release")
	if got.err != nil || got.label != "k2" {
		t.Fatalf("handoff = %s (err %v), want k2 — on the production clock k1's tenant has 30 ms of slot time, k2's none", got.label, got.err)
	}
	got.lease.Release()
	next := recvAdmission(t, ch, "k1's second acquire")
	if next.err != nil {
		t.Fatalf("k1-second: %v", next.err)
	}
	next.lease.Release()
}
