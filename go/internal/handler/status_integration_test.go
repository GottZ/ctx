//go:build integration

// Collector probes against a real PG18 testcontainer: the single-flight
// guarantee (N readers → one O(n) queue scan), the llm_24h aggregate + the
// backend-attribution completeness flag, and the last-dream-cycle timestamp.
//
//	go test -tags=integration ./internal/handler/ -run TestStatusCollector -count=1 -v
package handler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeDreamMode struct{}

func (fakeDreamMode) GetDreamMode() (int32, time.Duration) { return events.DreamModeOn, 0 }

// TestStatusCollectorSingleFlight proves the collector topology: N concurrent
// readers trigger exactly ONE dream-queue scan (the O(n) full-scan CTE), not N
// — the whole reason the collector exists (design 04 §3.6 / R12).
//
// The proof is a BARRIER, not a timing window (#30): the injected queueDepth
// holds the first flight open until the test releases it, so c.qsScan stays
// taken for the whole burst — it is acquired synchronously in the winning
// reader's own stack (status.go, scanQueueAsync's CAS) and released only by
// the flight goroutine's defer. Every later reader therefore MUST lose the
// CAS, whatever the scheduler does with it. The old shape instead waited on
// the scan COUNTER and then read: the counter moves inside queueDepth, one
// step before the goroutine stamps qsAt, so a reader could observe a finished
// flight with an unstamped qsAt and legitimately start a second scan — the
// nightly's "got 2".
func TestStatusCollectorSingleFlight(t *testing.T) {
	pool := testdb.SetupTestDB(t)

	var scans int32
	entered := make(chan struct{})
	release := make(chan struct{})
	// Never leave the held flight blocked, even on a t.Fatal below.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)
	c.queueDepth = func(_ context.Context, _ *pgxpool.Pool, _, _ []string) (*dream.QueueStats, error) {
		if atomic.AddInt32(&scans, 1) == 1 {
			close(entered)
			<-release // hold the first flight open for the whole burst
		}
		return &dream.QueueStats{}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Snapshot(context.Background())
		}()
	}
	wg.Wait()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no queue scan started for 4 concurrent readers") // deadlock guard, not the assertion
	}

	// (1) In-flight dedup: 4 concurrent readers plus one more read taken while
	// the flight is still running must cost ONE QueueDepth scan — the CAS
	// single-flight in scanQueueAsync.
	c.Snapshot(context.Background())
	if got := atomic.LoadInt32(&scans); got != 1 {
		t.Fatalf("N concurrent readers + a read during the flight must cost exactly 1 QueueDepth scan (CAS single-flight, scanQueueAsync), got %d", got)
	}

	releaseOnce()
	waitUntil(t, 10*time.Second, "the queue flight to stamp qsAt and release qsScan", func() bool {
		return c.qsAt.Load() != 0 && !c.qsScan.Load()
	})

	// (2) Interval dedup: qsAt is fresh now, so a further read stays inside the
	// queue_stats_interval (0 → the 30s fallback) and must not rescan.
	c.Snapshot(context.Background())
	if got := atomic.LoadInt32(&scans); got != 1 {
		t.Errorf("a read inside queue_stats_interval must not rescan, got %d scans", got)
	}
	if c.qs.Load() == nil {
		t.Errorf("qs must be populated after the flight landed")
	}
}

// TestStatusCollectorLLM24h pins the aggregate + the completeness flag: a row
// with NULL backend_name (the host-fallback path) flips llm_24h_complete false.
func TestStatusCollectorLLM24h(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Two attributed calls in one (backend, pipeline) group, one with an error.
	mustExec(`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, error, prompt_tokens, completion_tokens, backend_name)
		VALUES ('query-synthesize','qwen','herbert',8000,NULL,100,10,'herbert-chat')`)
	mustExec(`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, error, prompt_tokens, completion_tokens, backend_name)
		VALUES ('query-synthesize','qwen','herbert',4000,'boom',200,20,'herbert-chat')`)

	rows, complete := c.queryLLM24h(ctx)
	if !complete {
		t.Errorf("all rows attributed → complete must be true")
	}
	var found bool
	for _, r := range rows {
		if r.Backend == "herbert-chat" && r.Pipeline == "query-synthesize" {
			found = true
			if r.Calls != 2 || r.AvgMs != 6000 || r.Errors != 1 || r.PromptTokens != 300 || r.CompletionTokens != 30 {
				t.Errorf("aggregate wrong: %+v", r)
			}
		}
	}
	if !found {
		t.Fatalf("expected the herbert-chat/query-synthesize group, got %+v", rows)
	}

	// An un-attributed row (NULL backend_name → host fallback) flips complete.
	mustExec(`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, backend_name)
		VALUES ('rerank','bge','herbert-rerank',5,NULL)`)
	if _, complete2 := c.queryLLM24h(ctx); complete2 {
		t.Errorf("a NULL backend_name row must flip llm_24h_complete to false")
	}
}

// TestStatusCollectorLLM24hErrorRowComplete pins the attribution fix: an ERROR
// row with a NULL backend_name is legitimately un-attributed (the call failed
// before a backend was selected) and must NOT flip llm_24h_complete false — it is
// a known failure surfaced by the errors count, not a telemetry gap. Before the
// fix, bool_and(backend_name IS NOT NULL) flipped the flag for such a row; the
// corrected condition (… OR error IS NOT NULL) keeps the window complete.
func TestStatusCollectorLLM24hErrorRowComplete(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A failed call with NO backend_name (errored before backend attribution).
	mustExec(`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, error, backend_name)
		VALUES ('query-synthesize','qwen','herbert',12,'context deadline exceeded',NULL)`)

	if _, complete := c.queryLLM24h(ctx); !complete {
		t.Errorf("an error row with NULL backend_name must NOT flip complete false (legitimate failure, not a telemetry gap)")
	}
}

// TestStatusCollectorLastCycle pins that last_cycle_at tracks the dream-cycle
// pipelines only — a newer non-dream call must NOT advance it.
func TestStatusCollectorLastCycle(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)

	if c.queryLastCycleAt(ctx) != nil {
		t.Errorf("no dream rows → last_cycle_at must be nil")
	}

	mustExec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Dream cycle 2h ago; a NEWER non-dream synthesis call right now.
	mustExec(`INSERT INTO context_llm_log (created_at, pipeline, model, host)
		VALUES (now() - interval '2 hours','dream-eval','qwen','h')`)
	mustExec(`INSERT INTO context_llm_log (created_at, pipeline, model, host)
		VALUES (now(),'query-synthesize','qwen','h')`)

	lc := c.queryLastCycleAt(ctx)
	if lc == nil {
		t.Fatal("dream-eval row present → last_cycle_at must be non-nil")
	}
	// It must be the dream-eval timestamp (~2h ago), NOT the now() synthesis.
	if time.Since(*lc) < 30*time.Minute {
		t.Errorf("last_cycle_at advanced to a non-dream call: %v", lc)
	}
}
