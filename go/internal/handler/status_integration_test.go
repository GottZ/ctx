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
func TestStatusCollectorSingleFlight(t *testing.T) {
	pool := testdb.SetupTestDB(t)

	var scans int32
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil)
	c.queueDepth = func(_ context.Context, _ *pgxpool.Pool, _ []string) (*dream.QueueStats, error) {
		atomic.AddInt32(&scans, 1)
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

	// Wait for the single async scan goroutine to land.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&scans) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// A further read must NOT rescan: qsAt is fresh now (< queue_stats_interval).
	c.Snapshot(context.Background())
	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&scans); got != 1 {
		t.Errorf("expected exactly 1 QueueDepth scan for N readers, got %d", got)
	}
}

// TestStatusCollectorLLM24h pins the aggregate + the completeness flag: a row
// with NULL backend_name (the host-fallback path) flips llm_24h_complete false.
func TestStatusCollectorLLM24h(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil)

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
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil)

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
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil)

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
