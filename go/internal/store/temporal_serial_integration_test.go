//go:build integration

// Issues #421 and #428: the temporal writes of one block (the afterwork of a
// content write, the dream's review, the backfill) take turns, and none of
// them holds the block row in a mode a content writer or the query path's
// failure memo needs; a content write sets content_times itself, in its own
// statement. The transactions held here are held at their COMMIT, with every
// lock they took (testdb.HoldCommitPool), so the interleavings do not depend
// on timing.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestTemporalWriters|TestTemporalAfterworkHeld|TestContentWritesSetContentTimes' -count=1 -v
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
	"github.com/jackc/pgx/v5/pgxpool"
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

// TestTemporalAfterworkHeldAtCommitHoldsNoBlockRowLock is #421 at the store:
// the afterwork of a content write A, held at its COMMIT, holds the block row
// in no mode the query path's failure memo (FOR NO KEY UPDATE SKIP LOCKED) or
// a content writer needs, so a content update B lands while it is held. B's
// own afterwork then waits for A's and replaces its rows: the block ends with
// B's data, and content_times is B's from B's own statement.
func TestTemporalAfterworkHeldAtCommitHoldsNoBlockRowLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	contentA := "Draft due 2026-03-03."
	a, err := store.UpsertBlock(ctx, pool, "i421", "i421-held-afterwork", contentA, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store A: %v", err)
	}

	holdPool, hold := testdb.HoldCommitPool(t, dsn, i428TemporalBatch)
	aDone := make(chan error, 1)
	go func() {
		_, err := store.RefreshTemporal(ctx, holdPool, a.ID, a.Content, a.CreatedAt)
		aDone <- err
	}()
	aPID := hold.Wait(t)

	// The lock the failure memo takes, refusing to wait: it gets the row.
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	if _, err := probe.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, a.ID); err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("the afterwork held at its commit holds the block row against the failure memo's lock: %v", err)
	}
	_ = probe.Rollback(ctx)

	// A content update lands while A's afterwork is held.
	contentB := "Draft due 2026-08-08."
	type upd struct {
		b   *store.Block
		err error
	}
	bDone := make(chan upd, 1)
	go func() {
		bb, _, err := store.UpdateBlock(ctx, pool, a.ID, store.UpdateBlockData{Content: &contentB}, []string{"private"})
		bDone <- upd{bb, err}
	}()
	var bb *store.Block
	select {
	case r := <-bDone:
		if r.err != nil || r.b == nil {
			t.Fatalf("update B: %v", r.err)
		}
		bb = r.b
	case <-time.After(5 * time.Second):
		t.Fatalf("update B waited on the afterwork of A held at its commit")
	}

	// B's afterwork waits for A's.
	bTail := make(chan struct{})
	var bApplied bool
	var bErr error
	go func() {
		defer close(bTail)
		bApplied, bErr = store.RefreshTemporal(ctx, pool, bb.ID, bb.Content, bb.CreatedAt)
	}()
	event := testdb.WaitBlockedBy(t, pool, aPID, bTail, "B's afterwork")
	t.Logf("B's afterwork waits on A's: wait_event=%s", event)

	hold.Release()
	if err := <-aDone; err != nil {
		t.Fatalf("A's afterwork: %v", err)
	}
	select {
	case <-bTail:
	case <-time.After(testdb.CommitHoldBound):
		t.Fatalf("B's afterwork did not finish within %s after A's committed", testdb.CommitHoldBound)
	}
	if bErr != nil || !bApplied {
		t.Fatalf("B's afterwork: applied=%v err=%v", bApplied, bErr)
	}
	if got, want := i414Sources(t, pool, a.ID), i414Want(a.CreatedAt, i414Day("2026-08-08")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v (content B)", got, want)
	}
	if got := i421ContentTimes(t, pool, a.ID); !slices.EqualFunc(got, []time.Time{i414Day("2026-08-08")}, time.Time.Equal) {
		t.Errorf("content_times = %v, want B's [2026-08-08]", got)
	}
}

// i421ContentTimes is the block's content_times as stored, order and repeats
// kept (NULL reads empty).
func i421ContentTimes(t *testing.T, pool *pgxpool.Pool, id string) []time.Time {
	t.Helper()
	var times []time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(content_times, '{}') FROM context_blocks WHERE id = $1`, id).Scan(&times); err != nil {
		t.Fatalf("read content_times: %v", err)
	}
	return times
}

// i421AssertContentTimes checks that the block's content_times is what
// store.ExtractDates gives for content, element by element.
func i421AssertContentTimes(t *testing.T, pool *pgxpool.Pool, id, content, label string) {
	t.Helper()
	got, want := i421ContentTimes(t, pool, id), store.ExtractDates(content)
	if !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Errorf("%s: content_times = %v, want %v (derived from %q)", label, got, want, content)
	}
}

// TestContentWritesSetContentTimes: every content write the temporal
// afterwork follows sets content_times in its own statement, from the content
// it stores — no afterwork runs here. Before #421 the column was the
// afterwork's, written by an UPDATE after the write had answered.
func TestContentWritesSetContentTimes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{"private"}

	upsert := func(title, content string) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "i421", title, content, nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("upsert %q: %v", title, err)
		}
		return b
	}

	t.Run("upsert inserts", func(t *testing.T) {
		c := "Kick-off 2026-09-01, review 15.09.2026 14:30, launch März 2027."
		i421AssertContentTimes(t, pool, upsert("i421-insert", c).ID, c, "insert")
	})
	t.Run("upsert replaces the content", func(t *testing.T) {
		b := upsert("i421-conflict", "First on 2026-01-05.")
		c := "Moved to 2026-02-06 and 2026-01-05."
		if again := upsert("i421-conflict", c); again.ID != b.ID {
			t.Fatalf("the second upsert landed on another row: %s, %s", b.ID, again.ID)
		}
		i421AssertContentTimes(t, pool, b.ID, c, "conflict")
	})
	t.Run("content without dates", func(t *testing.T) {
		b := upsert("i421-nodates", "Dated 2026-04-04.")
		c := "No date in here any more."
		upsert("i421-nodates", c)
		i421AssertContentTimes(t, pool, b.ID, c, "conflict to no dates")
		var isNull bool
		if err := pool.QueryRow(ctx, `SELECT content_times IS NULL FROM context_blocks WHERE id = $1`, b.ID).Scan(&isNull); err != nil || !isNull {
			t.Errorf("content_times of a content without dates: NULL=%v err=%v, want NULL (what the afterwork wrote before)", isNull, err)
		}
	})
	t.Run("update content", func(t *testing.T) {
		b := upsert("i421-update", "Planned 2026-05-05.")
		c := "Done on 2026-05-07."
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &c}, scopes); err != nil {
			t.Fatalf("update: %v", err)
		}
		i421AssertContentTimes(t, pool, b.ID, c, "update")
	})
	t.Run("update edits", func(t *testing.T) {
		b := upsert("i421-edits", "Planned 2026-05-05, kept 2026-06-06.")
		repl := "2026-05-09"
		u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "2026-05-05", New: &repl}}}, scopes)
		if err != nil || u == nil {
			t.Fatalf("edits: %v", err)
		}
		i421AssertContentTimes(t, pool, b.ID, u.Content, "edits")
		if got := i421ContentTimes(t, pool, b.ID); len(got) != 2 || !got[0].Equal(i414Day("2026-05-09")) {
			t.Errorf("content_times after the edit = %v, want [2026-05-09 2026-06-06]", got)
		}
	})
	t.Run("update without content keeps it", func(t *testing.T) {
		c := "Planned 2026-05-05."
		b := upsert("i421-tags", c)
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Tags: []string{"i421"}}, scopes); err != nil {
			t.Fatalf("update tags: %v", err)
		}
		i421AssertContentTimes(t, pool, b.ID, c, "tags-only update")
	})
	t.Run("chunk insert and rewrite", func(t *testing.T) {
		var sourceID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/i421/notes.md', 'i421', 'private') RETURNING id::text`).Scan(&sourceID); err != nil {
			t.Fatalf("seed source: %v", err)
		}
		c := "Chunk of 2026-10-10."
		ch, err := store.InsertChunk(ctx, pool, sourceID, 0, "i421", "i421-chunk", c, nil, nil, "private")
		if err != nil {
			t.Fatalf("insert chunk: %v", err)
		}
		i421AssertContentTimes(t, pool, ch.ID, c, "chunk insert")
		c = "Chunk rewritten on 2026-10-12."
		if again, err := store.InsertChunk(ctx, pool, sourceID, 0, "i421", "i421-chunk", c, nil, nil, "private"); err != nil || again.ID != ch.ID {
			t.Fatalf("rewrite chunk: %v (id %v)", err, again)
		}
		i421AssertContentTimes(t, pool, ch.ID, c, "chunk rewrite")
	})
	t.Run("batch", func(t *testing.T) {
		upsert("i421-batch-b", "Old 2026-01-01.")
		ca, cb := "Batch A on 2026-11-01.", "Batch B on 2026-11-02."
		res, err := store.UpsertBlocks(ctx, pool, []store.BatchUpsert{
			{Category: "i421", Title: "i421-batch-a", Content: ca, Scope: "private"},
			{Category: "i421", Title: "i421-batch-b", Content: cb, Scope: "private"},
		}, "", nil, nil)
		if err != nil || len(res) != 2 || res[0].Block == nil || res[1].Block == nil {
			t.Fatalf("batch: %v (%d results)", err, len(res))
		}
		i421AssertContentTimes(t, pool, res[0].Block.ID, ca, "batch insert")
		i421AssertContentTimes(t, pool, res[1].Block.ID, cb, "batch conflict")
	})
}
