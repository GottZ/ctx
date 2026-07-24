//go:build integration

// Rot-Gate 2 for the Evokoa-Clean-Room-Plan Achse 04 W04-2 (design/04 §4.3
// "Pfad-A-Kappung", §7 wave row W04-2): the query-path pre-search backfill
// loop (backfillPending) had NO CAP — query.go:1090-1137 verified: a bare
// `for {}` that runs until the pending queue is empty or an error occurs.
// After a large rest-transient (e.g. post-cutover, or a batch ingest that
// outran the scheduler arm) the FIRST interactive query to hit an empty
// embed-cache pays the ENTIRE nachzug synchronously, inside its own request
// latency.
//
// Rot/Grün both run through the SAME production function
// (h.backfillPending) rather than a hand-duplicated old-shape loop, because
// the fix is additive and behavior-preserving at the sentinel: SyncCap<=0 is
// BY CONSTRUCTION the historical unbounded loop (no cap check at all, same
// query/embed/store sequence) — it is not a simulation of the old code, it
// IS the code path an operator gets by explicitly disabling the cap. So:
//
//	SyncCap=0 (Ist / historical default before this wave shipped a cap)
//	  → all N pending blocks embedded synchronously in one call.
//	SyncCap=4 (Soll, this wave's default)
//	  → exactly 4 embedded, the rest left pending for Pfad B.
//
// Run: go test -tags=integration ./internal/handler/ -run TestBackfillPending_SyncCap_Integration -count=1 -v
package handler

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackfillPending_SyncCap_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const totalPending = 6
	const syncCap = 4

	seed := func(t *testing.T, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			title := fmt.Sprintf("synccap-block-%d", i)
			ageMinutes := fmt.Sprintf("%d minutes", n-i)
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
				 VALUES ('learnings', $1, 'body of the synccap fixture block', 'private',
				         now() - $2::interval, now())`,
				title, ageMinutes); err != nil {
				t.Fatalf("seed block %d: %v", i, err)
			}
		}
	}

	pendingCount := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE embedding IS NULL AND NOT is_archived`).Scan(&n); err != nil {
			t.Fatalf("pending count: %v", err)
		}
		return n
	}

	newHandler := func(t *testing.T) (*QueryHandler, *atomic.Int32) {
		t.Helper()
		srv, hits := fakeEmbedServer(t)
		st := &countingStore{}
		st.cfg.Store(snapshotTestConfig())
		h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		return h, hits
	}

	// Rot: SyncCap<=0 is the historical unbounded shape — every pending
	// block gets embedded synchronously in ONE backfillPending call.
	t.Run("red_uncapped_embeds_everything", func(t *testing.T) {
		seed(t, totalPending)
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE title LIKE 'synccap-block-%'`) })

		h, hits := newHandler(t)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 0, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}

		got := h.backfillPending(ctx, nil, "private", h.embedAdmission(), cfg)
		if got != totalPending {
			t.Fatalf("RED premise: backfilled = %d, want %d (SyncCap=0 must embed EVERY pending block)", got, totalPending)
		}
		if int(hits.Load()) != totalPending {
			t.Errorf("wire hits = %d, want %d", hits.Load(), totalPending)
		}
		if left := pendingCount(t); left != 0 {
			t.Errorf("pending left = %d, want 0 (uncapped loop drains the whole queue)", left)
		}
		t.Logf("RED confirmed: SyncCap=0 embedded all %d pending blocks in one synchronous call (%d wire hits)", got, hits.Load())
	})

	// Grün: SyncCap=4 stops after 4, leaving the rest for Pfad B.
	t.Run("fixed_cap_stops_at_four", func(t *testing.T) {
		seed(t, totalPending)
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE title LIKE 'synccap-block-%'`) })

		h, hits := newHandler(t)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: syncCap, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}

		got := h.backfillPending(ctx, nil, "private", h.embedAdmission(), cfg)
		if got != syncCap {
			t.Fatalf("backfilled = %d, want %d (SyncCap must bound the synchronous work)", got, syncCap)
		}
		if int(hits.Load()) != syncCap {
			t.Errorf("wire hits = %d, want %d", hits.Load(), syncCap)
		}
		if left := pendingCount(t); left != totalPending-syncCap {
			t.Errorf("pending left = %d, want %d (rest deferred to Pfad B, not lost)", left, totalPending-syncCap)
		}
	})
}
