//go:build integration

// Rot-Gate for the tracking issue, force-push.me/damienmoon/ctx/issues/1: the query-path pre-search backfill
// (backfillPending) had a COUNT cap (embed_backfill.sync_cap) but no TIME
// budget. One block whose embed could not finish inside the caller's
// deadline consumed the entire deadline (live: a ~10k-token block against a
// CPU embed backend at ~23 tok/s needs ~7 min, the CLI client's deadline is
// 120 s), the question's own embed was never admitted ("embedcache:
// admission: context canceled") and the query answered 500. The failure was
// memoized as class `wire` — backend-down semantics for a backend that was
// healthy, just slow for this caller — so every query that landed after the
// backoff lapsed repeated the burn.
//
// Rot and Grün run through the SAME production function. SyncBudget=0 is BY
// CONSTRUCTION the historical shape (no deadline on the wire call, no
// budget check at the loop head); SyncBudget>0 is the fix.
//
//	SyncBudget=0 (Ist): the slow block holds the loop for its full embed time.
//	SyncBudget=300ms (Soll): the loop returns inside the budget, the block
//	  is memoized caller_timeout with next_attempt_at <= now(), the query
//	  path never picks it again, the background arm's predicate still does.
//
// Run: `go test -tags=integration ./internal/handler/ -run TestBackfillPending_SyncBudget -count=1 -v`.
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// slowEmbedServer answers like fakeEmbedServer, but only after `delay`, or
// at once with the request's cancellation if the client goes away first.
func slowEmbedServer(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, canceled atomic.Int32
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Drain the body first: net/http starts the background read that
		// turns a client disconnect into r.Context() cancellation only once
		// the request body has been consumed (registerOnHitEOF in
		// readRequest). Without this the handler never sees the cancel.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			canceled.Add(1)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &canceled
}

func TestBackfillPending_SyncBudget_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const title = "syncbudget-slow-block"
	seed := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
			 VALUES ('learnings', $1, 'body of the sync-budget fixture block', 'private', now(), now())`, title); err != nil {
			t.Fatalf("seed block: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM context_embed_failures WHERE block_id IN (SELECT id FROM context_blocks WHERE title = $1)`, title)
			_, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE title = $1`, title)
		})
	}
	readMemo := func(t *testing.T) (class string, dueNow bool, attempts int) {
		t.Helper()
		err := pool.QueryRow(ctx,
			`SELECT f.last_class, f.next_attempt_at <= now(), f.attempts
			 FROM context_embed_failures f JOIN context_blocks cb ON cb.id = f.block_id
			 WHERE cb.title = $1 AND f.migration_id IS NULL`, title).Scan(&class, &dueNow, &attempts)
		if err != nil {
			t.Fatalf("read memo: %v", err)
		}
		return class, dueNow, attempts
	}
	newHandler := func(t *testing.T, delay time.Duration) (*QueryHandler, *atomic.Int32, *atomic.Int32) {
		t.Helper()
		srv, hits, canceled := slowEmbedServer(t, delay)
		st := &countingStore{}
		st.cfg.Store(snapshotTestConfig())
		h := NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		return h, hits, canceled
	}

	// Rot: SyncBudget=0 is the historical shape — the loop waits for the
	// slow backend however long it takes. Nothing bounds the request's time.
	t.Run("red_unbudgeted_waits_for_the_slow_backend", func(t *testing.T) {
		seed(t)
		h, hits, _ := newHandler(t, 1500*time.Millisecond)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 4, SyncBudget: 0, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}

		started := time.Now()
		got := h.backfillPending(ctx, nil, "private", h.embedAdmission(), cfg)
		elapsed := time.Since(started)
		if got != 1 {
			t.Fatalf("RED premise: backfilled = %d, want 1 (unbudgeted loop embeds the slow block)", got)
		}
		if elapsed < 1400*time.Millisecond {
			t.Fatalf("RED premise: returned after %v, want >= 1.4 s (the loop waited for the backend)", elapsed)
		}
		if hits.Load() != 1 {
			t.Errorf("wire hits = %d, want 1", hits.Load())
		}
		t.Logf("RED confirmed: SyncBudget=0 held the request for %v on one slow block", elapsed)
	})

	// Grün: SyncBudget bounds the loop. The slow block is cut at the budget,
	// memoized caller_timeout and due for the background arm at once; the
	// parent context is untouched, so the question embed keeps its time.
	t.Run("budget_cuts_the_slow_block_and_hands_it_to_the_arm", func(t *testing.T) {
		seed(t)
		h, hits, canceled := newHandler(t, 5*time.Second)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 4, SyncBudget: 300 * time.Millisecond, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}
		reqCtx, cancelReq := context.WithCancel(ctx)
		defer cancelReq()

		started := time.Now()
		got := h.backfillPending(reqCtx, nil, "private", h.embedAdmission(), cfg)
		elapsed := time.Since(started)
		if got != 0 {
			t.Fatalf("backfilled = %d, want 0 (the slow block must not count)", got)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("returned after %v, want well inside the 5 s backend delay (budget 300 ms)", elapsed)
		}
		if reqCtx.Err() != nil {
			t.Fatalf("request context is dead (%v); the budget must cut only the embed call", reqCtx.Err())
		}
		if hits.Load() != 1 {
			t.Errorf("wire hits = %d, want 1", hits.Load())
		}
		// The backend saw the cancellation: no 7-minute embed keeps burning CPU
		// for a caller that is gone (the live task was cancelled at 30 %).
		deadline := time.Now().Add(2 * time.Second)
		for canceled.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if canceled.Load() != 1 {
			t.Errorf("backend saw %d cancellations, want 1", canceled.Load())
		}

		class, dueNow, attempts := readMemo(t)
		if class != string(store.EmbedFailureCallerTimeout) {
			t.Errorf("memo class = %q, want %q", class, store.EmbedFailureCallerTimeout)
		}
		if !dueNow {
			t.Errorf("memo next_attempt_at is in the future; a caller_timeout must be due for the background arm at once")
		}
		if attempts != 1 {
			t.Errorf("memo attempts = %d, want 1", attempts)
		}

		// Pfad A never picks it again, however much budget it has.
		cfg.EmbedBackfill.SyncBudget = 10 * time.Second
		h2, hits2, _ := newHandler(t, 0)
		if again := h2.backfillPending(ctx, nil, "private", h2.embedAdmission(), cfg); again != 0 {
			t.Fatalf("second backfillPending embedded %d, want 0 (caller_timeout blocks belong to the background arm)", again)
		}
		if hits2.Load() != 0 {
			t.Errorf("second handler made %d wire calls, want 0", hits2.Load())
		}

		// The background arm's predicate (the shared one) still sees it.
		var visibleToArm int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks
			 WHERE embedding IS NULL AND NOT is_archived AND title = $1`+store.EmbedFailureExcludedPredicate, title).
			Scan(&visibleToArm); err != nil {
			t.Fatalf("arm predicate: %v", err)
		}
		if visibleToArm != 1 {
			t.Errorf("background predicate sees %d block(s), want 1 (caller_timeout is due at once for the arm)", visibleToArm)
		}
	})

	// A request that is cancelled mid-embed (client gone) is the same case
	// for the block: caller_timeout, not wire — the backend did nothing wrong.
	t.Run("request_cancel_is_caller_timeout_not_wire", func(t *testing.T) {
		seed(t)
		h, _, _ := newHandler(t, 5*time.Second)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 4, SyncBudget: 10 * time.Second, MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}
		reqCtx, cancelReq := context.WithCancel(ctx)
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancelReq()
		}()
		if got := h.backfillPending(reqCtx, nil, "private", h.embedAdmission(), cfg); got != 0 {
			t.Fatalf("backfilled = %d, want 0", got)
		}
		class, dueNow, _ := readMemo(t)
		if class != string(store.EmbedFailureCallerTimeout) {
			t.Errorf("memo class = %q, want %q", class, store.EmbedFailureCallerTimeout)
		}
		if !dueNow {
			t.Errorf("memo must be due at once for the background arm")
		}
	})
}
