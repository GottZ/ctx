package dispatch

// A03-W17 gates (masterplan K21, design/02 §9.2 A03-9): RetryAfterHint is on
// the /v1 hot path (every 429 of an external request asks for it), so it no
// longer sorts the ≤ 512 wait window under the dispatcher mutex. The wait
// rings publish p95/max/samples per target × class at most once per
// waitPublishInterval, computed OUTSIDE the mutex (copy under it, sort
// outside, atomic store); the hint reads only the queue depth and the
// published value — no sort, no allocation. Snapshot keeps computing exactly
// (the server-admin pull).

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// warmRing fills origin's window of class c with n samples of wait through
// the seeding hook (which publishes, like a tick would).
func warmRing(d *Dispatcher, origin string, c Class, n int, wait time.Duration) {
	for i := 0; i < n; i++ {
		d.SeedWaitSampleForTest(origin, c, wait)
	}
}

// TestRetryAfterHintAllocFree: on a warm 512-sample window the hint performs
// no allocation — neither for the canonical origin nor for the raw spelling
// the rejection mapping passes (b.Host, here with case drift and a /v1
// suffix), in every class.
func TestRetryAfterHintAllocFree(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	for _, c := range []Class{ClassInteractive, ClassBackground, ClassExternal} {
		warmRing(d, testOrigin, c, waitRingK, 2*time.Second)
	}
	d.SeedInteractiveDepthForTest(testOrigin, 3)
	for _, origin := range []string{testOrigin, "http://GPU:8089/v1"} {
		for _, c := range []Class{ClassInteractive, ClassBackground, ClassExternal} {
			var est time.Duration
			allocs := testing.AllocsPerRun(1000, func() { est = d.RetryAfterHint(origin, c) })
			if allocs != 0 {
				t.Errorf("RetryAfterHint(%q, %s): %v allocations per call, want 0 (no window copy, no sort, no origin parse)", origin, c, allocs)
			}
			if c == ClassInteractive && est != 6*time.Second {
				t.Errorf("RetryAfterHint(%q, interactive) = %v, want 6s (depth 3 × published p95 2s)", origin, est)
			}
		}
	}
}

// TestRetryAfterHintPublishedWithinInterval is the staleness gate: after 600
// new samples the hint reflects the new p95 within waitPublishInterval PLUS
// a scheduling allowance of 250 ms — the gate's bound is 1 s + 250 ms, not
// 1 s (A03-W17 review 2 F-1, disclosed): in this setup the ticker phase lies
// practically at the interval (measured 990–1 000 ms in 10/10 runs), so a hard
// 1-s bound would be flaky under -race and host load, and the published value
// is documented as at most one interval "plus the pass itself" old
// (waitPublishInterval). It reads the PUBLISHED value, not the live window.
func TestRetryAfterHintPublishedWithinInterval(t *testing.T) {
	const schedulingAllowance = 250 * time.Millisecond
	// The bound itself is the design figure (design/02 §9.2 A03-9: at most
	// once per second) — pinned, so a wider interval cannot pass by moving
	// the bound along (review F2).
	if waitPublishInterval != time.Second {
		t.Fatalf("waitPublishInterval = %v, want 1s", waitPublishInterval)
	}
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(2))
	warmRing(d, testOrigin, ClassInteractive, waitRingK, time.Second)
	d.SeedInteractiveDepthForTest(testOrigin, 3)
	if got := d.RetryAfterHint(testOrigin, ClassInteractive); got != 3*time.Second {
		t.Fatalf("warm hint = %v, want 3s (depth 3 × p95 1s)", got)
	}
	d.mu.Lock()
	r := d.targets[testOrigin].ring(ClassInteractive)
	for i := 0; i < 600; i++ {
		r.add(5 * time.Second) // live samples, no publication
	}
	d.mu.Unlock()
	added := time.Now()
	for {
		got := d.RetryAfterHint(testOrigin, ClassInteractive)
		if got == 15*time.Second {
			break
		}
		if elapsed := time.Since(added); elapsed > waitPublishInterval+schedulingAllowance {
			t.Fatalf("hint still %v %v after 600 new samples, want 15s (depth 3 × p95 5s) within %v + %v", got, elapsed, waitPublishInterval, schedulingAllowance)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("new p95 published %v after the samples (bound %v + %v scheduling allowance)", time.Since(added), waitPublishInterval, schedulingAllowance)
	if ws := target(t, d).Interactive; ws.P95Wait != 5*time.Second || ws.Samples != waitRingK {
		t.Fatalf("snapshot p95 %v samples %d, want the exact 5s / %d", ws.P95Wait, ws.Samples, waitRingK)
	}
}

// TestRetryAfterHintSectionIndependentOfWindow pins "no sort under the
// mutex" with a statistic a descheduled goroutine cannot fake: the median
// hint call on a full 512-sample window costs about what it costs on a
// one-sample window. Sorting (or copying) the window per call — the pre-A03-W17
// statsInto path — makes the full-window median a multiple of the other.
func TestRetryAfterHintSectionIndependentOfWindow(t *testing.T) {
	const calls, maxRatio = 2001, 3
	full, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(2))
	one, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(2))
	for i := 0; i < waitRingK; i++ {
		full.SeedWaitSampleForTest(testOrigin, ClassInteractive, time.Duration(i)*time.Millisecond)
	}
	one.SeedWaitSampleForTest(testOrigin, ClassInteractive, time.Second)
	// Interleaved calls, so a load burst on the host hits both windows alike.
	medians := func() (time.Duration, time.Duration) {
		fullDurs, oneDurs := make([]time.Duration, calls), make([]time.Duration, calls)
		for i := range calls {
			start := time.Now()
			full.RetryAfterHint(testOrigin, ClassInteractive)
			fullDurs[i] = time.Since(start)
			start = time.Now()
			one.RetryAfterHint(testOrigin, ClassInteractive)
			oneDurs[i] = time.Since(start)
		}
		slices.Sort(fullDurs)
		slices.Sort(oneDurs)
		return fullDurs[calls/2], oneDurs[calls/2]
	}
	medians() // warm the spelling caches and code paths
	mFull, mOne := medians()
	t.Logf("median hint: full window %v, one sample %v", mFull, mOne)
	if mFull > maxRatio*mOne+time.Microsecond {
		t.Fatalf("median hint on a full window %v vs %v on one sample — over %d× (+1 µs): the hint still does O(window) work", mFull, mOne, maxRatio)
	}
}

// BenchmarkRetryAfterHintStormMaxOpDur is design/03 §7 A03-W6 gate (c), taken
// over here (masterplan K21 assigns the p95 cache to A03-W17): 32 goroutines
// storm the hint (≈ 10 000 calls) while 32 others acquire and release on the
// same target; it reports the longest measured dispatcher mutex section —
// since A03-W17 the hint's own section is measured too (Snapshot.MaxOpDur).
// It uses only the stock API, so the same measurement runs on the base tree
// (there with the hint section measured by an overlay). Like
// BenchmarkRankScanMaxOpDur it is a distribution against that baseline under
// the same host load, not a single-run pass/fail: one descheduled mutex
// holder on a saturated host lands in the maximum regardless of the code
// (measured on the A03-W17 tree: the > 1 ms maxima sit in unchanged
// Acquire/Release sections, with and without -race, GC on or off). Record
// the load (uptime):
//
//	go test -run '^$' -bench RetryAfterHintStormMaxOpDur -benchtime 1x -count 20 ./internal/dispatch/
func BenchmarkRetryAfterHintStormMaxOpDur(b *testing.B) {
	for i := 0; i < b.N; i++ {
		snap := hintStormRun(b)
		b.ReportMetric(float64(snap.MaxOpDur)/float64(time.Microsecond), "maxop-us")
		b.ReportMetric(float64(snap.OpsTotal), "ops")
	}
}

// hintStormRun drives one storm and returns the final snapshot.
func hintStormRun(b *testing.B) Snapshot {
	b.Helper()
	d := New(slog.New(slog.DiscardHandler), DefaultSettings())
	defer d.Close()
	d.UpdatePolicy(partitionPolicy(64, 32))
	for _, c := range []Class{ClassInteractive, ClassExternal} {
		warmRing(d, testOrigin, c, waitRingK, time.Second)
	}
	const hinters, workers, hintCalls, cycles = 32, 32, 10000, 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < hinters; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			c := ClassInteractive
			if g%2 == 1 {
				c = ClassExternal
			}
			for i := 0; i < hintCalls/hinters; i++ {
				d.RetryAfterHint(testOrigin, c)
			}
		}(g)
	}
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			ctx, req := ictx(principal("w")), interactiveReq()
			if g%2 == 1 {
				ctx, req = xctx(context.Background(), "k", "tenant-a"), externalReq()
			}
			for i := 0; i < cycles; i++ {
				l, _, err := d.Acquire(ctx, req)
				if err != nil {
					b.Errorf("acquire under the hint storm: %v", err)
					return
				}
				l.Release()
			}
		}(g)
	}
	close(start)
	wg.Wait()
	return d.Snapshot()
}

// loopGoroutines counts the goroutines running a dispatcher loop (reaper or
// wait publisher) — a goroutine count that unrelated goroutines of earlier
// tests, finishing asynchronously, cannot disturb.
func loopGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	return bytes.Count(buf, []byte("dispatch.(*Dispatcher).reapLoop(")) +
		bytes.Count(buf, []byte("dispatch.(*Dispatcher).publishLoop("))
}

// TestWaitPublisherStopsOnClose: New starts the reaper and the wait
// publisher, Close ends both — the dispatcher loops and the total goroutine
// count return to their values before New, no ticker leaks. The counts are
// polled for a short settle in both directions: a goroutine that has not run
// yet is not yet visible as its loop (measured: 567 of 2 000 rounds right
// after New under -race), and Close returns once each loop has signalled its
// exit (WaitGroup.Done runs in the loop's deferred path), so under -race the
// goroutine can still be on its way out for a moment (review F1: 39 of 2 000
// rounds with an immediate count, 0 after the settle). A loop that never
// stops stays counted.
func TestWaitPublisherStopsOnClose(t *testing.T) {
	const settle = time.Second
	l0, g0 := loopGoroutines(), runtime.NumGoroutine()
	d := New(nil, DefaultSettings())
	d.UpdatePolicy(onSlotPolicy(2))
	if l, _, err := d.Acquire(ictx(principal("a")), interactiveReq()); err != nil {
		t.Fatalf("acquire: %v", err)
	} else {
		l.Release()
	}
	// A goroutine that has not run yet shows in the stack dump as its go
	// statement's wrapper, not as the loop — wait for both loops to start.
	deadline := time.Now().Add(settle)
	for loopGoroutines() != l0+2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l := loopGoroutines(); l != l0+2 {
		t.Fatalf("dispatcher loops with a live dispatcher %d, want %d + 2 (reaper and wait publisher)", l, l0)
	}
	d.Close()
	deadline = time.Now().Add(settle)
	for loopGoroutines() != l0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l := loopGoroutines(); l != l0 {
		t.Fatalf("dispatcher loops after Close %d, want %d (before New) — a loop outlived Close", l, l0)
	}
	for runtime.NumGoroutine() > g0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > g0 {
		t.Fatalf("goroutines after Close %d, want <= %d (before New)", g, g0)
	}
	d.Close() // idempotent
}
