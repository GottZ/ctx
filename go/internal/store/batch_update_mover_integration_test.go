//go:build integration

// Issue #547 point 4(iii): a block batch whose update item names a block that
// a second writer keeps moving between two scopes. The batch read the block's
// scope without a lock, locked that scope and ran again when the block had
// left it — three runs, then the error; a single update of the block (no
// scope read) went through every time. The batch now carries the scope it
// found the block in into its next run and locks both, so a mover between two
// scopes cannot outrun it twice.
//
// TestBatchUpdateItemBesideAMover pins the guarantee: every batch call goes
// through beside a mover in a loop. With CTX_B547_CALLS it runs that many
// calls of the batch and of the single update each and logs the outcomes
// (the measurement of the issue's comment, 100 calls).
//
//	CTX_B547_CALLS=100 go test -tags=integration -p 1 ./internal/store/ -run 'TestBatchUpdateItemBesideAMover' -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestBatchUpdateItemBesideAMover(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	calls := 30
	if n, err := strconv.Atoi(os.Getenv("CTX_B547_CALLS")); err == nil && n > 0 {
		calls = n
	}
	scopes := []string{"b547-a", "b547-b"}
	writable := append([]string{"private"}, scopes...)
	id := f.block(t, scopes[0], "b547 moved")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var moves atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			to := scopes[(i+1)%2]
			if _, _, err := store.UpdateBlock(ctx, f.pool, id, store.UpdateBlockData{Scope: &to}, writable); err == nil {
				moves.Add(1)
			} else if !errors.Is(err, store.ErrWriteConflict) {
				t.Errorf("mover: %v", err)
				return
			}
		}
	}()

	var batchOK, batchConflict, batchOther, singleOK, singleOther int
	for i := 0; i < calls; i++ {
		content := fmt.Sprintf("batch %d", i)
		_, err := store.UpsertBlocks(ctx, f.pool, []store.BatchUpsert{{
			Update: &store.BatchUpdate{ID: id, Data: store.UpdateBlockData{Content: &content}, WriteScopes: writable},
		}}, "", f.set, nil)
		switch {
		case err == nil:
			batchOK++
		case errors.Is(err, store.ErrWriteConflict):
			batchConflict++
		default:
			batchOther++
			t.Logf("batch call %d: %v", i, err)
		}
		single := fmt.Sprintf("single %d", i)
		if _, _, err := store.UpdateBlock(ctx, f.pool, id, store.UpdateBlockData{Content: &single}, writable); err == nil {
			singleOK++
		} else {
			singleOther++
		}
	}
	close(stop)
	wg.Wait()
	t.Logf("%d calls beside %d moves: batch %d through, %d write conflicts, %d other; single update %d through, %d other",
		calls, moves.Load(), batchOK, batchConflict, batchOther, singleOK, singleOther)
	if moves.Load() < int64(calls) {
		t.Logf("the mover made only %d moves: the load was thin", moves.Load())
	}
	if batchOK != calls {
		t.Errorf("batch: %d of %d calls through beside the mover (%d conflicts, %d other), want all", batchOK, calls, batchConflict, batchOther)
	}
}
