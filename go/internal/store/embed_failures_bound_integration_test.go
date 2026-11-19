//go:build integration

// The memo writes of the query path's inline backfill (issue #372, part 1),
// the count of concurrent caller_timeout cuts (issue #240, part 2), and what a
// title or content change does to a caller_timeout memo (issue #240, part 4).
//
// The query path reads a block, embeds it over the wire without holding the
// row, and on a failure writes the block's memo. A title or content change
// deletes the memo in its own transaction (InvalidateEmbeddingTx). A memo of
// the attempt at the earlier text that lands after that delete parks the
// changed block under the earlier verdict; for an oversize memo for good.
//
//	go test -tags=integration ./internal/store/ -run 'TestRecordEmbedFailureIfUnchanged|TestRecordEmbedFailure_ConcurrentCuts|TestInvalidateEmbedding_CallerTimeoutMemo' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// boundMemo reads the block's regular memo: present, attempts, class and
// next_attempt_at (the zero time for 'infinity').
func boundMemo(t *testing.T, pool *pgxpool.Pool, id string) (present bool, attempts int, class string, next time.Time) {
	t.Helper()
	var at pgtype.Timestamptz // 'infinity' for a park
	err := pool.QueryRow(context.Background(),
		`SELECT attempts, last_class, next_attempt_at FROM context_embed_failures
		  WHERE block_id = $1::uuid AND migration_id IS NULL`, id).Scan(&attempts, &class, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, "", time.Time{}
	}
	if err != nil {
		t.Fatalf("read memo: %v", err)
	}
	return true, attempts, class, at.Time
}

// boundSeed writes a block through UpsertBlock and, when embedded, gives it a
// vector, so a later content change marks it stale.
func boundSeed(t *testing.T, pool *pgxpool.Pool, title, content string, embedded bool) string {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, pool, "learnings", title, content, nil, nil, "private", true, store.SensitivityWrite{}, "knowledge")
	if err != nil {
		t.Fatalf("seed %q: %v", title, err)
	}
	if embedded {
		vec := make([]float32, 1024)
		for i := range vec {
			vec[i] = float32(i%3) - 1
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "test-embed", vec); err != nil {
			t.Fatal(err)
		}
	}
	return b.ID
}

func TestRecordEmbedFailureIfUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	strptr := func(s string) *string { return &s }

	// The block still holds the text the attempt read: the memo lands, and an
	// oversize park drops the stale vector, as RecordEmbedFailure does.
	t.Run("unchanged_text_is_memoized", func(t *testing.T) {
		id := boundSeed(t, pool, "bound unchanged", "first text", true)
		if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr("second text")}, []string{"private"}); err != nil {
			t.Fatal(err)
		}
		written, err := store.RecordEmbedFailureIfUnchanged(ctx, pool, id, "bound unchanged", "second text",
			store.EmbedFailureOversize, "oversize: test", time.Minute, time.Hour)
		if err != nil || !written {
			t.Fatalf("written=%v err=%v, want a memo for the text the block holds", written, err)
		}
		if present, _, class, _ := boundMemo(t, pool, id); !present || class != string(store.EmbedFailureOversize) {
			t.Fatalf("memo present=%v class=%q, want an oversize memo", present, class)
		}
		var hasVec, stale bool
		if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL, embedding_stale FROM context_blocks WHERE id = $1::uuid`, id).Scan(&hasVec, &stale); err != nil {
			t.Fatal(err)
		}
		if hasVec || stale {
			t.Errorf("after the park: vector=%v stale=%v, want the stale vector dropped", hasVec, stale)
		}
	})

	// The change committed before the memo write: no memo, and the stale
	// vector of the changed block stays (it is pending, not parked).
	for _, class := range []store.EmbedFailureClass{store.EmbedFailureOversize, store.EmbedFailureWire, store.EmbedFailureCallerTimeout} {
		t.Run("change_committed_before_"+string(class), func(t *testing.T) {
			id := boundSeed(t, pool, "bound committed "+string(class), "first text", true)
			if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr("second text")}, []string{"private"}); err != nil {
				t.Fatal(err)
			}
			written, err := store.RecordEmbedFailureIfUnchanged(ctx, pool, id, "bound committed "+string(class), "first text",
				class, string(class)+": test", time.Minute, time.Hour)
			if err != nil || written {
				t.Fatalf("written=%v err=%v, want no memo for text the block no longer holds", written, err)
			}
			if present, _, _, _ := boundMemo(t, pool, id); present {
				t.Errorf("a memo of the earlier text parks the changed block")
			}
			var hasVec, stale bool
			if err := pool.QueryRow(ctx, `SELECT embedding IS NOT NULL, embedding_stale FROM context_blocks WHERE id = $1::uuid`, id).Scan(&hasVec, &stale); err != nil {
				t.Fatal(err)
			}
			if !hasVec || !stale {
				t.Errorf("vector=%v stale=%v, want the changed block's stale vector kept", hasVec, stale)
			}
		})
	}

	// Another writer embedded the block at the same text meanwhile (the arm
	// next to the query path): the block is no longer pending, no memo.
	t.Run("embedded_meanwhile", func(t *testing.T) {
		id := boundSeed(t, pool, "bound embedded meanwhile", "the text", false)
		vec := make([]float32, 1024)
		vec[0] = 1
		if err := store.StoreEmbedding(ctx, pool, id, "test-embed", vec); err != nil {
			t.Fatal(err)
		}
		written, err := store.RecordEmbedFailureIfUnchanged(ctx, pool, id, "bound embedded meanwhile", "the text",
			store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour)
		if err != nil || written {
			t.Fatalf("written=%v err=%v, want no memo for a block that has its vector", written, err)
		}
		if present, _, _, _ := boundMemo(t, pool, id); present {
			t.Errorf("a memo was left on an embedded block")
		}
	})

	t.Run("request_gone_after_a_change", func(t *testing.T) {
		id := boundSeed(t, pool, "bound gone", "first text", false)
		if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr("second text")}, []string{"private"}); err != nil {
			t.Fatal(err)
		}
		written, err := store.RecordEmbedRequestGoneIfUnchanged(ctx, pool, id, "bound gone", "first text", "caller_timeout: gone")
		if err != nil || written {
			t.Fatalf("written=%v err=%v, want no memo", written, err)
		}
		if present, _, _, _ := boundMemo(t, pool, id); present {
			t.Errorf("the memo of an ended request at the earlier text parks the changed block")
		}
		written, err = store.RecordEmbedRequestGoneIfUnchanged(ctx, pool, id, "bound gone", "second text", "caller_timeout: gone")
		if err != nil || !written {
			t.Fatalf("written=%v err=%v at the current text, want a memo", written, err)
		}
	})

	// The interleaving the lock is for (second connection): the change holds
	// the block's row and has released the memo, but has not committed, when
	// the memo write runs. A write that read the block from its snapshot would
	// see the earlier text, insert the memo without waiting, and the memo
	// would outlive the change. The bound write skips the held row and
	// inserts nothing.
	t.Run("change_in_flight", func(t *testing.T) {
		const title = "bound in flight"
		id := boundSeed(t, pool, title, "first text", false)
		written, err := boundDuringChange(t, pool, id, func() (bool, error) {
			return store.RecordEmbedFailureIfUnchanged(context.Background(), pool, id, title, "first text",
				store.EmbedFailureOversize, "oversize: test", time.Minute, time.Hour)
		})
		if err != nil {
			t.Fatal(err)
		}
		if present, _, _, _ := boundMemo(t, pool, id); present || written {
			t.Errorf("memo present=%v written=%v after a change that was in flight during the write: the changed block is parked under the earlier attempt",
				present, written)
		}
	})

	// The same with a caller_timeout memo in place whose hold has passed: the
	// change keeps it and moves its cut to the change (InvalidateEmbeddingTx);
	// the cut of the earlier text must not count an attempt on it. The
	// in-place update waits for the change on the memo row and then finds the
	// hold running.
	t.Run("change_in_flight_with_a_memo", func(t *testing.T) {
		const title = "bound in flight memo"
		id := boundSeed(t, pool, title, "first text", false)
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET next_attempt_at = now() - interval '1 day' WHERE block_id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := boundDuringChange(t, pool, id, func() (bool, error) {
			return store.RecordEmbedFailureIfUnchanged(context.Background(), pool, id, title, "first text",
				store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour)
		}); err != nil {
			t.Fatal(err)
		}
		if _, attempts, _, next := boundMemo(t, pool, id); attempts != 1 || time.Since(next) > time.Minute {
			t.Errorf("attempts=%d next=%v, want 1 and the change's time: the cut of the earlier text counted on the changed block", attempts, next)
		}
	})

	// A background embedder (the arm, Dream, a migration worker) holds the
	// block row FOR UPDATE for its whole embed call. The memo write does not
	// wait for it: a memo of the same class in place is updated, none is
	// inserted.
	t.Run("row_held_by_a_background_embedder", func(t *testing.T) {
		const title = "bound held"
		id := boundSeed(t, pool, title, "the text", false)
		fresh := boundSeed(t, pool, "bound held fresh", "the text", false)
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET next_attempt_at = now() - interval '1 day' WHERE block_id = $1::uuid`, id); err != nil {
			t.Fatal(err)
		}
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx, `SELECT id FROM context_blocks WHERE id = ANY($1::uuid[]) FOR UPDATE`, []string{id, fresh}); err != nil {
			t.Fatal(err)
		}
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		start := time.Now()
		written, err := store.RecordEmbedFailureIfUnchanged(wctx, pool, id, title, "the text",
			store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour)
		if err != nil || !written {
			t.Fatalf("written=%v err=%v after %v, want the memo in place updated without waiting for the holder", written, err, time.Since(start))
		}
		if _, attempts, _, next := boundMemo(t, pool, id); attempts != 2 || time.Since(next) > time.Minute {
			t.Errorf("attempts=%d next=%v, want 2 and now", attempts, next)
		}
		written, err = store.RecordEmbedFailureIfUnchanged(wctx, pool, fresh, "bound held fresh", "the text",
			store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour)
		if err != nil || written {
			t.Fatalf("block without a memo: written=%v err=%v, want nothing inserted under the holder", written, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("the two writes took %v: they waited for the holder", took)
		}
	})
}

// boundDuringChange runs write while a title or content change of id is in
// flight on a second connection (the block row and the memo row held, as
// UpdateBlock holds them), waits until write waits on a lock or has finished,
// commits the change, and returns what write returned.
func boundDuringChange(t *testing.T, pool *pgxpool.Pool, id string, write func() (bool, error)) (bool, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE context_blocks SET content = 'second text' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidateEmbeddingTx(ctx, tx, id); err != nil {
		t.Fatal(err)
	}
	type result struct {
		written bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		w, err := write()
		done <- result{w, err}
	}()
	var early *result
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && early == nil; time.Sleep(20 * time.Millisecond) {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'
			    AND query LIKE '%context_embed_failures%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			early = &r
		default:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if early == nil {
		r := <-done
		early = &r
	}
	return early.written, early.err
}

// TestRecordEmbedFailure_ConcurrentCutsCountOnce: eight requests that picked
// the same block before the first cut was written, all cut, count one
// attempt (issue #240, part 2). A cut after the hold counts the next one; a
// cut inside a memo's hold leaves the memo as it is.
func TestRecordEmbedFailure_ConcurrentCutsCountOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const base, capD = time.Minute, time.Hour

	for _, variant := range []string{"unbound", "bound"} {
		t.Run(variant, func(t *testing.T) {
			title := "concurrent cuts " + variant
			id := boundSeed(t, pool, title, "the text", false)
			cut := func() {
				if variant == "bound" {
					if _, err := store.RecordEmbedFailureIfUnchanged(context.Background(), pool, id, title, "the text",
						store.EmbedFailureCallerTimeout, "caller_timeout: cut", base, capD); err != nil {
						t.Error(err)
					}
					return
				}
				if err := store.RecordEmbedFailure(context.Background(), pool, id,
					store.EmbedFailureCallerTimeout, "caller_timeout: cut", base, capD); err != nil {
					t.Error(err)
				}
			}
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					cut()
				}()
			}
			close(start)
			wg.Wait()
			_, attempts, class, first := boundMemo(t, pool, id)
			if attempts != 1 || class != string(store.EmbedFailureCallerTimeout) {
				t.Fatalf("after eight concurrent cuts: attempts=%d class=%q, want 1 caller_timeout", attempts, class)
			}

			// Inside the hold (one attempt: backoff_base): unchanged.
			cut()
			if _, a, _, next := boundMemo(t, pool, id); a != 1 || !next.Equal(first) {
				t.Errorf("a cut inside the hold: attempts=%d next=%v, want 1 and the first cut's time %v", a, next, first)
			}

			// After the hold: the next attempt counts, from this cut.
			if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET next_attempt_at = now() - interval '2 minutes' WHERE block_id = $1::uuid`, id); err != nil {
				t.Fatal(err)
			}
			cut()
			if _, a, _, next := boundMemo(t, pool, id); a != 2 || time.Since(next) > time.Minute {
				t.Errorf("a cut after the hold: attempts=%d next=%v, want 2 and now", a, next)
			}
		})
	}

	// A wire memo inside its backoff: a cut that raced it changes nothing.
	t.Run("inside_a_wire_backoff", func(t *testing.T) {
		id := boundSeed(t, pool, "cut in wire backoff", "the text", false)
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureWire, "wire: down", base, capD); err != nil {
			t.Fatal(err)
		}
		_, _, _, wireNext := boundMemo(t, pool, id)
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureCallerTimeout, "caller_timeout: cut", base, capD); err != nil {
			t.Fatal(err)
		}
		if _, a, class, next := boundMemo(t, pool, id); a != 1 || class != string(store.EmbedFailureWire) || !next.Equal(wireNext) {
			t.Errorf("attempts=%d class=%q next=%v, want the wire memo unchanged (1, wire, %v)", a, class, next, wireNext)
		}
	})
}

// TestInvalidateEmbedding_CallerTimeoutMemo: a real edit keeps a caller_timeout
// memo's attempts and moves its cut to the edit (issue #240, part 4); every
// other class is deleted, as before.
func TestInvalidateEmbedding_CallerTimeoutMemo(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	strptr := func(s string) *string { return &s }

	t.Run("caller_timeout_keeps_its_attempts", func(t *testing.T) {
		id := boundSeed(t, pool, "edit keeps cut", "first text", false)
		for i := 0; i < 3; i++ {
			if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE context_embed_failures SET next_attempt_at = now() - interval '1 day' WHERE block_id = $1::uuid`, id); err != nil {
				t.Fatal(err)
			}
		}
		before := time.Now().Add(-time.Second)
		if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr("second text")}, []string{"private"}); err != nil {
			t.Fatal(err)
		}
		present, attempts, class, next := boundMemo(t, pool, id)
		if !present || attempts != 3 || class != string(store.EmbedFailureCallerTimeout) {
			t.Fatalf("after the edit: present=%v attempts=%d class=%q, want the caller_timeout memo at 3 attempts", present, attempts, class)
		}
		if next.Before(before) {
			t.Errorf("next_attempt_at = %v, want the time of the edit (after %v): the block queues from the edit", next, before)
		}
	})

	// A caller_timeout memo outlives a later successful embed (no vector write
	// removes it). Such a leftover is deleted by the next edit, as before.
	t.Run("caller_timeout_of_a_block_embedded_since_is_deleted", func(t *testing.T) {
		id := boundSeed(t, pool, "edit drops leftover", "first text", false)
		if err := store.RecordEmbedFailure(ctx, pool, id, store.EmbedFailureCallerTimeout, "caller_timeout: cut", time.Minute, time.Hour); err != nil {
			t.Fatal(err)
		}
		vec := make([]float32, 1024)
		vec[1] = 1
		if err := store.StoreEmbedding(ctx, pool, id, "test-embed", vec); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr("second text")}, []string{"private"}); err != nil {
			t.Fatal(err)
		}
		if present, attempts, _, _ := boundMemo(t, pool, id); present {
			t.Errorf("the leftover caller_timeout memo (attempts %d) of a block embedded since survived the edit: the edited block is held for a hold it never earned", attempts)
		}
	})

	for _, class := range []store.EmbedFailureClass{store.EmbedFailureWire, store.EmbedFailureOversize} {
		t.Run(string(class)+"_is_deleted", func(t *testing.T) {
			id := boundSeed(t, pool, "edit releases "+string(class), "first text", false)
			if err := store.RecordEmbedFailure(ctx, pool, id, class, string(class)+": test", time.Minute, time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: strptr("second text")}, []string{"private"}); err != nil {
				t.Fatal(err)
			}
			if present, _, _, _ := boundMemo(t, pool, id); present {
				t.Errorf("a %s memo survived the edit", class)
			}
		})
	}
}
