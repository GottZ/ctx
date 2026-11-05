package dispatch

// A03-W6 gates (design/03 §4.9, E-03-13, DECISIONS A-E2 no. 2): the
// lease-duration ring per target × class, the predicted lease duration ŝ in
// the ServeBy term of MaxWait_eff, predictive shedding from the acquire's own
// position, the relational aggregate cap, and the Retry-After that travels
// with an external rejection — computed from the OWN position, quantized onto
// the steps of dispatch.external_retry_after_steps. The fake targets carry
// fake lease durations: the lease window is staged through the seeding hook
// (which publishes, like a tick would), and the leases that hold the
// partition never end while a gate runs, so the staged window stays exact.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// otherOrigin is the second fake target of the Retry-After oracle probe.
const otherOrigin = "http://gpu2:8089"

// seedLeases stages n external lease-duration samples of dur on origin and
// publishes the window.
func seedLeases(d *Dispatcher, origin string, n int, dur time.Duration) {
	d.SeedLeaseSamplesForTest(origin, ClassExternal, n, dur)
}

// externalReqAt is externalReq on another origin.
func externalReqAt(origin string) Request {
	r := externalReq()
	r.Target.Origin = origin
	return r
}

// retryAfterOf is the Retry-After an external rejection carries (A03-W8
// reads it through errors.As, never a second mutex section).
func retryAfterOf(t *testing.T, err error) time.Duration {
	t.Helper()
	var re *RejectionError
	if !errors.As(err, &re) {
		t.Fatalf("rejection %v carries no *RejectionError", err)
	}
	return re.RetryAfter
}

// holdExternal admits n external leases of the consumer "holder" (its own
// tenant, never waiting) on origin; they are released at cleanup.
func holdExternal(t *testing.T, d *Dispatcher, origin string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		l := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-h"), externalReqAt(origin))
		t.Cleanup(l.Release)
	}
}

// targetOf returns the snapshot row of origin.
func targetOf(d *Dispatcher, origin string) TargetSnapshot {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == origin {
			return ts
		}
	}
	return TargetSnapshot{}
}

// queueAt starts n waiters of consumer on origin and waits until they are
// queued (the external waiting count of origin rises by n).
func queueAt(t *testing.T, d *Dispatcher, ctx context.Context, origin, consumer, tenant string, n int, ch chan admission) {
	t.Helper()
	for i := 0; i < n; i++ {
		before := targetOf(d, origin).External.Waiting
		startWaiter(xctx(ctx, consumer, tenant), d, consumer, externalReqAt(origin), ch)
		waitFor(t, fmt.Sprintf("%s waiter %d on %s", consumer, i, origin), func() bool {
			return targetOf(d, origin).External.Waiting == before+1
		})
	}
}

// TestPredictiveSheddingRejectsWithoutEnqueue is gate (a), design/03 §7
// A03-W6 (a): E = 2, L = 5 s, external_max_wait 5 s, 20 simultaneous
// acquires of 20 consumers. Before A03-W6 18 of them queue and wait the 5 s
// out (ErrWaitExpired after waiting); now the prediction
// ŵ = ⌈(Σw + w) · n_w / E⌉ · L_p50 refuses at least 15 of them at once,
// without an enqueue and without a timer, and each refusal carries the
// Retry-After of its own position (n_w = 3 ⇒ ⌈3/2⌉ · 5 s = 10 s).
func TestPredictiveSheddingRejectsWithoutEnqueue(t *testing.T) {
	s := budgetSettings(5 * time.Second)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant = 0, 0
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1)) // E = 2
	seedLeases(d, testOrigin, waitRingK, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seq0 := targetSeq(d)

	type outcome struct {
		l   *Lease
		err error
		el  time.Duration
	}
	out := make(chan outcome, 20)
	var gate sync.WaitGroup
	gate.Add(1)
	for i := 0; i < 20; i++ {
		go func(i int) {
			gate.Wait()
			start := time.Now()
			l, _, err := d.Acquire(xctx(ctx, fmt.Sprintf("a%02d", i), "tenant-a"), externalReq())
			out <- outcome{l, err, time.Since(start)}
		}(i)
	}
	gate.Done()
	var leases []*Lease
	var refused []outcome
	deadline := time.After(time.Second)
collect:
	for len(leases)+len(refused) < 20 {
		select {
		case o := <-out:
			if o.l != nil {
				leases = append(leases, o.l)
			} else {
				refused = append(refused, o)
			}
		case <-deadline:
			break collect
		}
	}
	ts := target(t, d)
	enqueued := targetSeq(d) - seq0
	t.Logf("20 acquires: %d admitted, %d refused within 1 s, %d enqueued, waiting %d, rejects %+v",
		len(leases), len(refused), enqueued, ts.External.Waiting, ts.ExternalRejects)
	cancel()
	for i := len(leases) + len(refused); i < 20; i++ {
		if o := <-out; o.l != nil {
			leases = append(leases, o.l)
		}
	}
	for _, l := range leases {
		l.Release()
	}

	if len(refused) < 15 || ts.ExternalRejects.Predicted < 15 {
		t.Fatalf("%d refused at once, predicted %d, want ≥ 15 each (red: every waiter waits its 5 s out)", len(refused), ts.ExternalRejects.Predicted)
	}
	if enqueued > 5 || ts.External.Waiting > 5 {
		t.Fatalf("%d enqueues, %d waiting, want ≤ 5 (no dead wait slot)", enqueued, ts.External.Waiting)
	}
	for _, o := range refused {
		if !errors.Is(o.err, ErrWaitExpired) || !IsRejection(o.err) {
			t.Fatalf("refusal %v, want ErrWaitExpired as a rejection", o.err)
		}
		if o.el >= 50*time.Millisecond {
			t.Fatalf("refusal after %v, want at once (< 50 ms, no timer)", o.el)
		}
		if ra := retryAfterOf(t, o.err); ra != 10*time.Second {
			t.Fatalf("Retry-After %v, want 10s (own position: ⌈(0+1)·3/2⌉ · 5 s)", ra)
		}
	}
}

// timedExternalFor runs one external acquire with a caller ctx of ctxTimeout
// and returns its outcome and duration; a lease is released at cleanup.
func timedExternalFor(t *testing.T, d *Dispatcher, consumer string, req Request, ctxTimeout time.Duration) (*Lease, time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(xctx(context.Background(), consumer, "tenant-a"), ctxTimeout)
	defer cancel()
	start := time.Now()
	l, _, err := d.Acquire(ctx, req)
	el := time.Since(start)
	if l != nil {
		t.Cleanup(l.Release)
	}
	return l, el, err
}

// TestPredictionCountsWeightsAhead pins lead decision 1 (A03-W6): the own
// position counts the WEIGHTS of the identity's waiting acquires plus the
// newcomer's, not the acquires (design/03 §4.9 writes q_id + 1). E = 2,
// L_p50 = 5 s, external_max_wait 5 s: consumer c has one waiting acquire of
// weight 2; its next acquire (w = 1) needs ⌈(2+1)·1/2⌉ = 2 rounds = 10 s >
// 5 s — refused at once with Retry-After 10 s. Counted in acquires it would
// be ⌈(1+1)·1/2⌉ = 1 round = 5 s and queue.
func TestPredictionCountsWeightsAhead(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(5*time.Second), partitionPolicy(3, 1)) // E = 2
	holdExternal(t, d, testOrigin, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c", "tenant-a"), d, "heavy", weightedReq(2), ch)
	waitQueued(t, d, ch, 1, "heavy waiter (cold window)")
	seedLeases(d, testOrigin, waitRingK, 5*time.Second)
	start := time.Now()
	_, _, err := d.Acquire(xctx(ctx, "c", "tenant-a"), weightedReq(1))
	if !errors.Is(err, ErrWaitExpired) || time.Since(start) >= 50*time.Millisecond {
		t.Fatalf("newcomer behind its own weight-2 waiter = %v after %v, want predicted ErrWaitExpired at once", err, time.Since(start))
	}
	if ra := retryAfterOf(t, err); ra != 10*time.Second {
		t.Fatalf("Retry-After %v, want 10s (⌈(2+1)·1/2⌉ · 5 s)", ra)
	}
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Predicted: 1}) {
		t.Fatalf("rejects %+v, want predicted=1", rej)
	}
	cancel()
	recvAdmission(t, ch, "heavy waiter")
}

// TestServeByAccountsPredictedLeaseDuration is gate (b): ServeBy = now + 3 s,
// a warm lease window with p90 = 2 s, the target full until t = 2.4 s. Before
// A03-W6 the budget is 3 − 0.5 = 2.5 s and the acquire is admitted at 2.4 s
// — a lease that cannot end before ServeBy. Now MaxWait_eff = 3 − 2 − 0.5 =
// 0.5 s: with p50 = 2 s the prediction refuses at once; with p50 = 100 ms
// (the prediction passes) the shortened budget alone expires at 0.5 s. The
// release lies at 2.4 s, not at 2.5 s, so the red run is not a coin flip
// between the old 2.5-s timer and the handoff.
func TestServeByAccountsPredictedLeaseDuration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fast      int // samples of 100 ms; the rest are 2 s
		predicted bool
	}{
		{name: "p50=p90=2s: predicted at once", fast: 0, predicted: true},
		{name: "p50=100ms p90=2s: the ŝ term alone", fast: 307, predicted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1)) // E = 1
			d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, tc.fast, 100*time.Millisecond)
			d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, waitRingK-tc.fast, 2*time.Second)
			holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-h"), externalReq())
			start := time.Now()
			release := time.AfterFunc(2400*time.Millisecond, holder.Release)
			t.Cleanup(func() { release.Stop(); holder.Release() })
			req := externalReq()
			req.ServeBy = start.Add(3 * time.Second)
			l, el, err := timedExternalFor(t, d, "c1", req, 5*time.Second)
			rej := target(t, d).ExternalRejects
			t.Logf("outcome after %v: lease %v err %v rejects %+v", el, l != nil, err, rej)
			if l != nil || !errors.Is(err, ErrWaitExpired) || el > time.Second {
				t.Fatalf("ServeBy now+3s behind a 2-s p90 lease: (lease %v, %v) after %v, want ErrWaitExpired by t ≤ 1 s", l != nil, err, el)
			}
			if tc.predicted {
				if rej != (ExternalRejectStats{Predicted: 1}) || el >= 50*time.Millisecond {
					t.Fatalf("rejects %+v after %v, want predicted=1 at once", rej, el)
				}
				return
			}
			if rej != (ExternalRejectStats{Expired: 1}) || !within(el, 500*time.Millisecond, 100*time.Millisecond) {
				t.Fatalf("rejects %+v after %v, want expired=1 after 0.5 s ± 100 ms (budget 3 − 2 − 0.5)", rej, el)
			}
		})
	}
}

// TestRetryAfterFromOwnPositionNotClassDepth is gate (c), the masterplan's
// Retry-After oracle probe (X11): two targets with the same own position of
// consumer c1 (one waiter of its own, n_w = 2, E = 2, L_p50 = 4 s), one with
// no foreign waiters, one with 40 waiters of a foreign tenant. The class
// formula (depth × p95) differs between them — it is the load oracle — the
// Retry-After the refusal carries does not.
func TestRetryAfterFromOwnPositionNotClassDepth(t *testing.T) {
	s := budgetSettings(45 * time.Second)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	pol := Policy{Targets: map[string]TargetPolicy{
		testOrigin:  {Slots: 4, InternalReserved: 2},
		otherOrigin: {Slots: 4, InternalReserved: 2},
	}}
	d, _ := newTestDispatcher(t, s, pol)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 64)
	for _, o := range []string{testOrigin, otherOrigin} {
		holdExternal(t, d, o, 2)
		queueAt(t, d, ctx, o, "c1", "tenant-a", 1, ch)
	}
	queueAt(t, d, ctx, testOrigin, "c2", "tenant-a", 1, ch)
	queueAt(t, d, ctx, otherOrigin, "f1", "tenant-f", 40, ch)
	s.ExternalMaxWait = 5 * time.Second
	d.UpdateSettings(s)
	for _, o := range []string{testOrigin, otherOrigin} {
		seedLeases(d, o, waitRingK, 4*time.Second)
		for i := 0; i < waitRingK; i++ {
			d.SeedWaitSampleForTest(o, ClassExternal, time.Second)
		}
	}
	hintA, hintB := d.RetryAfterHint(testOrigin, ClassExternal), d.RetryAfterHint(otherOrigin, ClassExternal)
	if hintA == hintB {
		t.Fatalf("class formula equal (%v) — the probe does not stage the oracle", hintA)
	}
	_, _, errA := d.Acquire(xctx(ctx, "c1", "tenant-a"), externalReqAt(testOrigin))
	_, _, errB := d.Acquire(xctx(ctx, "c1", "tenant-a"), externalReqAt(otherOrigin))
	if !IsRejection(errA) || !IsRejection(errB) {
		t.Fatalf("c1 newcomers = %v / %v, want rejections on both targets", errA, errB)
	}
	raA, raB := retryAfterOf(t, errA), retryAfterOf(t, errB)
	t.Logf("class formula %v vs %v; own-position Retry-After %v (%v) vs %v (%v)", hintA, hintB, raA, errA, raB, errB)
	if raA != raB {
		t.Fatalf("Retry-After %v vs %v with the same own position — foreign depth leaks", raA, raB)
	}
	if raA != 10*time.Second {
		t.Fatalf("Retry-After %v, want 10s (2 units at the own share E·(1/T_w)·(fw/Σfw) = 1 unit: 2 rounds · 4 s = 8 s, quantized up)", raA)
	}
	cancel()
	for i := 0; i < 43; i++ {
		recvAdmission(t, ch, "canceled waiter")
	}
}

// TestRetryAfterIgnoresForeignConsumerCount is gate (c) for the foreign
// consumer COUNT (review 1 F-2): one foreign tenant waiting with one consumer
// or with ten — the own position (one own waiter, E = 2, L_p50 = 2 s) and
// with it the Retry-After stay the same; only the number of waiting tenants
// enters the share (the flat n_w of design §4.9 gave 5 s vs 30 s).
func TestRetryAfterIgnoresForeignConsumerCount(t *testing.T) {
	s := budgetSettings(45 * time.Second)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	pol := Policy{Targets: map[string]TargetPolicy{
		testOrigin:  {Slots: 4, InternalReserved: 2},
		otherOrigin: {Slots: 4, InternalReserved: 2},
	}}
	d, _ := newTestDispatcher(t, s, pol)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 16)
	for _, o := range []string{testOrigin, otherOrigin} {
		holdExternal(t, d, o, 2)
		queueAt(t, d, ctx, o, "x", "tenant-a", 1, ch)
	}
	queueAt(t, d, ctx, testOrigin, "f0", "tenant-f", 1, ch)
	for i := 0; i < 10; i++ {
		queueAt(t, d, ctx, otherOrigin, fmt.Sprintf("f%d", i), "tenant-f", 1, ch)
	}
	s.ExternalMaxWait = time.Second
	d.UpdateSettings(s)
	for _, o := range []string{testOrigin, otherOrigin} {
		seedLeases(d, o, waitRingK, 2*time.Second)
	}
	_, _, errA := d.Acquire(xctx(ctx, "x", "tenant-a"), externalReqAt(testOrigin))
	_, _, errB := d.Acquire(xctx(ctx, "x", "tenant-a"), externalReqAt(otherOrigin))
	raA, raB := retryAfterOf(t, errA), retryAfterOf(t, errB)
	t.Logf("one vs ten foreign consumers: Retry-After %v (%v) vs %v (%v)", raA, errA, raB, errB)
	if raA != raB || raA != 5*time.Second {
		t.Fatalf("Retry-After %v vs %v, want 5s on both (2 units at share 2·(1/2)·(1/1) = 1: 2 rounds · 2 s = 4 s, quantized)", raA, raB)
	}
	cancel()
	for i := 0; i < 13; i++ {
		recvAdmission(t, ch, "canceled waiter")
	}
}

// TestPredictionFollowsTenantShares pins review 1 F-1: the own share follows
// the two levels of the A03-W4 pick. (a) Tenant A has eight waiting
// consumers, tenant B's lone consumer arrives (E = 2, L_p50 = 5 s,
// external_max_wait 20 s): B's share is a whole tenant's, E/2 = 1 unit, so
// ŵ = 5 s and B queues — the pick serves it at the second release (flat n_w:
// ⌈9/2⌉·5 s = 25 s, predicted). A's ninth consumer queues no more. (c) Tenant
// A alone fills the relational cap R = ⌈1.5·2·20/5⌉ = 12 (queued while the
// window was cold); B still finds its tenant share ⌈12/2⌉ = 6 of it, and A's
// next acquire meets its share.
func TestPredictionFollowsTenantShares(t *testing.T) {
	t.Run("(a) a lone consumer of another tenant is not shed", func(t *testing.T) {
		d, _ := newTestDispatcher(t, budgetSettings(20*time.Second), partitionPolicy(3, 1)) // E = 2
		holdExternal(t, d, testOrigin, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 16)
		for i := 0; i < 8; i++ {
			queueAt(t, d, ctx, testOrigin, fmt.Sprintf("a%d", i), "tenant-a", 1, ch)
		}
		seedLeases(d, testOrigin, waitRingK, 5*time.Second)
		queueAt(t, d, ctx, testOrigin, "b", "tenant-b", 1, ch)
		_, _, err := d.Acquire(xctx(ctx, "a8", "tenant-a"), externalReq())
		if !IsRejection(err) {
			t.Fatalf("A's ninth consumer = %v, want a rejection (tenant share of A exhausted)", err)
		}
		if diff, _ := d.fairAuditForTest(testOrigin); diff != "" {
			t.Fatalf("position counters differ from their recomputation: %s", diff)
		}
		cancel()
		for i := 0; i < 9; i++ {
			recvAdmission(t, ch, "canceled waiter")
		}
	})
	t.Run("(b) the own tenant counts once it waits", func(t *testing.T) {
		// E = 1, L_p50 = 5 s, external_max_wait 5 s: tenant A waits with one
		// consumer; B's newcomer shares E with A — T_w = 2, E_id = 1/2 unit,
		// ⌈1·2/1⌉ = 2 rounds = 10 s > 5 s ⇒ predicted, Retry-After 10 s. Without
		// its own tenant counted it would see all of E and queue.
		d, _ := newTestDispatcher(t, budgetSettings(5*time.Second), partitionPolicy(2, 1)) // E = 1
		holdExternal(t, d, testOrigin, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 1)
		queueAt(t, d, ctx, testOrigin, "a", "tenant-a", 1, ch)
		seedLeases(d, testOrigin, waitRingK, 5*time.Second)
		_, _, err := d.Acquire(xctx(ctx, "b", "tenant-b"), externalReq())
		if !errors.Is(err, ErrWaitExpired) || retryAfterOf(t, err) != 10*time.Second {
			t.Fatalf("B's newcomer next to A = %v, want predicted ErrWaitExpired with Retry-After 10s (T_w = 2)", err)
		}
		cancel()
		recvAdmission(t, ch, "canceled waiter")
	})
	t.Run("(c) one tenant never fills the relational cap for all", func(t *testing.T) {
		d, _ := newTestDispatcher(t, budgetSettings(20*time.Second), partitionPolicy(3, 1)) // E = 2
		holdExternal(t, d, testOrigin, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 16)
		for i := 0; i < 3; i++ {
			queueAt(t, d, ctx, testOrigin, fmt.Sprintf("a%d", i), "tenant-a", 4, ch)
		}
		seedLeases(d, testOrigin, waitRingK, 5*time.Second)
		queueAt(t, d, ctx, testOrigin, "b", "tenant-b", 1, ch)
		_, _, err := d.Acquire(xctx(ctx, "a3", "tenant-a"), externalReq())
		if !errors.Is(err, ErrTargetSaturated) {
			t.Fatalf("A's next acquire = %v, want ErrTargetSaturated at A's share ⌈12/2⌉ = 6 of the relational cap", err)
		}
		if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Cap: 1}) {
			t.Fatalf("rejects %+v, want cap=1", rej)
		}
		cancel()
		for i := 0; i < 13; i++ {
			recvAdmission(t, ch, "canceled waiter")
		}
	})
}

// TestRelationalCapIsSplitAmongWaitingTenants pins the split itself (delta
// review D-1b): R = ⌈1.5·2·20/5⌉ = 12 over T_w = 2 waiting tenants gives each
// ⌈12/2⌉ = 6. Tenant A holds 8 waiting acquires (queued while the window was
// cold), B one; a newcomer of A with fair_weight 100 would pass the prediction
// (⌈1·2·180/(2·100)⌉ = 2 rounds = 10 s ≤ 20 s) and the whole R (8 < 12), but
// meets A's share 6 — ErrTargetSaturated.
func TestRelationalCapIsSplitAmongWaitingTenants(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(20*time.Second), partitionPolicy(3, 1)) // E = 2
	holdExternal(t, d, testOrigin, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 16)
	for i := 0; i < 2; i++ {
		queueAt(t, d, ctx, testOrigin, fmt.Sprintf("a%d", i), "tenant-a", 4, ch)
	}
	queueAt(t, d, ctx, testOrigin, "b", "tenant-b", 1, ch)
	seedLeases(d, testOrigin, waitRingK, 5*time.Second)
	p := xuPrincipal("heavy", "tenant-a")
	p.FairWeight = 100
	_, _, err := d.Acquire(withPrincipal(ctx, p), externalReq())
	if !errors.Is(err, ErrTargetSaturated) {
		t.Fatalf("A's heavy newcomer at 8 waiting = %v, want ErrTargetSaturated at A's share ⌈12/2⌉ = 6", err)
	}
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Cap: 1}) {
		t.Fatalf("rejects %+v, want cap=1", rej)
	}
	cancel()
	for i := 0; i < 9; i++ {
		recvAdmission(t, ch, "canceled waiter")
	}
}

// TestRelationalQueueCapRoundsUp pins the ceiling of the relational cap
// (review 1 F-3): E = 3, L_p50 = 7 s, external_max_wait 10 s ⇒ 1.5·3·10/7 =
// 6.43 ⇒ 7. With 6 waiting the cap passes (the prediction refuses: reason
// predicted); with 7 (the 7th a continuation, which passes both) the next
// acquire meets the cap. Floor (6) would refuse at 6 already.
func TestRelationalQueueCapRoundsUp(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(10*time.Second), partitionPolicy(4, 1)) // E = 3
	holdExternal(t, d, testOrigin, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 8)
	for i := 0; i < 6; i++ {
		queueAt(t, d, ctx, testOrigin, fmt.Sprintf("w%d", i), "tenant-a", 1, ch)
	}
	seedLeases(d, testOrigin, waitRingK, 7*time.Second)
	if _, _, err := d.Acquire(xctx(ctx, "n1", "tenant-a"), externalReq()); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("newcomer at 6 waiting = %v, want the prediction's ErrWaitExpired (the cap 7 passes)", err)
	}
	startWaiter(xctx(ctx, "w0", "tenant-a"), d, "cont", contReq(1), ch)
	waitQueued(t, d, ch, 7, "continuation")
	if _, _, err := d.Acquire(xctx(ctx, "n2", "tenant-a"), externalReq()); !errors.Is(err, ErrTargetSaturated) {
		t.Fatalf("newcomer at 7 waiting = %v, want ErrTargetSaturated at the cap 7", err)
	}
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Cap: 1, Predicted: 1}) {
		t.Fatalf("rejects %+v, want cap=1 predicted=1", rej)
	}
	cancel()
	for i := 0; i < 7; i++ {
		recvAdmission(t, ch, "canceled waiter")
	}
}

// TestRelationalTenantShareRoundsUp pins the ceiling of the tenant share of
// the relational cap (delta review D-1): E = 2, L_p50 = 5 s,
// external_max_wait 20 s ⇒ R = 12 over T_w = 5 waiting tenants ⇒ ⌈12/5⌉ = 3
// (floor 2). Tenant A holds 2 waiting acquires; a newcomer of A with
// fair_weight 100 passes the prediction (⌈1·5·102/(2·100)⌉ = 3 rounds = 15 s
// ≤ 20 s) and queues as A's third.
func TestRelationalTenantShareRoundsUp(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(20*time.Second), partitionPolicy(3, 1)) // E = 2
	holdExternal(t, d, testOrigin, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fwCtx := func(consumer, tenant string, fw int) context.Context {
		p := xuPrincipal(consumer, tenant)
		p.FairWeight = fw
		return withPrincipal(ctx, p)
	}
	ch := make(chan admission, 8)
	for i, ct := range [][2]string{{"a0", "A"}, {"a1", "A"}, {"b", "B"}, {"c", "C"}, {"d", "D"}, {"e", "E"}} {
		startWaiter(fwCtx(ct[0], ct[1], 1), d, ct[0], externalReq(), ch)
		waitQueued(t, d, ch, i+1, ct[0])
	}
	seedLeases(d, testOrigin, waitRingK, 5*time.Second)
	startWaiter(fwCtx("heavy", "A", 100), d, "heavy", externalReq(), ch)
	waitQueued(t, d, ch, 7, "A's third at the tenant share ⌈12/5⌉ = 3")
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{}) {
		t.Fatalf("rejects %+v, want none", rej)
	}
	cancel()
	for i := 0; i < 7; i++ {
		recvAdmission(t, ch, "canceled waiter")
	}
}

// TestExpiredRetryAfterIsTheOwnPosition pins review 1 F-4: an expired waiter's
// Retry-After is computed after it left the queue — its own weight counts
// once. E = 1, L_p50 = 100 ms, budget 150 ms, steps {150 ms, 250 ms}: one
// round (100 ms) ⇒ 150 ms; counting the waiter twice would give 200 ms ⇒
// 250 ms. (Sub-second steps are reachable only through Settings, never
// through the whole-second config grammar.)
func TestExpiredRetryAfterIsTheOwnPosition(t *testing.T) {
	s := budgetSettings(150 * time.Millisecond)
	s.ExternalRetryAfterSteps = []time.Duration{150 * time.Millisecond, 250 * time.Millisecond}
	d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1)) // E = 1
	holdExternal(t, d, testOrigin, 1)
	seedLeases(d, testOrigin, waitRingK, 100*time.Millisecond)
	l, el, err := timedExternalFor(t, d, "c", externalReq(), 3*time.Second)
	if l != nil || !errors.Is(err, ErrWaitExpired) || !within(el, 150*time.Millisecond, 50*time.Millisecond) {
		t.Fatalf("waiter = (lease %v, %v) after %v, want ErrWaitExpired after the 150-ms budget", l != nil, err, el)
	}
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Expired: 1}) {
		t.Fatalf("rejects %+v, want expired=1 (it queued: ŵ 100 ms ≤ 150 ms)", rej)
	}
	if ra := retryAfterOf(t, err); ra != 150*time.Millisecond {
		t.Fatalf("expired Retry-After %v, want 150ms (one round at the own position after leaving the queue)", ra)
	}
}

// isStep reports whether d is one of the steps.
func isStep(d time.Duration, steps []time.Duration) bool {
	for _, s := range steps {
		if d == s {
			return true
		}
	}
	return false
}

// TestRetryAfterAlwaysAStep is gate (c), second half: 1 000 refusals over
// random positions (consumer, weight, lease p50 between 1.1 s and 120 s)
// carry a Retry-After from the step set, never a raw estimate.
func TestRetryAfterAlwaysAStep(t *testing.T) {
	s := budgetSettings(45 * time.Second)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
	holdExternal(t, d, testOrigin, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rng := rand.New(rand.NewSource(6))
	ch := make(chan admission, 64)
	queued := 0
	for k := 0; k < 6; k++ {
		n := rng.Intn(5)
		queueAt(t, d, ctx, testOrigin, fmt.Sprintf("k%d", k), fmt.Sprintf("tenant-%d", k%3), n, ch)
		queued += n
	}
	s.ExternalMaxWait = time.Second
	d.UpdateSettings(s)
	steps := DefaultSettings().ExternalRetryAfterSteps
	seen := map[time.Duration]int{}
	for i := 0; i < 1000; i++ {
		p50 := 1100*time.Millisecond + time.Duration(rng.Int63n(int64(119*time.Second)))
		seedLeases(d, testOrigin, waitRingK, p50)
		req := weightedReq(1 + rng.Intn(2))
		k := rng.Intn(10)
		_, _, err := d.Acquire(xctx(ctx, fmt.Sprintf("k%d", k), fmt.Sprintf("tenant-%d", k%3)), req)
		if !IsRejection(err) {
			t.Fatalf("refusal %d = %v, want a rejection", i, err)
		}
		ra := retryAfterOf(t, err)
		if !isStep(ra, steps) {
			t.Fatalf("refusal %d: Retry-After %v not in the step set %v", i, ra, steps)
		}
		seen[ra]++
	}
	t.Logf("1000 refusals over random positions: Retry-After histogram %v", seen)
	if len(seen) < 2 {
		t.Fatalf("Retry-After histogram %v — one value only, the positions did not vary", seen)
	}
	cancel()
	for i := 0; i < queued; i++ {
		recvAdmission(t, ch, "canceled waiter")
	}
}

// TestRetryAfterStepsAreASetting is gate (d): the steps are the setting, not
// a literal. With steps {3 s, 7 s} every refusal carries 3 s or 7 s — the
// small positions round up to 3 s, the large ones are capped at 7 s, the
// largest step — on the warm path and the cold one alike, and the class hint
// of RetryAfterHint too.
func TestRetryAfterStepsAreASetting(t *testing.T) {
	s := budgetSettings(45 * time.Second)
	s.ExternalQueueMax = 1
	s.ExternalRetryAfterSteps = []time.Duration{3 * time.Second, 7 * time.Second}
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1)) // E = 2
	holdExternal(t, d, testOrigin, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	queueAt(t, d, ctx, testOrigin, "q", "tenant-a", 1, ch)
	for i := 0; i < waitRingK; i++ {
		d.SeedWaitSampleForTest(testOrigin, ClassExternal, 2*time.Second)
	}
	refusal := func() time.Duration {
		t.Helper()
		_, _, err := d.Acquire(xctx(ctx, "c", "tenant-a"), externalReq())
		if !errors.Is(err, ErrTargetSaturated) {
			t.Fatalf("newcomer over queue_max 1 = %v, want ErrTargetSaturated", err)
		}
		return retryAfterOf(t, err)
	}
	if got := refusal(); got != 3*time.Second {
		t.Fatalf("cold: Retry-After %v, want 3s (depth 1 × p95 2 s = 2 s, rounded up to the step 3 s)", got)
	}
	for _, tc := range []struct {
		p50  time.Duration
		want time.Duration
	}{
		{100 * time.Millisecond, 3 * time.Second}, // ⌈1·2/2⌉ · 0.1 s
		{3 * time.Second, 3 * time.Second},
		{5 * time.Second, 7 * time.Second},
		{60 * time.Second, 7 * time.Second}, // the largest step caps
	} {
		seedLeases(d, testOrigin, waitRingK, tc.p50)
		if got := refusal(); got != tc.want {
			t.Fatalf("p50 %v: Retry-After %v, want %v", tc.p50, got, tc.want)
		}
	}
	if got := d.RetryAfterHint(testOrigin, ClassExternal); got != 3*time.Second {
		t.Fatalf("class hint %v, want 3s (depth 1 × p95 2 s, quantized on the setting)", got)
	}
	cancel()
	recvAdmission(t, ch, "canceled waiter")
}

// TestRelationalQueueCap is gate (e): the effective aggregate cap is
// min(external_queue_max, ⌈1.5 · E · MaxWait / L_p50⌉).
func TestRelationalQueueCap(t *testing.T) {
	t.Run("E=2 L50=10s MaxWait=5s ⇒ 2", func(t *testing.T) {
		d, _ := newTestDispatcher(t, budgetSettings(5*time.Second), partitionPolicy(3, 1)) // queue_max 64
		holdExternal(t, d, testOrigin, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 2)
		queueAt(t, d, ctx, testOrigin, "w1", "tenant-a", 1, ch) // cold ring: no prediction, no relational cap
		queueAt(t, d, ctx, testOrigin, "w2", "tenant-a", 1, ch)
		seedLeases(d, testOrigin, waitRingK, 10*time.Second)
		seq := targetSeq(d)
		start := time.Now()
		_, _, err := d.Acquire(xctx(ctx, "w3", "tenant-a"), externalReq())
		el := time.Since(start)
		rej := target(t, d).ExternalRejects
		if !errors.Is(err, ErrTargetSaturated) || el >= 50*time.Millisecond {
			t.Fatalf("third waiter = %v after %v, want ErrTargetSaturated at once (red: queued under queue_max 64)", err, el)
		}
		if rej != (ExternalRejectStats{Cap: 1}) || targetSeq(d) != seq {
			t.Fatalf("rejects %+v, seq %d → %d, want cap=1 without an enqueue", rej, seq, targetSeq(d))
		}
		if ra := retryAfterOf(t, err); ra != 20*time.Second {
			t.Fatalf("Retry-After %v, want 20s (⌈1·3/2⌉ · 10 s)", ra)
		}
		cancel()
		recvAdmission(t, ch, "w1")
		recvAdmission(t, ch, "w2")
	})
	t.Run("staggered identities: the cap binds where the prediction alone overfills", func(t *testing.T) {
		// E = 2, L = 5 s, MaxWait = 15 s: the prediction admits (Σw+w)·n_w ≤ 6
		// per identity — A alone queues 6, B next to A 3, C next to A and B
		// would queue 2 more — while the aggregate cap is ⌈1.5·2·15/5⌉ = 9:
		// C's first acquire meets the cap (the ladder runs before the
		// prediction; without the relational term C would queue).
		d, _ := newTestDispatcher(t, budgetSettings(15*time.Second), partitionPolicy(3, 1))
		holdExternal(t, d, testOrigin, 2)
		seedLeases(d, testOrigin, waitRingK, 5*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 16)
		queueAt(t, d, ctx, testOrigin, "A", "tenant-a", 6, ch)
		if _, _, err := d.Acquire(xctx(ctx, "A", "tenant-a"), externalReq()); !errors.Is(err, ErrWaitExpired) {
			t.Fatalf("A's 7th = %v, want predicted ErrWaitExpired ((6+1)·1 > 6)", err)
		}
		queueAt(t, d, ctx, testOrigin, "B", "tenant-a", 3, ch)
		_, _, err := d.Acquire(xctx(ctx, "C", "tenant-a"), externalReq())
		ts := target(t, d)
		if !errors.Is(err, ErrTargetSaturated) || ts.External.Waiting != 9 {
			t.Fatalf("C's 1st = %v with %d waiting, want ErrTargetSaturated at the relational cap 9", err, ts.External.Waiting)
		}
		if ts.ExternalRejects != (ExternalRejectStats{Cap: 1, Predicted: 1}) {
			t.Fatalf("rejects %+v, want cap=1 predicted=1", ts.ExternalRejects)
		}
		cancel()
		for i := 0; i < 9; i++ {
			recvAdmission(t, ch, "canceled waiter")
		}
	})
}

// TestColdLeaseRingNoPrediction is gate (f): below leaseColdSamples (32)
// published lease samples there is no prediction, no relational cap and no ŝ;
// the Retry-After of a refusal is the class formula of the external class,
// quantized. The 32nd sample warms the window.
func TestColdLeaseRingNoPrediction(t *testing.T) {
	t.Run("31 samples: queues past the warm caps, class Retry-After", func(t *testing.T) {
		s := budgetSettings(5 * time.Second)
		s.ExternalQueueMax = 4
		d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1)) // E = 2
		holdExternal(t, d, testOrigin, 2)
		seedLeases(d, testOrigin, leaseColdSamples-1, 10*time.Second)
		for i := 0; i < waitRingK; i++ {
			d.SeedWaitSampleForTest(testOrigin, ClassExternal, 3*time.Second)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan admission, 4)
		for i := 0; i < 4; i++ { // warm: every one predicted (ŵ ≥ 10 s > 5 s), relational cap 2
			queueAt(t, d, ctx, testOrigin, fmt.Sprintf("w%d", i), "tenant-a", 1, ch)
		}
		_, _, err := d.Acquire(xctx(ctx, "w4", "tenant-a"), externalReq())
		if !errors.Is(err, ErrTargetSaturated) {
			t.Fatalf("5th = %v, want ErrTargetSaturated at the absolute cap 4", err)
		}
		if ra := retryAfterOf(t, err); ra != 20*time.Second {
			t.Fatalf("cold Retry-After %v, want 20s (class formula 4 × 3 s = 12 s, quantized)", ra)
		}
		if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Cap: 1}) {
			t.Fatalf("rejects %+v, want cap=1 only (no prediction)", rej)
		}
		seedLeases(d, testOrigin, 1, 10*time.Second) // the 32nd sample: warm
		_, _, err = d.Acquire(xctx(ctx, "w5", "tenant-a"), externalReq())
		if !errors.Is(err, ErrTargetSaturated) || retryAfterOf(t, err) != 30*time.Second {
			t.Fatalf("warm 6th = %v, want ErrTargetSaturated with Retry-After 30s (⌈1·5/2⌉ · 10 s)", err)
		}
		cancel()
		for i := 0; i < 4; i++ {
			recvAdmission(t, ch, "canceled waiter")
		}
	})
	t.Run("31 samples: ŝ = 0 in the ServeBy term", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1)) // E = 1
		seedLeases(d, testOrigin, leaseColdSamples-1, 10*time.Second)
		holdExternal(t, d, testOrigin, 1)
		req := externalReq()
		req.ServeBy = time.Now().Add(time.Second)
		l, el, err := timedExternalFor(t, d, "c1", req, 3*time.Second)
		if l != nil || !errors.Is(err, ErrWaitExpired) || !within(el, 500*time.Millisecond, 100*time.Millisecond) {
			t.Fatalf("cold ServeBy now+1s = (lease %v, %v) after %v, want ErrWaitExpired after 0.5 s (ŝ = 0)", l != nil, err, el)
		}
		if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Expired: 1}) {
			t.Fatalf("rejects %+v, want expired=1", rej)
		}
	})
}

// TestLeaseDurationRingSamplesEveryLeaseEnd: one sample per lease end, per
// target × class (continuation leases included), the window exact in the
// snapshot (p50/p90 nearest rank) and published for the hot path.
func TestLeaseDurationRingSamplesEveryLeaseEnd(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	bg := context.Background()
	for i := 0; i < 3; i++ {
		mustAcquire(t, d, xctx(bg, "x", "tenant-a"), externalReq()).Release()
	}
	mustAcquire(t, d, xctx(bg, "x", "tenant-a"), contReq(1)).Release()
	for i := 0; i < 2; i++ {
		mustAcquire(t, d, ictx(principal("p")), interactiveReq()).Release()
	}
	mustAcquire(t, d, bg, backgroundReq()).Release()
	ts := target(t, d)
	if got := ts.LeaseDur[ClassExternal].Samples; got != 4 {
		t.Fatalf("external lease samples %d, want 4 (three leases and one continuation)", got)
	}
	if ts.LeaseDur[ClassInteractive].Samples != 2 || ts.LeaseDur[ClassBackground].Samples != 1 {
		t.Fatalf("lease samples interactive %d background %d, want 2/1", ts.LeaseDur[ClassInteractive].Samples, ts.LeaseDur[ClassBackground].Samples)
	}
	d2, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	d2.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 60, time.Second)
	d2.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 30, 2*time.Second)
	d2.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 10, 9*time.Second)
	ls := target(t, d2).LeaseDur[ClassExternal]
	if ls.Samples != 100 || ls.P50 != time.Second || ls.P90 != 2*time.Second {
		t.Fatalf("lease window %+v, want 100 samples, p50 1s, p90 2s (nearest rank)", ls)
	}
}

// TestRejectionErrorUnwrapsToSentinel: every external rejection is a
// *RejectionError over its sentinel — errors.Is and IsRejection see the
// sentinel unchanged (the one handler check point), RetryAfter is set.
func TestRejectionErrorUnwrapsToSentinel(t *testing.T) {
	for _, tc := range []struct {
		sentinel error
		set      func(*Settings)
	}{
		{ErrPrincipalSaturated, func(s *Settings) { s.ExternalQueuePerConsumer, s.ExternalMaxWait = 1, 45*time.Second }},
		{ErrTenantSaturated, func(s *Settings) { s.ExternalQueuePerTenant, s.ExternalMaxWait = 1, 45*time.Second }},
		{ErrTargetSaturated, func(s *Settings) { s.ExternalQueueMax, s.ExternalMaxWait = 1, 45*time.Second }},
		{ErrWaitExpired, func(*Settings) {}},
	} {
		s := budgetSettings(time.Millisecond)
		s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
		tc.set(&s)
		d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
		holdExternal(t, d, testOrigin, 1)
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan admission, 1)
		queued := s.ExternalMaxWait > time.Millisecond
		if queued {
			queueAt(t, d, ctx, testOrigin, "c", "tenant-a", 1, ch)
		}
		_, _, err := d.Acquire(xctx(ctx, "c", "tenant-a"), externalReq())
		var re *RejectionError
		if !errors.Is(err, tc.sentinel) || !IsRejection(err) || !errors.As(err, &re) || re.RetryAfter <= 0 ||
			re.Err != tc.sentinel { //nolint:errorlint // identity is the contract: Err is the sentinel itself
			t.Fatalf("external refusal = %v (%T), want a *RejectionError over %v with a Retry-After", err, err, tc.sentinel)
		}
		if err.Error() != tc.sentinel.Error() {
			t.Fatalf("error text %q, want the sentinel's %q (server-side text unchanged)", err.Error(), tc.sentinel.Error())
		}
		cancel()
		if queued {
			recvAdmission(t, ch, "canceled waiter")
		}
	}
}

// TestPredictionLeavesNoResidue is gate (g): 10 000 mixed acquires with the
// prediction active (warm lease window, short budgets, weights, ServeBy,
// continuations, cancels) leave the queue counters, heldBy and the waiting
// identities equal to their recomputation, nothing held, and the goroutine
// count unchanged. Run with -race -count=3.
func TestPredictionLeavesNoResidue(t *testing.T) {
	const self = "TestPredictionLeavesNoResidue"
	s := budgetSettings(30 * time.Millisecond)
	d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2)) // E = 2
	for i := 0; i < waitRingK; i++ {
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 1, time.Duration(1+i%8)*time.Millisecond)
	}
	before := settledGateGoroutines(self)
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[string]int{}
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 625; i++ {
				req := weightedReq(1 + rng.Intn(2))
				switch rng.Intn(10) {
				case 0:
					req.Continuation = true
				case 1:
					req.ServeBy = time.Now().Add(time.Duration(rng.Intn(40)) * time.Millisecond)
				}
				k := rng.Intn(8)
				ctx, cancel := context.WithTimeout(xctx(context.Background(), fmt.Sprintf("k%d", k), fmt.Sprintf("tenant-%d", k%3)),
					time.Duration(5+rng.Intn(60))*time.Millisecond)
				l, _, err := d.Acquire(ctx, req)
				key := "lease"
				switch {
				case l != nil:
					time.Sleep(time.Duration(rng.Intn(3)) * time.Millisecond)
					l.Release()
				case errors.Is(err, ErrWaitExpired):
					key = "wait_expired"
				case IsRejection(err):
					key = "cap"
				case errors.Is(err, context.DeadlineExceeded):
					key = "ctx"
				default:
					t.Errorf("acquire: %v", err)
				}
				cancel()
				mu.Lock()
				outcomes[key]++
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	ts := target(t, d)
	t.Logf("10000 acquires: %v; rejects %+v; lease window %+v", outcomes, ts.ExternalRejects, ts.LeaseDur[ClassExternal])
	if ts.Held != 0 || ts.Inflight != 0 || ts.External.Waiting != 0 || externalQueueResidue(d) != 0 {
		t.Fatalf("residue: held %d inflight %d waiting %d queue %d, want 0", ts.Held, ts.Inflight, ts.External.Waiting, externalQueueResidue(d))
	}
	if diff, _ := d.fairAuditForTest(testOrigin); diff != "" {
		t.Fatalf("fairness bookkeeping differs from its recomputation: %s", diff)
	}
	if ts.ExternalRejects.Predicted == 0 || outcomes["lease"] == 0 {
		t.Fatalf("vacuous: %v rejects %+v — the prediction and admissions must both occur", outcomes, ts.ExternalRejects)
	}
	deadline := time.Now().Add(2 * time.Second)
	for gateGoroutines(self) != before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if after := gateGoroutines(self); after != before {
		t.Fatalf("goroutines before %d, after %d — want ±0", before, after)
	}
}

// TestExpireWaiterIsMeasured is gate (h), second half (A03-W5 review F7):
// the timer branch's mutex section counts in OpsTotal/MaxOpDur like every
// Acquire section — 1 000 expiries add 1 000 sections of their own (red: an
// expiring acquire is one section, the Acquire's).
func TestExpireWaiterIsMeasured(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(time.Millisecond), partitionPolicy(2, 1))
	holdExternal(t, d, testOrigin, 1)
	d.Close() // no publication pass may add sections while the gate counts
	ops0 := d.Snapshot().OpsTotal
	for i := 0; i < 1000; i++ {
		_, _, err := d.Acquire(xctx(context.Background(), "c", "tenant-a"), externalReq())
		if !errors.Is(err, ErrWaitExpired) {
			t.Fatalf("expiry %d = %v, want ErrWaitExpired", i, err)
		}
	}
	ts := target(t, d)
	got := d.Snapshot().OpsTotal - ops0
	if ts.ExternalRejects.Expired != 1000 || got != 2000 {
		t.Fatalf("1000 expiries: expired %d, OpsTotal +%d, want 1000 / +2000 (Acquire + expireWaiter each)", ts.ExternalRejects.Expired, got)
	}
}

// TestHandoffKeepsRecordFairWeight is the A03-W4 bot-review hand-over (PR
// #132, review 2 F-6): a queued external waiter that is handed a lease keeps
// the record's current fair weight — a newer waiter of the same consumer
// raised it to 3 — instead of re-entering with its own stale fair_weight 1.
// An immediate admission still takes the principal's weight.
func TestHandoffKeepsRecordFairWeight(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(45*time.Second), partitionPolicy(2, 1)) // E = 1
	bg := context.Background()
	fw := func(w int) context.Context {
		p := xuPrincipal("c", "tenant-a")
		p.FairWeight = w
		return withPrincipal(bg, p)
	}
	record := func() (int, int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.targets[canonicalOrigin(testOrigin)]
		c := st.external.fair.consumers[xuPrincipal("c", "tenant-a").identityKey()]
		return c.fw, c.tenant.sumFW
	}
	holder := mustAcquire(t, d, xctx(bg, "holder", "tenant-h"), externalReq())
	ch := make(chan admission, 2)
	startWaiter(fw(1), d, "old", externalReq(), ch)
	waitQueued(t, d, ch, 1, "old waiter")
	startWaiter(fw(3), d, "new", externalReq(), ch)
	waitQueued(t, d, ch, 2, "new waiter")
	if diff, _ := d.fairAuditForTest(testOrigin); diff != "" { // the tenant's waiting Σfw follows the record to 3
		t.Fatalf("bookkeeping after the fair-weight change: %s", diff)
	}
	holder.Release()
	old := recvAdmission(t, ch, "old waiter admitted")
	if old.label != "old" || old.lease == nil {
		t.Fatalf("first admission %s (lease %v, %v), want the older waiter", old.label, old.lease != nil, old.err)
	}
	if got, sum := record(); got != 3 || sum != 3 {
		t.Fatalf("record fair weight %d (tenant Σfw %d) after the handoff, want 3/3 — the newer waiter's weight", got, sum)
	}
	old.lease.Release()
	recvAdmission(t, ch, "new waiter admitted").lease.Release()
	l := mustAcquire(t, d, fw(5), externalReq()) // immediate admission: the principal's weight
	if got, _ := record(); got != 5 {
		t.Fatalf("record fair weight %d after an immediate admission, want the principal's 5", got)
	}
	l.Release()
}

// TestRetryAfterHintUnknownSpellingNotCachedNil is the A03-W17 review 2 F-3
// pin: a hint for a spelling of a target that does not exist yet answers 0
// and is NOT cached as nil — once an acquire created the target, the same
// spelling yields an estimate (≥ the 1-s floor).
func TestRetryAfterHintUnknownSpellingNotCachedNil(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	const spelling = "http://GPU:8089/v1"
	if got := d.RetryAfterHint(spelling, ClassInteractive); got != 0 {
		t.Fatalf("hint before the target exists = %v, want 0", got)
	}
	mustAcquire(t, d, ictx(principal("p")), interactiveReq()).Release()
	if got := d.RetryAfterHint(spelling, ClassInteractive); got < retryAfterFloor {
		t.Fatalf("hint after the target exists = %v, want ≥ %v (the spelling was cached as unknown)", got, retryAfterFloor)
	}
}

// TestParseRetryAfterSteps pins the step grammar the setting validator (V46)
// and the owner mapping share: comma-separated whole seconds ≥ 1, strictly
// ascending, at least one entry.
func TestParseRetryAfterSteps(t *testing.T) {
	good := map[string][]time.Duration{
		"1,2,5,10,20,30": DefaultSettings().ExternalRetryAfterSteps,
		" 3 , 7 ":        {3 * time.Second, 7 * time.Second},
		"42":             {42 * time.Second},
	}
	for raw, want := range good {
		got, err := ParseRetryAfterSteps(raw)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("ParseRetryAfterSteps(%q) = %v, %v, want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", " ", "0,5", "5,2", "2,2", "a", "1,,2", "1.5", "-1", "99999999999"} {
		if got, err := ParseRetryAfterSteps(raw); err == nil {
			t.Fatalf("ParseRetryAfterSteps(%q) = %v, want an error", raw, got)
		}
	}
}
