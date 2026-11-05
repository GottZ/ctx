//go:build !race

package dispatch

// Gate (h) of A03-W6 measures wall time of dispatcher mutex sections — the
// same measurement class as TestExternalFairPickCost (fairness_cost_test.go):
// excluded from -race builds, and opt-in through CTX_FAIRNESS_COST_GATE, the
// one switch of the dispatcher's wall-time cost gates, so a plain
// `go test ./internal/dispatch/` on a loaded host cannot fail on timing
// noise. Run it explicitly and record the load (uptime):
//
//	CTX_FAIRNESS_COST_GATE=1 go test -count=1 -v -run TestPredictionEnqueueCost ./internal/dispatch/

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPredictionEnqueueCost is gate (h): 64 identity buckets (64 consumers in
// eight tenants, one closed-loop client each) against E = 1 on a warm lease
// window (p50 100 µs, then the measured lease ends), until 10 000 acquires
// have run the relational cap and the prediction — nearly all of them pass
// and queue behind up to 63 other buckets; the longest dispatcher mutex
// section stays below the 1-ms WARN threshold. Minimum over three runs, as in
// gate (i) of A03-W4.
func TestPredictionEnqueueCost(t *testing.T) {
	if testing.Short() || os.Getenv("CTX_FAIRNESS_COST_GATE") == "" {
		t.Skip("wall-time measurement of mutex sections: opt-in with CTX_FAIRNESS_COST_GATE=1, without -short (see the file comment)")
	}
	best := time.Duration(-1)
	for run := 0; run < 3; run++ {
		snap, refused, pushes := predictionCostRun(t, 64, 10_000)
		// A refusal predicted after the push is a push removed again in
		// its own section (review 2 F-1): the lasting enqueues are the
		// pushes minus the predicted refusals.
		var predicted int64
		for _, ts := range snap.Targets {
			predicted += ts.ExternalRejects.Predicted
		}
		enqueued := pushes - uint64(predicted)
		t.Logf("run %d: MaxOpDur %v over %d ops, %d pushes, %d enqueued, %d refused", run, snap.MaxOpDur, snap.OpsTotal, pushes, enqueued, refused)
		if enqueued < 5_000 {
			t.Fatalf("run %d: %d enqueues, want most of the 10 000 acquires to pass the prediction and queue", run, enqueued)
		}
		if best < 0 || snap.MaxOpDur < best {
			best = snap.MaxOpDur
		}
	}
	if best >= opWarnThreshold {
		t.Fatalf("MaxOpDur %v (minimum over three runs) with 64 buckets and the prediction, want < %v", best, opWarnThreshold)
	}
}

// predictionCostRun drives one cost run and returns the final snapshot, the
// number of refusals and the number of pushes (arrival seq).
func predictionCostRun(t *testing.T, buckets, acquires int) (Snapshot, int64, uint64) {
	t.Helper()
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	s.ExternalMaxWait = time.Second
	d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, waitRingK, 100*time.Microsecond)
	seq0 := targetSeq(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var remaining, refused atomic.Int64
	remaining.Store(int64(acquires))
	var wg sync.WaitGroup
	for i := 0; i < buckets; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cctx := xctx(ctx, fmt.Sprintf("cost-%d", i), fmt.Sprintf("tenant-%d", i%8))
			for remaining.Add(-1) >= 0 {
				l, _, err := d.Acquire(cctx, externalReq())
				switch {
				case err == nil:
					l.Release()
				case IsRejection(err):
					refused.Add(1)
				default:
					t.Errorf("cost acquire: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	return d.Snapshot(), refused.Load(), targetSeq(d) - seq0
}

// TestPredictionRefusalPathCost is the cost gate of the refusal path the
// hardening after review 2 added (F-1, F-2): each refused acquire is pushed,
// woken, measured against the rounds bound, passes over the held external
// leases for r̂, over the bucket heads for the fairness-first check, and is
// dropped again — E = 64 held (the oldest lease 9 s into an L_p50 of 10 s ⇒
// r̂ ≈ 1 s ≤ MaxWait 5 s < ŵ ≈ 11 s), 64 waiting buckets of 64 tenants
// ahead; 10 000 newcomers of 13 other tenants, 8 parallel callers. The
// longest dispatcher mutex section stays below the 1-ms WARN threshold
// (minimum over three runs); every newcomer is refused with Q(r̂ + L_p50) =
// 15 s on the steps 1,2,5,10,15,20,30 (Q(2 · L_p50) would be 20 s — the
// passes ran).
func TestPredictionRefusalPathCost(t *testing.T) {
	if testing.Short() || os.Getenv("CTX_FAIRNESS_COST_GATE") == "" {
		t.Skip("wall-time measurement of mutex sections: opt-in with CTX_FAIRNESS_COST_GATE=1, without -short (see the file comment)")
	}
	best := time.Duration(-1)
	for run := 0; run < 3; run++ {
		snap, refused, ras := refusalPathCostRun(t, 64, 10_000)
		t.Logf("run %d: MaxOpDur %v over %d ops, %d refused, Retry-After %v", run, snap.MaxOpDur, snap.OpsTotal, refused, ras)
		if refused != 10_000 || len(ras) != 1 || ras[15*time.Second] != 10_000 {
			t.Fatalf("run %d: %d refused, Retry-After %v, want all 10 000 refused at 15 s (the r̂ and fairness passes ran)", run, refused, ras)
		}
		if best < 0 || snap.MaxOpDur < best {
			best = snap.MaxOpDur
		}
	}
	if best >= opWarnThreshold {
		t.Fatalf("MaxOpDur %v (minimum over three runs) on the refusal path with 64 leases and 64 buckets, want < %v", best, opWarnThreshold)
	}
}

// refusalPathCostRun drives one refusal-path cost run: the final snapshot,
// the refusals and their Retry-After histogram.
func refusalPathCostRun(t *testing.T, e, acquires int) (Snapshot, int, map[time.Duration]int) {
	t.Helper()
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	s.ExternalMaxWait = 5 * time.Second
	s.ExternalRetryAfterSteps = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second, 30 * time.Second}
	d, _ := newTestDispatcher(t, s, partitionPolicy(e+1, 1)) // E = e
	bg := context.Background()
	var held []*Lease
	for i := 0; i < e; i++ {
		held = append(held, mustAcquire(t, d, xctx(bg, "holder", "tenant-h"), externalReq()))
	}
	t.Cleanup(func() {
		for _, l := range held {
			l.Release()
		}
	})
	ageLease(d, held[0], 9*time.Second)
	ctx, cancel := context.WithCancel(bg)
	t.Cleanup(cancel)
	ch := make(chan admission, e)
	for i := 0; i < e; i++ {
		queueAt(t, d, ctx, testOrigin, fmt.Sprintf("w%d", i), fmt.Sprintf("wt%d", i), 1, ch) // cold: queues
	}
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, waitRingK, 10*time.Second)
	var mu sync.Mutex
	ras := map[time.Duration]int{}
	refused := 0
	const par = 8
	var wg sync.WaitGroup
	for p := 0; p < par; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < acquires/par; i++ {
				l, _, err := d.Acquire(xctx(bg, fmt.Sprintf("n%d-%d", p, i%37), fmt.Sprintf("nt%d", i%13)), externalReq())
				if l != nil {
					l.Release()
				}
				var re *RejectionError
				if errors.As(err, &re) {
					mu.Lock()
					refused++
					ras[re.RetryAfter]++
					mu.Unlock()
				}
			}
		}(p)
	}
	wg.Wait()
	return d.Snapshot(), refused, ras
}
