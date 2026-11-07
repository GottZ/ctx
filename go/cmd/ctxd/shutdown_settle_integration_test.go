//go:build integration

package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// delayLLMLogInserts holds every context_llm_log INSERT for d after it has its
// connection — the "slow database at shutdown" seam of W06-1 gate (c). It sits
// on the pool, so the probe needs nothing from llmlog's internals.
type delayLLMLogInserts struct{ d time.Duration }

func (s delayLLMLogInserts) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO context_llm_log") {
		time.Sleep(s.d)
	}
	return ctx
}

func (delayLLMLogInserts) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// slowShutdownPool opens a production-sized pool (MaxConns 20, store.NewPool)
// on the test database with the insert delay installed. The caller closes it:
// the close IS the step under test.
func slowShutdownPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 20
	cfg.ConnConfig.Tracer = delayLLMLogInserts{d: 50 * time.Millisecond}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return p
}

func countLLMLogRows(t *testing.T, pool *pgxpool.Pool, pipeline string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, pipeline).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", pipeline, err)
	}
	return n
}

// TestShutdownWithoutSettleLosesRows is the CONTROL of gate (c), not a gate:
// the pre-W06-1 teardown — the pool closes right after the serve loop and the
// scheduler ended, with nothing waiting for the llmlog writes — loses rows
// once inserts are slower than the teardown. It stays green on both sides of
// the change; it exists so the gate below cannot pass because the delay seam
// silently stopped creating a window.
func TestShutdownWithoutSettleLosesRows(t *testing.T) {
	ref, dsn := testdb.SetupTestDBWithDSN(t)
	pool := slowShutdownPool(t, dsn)

	const n = 50
	for range n {
		llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-no-settle", Model: "m", Host: "h"})
	}
	pool.Close()

	got := countLLMLogRows(t, ref, "w06-1-no-settle")
	t.Logf("rows landed without settle: %d of %d", got, n)
	if got >= n {
		t.Fatalf("all %d rows landed without a settle — the delay seam no longer opens a loss window, gate (c) proves nothing", n)
	}
}

// drainingScheduler stands in for *events.Scheduler: its Wait is where a
// running dream cycle — SIGTERM does not cancel it — still writes llmlog rows.
type drainingScheduler struct{ wait func() }

func (s drainingScheduler) Wait() { s.wait() }

// TestShutdownSequenceSettlesLLMLog is W06-1 gate (c): the daemon's teardown
// waits for the llmlog writes before the pool goes. 50 rows recorded before
// the shutdown signal against a slow database all land; so do the 10 rows a
// draining scheduler writes INSIDE scheduler.Wait — the probe for the second
// settle after Wait (a settle only between srv.Shutdown and Wait, as design/06
// §4.2 sketched it, returns before the dream drain has written; the first
// settle's own probe is TestShutdownSettlesHTTPRowsBeforeSchedulerWait).
// A late Record after the sequence neither panics nor holds up the close.
func TestShutdownSequenceSettlesLLMLog(t *testing.T) {
	ref, dsn := testdb.SetupTestDBWithDSN(t)
	pool := slowShutdownPool(t, dsn)

	const before, drain = 50, 10
	for range before {
		llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-before-shutdown", Model: "m", Host: "h"})
	}
	sched := drainingScheduler{wait: func() {
		for range drain {
			llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-scheduler-drain", Model: "m", Host: "h"})
		}
	}}

	shutdownSequence(&http.Server{}, sched, pool, func() {}, func() {})
	llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-late", Model: "m", Host: "h"})
	closed := make(chan struct{})
	go func() { pool.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("pool.Close hung behind a late Record after the shutdown sequence")
	}

	if got := countLLMLogRows(t, ref, "w06-1-before-shutdown"); got != before {
		t.Errorf("rows recorded before shutdown that landed = %d, want %d", got, before)
	}
	if got := countLLMLogRows(t, ref, "w06-1-scheduler-drain"); got != drain {
		t.Errorf("rows recorded during scheduler.Wait that landed = %d, want %d — the settle must run after the scheduler drained", got, drain)
	}
}

// TestSettleLLMLogRespectsTimeout: the shutdown settle gives up at its budget
// instead of holding the process past the stop grace period.
func TestSettleLLMLogRespectsTimeout(t *testing.T) {
	_, dsn := testdb.SetupTestDBWithDSN(t)
	pool := slowShutdownPool(t, dsn)
	defer pool.Close()

	for range 50 {
		llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-timeout", Model: "m", Host: "h"})
	}
	begin := time.Now()
	settleLLMLog(pool, 20*time.Millisecond, "http")
	if el := time.Since(begin); el > 2*time.Second {
		t.Errorf("settleLLMLog with a 20ms budget returned after %s", el)
	}
}

// TestShutdownSettlesHTTPRowsBeforeSchedulerWait is the W06-1 hardening probe
// (H1): the rows the HTTP side recorded must be in the database BEFORE the
// teardown blocks in scheduler.Wait. A running dream cycle is not cancelled by
// SIGTERM — it runs on its own context up to dream.cycle_timeout (700 s by
// default) — so Wait can outlast the stop grace period, and a settle that only
// runs after Wait never runs at all: Docker's SIGKILL takes the queue with it.
// The stand-in Wait counts the landed rows at entry, before it returns.
func TestShutdownSettlesHTTPRowsBeforeSchedulerWait(t *testing.T) {
	ref, dsn := testdb.SetupTestDBWithDSN(t)
	pool := slowShutdownPool(t, dsn)
	defer pool.Close()

	const n = 50
	for range n {
		llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-1-http-before-wait", Model: "m", Host: "h"})
	}
	atWait := -1
	sched := drainingScheduler{wait: func() {
		atWait = countLLMLogRows(t, ref, "w06-1-http-before-wait")
	}}

	shutdownSequence(&http.Server{}, sched, pool, func() {}, func() {})

	t.Logf("HTTP-side rows landed when the teardown entered scheduler.Wait: %d of %d", atWait, n)
	if atWait != n {
		t.Errorf("rows landed at scheduler.Wait entry = %d, want %d — a dream cycle outliving the grace period would SIGKILL the rest", atWait, n)
	}
}

// TestShutdownGatewayStopRunsAlongsideTheSettle pins W02-05 review 2 L-b:
// the model gateway's stop runs alongside the first llmlog settle, so the
// bounded part of the shutdown stays 15 s + max(5 s, 5 s) + 5 s = 25 s
// (internal/config minCtxStopGrace) instead of adding a step. Both are made
// to block for their full budget — one transaction holds context_model_keys
// (the final usage flush of a real gateway waits on it) and context_llm_log
// (queued inserts wait) — and the scheduler stand-in releases the locks, so
// the second settle drains at once. Twelve rows are three waves of llmlog's
// four concurrent inserts, each wave held for its 5 s insert deadline: the
// settle has pending work for 15 s, wherever it starts. Side by side the
// sequence takes about 5 s, one after the other about 10 s.
func TestShutdownGatewayStopRunsAlongsideTheSettle(t *testing.T) {
	ref, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	gw := newModelGateway(pool, config.NewStore(cc))

	lock, err := ref.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, `LOCK TABLE context_model_keys, context_llm_log IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}
	stop := startModelGateway(gw)
	// A stamp for the final flush to write: its statement waits on the lock.
	gw.usage.Touch("00000000-0000-4000-8000-0000000002b5")
	const rows = 12
	for range rows {
		llmlog.Record(pool, llmlog.Entry{Pipeline: "w02-05-l-b", Model: "m", Host: "h"})
	}

	var stopTook time.Duration
	timed := func() {
		b := time.Now()
		stop()
		stopTook = time.Since(b)
	}
	sched := drainingScheduler{wait: func() { _ = lock.Rollback(ctx) }}
	begin := time.Now()
	shutdownSequence(&http.Server{}, sched, pool, gw.flushRejections, timed)
	total := time.Since(begin)
	t.Logf("shutdown sequence %s, gateway stop %s (budgets: gateway stop %s, settle %s)",
		total.Round(time.Millisecond), stopTook.Round(time.Millisecond), gatewayStopTimeout, llmlogSettleTimeout)
	// The probe is only a probe while the stop really blocked for its budget.
	if stopTook < gatewayStopTimeout-time.Second {
		t.Fatalf("gateway stop took %s — the lock did not hold the final flush, the probe proves nothing", stopTook)
	}
	// Side by side: about max(stop, settle) = 5 s; in sequence about 10 s.
	// The threshold leaves 3 s for host load.
	if limit := gatewayStopTimeout + llmlogSettleTimeout - 2*time.Second; total >= limit {
		t.Errorf("shutdown sequence %s >= %s — the gateway stop and the first settle ran one after the other", total.Round(time.Millisecond), limit)
	}
}
