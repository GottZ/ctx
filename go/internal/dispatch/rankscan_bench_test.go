package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkRankScanMaxOpDur is the A03-W2 cost measurement (design/03 §7:
// MaxOpDur < 1 ms at 64 waiters): 64 interactive and 64 background
// acquire/release loops contend for one slot until 10 000 interactive cycles
// have run, so every release scans the rank table over deep queues. It
// reports the longest dispatcher mutex section (Snapshot.MaxOpDur — Release
// includes the wake) per run. It uses only the stock API, so the same
// measurement runs on a tree without the rank scan; read it as a
// distribution against that baseline under the same host load, not as a
// single-run pass/fail: one descheduled mutex holder on a saturated host
// lands in the maximum regardless of the scan. Measure WITHOUT -race (the
// race detector inflates every section) and record the load (uptime):
//
//	go test -run '^$' -bench RankScanMaxOpDur -benchtime 1x -count 20 ./internal/dispatch/
func BenchmarkRankScanMaxOpDur(b *testing.B) {
	for i := 0; i < b.N; i++ {
		snap := rankScanCostRun(b)
		b.ReportMetric(float64(snap.MaxOpDur)/float64(time.Microsecond), "maxop-us")
		b.ReportMetric(float64(snap.OpsTotal), "ops")
	}
}

// rankScanCostRun drives one cost scenario and returns the final snapshot.
// Eight principals (eight tenants) run eight interactive loops each — exactly
// the per-principal cap, so no loop is ever rejected; background is
// uncapped so all 64 background loops can wait at once.
func rankScanCostRun(b *testing.B) Snapshot {
	b.Helper()
	const perClass, cycles = 64, 10_000
	s := DefaultSettings()
	s.BackgroundQueueMax = 0
	d := New(slog.New(slog.DiscardHandler), s)
	defer d.Close()
	d.UpdatePolicy(onSlotPolicy(1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var remaining atomic.Int64
	remaining.Store(cycles)
	var iaWG, bgWG sync.WaitGroup
	for i := 0; i < perClass; i++ {
		ictx := withPrincipal(ctx, principal(fmt.Sprintf("p%d", i%8)))
		iaWG.Add(1)
		go func() {
			defer iaWG.Done()
			for remaining.Add(-1) >= 0 {
				l, _, err := d.Acquire(ictx, interactiveReq())
				if err != nil {
					b.Errorf("interactive acquire: %v", err)
					return
				}
				l.Release()
			}
		}()
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			for ctx.Err() == nil {
				l, _, err := d.Acquire(ctx, backgroundReq())
				if err != nil {
					return // ctx canceled once the interactive cycles are done
				}
				l.Release()
			}
		}()
	}
	iaWG.Wait()
	cancel()
	bgWG.Wait()
	return d.Snapshot()
}
