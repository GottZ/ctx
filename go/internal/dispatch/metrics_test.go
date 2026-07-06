package dispatch

// MW7 probes: per-target×class wait measurands (design/03 §4.6.1, D3-W3
// gates): p95 over exactly the last K samples (constructed distributions,
// sample K+1 evicts sample 1), max_wait rising measurably under blocked
// admission with classes separated, and window honesty (snapshot sample
// count == fed samples).

import (
	"testing"
	"time"
)

// TestWaitRingP95KnownDistributions probes the nearest-rank estimator with
// constructed distributions whose quantiles are known by hand.
func TestWaitRingP95KnownDistributions(t *testing.T) {
	cases := []struct {
		name    string
		samples []time.Duration
		p95     time.Duration
		max     time.Duration
	}{
		{
			name:    "uniform 1..100ms",
			samples: rangeMillis(1, 100),
			p95:     95 * time.Millisecond, // ⌈0.95·100⌉ = 95th smallest
			max:     100 * time.Millisecond,
		},
		{
			name:    "uniform 1..20ms",
			samples: rangeMillis(1, 20),
			p95:     19 * time.Millisecond, // ⌈0.95·20⌉ = 19th smallest
			max:     20 * time.Millisecond,
		},
		{
			name:    "single sample",
			samples: []time.Duration{7 * time.Millisecond},
			p95:     7 * time.Millisecond, // ⌈0.95·1⌉ = 1st
			max:     7 * time.Millisecond,
		},
		{
			name:    "skew: one outlier does not drag p95",
			samples: append(repeatMillis(1, 99), time.Second),
			p95:     time.Millisecond, // 95th smallest of 99×1ms + 1×1s
			max:     time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r waitRing
			for _, s := range tc.samples {
				r.add(s)
			}
			var ws WaitStats
			r.statsInto(&ws)
			if ws.Samples != len(tc.samples) {
				t.Fatalf("samples: got %d, want %d", ws.Samples, len(tc.samples))
			}
			if ws.P95Wait != tc.p95 {
				t.Fatalf("p95: got %v, want %v", ws.P95Wait, tc.p95)
			}
			if ws.MaxWait != tc.max {
				t.Fatalf("max: got %v, want %v", ws.MaxWait, tc.max)
			}
		})
	}
}

// TestWaitRingEmptyWindow: a target without admissions reports zeros, not
// garbage (samples 0 is the honesty signal for "no data yet").
func TestWaitRingEmptyWindow(t *testing.T) {
	var r waitRing
	var ws WaitStats
	r.statsInto(&ws)
	if ws.Samples != 0 || ws.P95Wait != 0 || ws.MaxWait != 0 {
		t.Fatalf("empty ring must report zeros, got %+v", ws)
	}
}

// TestWaitRingRolloverEvictsOldest is the D3-W3 window gate: the aggregates
// come from exactly the last K samples — sample K+1 displaces sample 1. An
// early 10s outlier must dominate max through sample K and vanish with
// sample K+1.
func TestWaitRingRolloverEvictsOldest(t *testing.T) {
	var r waitRing
	r.add(10 * time.Second) // sample 1: the outlier
	for i := 0; i < waitRingK-1; i++ {
		r.add(time.Millisecond) // samples 2..K
	}
	var ws WaitStats
	r.statsInto(&ws)
	if ws.Samples != waitRingK {
		t.Fatalf("full window: got %d samples, want %d", ws.Samples, waitRingK)
	}
	if ws.MaxWait != 10*time.Second {
		t.Fatalf("outlier still inside the K-window must dominate max: got %v", ws.MaxWait)
	}
	r.add(time.Millisecond) // sample K+1 evicts sample 1
	r.statsInto(&ws)
	if ws.Samples != waitRingK {
		t.Fatalf("window must stay capped at K: got %d", ws.Samples)
	}
	if ws.MaxWait != time.Millisecond {
		t.Fatalf("sample K+1 must evict the outlier: max still %v", ws.MaxWait)
	}
	if ws.P95Wait != time.Millisecond {
		t.Fatalf("post-rollover p95: got %v, want %v", ws.P95Wait, time.Millisecond)
	}
}

// TestWaitStatsClassSeparation is the D3-W3 dispatcher-level gate: max_wait
// rises measurably under blocked admission, and the classes stay separated —
// a blocked background acquire lifts ONLY the background window, the
// interactive window keeps its ~0 immediate-admission samples.
func TestWaitStatsClassSeparation(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	const block = 60 * time.Millisecond

	occ, _, err := d.Acquire(ictx(principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ch := make(chan admission, 1)
	startWaiter(t.Context(), d, "bg", backgroundReq(), ch)
	waitFor(t, "background queued behind the held slot", func() bool {
		return waitingBackground(d) == 1
	})

	// While blocked: no background sample yet (samples are ADMITTED waits;
	// the still-queued acquire shows in Waiting/OldestWait instead).
	if ts := target(t, d); ts.Background.Samples != 0 {
		t.Fatalf("queued acquire must not sample before admission: %d", ts.Background.Samples)
	}

	time.Sleep(block)
	occ.Release()
	adm := <-ch
	if adm.err != nil {
		t.Fatalf("background admission: %v", adm.err)
	}
	defer adm.lease.Release()

	ts := target(t, d)
	if ts.Background.Samples != 1 {
		t.Fatalf("background samples: got %d, want 1", ts.Background.Samples)
	}
	if ts.Background.MaxWait < block {
		t.Fatalf("blocked admission must raise background max_wait: got %v, want ≥ %v",
			ts.Background.MaxWait, block)
	}
	if ts.Background.P95Wait < block {
		t.Fatalf("single-sample background p95 must equal the blocked wait: got %v", ts.Background.P95Wait)
	}
	// Class separation: the background blockade must not lift the
	// interactive window — its one sample is the occupier's immediate
	// admission (~0 wait).
	if ts.Interactive.Samples != 1 {
		t.Fatalf("interactive samples: got %d, want 1", ts.Interactive.Samples)
	}
	if ts.Interactive.MaxWait >= block {
		t.Fatalf("interactive window contaminated by the background wait: max %v", ts.Interactive.MaxWait)
	}
}

// TestWaitSamplesCountMatchesAdmissions is the window-honesty gate: the
// snapshot sample count equals the fed admissions per class (D3-W6
// before/after comparisons must not run on a since-boot mixture they cannot
// size).
func TestWaitSamplesCountMatchesAdmissions(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	for i := 0; i < 5; i++ {
		l, _, err := d.Acquire(ictx(principal("p")), interactiveReq())
		if err != nil {
			t.Fatalf("interactive %d: %v", i, err)
		}
		l.Release()
	}
	for i := 0; i < 3; i++ {
		l, _, err := d.Acquire(t.Context(), backgroundReq())
		if err != nil {
			t.Fatalf("background %d: %v", i, err)
		}
		l.Release()
	}
	ts := target(t, d)
	if ts.Interactive.Samples != 5 {
		t.Fatalf("interactive samples: got %d, want 5", ts.Interactive.Samples)
	}
	if ts.Background.Samples != 3 {
		t.Fatalf("background samples: got %d, want 3", ts.Background.Samples)
	}
}

// rangeMillis returns [from..to] milliseconds, ascending.
func rangeMillis(from, to int) []time.Duration {
	out := make([]time.Duration, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, time.Duration(i)*time.Millisecond)
	}
	return out
}

// repeatMillis returns n copies of v milliseconds.
func repeatMillis(v, n int) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}
