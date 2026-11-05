package dispatch

// A03-W3 gates (design/03 §4.1–§4.3, §4.6, §4.14; DECISIONS A-E2): the
// external class XU. Its authorization (a consumer principal with a tenant
// and without a knowledge key, both doors), the identity bucket
// _consumer:<id> with its own cap ladder consumer → tenant → aggregate (C8),
// the internal partition R on slot-managed targets and the opt-in on
// self-admitting ones, the rank orders o1/o2 as data, the reaper-only cancel
// and the kill-switch drain that never passes an external waiter through.
// The principal travels ONLY in the ctx through the TestMain hook
// (principal_test.go), exactly like every other gate of this package.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// xuPrincipal is the shape handler.RequestPrincipal yields for a valid
// model-key request: consumer and tenant set, no knowledge key (I-K0).
func xuPrincipal(consumer, tenant string) Principal {
	return Principal{ConsumerID: consumer, TenantID: tenant, ConsumerKind: "app", FairWeight: 10}
}

func xctx(ctx context.Context, consumer, tenant string) context.Context {
	return withPrincipal(ctx, xuPrincipal(consumer, tenant))
}

func externalReq() Request {
	return Request{Target: Target{Origin: testOrigin}, Class: ClassExternal, Role: "test"}
}

// partitionPolicy is a slot-managed (L) target with the internal partition R.
func partitionPolicy(slots, reserved int) Policy {
	return Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: slots, InternalReserved: reserved}}}
}

func waitingExternal(d *Dispatcher) int {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == testOrigin {
			return ts.External.Waiting
		}
	}
	return 0
}

// errStillBlocked marks an acquire that was still waiting when the test gave
// up on it — a gate whose implementation broke into "queued" fails instead of
// hanging the package.
var errStillBlocked = errors.New("acquire still blocked")

// acquireWithin runs one Acquire and gives up after wait. The derived ctx has
// NO deadline (a deadline would become the lease's reap reference); it is
// canceled on the give-up path and at test cleanup.
func acquireWithin(t *testing.T, d *Dispatcher, ctx context.Context, req Request, wait time.Duration) (*Lease, context.Context, error) {
	t.Helper()
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	type result struct {
		l      *Lease
		runCtx context.Context
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		l, runCtx, err := d.Acquire(cctx, req)
		ch <- result{l, runCtx, err}
	}()
	select {
	case r := <-ch:
		return r.l, r.runCtx, r.err
	case <-time.After(wait):
		cancel()
		if r := <-ch; r.l != nil {
			r.l.Release() // the admission raced the give-up
		}
		return nil, nil, fmt.Errorf("%s after %v: %w", req.Class, wait, errStillBlocked)
	}
}

func mustAcquire(t *testing.T, d *Dispatcher, ctx context.Context, req Request) *Lease {
	t.Helper()
	l, _, err := acquireWithin(t, d, ctx, req, 3*time.Second)
	if err != nil {
		t.Fatalf("acquire %s: %v", req.Class, err)
	}
	return l
}

func recvAdmission(t *testing.T, ch chan admission, what string) admission {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return admission{}
	}
}

// TestExternalCannotStarveInternalBackground is R1 (masterplan §6): S = 4,
// R = 2, eight consumers in two tenants with four external acquires each, then
// one background acquire. The external partition E = S − R holds two slots;
// the background acquire is admitted at once because the waiting external
// heads are not admissible at the partition border (work conservation), and
// at every external handoff heldExt ≤ 2.
func TestExternalCannotStarveInternalBackground(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 32)
	for c := 1; c <= 8; c++ {
		tenant := "tenant-a"
		if c > 4 {
			tenant = "tenant-b"
		}
		for i := 1; i <= 4; i++ {
			startWaiter(xctx(ctx, fmt.Sprintf("c%d", c), tenant), d, fmt.Sprintf("c%d-%d", c, i), externalReq(), ch)
		}
	}
	waitFor(t, "32 external acquires settled", func() bool {
		ts := target(t, d)
		return ts.HeldExternal+ts.External.Waiting == 32
	})
	if ts := target(t, d); ts.HeldExternal != 2 || ts.Held != 2 || ts.HeldInternal != 0 {
		t.Errorf("external flood holds heldExt=%d held=%d heldInt=%d, want 2/2/0 (heldExt <= S-R)", ts.HeldExternal, ts.Held, ts.HeldInternal)
	}

	bctx, bcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer bcancel()
	bl, _, err := d.Acquire(bctx, backgroundReq())
	if err != nil {
		t.Fatalf("background acquire behind an external flood: %v — external load starved in-house background", err)
	}
	if ts := target(t, d); ts.HeldInternal != 1 || ts.HeldExternal != 2 {
		t.Fatalf("after the background admission heldInt=%d heldExt=%d, want 1/2", ts.HeldInternal, ts.HeldExternal)
	}

	var live []*Lease
	for i := 0; i < 2; i++ {
		live = append(live, recvAdmission(t, ch, "initial external admission").lease)
	}
	for served := 2; served < 32; served++ {
		live[0].Release()
		live = live[1:]
		a := recvAdmission(t, ch, "next external handoff")
		if a.err != nil || a.lease.Class() != ClassExternal {
			t.Fatalf("handoff %d: lease=%v err=%v", served, a.lease, a.err)
		}
		if ts := target(t, d); ts.HeldExternal > 2 || ts.Held > 4 {
			t.Fatalf("handoff %d: heldExt=%d held=%d, want heldExt <= 2 and held <= 4", served, ts.HeldExternal, ts.Held)
		}
		live = append(live, a.lease)
	}
	for _, l := range live {
		l.Release()
	}
	bl.Release()
}

func mustRankOrder(t *testing.T, name string) [numClasses]Class {
	t.Helper()
	o, ok := RankOrderFor(name)
	if !ok {
		t.Fatalf("RankOrderFor(%q) unknown", name)
	}
	return o
}

// orderFixture holds S = 3 with R = 1 by one external and two background
// leases and queues one external waiter of ANOTHER consumer, then one
// background waiter — the A-E2 input on which o1 and o2 must disagree.
func orderFixture(t *testing.T, d *Dispatcher, ctx context.Context, ch chan admission) (b1, b2, x1 *Lease) {
	t.Helper()
	b1 = mustAcquire(t, d, context.Background(), backgroundReq())
	b2 = mustAcquire(t, d, context.Background(), backgroundReq())
	x1 = mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), externalReq())
	startWaiter(xctx(ctx, "c2", "tenant-a"), d, "external", externalReq(), ch)
	waitFor(t, "external waiter queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(ctx, d, "background", backgroundReq(), ch)
	waitFor(t, "background waiter queued", func() bool { return waitingBackground(d) == 1 })
	return b1, b2, x1
}

// TestExternalOrderSwitchSteersHandoff is the A-E2 no. 1 gate: the same
// input, only the named order differs — o1 (I, XU, B) hands the freed slot to
// the external waiter, o2 (I, B, XU) to the background waiter.
func TestExternalOrderSwitchSteersHandoff(t *testing.T) {
	for _, tc := range []struct{ order, want string }{
		{order: ExternalOrderO1, want: "external"},
		{order: ExternalOrderO2, want: "background"},
	} {
		t.Run(tc.order, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, tc.order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan admission, 2)
			b1, b2, x1 := orderFixture(t, d, ctx, ch)
			b1.Release()
			got := recvAdmission(t, ch, "handoff after the background release")
			if got.err != nil || got.label != tc.want {
				t.Fatalf("%s: admitted %s (err %v), want %s", tc.order, got.label, got.err, tc.want)
			}
			if ts := target(t, d); ts.HeldExternal > 2 {
				t.Fatalf("%s: heldExt=%d past E = S-R = 2", tc.order, ts.HeldExternal)
			}
			got.lease.Release()
			second := recvAdmission(t, ch, "second handoff")
			if second.err != nil {
				t.Fatalf("second handoff: %v", second.err)
			}
			second.lease.Release()
			b2.Release()
			x1.Release()
		})
	}
}

// TestExternalOrderHotSwap pins that dispatch.external_order is hot: a swap
// by UpdateSettings acts on the very next handoff.
func TestExternalOrderHotSwap(t *testing.T) {
	s := DefaultSettings()
	s.RankOrder = mustRankOrder(t, ExternalOrderO1)
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 3)
	b1, b2, x1 := orderFixture(t, d, ctx, ch)

	s.RankOrder = mustRankOrder(t, ExternalOrderO2)
	d.UpdateSettings(s) // every slot held: the swap acts on the release below
	b1.Release()
	if got := recvAdmission(t, ch, "handoff under o2"); got.label != "background" {
		t.Fatalf("after the swap to o2: admitted %s, want background", got.label)
	} else {
		b1 = got.lease
	}
	startWaiter(ctx, d, "background-2", backgroundReq(), ch)
	waitFor(t, "second background waiter queued", func() bool { return waitingBackground(d) == 1 })

	s.RankOrder = mustRankOrder(t, ExternalOrderO1)
	d.UpdateSettings(s)
	b2.Release()
	got := recvAdmission(t, ch, "handoff under o1")
	if got.label != "external" {
		t.Fatalf("after the swap back to o1: admitted %s, want external", got.label)
	}
	got.lease.Release()
	last := recvAdmission(t, ch, "last handoff")
	last.lease.Release()
	b1.Release()
	x1.Release()
}

// TestExternalPartitionHoldsInBothOrders: under an external flood heldExt
// stays at S − R in o1 AND o2, and an external head at the partition border
// never blocks a background newcomer (it is not admissible).
func TestExternalPartitionHoldsInBothOrders(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		t.Run(order, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan admission, 6)
			for c := 1; c <= 6; c++ {
				startWaiter(xctx(ctx, fmt.Sprintf("c%d", c), "tenant-a"), d, "x", externalReq(), ch)
			}
			waitFor(t, "flood settled", func() bool { ts := target(t, d); return ts.HeldExternal+ts.External.Waiting == 6 })
			if ts := target(t, d); ts.HeldExternal != 2 {
				t.Fatalf("%s: heldExt=%d, want 2 = S-R", order, ts.HeldExternal)
			}
			bctx, bcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer bcancel()
			bl, _, err := d.Acquire(bctx, backgroundReq())
			if err != nil {
				t.Fatalf("%s: background blocked by an external head at the partition border: %v", order, err)
			}
			bl.Release()
			for i := 0; i < 2; i++ {
				recvAdmission(t, ch, "initial external").lease.Release()
			}
			cancel()
		})
	}
}

// TestExternalCapLadderC8 is the C8 gate (design/01 §7 duty 2, K-03-6): five
// consumers of ONE tenant with eight waiters each — the 33rd waiter of that
// tenant is refused with ErrTenantSaturated (the external tenant cap 32, a
// counter of its own, not the interactive 16), a consumer of a foreign tenant
// still queues; the ninth waiter of one consumer is refused with
// ErrPrincipalSaturated while a second consumer is untouched — two consumers,
// two identity buckets.
func TestExternalCapLadderC8(t *testing.T) {
	t.Run("tenant cap", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-x"), externalReq())
		defer occ.Release()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 64)
		for k := 1; k <= 4; k++ {
			for i := 0; i < 8; i++ {
				startWaiter(xctx(ctx, fmt.Sprintf("k%d", k), "tenant-a"), d, "a", externalReq(), ch)
			}
		}
		waitFor(t, "32 waiters of tenant-a queued", func() bool { return waitingExternal(d) == 32 })
		k5 := make(chan admission, 8)
		for i := 0; i < 8; i++ {
			startWaiter(xctx(ctx, "k5", "tenant-a"), d, "k5", externalReq(), k5)
		}
		var refusals []error
		waitFor(t, "the 8 acquires of k5 queued or refused", func() bool {
			for {
				select {
				case a := <-k5:
					refusals = append(refusals, a.err)
					continue
				default:
				}
				return waitingExternal(d)+len(refusals) == 40
			}
		})
		tenantSat := 0
		for _, err := range refusals {
			if errors.Is(err, ErrTenantSaturated) {
				tenantSat++
			}
		}
		if got := waitingExternal(d); got != 32 || tenantSat != 8 {
			t.Fatalf("tenant-a holds %d waiters with %d tenant refusals, want 32 waiters and 8 ErrTenantSaturated (the 33rd waiter of a tenant is refused)", got, tenantSat)
		}
		startWaiter(xctx(ctx, "b1", "tenant-b"), d, "b", externalReq(), ch)
		waitFor(t, "a consumer of a foreign tenant still queues", func() bool { return waitingExternal(d) == 33 })
		if ts := target(t, d); ts.ExternalRejects.Cap != 8 {
			t.Errorf("ExternalRejects.Cap = %d, want 8", ts.ExternalRejects.Cap)
		}
		if got := target(t, d).Interactive.Waiting; got != 0 {
			t.Errorf("external waiters leaked into the interactive queue: %d", got)
		}
	})
	t.Run("consumer cap and identity buckets", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-x"), externalReq())
		defer occ.Release()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 16)
		for i := 0; i < 8; i++ {
			startWaiter(xctx(ctx, "k1", "tenant-a"), d, "k1", externalReq(), ch)
		}
		waitFor(t, "8 waiters of k1", func() bool { return waitingExternal(d) == 8 })
		if _, _, err := acquireWithin(t, d, xctx(ctx, "k1", "tenant-a"), externalReq(), 500*time.Millisecond); !errors.Is(err, ErrPrincipalSaturated) {
			t.Fatalf("9th acquire of k1: %v, want ErrPrincipalSaturated", err)
		}
		startWaiter(xctx(ctx, "k2", "tenant-a"), d, "k2", externalReq(), ch)
		waitFor(t, "k2 untouched by the cap of k1", func() bool { return waitingExternal(d) == 9 })
		buckets := map[string]BucketSnapshot{}
		for _, b := range target(t, d).Buckets {
			if b.Class == ClassExternal {
				buckets[b.FairKey] = b
			}
		}
		k1, k2 := buckets["_consumer:k1"], buckets["_consumer:k2"]
		if len(buckets) != 3 || k1.Waiting != 8 || k2.Waiting != 1 || k1.TenantID != "tenant-a" || k2.TenantID != "tenant-a" {
			t.Fatalf("identity buckets = %+v, want _consumer:k1 (8 waiting), _consumer:k2 (1 waiting) and the occupier, tenant-a", buckets)
		}
		if b := buckets["_consumer:occ"]; b.Inflight != 1 || b.TenantID != "tenant-x" {
			t.Fatalf("occupier bucket = %+v, want 1 running lease of tenant-x", b)
		}
	})
}

// TestExternalCapLadderNonDefaultValues pins the wiring of the three external
// queue caps, the aggregate cap dispatch.external_queue_max included (review
// 2, F-A): consumer 2, tenant 3, aggregate 4 while the interactive caps keep
// their defaults 8/16/64. A ladder that reads an interactive knob for the
// external queue, drops the external aggregate cap or compares it off by one
// queues a waiter here that the external settings refuse; the C8 gate above
// runs on the defaults, where the consumer caps of both classes are 8. A hot
// raise of the aggregate cap acts on the next acquire.
func TestExternalCapLadderNonDefaultValues(t *testing.T) {
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 2, 3, 4
	if s.InteractiveQueuePerPrincipal <= 2 || s.InteractiveQueuePerTenant <= 3 || s.InteractiveQueueMax <= 4 {
		t.Fatalf("interactive caps %d/%d/%d must stay above the external ones for this gate to tell them apart",
			s.InteractiveQueuePerPrincipal, s.InteractiveQueuePerTenant, s.InteractiveQueueMax)
	}
	d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-x"), externalReq())
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 16)
	queue := func(consumer, tenant string, want int) {
		t.Helper()
		startWaiter(xctx(ctx, consumer, tenant), d, consumer, externalReq(), ch)
		waitFor(t, fmt.Sprintf("%s of %s queued as external waiter %d", consumer, tenant, want),
			func() bool { return waitingExternal(d) == want })
	}
	refuse := func(consumer, tenant string, want error, why string) {
		t.Helper()
		if _, _, err := acquireWithin(t, d, xctx(ctx, consumer, tenant), externalReq(), 500*time.Millisecond); !errors.Is(err, want) {
			t.Fatalf("%s of %s: %v, want %v (%s)", consumer, tenant, err, want, why)
		}
	}
	queue("c1", "tenant-a", 1)
	queue("c1", "tenant-a", 2)
	refuse("c1", "tenant-a", ErrPrincipalSaturated, "external_queue_per_consumer 2, not the interactive 8")
	queue("c2", "tenant-a", 3)
	refuse("c3", "tenant-a", ErrTenantSaturated, "external_queue_per_tenant 3, not the interactive 16")
	queue("c4", "tenant-b", 4)
	refuse("c5", "tenant-c", ErrTargetSaturated, "external_queue_max 4, not the interactive 64")
	if ts := target(t, d); ts.ExternalRejects.Cap != 3 || ts.External.Waiting != 4 || ts.Interactive.Waiting != 0 {
		t.Fatalf("cap=%d external waiting=%d interactive waiting=%d, want 3/4/0",
			ts.ExternalRejects.Cap, ts.External.Waiting, ts.Interactive.Waiting)
	}
	s.ExternalQueueMax = 5
	d.UpdateSettings(s)
	queue("c5", "tenant-c", 5)
	refuse("c6", "tenant-d", ErrTargetSaturated, "the hot-raised external_queue_max 5")
	if got := target(t, d).ExternalRejects.Cap; got != 4 {
		t.Fatalf("ExternalRejects.Cap = %d, want 4", got)
	}
}

// TestExternalHeldIsTheDetectorSum pins the foreign-load detector contract
// (A-E5 no. 3, handler/foreign_load.go heldOf): TargetSnapshot.Held stays the
// sum of ALL slot-holding leases, internal and external — two external and
// one background lease read Held 3, HeldExternal 2, HeldInternal 1.
func TestExternalHeldIsTheDetectorSum(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	x1 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), externalReq())
	x2 := mustAcquire(t, d, xctx(context.Background(), "c2", "tenant-b"), externalReq())
	b1 := mustAcquire(t, d, context.Background(), backgroundReq())
	ts := target(t, d)
	if ts.Held != 3 || ts.HeldExternal != 2 || ts.HeldInternal != 1 || ts.Inflight != 3 || ts.InflightInternal != 1 {
		t.Fatalf("Held=%d HeldExternal=%d HeldInternal=%d Inflight=%d InflightInternal=%d, want 3/2/1/3/1",
			ts.Held, ts.HeldExternal, ts.HeldInternal, ts.Inflight, ts.InflightInternal)
	}
	if ts.InternalReserved != 2 || !ts.ExternalOffered || ts.ExternalCapacity != 2 {
		t.Fatalf("InternalReserved=%d ExternalOffered=%v ExternalCapacity=%d, want 2/true/2", ts.InternalReserved, ts.ExternalOffered, ts.ExternalCapacity)
	}
	x1.Release()
	x2.Release()
	b1.Release()
	if ts := target(t, d); ts.Held != 0 || ts.HeldExternal != 0 || ts.Inflight != 0 {
		t.Fatalf("after release Held=%d HeldExternal=%d Inflight=%d, want 0/0/0", ts.Held, ts.HeldExternal, ts.Inflight)
	}
}

// authCase is one principal of the authorization matrix.
type authCase struct {
	name string
	p    Principal
	want error
}

// externalAuthMatrix lists every principal that may NOT use the external
// class (K-03-1: zulässig gdw. ConsumerID ≠ "" ∧ ApiKeyID == "" ∧ TenantID ≠
// "" ∧ ¬Conflict).
var externalAuthMatrix = []authCase{
	{name: "zero principal", p: Principal{}, want: ErrClassUnauthorized},
	{name: "knowledge key", p: principal("k"), want: ErrClassUnauthorized},
	// The S9 synthetic AuthResult shape (handler/middleware.go): tenant and
	// scope set, neither a knowledge key nor a consumer (review F3).
	{name: "tenant without consumer", p: Principal{TenantID: "tenant-a", HomeScope: "scope-a"}, want: ErrClassUnauthorized},
	{name: "consumer without tenant", p: Principal{ConsumerID: "c1", ConsumerKind: "app", FairWeight: 10}, want: ErrClassUnauthorized},
	{name: "consumer with knowledge key id", p: Principal{ConsumerID: "c1", TenantID: "tenant-a", ApiKeyID: "key-1"}, want: ErrClassUnauthorized},
	{name: "conflict", p: Principal{ConsumerID: "c1", TenantID: "tenant-a", Conflict: true}, want: ErrPrincipalConflict},
}

// TestExternalClassAuthorization is the authorization gate of both doors
// (design/03 §4.1 C7 (3), the reverse direction of I-K10): every principal of
// the matrix is refused in the external class — no waiter, no lease, no
// downgrade, counted once — on a free, a full, a pass-through target and with
// the kill switch off; a valid consumer is admitted. A consumer in an
// in-house class stays refused, the background queue and class_downgrades
// stay untouched.
func TestExternalClassAuthorization(t *testing.T) {
	type door func(*Dispatcher, context.Context, Request) (*Lease, context.Context, error)
	doors := map[string]door{"acquire": (*Dispatcher).Acquire, "try-acquire": (*Dispatcher).TryAcquire}
	situations := map[string]func(*testing.T) (*Dispatcher, func()){
		"free": func(t *testing.T) (*Dispatcher, func()) {
			d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
			return d, func() {}
		},
		"external partition full": func(t *testing.T) (*Dispatcher, func()) {
			d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
			occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-x"), externalReq())
			return d, occ.Release
		},
		"pass-through": func(t *testing.T) (*Dispatcher, func()) {
			d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
			return d, func() {}
		},
		"kill switch off": func(t *testing.T) (*Dispatcher, func()) {
			s := DefaultSettings()
			s.Enabled = false
			d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
			return d, func() {}
		},
	}
	for sname, setup := range situations {
		for dname, enter := range doors {
			for _, ac := range externalAuthMatrix {
				t.Run(sname+"/"+dname+"/"+ac.name, func(t *testing.T) {
					d, done := setup(t)
					defer done()
					before := d.Snapshot()
					held := target(t, d).Held
					ctx, cancel := context.WithTimeout(withPrincipal(context.Background(), ac.p), time.Second)
					defer cancel()
					l, runCtx, err := enter(d, ctx, externalReq())
					if !errors.Is(err, ac.want) || l != nil || runCtx != nil {
						t.Fatalf("(lease %v, runCtx %v, err %v), want (nil, nil, %v)", l != nil, runCtx != nil, err, ac.want)
					}
					after := d.Snapshot()
					ts := target(t, d)
					if ts.External.Waiting != 0 || ts.Background.Waiting != 0 || ts.Interactive.Waiting != 0 || ts.Held != held {
						t.Fatalf("refused principal left state: waiting ext/bg/ia=%d/%d/%d held=%d (was %d)",
							ts.External.Waiting, ts.Background.Waiting, ts.Interactive.Waiting, ts.Held, held)
					}
					if after.ClassDowngrades != before.ClassDowngrades {
						t.Fatalf("class_downgrades moved: %d → %d", before.ClassDowngrades, after.ClassDowngrades)
					}
					gotU := after.ClassUnauthorized - before.ClassUnauthorized
					gotC := after.PrincipalConflicts - before.PrincipalConflicts
					wantU, wantC := int64(1), int64(0)
					if errors.Is(ac.want, ErrPrincipalConflict) {
						wantU, wantC = 0, 1
					}
					if gotU != wantU || gotC != wantC {
						t.Fatalf("counters class_unauthorized +%d principal_conflicts +%d, want +%d/+%d", gotU, gotC, wantU, wantC)
					}
				})
			}
		}
	}
	t.Run("valid consumer is admitted at both doors", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(3, 1))
		for name, enter := range doors {
			ctx, cancel := context.WithTimeout(xctx(context.Background(), "c-"+name, "tenant-a"), 3*time.Second)
			l, runCtx, err := enter(d, ctx, externalReq())
			cancel()
			if err != nil || l == nil || runCtx == nil || l.Class() != ClassExternal || !l.slotHeld {
				t.Fatalf("%s: lease=%v err=%v — a valid consumer must take a slot-holding external lease", name, l, err)
			}
			l.Release()
		}
		if s := d.Snapshot(); s.ClassUnauthorized != 0 || s.PrincipalConflicts != 0 {
			t.Fatalf("valid consumer counted as refusal: %+v", s)
		}
	})
	t.Run("consumer in an in-house class stays refused", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		for name, enter := range doors {
			for _, req := range []Request{interactiveReq(), backgroundReq()} {
				l, _, err := enter(d, xctx(context.Background(), "c1", "tenant-a"), req)
				if !errors.Is(err, ErrClassUnauthorized) || l != nil {
					t.Fatalf("%s/%s: (lease %v, err %v), want ErrClassUnauthorized", name, req.Class, l != nil, err)
				}
			}
		}
		s := d.Snapshot()
		if waitingBackground(d) != 0 || s.ClassDowngrades != 0 || s.ClassUnauthorized != 4 {
			t.Fatalf("background waiting=%d downgrades=%d unauthorized=%d, want 0/0/4", waitingBackground(d), s.ClassDowngrades, s.ClassUnauthorized)
		}
	})
}

// TestExternalNeverPreemptVictim pins I-03-3: an external lease is never a
// preempt victim. A target full with one external and one interactive lease
// and a waiting interactive acquire cancels nothing; the next release hands
// the slot to interactive ahead of a waiting external acquire. With heldInt <
// R, an interactive acquire under an external flood is admitted at once.
func TestExternalNeverPreemptVictim(t *testing.T) {
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: 2, InternalReserved: 1, PreemptBackground: true}}}
	d, h := newTestDispatcher(t, DefaultSettings(), pol)
	xl, xrun, err := acquireWithin(t, d, xctx(context.Background(), "c1", "tenant-a"), externalReq(), 3*time.Second)
	if err != nil {
		t.Fatalf("external occupier: %v", err)
	}
	il := mustAcquire(t, d, ictx(principal("i1")), interactiveReq())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "c2", "tenant-a"), d, "external", externalReq(), ch)
	waitFor(t, "external waiter queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(withPrincipal(ctx, principal("i2")), d, "interactive", interactiveReq(), ch)
	waitFor(t, "interactive waiter queued", func() bool { return waitingInteractive(d) == 1 })
	if xrun.Err() != nil || target(t, d).Preempt.PreemptsTotal != 0 {
		t.Fatalf("external lease canceled by a scheduling decision: runCtx=%v preempts=%d", context.Cause(xrun), target(t, d).Preempt.PreemptsTotal)
	}
	if !h.contains("without cancelable background lease") {
		t.Fatalf("expected the no-victim WARN on a target full with interactive and external leases")
	}
	xl.Release()
	got := recvAdmission(t, ch, "handoff after the external release")
	if got.label != "interactive" {
		t.Fatalf("handoff = %s, want interactive ahead of the waiting external acquire", got.label)
	}
	got.lease.Release()
	recvAdmission(t, ch, "external handoff").lease.Release()
	il.Release()

	d2, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	fctx, fcancel := context.WithCancel(context.Background())
	defer fcancel()
	fch := make(chan admission, 8)
	for c := 1; c <= 8; c++ {
		startWaiter(xctx(fctx, fmt.Sprintf("c%d", c), "tenant-a"), d2, "x", externalReq(), fch)
	}
	waitFor(t, "flood settled", func() bool { ts := target(t, d2); return ts.HeldExternal+ts.External.Waiting == 8 })
	ia := mustAcquire(t, d2, ictx(principal("i1")), interactiveReq())
	if w := ia.WaitDur(); w >= 5*time.Millisecond {
		t.Fatalf("interactive wait under an external flood with heldInt < R: %v, want < 5ms", w)
	}
	ia.Release()
	fcancel()
	for i := 0; i < 2; i++ {
		recvAdmission(t, fch, "flood lease").lease.Release()
	}
}

// TestExternalReaperCancelsOnlyExternalAndBackground pins the reaper half of
// I-03-3: a leaked external lease is canceled with cause ErrReaped once
// admitted + external_lease_max + grace has passed — even though its own reap
// reference (lease_max_age) lies far later — and its slot is free; an
// interactive lease is still never canceled.
func TestExternalReaperCancelsOnlyExternalAndBackground(t *testing.T) {
	s := DefaultSettings()
	s.ExternalLeaseMax = time.Minute
	s.LeaseMaxAge = time.Hour
	s.LeaseReapGrace = time.Second
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
	xl, xrun, err := acquireWithin(t, d, xctx(context.Background(), "c1", "tenant-a"), externalReq(), 3*time.Second)
	if err != nil {
		t.Fatalf("external: %v", err)
	}
	_, irun, err := acquireWithin(t, d, ictx(principal("i1")), interactiveReq(), 3*time.Second)
	if err != nil {
		t.Fatalf("interactive: %v", err)
	}
	admitted := xl.admitted
	d.ReapForTest(admitted.Add(30 * time.Second))
	if xrun.Err() != nil || target(t, d).HeldExternal != 1 {
		t.Fatalf("external lease reaped before external_lease_max: cause=%v", context.Cause(xrun))
	}
	d.ReapForTest(admitted.Add(time.Minute + 2*time.Second))
	if !errors.Is(context.Cause(xrun), ErrReaped) {
		t.Fatalf("leaked external lease past external_lease_max: cause=%v, want ErrReaped", context.Cause(xrun))
	}
	if ts := target(t, d); ts.HeldExternal != 0 || ts.Held != 1 {
		t.Fatalf("after the reap heldExt=%d held=%d, want 0/1 (slot free)", ts.HeldExternal, ts.Held)
	}
	d.ReapForTest(admitted.Add(2 * time.Hour))
	if irun.Err() != nil {
		t.Fatalf("interactive lease canceled by the reaper: %v (I-D1)", context.Cause(irun))
	}
	if ts := target(t, d); ts.Held != 0 || ts.Inflight != 0 {
		t.Fatalf("after the late reap held=%d inflight=%d, want 0/0", ts.Held, ts.Inflight)
	}

	off := DefaultSettings()
	off.ExternalLeaseMax = 0 // 0 = no external cap: lease_max_age alone applies
	off.LeaseMaxAge = time.Hour
	off.LeaseReapGrace = time.Second
	d2, _ := newTestDispatcher(t, off, partitionPolicy(3, 1))
	l2, run2, err := acquireWithin(t, d2, xctx(context.Background(), "c1", "tenant-a"), externalReq(), 3*time.Second)
	if err != nil {
		t.Fatalf("external: %v", err)
	}
	d2.ReapForTest(l2.admitted.Add(30 * time.Minute))
	if run2.Err() != nil {
		t.Fatalf("external_lease_max = 0 must not cap the reap reference: cause=%v", context.Cause(run2))
	}
	d2.ReapForTest(l2.admitted.Add(time.Hour + 2*time.Second))
	if !errors.Is(context.Cause(run2), ErrReaped) {
		t.Fatalf("external lease past lease_max_age: cause=%v, want ErrReaped", context.Cause(run2))
	}
}

// optInRow is one _global row on the test origin.
func optInRow(name string, limits map[string]any, selfAdmitting bool) BackendRow {
	return BackendRow{Name: name, Scope: GlobalScope, BaseURL: testOrigin, Limits: limits, SelfAdmitting: selfAdmitting}
}

// TestExternalOptInPerTargetKind is the X7/X8 gate (I-03-6, design/03 §3.2,
// §4.3): a slot-managed target offers the external class only with S > 0 and
// a valid R (validated after the merge), a self-admitting target only with
// external_offer; every violation logs a WARN and the acquire ends with
// ErrExternalNotOffered (no rejection). A row without api_key_ref is offered
// (A-E5: authentication is no offer condition).
func TestExternalOptInPerTargetKind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     []BackendRow
		warn     string
		offered  bool
		capacity int
	}{
		{name: "R >= S", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 4}, false)}, warn: "internal_reserved_slots"},
		{name: "R = 0", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 0}, false)}, warn: "internal_reserved_slots"},
		{name: "S = 0", rows: []BackendRow{optInRow("a", map[string]any{"internal_reserved_slots": 2}, false)}, warn: "internal_reserved_slots"},
		{name: "row A slots 4, row B R 5", rows: []BackendRow{
			optInRow("a", map[string]any{"slots": 4}, false),
			optInRow("b", map[string]any{"internal_reserved_slots": 5}, false),
		}, warn: "internal_reserved_slots"},
		{name: "background_reserved_slots >= R", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 2, "background_reserved_slots": 2}, false)}, warn: "internal_reserved_slots"},
		{name: "R not an integer", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": "two"}, false)}, warn: "internal_reserved_slots"},
		{name: "LAN target without slots", rows: []BackendRow{optInRow("a", map[string]any{}, false)}},
		{name: "external_offer on a slot-managed target", rows: []BackendRow{optInRow("a", map[string]any{"external_offer": true}, false)}, warn: "external_offer"},
		{name: "self-admitting target without external_offer", rows: []BackendRow{optInRow("a", map[string]any{}, true)}},
		{name: "external_offer not a bool", rows: []BackendRow{optInRow("a", map[string]any{"external_offer": "yes"}, true)}, warn: "external_offer"},
		{name: "external_offer on one of two rows", rows: []BackendRow{
			optInRow("a", map[string]any{"external_offer": true}, true),
			optInRow("b", map[string]any{}, true),
		}},
		{name: "external_max_inflight 0", rows: []BackendRow{optInRow("a", map[string]any{"external_offer": true, "external_max_inflight": 0}, true)}, warn: "external_max_inflight"},
		{name: "self-admitting target with slots but without R", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "external_offer": true}, true)}, warn: "internal_reserved_slots"},
		// Review F8: a self-admitting target with slots needs R AND the offer.
		{name: "self-admitting target with slots and R but without external_offer", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 1}, true)}, warn: "internal_reserved_slots"},
		// Review F9: a malformed count on ONE row never widens the offer of the
		// merge — fractional, wrapping and negative values are invalid per row.
		{name: "fractional R", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 1.5}, false)}, warn: "internal_reserved_slots"},
		{name: "huge R next to R = 1", rows: []BackendRow{
			optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": float64(1e19)}, false),
			optInRow("b", map[string]any{"internal_reserved_slots": 1}, false),
		}, warn: "internal_reserved_slots"},
		{name: "negative R next to R = 2", rows: []BackendRow{
			optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": -1}, false),
			optInRow("b", map[string]any{"internal_reserved_slots": 2}, false),
		}, warn: "internal_reserved_slots"},
		{name: "fractional external_max_inflight", rows: []BackendRow{optInRow("a", map[string]any{"external_offer": true, "external_max_inflight": 2.5}, true)}, warn: "external_max_inflight"},
		// Offered.
		{name: "valid R without api_key_ref", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 2}, false)}, offered: true, capacity: 2},
		{name: "external_offer on a slot-managed target is ignored, R offers", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 3, "external_offer": true}, false)}, warn: "external_offer", offered: true, capacity: 1},
		{name: "self-admitting target with external_offer", rows: []BackendRow{optInRow("a", map[string]any{"external_offer": true}, true)}, offered: true},
		{name: "self-admitting target with external_max_inflight", rows: []BackendRow{optInRow("a", map[string]any{"external_offer": true, "external_max_inflight": 2}, true)}, offered: true, capacity: 2},
		{name: "self-admitting target with slots, R and external_offer", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 1, "external_offer": true, "external_max_inflight": 2}, true)}, offered: true, capacity: 2},
		// Review M31: external_max_inflight on a slot-managed target is ignored
		// with a WARN — R offers with E = S − R.
		{name: "external_max_inflight without external_offer is ignored", rows: []BackendRow{optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 1, "external_max_inflight": 1}, false)}, warn: "external_max_inflight", offered: true, capacity: 3},
		// Review F4/M4: external_max_inflight merges MIN over the rows.
		{name: "external_max_inflight merges MIN over rows", rows: []BackendRow{
			optInRow("a", map[string]any{"external_offer": true, "external_max_inflight": 3}, true),
			optInRow("b", map[string]any{"external_offer": true, "external_max_inflight": 2}, true),
		}, offered: true, capacity: 2},
		{name: "R merges MAX over rows", rows: []BackendRow{
			optInRow("a", map[string]any{"slots": 4, "internal_reserved_slots": 1}, false),
			optInRow("b", map[string]any{"internal_reserved_slots": 2}, false),
		}, offered: true, capacity: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingHandler{}
			pol := DerivePolicy(tc.rows, slog.New(h))
			if tc.warn != "" && !h.contains(tc.warn) {
				t.Errorf("derivation WARNs %q, want one naming %s", h.msgs, tc.warn)
			}
			if tc.warn == "" && len(h.msgs) != 0 {
				t.Errorf("unexpected derivation WARNs %q", h.msgs)
			}
			d, _ := newTestDispatcher(t, DefaultSettings(), pol)
			if got := d.ExternalAdmissible(testOrigin); got != tc.offered {
				t.Errorf("ExternalAdmissible = %v, want %v", got, tc.offered)
			}
			acquire := func(ctx context.Context, req Request) (*Lease, context.Context, error) {
				return acquireWithin(t, d, ctx, req, 500*time.Millisecond)
			}
			for name, enter := range map[string]func(context.Context, Request) (*Lease, context.Context, error){"acquire": acquire, "try-acquire": d.TryAcquire} {
				l, _, err := enter(xctx(context.Background(), "c1", "tenant-a"), externalReq())
				switch {
				case tc.offered && err != nil:
					t.Fatalf("%s on an offered target: %v", name, err)
				case tc.offered:
					l.Release()
				case !errors.Is(err, ErrExternalNotOffered) || l != nil:
					t.Fatalf("%s on a target without offer: (lease %v, err %v), want ErrExternalNotOffered", name, l != nil, err)
				}
			}
			ts := target(t, d)
			if ts.ExternalOffered != tc.offered || ts.ExternalCapacity != tc.capacity {
				t.Errorf("snapshot ExternalOffered=%v ExternalCapacity=%d, want %v/%d", ts.ExternalOffered, ts.ExternalCapacity, tc.offered, tc.capacity)
			}
			if !tc.offered && ts.ExternalRejects.NotOffered != 2 {
				t.Errorf("ExternalRejects.NotOffered = %d, want 2", ts.ExternalRejects.NotOffered)
			}
		})
	}
	if IsRejection(ErrExternalNotOffered) || IsRejection(fmt.Errorf("wrapped: %w", ErrExternalNotOffered)) {
		t.Fatal("ErrExternalNotOffered must not be a rejection (no 429)")
	}
}

// TestExternalMaxInflightOnSelfAdmittingTarget: on a self-admitting target
// (no slots, internal classes pass through) the external class is admitted
// against external_max_inflight — the third acquire with a cap of 2 waits and
// takes the slot of the first one released; internal acquires stay
// pass-through.
func TestExternalMaxInflightOnSelfAdmittingTarget(t *testing.T) {
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {ExternalOffer: true, ExternalMaxInflight: 2}}}
	d, _ := newTestDispatcher(t, DefaultSettings(), pol)
	x1 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), externalReq())
	x2 := mustAcquire(t, d, xctx(context.Background(), "c2", "tenant-a"), externalReq())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c3", "tenant-a"), d, "third", externalReq(), ch)
	waitFor(t, "third external acquire waits", func() bool { return waitingExternal(d) == 1 })
	b := mustAcquire(t, d, context.Background(), backgroundReq())
	if b.slotHeld {
		t.Fatalf("background on a self-admitting target must stay pass-through")
	}
	startWaiter(xctx(ctx, "c4", "tenant-a"), d, "fourth", externalReq(), ch)
	waitFor(t, "fourth external acquire waits", func() bool { return waitingExternal(d) == 2 })
	// Review F4/M39: a wake without a release (policy re-store, settings
	// swap) admits nobody past the cap.
	d.UpdatePolicy(pol)
	d.UpdateSettings(DefaultSettings())
	if ts := target(t, d); ts.HeldExternal != 2 || ts.External.Waiting != 2 {
		t.Fatalf("after a wake at the cap: heldExt=%d waiting=%d, want 2/2", ts.HeldExternal, ts.External.Waiting)
	}
	x1.Release()
	got := recvAdmission(t, ch, "third external handoff")
	if got.err != nil || !got.lease.slotHeld || got.label != "third" {
		t.Fatalf("third external: label=%s err=%v — FIFO, and an external lease is never pass-through", got.label, got.err)
	}
	if ts := target(t, d); ts.HeldExternal != 2 || ts.External.Waiting != 1 {
		t.Fatalf("one release admits exactly one: heldExt=%d waiting=%d, want 2/1", ts.HeldExternal, ts.External.Waiting)
	}
	got.lease.Release()
	fourth := recvAdmission(t, ch, "fourth external handoff")
	fourth.lease.Release()
	x2.Release()
	b.Release()
}

// TestExternalKillSwitchEndsExternalWaiters is the X9 gate: the emergency
// stop (dispatch.enabled=false) and a policy swap that withdraws the offer end
// waiting external acquires with ErrExternalNotOffered within one wake — never
// with a pass-through handoff (no external lease without a slot). In-house
// waiters keep the pass-through drain, new external acquires are refused.
func TestExternalKillSwitchEndsExternalWaiters(t *testing.T) {
	t.Run("enabled=false", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), externalReq())
		ia := mustAcquire(t, d, ictx(principal("i1")), interactiveReq())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 2)
		startWaiter(xctx(ctx, "c1", "tenant-a"), d, "external", externalReq(), ch)
		waitFor(t, "external waiter queued", func() bool { return waitingExternal(d) == 1 })
		startWaiter(ctx, d, "background", backgroundReq(), ch)
		waitFor(t, "background waiter queued", func() bool { return waitingBackground(d) == 1 })

		s := DefaultSettings()
		s.Enabled = false
		d.UpdateSettings(s)
		for i := 0; i < 2; i++ {
			a := recvAdmission(t, ch, "drain after the emergency stop")
			switch a.label {
			case "external":
				if !errors.Is(a.err, ErrExternalNotOffered) || a.lease != nil {
					t.Fatalf("external waiter after the emergency stop: (lease %v, err %v), want ErrExternalNotOffered — never a pass-through handoff", a.lease != nil, a.err)
				}
			case "background":
				if a.err != nil || a.lease.slotHeld {
					t.Fatalf("background waiter: err=%v — in-house waiters keep the pass-through drain", a.err)
				}
				a.lease.Release()
			}
		}
		if l, _, err := acquireWithin(t, d, xctx(context.Background(), "c2", "tenant-a"), externalReq(), 500*time.Millisecond); !errors.Is(err, ErrExternalNotOffered) {
			t.Fatalf("new external acquire with the emergency stop: (lease %v, err %v), want ErrExternalNotOffered — never a pass-through lease", l != nil, err)
		}
		if d.ExternalAdmissible(testOrigin) {
			t.Fatal("ExternalAdmissible with dispatch.enabled=false")
		}
		// Review F7: the snapshot reports no offer under the emergency stop.
		if ts := target(t, d); ts.ExternalOffered || ts.ExternalCapacity != 0 {
			t.Fatalf("snapshot under the emergency stop: ExternalOffered=%v ExternalCapacity=%d, want false/0", ts.ExternalOffered, ts.ExternalCapacity)
		}
		occ.Release()
		ia.Release()
	})
	t.Run("offer withdrawn", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), externalReq())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 1)
		startWaiter(xctx(ctx, "c1", "tenant-a"), d, "external", externalReq(), ch)
		waitFor(t, "external waiter queued", func() bool { return waitingExternal(d) == 1 })
		d.UpdatePolicy(onSlotPolicy(2)) // R removed: slots stay, the offer is gone
		a := recvAdmission(t, ch, "external waiter after the offer is withdrawn")
		if !errors.Is(a.err, ErrExternalNotOffered) || a.lease != nil {
			t.Fatalf("(lease %v, err %v), want ErrExternalNotOffered", a.lease != nil, a.err)
		}
		if ts := target(t, d); ts.HeldExternal != 1 || ts.ExternalRejects.NotOffered != 1 {
			t.Fatalf("heldExt=%d not_offered=%d, want 1/1 (the running lease lives out)", ts.HeldExternal, ts.ExternalRejects.NotOffered)
		}
		occ.Release()
	})
}

// TestExternalRetryAfterHint: Retry-After knows the external class — its own
// queue depth and its own wait window — and, since A03-W6, quantizes the
// external estimate onto dispatch.external_retry_after_steps (3 × 2 s = 6 s
// rounds up to the step 10 s); the interactive estimate keeps the B1 clamp.
func TestExternalRetryAfterHint(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), externalReq())
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 3)
	for i := 0; i < 3; i++ {
		startWaiter(xctx(ctx, fmt.Sprintf("c%d", i), "tenant-a"), d, "x", externalReq(), ch)
	}
	waitFor(t, "3 external waiters", func() bool { return waitingExternal(d) == 3 })
	for i := 0; i < 20; i++ {
		d.SeedWaitSampleForTest(testOrigin, ClassExternal, 2*time.Second)
	}
	if got := d.RetryAfterHint(testOrigin, ClassExternal); got != 10*time.Second {
		t.Fatalf("RetryAfterHint(external) = %v, want 10s (3 waiting × p95 2s = 6s, quantized up)", got)
	}
	if got := d.RetryAfterHint(testOrigin, ClassInteractive); got != time.Second {
		t.Fatalf("RetryAfterHint(interactive) = %v, want the 1s floor (no interactive depth)", got)
	}
}

// TestNamedRankOrders pins the A-E2 data table: o1 = (I, XU, B, XB), o2 =
// (I, B, XU, XB) — XB last in both (DECISIONS E2, A03-W9) — both valid; the
// default order is o1; an unknown name is not an order; a duplicate with
// interactive on rank 0 — representable since numClasses = 3 — reads as the
// default with one WARN (A03-W2 handover 4).
func TestNamedRankOrders(t *testing.T) {
	want := map[string][numClasses]Class{
		ExternalOrderO1: {ClassInteractive, ClassExternal, ClassBackground, ClassExternalBulk},
		ExternalOrderO2: {ClassInteractive, ClassBackground, ClassExternal, ClassExternalBulk},
	}
	for name, w := range want {
		got, ok := RankOrderFor(name)
		if !ok || got != w || !validRankOrder(got) {
			t.Errorf("RankOrderFor(%q) = %v/%v, want %v (valid)", name, got, ok, w)
		}
	}
	if DefaultRankOrder() != want[ExternalOrderO1] {
		t.Errorf("DefaultRankOrder() = %v, want o1", DefaultRankOrder())
	}
	for _, name := range []string{"", "o3", "O1", " o1"} {
		if _, ok := RankOrderFor(name); ok {
			t.Errorf("RankOrderFor(%q) known, want unknown (the config layer trims and lowers)", name)
		}
	}
	s := DefaultSettings()
	s.RankOrder = [numClasses]Class{ClassInteractive, ClassInteractive, ClassBackground, ClassExternalBulk}
	d, h := newTestDispatcher(t, s, onSlotPolicy(1))
	if got := h.count("dispatch: invalid rank order"); got != 1 {
		t.Errorf("(I, I, B, XB): %d WARN, want 1", got)
	}
	if got := d.currentSettings().RankOrder; got != DefaultRankOrder() {
		t.Errorf("(I, I, B, XB): effective order %v, want the default", got)
	}
	if !strings.Contains(fmt.Sprint(DefaultRankOrder()), "external") {
		t.Errorf("rank order rendering %q lacks the external class", fmt.Sprint(DefaultRankOrder()))
	}
}

// TestExternalClassRow pins the class-table rows of XU and XB (design/03
// §4.1 C7): String "external" and "external_bulk" (the persisted
// dispatch_class values and the fedhop urgency vocabulary keys), External()
// true for both — the two external classes since A03-W9 — and false for
// both in-house classes.
func TestExternalClassRow(t *testing.T) {
	if got := ClassExternal.String(); got != "external" {
		t.Fatalf("ClassExternal.String() = %q, want external", got)
	}
	if !ClassExternal.External() || !ClassExternal.valid() {
		t.Fatal("ClassExternal must be a valid external class")
	}
	if got := ClassExternalBulk.String(); got != "external_bulk" {
		t.Fatalf("ClassExternalBulk.String() = %q, want external_bulk", got)
	}
	if !ClassExternalBulk.External() || !ClassExternalBulk.valid() {
		t.Fatal("ClassExternalBulk must be a valid external class")
	}
	var external []Class
	for c := Class(0); c < numClasses; c++ {
		if c.External() {
			external = append(external, c)
		}
	}
	if !slices.Equal(external, []Class{ClassExternal, ClassExternalBulk}) {
		t.Fatalf("external classes %v, want exactly [external external_bulk]", external)
	}
	if got := (Principal{ConsumerID: "c1"}).identityKey(); got != "_consumer:c1" {
		t.Fatalf("identityKey = %q, want _consumer:c1", got)
	}
}

// --- Review hardening (lead-own external review of A03-W3).

// TestExternalWaiterCancelVacatesItsSlot is the external twin of the W1
// negative probe (review F2): a canceled external waiter leaves no counter,
// bucket or ring entry behind, and the next release hands its slot to no
// dead waiter — the consumer can queue its full cap again.
func TestExternalWaiterCancelVacatesItsSlot(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), externalReq())
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan admission, 8)
	for i := 0; i < 8; i++ {
		startWaiter(xctx(ctx, "k1", "tenant-a"), d, "dead", externalReq(), ch)
	}
	waitFor(t, "8 waiters of k1", func() bool { return waitingExternal(d) == 8 })
	cancel()
	for i := 0; i < 8; i++ {
		if a := recvAdmission(t, ch, "canceled waiter"); a.err == nil {
			t.Fatalf("canceled waiter admitted: %v", a.lease)
		}
	}
	d.mu.Lock()
	st := d.targetLocked(testOrigin)
	q := st.external
	leak := q.total != 0 || len(q.byKey) != 0 || len(q.byTenant) != 0 || len(q.buckets) != 0 || len(q.ring) != 0
	d.mu.Unlock()
	if leak {
		t.Fatalf("canceled external waiters leaked: total=%d byKey=%v byTenant=%v buckets=%d ring=%v", q.total, q.byKey, q.byTenant, len(q.buckets), q.ring)
	}
	live := make(chan admission, 8)
	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	for i := 0; i < 8; i++ {
		startWaiter(xctx(lctx, "k1", "tenant-a"), d, "live", externalReq(), live)
	}
	waitFor(t, "k1 queues its full cap again", func() bool { return waitingExternal(d) == 8 })
	occ.Release()
	if a := recvAdmission(t, live, "handoff after the release"); a.err != nil || a.label != "live" {
		t.Fatalf("handoff went to %s (err %v), want a live waiter", a.label, a.err)
	} else {
		a.lease.Release()
	}
	lcancel()
}

// TestExternalCancelEntriesSettle pins the extCancel bookkeeping (review
// F5): an entry per running external lease, none after Release or the reap.
func TestExternalCancelEntriesSettle(t *testing.T) {
	s := DefaultSettings()
	s.ExternalLeaseMax = time.Minute
	s.LeaseReapGrace = time.Second
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
	x1 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), externalReq())
	x2 := mustAcquire(t, d, xctx(context.Background(), "c2", "tenant-a"), externalReq())
	count := func() (ext, cancelable int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.targetLocked(testOrigin)
		return len(st.extCancel), len(st.cancelable)
	}
	if e, c := count(); e != 2 || c != 0 {
		t.Fatalf("extCancel=%d cancelable=%d with two external leases, want 2/0", e, c)
	}
	x1.Release()
	d.ReapForTest(x2.admitted.Add(2 * time.Minute))
	if e, _ := count(); e != 0 {
		t.Fatalf("extCancel=%d after release and reap, want 0 (one entry leaked per lease)", e)
	}
}

// TestExternalReapEarlierHintWins (review F6): external_lease_max only CAPS
// the reap reference — an earlier deadline hint still reaps first.
func TestExternalReapEarlierHintWins(t *testing.T) {
	s := DefaultSettings()
	s.ExternalLeaseMax = time.Minute
	s.LeaseReapGrace = time.Second
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
	req := externalReq()
	req.DeadlineIn = 10 * time.Second
	l, run, err := acquireWithin(t, d, xctx(context.Background(), "c1", "tenant-a"), req, 3*time.Second)
	if err != nil {
		t.Fatalf("external: %v", err)
	}
	d.ReapForTest(l.admitted.Add(12 * time.Second))
	if !errors.Is(context.Cause(run), ErrReaped) {
		t.Fatalf("external lease past its 10s hint + grace: cause=%v, want ErrReaped (the earlier reference wins)", context.Cause(run))
	}
}

// TestExternalPickServesLeastSlotTimeConsumerFirst (review M38, rewritten
// by A03-W4 from the FIFO pin TestExternalPickIsFIFOAcrossConsumers): the
// external queue hands a freed unit to the consumer with the least slot time
// (running leases included), FIFO only inside a bucket and between equal
// slot times. Arrival c1, c1, c2 on E = 1: c1's first (both consumers at 0
// slot time — arrival order breaks the tie), then c2 (c1 has served one
// lease), then c1's second.
func TestExternalPickServesLeastSlotTimeConsumerFirst(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	var clk atomic.Int64
	clk.Store(time.Now().UnixNano())
	if cs, ok := any(d).(fairClockSetter); ok {
		cs.setFairClockForTest(func() time.Time { return time.Unix(0, clk.Load()) })
	}
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), externalReq())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 3)
	for i, c := range []string{"c1", "c1", "c2"} {
		startWaiter(xctx(ctx, c, "tenant-a"), d, fmt.Sprintf("%s-%d", c, i), externalReq(), ch)
		n := i + 1
		waitFor(t, "external waiter queued", func() bool { return waitingExternal(d) == n })
	}
	holder := occ
	for _, want := range []string{"c1-0", "c2-2", "c1-1"} {
		clk.Add(int64(time.Second))
		holder.Release()
		got := recvAdmission(t, ch, "external handoff")
		if got.label != want {
			t.Fatalf("handoff = %s, want %s (least slot time first, FIFO inside a bucket)", got.label, want)
		}
		holder = got.lease
	}
	holder.Release()
}

// TestExternalBucketsKeyedByClass (review M10): a bucket's identity is the
// pair (class, key) — an interactive fair key that spells like an identity
// key never merges with the external bucket.
func TestExternalBucketsKeyedByClass(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(3, 1))
	x := mustAcquire(t, d, xctx(context.Background(), "k1", "tenant-a"), externalReq())
	defer x.Release()
	odd := Principal{ApiKeyID: "key-odd", TenantID: "tenant-b", HomeScope: "_consumer:k1"}
	i := mustAcquire(t, d, ictx(odd), interactiveReq())
	defer i.Release()
	var classes []Class
	for _, b := range target(t, d).Buckets {
		if b.FairKey == "_consumer:k1" {
			classes = append(classes, b.Class)
		}
	}
	if len(classes) != 2 || classes[0] != ClassInteractive || classes[1] != ClassExternal {
		t.Fatalf("buckets keyed _consumer:k1 = %v, want one interactive and one external", classes)
	}
}

// TestInteractiveTenantCapSkipsEmptyTenant (review M12, pinned through the
// shared pushCappedLocked): interactive principals without a TenantID are not
// coupled into one "" tenant cap — only the per-key and aggregate caps bound
// them.
func TestInteractiveTenantCapSkipsEmptyTenant(t *testing.T) {
	s := DefaultSettings()
	s.InteractiveQueuePerTenant = 1
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	occ := mustAcquire(t, d, ictx(principal("occ")), interactiveReq())
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	for _, k := range []string{"a", "b"} {
		startWaiter(withPrincipal(ctx, Principal{ApiKeyID: "key-" + k, HomeScope: "scope-" + k}), d, k, interactiveReq(), ch)
	}
	waitFor(t, "two tenant-less interactive waiters queued", func() bool { return waitingInteractive(d) == 2 })
}

// TestExternalCapacityRules pins the runtime half of the offer (review M42,
// M23): a hand-built policy outside the R bounds never offers, and
// ExternalAdmissible normalizes the origin like an acquire.
func TestExternalCapacityRules(t *testing.T) {
	for _, tc := range []struct {
		tp       TargetPolicy
		offered  bool
		capacity int
	}{
		{TargetPolicy{Slots: 2, InternalReserved: 2}, false, 0},
		{TargetPolicy{Slots: 2, InternalReserved: 0}, false, 0},
		{TargetPolicy{Slots: 4, InternalReserved: 1}, true, 3},
		{TargetPolicy{Slots: 4, InternalReserved: 1, ExternalMaxInflight: 2}, true, 2},
		{TargetPolicy{ExternalOffer: true}, true, 0},
		{TargetPolicy{InternalReserved: 1}, false, 0},
	} {
		if off, c := tc.tp.externalCapacity(); off != tc.offered || c != tc.capacity {
			t.Errorf("%+v: externalCapacity = %v/%d, want %v/%d", tc.tp, off, c, tc.offered, tc.capacity)
		}
	}
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	if !d.ExternalAdmissible("HTTP://GPU:8089/v1") {
		t.Fatal("ExternalAdmissible does not normalize the origin")
	}
}
