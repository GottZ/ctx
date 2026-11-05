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
		snap, refused, enqueued := predictionCostRun(t, 64, 10_000)
		t.Logf("run %d: MaxOpDur %v over %d ops, %d enqueued, %d refused", run, snap.MaxOpDur, snap.OpsTotal, enqueued, refused)
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
// number of refusals and the number of enqueues.
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
