package oai_test

// Measurements of the embeddings route over the real dispatcher (W02-09):
// the named limit "XB continuation under XU load above E" (lead decision on
// the f6c6c189 refresh: measured and pinned, no new mechanism — the
// dispatcher is A03-W8a's zone) and Z6, the multi-block displacement of a
// heavy waiter (A03-W17 review 2 F-2: a measurement, no red gate).

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// xuLoad keeps the external XU class of host above E: n loops, each
// acquiring a w = 1 lease as its own foreign urgent consumer, holding it for
// hold, releasing, acquiring again — so an XU head always waits. stop ends
// the loops and returns once they are gone.
func xuLoad(t *testing.T, d *dispatch.Dispatcher, host string, n int, hold time.Duration) (stop func()) {
	t.Helper()
	quit := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ca := foreignConsumer(urgentApp(), "xu-load-"+itoa(i))
			for {
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), consumerKey{}, ca))
				go func() {
					select {
					case <-quit:
						cancel()
					case <-ctx.Done():
					}
				}()
				l, _, err := d.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: host}, Class: dispatch.ClassExternal, Role: "embed"})
				if err == nil {
					select {
					case <-quit:
					case <-time.After(hold):
					}
					l.Release()
				}
				cancel()
				select {
				case <-quit:
					return
				default:
				}
			}
		}()
	}
	var once sync.Once
	return func() { once.Do(func() { close(quit); wg.Wait() }) }
}

// TestEmbedBulkContinuationUnderUrgentLoad pins the named limit (Stand,
// lead decision on the f6c6c189 refresh): a consumer on the lane bulk (XB)
// with several blocks; after block one, XU demand above E takes every unit
// the moment one comes back (an XU head claims it, rank.go/fairness.go).
// The continuation of block two waits at the head of XB without a wait
// budget and ends only at the request deadline — the planner's writer
// answers 503 internal_error, x-should-retry false (the N-1 class). The
// pinned outcome: that status, no Retry-After, one row with block one's
// usage, no lease held, no goroutine left, the duration ≈ the request
// deadline. A change of this outcome is the decision of A03-W10 / the user
// (E2 "bulk under permanent urgent load"), not of this route.
func TestEmbedBulkContinuationUnderUrgentLoad(t *testing.T) {
	const deadline = 1500 * time.Millisecond
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
	rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = deadline })
	bulk := urgentApp()
	bulk.AdmissionLane = handler.LaneBulk
	rig.ca.Store(bulk)
	host := e.srv.URL
	// Warm the keep-alive connection to the engine: its goroutines (the
	// pool's idle connection) are part of the baseline, not of the request.
	decodeEmbed(t, rig.postEmbed(t, embedBody("emb", []string{"warm-up"})))
	e.hits.Store(0)
	rig.rows = &rowSink{}
	rig.deps.Record = rig.rows.record
	rig.router = rig.mountEmbed(oai.Embeddings(rig.deps))
	runtime.GC()
	before := runtime.NumGoroutine()
	var stopLoad func()
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		if e.hits.Load() == 1 {
			stopLoad = xuLoad(t, rig.disp, host, 2, 30*time.Millisecond)
			waitWaiting(t, rig.disp, host, 1, false) // an XU head waits before block one ends
		}
		return false
	})
	start := time.Now()
	a := rig.postEmbed(t, embedBody("emb", embedInputs(3)))
	took := time.Since(start)
	if stopLoad == nil {
		t.Fatal("the load never started")
	}
	stopLoad()
	code, _, _ := a.envelope(t)
	rows := rig.rows.all()
	t.Logf("XB continuation under XU load: %d %s after %v (request deadline %v); Retry-After %q x-should-retry %q; wire hits %d",
		a.status, code, took.Round(time.Millisecond), deadline, a.header.Get("Retry-After"), a.header.Get("x-should-retry"), e.hits.Load())
	if len(rows) == 1 {
		t.Logf("row: finish %s err %+v sub_batches %v done %v tokens %d class %s",
			rows[0].Gateway.Finish, rows[0].Err, rows[0].Telemetry.Metadata["sub_batches"], rows[0].Telemetry.Metadata["sub_batches_done"],
			rows[0].Telemetry.PromptTokens, rows[0].Telemetry.DispatchClass)
	}
	if a.status == http.StatusOK && len(a.body) == 0 {
		t.Fatal("empty 200 — no defined state (stop finding)")
	}
	if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" || a.header.Get("Retry-After") != "" {
		t.Fatalf("outcome %d %s (retry %q, Retry-After %q) differs from the read limit: 503 internal_error, x-should-retry false, no Retry-After",
			a.status, code, a.header.Get("x-should-retry"), a.header.Get("Retry-After"))
	}
	if took < deadline-50*time.Millisecond || took > deadline+500*time.Millisecond {
		t.Errorf("ended after %v, want ≈ the request deadline %v", took, deadline)
	}
	if e.hits.Load() != 1 {
		t.Errorf("wire hits %d, want 1 (block one)", e.hits.Load())
	}
	if len(rows) != 1 || rows[0].Telemetry.Metadata["sub_batches_done"] != 1 || rows[0].Telemetry.PromptTokens != 2 ||
		rows[0].Telemetry.DispatchClass != dispatch.ClassExternalBulk.String() {
		t.Errorf("rows %+v, want one XB row with block one's 2 tokens", rows)
	}
	reservationsEndedOnce(t, rig.limiter, 2, 0) // the warm-up and the request under test
	noLeaseHeld(t, rig.disp, host)
	deadlineG := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadlineG) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d → %d", before, after)
	}
}

// TestEmbedMultiBlockDisplacesAHeavyWaiter (Z6, a measurement): two
// requests of 8 one-input blocks (w = 1 each, "emb1"), phase-shifted by half
// a block, hold E = 2 between them; a heavy waiter (w = 2, two inputs on "emb") with a wait budget of
// 300 ms queues behind. Every lease boundary hands the freed unit to the
// next continuation at the head — the heavy waiter needs both units at
// once. The measured outcome (wait, ErrWaitExpired as 429 or served) goes to
// the report; the gate only holds the defined states.
func TestEmbedMultiBlockDisplacesAHeavyWaiter(t *testing.T) {
	for _, budget := range []string{"300", "150"} {
		t.Run("wait budget "+budget+" ms", func(t *testing.T) { measureDisplacement(t, budget) })
	}
}

func measureDisplacement(t *testing.T, budget string) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 3, 1, withoutCensus)
	host := e.srv.URL
	var started atomic.Int64
	e.setRespond(func(_ http.ResponseWriter, _ *http.Request, _ int64, inputs []string) bool {
		if inputs[0] != "heavy-0" {
			started.Add(1)
			time.Sleep(30 * time.Millisecond)
		}
		return false
	})
	type res struct {
		a    chatAnswer
		took time.Duration
	}
	run := func(tag string, body string, hdr ...string) chan res {
		ch := make(chan res, 1)
		go func() {
			s := time.Now()
			a := rig.postEmbed(t, body, append([]string{foreignTenantHeader, tag}, hdr...)...)
			ch <- res{a, time.Since(s)}
		}()
		return ch
	}
	inputsA, inputsB := embedInputs(8), embedInputs(8)
	for i := range inputsB {
		inputsB[i] = "b-" + inputsB[i]
	}
	// Phase-shifted by half a block: the two never free their units at the
	// same moment, so a lease boundary frees one unit at a time.
	ra := run("multi-a", embedBody("emb1", inputsA))
	for started.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(15 * time.Millisecond)
	rb := run("multi-b", embedBody("emb1", inputsB))
	for started.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	rw := run("heavy", embedBody("emb", []string{"heavy-0", "heavy-1"}), "Ctx-Max-Wait-Ms", budget)
	a, b, w := <-ra, <-rb, <-rw
	wcode := ""
	if w.a.status != http.StatusOK {
		wcode, _, _ = w.a.envelope(t)
	}
	t.Logf("Z6: multi A %d after %v, multi B %d after %v; heavy waiter (w = 2, wait budget %s ms) %d %s after %v",
		a.a.status, a.took.Round(time.Millisecond), b.a.status, b.took.Round(time.Millisecond), budget, w.a.status, wcode, w.took.Round(time.Millisecond))
	for name, r := range map[string]res{"A": a, "B": b, "heavy": w} {
		if r.a.status == http.StatusOK && len(r.a.body) == 0 {
			t.Errorf("%s: empty 200", name)
		}
	}
	if a.a.status != http.StatusOK || b.a.status != http.StatusOK {
		t.Errorf("multi-block requests: %d %.120s / %d %.120s, want 200/200", a.a.status, a.a.body, b.a.status, b.a.body)
	}
	if w.a.status != http.StatusOK && (w.a.status != http.StatusTooManyRequests || wcode != oai.CodeServerBusy) {
		t.Errorf("heavy waiter %d %s, want 200 or 429 server_busy", w.a.status, wcode)
	}
	noLeaseHeld(t, rig.disp, host)
}
