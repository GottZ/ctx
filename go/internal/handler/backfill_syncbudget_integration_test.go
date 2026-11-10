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
		got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg)
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
		// Interval > 0: the background arm runs, so the query path hands
		// the block off to it (with the arm off it would re-pick it — see
		// arm_off_query_path_repicks_a_caller_timeout_block below).
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 4, SyncBudget: 300 * time.Millisecond, Interval: 15 * time.Second,
			MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}
		reqCtx, cancelReq := context.WithCancel(ctx)
		defer cancelReq()

		started := time.Now()
		got := h.backfillPending(reqCtx, nil, []string{"private"}, h.embedAdmission(), cfg)
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
		if again := h2.backfillPending(ctx, nil, []string{"private"}, h2.embedAdmission(), cfg); again != 0 {
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
		if got := h.backfillPending(reqCtx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 0 {
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

	// Review finding M1 (a): sync_budget=0 is the explicit opt-out to the
	// pre-budget shape, where the wire call runs under the bare request
	// context. A client that goes away mid-embed must keep the pre-PR class
	// wire (exponential backoff, re-picked once it lapses) — caller_timeout
	// means "handed to the arm" and is a budget statement, so without a
	// budget there is nothing to hand off.
	t.Run("budget_zero_request_cancel_stays_wire", func(t *testing.T) {
		seed(t)
		h, _, _ := newHandler(t, 5*time.Second)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 4, SyncBudget: 0, Interval: 15 * time.Second,
			MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}
		reqCtx, cancelReq := context.WithCancel(ctx)
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancelReq()
		}()
		if got := h.backfillPending(reqCtx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 0 {
			t.Fatalf("backfilled = %d, want 0", got)
		}
		class, dueNow, _ := readMemo(t)
		if class != string(store.EmbedFailureWire) {
			t.Errorf("memo class = %q, want %q (sync_budget=0 is the pre-budget behavior)", class, store.EmbedFailureWire)
		}
		if dueNow {
			t.Errorf("memo is due at once; a wire memo sits on the backoff curve (backoff_base = 1 min here)")
		}
	})

	// Review finding M1 (b): the query path leaves a caller_timeout block to
	// the background arm only while that arm runs. With
	// embed_backfill.interval=0 (arm off) and Dream off nothing else would
	// ever embed it, so the query path picks it up again (the memo is due at
	// once), with the plain predicate, once nothing else is pending.
	t.Run("arm_off_query_path_repicks_a_caller_timeout_block", func(t *testing.T) {
		seed(t)
		var blockID string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&blockID); err != nil {
			t.Fatalf("read block id: %v", err)
		}
		if err := store.RecordEmbedFailure(ctx, pool, blockID, store.EmbedFailureCallerTimeout,
			store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "context deadline exceeded"),
			time.Minute, time.Hour); err != nil {
			t.Fatalf("record caller_timeout memo: %v", err)
		}

		h, hits, _ := newHandler(t, 0)
		cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
			SyncCap: 4, SyncBudget: 10 * time.Second, Interval: 0,
			MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
		}}
		if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 1 {
			t.Fatalf("backfilled = %d, want 1 (arm off: the query path must not leave the block to nobody)", got)
		}
		if hits.Load() != 1 {
			t.Errorf("wire hits = %d, want 1", hits.Load())
		}
	})

	// Review of #214: with the arm off the query path is the only embedder
	// of a caller_timeout block, but a block that overran the budget must not
	// be what every request picks first, or one block too slow for the budget
	// holds everything behind it for good (its memo is due at once and the
	// loop ends on the failure). It goes last: a request takes it only when
	// nothing else is pending. Two rows, the overrun block on either side of
	// a plain pending one, so the order of the pick cannot decide the result.
	t.Run("arm_off_a_block_that_overran_the_budget_goes_last", func(t *testing.T) {
		const overran, plain = "syncbudget-overran-block", "syncbudget-plain-block"
		for _, tc := range []struct {
			name                    string
			overranAgeMin, plainAge int
		}{
			{"overran_block_is_the_newer", 1, 2},
			{"overran_block_is_the_older", 2, 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for title, ageMin := range map[string]int{overran: tc.overranAgeMin, plain: tc.plainAge} {
					if _, err := pool.Exec(ctx,
						`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
						 VALUES ('learnings', $1, 'body of the sync-budget fixture block ' || $1, 'private',
						         now() - $2::int * interval '1 minute', now())`, title, ageMin); err != nil {
						t.Fatalf("seed block %q: %v", title, err)
					}
				}
				t.Cleanup(func() {
					_, _ = pool.Exec(ctx, `DELETE FROM context_embed_failures WHERE block_id IN (SELECT id FROM context_blocks WHERE title = ANY($1))`, []string{overran, plain})
					_, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE title = ANY($1)`, []string{overran, plain})
				})
				var blockID string
				if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, overran).Scan(&blockID); err != nil {
					t.Fatalf("read block id: %v", err)
				}
				if err := store.RecordEmbedFailure(ctx, pool, blockID, store.EmbedFailureCallerTimeout,
					store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "context deadline exceeded"),
					time.Minute, time.Hour); err != nil {
					t.Fatalf("record caller_timeout memo: %v", err)
				}
				embedded := func(title string) bool {
					t.Helper()
					var present bool
					if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL FROM context_blocks WHERE title = $1`, title).Scan(&present); err != nil {
						t.Fatalf("read embedding state of %q: %v", title, err)
					}
					return present
				}

				h, _, _ := newHandler(t, 0)
				cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
					SyncCap: 1, SyncBudget: 10 * time.Second, Interval: 0,
					MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
				}}
				if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 1 {
					t.Fatalf("first request backfilled %d, want 1", got)
				}
				if !embedded(plain) || embedded(overran) {
					t.Fatalf("first request: plain block embedded=%v, overrun block embedded=%v, want the plain one only (the overrun block goes last)",
						embedded(plain), embedded(overran))
				}
				// Nothing else is pending now: the arm is off, so this path
				// is what embeds the overrun block.
				if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 1 {
					t.Fatalf("second request backfilled %d, want 1 (arm off: the query path must not leave the block to nobody)", got)
				}
				if !embedded(overran) {
					t.Error("second request: the overrun block is still pending with nothing else in the queue")
				}
			})
		}
	})
}
