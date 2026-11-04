package dispatch

// A03-W4 gates (design/03 §4.5, §4.2 work conservation; masterplan §3.1
// "Self-Service-Mint vervielfacht Anteil ⇒ rot"): the external fairness of
// one target. Shares tenant → consumer (fair_weight), the slot-time pick and
// meter, the weight-aware head (d1: a head that does not fit now does not
// block another bucket; d2: in o1 background does not take units a waiting
// external head needs). Every gate in this file uses only the stock API and
// reaches the A03-W4 test hooks through interface assertions, so the very
// same file runs against a tree without them and measures that tree (the
// red run of the gate protocol); a hook the gate needs and does not find
// fails the gate.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fairClockSetter is the test-only clock seam of the fairness meter (A03-W4,
// export_test.go): the slot-time meter and the pick read it instead of the
// admission instant, production stays on time.Now.
type fairClockSetter interface {
	setFairClockForTest(now func() time.Time)
}

// fairAuditor recomputes the fairness bookkeeping of one target from the
// lease register and the external queue under the dispatcher mutex: "" when
// the maintained counters (heldBy per consumer and tenant, activity, Σ
// fair_weight, the active tenant count, the head-weight minimum) equal the
// recomputation, else the first difference. records is the number of
// fairness records left (served windows included).
type fairAuditor interface {
	fairAuditForTest(origin string) (diff string, records int)
}

// simClient is one closed-loop external client of the fairness simulation:
// acquire, hold for hold on the fake clock, release, acquire again.
type simClient struct {
	consumer, tenant string
	fw, weight       int
	hold             time.Duration
}

// simShares is the slot time (lease duration × weight, in unit-seconds on
// the fake clock) the simulation measured per consumer and per tenant — the
// test's own bookkeeping, not the dispatcher's meter.
type simShares struct {
	consumer map[string]float64
	tenant   map[string]float64
	total    float64
	releases int
	seamed   bool
}

// runFairSim drives clients against one target for horizon of fake time: a
// discrete-event loop over the real Acquire/Release path. The clients start
// one by one in slice order (a deterministic initial state); after every
// event the loop waits until each client either holds a reported lease or
// waits in the external queue, then advances the fake clock to the next
// lease end and releases that lease (ties: lower client index first). The
// caps are off and the wait budget is far above the real run time, so the
// pick alone decides who is served.
func runFairSim(t *testing.T, pol Policy, clients []simClient, horizon time.Duration) simShares {
	t.Helper()
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	s.ExternalMaxWait = 10 * time.Minute
	d, _ := newTestDispatcher(t, s, pol)
	var clk atomic.Int64
	start := time.Now()
	clk.Store(start.UnixNano())
	fake := func() time.Time { return time.Unix(0, clk.Load()) }
	out := simShares{consumer: map[string]float64{}, tenant: map[string]float64{}}
	if cs, ok := any(d).(fairClockSetter); ok {
		cs.setFairClockForTest(fake)
		out.seamed = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	type adm struct {
		c int
		l *Lease
	}
	admCh := make(chan adm, len(clients))
	rel := make([]chan struct{}, len(clients))
	var failed atomic.Value
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	active := make(map[int]time.Time)
	now := time.Unix(0, clk.Load())
	settle := func(n int) {
		deadline := time.Now().Add(20 * time.Second)
		for {
		drain:
			for {
				select {
				case a := <-admCh:
					active[a.c] = now
				default:
					break drain
				}
			}
			if msg := failed.Load(); msg != nil {
				t.Fatal(msg)
			}
			if len(active)+target(t, d).External.Waiting == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("simulation did not settle: %d active + %d waiting != %d clients", len(active), target(t, d).External.Waiting, n)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
	for i, c := range clients {
		rel[i] = make(chan struct{})
		wg.Add(1)
		go func(i int, c simClient) {
			defer wg.Done()
			cctx := withPrincipal(ctx, Principal{ConsumerID: c.consumer, TenantID: c.tenant, ConsumerKind: "app", FairWeight: c.fw})
			req := weightedReq(c.weight)
			for {
				l, _, err := d.Acquire(cctx, req)
				if err != nil {
					if ctx.Err() == nil {
						failed.CompareAndSwap(nil, fmt.Sprintf("client %d (%s): %v", i, c.consumer, err))
					}
					return
				}
				admCh <- adm{c: i, l: l}
				select {
				case <-rel[i]:
					l.Release()
				case <-ctx.Done():
					l.Release()
					return
				}
			}
		}(i, c)
		settle(i + 1)
	}
	end := start.Add(horizon)
	account := func(c int, until time.Time) {
		v := until.Sub(active[c]).Seconds() * float64(clients[c].weight)
		out.consumer[clients[c].consumer] += v
		out.tenant[clients[c].tenant] += v
		out.total += v
	}
	for {
		settle(len(clients))
		next := -1
		var nextAt time.Time
		for c, at := range active {
			e := at.Add(clients[c].hold)
			if next < 0 || e.Before(nextAt) || (e.Equal(nextAt) && c < next) {
				next, nextAt = c, e
			}
		}
		if next < 0 || nextAt.After(end) {
			break
		}
		now = nextAt
		clk.Store(now.UnixNano())
		account(next, now)
		delete(active, next)
		out.releases++
		rel[next] <- struct{}{}
	}
	for c := range active {
		account(c, end)
	}
	t.Logf("fairness clock seam present: %v; %d releases over %v of fake time", out.seamed, out.releases, horizon)
	return out
}

// sortedShares renders a share map as "key=pct" in key order.
func sortedShares(m map[string]float64, total float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for _, k := range keys {
		out += fmt.Sprintf(" %s=%.1f%%", k, 100*m[k]/total)
	}
	return out
}

// simHolds are the four lease durations every simulated consumer cycles
// through — the same multiset for every consumer, so a pick that serves
// clients equally (FIFO) yields slot time proportional to the client count.
var simHolds = []time.Duration{6 * time.Second, 8 * time.Second, 10 * time.Second, 12 * time.Second}

// TestExternalFairnessTenantShareResistsConsumerMint is gate (a), masterplan
// "Self-Service-Mint vervielfacht Anteil ⇒ rot" (design/03 X15): E = 5
// (S = 7, R = 2) under saturation for 30 min of fake time; tenant A runs ten
// consumers, tenant B one, every consumer four clients. FIFO serves clients
// equally — A:B ≈ 10:1. The two-stage pick (tenant first, then consumer)
// gives tenant B 45–55 % of the slot time: minting consumers buys a tenant
// no larger share. E is odd on purpose: the two tenant shares (⌈5/2⌉ = 3
// each) overlap, so the contested unit goes by the tenant slot time — a pick
// that ordered consumers before tenants would hand it to tenant A's ten
// barely served consumers (review F2).
func TestExternalFairnessTenantShareResistsConsumerMint(t *testing.T) {
	var clients []simClient
	for j, h := range simHolds {
		clients = append(clients, simClient{consumer: "b0", tenant: "tenant-b", fw: 1, weight: 1, hold: h})
		for c := 0; c < 10; c++ {
			clients = append(clients, simClient{consumer: fmt.Sprintf("a%d", c), tenant: "tenant-a", fw: 1, weight: 1, hold: simHolds[(j+c)%len(simHolds)]})
		}
	}
	got := runFairSim(t, partitionPolicy(7, 2), clients, 30*time.Minute)
	b := got.tenant["tenant-b"] / got.total
	t.Logf("slot time per tenant:%s; per consumer:%s", sortedShares(got.tenant, got.total), sortedShares(got.consumer, got.total))
	if b < 0.45 || b > 0.55 {
		t.Fatalf("tenant B (one consumer) got %.1f%% of the slot time against tenant A (ten consumers), want 45–55%% — consumer mint multiplies the tenant share", 100*b)
	}
}

// TestExternalFairnessMonopolistYields is gate (b): E = 5 (S = 7, R = 2),
// consumer K1 holds five leases (60 s on the fake clock), K1's sixth acquire
// waits before K2's. FIFO hands the next release to K1; the share pick hands
// it to K2 (below its share) and K1's sixth keeps waiting — a monopolist's
// hold shrinks by attrition, no lease is aborted.
func TestExternalFairnessMonopolistYields(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	var clk atomic.Int64
	clk.Store(time.Now().UnixNano())
	if cs, ok := any(d).(fairClockSetter); ok {
		cs.setFairClockForTest(func() time.Time { return time.Unix(0, clk.Load()) })
	}
	var k1 []*Lease
	for i := 0; i < weightE; i++ {
		k1 = append(k1, mustAcquire(t, d, xctx(context.Background(), "k1", "tenant-a"), externalReq()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "k1-sixth", externalReq(), ch)
	waitFor(t, "K1's sixth acquire queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(ctx, "k2", "tenant-a"), d, "k2", externalReq(), ch)
	waitFor(t, "K2 queued behind it", func() bool { return waitingExternal(d) == 2 })
	clk.Add(int64(60 * time.Second))
	k1[0].Release()
	got := recvAdmission(t, ch, "handoff after one K1 release")
	if got.err != nil || got.label != "k2" {
		t.Fatalf("next release went to %s (err %v), want k2 — the monopolist K1 (5 of E = 5 held) is served before a consumer below its share", got.label, got.err)
	}
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldExternalWeight != weightE {
		t.Fatalf("after the handoff: waiting=%d HeldExternalWeight=%d, want 1/%d (K1's sixth keeps waiting)", ts.External.Waiting, ts.HeldExternalWeight, weightE)
	}
	got.lease.Release()
	sixth := recvAdmission(t, ch, "K1's sixth after K2's release")
	if sixth.err != nil {
		t.Fatalf("K1's sixth: %v", sixth.err)
	}
	sixth.lease.Release()
	for _, l := range k1[1:] {
		l.Release()
	}
}

// TestExternalFairnessSlotTimeNotPickCount is gate (c): 120-s against 2-s
// leases on E = 3 (S = 5, R = 2), one tenant, equal fair_weight, four clients
// each, 30 min of fake time. A pick that rotates per pick (FIFO, principal
// round robin) lets the 120-s consumer hold ≈ 98 % of the slot time; the
// slot-time pick keeps each consumer within 40–60 %. The consumer shares
// (⌈3/2⌉ = 2 each) overlap, so the third unit goes by slot time — the shares
// alone would not carry the gate (review F2).
func TestExternalFairnessSlotTimeNotPickCount(t *testing.T) {
	var clients []simClient
	for i := 0; i < 4; i++ {
		clients = append(clients,
			simClient{consumer: "k120", tenant: "tenant-a", fw: 1, weight: 1, hold: 120 * time.Second},
			simClient{consumer: "k2", tenant: "tenant-a", fw: 1, weight: 1, hold: 2 * time.Second})
	}
	got := runFairSim(t, partitionPolicy(5, 2), clients, 30*time.Minute)
	t.Logf("slot time per consumer:%s", sortedShares(got.consumer, got.total))
	for _, c := range []string{"k120", "k2"} {
		if f := got.consumer[c] / got.total; f < 0.40 || f > 0.60 {
			t.Fatalf("consumer %s holds %.1f%% of the slot time, want 40–60%% — the pick counts picks, not slot time", c, 100*f)
		}
	}
}

// TestExternalFairnessWeightThreeToOne is gate (d): fair_weight 3 against 1
// in one tenant on E = 5 (S = 7, R = 2), five clients each, 30 min of fake
// time — the slot-time ratio is 3:1 ± 10 %. A pick that ignores fair_weight
// yields 1:1. The consumer shares (⌈5·3/4⌉ = 4 and ⌈5·1/4⌉ = 2) overlap, so
// the contested unit goes by slot time per fair weight — a pick that ordered
// by raw slot time would settle near 1.5:1 (review F2).
func TestExternalFairnessWeightThreeToOne(t *testing.T) {
	var clients []simClient
	for i := 0; i < 5; i++ {
		h := simHolds[i%len(simHolds)]
		clients = append(clients,
			simClient{consumer: "heavy", tenant: "tenant-a", fw: 3, weight: 1, hold: h},
			simClient{consumer: "light", tenant: "tenant-a", fw: 1, weight: 1, hold: h})
	}
	got := runFairSim(t, partitionPolicy(7, 2), clients, 30*time.Minute)
	ratio := got.consumer["heavy"] / got.consumer["light"]
	t.Logf("slot time per consumer:%s; ratio heavy:light = %.3f", sortedShares(got.consumer, got.total), ratio)
	if ratio < 2.7 || ratio > 3.3 {
		t.Fatalf("fair_weight 3:1 yields a slot-time ratio of %.3f, want 3 ± 10%%", ratio)
	}
}

// TestExternalFairnessConservesWork is gate (e): the shares order the pick,
// they never idle a unit. A single identity fills E = 6 (S = 8, R = 2) at
// once; and through the pick: while tenant B holds one unit and waits for
// nothing, tenant A's only consumer takes all five freed units although its
// tenant share is 3 (stage (ii) of the pick) — HeldExternalWeight == E.
func TestExternalFairnessConservesWork(t *testing.T) {
	const e = 6
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(8, 2))
	var solo []*Lease
	for i := 0; i < e; i++ {
		solo = append(solo, mustAcquire(t, d, xctx(context.Background(), "solo", "tenant-a"), externalReq()))
	}
	if ts := target(t, d); ts.HeldExternalWeight != e || ts.External.Waiting != 0 {
		t.Fatalf("single identity: HeldExternalWeight=%d waiting=%d, want %d/0", ts.HeldExternalWeight, ts.External.Waiting, e)
	}
	for _, l := range solo {
		l.Release()
	}

	var occ []*Lease
	for i := 0; i < e; i++ {
		occ = append(occ, mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-b"), externalReq()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, e)
	for i := 0; i < e; i++ {
		startWaiter(xctx(ctx, "solo", "tenant-a"), d, fmt.Sprintf("solo-%d", i), externalReq(), ch)
		n := i + 1
		waitFor(t, "solo waiter queued", func() bool { return waitingExternal(d) == n })
	}
	var got []*Lease
	for i := 0; i < e-1; i++ {
		occ[i].Release()
		a := recvAdmission(t, ch, "solo handoff")
		if a.err != nil {
			t.Fatalf("solo handoff %d: %v", i, a.err)
		}
		got = append(got, a.lease)
	}
	if ts := target(t, d); ts.HeldExternalWeight != e || ts.External.Waiting != 1 {
		t.Fatalf("over its share with nobody else waiting: HeldExternalWeight=%d waiting=%d, want %d/1 — the shares idled a unit", ts.HeldExternalWeight, ts.External.Waiting, e)
	}
	occ[e-1].Release()
	last := recvAdmission(t, ch, "last solo handoff")
	if last.err != nil {
		t.Fatalf("last solo handoff: %v", last.err)
	}
	last.lease.Release()
	for _, l := range got {
		l.Release()
	}
}

// TestExternalPickShareFloorIsOneUnit pins the consumer stage of the pick
// as heldBy[c] + w ≤ consumerShare(c) (design/03 §4.5 no. 3 writes "<"; with
// "<" the floor max(1, …) of the share formula could never admit a consumer
// whose share is one unit, so every consumer of a tenant with more consumers
// than tenant-share units would sit in stage (ii) for good). E = 6 (S = 8,
// R = 2), two tenants ⇒ tenant share 3; tenant A has three consumers ⇒ one
// unit each, tenant B one ⇒ three. B's lease has run 100 s, A's two leases
// 1 s; background holds the other five units. B's second acquire queues
// before A's third consumer. When a background lease ends, A's third
// consumer (0 + 1 ≤ 1, tenant A at 2 of 3, the tenant with less slot time)
// is served; with "<" — and under FIFO — it would be B (1 + 1 < 3).
func TestExternalPickShareFloorIsOneUnit(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(8, 2))
	var clk atomic.Int64
	clk.Store(time.Now().UnixNano())
	if cs, ok := any(d).(fairClockSetter); ok {
		cs.setFairClockForTest(func() time.Time { return time.Unix(0, clk.Load()) })
	}
	bg := context.Background()
	b1 := mustAcquire(t, d, xctx(bg, "b1", "tenant-b"), externalReq())
	defer b1.Release()
	clk.Add(int64(100 * time.Second))
	for _, c := range []string{"a1", "a2"} {
		l := mustAcquire(t, d, xctx(bg, c, "tenant-a"), externalReq())
		defer l.Release()
	}
	var fill []*Lease
	for i := 0; i < 5; i++ {
		fill = append(fill, mustAcquire(t, d, bg, backgroundReq()))
	}
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "b1", "tenant-b"), d, "b1-second", externalReq(), ch)
	waitFor(t, "B's second acquire queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(ctx, "a3", "tenant-a"), d, "a3", externalReq(), ch)
	waitFor(t, "A's third consumer queued", func() bool { return waitingExternal(d) == 2 })
	fill[0].Release()
	got := recvAdmission(t, ch, "handoff after a background release")
	if got.err != nil || got.label != "a3" {
		t.Fatalf("the freed unit went to %s (err %v), want a3 — a consumer whose share is one unit holds 0 and is within it", got.label, got.err)
	}
	got.lease.Release()
	second := recvAdmission(t, ch, "B's second after a3's release")
	if second.err != nil {
		t.Fatalf("B's second: %v", second.err)
	}
	second.lease.Release()
	for _, l := range fill[1:] {
		l.Release()
	}
}

// fakeFairClock installs a fake fairness clock on d when the seam exists
// and returns the clock to advance (the dispatcher keeps time.Now without it).
func fakeFairClock(d *Dispatcher) *atomic.Int64 {
	var clk atomic.Int64
	clk.Store(time.Now().UnixNano())
	if cs, ok := any(d).(fairClockSetter); ok {
		cs.setFairClockForTest(func() time.Time { return time.Unix(0, clk.Load()) })
	}
	return &clk
}

// fillWithBackground acquires n background leases on d (released at cleanup).
func fillWithBackground(t *testing.T, d *Dispatcher, n int) []*Lease {
	t.Helper()
	var out []*Lease
	for i := 0; i < n; i++ {
		l := mustAcquire(t, d, context.Background(), backgroundReq())
		t.Cleanup(l.Release)
		out = append(out, l)
	}
	return out
}

// TestExternalPickTenantShareFollowsActiveTenants pins tenantShare =
// ⌈E / T_act⌉ (review F2/R06): E = 5 (S = 7, R = 2), tenant A's K1 holds 3
// units (1 s of slot time), tenant B's K2 has served 100 s and holds none;
// background holds the rest of S. K1's fourth acquire queues before K2's.
// With two active tenants the tenant share is 3: K1's fourth is above it,
// K2 is within it — K2 is served, although tenant B has far more slot time.
// A share of E (T_act ignored) would put both within it and hand the unit to
// the tenant with less slot time, K1; FIFO hands it to K1 too.
func TestExternalPickTenantShareFollowsActiveTenants(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	past := mustAcquire(t, d, xctx(bg, "k2", "tenant-b"), externalReq())
	clk.Add(int64(100 * time.Second))
	past.Release()
	for i := 0; i < 3; i++ {
		l := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), externalReq())
		t.Cleanup(l.Release)
	}
	fill := fillWithBackground(t, d, weightSlots-3)
	clk.Add(int64(time.Second))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "k1-fourth", externalReq(), ch)
	waitFor(t, "K1's fourth queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(ctx, "k2", "tenant-b"), d, "k2", externalReq(), ch)
	waitFor(t, "K2 queued", func() bool { return waitingExternal(d) == 2 })
	fill[0].Release()
	got := recvAdmission(t, ch, "handoff after a background release")
	if got.err != nil || got.label != "k2" {
		t.Fatalf("the freed unit went to %s (err %v), want k2 — with two active tenants the tenant share is ⌈5/2⌉ = 3 and K1 holds 3", got.label, got.err)
	}
	got.lease.Release()
	if a := recvAdmission(t, ch, "K1's fourth"); a.lease != nil {
		a.lease.Release()
	}
}

// TestExternalFairWeightChangeWhileActive pins the fair_weight update of an
// active consumer (review F5/R09; fair_weight is writable while keys are in
// use): E = 4 (S = 6, R = 2), one tenant; H and L hold two units each,
// acquired with fair_weight 1. L queues one more (fw 1), then H one with
// fair_weight 3 — the newest value counts: shares ⌈4·3/4⌉ = 3 for H and 1 for
// L, so after one of L's leases ends, H (2 + 1 ≤ 3) is served and L (1 + 1 >
// 1) is not. With H's weight left at 1 both shares would be 2 and L would be
// served — like FIFO.
func TestExternalFairWeightChangeWhileActive(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(6, 2))
	fakeFairClock(d)
	bg := context.Background()
	pc := func(ctx context.Context, consumer string, fw int) context.Context {
		return withPrincipal(ctx, Principal{ConsumerID: consumer, TenantID: "tenant-a", ConsumerKind: "app", FairWeight: fw})
	}
	var l []*Lease
	for _, c := range []string{"h", "h", "l", "l"} {
		l = append(l, mustAcquire(t, d, pc(bg, c, 1), externalReq()))
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(pc(ctx, "l", 1), d, "l", externalReq(), ch)
	waitFor(t, "L queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(pc(ctx, "h", 3), d, "h", externalReq(), ch)
	waitFor(t, "H queued with fair_weight 3", func() bool { return waitingExternal(d) == 2 })
	l[2].Release()
	got := recvAdmission(t, ch, "handoff after one of L's releases")
	if got.err != nil || got.label != "h" {
		t.Fatalf("the freed unit went to %s (err %v), want h — H's fair_weight changed to 3 while it was active", got.label, got.err)
	}
	got.lease.Release()
	if a := recvAdmission(t, ch, "L's waiter"); a.lease != nil {
		a.lease.Release()
	}
	for _, x := range []*Lease{l[0], l[1], l[3]} {
		x.Release()
	}
}

// TestExternalReservationAlsoBindsTheAgingEscape pins d2 on the aging path
// (review F5/R12): S = 7, R = 2 with background_aging_after armed, seven
// background leases, a waiting external head of weight E (it fits E, not S),
// interactive demand in the house and an aged background waiter. A released
// background unit is not taken by the aging escape — the reservation is
// class order, the escape only overrules the herald; once the background
// side is down to R the head is admitted.
func TestExternalReservationAlsoBindsTheAgingEscape(t *testing.T) {
	s := DefaultSettings()
	s.BackgroundAgingAfter = time.Millisecond
	d, _ := newTestDispatcher(t, s, weightPolicy())
	var bl []*Lease
	for i := 0; i < weightSlots; i++ {
		bl = append(bl, mustAcquire(t, d, context.Background(), backgroundReq()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "heavy", weightedReq(weightE), heavyCh)
	waitFor(t, "heavy head queued", func() bool { return waitingExternal(d) == 1 })
	done := d.InteractiveArrived()
	defer done()
	bgCh := make(chan admission, 1)
	startWaiter(ctx, d, "aged", backgroundReq(), bgCh)
	waitFor(t, "background waiter queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(10 * time.Millisecond)
	bl[0].Release()
	if ts := target(t, d); ts.Background.Waiting != 1 || ts.HeldWeight != weightSlots-1 {
		t.Fatalf("after one background release: background waiting=%d HeldWeight=%d, want 1/%d — the aging escape took a unit reserved for the external head",
			ts.Background.Waiting, ts.HeldWeight, weightSlots-1)
	}
	for _, l := range bl[1 : weightSlots-weightReserved] {
		l.Release()
	}
	heavy := recvAdmission(t, heavyCh, "heavy head once background is down to R")
	if heavy.err != nil {
		t.Fatalf("heavy head: %v", heavy.err)
	}
	heavy.lease.Release()
	done()
	if a := recvAdmission(t, bgCh, "aged background waiter"); a.lease != nil {
		a.lease.Release()
	}
	for _, l := range bl[weightSlots-weightReserved:] {
		l.Release()
	}
}

// TestExternalSlotTimeWindowRunsOut pins the fixed window of the slot-time
// meter (review F5/R08): E = 1 (S = 2, R = 1); K1 settled 100 s, K2 1 s, both
// in tenants of their own; K1 queues before K2 behind an occupying lease.
// Within the window the release serves K2 (less slot time); after the window
// (usage window 1 h) both read 0 and arrival order serves K1.
func TestExternalSlotTimeWindowRunsOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance time.Duration
		want    string
	}{
		{"within the window", time.Minute, "k2"},
		{"after the window", defaultUsageWindow + time.Minute, "k1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
			clk := fakeFairClock(d)
			bg := context.Background()
			for _, step := range []struct {
				consumer string
				hold     time.Duration
			}{{"k1", 100 * time.Second}, {"k2", time.Second}} {
				l := mustAcquire(t, d, xctx(bg, step.consumer, "tenant-"+step.consumer), externalReq())
				clk.Add(int64(step.hold))
				l.Release()
			}
			occ := mustAcquire(t, d, xctx(bg, "occ", "tenant-occ"), externalReq())
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 2)
			for i, c := range []string{"k1", "k2"} {
				startWaiter(xctx(ctx, c, "tenant-"+c), d, c, externalReq(), ch)
				n := i + 1
				waitFor(t, c+" queued", func() bool { return waitingExternal(d) == n })
			}
			clk.Add(int64(tc.advance))
			occ.Release()
			got := recvAdmission(t, ch, "handoff")
			if got.err != nil || got.label != tc.want {
				t.Fatalf("%s: handoff = %s (err %v), want %s", tc.name, got.label, got.err, tc.want)
			}
			got.lease.Release()
			if a := recvAdmission(t, ch, "second"); a.lease != nil {
				a.lease.Release()
			}
		})
	}
}

// headGateHold is the lease duration of the background workers of the head
// gates — "one background hold time" of gates (f) and (g); headGateSlack is
// the dispatch-gate bound for "at once" (Lead Q19: 50 ms, not 5 ms).
const (
	headGateHold  = 20 * time.Millisecond
	headGateSlack = 50 * time.Millisecond
	headGateBG    = 16
)

// backgroundLoad runs n background workers on d that acquire, hold for
// hold and release in a loop; check runs after every admission. stop ends
// them and waits.
func backgroundLoad(d *Dispatcher, n int, hold time.Duration, check func()) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				l, _, err := d.Acquire(ctx, backgroundReq())
				if err != nil {
					return
				}
				if check != nil {
					check()
				}
				select {
				case <-time.After(hold):
				case <-ctx.Done():
				}
				l.Release()
			}
		}()
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// timedAcquire runs one external acquire for consumer of tenant and returns
// its lease, duration and error.
func timedAcquire(d *Dispatcher, consumer, tenant string, req Request) (*Lease, time.Duration, error) {
	ctx, cancel := context.WithTimeout(xctx(context.Background(), consumer, tenant), 10*time.Second)
	defer cancel()
	start := time.Now()
	l, _, err := d.Acquire(ctx, req)
	return l, time.Since(start), err
}

// TestExternalHeavyHeadDoesNotBlockOtherBuckets is gate (f), (d1), from
// A03-W7 review 2 F-A: S = 7, R = 2 (E = 5), 16 background workers; K0 holds
// a w = 1 lease, so K1's w = E head does not fit (1 + 5 > E) for the whole
// gate, then K2's w = 1 arrives 100 ms later. Under FIFO across the buckets
// K2 waits until the head's 300-ms budget ends (≈ 200 ms after its arrival);
// with the weight-aware pick the head's bucket is skipped and K2 is admitted
// within one background hold time (+ the 50-ms bound).
func TestExternalHeavyHeadDoesNotBlockOtherBuckets(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	k0 := mustAcquire(t, d, xctx(context.Background(), "k0", "tenant-a"), weightedReq(1))
	defer k0.Release()
	stop := backgroundLoad(d, headGateBG, headGateHold, nil)
	defer stop()
	waitFor(t, "background saturates the target", func() bool { return target(t, d).HeldWeight == weightSlots })

	heavyCh := make(chan error, 1)
	go func() {
		req := weightedReq(weightE)
		req.MaxWait = 300 * time.Millisecond
		l, _, err := timedAcquire(d, "k1", "tenant-a", req)
		if l != nil {
			l.Release()
		}
		heavyCh <- err
	}()
	waitFor(t, "heavy head queued", func() bool { return waitingExternal(d) == 1 })
	time.Sleep(100 * time.Millisecond)
	req := weightedReq(1)
	req.MaxWait = 2 * time.Second
	l, took, err := timedAcquire(d, "k2", "tenant-b", req)
	if l != nil {
		l.Release()
	}
	t.Logf("K2 (w=1) behind a w=E head that does not fit: err=%v after %v (bound %v)", err, took, headGateHold+headGateSlack)
	if err != nil || took > headGateHold+headGateSlack {
		t.Fatalf("K2 w=1: err=%v after %v, want admitted within one background hold time + %v — the inadmissible w=E head of another bucket blocks it", err, took, headGateSlack)
	}
	<-heavyCh
}

// TestExternalHeavyHeadReservesUnitsAgainstBackground is gate (g), (d2): the
// same fixture without K0 — K1's w = E head fits the external partition
// (heldExt 0) but not the slots while background holds them. Before A03-W4
// background takes every freed unit (work conservation) and the head ends
// with ErrWaitExpired; in o1 background now leaves the units the head needs
// and the head is admitted within one background hold time (+ 50 ms). In o2
// the behaviour is the stock one (background ranks first). Under an
// external flood plus the heavy head the background floor holds at every
// handoff: background waits only while the in-house side holds R units.
func TestExternalHeavyHeadReservesUnitsAgainstBackground(t *testing.T) {
	t.Run("o1 head admitted within one hold", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
		stop := backgroundLoad(d, headGateBG, headGateHold, nil)
		defer stop()
		waitFor(t, "background saturates the target", func() bool { return target(t, d).HeldWeight == weightSlots })
		req := weightedReq(weightE)
		req.MaxWait = 2 * time.Second
		l, took, err := timedAcquire(d, "k1", "tenant-a", req)
		if l != nil {
			l.Release()
		}
		t.Logf("o1 w=E head under %d background workers: err=%v after %v (bound %v)", headGateBG, err, took, headGateHold+headGateSlack)
		if err != nil || took > headGateHold+headGateSlack {
			t.Fatalf("o1 w=E head: err=%v after %v, want admitted within one background hold time + %v", err, took, headGateSlack)
		}
	})
	t.Run("o2 unchanged", func(t *testing.T) {
		s := DefaultSettings()
		s.RankOrder = mustRankOrder(t, ExternalOrderO2)
		d, _ := newTestDispatcher(t, s, weightPolicy())
		stop := backgroundLoad(d, headGateBG, headGateHold, nil)
		defer stop()
		waitFor(t, "background saturates the target", func() bool { return target(t, d).HeldWeight == weightSlots })
		const budget = 300 * time.Millisecond
		req := weightedReq(weightE)
		req.MaxWait = budget
		l, took, err := timedAcquire(d, "k1", "tenant-a", req)
		if l != nil {
			l.Release()
		}
		t.Logf("o2 w=E head: err=%v after %v", err, took)
		if !errors.Is(err, ErrWaitExpired) || took < budget {
			t.Fatalf("o2 w=E head: err=%v after %v, want ErrWaitExpired after its %v budget — o2 must keep background first", err, took, budget)
		}
		if ts := target(t, d); ts.ExternalRejects.Expired != 1 || ts.HeldExternal != 0 {
			t.Fatalf("o2: expired=%d heldExt=%d, want 1/0", ts.ExternalRejects.Expired, ts.HeldExternal)
		}
	})
	t.Run("background floor under flood and heavy head", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
		var fail atomic.Value
		check := func(where string) {
			ts := target(t, d)
			if ts.Background.Waiting > 0 && ts.HeldInternal < weightReserved {
				fail.CompareAndSwap(nil, fmt.Sprintf("%s: background waits (%d) while the in-house side holds %d < R = %d units", where, ts.Background.Waiting, ts.HeldInternal, weightReserved))
			}
		}
		stop := backgroundLoad(d, headGateBG, headGateHold, func() { check("background handoff") })
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for c := 0; c < 4; c++ {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				cctx := xctx(ctx, fmt.Sprintf("flood-%d", c), "tenant-b")
				rng := rand.New(rand.NewSource(int64(c)))
				for ctx.Err() == nil {
					l, _, err := d.Acquire(cctx, weightedReq(1))
					if err != nil {
						continue
					}
					check("external handoff")
					time.Sleep(time.Duration(1+rng.Intn(10)) * time.Millisecond)
					l.Release()
				}
			}(c)
		}
		heavyDone := make(chan struct{})
		go func() {
			defer close(heavyDone)
			for i := 0; i < 5 && ctx.Err() == nil; i++ {
				req := weightedReq(weightE)
				req.MaxWait = 200 * time.Millisecond
				if l, _, err := timedAcquire(d, "k1", "tenant-a", req); err == nil {
					check("heavy handoff")
					time.Sleep(5 * time.Millisecond)
					l.Release()
				}
			}
		}()
		sampleEnd := time.Now().Add(time.Second)
		for time.Now().Before(sampleEnd) {
			check("sampler")
			time.Sleep(200 * time.Microsecond)
		}
		cancel()
		wg.Wait()
		<-heavyDone
		stop()
		if msg := fail.Load(); msg != nil {
			t.Fatal(msg)
		}
	})
}

// TestExternalFairCountersDoNotDrift is gate (j): 4 000 random external
// sequences — admit/release, caller cancel while waiting (and racing the
// handoff), wait-budget expiry, and reaper passes that force-release every
// running lease — with random weights and fair_weights on S = 7, R = 2 next
// to background load; one in five acquires is a continuation (A03-W17, the
// class head — review 2 F-3 gap 2), the checker toggles the emergency stop
// (every waiter ends with ErrExternalNotOffered) and shrinks E to 2 and back
// (the overweight drain ends every waiter above it, continuations
// included). Under the dispatcher mutex the maintained bookkeeping (heldBy per
// consumer and tenant, activity, Σ fair_weight, active tenants, head-weight
// minimum) equals its recomputation from the lease register and the queue at
// every check; afterwards nothing is held, and after one usage window the
// reaper's sweep leaves no fairness record. Goroutines ±0. Run 3× with -race.
func TestExternalFairCountersDoNotDrift(t *testing.T) {
	const self = "TestExternalFairCountersDoNotDrift"
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	d, h := newTestDispatcher(t, s, weightPolicy())
	audit, ok := any(d).(fairAuditor)
	if !ok {
		t.Fatal("no fairness bookkeeping on this dispatcher (fairAuditForTest missing): heldBy per consumer and tenant does not exist")
	}
	var offset atomic.Int64
	if cs, ok := any(d).(fairClockSetter); ok {
		cs.setFairClockForTest(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
	}
	before := settledGateGoroutines(self)

	var fail atomic.Value
	report := func(format string, args ...any) { fail.CompareAndSwap(nil, fmt.Sprintf(format, args...)) }
	checkNow := func(where string) {
		if diff, _ := audit.fairAuditForTest(testOrigin); diff != "" {
			report("%s: %s", where, diff)
		}
	}
	stopBG := backgroundLoad(d, 4, 300*time.Microsecond, nil)
	stopCheck := make(chan struct{})
	checkDone := make(chan struct{})
	var reaps, stops, swaps atomic.Int64
	go func() {
		defer close(checkDone)
		for i := 1; ; i++ {
			select {
			case <-stopCheck:
				return
			default:
				checkNow("checker")
				if i%40 == 0 {
					// Past every reap reference (lease_max_age + grace): the
					// reaper settles each running lease through finishLocked.
					d.ReapForTest(time.Now().Add(time.Hour))
					reaps.Add(1)
					checkNow("after a reaper pass")
				}
				switch i % 150 {
				case 55: // emergency stop on and off: every external waiter ends
					off := s
					off.Enabled = false
					d.UpdateSettings(off)
					checkNow("under the emergency stop")
					d.UpdateSettings(s)
					stops.Add(1)
				case 110: // E 5 → 2 → 5: the overweight drain ends every waiter above 2
					d.UpdatePolicy(partitionPolicy(weightSlots, weightSlots-2))
					checkNow("after the shrink")
					d.UpdatePolicy(weightPolicy())
					swaps.Add(1)
				}
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	const workers, perWorker = 8, 500
	var wg sync.WaitGroup
	var outcomes [5]atomic.Int64 // admitted, canceled, expired, not offered (stop/drain), other
	var contAdmitted atomic.Int64
	for wkr := 0; wkr < workers; wkr++ {
		wg.Add(1)
		go func(wkr int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(1000 + wkr)))
			for i := 0; i < perWorker; i++ {
				c := rng.Intn(6)
				p := Principal{ConsumerID: fmt.Sprintf("c%d", c), TenantID: fmt.Sprintf("t%d", c%3), ConsumerKind: "app", FairWeight: rng.Intn(4)}
				req := weightedReq(1 + rng.Intn(3))
				req.Continuation = rng.Intn(5) == 0
				ctx, cancel := context.WithCancel(withPrincipal(context.Background(), p))
				switch rng.Intn(4) {
				case 0:
					req.MaxWait = time.Duration(1+rng.Intn(500)) * time.Microsecond
				case 1:
					time.AfterFunc(time.Duration(rng.Intn(800))*time.Microsecond, cancel)
				}
				l, _, err := d.Acquire(ctx, req)
				switch {
				case err == nil:
					outcomes[0].Add(1)
					if req.Continuation {
						contAdmitted.Add(1)
					}
					time.Sleep(time.Duration(rng.Intn(400)) * time.Microsecond)
					l.Release()
				case errors.Is(err, context.Canceled):
					outcomes[1].Add(1)
				case errors.Is(err, ErrWaitExpired):
					outcomes[2].Add(1)
				case errors.Is(err, ErrExternalNotOffered):
					outcomes[3].Add(1)
				default:
					outcomes[4].Add(1)
					report("worker %d: acquire: %v", wkr, err)
				}
				cancel()
				if i%50 == 0 {
					checkNow(fmt.Sprintf("worker %d step %d", wkr, i))
				}
			}
		}(wkr)
	}
	wg.Wait()
	close(stopCheck)
	<-checkDone
	stopBG()
	checkNow("after the load")
	if msg := fail.Load(); msg != nil {
		t.Fatal(msg)
	}
	reaped := d.Snapshot().ReapsTotal
	drains := h.count("waiting acquires ended after the capacity shrank")
	t.Logf("outcomes: admitted %d (continuations %d), canceled %d, expired %d, not offered %d, other %d; %d reaper passes reaped %d leases; %d emergency stops, %d shrinks (%d drained a waiter)",
		outcomes[0].Load(), contAdmitted.Load(), outcomes[1].Load(), outcomes[2].Load(), outcomes[3].Load(), outcomes[4].Load(), reaps.Load(), reaped, stops.Load(), swaps.Load(), drains)
	if outcomes[0].Load() == 0 || outcomes[1].Load() == 0 || outcomes[2].Load() == 0 || outcomes[3].Load() == 0 || contAdmitted.Load() == 0 || reaped == 0 || drains == 0 {
		t.Fatalf("stress vacuous: every outcome must occur: %d/%d/%d/%d, continuations admitted %d, reaped %d, drains %d",
			outcomes[0].Load(), outcomes[1].Load(), outcomes[2].Load(), outcomes[3].Load(), contAdmitted.Load(), reaped, drains)
	}
	if ts := target(t, d); ts.HeldWeight != 0 || ts.Held != 0 || ts.External.Waiting != 0 || externalQueueResidue(d) != 0 {
		t.Fatalf("after the load: HeldWeight=%d Held=%d waiting=%d residue=%d, want 0", ts.HeldWeight, ts.Held, ts.External.Waiting, externalQueueResidue(d))
	}
	_, records := audit.fairAuditForTest(testOrigin)
	offset.Store(int64(2 * defaultUsageWindow))
	d.ReapForTest(time.Now())
	diff, left := audit.fairAuditForTest(testOrigin)
	t.Logf("fairness records: %d before the window sweep, %d after", records, left)
	if diff != "" || left != 0 || records == 0 {
		t.Fatalf("after the window sweep: diff=%q records=%d (before %d), want no difference, 0 records left and a non-empty meter before", diff, left, records)
	}
	deadline := time.Now().Add(2 * time.Second)
	for gateGoroutines(self) != before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if after := gateGoroutines(self); after != before {
		t.Fatalf("goroutines before %d, after %d — want ±0", before, after)
	}
}
