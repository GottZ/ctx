//go:build integration

// Rot-Gate for the tracking issue, force-push.me/damienmoon/ctx/issues/1, scheduler half: the embed-backfill
// arm (backfillOneEmbedding) was called ONLY at the top of runDreamLoop, so
// with dream.enabled=false — the registry default — no goroutine ever
// embedded a pending block in the background. Live: a block stayed pending
// for two hours while the scheduler logged guard and digest ticks only; the
// query path was the sole embedder, inside its caller's deadline.
//
// Rot is the premise, stated against the production wiring: a scheduler
// with DreamEnabled=false starts no dream worker, and nothing else in Run
// called backfillOneEmbedding before this wave. Grün is the dedicated arm
// (runEmbedBackfillLoop): started regardless of Dream, it embeds the pending
// block within a few intervals, and a caller_timeout memo (the query path's
// hand-off) does not hold it back.
//
// Run: `go test -tags=integration ./internal/events/ -run TestEmbedBackfillArm -count=1 -v`.
package events

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestEmbedBackfillArm_RunsWithoutDream_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	srv := headOfLineEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedPoolRow("embed-a", srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings()) // empty policy: pass-through
	t.Cleanup(d.Close)

	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		Interval:    100 * time.Millisecond,
		MaxTokens:   0,
		BackoffBase: time.Hour,
		BackoffCap:  24 * time.Hour,
	}}
	// DreamEnabled=false: the RED premise — before this wave, Run started no
	// goroutine that could reach backfillOneEmbedding in this configuration.
	s := NewScheduler(pool, config.NewStore(cfg), bpool, StartupConfig{DreamEnabled: false})
	s.SetDispatcher(d)

	waitEmbedded := func(t *testing.T, within time.Duration) bool {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if pendingCount(t, pool) == 0 {
				return true
			}
			time.Sleep(25 * time.Millisecond)
		}
		return false
	}

	t.Run("arm_embeds_a_pending_block_with_dream_off", func(t *testing.T) {
		seedPendingBlock(t, pool, "arm-pending-normal", time.Hour)
		defer clearBlocks(t, pool)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		if !waitEmbedded(t, 5*time.Second) {
			t.Fatalf("block still pending after 5 s: the dedicated arm did not embed it (pending=%d)", pendingCount(t, pool))
		}
	})

	t.Run("arm_picks_a_caller_timeout_block_at_once", func(t *testing.T) {
		seedPendingBlock(t, pool, "arm-handed-off", time.Hour)
		defer clearBlocks(t, pool)

		// The query path's hand-off: a caller_timeout memo, due now.
		var blockID string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = 'arm-handed-off'`).Scan(&blockID); err != nil {
			t.Fatalf("read block id: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, blockID, store.EmbedFailureCallerTimeout,
			store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "context deadline exceeded"),
			cfg.EmbedBackfill.BackoffBase, cfg.EmbedBackfill.BackoffCap); err != nil {
			t.Fatalf("record caller_timeout memo: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM context_embed_failures WHERE block_id = $1`, blockID) })

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go s.runEmbedBackfillLoop(armCtx)

		if !waitEmbedded(t, 5*time.Second) {
			t.Fatalf("caller_timeout block still pending after 5 s: the arm must pick it at once, not after a backoff")
		}
	})

	t.Run("interval_zero_turns_the_arm_off", func(t *testing.T) {
		seedPendingBlock(t, pool, "arm-off-pending", time.Hour)
		defer clearBlocks(t, pool)

		off := *cfg
		off.EmbedBackfill.Interval = 0
		sOff := NewScheduler(pool, config.NewStore(&off), bpool, StartupConfig{DreamEnabled: false})
		sOff.SetDispatcher(d)

		armCtx, stop := context.WithCancel(ctx)
		defer stop()
		go sOff.runEmbedBackfillLoop(armCtx)

		time.Sleep(600 * time.Millisecond)
		if got := pendingCount(t, pool); got != 1 {
			t.Fatalf("pending = %d, want 1 (Interval=0 is the explicit off switch)", got)
		}
	})
}
