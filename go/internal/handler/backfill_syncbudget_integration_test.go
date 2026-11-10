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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	// ever embed it, so the query path picks it up again. Second review of
	// #214: it does so on the backoff curve, counted from the cut, and not
	// with the very next query, or a block that cannot fit the budget costs
	// every query its whole budget.
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
		// Cut a moment ago, one attempt: backoff_base (1 min) has not passed.
		if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 0 {
			t.Fatalf("backfilled = %d right after the cut, want 0 (arm off: an overrun block waits out its backoff)", got)
		}
		if hits.Load() != 0 {
			t.Errorf("wire hits = %d right after the cut, want 0", hits.Load())
		}
		// The cut is two minutes back: the backoff of one attempt has passed.
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_failures SET next_attempt_at = now() - interval '2 minutes' WHERE block_id = $1`, blockID); err != nil {
			t.Fatalf("age the memo: %v", err)
		}
		if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 1 {
			t.Fatalf("backfilled = %d after the backoff, want 1 (arm off: the query path must not leave the block to nobody)", got)
		}
		if hits.Load() != 1 {
			t.Errorf("wire hits = %d, want 1", hits.Load())
		}
	})

	// Review of #214: with the arm off the query path is the only embedder
	// of a caller_timeout block, but a block that overran the budget must not
	// be what every request picks first, or one block too slow for the budget
	// holds everything behind it for good (a failed embed ends the loop).
	// Second review: "after everything else" is no answer either, because
	// under steady writes there is always something else. The block sits out
	// a backoff counted from its cut (backoff_base * 2^(attempts-1), capped)
	// and then stands in its place in the order again. Two rows, the overrun
	// block on either side of a plain pending one, so the order of the pick
	// cannot decide the result of the first two requests.
	t.Run("arm_off_a_block_that_overran_the_budget_waits_out_its_backoff", func(t *testing.T) {
		const overran, plain, late = "syncbudget-overran-block", "syncbudget-plain-block", "syncbudget-late-block"
		for _, tc := range []struct {
			name                    string
			overranAgeMin, plainAge int
		}{
			{"overran_block_is_the_newer", 2, 3},
			{"overran_block_is_the_older", 3, 2},
		} {
			t.Run(tc.name, func(t *testing.T) {
				insert := func(title string, ageMin int) {
					t.Helper()
					if _, err := pool.Exec(ctx,
						`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
						 VALUES ('learnings', $1, 'body of the sync-budget fixture block ' || $1, 'private',
						         now() - $2::int * interval '1 minute', now())`, title, ageMin); err != nil {
						t.Fatalf("seed block %q: %v", title, err)
					}
				}
				insert(overran, tc.overranAgeMin)
				insert(plain, tc.plainAge)
				t.Cleanup(func() {
					_, _ = pool.Exec(ctx, `DELETE FROM context_embed_failures WHERE block_id IN (SELECT id FROM context_blocks WHERE title = ANY($1))`, []string{overran, plain, late})
					_, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE title = ANY($1)`, []string{overran, plain, late})
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

				h, hits, _ := newHandler(t, 0)
				cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
					SyncCap: 1, SyncBudget: 10 * time.Second, Interval: 0,
					MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
				}}
				if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 1 {
					t.Fatalf("first request backfilled %d, want 1", got)
				}
				if !embedded(plain) || embedded(overran) {
					t.Fatalf("first request: plain block embedded=%v, overrun block embedded=%v, want the plain one only (the overrun block is inside its backoff)",
						embedded(plain), embedded(overran))
				}
				// Nothing else is pending, and the backoff has not passed: the
				// request leaves the block alone and makes no wire call.
				if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 0 {
					t.Fatalf("second request backfilled %d, want 0 (the overrun block is inside its backoff; retrying it with every query costs each one its budget)", got)
				}
				if hits.Load() != 1 {
					t.Errorf("wire hits = %d after two requests, want 1 (the plain block only)", hits.Load())
				}
				// The backoff has passed and a newer block is pending: the one
				// pick takes the overrun block, which is the older of the two.
				if _, err := pool.Exec(ctx,
					`UPDATE context_embed_failures SET next_attempt_at = now() - interval '2 minutes' WHERE block_id = $1`, blockID); err != nil {
					t.Fatalf("age the memo: %v", err)
				}
				insert(late, 0)
				if got := h.backfillPending(ctx, nil, []string{"private"}, h.embedAdmission(), cfg); got != 1 {
					t.Fatalf("third request backfilled %d, want 1", got)
				}
				if !embedded(overran) || embedded(late) {
					t.Errorf("third request: overrun block embedded=%v, newer block embedded=%v, want the overrun one (after its backoff it stands in its place, ahead of a newer block)",
						embedded(overran), embedded(late))
				}
			})
		}
	})
}

// pacedEmbedServer answers like slowEmbedServer, with a delay per request that
// pace works out from the request body (the embed text is in it), and counts
// the requests whose body contains a given marker.
func pacedEmbedServer(t *testing.T, pace func(body string) time.Duration) (*httptest.Server, func(marker string) int) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []string
	)
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the whole body is also what lets net/http report a client
		// that goes away (see slowEmbedServer).
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		select {
		case <-time.After(pace(string(raw))):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv, func(marker string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, b := range bodies {
			if strings.Contains(b, marker) {
				n++
			}
		}
		return n
	}
}

// armOffFixture is one database of its own (with the arm off the old end of
// the pick is not bound to a scope, so the probes must not share pending
// rows) and a query handler in front of a paced embed backend.
type armOffFixture struct {
	pool *pgxpool.Pool
	h    *QueryHandler
	hits func(marker string) int
}

func armOffSetup(t *testing.T, pace func(body string) time.Duration) *armOffFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	srv, hits := pacedEmbedServer(t, pace)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	return &armOffFixture{
		pool: pool,
		h:    NewQueryHandler(pool, st, embedPool(srv.URL), nil, blocktype.NewRegistry(), snapshotTestAdmitter(t)),
		hits: hits,
	}
}

// seed inserts one pending block in `private`, created ageMin minutes ago, and
// returns its id.
func (f *armOffFixture) seed(t *testing.T, title string, ageMin int) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('learnings', $1, 'body of the arm-off fixture block ' || $1, 'private',
		         now() - $2::int * interval '1 minute', now()) RETURNING id::text`, title, ageMin).Scan(&id); err != nil {
		t.Fatalf("seed block %q: %v", title, err)
	}
	return id
}

// overran gives the block a caller_timeout memo through the production
// writer, with the cut an hour back: one attempt, so its backoff (backoff_base
// of the probes is a minute) has long passed.
func (f *armOffFixture) overran(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	if err := store.RecordEmbedFailure(ctx, f.pool, id, store.EmbedFailureCallerTimeout,
		store.NormalizeEmbedError(store.EmbedFailureCallerTimeout, "context deadline exceeded"),
		time.Minute, time.Hour); err != nil {
		t.Fatalf("record caller_timeout memo: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE context_embed_failures SET next_attempt_at = now() - interval '1 hour' WHERE block_id = $1`, id); err != nil {
		t.Fatalf("age the memo: %v", err)
	}
}

func (f *armOffFixture) embedded(t *testing.T, id string) bool {
	t.Helper()
	var present bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE id = $1`, id).Scan(&present); err != nil {
		t.Fatalf("read embedding state of %s: %v", id, err)
	}
	return present
}

// memo returns the class and the attempts of the block's failure memo ("" and
// 0 when it has none).
func (f *armOffFixture) memo(t *testing.T, id string) (class string, attempts int) {
	t.Helper()
	err := f.pool.QueryRow(context.Background(),
		`SELECT last_class, attempts FROM context_embed_failures WHERE block_id = $1 AND migration_id IS NULL`, id).
		Scan(&class, &attempts)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read memo of %s: %v", id, err)
	}
	return class, attempts
}

// run is one backfillPending call with the arm off, as a caller that reads
// `private`.
func (f *armOffFixture) run(syncCap int, budget time.Duration) int {
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: syncCap, SyncBudget: budget, Interval: 0,
		MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}
	return f.h.backfillPending(context.Background(), nil, []string{"private"}, f.h.embedAdmission(), cfg)
}

// TestBackfillPending_ArmOff_Integration holds the probes of the second review
// of #214. With the background arm off (embed_backfill.interval=0) the inline
// loop is the only embedder, and three things kept blocks from ever being
// embedded although each fits the budget, or let one block that does not fit
// hold others. A block that once overran the budget was picked only when a
// request found nothing else pending anywhere, which under steady writes is
// never. Among such blocks the oldest was picked first by every request, so
// one that never fits held the others behind it. And the old end had the
// second pick, and with it only what the caller's fresh block had left of the
// budget, so with embeds that take more than half the budget every old block
// was cut, and marked as overrun.
//
// Run: `go test -tags=integration ./internal/handler/ -run TestBackfillPending_ArmOff -count=1 -v`.
func TestBackfillPending_ArmOff_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	instant := func(string) time.Duration { return 0 }

	// A block whose backoff has passed, and as many fresh writes per query as
	// sync_cap allows picks: the block is the oldest pending one and the
	// first pick of the first request takes it.
	t.Run("overran_block_returns_under_steady_writes", func(t *testing.T) {
		for _, syncCap := range []int{1, 2, 4} {
			t.Run(fmt.Sprintf("cap_%d", syncCap), func(t *testing.T) {
				f := armOffSetup(t, instant)
				old := f.seed(t, "armoff overrun block", 60)
				f.overran(t, old)

				reached := 0
				for round := 1; round <= 3; round++ {
					for i := 1; i <= syncCap; i++ {
						f.seed(t, fmt.Sprintf("armoff fresh block, round %d number %d", round, i), 0)
					}
					f.run(syncCap, 10*time.Second)
					if reached == 0 && f.embedded(t, old) {
						reached = round
					}
				}
				if reached != 1 {
					t.Errorf("arm off, sync_cap %d, %d writes per query: the block that once overran the budget got its vector in round %d, want round 1 (0 = in none of three rounds)",
						syncCap, syncCap, reached)
				}
			})
		}
	})

	// Two blocks whose backoff has passed: the older one never fits the
	// budget, the newer one does. The first request spends its budget on the
	// older one; that puts it back inside a backoff, and the second request
	// embeds the newer one. A third request finds nothing it may pick and
	// makes no wire call: the block that never fits costs one budget per
	// backoff period, not one per request.
	t.Run("a_block_that_never_fits_does_not_hold_the_others", func(t *testing.T) {
		f := armOffSetup(t, func(body string) time.Duration {
			if strings.Contains(body, "armoff huge block") {
				return 5 * time.Second
			}
			return 0
		})
		huge := f.seed(t, "armoff huge block", 2)
		small := f.seed(t, "armoff small block", 1)
		f.overran(t, huge)
		f.overran(t, small)

		for request := 1; request <= 3; request++ {
			f.run(4, 300*time.Millisecond)
		}

		if !f.embedded(t, small) {
			t.Error("arm off: the block that fits the budget is still pending after three requests, behind the one that does not")
		}
		if f.embedded(t, huge) {
			t.Error("the block that cannot fit the budget has a vector; the probe's premise is gone")
		}
		if got := f.hits("armoff huge block"); got != 1 {
			t.Errorf("three requests made %d wire calls for the block that cannot fit the budget, want 1 (it is inside its backoff after the first)", got)
		}
		if class, attempts := f.memo(t, huge); class != string(store.EmbedFailureCallerTimeout) || attempts != 2 {
			t.Errorf("memo of the block that cannot fit: class %q, attempts %d, want %q and 2", class, attempts, store.EmbedFailureCallerTimeout)
		}
	})

	// Every embed takes more than half the budget, sync_cap is the default 4,
	// one write and one query per round. The old end has the first pick and
	// with it the whole budget, so each round embeds one old block; the fresh
	// block gets what is left, is cut, and keeps its place without a memo,
	// because the rest of a budget says nothing about the block.
	t.Run("old_end_gets_the_whole_budget", func(t *testing.T) {
		f := armOffSetup(t, func(string) time.Duration { return 1200 * time.Millisecond })
		olds := []string{f.seed(t, "armoff old block, first", 3), f.seed(t, "armoff old block, second", 2)}

		var fresh []string
		for round := 1; round <= len(olds); round++ {
			fresh = append(fresh, f.seed(t, fmt.Sprintf("armoff fresh block, round %d", round), 0))
			f.run(4, 2*time.Second)
			if !f.embedded(t, olds[round-1]) {
				class, attempts := f.memo(t, olds[round-1])
				t.Errorf("arm off, round %d: old block %d is still pending (memo class %q, attempts %d); an embed of 1.2 s fits the budget of 2 s when it has the whole of it",
					round, round, class, attempts)
			}
		}
		for i, id := range fresh {
			if class, attempts := f.memo(t, id); class != "" {
				t.Errorf("fresh block of round %d carries a memo (class %q, attempts %d) although it only ever had the rest of a budget", i+1, class, attempts)
			}
		}
	})
}
