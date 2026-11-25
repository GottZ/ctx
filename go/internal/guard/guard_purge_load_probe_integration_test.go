//go:build integration

// Issue #547 point 1, the measurement: a guard loop and a scope purge over the
// same blocks. CTX_G547_LOAD=<n> seeds n checked blocks that the purge takes
// (one tag) and n pending blocks, each a review-range neighbour of one of
// them, so nearly every audit row of the guard names a block the purge holds.
// It logs, for the guard alone, the purge alone and both side by side: the
// guard's processed and deferred blocks per second, the purge's duration, and
// the statements that ended in 40P01 on either side. A measurement, not a pin;
// it runs only with CTX_G547_LOAD set.
//
//	CTX_G547_LOAD=2000 go test -tags=integration -p 1 ./internal/guard/ -run 'TestGuardPurgeLoad' -v
package guard_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
)

func loadVec(r *rand.Rand) []float32 {
	v := make([]float32, 1024)
	var n float64
	for i := range v {
		v[i] = float32(r.NormFloat64())
		n += float64(v[i]) * float64(v[i])
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func TestGuardPurgeLoad(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("CTX_G547_LOAD"))
	if err != nil || n <= 0 {
		t.Skip("set CTX_G547_LOAD to the block count of each side")
	}
	f := newG284Fixture(t)
	ctx := context.Background()
	r := rand.New(rand.NewSource(547))

	seed := func(phase string) (tag string) {
		tag = "load-" + phase
		var pairs [][]float32
		for i := 0; i < n; i++ {
			v := loadVec(r)
			pairs = append(pairs, v)
			f.tagged(t, fmt.Sprintf("%s target %d", phase, i), tag, v)
		}
		for {
			k, err := guard.RunGuardBatch(ctx, f.pool, f.set, 100, guard.SearchIndexForTest)
			if err != nil {
				t.Fatal(err)
			}
			if k == 0 {
				break
			}
		}
		for i, v := range pairs {
			w := loadVec(r)
			p := make([]float32, len(v))
			for j := range v {
				p[j] = 0.96*v[j] + 0.28*w[j]
			}
			f.block(t, fmt.Sprintf("%s pending %d", phase, i), p)
		}
		return tag
	}

	type outcome struct {
		processed, deferred      int64
		guardTook, purgeTook     time.Duration
		runDeadlocks, purgeDeads int64
	}
	runGuard := func(stop <-chan struct{}, o *outcome, tr *g547Tracer) {
		pool := f.traced(t, tr)
		start := time.Now()
		for {
			select {
			case <-stop:
				o.guardTook = time.Since(start)
				return
			default:
			}
			k, err := guard.RunGuardBatch(ctx, pool, f.set, 100, guard.SearchIndexForTest)
			if err != nil {
				t.Errorf("guard: %v", err)
			}
			_, last := guard.DeferredVerdicts()
			o.processed += int64(k)
			o.deferred += last
			if k == 0 && last == 0 {
				o.guardTook = time.Since(start)
				return
			}
		}
	}
	purge := func(tag string, o *outcome, tr *g547Tracer) {
		pool := f.traced(t, tr)
		start := time.Now()
		confirm := int64(n)
		res, err := store.PurgeScope(ctx, pool, store.PurgeQuery{Scope: "private", AllTenants: true,
			Filter: store.PurgeFilter{Tags: []string{tag}}, ConfirmBlocks: &confirm})
		o.purgeTook = time.Since(start)
		if err != nil || res.Deleted == nil || res.Deleted.Blocks != confirm {
			t.Errorf("purge: %+v %v", res, err)
		}
	}

	var alone, purgeAlone, both outcome
	tag := seed("alone")
	{
		tr := &g547Tracer{}
		runGuard(make(chan struct{}), &alone, tr)
		alone.runDeadlocks = tr.deadlocks.Load()
		ptr := &g547Tracer{}
		purge(tag, &purgeAlone, ptr)
		purgeAlone.purgeDeads = ptr.deadlocks.Load()
	}
	tag = seed("both")
	{
		tr, ptr := &g547Tracer{}, &g547Tracer{}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); runGuard(stop, &both, tr) }()
		purge(tag, &both, ptr)
		close(stop)
		wg.Wait()
		both.runDeadlocks, both.purgeDeads = tr.deadlocks.Load(), ptr.deadlocks.Load()
		for { // what the loop left: the deferred blocks get their verdict in quiet runs
			k, err := guard.RunGuardBatch(ctx, f.pool, f.set, 100, guard.SearchIndexForTest)
			if err != nil || k == 0 {
				break
			}
		}
	}
	rate := func(o outcome) float64 { return float64(o.processed) / o.guardTook.Seconds() }
	t.Logf("guard alone: %d processed, %d deferred in %s (%.0f/s), 40P01 %d", alone.processed, alone.deferred, alone.guardTook.Round(time.Millisecond), rate(alone), alone.runDeadlocks)
	t.Logf("purge alone: %d blocks in %s, 40P01 %d", n, purgeAlone.purgeTook.Round(time.Millisecond), purgeAlone.purgeDeads)
	t.Logf("side by side: purge %d blocks in %s (40P01 %d); guard %d processed, %d deferred until the purge ended, %s (%.0f/s), 40P01 %d",
		n, both.purgeTook.Round(time.Millisecond), both.purgeDeads, both.processed, both.deferred, both.guardTook.Round(time.Millisecond), rate(both), both.runDeadlocks)
}
