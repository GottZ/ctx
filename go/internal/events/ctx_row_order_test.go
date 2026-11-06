package events

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// The order probe of F-W7 (design/04 §7 F-W7 (b), Finding 14): instance B
// carries peer A as a ctx row; A serves B's consumer with a concurrency limit
// L1 and refuses every request beyond it with 429. B's dream arm floods the
// row, one human request follows. With slots on the row, B's dispatcher
// orders the two before A's door — the human request takes the first freed
// slot and A never sees more than L1 at once. With limits {} the row is a
// pass-through: the flood fills L1 and the human request meets A's 429. This
// is an inventory proof of the base code, not a gate of the F-W7 change: the
// order exists as soon as slots is set (the data variant limits {} is the
// red one).
const (
	// orderProbeL1 is A's concurrency limit for B's consumer and the slots
	// value of the covered variant (the briefing's L1 = 4, slots: 4).
	orderProbeL1 = 4
	// orderProbeDreams is the size of B's background flood.
	orderProbeDreams = 50
	// orderProbeHandoff is the dispatch gates' "at once" (Index Q19): the
	// freed slot must reach the waiting interactive acquire within it.
	orderProbeHandoff = 50 * time.Millisecond
	// orderProbeDeadline bounds every wait of the probe so a broken order
	// fails instead of hanging.
	orderProbeDeadline = 10 * time.Second
	// orderProbeClassHeader tells fake A which side of B sent a request.
	orderProbeClassHeader = "X-Order-Probe-Class"
)

// orderProbePrincipalKey carries the synthetic interactive principal: the
// interactive class is ctx-bound (B8), so the probe installs the hook.
type orderProbePrincipalKey struct{}

// orderProbeObs is what one probe run observed, on B and at fake A.
type orderProbeObs struct {
	enforcing bool
	// interactive acquire on B and its request at A.
	interactiveErr    error
	interactiveClass  dispatch.Class
	interactiveStatus int
	interactiveWait   time.Duration
	// interactiveSeq is the position of the interactive request among the
	// requests A admitted, -1 when A never admitted it.
	interactiveSeq int
	// handoff is first dream release → interactive admission on B; firstLease
	// the duration of that released dream lease.
	handoff    time.Duration
	firstLease time.Duration
	// peak is the highest number of concurrent requests A saw, refused ones
	// included; refused counts A's 429 per class.
	peak    int
	refused map[string]int
	// dream outcomes on B.
	dreamErrs   int
	dreamStatus map[int]int
	// goroutines before and after the run.
	goroutinesBefore, goroutinesAfter int
}

func (o orderProbeObs) String() string {
	return fmt.Sprintf("enforcing=%v interactive{err=%v class=%v status=%d wait=%v seq=%d} handoff=%v firstLease=%v "+
		"peerA{peak=%d refused=%v} dreams{errs=%d status=%v} goroutines{before=%d after=%d}",
		o.enforcing, o.interactiveErr, o.interactiveClass, o.interactiveStatus, o.interactiveWait, o.interactiveSeq,
		o.handoff, o.firstLease, o.peak, o.refused, o.dreamErrs, o.dreamStatus, o.goroutinesBefore, o.goroutinesAfter)
}

// fakePeerA is A's door for one consumer: at most orderProbeL1 concurrent
// requests, the rest 429 at once. An admitted request is held until the
// probe steps it (one token) or drains all of them.
type fakePeerA struct {
	mu       sync.Mutex
	inflight int
	peak     int
	admitted []string
	refused  map[string]int

	step      chan struct{}
	release   chan struct{}
	drainOnce sync.Once
}

func newFakePeerA() *fakePeerA {
	return &fakePeerA{refused: map[string]int{}, step: make(chan struct{}, 1), release: make(chan struct{})}
}

func (a *fakePeerA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	class := r.Header.Get(orderProbeClassHeader)
	a.mu.Lock()
	a.inflight++
	if a.inflight > a.peak {
		a.peak = a.inflight
	}
	if a.inflight > orderProbeL1 {
		a.inflight--
		a.refused[class]++
		a.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	a.admitted = append(a.admitted, class)
	a.mu.Unlock()
	select {
	case <-a.step:
	case <-a.release:
	case <-r.Context().Done():
	}
	a.mu.Lock()
	a.inflight--
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (a *fakePeerA) drain() { a.drainOnce.Do(func() { close(a.release) }) }

// view reads A's counters: admitted requests, refused total, refused of one
// class, and the position of the first admitted request of that class.
func (a *fakePeerA) view(class string) (admitted, refusedTotal, refusedClass, seq int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	seq = -1
	for i, c := range a.admitted {
		if c == class {
			seq = i
			break
		}
	}
	for _, n := range a.refused {
		refusedTotal += n
	}
	return len(a.admitted), refusedTotal, a.refused[class], seq
}

// orderProbeWait polls cond until it holds or the probe deadline passes.
func orderProbeWait(cond func() bool) bool {
	deadline := time.Now().Add(orderProbeDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// targetWaiting reads the waiting counts of one origin on B (0 when the
// dispatcher keeps no state for it).
func targetWaiting(d *dispatch.Dispatcher, origin string) (held, background, interactive int) {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == origin {
			return ts.Held, ts.Background.Waiting, ts.Interactive.Waiting
		}
	}
	return 0, 0, 0
}

// runOrderProbe runs one flood + one interactive request from B against fake
// A through a ctx row with the given limits, projected and derived as at
// boot (DispatchBackendRows → DerivePolicy → UpdatePolicy). herald raises the
// interactive demand for the request's lifetime, as the HTTP ingress does
// (handler.WithScheduler); without it the rank order alone has to act.
func runOrderProbe(t *testing.T, limits map[string]any, herald bool) orderProbeObs {
	t.Helper()
	obs := orderProbeObs{goroutinesBefore: runtime.NumGoroutine(), interactiveSeq: -1, dreamStatus: map[int]int{}}

	a := newFakePeerA()
	srv := httptest.NewServer(a)
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	logger := slog.New(slog.DiscardHandler)
	set := dispatch.DefaultSettings()
	set.BackgroundQueueMax = orderProbeDreams // the whole flood queues; the cap is not under test
	d := dispatch.New(logger, set)
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		a.drain()
		srv.Close()
		tr.CloseIdleConnections()
		d.Close()
	}
	t.Cleanup(stop)

	origin, err := dispatch.NormalizeOrigin(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	d.UpdatePolicy(dispatch.DerivePolicy(DispatchBackendRows([]backends.Backend{{
		Name: "peer-a", Scope: dispatch.GlobalScope, Host: srv.URL,
		ProviderClass: backends.ProviderCtx, Locality: backends.LocalityExternal,
		Roles: []string{"chat", "dream"}, Limits: limits, Enabled: true,
	}}), logger))
	obs.enforcing = d.Enforcing()

	send := func(ctx context.Context, class string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", http.NoBody)
		if err != nil {
			return 0, err
		}
		req.Header.Set(orderProbeClassHeader, class)
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}

	// B's background flood: every dream acquires, calls A, releases.
	var (
		wg           sync.WaitGroup
		dreamMu      sync.Mutex
		firstRelease atomic.Int64
		firstLease   atomic.Int64
	)
	for range orderProbeDreams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, runCtx, err := d.Acquire(context.Background(), dispatch.Request{
				Target: dispatch.Target{Origin: origin}, Class: dispatch.ClassBackground, Role: "dream",
			})
			if err != nil {
				dreamMu.Lock()
				obs.dreamErrs++
				dreamMu.Unlock()
				return
			}
			admitted := time.Now()
			status, err := send(runCtx, "dream")
			released := time.Now()
			if firstRelease.CompareAndSwap(0, released.UnixNano()) {
				firstLease.Store(int64(released.Sub(admitted)))
			}
			lease.Release()
			dreamMu.Lock()
			if err != nil {
				obs.dreamErrs++
			} else {
				obs.dreamStatus[status]++
			}
			dreamMu.Unlock()
		}()
	}
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()

	// The flood settles: L1 requests held at A, the rest queued on B (slots)
	// or refused by A (pass-through).
	if !orderProbeWait(func() bool {
		admitted, refused, _, _ := a.view("dream")
		_, bgWaiting, _ := targetWaiting(d, origin)
		return admitted == orderProbeL1 && refused+bgWaiting == orderProbeDreams-orderProbeL1
	}) {
		stop()
		admitted, refused, _, _ := a.view("dream")
		held, bg, _ := targetWaiting(d, origin)
		t.Fatalf("flood did not settle: A admitted %d refused %d, B held %d background waiting %d", admitted, refused, held, bg)
	}

	// One human request with an authenticated principal; the herald rises
	// for its lifetime when the run models the HTTP ingress.
	type interactiveResult struct {
		err      error
		class    dispatch.Class
		status   int
		wait     time.Duration
		admitted time.Time
	}
	ich := make(chan interactiveResult, 1)
	heraldDone := func() {}
	if herald {
		heraldDone = d.InteractiveArrived()
	}
	ictx := context.WithValue(context.Background(), orderProbePrincipalKey{}, dispatch.Principal{
		ApiKeyID: "key-fw7", TenantID: "tenant-fw7", HomeScope: "scope-fw7",
	})
	go func() {
		defer heraldDone()
		lease, runCtx, err := d.Acquire(ictx, dispatch.Request{
			Target: dispatch.Target{Origin: origin}, Class: dispatch.ClassInteractive, Role: "chat",
		})
		if err != nil {
			ich <- interactiveResult{err: err}
			return
		}
		res := interactiveResult{class: lease.Class(), admitted: time.Now(), wait: lease.WaitDur()}
		res.status, res.err = send(runCtx, "interactive")
		lease.Release()
		ich <- res
	}()

	// The interactive request is either queued on B or already answered by A.
	if !orderProbeWait(func() bool {
		_, _, iWaiting := targetWaiting(d, origin)
		_, _, refused, seq := a.view("interactive")
		return iWaiting == 1 || refused > 0 || seq >= 0
	}) {
		stop()
		t.Fatal("interactive acquire neither queued on B nor reached A")
	}

	// One dream lease ends: its slot is the first one B hands out after the
	// interactive request arrived. No further lease ends before the
	// interactive request reaches A — a slot given to a dream instead stays
	// held, and the wait runs into its deadline (the verdict names it: the
	// handoff and the sequence number then show the broken order).
	a.step <- struct{}{}
	orderProbeWait(func() bool {
		_, _, refused, seq := a.view("interactive")
		return seq >= 0 || refused > 0
	})

	// Drain: every held request at A answers, B serves the rest of its queue.
	a.drain()
	var ires interactiveResult
	select {
	case ires = <-ich:
	case <-time.After(orderProbeDeadline):
		stop()
		t.Fatal("interactive request did not finish after the drain")
	}
	select {
	case <-wgDone:
	case <-time.After(orderProbeDeadline):
		stop()
		t.Fatal("dream flood did not finish after the drain")
	}

	_, _, _, obs.interactiveSeq = a.view("interactive")
	a.mu.Lock()
	obs.peak = a.peak
	obs.refused = map[string]int{}
	for k, v := range a.refused {
		obs.refused[k] = v
	}
	a.mu.Unlock()
	obs.interactiveErr, obs.interactiveClass, obs.interactiveStatus, obs.interactiveWait = ires.err, ires.class, ires.status, ires.wait
	if fr := firstRelease.Load(); fr != 0 && !ires.admitted.IsZero() {
		obs.handoff = ires.admitted.Sub(time.Unix(0, fr))
	}
	obs.firstLease = time.Duration(firstLease.Load())

	stop()
	orderProbeWait(func() bool { return runtime.NumGoroutine() <= obs.goroutinesBefore })
	obs.goroutinesAfter = runtime.NumGoroutine()
	return obs
}

// TestCtxRowOrderHoldsBeforePeerL1 is the F-W7 order probe (G3). The covered
// variants pin B's order before A's door — with the herald of the HTTP
// ingress, and with the rank order alone; the pass-through variant pins that
// the probe has teeth — fake A's L1 refuses the human request when B's order
// does not act.
func TestCtxRowOrderHoldsBeforePeerL1(t *testing.T) {
	dispatch.SetPrincipalHook(func(ctx context.Context) dispatch.Principal {
		p, _ := ctx.Value(orderProbePrincipalKey{}).(dispatch.Principal)
		return p
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })

	for _, herald := range []bool{true, false} {
		t.Run(fmt.Sprintf("slots on the ctx row, herald=%v: B orders before A's door", herald), func(t *testing.T) {
			assertOrderHolds(t, runOrderProbe(t, map[string]any{"slots": float64(orderProbeL1)}, herald))
		})
	}

	t.Run("limits {} on the ctx row: the flood meets A's L1 first", func(t *testing.T) {
		obs := runOrderProbe(t, map[string]any{}, true)
		t.Logf("observed: %v", obs)
		if obs.interactiveErr != nil {
			t.Fatalf("interactive request: %v", obs.interactiveErr)
		}
		if obs.interactiveStatus != http.StatusTooManyRequests || obs.refused["interactive"] != 1 {
			t.Errorf("interactive request answered %d (A refused %v), want A's 429 — the probe lost its teeth", obs.interactiveStatus, obs.refused)
		}
		if obs.peak <= orderProbeL1 {
			t.Errorf("A saw at most %d concurrent requests on a pass-through row, want > %d", obs.peak, orderProbeL1)
		}
		if obs.goroutinesAfter > obs.goroutinesBefore {
			t.Errorf("goroutines %d before, %d after the probe", obs.goroutinesBefore, obs.goroutinesAfter)
		}
	})
}

// assertOrderHolds is the verdict of a covered run: the human request takes
// the first slot freed after it arrived, at once, and A never refuses.
func assertOrderHolds(t *testing.T, obs orderProbeObs) {
	t.Helper()
	t.Logf("observed: %v", obs)
	if !obs.enforcing {
		t.Error("Enforcing() = false with slots on the ctx row, want true")
	}
	if obs.interactiveErr != nil || obs.interactiveClass != dispatch.ClassInteractive {
		t.Fatalf("interactive request: err=%v class=%v, want an interactive lease", obs.interactiveErr, obs.interactiveClass)
	}
	if obs.interactiveStatus != http.StatusOK {
		t.Errorf("interactive request answered %d at A, want 200", obs.interactiveStatus)
	}
	if obs.interactiveSeq != orderProbeL1 {
		t.Errorf("interactive request was A's admitted request no. %d, want %d (the first slot freed after it arrived)", obs.interactiveSeq, orderProbeL1)
	}
	if obs.handoff < 0 || obs.handoff > orderProbeHandoff {
		t.Errorf("first dream release → interactive admission took %v, want ≤ %v", obs.handoff, orderProbeHandoff)
	}
	if obs.interactiveWait > obs.firstLease+orderProbeHandoff {
		t.Errorf("interactive wait %v exceeds one lease duration %v", obs.interactiveWait, obs.firstLease)
	}
	if obs.peak > orderProbeL1 {
		t.Errorf("A saw %d concurrent requests, want ≤ %d", obs.peak, orderProbeL1)
	}
	if len(obs.refused) != 0 {
		t.Errorf("A refused %v, want no 429 at all", obs.refused)
	}
	if obs.dreamErrs != 0 || obs.dreamStatus[http.StatusOK] != orderProbeDreams {
		t.Errorf("dream flood: %d errors, statuses %v, want all %d answered 200", obs.dreamErrs, obs.dreamStatus, orderProbeDreams)
	}
	if obs.goroutinesAfter > obs.goroutinesBefore {
		t.Errorf("goroutines %d before, %d after the probe", obs.goroutinesBefore, obs.goroutinesAfter)
	}
}
