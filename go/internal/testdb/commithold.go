//go:build integration

package testdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommitHoldBound bounds every wait of a CommitHold: how long a held COMMIT
// waits for Release before it goes on by itself — which fails the test, see
// HoldCommitPool — and how long Wait waits for a transaction to reach its
// COMMIT.
const CommitHoldBound = 30 * time.Second

// CommitHold holds one transaction at its COMMIT, with every lock it took
// still held: the first transaction on its pool that runs a statement (or a
// batch with a statement) for which match answers true waits before it sends
// its COMMIT until Release; one that rolls back is not held. Later
// transactions pass. match must name statements that run inside a
// transaction: an autocommit statement arms its connection for the next
// transaction that commits on it. It is a pgx tracer, so
// the statements run are the production ones, unchanged; only the COMMIT is
// delayed, on the client side.
type CommitHold struct {
	match func(sql string) bool

	mu    sync.Mutex
	armed map[*pgx.Conn]bool
	used  bool
	pid   uint32

	held     chan struct{}
	release  chan struct{}
	relOnce  sync.Once
	heldOnce sync.Once
	// expired is set when the held COMMIT went on after CommitHoldBound
	// without Release.
	expired atomic.Bool
}

// HoldCommitPool returns a pool on the database at dsn whose first
// transaction matching match is held at its COMMIT (CommitHold). The hold is
// released and the pool closed when the test ends. A held COMMIT that went on
// by itself after CommitHoldBound, before Release, fails the test then: the
// test's waits ran against a transaction that was no longer held.
func HoldCommitPool(t *testing.T, dsn string, match func(sql string) bool) (*pgxpool.Pool, *CommitHold) {
	t.Helper()
	h := &CommitHold{
		match:   match,
		armed:   map[*pgx.Conn]bool{},
		held:    make(chan struct{}),
		release: make(chan struct{}),
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("commit hold: parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = h
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("commit hold: pool: %v", err)
	}
	t.Cleanup(func() {
		h.Release()
		pool.Close()
		if h.expired.Load() {
			t.Errorf("commit hold: the held COMMIT went on by itself after %s, before Release", CommitHoldBound)
		}
	})
	return pool, h
}

// Wait waits until the matching transaction is held at its COMMIT and returns
// the backend pid of its connection; the test fails when none is held within
// CommitHoldBound.
func (h *CommitHold) Wait(t *testing.T) uint32 {
	t.Helper()
	select {
	case <-h.held:
	case <-time.After(CommitHoldBound):
		t.Fatalf("commit hold: no matching transaction reached its COMMIT within %s", CommitHoldBound)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pid
}

// Release lets the held COMMIT go (and any later one through at once).
func (h *CommitHold) Release() { h.relOnce.Do(func() { close(h.release) }) }

func (h *CommitHold) arm(conn *pgx.Conn, sql string) {
	if !h.match(sql) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.used {
		h.armed[conn] = true
	}
}

// TraceQueryStart arms the connection on a matching statement and holds its
// COMMIT once armed.
func (h *CommitHold) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "rollback") {
		// A matching transaction that rolls back is not the one to hold, and
		// the connection's next transaction is another one.
		h.mu.Lock()
		delete(h.armed, conn)
		h.mu.Unlock()
		return ctx
	}
	if !strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		h.arm(conn, data.SQL)
		return ctx
	}
	h.mu.Lock()
	hold := h.armed[conn] && !h.used
	if hold {
		h.used = true
		h.pid = conn.PgConn().PID()
		delete(h.armed, conn)
	}
	h.mu.Unlock()
	if hold {
		h.heldOnce.Do(func() { close(h.held) })
		select {
		case <-h.release:
		case <-time.After(CommitHoldBound):
			h.expired.Store(true)
		}
	}
	return ctx
}

// TraceQueryEnd is part of pgx.QueryTracer.
func (h *CommitHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart arms the connection when a statement of the batch matches:
// the statements of a batch do not pass TraceQueryStart.
func (h *CommitHold) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, q := range data.Batch.QueuedQueries {
		h.arm(conn, q.SQL)
	}
	return ctx
}

// TraceBatchQuery is part of pgx.BatchTracer.
func (h *CommitHold) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

// TraceBatchEnd is part of pgx.BatchTracer.
func (h *CommitHold) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// WaitBlockedBy waits until a backend waits on a lock pid holds
// (pg_blocking_pids) and, when event is not "", until that backend's
// wait_event is event ("advisory", "transactionid", ...); it returns the
// wait_event it saw. A backend's wait_event is not set in one step with its
// entry in pg_blocking_pids, so an empty wait_event is read again, within the
// same bound; a waiter on another event fails the test at once. The test
// fails as well when no backend waits within CommitHoldBound, and when done
// is closed first: the waiter it stands for finished without waiting.
func WaitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid uint32, done <-chan struct{}, what, event string) string {
	t.Helper()
	deadline := time.Now().Add(CommitHoldBound)
	seen := "none"
	for {
		select {
		case <-done:
			t.Fatalf("%s finished without waiting on the held transaction (pid %d)", what, pid)
		default:
		}
		var got string
		err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(wait_event, '') FROM pg_stat_activity
			  WHERE $1::int = ANY(pg_blocking_pids(pid))
			  ORDER BY COALESCE(wait_event, '') = $2 DESC LIMIT 1`, int64(pid), event).Scan(&got)
		switch {
		case err == nil && (event == "" || got == event):
			return got
		case err == nil && got != "":
			t.Fatalf("%s waits on the held transaction (pid %d) on wait_event %q, want %q", what, pid, got, event)
		case err == nil:
			seen = "a waiter with no wait_event yet"
		case !errors.Is(err, pgx.ErrNoRows):
			t.Fatalf("poll lock wait: %v", err)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s never waited on the held transaction (pid %d) on wait_event %q within %s (last seen: %s)", what, pid, event, CommitHoldBound, seen)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
