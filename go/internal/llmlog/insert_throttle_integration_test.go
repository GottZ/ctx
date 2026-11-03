//go:build integration

package llmlog_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// slowInsertTracer is the delay seam of the W06-1 gates: a pgx tracer that
// holds every context_llm_log INSERT for delay after it has its connection,
// and records how many of them run at the same time. It sits on the pool, not
// in llmlog, so the same probe measures any implementation of Record —
// including one that never heard of a test seam.
type slowInsertTracer struct {
	delay  time.Duration
	active atomic.Int64
	peak   atomic.Int64
}

type slowInsertKey struct{}

func (s *slowInsertTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "INSERT INTO context_llm_log") {
		return ctx
	}
	n := s.active.Add(1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(s.delay)
	return context.WithValue(ctx, slowInsertKey{}, true)
}

func (s *slowInsertTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(slowInsertKey{}) != nil {
		s.active.Add(-1)
	}
}

// slowLLMLogPool opens a second pool on the test database, sized like the
// production pool (store.NewPool: MaxConns 20), with tr on every connection.
func slowLLMLogPool(t *testing.T, dsn string, tr pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 20
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestRecordInsertConcurrencyBounded is W06-1 gate (b): a flood of 200
// simultaneous Record calls against a slow insert holds at most four
// context_llm_log inserts in flight at once — the llmlog path shares the
// 20-connection pool with every request path (and, later, the /v1 key
// auth), so it may not take the whole pool when the database is slow. Every
// row still lands: the throttle queues, it does not drop below the buffer
// bound.
func TestRecordInsertConcurrencyBounded(t *testing.T) {
	ref, dsn := testdb.SetupTestDBWithDSN(t)
	tr := &slowInsertTracer{delay: 50 * time.Millisecond}
	pool := slowLLMLogPool(t, dsn, tr)

	const n = 200
	var start, done sync.WaitGroup
	start.Add(1)
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-flood", Model: "m", Host: "h"})
		}()
	}
	start.Done()
	done.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := llmlog.Settle(ctx, pool); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	peak := tr.peak.Load()
	t.Logf("peak concurrent context_llm_log inserts: %d (pool MaxConns 20, %d Record calls)", peak, n)
	if peak > 4 {
		t.Errorf("peak concurrent llmlog inserts = %d, want <= 4 — the write path must not take the shared pool", peak)
	}
	if peak < 1 {
		t.Fatalf("the delay seam saw no insert at all — the probe lost its anchor")
	}

	var rows int
	if err := ref.QueryRow(context.Background(),
		`SELECT count(*) FROM context_llm_log WHERE pipeline = 'w06-1-flood'`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != n {
		t.Errorf("rows landed = %d, want %d — the throttle must queue, not lose", rows, n)
	}
}
