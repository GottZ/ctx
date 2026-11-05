//go:build !race

package dispatch

// Gate (i) of A03-W4 measures wall time of dispatcher mutex sections, so it
// is excluded from -race builds (the race detector inflates every section: a
// tree without A03-W4 measured 3.0–4.8 ms under -race at load 11–15, 0.12–
// 0.69 ms without), from -short runs, and it is opt-in: it runs only with
// CTX_FAIRNESS_COST_GATE set (A03-W6, the PR #132 bot review) — on a loaded
// host one descheduled mutex holder lands in the maximum regardless of the
// pick (the same tree without A03-W4 measured up to 15.7 ms at load 21–28),
// so a plain `go test ./internal/dispatch/` must not depend on it. Run it
// explicitly, without -race and -short, next to the same file on the base
// tree under the same load (the A03-W2 measurement precedent,
// rankscan_bench_test.go), and record the load (uptime):
//
//	CTX_FAIRNESS_COST_GATE=1 go test -count=1 -v -run TestExternalFairPickCost ./internal/dispatch/
//
// The complexity of the pick is gated in every mode by the median-based
// TestExternalPickCostScales (fairness_internal_test.go).

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestExternalFairPickCost is gate (i): 64 identity buckets (64 consumers in
// eight tenants, one closed-loop client each) contend for E = 1 (S = 2,
// R = 1) until 10 000 releases have each run the pick over the queue; the
// longest dispatcher mutex section stays below the 1-ms WARN threshold
// (opWarnThreshold). The minimum over three runs is taken: one descheduled
// mutex holder on a loaded host lands in a single run's maximum regardless
// of the pick (the A03-W2 measurement note), a pick cost recurs in all three.
func TestExternalFairPickCost(t *testing.T) {
	if testing.Short() || os.Getenv("CTX_FAIRNESS_COST_GATE") == "" {
		t.Skip("wall-time measurement of mutex sections: opt-in with CTX_FAIRNESS_COST_GATE=1, without -short (see the file comment)")
	}
	best := time.Duration(-1)
	for run := 0; run < 3; run++ {
		snap := fairPickCostRun(t, 64, 10_000)
		t.Logf("run %d: MaxOpDur %v over %d ops", run, snap.MaxOpDur, snap.OpsTotal)
		if best < 0 || snap.MaxOpDur < best {
			best = snap.MaxOpDur
		}
	}
	if best >= opWarnThreshold {
		t.Fatalf("MaxOpDur %v (minimum over three runs) with 64 external buckets, want < %v", best, opWarnThreshold)
	}
}

// fairPickCostRun drives one cost run and returns the final snapshot.
func fairPickCostRun(t *testing.T, buckets, cycles int) Snapshot {
	t.Helper()
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	s.ExternalMaxWait = time.Minute
	d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var remaining atomic.Int64
	remaining.Store(int64(cycles))
	var wg sync.WaitGroup
	for i := 0; i < buckets; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cctx := xctx(ctx, fmt.Sprintf("cost-%d", i), fmt.Sprintf("tenant-%d", i%8))
			for remaining.Add(-1) >= 0 {
				l, _, err := d.Acquire(cctx, externalReq())
				if err != nil {
					t.Errorf("cost acquire: %v", err)
					return
				}
				l.Release()
			}
		}(i)
	}
	wg.Wait()
	return d.Snapshot()
}
