package llmlog

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// refusingPool returns a pool whose every insert fails at once: pgxpool.New
// connects lazily and port 1 refuses, so the tests need no database.
func refusingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://u@127.0.0.1:1/x?connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// useInsertForTest installs fn as pool's insert, before any Record on pool —
// the per-pool seam: no package variable is swapped under a running worker.
func useInsertForTest(pool *pgxpool.Pool, fn func(*pgxpool.Pool, Entry) error) {
	writers.Store(pool, newWriter(pool, fn))
}

// resetLossStateForTest zeroes the process-wide loss counters and re-arms the
// WARN throttle, so a test can count from a known origin.
func resetLossStateForTest() {
	insertFailures.Store(0)
	insertDropped.Store(0)
	lastLossWarn.Store(0)
}

// warnCapture collects the llmlog WARN records written through slog.Default.
type warnCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (c *warnCapture) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (c *warnCapture) Handle(_ context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, "llmlog:") {
		c.mu.Lock()
		c.recs = append(c.recs, r)
		c.mu.Unlock()
	}
	return nil
}
func (c *warnCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *warnCapture) WithGroup(string) slog.Handler      { return c }

func (c *warnCapture) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slog.Record(nil), c.recs...)
}

func captureWarns(t *testing.T) *warnCapture {
	t.Helper()
	c := &warnCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

func int64Attr(r slog.Record, key string) (int64, bool) {
	var v int64
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, ok = a.Value.Int64(), true
			return false
		}
		return true
	})
	return v, ok
}

func settleOrFail(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Settle(ctx, pool); err != nil {
		t.Fatalf("Settle: %v", err)
	}
}

// TestInsertFailureCountedAndWarnThrottled is the unit half of W06-1 gate (a):
// every failed insert moves the process-wide failure counter, and a burst of
// 100 failures inside one minute raises exactly ONE WARN — carrying the running
// total — instead of 100 lines (or, before W06-1, none at all: the failure was
// a Debug line and nothing else). Once the interval has passed, the next
// failure warns again with the total since start.
func TestInsertFailureCountedAndWarnThrottled(t *testing.T) {
	pool := refusingPool(t)
	resetLossStateForTest()
	warns := captureWarns(t)

	for range 100 {
		Record(pool, Entry{Pipeline: "w06-1-fail", Model: "m"})
	}
	settleOrFail(t, pool)

	if got := InsertFailures(); got != 100 {
		t.Fatalf("InsertFailures() = %d after 100 failed inserts, want 100", got)
	}
	recs := warns.snapshot()
	if len(recs) != 1 {
		t.Fatalf("100 failures inside one minute raised %d WARN lines, want exactly 1", len(recs))
	}
	if n, ok := int64Attr(recs[0], "insert_failures_total"); !ok || n < 1 {
		t.Errorf("the WARN must carry insert_failures_total >= 1, got %d (present=%v)", n, ok)
	}

	// The interval has passed: the next failure warns, with the total since start.
	lastLossWarn.Add(-int64(lossWarnInterval))
	Record(pool, Entry{Pipeline: "w06-1-fail", Model: "m"})
	settleOrFail(t, pool)

	recs = warns.snapshot()
	if len(recs) != 2 {
		t.Fatalf("a failure after the interval raised %d WARN lines in total, want 2", len(recs))
	}
	if n, _ := int64Attr(recs[1], "insert_failures_total"); n != 101 {
		t.Errorf("second WARN insert_failures_total = %d, want 101 (the total since start)", n)
	}
}

// TestRecordQueueBoundDropsAndCounts pins the overflow half of W06-1: with all
// insert slots busy, Record queues up to maxQueuedInserts rows and drops — and
// counts — every row beyond that, without blocking its caller. Settle waits on
// queued rows that have not started yet, gives up with its ctx while an insert
// hangs, and returns once the queue drained.
func TestRecordQueueBoundDropsAndCounts(t *testing.T) {
	pool := refusingPool(t)
	release := make(chan struct{})
	started := make(chan struct{}, maxConcurrentInserts)
	var inserted atomic.Int64
	useInsertForTest(pool, func(*pgxpool.Pool, Entry) error {
		select {
		case started <- struct{}{}: // only the first slot-fillers are awaited
		default:
		}
		<-release
		inserted.Add(1)
		return nil
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	resetLossStateForTest()

	// Fill every slot and wait until each insert is running, so the queue
	// below holds exactly what Record queued.
	for range maxConcurrentInserts {
		Record(pool, Entry{Pipeline: "w06-1-slot", Model: "m"})
	}
	for range maxConcurrentInserts {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the insert slots never started")
		}
	}

	const overflow = 7
	begin := time.Now()
	for range maxQueuedInserts + overflow {
		Record(pool, Entry{Pipeline: "w06-1-queue", Model: "m"})
	}
	if el := time.Since(begin); el > 2*time.Second {
		t.Errorf("Record blocked its caller for %s while the slots were busy", el)
	}
	if got := InsertDropped(); got != overflow {
		t.Errorf("InsertDropped() = %d, want %d (only the rows beyond the %d-row buffer)", got, overflow, maxQueuedInserts)
	}
	if got, want := inflightFor(pool).Load(), int64(maxConcurrentInserts+maxQueuedInserts); got != want {
		t.Errorf("pending = %d, want %d (running + queued count from enqueue)", got, want)
	}

	// Settle waits on queued-but-not-started rows and respects its ctx.
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := Settle(short, pool); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Settle with a hanging insert = %v, want DeadlineExceeded", err)
	}

	close(release)
	settleOrFail(t, pool)
	if got, want := inserted.Load(), int64(maxConcurrentInserts+maxQueuedInserts); got != want {
		t.Errorf("inserted = %d, want %d — every queued row must run", got, want)
	}
	if got := InsertFailures(); got != 0 {
		t.Errorf("InsertFailures() = %d, want 0 — drops are not failures", got)
	}
}

// TestRecordInsertSlotsBounded is the unit twin of gate (b): 200 simultaneous
// Record calls never run more than maxConcurrentInserts inserts at once.
func TestRecordInsertSlotsBounded(t *testing.T) {
	pool := refusingPool(t)
	var active, peak, calls atomic.Int64
	useInsertForTest(pool, func(*pgxpool.Pool, Entry) error {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
		calls.Add(1)
		return nil
	})

	var start, done sync.WaitGroup
	start.Add(1)
	for range 200 {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			Record(pool, Entry{Pipeline: "w06-1-slots", Model: "m"})
		}()
	}
	start.Done()
	done.Wait()
	settleOrFail(t, pool)

	if p := peak.Load(); p < 1 || p > maxConcurrentInserts {
		t.Errorf("peak concurrent inserts = %d, want 1..%d", p, maxConcurrentInserts)
	}
	if c := calls.Load(); c != 200 {
		t.Errorf("inserts run = %d, want 200", c)
	}
}

// TestRecordAfterSettleAndClose is the negative probe of W06-1: a late caller
// after the shutdown settle — even on a pool that is already closed — neither
// panics nor wedges; its row is a counted failure and Settle returns.
func TestRecordAfterSettleAndClose(t *testing.T) {
	pool := refusingPool(t)
	settleOrFail(t, pool)

	before := InsertFailures()
	pool.Close()
	Record(pool, Entry{Pipeline: "w06-1-late", Model: "m"})
	settleOrFail(t, pool)
	if got := InsertFailures() - before; got != 1 {
		t.Errorf("a late Record on a closed pool moved InsertFailures by %d, want 1", got)
	}
}
