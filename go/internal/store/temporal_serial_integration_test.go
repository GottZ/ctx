//go:build integration

// Issue #428: the temporal writes of one block (the afterwork of a content
// write, the dream's review, the backfill) take turns. The transactions held
// here are held at their COMMIT, with every lock they took
// (testdb.HoldCommitPool), so the interleavings do not depend on timing.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestTemporalWriters' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i428TemporalBatch matches the dimension batch of a temporal write.
func i428TemporalBatch(sql string) bool {
	return strings.Contains(sql, "DELETE FROM context_temporal")
}

// TestTemporalWritersOfOneBlockLeaveOneDateSet is the interleaving of #428:
// two temporal writes of one block with the same content and different dates
// (the write path's extraction and the dream's review, or two reviews), the
// first held between its DELETE and its COMMIT. The second must wait for the
// first and then replace its rows; before, its DELETE skipped the rows the
// first deleted and did not see the ones it inserted, and both date sets
// stayed.
func TestTemporalWritersOfOneBlockLeaveOneDateSet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	content := "Retro on 2026-02-10."
	b, err := store.UpsertBlock(ctx, pool, "i428", "i428-two-reviews", content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}

	holdPool, hold := testdb.HoldCommitPool(t, dsn, i428TemporalBatch)
	first := append(store.ExtractDates(content), i414Day("2026-02-11"))
	firstDone := make(chan error, 1)
	go func() {
		applied, err := store.PopulateTemporal(ctx, holdPool, b.ID, content, first, b.CreatedAt)
		if err == nil && !applied {
			err = errNotApplied
		}
		firstDone <- err
	}()
	firstPID := hold.Wait(t)

	second := append(store.ExtractDates(content), i414Day("2026-02-12"))
	secondDone := make(chan struct{})
	var secondApplied bool
	var secondErr error
	go func() {
		defer close(secondDone)
		secondApplied, secondErr = store.PopulateTemporal(ctx, pool, b.ID, content, second, b.CreatedAt)
	}()
	event := testdb.WaitBlockedBy(t, pool, firstPID, secondDone, "the second temporal write")
	t.Logf("the second temporal write waits on the first: wait_event=%s", event)

	hold.Release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first temporal write: %v", err)
	}
	select {
	case <-secondDone:
	case <-time.After(testdb.CommitHoldBound):
		t.Fatalf("the second temporal write did not finish within %s after the first committed", testdb.CommitHoldBound)
	}
	if secondErr != nil || !secondApplied {
		t.Fatalf("second temporal write: applied=%v err=%v", secondApplied, secondErr)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, second...); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v (the second write's set alone)", got, want)
	}
}

// errNotApplied marks a temporal write that answered applied=false where the
// test needs it applied.
var errNotApplied = errors.New("temporal write not applied")
