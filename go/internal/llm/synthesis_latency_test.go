package llm

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// ringStart is a fixed process start for the ring tests (sub-second part on
// purpose: since is RFC3339 without a fraction).
var ringStart = time.Date(2026, 11, 8, 19, 0, 0, 750_000_000, time.UTC)

func ms(n int64) time.Duration { return time.Duration(n) * time.Millisecond }

func i64(p *int64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

// TestDurationRingContract pins the index constants of FOLGE-Y (§1/§3 of
// FOLGE-UY-INDEX): 512 durations in the ring, p95 from 20 samples on — the
// figure C-PY reads query_synthesize.p95_ms against (samples >= 20).
func TestDurationRingContract(t *testing.T) {
	if synthesisLatencyCap != 512 || synthesisLatencyMinSamples != 20 {
		t.Errorf("FOLGE-Y gate (contract): cap %d, p95 floor %d — want 512 and 20 (FOLGE-UY-INDEX §3)", synthesisLatencyCap, synthesisLatencyMinSamples)
	}
	querySynthesizeLatency.mu.Lock()
	n := len(querySynthesizeLatency.ms)
	querySynthesizeLatency.mu.Unlock()
	if n != synthesisLatencyCap {
		t.Errorf("FOLGE-Y gate (contract): the process-wide ring holds %d durations, want %d", n, synthesisLatencyCap)
	}
}

// TestDurationRingEmpty: no sample — samples 0, p95_ms and avg_ms null
// (null ≠ 0: "not measured" must not read as "0 ms"), since = the start,
// whole seconds in UTC.
func TestDurationRingEmpty(t *testing.T) {
	got := newDurationRing(synthesisLatencyCap, ringStart).snapshot()
	if got.Samples != 0 || got.P95Ms != nil || got.AvgMs != nil {
		t.Errorf("FOLGE-Y gate (empty): samples %d p95 %s avg %s, want 0/null/null", got.Samples, i64(got.P95Ms), i64(got.AvgMs))
	}
	if want := ringStart.Truncate(time.Second); !got.Since.Equal(want) || got.Since.Location() != time.UTC {
		t.Errorf("FOLGE-Y gate (since): since %v, want the start %v in UTC, whole seconds", got.Since, want)
	}
}

// TestDurationRingP95Floor: below 20 samples p95_ms is null while avg_ms is
// already there; the 20th sample turns p95 on.
func TestDurationRingP95Floor(t *testing.T) {
	r := newDurationRing(synthesisLatencyCap, ringStart)
	for i := int64(1); i < synthesisLatencyMinSamples; i++ {
		r.observe(ms(i), ringStart.Add(time.Duration(i)*time.Second))
	}
	got := r.snapshot()
	if got.Samples != 19 || got.P95Ms != nil || got.AvgMs == nil || *got.AvgMs != 10 {
		t.Errorf("FOLGE-Y gate (p95 floor): 19 samples 1..19 ms → samples %d p95 %s avg %s, want 19/null/10", got.Samples, i64(got.P95Ms), i64(got.AvgMs))
	}
	r.observe(ms(20), ringStart.Add(20*time.Second))
	got = r.snapshot()
	// nearest rank: ceil(0.95 × 20) = 19 → the 19th smallest = 19 ms.
	if got.Samples != 20 || got.P95Ms == nil || *got.P95Ms != 19 {
		t.Errorf("FOLGE-Y gate (p95 floor): 20 samples 1..20 ms → samples %d p95 %s, want 20/19", got.Samples, i64(got.P95Ms))
	}
}

// TestDurationRingPercentile: 100 samples 1..100 ms in random order — p95
// is the nearest rank (95), avg rounds half up (50.5 → 51), the order of
// arrival does not matter.
func TestDurationRingPercentile(t *testing.T) {
	r := newDurationRing(synthesisLatencyCap, ringStart)
	rng := rand.New(rand.NewPCG(1, 2))
	for _, i := range rng.Perm(100) {
		r.observe(ms(int64(i+1)), ringStart.Add(time.Second))
	}
	got := r.snapshot()
	if got.Samples != 100 || i64(got.P95Ms) != "95" || i64(got.AvgMs) != "51" {
		t.Errorf("FOLGE-Y gate (percentile): samples %d p95 %s avg %s, want 100/95/51", got.Samples, i64(got.P95Ms), i64(got.AvgMs))
	}
	// n = 30: 0.95·30 = 28.5 — the nearest rank rounds UP to the 29th
	// smallest (Fremd-Review F4: at n = 20 and 100 ceil and floor agree).
	r30 := newDurationRing(synthesisLatencyCap, ringStart)
	for i := int64(1); i <= 30; i++ {
		r30.observe(ms(i), ringStart)
	}
	if got := r30.snapshot(); i64(got.P95Ms) != "29" {
		t.Errorf("FOLGE-Y gate (percentile): 30 samples 1..30 ms → p95 %s, want 29 (rank ceil(28.5) = 29)", i64(got.P95Ms))
	}
	// Sub-millisecond parts round to the nearest millisecond; a negative
	// duration (no monotonic clock produces one) counts as 0.
	q := newDurationRing(synthesisLatencyCap, ringStart)
	q.observe(1499*time.Microsecond, ringStart)
	q.observe(1500*time.Microsecond, ringStart)
	q.observe(-ms(5), ringStart)
	got = q.snapshot()
	if got.Samples != 3 || i64(got.AvgMs) != "1" {
		t.Errorf("FOLGE-Y gate (rounding): 1.499 ms, 1.5 ms, −5 ms → samples %d avg %s, want 3 and avg (1+2+0)/3 = 1", got.Samples, i64(got.AvgMs))
	}
}

// TestDurationRingWrap: the ring keeps the last 512 durations — 88 slow
// samples (10 s) followed by 512 fast ones leave none of the slow ones in
// p95/avg, samples stays at 512, and since moves to the oldest retained
// sample (the window the figures describe).
func TestDurationRingWrap(t *testing.T) {
	r := newDurationRing(synthesisLatencyCap, ringStart)
	at := func(i int) time.Time { return ringStart.Add(time.Duration(i) * time.Second) }
	full := newDurationRing(synthesisLatencyCap, ringStart)
	for i := range synthesisLatencyCap {
		full.observe(ms(1), at(i+1))
	}
	if got, want := full.snapshot(), ringStart.Truncate(time.Second); got.Samples != synthesisLatencyCap || !got.Since.Equal(want) {
		t.Errorf("FOLGE-Y gate (wrap since): exactly %d samples → samples %d since %v, want %d and the start %v (nothing dropped yet)",
			synthesisLatencyCap, got.Samples, got.Since, synthesisLatencyCap, want)
	}
	const slow = 88
	for i := range slow {
		r.observe(ms(10_000), at(i))
	}
	for i := range synthesisLatencyCap {
		r.observe(ms(int64(i%100)+1), at(slow+i))
	}
	got := r.snapshot()
	if got.Samples != synthesisLatencyCap {
		t.Errorf("FOLGE-Y gate (wrap): samples %d after %d observations, want %d", got.Samples, slow+synthesisLatencyCap, synthesisLatencyCap)
	}
	if got.P95Ms == nil || *got.P95Ms > 100 || got.AvgMs == nil || *got.AvgMs > 100 {
		t.Errorf("FOLGE-Y gate (wrap): p95 %s avg %s — a 10-s sample older than the last 512 is still counted", i64(got.P95Ms), i64(got.AvgMs))
	}
	if want := at(slow).Truncate(time.Second); !got.Since.Equal(want) {
		t.Errorf("FOLGE-Y gate (wrap since): since %v, want the oldest retained sample's time %v", got.Since, want)
	}
	// One more lap moves since by exactly one observation.
	r.observe(ms(1), at(slow+synthesisLatencyCap))
	if got, want := r.snapshot(), at(slow+1).Truncate(time.Second); !got.Since.Equal(want) {
		t.Errorf("FOLGE-Y gate (wrap since): after one more sample since %v, want %v", got.Since, want)
	}
}

// TestDurationRingConcurrent: writers and readers at once (run with -race):
// every observation lands, none twice.
func TestDurationRingConcurrent(t *testing.T) {
	r := newDurationRing(synthesisLatencyCap, ringStart)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 40 {
				r.observe(ms(int64(w*40+i)), ringStart)
				_ = r.snapshot()
			}
		})
	}
	wg.Wait()
	if got := r.snapshot(); got.Samples != 320 || i64(got.AvgMs) != "160" {
		t.Errorf("FOLGE-Y gate (concurrency): samples %d avg %s after 8×40 observations of 0..319 ms, want 320/160", got.Samples, i64(got.AvgMs))
	}
}

// ringCount and ringNewest read the process-wide ring for the Messpunkt
// test (no test in this package runs in parallel: a delta is exact).
func ringCount() uint64 {
	querySynthesizeLatency.mu.Lock()
	defer querySynthesizeLatency.mu.Unlock()
	return querySynthesizeLatency.n
}

func ringNewest() int64 {
	r := querySynthesizeLatency
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return -1
	}
	return r.ms[(r.n-1)%uint64(len(r.ms))]
}

// slowOllama answers after delay with status (200: an ollama chat answer).
func slowOllama(t *testing.T, delay time.Duration, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"An answer."},"eval_count":1,"prompt_eval_count":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSynthesizeObservesLatency is the Messpunkt pin of FOLGE-Y: exactly
// one documented place feeds query_synthesize — Synthesize, after the
// row-defining attempt is known (applyDispatchTelemetry, MW10), with that
// attempt's wait-free wire duration; the same population llm_24h averages
// for pipeline query-synthesize (every call that reached a backend, a
// failed one too). A call that never reached a backend records nothing.
func TestSynthesizeObservesLatency(t *testing.T) {
	sources := []Source{{ID: "1", Title: "t", Category: "c", Content: "body", Score: 0.5, AgeDays: 1, Sensitivity: backends.SensPersonal}}
	run := func(t *testing.T, host string) error {
		t.Helper()
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{gateBackend("lat", host, backends.TrustFull, 1)})
		_, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, gateSettings(), backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
		return err
	}
	const delay = 40 * time.Millisecond

	t.Run("answered", func(t *testing.T) {
		before := ringCount()
		if err := run(t, slowOllama(t, delay, http.StatusOK).URL); err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
		if d := ringCount() - before; d != 1 {
			t.Fatalf("FOLGE-Y gate (Messpunkt): one synthesis recorded %d durations, want exactly 1", d)
		}
		if got := ringNewest(); got < delay.Milliseconds() || got > 30_000 {
			t.Errorf("FOLGE-Y gate (Messpunkt): recorded %d ms for a backend that answered after %v, want the wire duration (≥ %d ms)", got, delay, delay.Milliseconds())
		}
	})
	t.Run("failed_on_the_wire", func(t *testing.T) {
		before := ringCount()
		if err := run(t, slowOllama(t, delay, http.StatusInternalServerError).URL); err == nil {
			t.Fatal("Synthesize: want the 500 as an error")
		}
		if d := ringCount() - before; d != 1 {
			t.Errorf("FOLGE-Y gate (Messpunkt): a call that reached the backend and failed recorded %d durations, want 1 (llm_24h counts it too)", d)
		}
	})
	t.Run("model_less_row", func(t *testing.T) {
		// A row without a model for the role (psql-edited past validation)
		// is a no_model pseudo-attempt: a log row, but no wire contact —
		// nothing to time.
		b := gateBackend("nomodel", slowOllama(t, 0, http.StatusOK).URL, backends.TrustFull, 1)
		b.Model = ""
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{b})
		before := ringCount()
		if _, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, gateSettings(), backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive)); err == nil {
			t.Fatal("Synthesize over a model-less row: want an error")
		}
		if d := ringCount() - before; d != 0 {
			t.Errorf("FOLGE-Y gate (Messpunkt): a walk of only no_model pseudo-attempts recorded %d durations, want 0", d)
		}
	})
	t.Run("no_backend", func(t *testing.T) {
		before := ringCount()
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest(nil)
		if _, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, gateSettings(), backends.SensPersonal, "q", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive)); err == nil {
			t.Fatal("Synthesize without a backend: want an error")
		}
		if d := ringCount() - before; d != 0 {
			t.Errorf("FOLGE-Y gate (Messpunkt): a call that never reached a backend recorded %d durations, want 0", d)
		}
	})
}

// BenchmarkDurationRingSnapshot is the cost of one query_synthesize read at
// a full ring (512): what every /api/status call of a dashboard pays.
func BenchmarkDurationRingSnapshot(b *testing.B) {
	r := newDurationRing(synthesisLatencyCap, ringStart)
	for i := range synthesisLatencyCap + 7 {
		r.observe(ms(int64(i*37%5000)), ringStart)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = r.snapshot()
	}
}
