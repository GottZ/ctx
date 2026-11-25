//go:build integration

package handler

// Issue #536 (comment on #548): the report backfill decides at the eviction
// boundary in the snapshot of its decision count. A retention run between the
// handler's own boundary check and that count is answered with the boundary's
// 400, not with a report whose decision part the retention emptied.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// evictBeforeCount runs the write-log retention once, on its own pool, just
// before the backfill's decision count (the statement that reads the
// boundary beside the counts) starts.
type evictBeforeCount struct {
	t    *testing.T
	pool *pgxpool.Pool
	days int
	once sync.Once
	ran  bool
}

func (e *evictBeforeCount) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "write_log_evicted_before") && strings.Contains(d.SQL, "LEFT JOIN") {
		e.once.Do(func() {
			if _, err := store.EvictWriteLog(context.Background(), e.pool, e.days); err != nil {
				e.t.Errorf("retention beside the backfill: %v", err)
			}
			e.ran = true
		})
	}
	return ctx
}

func (e *evictBeforeCount) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestHandleDailyBackfillRefusedAtTheBoundaryOfItsCount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "scope-count-boundary"
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -10)
	windowStart := time.Date(day.Year(), day.Month(), day.Day(), 3, 0, 0, 0, time.UTC).Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO context_write_log (decision, scope, created_at) VALUES ('dream_link', $1, $2)`,
		scope, windowStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// No boundary yet, so the handler's check passes; the retention at five
	// days runs between that check and the count and deletes the decision.
	hook := &evictBeforeCount{t: t, pool: pool, days: 5}
	pcfg := pool.Config()
	pcfg.ConnConfig.Tracer = hook
	traced, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	cfg := &swapStore{}
	h := NewSynthesizeHandler(traced, horizonEchoBackend(t), blocktype.NewRegistry(), newHandlerRecordingAdmitter(t), cfg)
	code, body := horizonPost(t, h, cfg, scope, day.Format("2006-01-02"), 0)
	if !hook.ran {
		t.Fatal("the decision count of the backfill was not traced")
	}
	if msg, _ := body["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "eviction boundary") {
		t.Errorf("backfill of %s, the retention ran between the check and the count: status %d (%v), want 400 naming the eviction boundary",
			day.Format("2006-01-02"), code, body)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE scope = $1`, scope).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d reports written for the refused window", n)
	}
}
