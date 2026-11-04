//go:build integration

// W01-6 gate (d) — the DB semaphore (B18, design/01 §6 "DB-Pool"): 1000
// parallel forgeries (probe bucket opened wide) while a DB-bound knowledge
// load runs on the same production-sized pool (MaxConns 20). The knowledge
// load is a stand-in with a real pgxpool.Acquire (/api/query needs models):
// 12 workers, each Acquire → SELECT pg_sleep(0.002) → Release. Measured: the
// p99 of the load's Acquire duration, flood against baseline; the door
// answers 401 or 503 auth_unavailable, never 500. Red without the semaphore.
// This measurement closes the open measurement "Auth-Semaphor-Kapazität"
// (masterplan, Offene Messungen).
//
//	go test -tags=integration -p 1 -count=1 -run TestModelAuthSemaphore -v ./internal/handler/
package handler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/oai"
)

const (
	semLoadWorkers = 12 // stays below the 16 connections the semaphore leaves
	semFlooders    = 1000
	semPhase       = 3 * time.Second
	semRatio       = 1.1
)

func semLoadavg() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "n/a"
	}
	return strings.TrimSpace(string(b))
}

func semQuantile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(q*float64(len(s))+0.5) - 1
	return s[max(0, min(i, len(s)-1))]
}

func TestModelAuthSemaphore_Integration(t *testing.T) {
	e := madSetup(t)
	e.settings.auth(func(a *oai.AuthSettings) { a.ProbeRate = 1 << 30 })
	d, h, _ := e.door(t, "")
	keys := forged(t, 20000)
	ctx := context.Background()

	phase := func(flood bool) ([]time.Duration, map[string]int, int64) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var samples []time.Duration
		statuses := map[string]int{}
		var requests atomic.Int64
		if flood {
			for g := range semFlooders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ip := fmt.Sprintf("10.30.%d.%d", g>>8, g&255)
					for it := 0; ; it++ {
						select {
						case <-stop:
							return
						default:
						}
						k := keys[(g*7919+it)%len(keys)]
						r := madDo(h, madReq{ip: ip, bearer: k.Plaintext()})
						requests.Add(1)
						key := fmt.Sprintf("%d %s", r.Code, madCode(r))
						mu.Lock()
						statuses[key]++
						mu.Unlock()
					}
				}()
			}
			time.Sleep(200 * time.Millisecond) // let the flood reach steady state
		}
		var lw sync.WaitGroup
		for range semLoadWorkers {
			lw.Add(1)
			go func() {
				defer lw.Done()
				var local []time.Duration
				for {
					select {
					case <-stop:
						mu.Lock()
						samples = append(samples, local...)
						mu.Unlock()
						return
					default:
					}
					t0 := time.Now()
					c, err := e.pool.Acquire(ctx)
					dt := time.Since(t0)
					if err != nil {
						t.Errorf("knowledge load acquire: %v", err)
						return
					}
					local = append(local, dt)
					if _, err := c.Exec(ctx, "SELECT pg_sleep(0.002)"); err != nil {
						t.Errorf("knowledge load query: %v", err)
					}
					c.Release()
				}
			}()
		}
		time.Sleep(semPhase)
		close(stop)
		lw.Wait()
		wg.Wait()
		return samples, statuses, requests.Load()
	}

	t.Logf("loadavg before: %s", semLoadavg())
	base, _, _ := phase(false)
	flood, statuses, reqs := phase(true)
	t.Logf("loadavg after: %s", semLoadavg())
	st := d.Stats()
	b99, f99 := semQuantile(base, 0.99), semQuantile(flood, 0.99)
	ratio := float64(f99) / float64(b99)
	t.Logf("baseline: n=%d p50=%v p99=%v max=%v", len(base), semQuantile(base, 0.5), b99, semQuantile(base, 1))
	t.Logf("flood:    n=%d p50=%v p99=%v max=%v", len(flood), semQuantile(flood, 0.5), f99, semQuantile(flood, 1))
	t.Logf("p99 ratio flood/baseline = %.3f (gate <= %.1f); flood requests %d, statuses %v", ratio, semRatio, reqs, statuses)
	t.Logf("door: semaphore waits %d, total wait %v, semaphore 503 %d, db errors %d, invalid %d",
		st.SemaphoreWaits, st.SemaphoreWaitTotal, st.SemaphoreTimeouts, st.DBErrors, st.Invalid)
	for k := range statuses {
		if k != fmt.Sprintf("%d %s", http.StatusUnauthorized, oai.CodeInvalidAPIKey) &&
			k != fmt.Sprintf("%d %s", http.StatusServiceUnavailable, ModelAuthCodeUnavailable) {
			t.Errorf("flood answer %q (%d×) — only 401 invalid_api_key or 503 %s are allowed, never 500", k, statuses[k], ModelAuthCodeUnavailable)
		}
	}
	if reqs < semFlooders {
		t.Fatalf("flood ran only %d requests", reqs)
	}
	if ratio > semRatio {
		t.Fatalf("knowledge-load acquire p99 %v under the flood is %.3f × the baseline %v (gate <= %.1f)", f99, ratio, b99, semRatio)
	}
}
