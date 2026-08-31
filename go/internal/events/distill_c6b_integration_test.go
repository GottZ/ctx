//go:build integration

// Gate C6-B (event-driven distiller tick), the half that only a live catalog can
// answer: the REAL trg_block_write NOTIFY, routed through the PRODUCTION
// WriteHandler, must wake the distiller arm — long before distill.interval, which
// after this wave is an idle fallback rather than the taktgeber.
//
// RED against Ist (measured, wave report): the arm's only cadence was
// time.After(distill.interval), so the latency between a written compaction
// checkpoint and the tick that sees it was uniform in [0, interval] — mean
// interval/2. On live that interval is 900 s by default (60 s while the E-6
// backfill ran), and the idle tick gap measured on distill_run was exactly
// 900.0 s. WakeBeatsThePoll pins the contrast with an interval of 30 s and a
// budget of 5 s: before this wave nothing ticks inside that budget.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gotmp GOCACHE=/compose/n8n/.gotmp/build \
//	  go test -tags=integration ./internal/events/ -run TestC6B -count=1 -v
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// c6bListener is a dedicated LISTEN connection on ctx_block_write (w053Listen
// pattern): observing the channel directly and feeding the production handler by
// hand is what makes the assertion "the REAL notification woke the arm" instead
// of "some in-process call did".
type c6bListener struct {
	t    *testing.T
	conn *pgxpool.Conn
}

func c6bListen(t *testing.T, pool *pgxpool.Pool) *c6bListener {
	t.Helper()
	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	t.Cleanup(c.Release)
	if _, err := c.Exec(context.Background(), "LISTEN "+channelBlockWrite); err != nil {
		t.Fatalf("LISTEN %s: %v", channelBlockWrite, err)
	}
	return &c6bListener{t: t, conn: c}
}

// pump routes every queued notification through the production WriteHandler
// until the wire stays idle for the given window. It returns how many it moved.
func (l *c6bListener) pump(s *Scheduler, idle time.Duration) int {
	l.t.Helper()
	h := &WriteHandler{scheduler: s}
	n := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), idle)
		nfy, err := l.conn.Conn().WaitForNotification(ctx)
		cancel()
		if err != nil {
			return n
		}
		if err := h.HandleNotification(context.Background(), nfy, nil); err != nil {
			l.t.Fatalf("HandleNotification: %v", err)
		}
		n++
	}
}

// c6bAwaitRow polls distill_run for a row started after `since` and returns its
// started_at. The zero time means the budget elapsed without a tick.
func c6bAwaitRow(t *testing.T, pool *pgxpool.Pool, since time.Time, budget time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var at time.Time
		err := pool.QueryRow(context.Background(),
			`SELECT min(started_at) FROM distill_run WHERE started_at > $1`, since).Scan(&at)
		if err == nil && !at.IsZero() {
			return at
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Time{}
}

// TestC6BEventTick is the rot→grün gate of the wave.
func TestC6BEventTick(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// THE RED PROBE, and it is written to compile against the pre-wave tree so
	// the contrast is a measured latency rather than a missing symbol: an arm
	// whose only cadence is the poll cannot answer a write inside a budget far
	// below distill.interval.
	t.Run("WakeBeatsThePoll", func(t *testing.T) {
		dfTruncate(t, pool)
		c6bClearCheckpoints(t, pool)

		cfg := dfConfig()
		// Far out of reach of the budget below: only an event can tick here.
		cfg.Distill.Interval = 30 * time.Second
		s := dfScheduler(pool, cfg, nil) // nil ⇒ the REAL ctxcheckpoint reader

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		armDone := make(chan struct{})
		go func() { defer close(armDone); s.runDistiller(ctx) }()

		// Let the arm reach its wait: an empty corpus yields no candidate root,
		// so whatever it does at boot writes no journal row (§4.5.3).
		time.Sleep(750 * time.Millisecond)
		if rows := dfRows(t, pool); len(rows) != 0 {
			t.Fatalf("rows = %d before any material, want 0: %+v", len(rows), rows)
		}

		l := c6bListen(t, pool)

		// The production write path: part first, manifest last — the ordering
		// live holds in 292/292 measured compaction groups, which is why a tick
		// that lands mid-burst can never read a torn manifest.
		t0 := time.Now()
		dfSeedCheckpoint(t, pool, dfRoot, time.Now().Add(-2*time.Hour))
		if n := l.pump(s, 2*time.Second); n == 0 {
			t.Fatal("no ctx_block_write notification for a real checkpoint insert — the trigger is not on the wire")
		}

		const budget = 5 * time.Second
		at := c6bAwaitRow(t, pool, t0, budget)
		if at.IsZero() {
			t.Fatalf("no distill_run row within %v of a written checkpoint although distill.interval is %v "+
				"— the arm is poll-driven (RED against Ist: latency = poll interval, live idle gap measured at 900.0 s)",
				budget, cfg.Distill.Interval)
		}
		t.Logf("C6-B end-to-end tick latency (last part/manifest written → tick started): %v", at.Sub(t0))

		cancel()
		select {
		case <-armDone:
		case <-time.After(5 * time.Second):
			t.Fatal("runDistiller did not return after ctx cancel — the wait leaks the goroutine")
		}
	})

	// The boot tick (C6-B decision 3). RED against Ist: the arm used to enter
	// time.After(interval) straight out of the startup sweep, so material a
	// restart left behind waited a full interval — on live 900 s, and a deploy
	// was the slowest path in the system.
	t.Run("BootTickIsImmediate", func(t *testing.T) {
		dfTruncate(t, pool)
		c6bClearCheckpoints(t, pool)
		dfSeedCheckpoint(t, pool, dfRoot, time.Now().Add(-2*time.Hour))

		cfg := dfConfig()
		cfg.Distill.Interval = 30 * time.Second
		s := dfScheduler(pool, cfg, nil)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		t0 := time.Now()
		go s.runDistiller(ctx)

		if at := c6bAwaitRow(t, pool, t0, 5*time.Second); at.IsZero() {
			t.Fatalf("no distill_run row within 5s of boot although material was waiting and distill.interval is %v "+
				"— a restart is blind to exactly the material it is most likely to have left behind", cfg.Distill.Interval)
		}
	})

	// The filter over the REAL predicate (C6-B decision 1): a write that is not
	// checkpoint material must not cost a tick. This is the half the seam cannot
	// answer — that the SQL narrows on the same type/category the reader is built
	// with.
	t.Run("ForeignWriteCostsNoTick", func(t *testing.T) {
		dfTruncate(t, pool)
		c6bClearCheckpoints(t, pool)

		cfg := dfConfig()
		cfg.Distill.Interval = 30 * time.Second
		s := dfScheduler(pool, cfg, nil)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.runDistiller(ctx)
		time.Sleep(750 * time.Millisecond)

		l := c6bListen(t, pool)
		before := s.LastDistillRun()

		// A perfectly ordinary knowledge block: the write every ctx install makes
		// all day, and the one whose volume decides whether an event-driven arm
		// scales to 10M blocks.
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ('test', 'c6b-foreign-write', 'content', $1, 'knowledge')`, dfScope); err != nil {
			t.Fatalf("foreign write: %v", err)
		}
		if n := l.pump(s, 2*time.Second); n == 0 {
			t.Fatal("no ctx_block_write notification for a plain block insert (setup broke)")
		}

		// Well past the debounce, well short of the fallback. A literal rather
		// than the constant so this probe compiles against the pre-wave tree too
		// — the red side of this gate is a tick that DOES happen (waking
		// unconditionally would be one of the rejected designs), and it has to be
		// measurable in the same file.
		time.Sleep(5 * time.Second)
		if got := s.LastDistillRun(); !got.Equal(before) {
			t.Fatalf("the arm ticked at %v for a non-checkpoint write (stamp was %v) — "+
				"at 10M blocks that is a tick per write burst anywhere in the corpus", got, before)
		}
		if rows := dfRows(t, pool); len(rows) != 0 {
			t.Fatalf("rows = %d after a foreign write, want 0: %+v", len(rows), rows)
		}
	})

	// The reconnect gap (C6-B): pgxlisten's backlog entry point must reach the
	// distiller too, or every compaction written while the LISTEN connection was
	// down waits for the idle fallback.
	t.Run("BacklogWakesTheArm", func(t *testing.T) {
		dfTruncate(t, pool)
		c6bClearCheckpoints(t, pool)

		cfg := dfConfig()
		cfg.Distill.Interval = 30 * time.Second
		s := dfScheduler(pool, cfg, nil)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.runDistiller(ctx)
		time.Sleep(750 * time.Millisecond)

		// The material arrives WITHOUT a notification the arm can see — exactly
		// what a disconnect window looks like from the inside.
		dfSeedCheckpoint(t, pool, dfRoot, time.Now().Add(-2*time.Hour))
		t1 := time.Now()

		h := &WriteHandler{scheduler: s}
		if err := h.HandleBacklog(context.Background(), channelBlockWrite, nil); err != nil {
			t.Fatalf("HandleBacklog: %v", err)
		}
		if at := c6bAwaitRow(t, pool, t1, 5*time.Second); at.IsZero() {
			t.Fatalf("no distill_run row within 5s of a reconnect backlog although distill.interval is %v "+
				"— the compactions of the disconnect window are lost until the fallback", cfg.Distill.Interval)
		}
	})
}

// c6bClearCheckpoints removes every checkpoint block so a subtest starts from a
// corpus it fully controls.
func c6bClearCheckpoints(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM context_blocks WHERE type_name = 'checkpoint'`); err != nil {
		t.Fatalf("clear checkpoints: %v", err)
	}
}
