package dispatch

// Gates of the aging rule of heavy external heads (wave A03-W8a; lead
// decision O3, A03-W4 review 2 F-1 / review 3 F3-2), all through the stock
// API — they compile against the tree before the rule, which is their red
// run. The named limit they replace: a head of weight w next to a busy
// consumer of the SAME tenant with consumer share s stays above its tenant
// share while w + (s − 1) exceeds the tenant share — the co-consumer ranks
// in stage (i) up to s − 1 held units, the tenant never drops below s − 1,
// every freed unit goes to the co-consumer and the head ends at its wait
// budget. The rule: once units of its class amounting to one E_eff went to
// other consumers while it waited, the head has aged and its share term no
// longer puts it in stage (ii); the slot-time order and the hold do the rest.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// agingCase is one scripted same-tenant case of the aging gates.
type agingCase struct {
	class Class // ClassExternal or ClassExternalBulk
	w     int   // the heavy head's weight
	// lightFW is the busy co-consumer's fair weight (the heavy consumer's is
	// xuPrincipal's 10).
	lightFW int
	// occupant holds one unit of tenant-b throughout (T_act = 2, tenant
	// share ⌈5/2⌉ = 3).
	occupant bool
}

// shares returns the tenant share and the busy consumer's share s of c (one
// or two active tenants, E = 5; the formula of fairness.go shareOf).
func (c agingCase) shares() (tenantShare, s int) {
	tenantShare = weightE
	if c.occupant {
		tenantShare = (weightE + 1) / 2
	}
	return tenantShare, max(1, (tenantShare*c.lightFW+c.lightFW+10-1)/(c.lightFW+10))
}

// agingScript runs one case as a deterministic sequence on the fake clock
// (the script of the former pin TestExternalHeavyHeadSameTenantNamedLimit):
// the busy consumer "light" holds s − 1 units (s when a starving head would
// fit beside s − 1 on arrival), the heavy head of weight w (consumer
// "heavy", the same tenant) queues, light's s-th request is admitted at
// once; then, until E units went to light after the head's
// arrival, light queues one request and one of its leases ends — every
// freed unit goes to light, the head keeps waiting (before the threshold
// nothing changes). From the threshold on the head has aged: at the next
// release light's queued request no longer gets the unit, a fresh consumer
// of the same tenant does not pass the head, and the head is admitted at the
// release that frees its w units — at the latest when the last lease light
// held at the threshold ends (within one lease duration of the busy
// consumer), each admission within 50 ms of its release. Control: a head
// with w + (s − 1) within the tenant share is admitted on arrival.
func agingScript(t *testing.T, c agingCase) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	tenantShare, s := c.shares()
	req := func(w int) Request {
		r := weightedReq(w)
		r.Class = c.class
		return r
	}
	waiting := func() int {
		if c.class == ClassExternalBulk {
			return waitingBulk(d)
		}
		return waitingExternal(d)
	}
	waitWaiting := func(n int, what string) {
		t.Helper()
		waitFor(t, what, func() bool { return waiting() == n })
	}
	lightP := xuPrincipal("light", "tenant-a")
	lightP.FairWeight = c.lightFW
	lightCtx := func(ctx context.Context) context.Context { return withPrincipal(ctx, lightP) }

	var light []*Lease
	defer func() {
		for _, l := range light {
			l.Release()
		}
	}()
	occupied := 0
	if c.occupant {
		occupied = 1
	}
	// light holds s − 1 units; a starving head that would fit beside them
	// on arrival (no waiter, the immediate admission reads no share) meets
	// light at its share s instead — the state the limit cycles through.
	lead := s - 1
	if c.w+s-1 > tenantShare && lead+occupied+c.w <= weightE {
		lead = s
	}
	for i := 0; i < lead; i++ {
		light = append(light, mustAcquire(t, d, lightCtx(bg), req(1)))
	}
	if c.occupant {
		defer mustAcquire(t, d, xctx(bg, "occ", "tenant-b"), req(1)).Release()
	}
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "heavy", "tenant-a"), d, "heavy", req(c.w), heavyCh)
	if c.w+s-1 <= tenantShare {
		a := recvAdmission(t, heavyCh, "control head")
		if a.err != nil {
			t.Fatalf("control w=%d: %v, want admitted on arrival (w + s − 1 = %d ≤ tenant share %d)", c.w, a.err, c.w+s-1, tenantShare)
		}
		a.lease.Release()
		return
	}
	waitWaiting(1, "heavy head queued")
	passed := 0
	if lead < s {
		l, _, err := d.TryAcquire(lightCtx(bg), req(1))
		if err != nil || l == nil {
			t.Fatalf("light's request %d (within its share %d): (lease %v, err %v), want an admission", s, s, l != nil, err)
		}
		light = append(light, l)
		passed++
	}
	lightCh := make(chan admission, 1)
	release := func() {
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
	}
	// Before the threshold: every freed unit goes to the busy consumer.
	for ; passed < weightE; passed++ {
		startWaiter(lightCtx(ctx), d, "light", req(1), lightCh)
		waitWaiting(2, "light request queued")
		release()
		a := recvAdmission(t, lightCh, "light after the release")
		if a.err != nil {
			t.Fatalf("before the threshold (%d units passed the head): light: %v, want the freed unit", passed, a.err)
		}
		light = append(light, a.lease)
		if n := waiting(); n != 1 {
			t.Fatalf("before the threshold (%d units passed the head): waiting %d, want 1 (the heavy head)", passed+1, n)
		}
		select {
		case a := <-heavyCh:
			t.Fatalf("before the threshold (%d units passed the head): heavy ended early (err %v) — the head aged before E units passed it", passed+1, a.err)
		default:
		}
	}
	// The threshold: E units went to light after the head arrived.
	atThreshold := len(light)
	startWaiter(lightCtx(ctx), d, "light", req(1), lightCh)
	waitWaiting(2, "light request queued at the threshold")
	newcomerProbed := false
	for i := 0; i < atThreshold; i++ {
		release()
		held := len(light) + occupied
		if held+c.w <= weightE {
			select {
			case a := <-heavyCh:
				if a.err != nil {
					t.Fatalf("w=%d: %v, want admitted at the release that frees its units", c.w, a.err)
				}
				t.Logf("w=%d (%s, s=%d, tenant share %d): admitted at release %d of the %d leases light held at the threshold; before it %d units went to light", c.w, c.class, s, tenantShare, i+1, atThreshold, passed)
				a.lease.Release()
				if a := recvAdmission(t, lightCh, "light after the heavy head"); a.lease != nil {
					a.lease.Release()
				}
				return
			case a := <-lightCh:
				t.Fatalf("w=%d fits after release %d and light got the unit (err %v) — the aged head lost its units", c.w, i+1, a.err)
			case <-time.After(headGateSlack):
				t.Fatalf("w=%d: fits after release %d (%d units held) and still waits %v later, waiting %d — %d units passed it, the named limit still holds (the busy consumer keeps the units)", c.w, i+1, held, headGateSlack, waiting(), passed)
			}
		}
		if n := waiting(); n != 2 {
			t.Fatalf("w=%d after release %d at the threshold (%d units passed the head): waiting %d, want 2 — the busy consumer took the unit, the aged head does not hold its class", c.w, i+1, passed, n)
		}
		if !newcomerProbed {
			newcomerProbed = true
			l, _, err := d.TryAcquire(xctx(bg, "light2", "tenant-a"), req(1))
			if l != nil {
				l.Release()
			}
			if !errors.Is(err, ErrWouldBlock) {
				t.Fatalf("a fresh consumer of the same tenant beside the aged head: (lease %v, err %v), want ErrWouldBlock — it passed the head that holds its class", l != nil, err)
			}
		}
	}
	select {
	case a := <-heavyCh:
		t.Fatalf("w=%d ended with %v after the leases light held at the threshold", c.w, a.err)
	default:
	}
	t.Fatalf("w=%d still waits after the %d leases light held at the threshold ended — not admitted within one lease duration of the busy consumer", c.w, atThreshold)
}

// TestExternalHeavyHeadSameTenantAges is gate (a), the pin of the named
// limit (TestExternalHeavyHeadSameTenantNamedLimit, A03-W4) rewritten into
// the gate of the aging rule: one active tenant, E = 5 (tenant share 5),
// equal fair weights (s = 3) — w = 5 and w = 4 are served once E units
// passed them, w = 3 (3 + 2 ≤ 5) is admitted on arrival.
func TestExternalHeavyHeadSameTenantAges(t *testing.T) {
	for _, w := range []int{weightE, weightE - 1, weightE - 2} {
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) {
			agingScript(t, agingCase{class: ClassExternal, w: w, lightFW: 10})
		})
	}
}

// TestExternalHeavyHeadTwoTenantsAges is gate (b), review 3 F3-2 probe
// X3/P8: tenant-b holds one unit (T_act = 2, tenant share 3), so tenant-a's
// busy consumer has s = 2 and a head of w = 3 already starves (3 + 1 > 3);
// once E units passed it, it is served. Control: w = 2 (2 + 1 ≤ 3) on
// arrival.
func TestExternalHeavyHeadTwoTenantsAges(t *testing.T) {
	for _, w := range []int{3, 2} {
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) {
			agingScript(t, agingCase{class: ClassExternal, w: w, lightFW: 10, occupant: true})
		})
	}
}

// TestExternalHeavyHeadHeavierCoConsumerAges is gate (c), review 3 F3-2
// ("with a higher fair_weight of the co-consumer the limit sinks further"):
// the busy consumer's fair weight is three times the head's, s = ⌈5·3/4⌉ =
// 4, so w = 3 already starves (3 + 3 > 5); once E units passed it, it is
// served. Control: w = 2 (2 + 3 ≤ 5) on arrival.
func TestExternalHeavyHeadHeavierCoConsumerAges(t *testing.T) {
	for _, w := range []int{3, 2} {
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) {
			agingScript(t, agingCase{class: ClassExternal, w: w, lightFW: 30})
		})
	}
}

// TestExternalBulkHeavyHeadSameTenantAges is gate (d), first part: the same
// limit inside the XB queue — in the configuration without a waiting XU head
// (no XU load: no XU unit keeps the XB head out, keptOutLocked, and no XU
// claim binds it, externalClaimLocked; under sustained XU load above E it is
// A03-W9's named limit instead). The rule acts per external class in its own
// queue: w = 5 and w = 4 are served once E XB units passed them; w = 3 on
// arrival; two tenants w = 3; a heavier co-consumer w = 3.
func TestExternalBulkHeavyHeadSameTenantAges(t *testing.T) {
	for _, c := range []agingCase{
		{class: ClassExternalBulk, w: weightE, lightFW: 10},
		{class: ClassExternalBulk, w: weightE - 1, lightFW: 10},
		{class: ClassExternalBulk, w: weightE - 2, lightFW: 10},
		{class: ClassExternalBulk, w: 3, lightFW: 10, occupant: true},
		{class: ClassExternalBulk, w: 3, lightFW: 30},
	} {
		t.Run(fmt.Sprintf("w%d_fw%d_occ%v", c.w, c.lightFW, c.occupant), func(t *testing.T) { agingScript(t, c) })
	}
}

// TestExternalBulkAgedHeadNeverPassesAWaitingXUHead is gate (d), second
// part (rank order E2: XU ≻ XB; design/03 §4.8 "anti-starvation never
// overrules the class order"): the XB head of weight 3 ages beside its busy
// co-consumer (fair weight 3:1, s = 4; E XB units passed it) and holds the
// XB class; then an XU head of weight 4 queues (2 units free) and claims its
// units against XB. When the next XB lease ends (3 free) the aged XB head
// would fit but takes none of the units the XU head waits for; at the next
// release (4 free) they go to the XU head, and the aged XB head gets its
// units only after the XU lease ended.
func TestExternalBulkAgedHeadNeverPassesAWaitingXUHead(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	lightP := xuPrincipal("light", "tenant-a")
	lightP.FairWeight = 30
	lightCtx := func(ctx context.Context) context.Context { return withPrincipal(ctx, lightP) }
	var light []*Lease
	defer func() {
		for _, l := range light {
			l.Release()
		}
	}()
	for i := 0; i < 3; i++ {
		light = append(light, mustAcquire(t, d, lightCtx(bg), xbReq()))
	}
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "heavy", "tenant-a"), d, "heavy", xbWeighted(3), heavyCh)
	waitFor(t, "heavy XB head queued", func() bool { return waitingBulk(d) == 1 })
	l, _, err := d.TryAcquire(lightCtx(bg), xbReq())
	if err != nil {
		t.Fatalf("light's fourth XB request: %v, want an admission", err)
	}
	light = append(light, l)
	lightCh := make(chan admission, 1)
	release := func() {
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
	}
	for passed := 1; passed < weightE; passed++ {
		startWaiter(lightCtx(ctx), d, "light", xbReq(), lightCh)
		waitFor(t, "light XB request queued", func() bool { return waitingBulk(d) == 2 })
		release()
		a := recvAdmission(t, lightCh, "light XB after the release")
		if a.err != nil {
			t.Fatalf("before the threshold: light XB: %v", a.err)
		}
		light = append(light, a.lease)
	}
	// The XB head has aged (E XB units passed it); light holds 4.
	startWaiter(lightCtx(ctx), d, "light", xbReq(), lightCh)
	waitFor(t, "light XB request queued at the threshold", func() bool { return waitingBulk(d) == 2 })
	release() // light 3, 2 free
	if n := waitingBulk(d); n != 2 {
		t.Fatalf("after the threshold release: XB waiting %d, want 2 — the aged XB head does not hold its class", n)
	}
	xuCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "u", "tenant-c"), d, "xu", weightedReq(4), xuCh)
	waitQueued(t, d, xuCh, 1, "XU head queued")
	release() // light 2, 3 free: the aged XB head fits, the XU head does not
	select {
	case h := <-heavyCh:
		t.Fatalf("aged XB head admitted (err %v) while an XU head waits for the same units — aging overruled the class order", h.err)
	case a := <-xuCh:
		t.Fatalf("XU head ended (err %v) with 3 units free, want it waiting for 4", a.err)
	case <-time.After(headGateSlack):
	}
	if ts := target(t, d); ts.External.Waiting != 1 || ts.ExternalBulk.Waiting != 2 || ts.HeldExternalWeight != 2 {
		t.Fatalf("with 3 units free: XU waiting %d, XB waiting %d, held %d, want 1/2/2", ts.External.Waiting, ts.ExternalBulk.Waiting, ts.HeldExternalWeight)
	}
	release() // light 1, 4 free
	a := recvAdmission(t, xuCh, "XU head at 4 free units")
	if a.err != nil {
		t.Fatalf("XU head: %v, want the 4 freed units", a.err)
	}
	select {
	case h := <-heavyCh:
		t.Fatalf("aged XB head ended (err %v) beside the XU admission", h.err)
	default:
	}
	a.lease.Release()
	h := recvAdmission(t, heavyCh, "aged XB head after the XU lease ended")
	if h.err != nil {
		t.Fatalf("aged XB head after the XU lease ended: %v, want its units", h.err)
	}
	h.lease.Release()
	if a := recvAdmission(t, lightCh, "light XB after the aged head"); a.lease != nil {
		a.lease.Release()
	}
}

// TestExternalAgedHeadKeepsTheTenantOrder is the negative probe of the rule
// (the lead review's "aging must not displace light heads of other
// tenants"): the aged head of tenant-a only leaves stage (ii) — the slot-time
// order between tenants stays. Beside the aged, holding head a light
// newcomer of a fresh tenant (no slot time) ranks before it and is admitted
// at once; a fresh consumer of tenant-a itself does not pass it.
func TestExternalAgedHeadKeepsTheTenantOrder(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	var light []*Lease
	defer func() {
		for _, l := range light {
			l.Release()
		}
	}()
	for i := 0; i < 2; i++ {
		light = append(light, mustAcquire(t, d, xctx(bg, "light", "tenant-a"), weightedReq(1)))
	}
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "heavy", "tenant-a"), d, "heavy", weightedReq(weightE), heavyCh)
	waitQueued(t, d, heavyCh, 1, "heavy head queued")
	l, _, err := d.TryAcquire(xctx(bg, "light", "tenant-a"), weightedReq(1))
	if err != nil {
		t.Fatalf("light's third request: %v", err)
	}
	light = append(light, l)
	lightCh := make(chan admission, 1)
	for passed := 1; passed < weightE; passed++ {
		startWaiter(xctx(ctx, "light", "tenant-a"), d, "light", weightedReq(1), lightCh)
		waitFor(t, "light request queued", func() bool { return waitingExternal(d) == 2 })
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
		a := recvAdmission(t, lightCh, "light after the release")
		if a.err != nil {
			t.Fatalf("before the threshold: light: %v", a.err)
		}
		light = append(light, a.lease)
	}
	clk.Add(int64(time.Second))
	if l, _, err := d.TryAcquire(xctx(bg, "light2", "tenant-a"), weightedReq(1)); !errors.Is(err, ErrWouldBlock) {
		if l != nil {
			l.Release()
		}
		t.Fatalf("fresh consumer of tenant-a beside the aged head: err %v, want ErrWouldBlock", err)
	}
	other, _, err := d.TryAcquire(xctx(bg, "other", "tenant-b"), weightedReq(1))
	if err != nil {
		t.Fatalf("light newcomer of a fresh tenant beside the aged head of tenant-a: %v, want an admission — aging displaced the tenant order", err)
	}
	other.Release()
	select {
	case a := <-heavyCh:
		t.Fatalf("heavy ended (err %v) while light holds 3 units", a.err)
	default:
	}
}

// stampedLightStream is lightStream with the admission instants of the
// light leases recorded (production clock).
func stampedLightStream(d *Dispatcher, n int, hold time.Duration, consumer, tenant string) (stop func() []time.Time) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var at []time.Time
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
				mu.Lock()
				at = append(at, time.Now())
				mu.Unlock()
				select {
				case <-time.After(hold):
				case <-ctx.Done():
				}
				l.Release()
			}
		}()
	}
	return func() []time.Time {
		cancel()
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return at
	}
}

// TestExternalSameTenantHeavyHeadAgesOnTheProductionClock is gate (a) on the
// production clock (no seam, review 2 F-4 precedent): six light clients of
// consumer "light" (tenant-a) hold 20 ms and ask again at once; 110 ms later
// a heavy head of the same tenant (w = 5, w = 4) asks with a 1-s budget.
// Before the rule it ends with ErrWaitExpired; with it, it is admitted
// within one light hold time + 50 ms after the E-th light admission that
// followed its arrival (the threshold).
func TestExternalSameTenantHeavyHeadAgesOnTheProductionClock(t *testing.T) {
	for _, w := range []int{weightE, weightE - 1} {
		t.Run(fmt.Sprintf("w%d", w), func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
			stop := stampedLightStream(d, 6, headGateHold, "light", "tenant-a")
			time.Sleep(110 * time.Millisecond)
			req := weightedReq(w)
			req.MaxWait = time.Second
			arrived := time.Now()
			l, took, err := timedAcquire(d, "heavy", "tenant-a", req)
			admitted := arrived.Add(took)
			if l != nil {
				l.Release()
			}
			at := stop()
			var threshold time.Time
			n := 0
			for _, a := range at {
				if a.After(arrived) {
					if n++; n == weightE {
						threshold = a
						break
					}
				}
			}
			t.Logf("same-tenant w=%d under 6 light clients: err=%v after %v; E-th light admission after its arrival at +%v", w, err, took, threshold.Sub(arrived))
			if err != nil {
				t.Fatalf("w=%d: %v after %v, want admitted once E units passed it", w, err, took)
			}
			if !threshold.IsZero() && admitted.After(threshold.Add(headGateHold+headGateSlack)) {
				t.Fatalf("w=%d admitted %v after the threshold, want within one light hold time + %v", w, admitted.Sub(threshold), headGateSlack)
			}
		})
	}
}

// TestExternalAgedHeadWithMoreSlotTimeGoesFirstInItsTenant is the gate of
// review 1 F-1 (probe TestReviewAgedHeavyHeadWithMoreSlotTime, adopted): the
// heavy consumer ran before (one w = 5 lease for 100 s of fake time, 500
// unit-seconds — more slot time than its busy co-consumer will gather), then
// the same-tenant limit with w = 4 (s = 3). Before the threshold every freed
// unit goes to light; once E units passed it, the aged head goes first
// inside its tenant whatever the slot time — light gets no further unit,
// and the head is admitted when light's leases left it 4 units. Exactly E
// units go to light after its push.
func TestExternalAgedHeadWithMoreSlotTimeGoesFirstInItsTenant(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	h0 := mustAcquire(t, d, xctx(bg, "heavy", "tenant-a"), weightedReq(weightE))
	clk.Add(int64(100 * time.Second))
	h0.Release()
	clk.Add(int64(time.Second))
	var light []*Lease
	defer func() {
		for _, l := range light {
			l.Release()
		}
	}()
	for i := 0; i < 3; i++ {
		light = append(light, mustAcquire(t, d, xctx(bg, "light", "tenant-a"), weightedReq(1)))
	}
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "heavy", "tenant-a"), d, "heavy", weightedReq(4), heavyCh)
	waitQueued(t, d, heavyCh, 1, "heavy head queued")
	lightCh := make(chan admission, 1)
	toLight, pending := 0, false
	for it := 1; it <= 8*weightE; it++ {
		if !pending {
			// light asks again: admitted at once while it ranks before the
			// head (less slot time), else it queues behind it.
			startWaiter(xctx(ctx, "light", "tenant-a"), d, "light", weightedReq(1), lightCh)
			pending = true
			deadline := time.Now().Add(time.Second)
			for pending && waitingExternal(d) != 2 && time.Now().Before(deadline) {
				select {
				case a := <-lightCh:
					if a.err != nil {
						t.Fatalf("light: %v", a.err)
					}
					light = append(light, a.lease)
					toLight++
					pending = false
				case <-time.After(2 * time.Millisecond):
				}
			}
			if !pending {
				continue
			}
			if waitingExternal(d) != 2 {
				t.Fatalf("iteration %d: light's request neither admitted nor queued", it)
			}
		}
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
		select {
		case a := <-heavyCh:
			if a.err != nil {
				t.Fatalf("heavy w=4: %v", a.err)
			}
			a.lease.Release()
			if toLight != weightE {
				t.Fatalf("heavy w=4 (more slot time than light) admitted after %d units went to light since its push, want exactly E = %d (unchanged before the threshold, none after)", toLight, weightE)
			}
			t.Logf("heavy w=4 with more slot time admitted after %d units went to light, light holds %d", toLight, len(light))
			if a := recvAdmission(t, lightCh, "light after the heavy head"); a.lease != nil {
				a.lease.Release()
			}
			return
		case a := <-lightCh:
			if a.err != nil {
				t.Fatalf("light: %v", a.err)
			}
			light = append(light, a.lease)
			toLight++
			pending = false
		case <-time.After(headGateSlack):
			if toLight < weightE {
				t.Fatalf("iteration %d: before the threshold (%d units passed) the freed unit did not go to light", it, toLight)
			}
		}
	}
	t.Fatalf("heavy w=4 (more slot time than light) still waits: %d units (%d × E) went to light since its push — the aged head does not go first inside its tenant", toLight, toLight/weightE)
}

// TestExternalAgedHeadNeverLiftsAnOverShareTenant pins review 1 F-2 on the
// hardened rule: an aged head counts towards its tenant's share as one unit,
// it never lifts a tenant that is at or above its share. Warm lease window
// (L_p50 1 s), so the queued heads of tenant-x's consumer x2 (holding
// nothing) age one second after their push; tenant-x's consumer x1 holds all
// of E = 5 (tenant share 3 once tenant-y is active); tenant-y ran before (200
// unit-seconds of slot time, more than tenant-x) and queues one request
// holding nothing. When an x1 lease ends, the unit goes to y (within its
// share), not to an aged x2 head (its tenant holds 4, over the share even
// for one unit). Before the hardening a lift of the whole share term handed
// it to x2 (the tenant with less slot time).
func TestExternalAgedHeadNeverLiftsAnOverShareTenant(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	seedLeases(d, testOrigin, 4*leaseColdSamples, time.Second)
	clk := fakeFairClock(d)
	bg := context.Background()
	y0 := mustAcquire(t, d, xctx(bg, "y", "tenant-y"), weightedReq(1))
	clk.Add(int64(200 * time.Second))
	y0.Release()
	clk.Add(int64(time.Second))
	var x1 []*Lease
	defer func() {
		for _, l := range x1 {
			l.Release()
		}
	}()
	for i := 0; i < weightE; i++ {
		x1 = append(x1, mustAcquire(t, d, xctx(bg, "x1", "tenant-x"), weightedReq(1)))
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	x2Ch := make(chan admission, 4)
	for i := 0; i < 3; i++ {
		startWaiter(xctx(ctx, "x2", "tenant-x"), d, "x2", weightedReq(1), x2Ch)
		n := i + 1
		waitQueued(t, d, x2Ch, n, "x2 queued")
	}
	clk.Add(int64(2 * time.Second)) // past x2's L_p50: its heads have aged
	yCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "y", "tenant-y"), d, "y", weightedReq(1), yCh)
	waitQueued(t, d, yCh, 4, "y queued")
	clk.Add(int64(time.Second))
	x1[0].Release()
	x1 = x1[1:]
	select {
	case a := <-yCh:
		if a.err != nil {
			t.Fatalf("y: %v", a.err)
		}
		a.lease.Release()
	case a := <-x2Ch:
		if a.lease != nil {
			a.lease.Release()
		}
		t.Fatalf("an aged head of tenant-x (holding 4 of share 3) got the freed unit before tenant-y within its share (err %v) — aging lifted an over-share tenant", a.err)
	case <-time.After(time.Second):
		t.Fatal("nobody admitted after the x1 release")
	}
}

// agingTimeCase runs the slow-turnover configuration of review 1 F-3 (probe
// TestReviewSlowTurnover): E = 20 (S = 22, R = 2), one tenant, light fair
// weight 10 against heavy 90 (s = ⌈20 · 10/100⌉ = 2), a heavy head of w = 20
// — light turns over at most two units per lease, so E units take about ten
// of its lease durations. Light's leases live two releases (two fake
// seconds). warm: the lease window holds L_p50 = 2 s, the head ages one
// L_p50 after its push — at the second release, after two units went to
// light — then holds, and is admitted when light's last lease ends. cold:
// no time threshold; the head ages only once E units went to light.
func agingTimeCase(t *testing.T, warm bool) {
	const e, light2 = 20, 2
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(e+2, 2))
	if warm {
		seedLeases(d, testOrigin, 4*leaseColdSamples, 2*time.Second)
	}
	clk := fakeFairClock(d)
	bg := context.Background()
	lightP := xuPrincipal("light", "tenant-a")
	heavyP := xuPrincipal("heavy", "tenant-a")
	heavyP.FairWeight = 90
	var light []*Lease
	defer func() {
		for _, l := range light {
			l.Release()
		}
	}()
	light = append(light, mustAcquire(t, d, withPrincipal(bg, lightP), weightedReq(1)))
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, heavyP), d, "heavy", weightedReq(e), heavyCh)
	waitQueued(t, d, heavyCh, 1, "heavy head queued")
	l, _, err := d.TryAcquire(withPrincipal(bg, lightP), weightedReq(1))
	if err != nil {
		t.Fatalf("light's second request (its share %d): %v", light2, err)
	}
	light = append(light, l)
	passed := 1
	lightCh := make(chan admission, 1)
	agedAt := e // units passed when the head ages
	if warm {
		agedAt = 2
	}
	for ; passed < agedAt; passed++ {
		startWaiter(withPrincipal(ctx, lightP), d, "light", weightedReq(1), lightCh)
		waitQueued(t, d, lightCh, 2, "light request queued")
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
		a := recvAdmission(t, lightCh, "light after the release")
		if a.err != nil {
			t.Fatalf("warm=%v before the threshold (%d units passed): light: %v", warm, passed, a.err)
		}
		light = append(light, a.lease)
	}
	startWaiter(withPrincipal(ctx, lightP), d, "light", weightedReq(1), lightCh)
	waitQueued(t, d, lightCh, 2, "light request queued at the threshold")
	for len(light) > 0 {
		clk.Add(int64(time.Second))
		light[0].Release()
		light = light[1:]
		if len(light) > 0 {
			if n := waitingExternal(d); n != 2 {
				t.Fatalf("warm=%v after %d units passed: waiting %d, want 2 — the head has not aged (the busy consumer took the unit)", warm, passed, n)
			}
			continue
		}
		select {
		case a := <-heavyCh:
			if a.err != nil {
				t.Fatalf("heavy w=%d: %v", e, a.err)
			}
			t.Logf("warm=%v: heavy w=%d admitted after %d units went to light", warm, e, passed)
			a.lease.Release()
			if a := recvAdmission(t, lightCh, "light after the heavy head"); a.lease != nil {
				a.lease.Release()
			}
			return
		case a := <-lightCh:
			t.Fatalf("warm=%v: light got the unit (err %v) when the heavy head fits", warm, a.err)
		case <-time.After(headGateSlack):
			t.Fatalf("warm=%v: heavy w=%d fits and still waits %v later", warm, e, headGateSlack)
		}
	}
}

// TestExternalAgedHeadTimeThreshold is the gate of review 1 F-3 (lead
// decision Q1 (c), k · L_p50): on a warm lease window the heavy head of the
// slow-turnover configuration ages one L_p50 after its push, long before E
// units passed it; on a cold window the turnover count alone ages it.
func TestExternalAgedHeadTimeThreshold(t *testing.T) {
	t.Run("warm", func(t *testing.T) { agingTimeCase(t, true) })
	t.Run("cold", func(t *testing.T) { agingTimeCase(t, false) })
}

// TestExternalAgedLightHeadsKeepTheShareOrder is the gate of review 1 F-2
// (probe TestReviewAgedOverShareHeadPassesWithinShareTenant, adopted): the
// aging rule must not lift the share order for a backlog of light heads.
// Tenant-x's consumer x holds all of E = 5 with seven light heads queued;
// tenant-y ran before (200 unit-seconds, more slot time than tenant-x).
// Phase B: five cycles — y queues, an x lease ends, y (within its share) gets
// it, y releases, x's queue gets it back (E units went to others than x).
// Phase C: y queues again holding nothing; when an x lease ends, the unit
// goes to y (within its share 3), not to x (holding 4, over it). The rule
// before the hardening aged x's backlog heads and handed it to x (the
// tenant with less slot time).
func TestExternalAgedLightHeadsKeepTheShareOrder(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	y0 := mustAcquire(t, d, xctx(bg, "y", "tenant-y"), weightedReq(1))
	clk.Add(int64(200 * time.Second))
	y0.Release()
	clk.Add(int64(time.Second))
	var xl []*Lease
	defer func() {
		for _, l := range xl {
			l.Release()
		}
	}()
	for i := 0; i < weightE; i++ {
		xl = append(xl, mustAcquire(t, d, xctx(bg, "x", "tenant-x"), weightedReq(1)))
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	xCh := make(chan admission, 64)
	const xQueued = 7
	for i := 0; i < xQueued; i++ {
		startWaiter(xctx(ctx, "x", "tenant-x"), d, "x", weightedReq(1), xCh)
		waitFor(t, "x queued", func() bool { return waitingExternal(d) == i+1 })
	}
	yCh := make(chan admission, 1)
	// Phase B: E cycles — y queues, an x lease ends ⇒ y gets it (within vs
	// over); y releases ⇒ x's queue gets it back.
	for cycle := 0; cycle < weightE; cycle++ {
		startWaiter(xctx(ctx, "y", "tenant-y"), d, "y", weightedReq(1), yCh)
		waitFor(t, "y queued", func() bool { return waitingExternal(d) == xQueued+1 })
		clk.Add(int64(time.Second))
		xl[0].Release()
		xl = xl[1:]
		a := recvAdmission(t, yCh, "y in phase B")
		if a.err != nil {
			t.Fatalf("phase B y: %v", a.err)
		}
		clk.Add(int64(time.Second))
		a.lease.Release()
		b := recvAdmission(t, xCh, "x back")
		if b.err != nil {
			t.Fatalf("phase B x: %v", b.err)
		}
		xl = append(xl, b.lease)
		startWaiter(xctx(ctx, "x", "tenant-x"), d, "x", weightedReq(1), xCh)
		waitFor(t, "x requeued", func() bool { return waitingExternal(d) == xQueued })
	}
	// Phase C: y (holds 0, within its share 3) queues; x holds 5 (over 3).
	startWaiter(xctx(ctx, "y", "tenant-y"), d, "y", weightedReq(1), yCh)
	waitFor(t, "y queued C", func() bool { return waitingExternal(d) == xQueued+1 })
	clk.Add(int64(time.Second))
	xl[0].Release()
	xl = xl[1:]
	select {
	case a := <-yCh:
		if a.err != nil {
			t.Fatalf("phase C: y: %v", a.err)
		}
		a.lease.Release()
	case b := <-xCh:
		if b.lease != nil {
			xl = append(xl, b.lease)
		}
		ts := target(t, d)
		t.Fatalf("phase C: x (tenant above its share 3, holds %d) got the freed unit before y (tenant within its share, holds 0) — aged light heads lift the share order; held %d", len(xl), ts.HeldExternalWeight)
	case <-time.After(time.Second):
		t.Fatalf("phase C: nobody admitted")
	}
}
