//go:build integration

// Issues #421 and #428: the temporal writes of one block (the afterwork of a
// content write, the dream's review, the backfill) take turns, and none of
// them holds the block row in a mode UpdateBlock or the query path's failure
// memo (both FOR NO KEY UPDATE) conflicts with; a content write sets
// content_times itself, in its own statement. The transactions held here are held at their COMMIT, with every
// lock they took (testdb.HoldCommitPool), so the interleavings do not depend
// on timing.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestTemporalWriters|TestTemporalAfterworkHeld|TestContentWritesSetContentTimes|TestEditsComputeBeforeTheLock|TestEditsRecheck' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
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
	event := testdb.WaitBlockedBy(t, pool, firstPID, secondDone, "the second temporal write", "advisory")
	t.Logf("the second temporal write waits on the first's turn lock: wait_event=%s", event)

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
// an update (FOR NO KEY UPDATE) conflicts with, so a content update B lands
// while it is held. B's
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
	event := testdb.WaitBlockedBy(t, pool, aPID, bTail, "B's afterwork", "advisory")
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

// i421EditGap runs, once, when the unlocked read of an update's edits has
// ended: a lock probe that refuses to wait (the read must have taken no lock),
// then a content write of another connection, between the read and the lock.
type i421EditGap struct {
	t       *testing.T
	pool    *pgxpool.Pool
	id      string
	content string
	once    sync.Once
	ran     atomic.Bool
	probe   error
}

func (g *i421EditGap) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "SELECT content, content_hash FROM context_blocks") {
		return context.WithValue(ctx, i421EditReadKey{}, true)
	}
	return ctx
}

type i421EditReadKey struct{}

func (g *i421EditGap) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(i421EditReadKey{}) == nil || data.Err != nil {
		return
	}
	g.once.Do(func() {
		g.ran.Store(true)
		bg := context.Background()
		tx, err := g.pool.Begin(bg)
		if err != nil {
			g.probe = err
			return
		}
		_, g.probe = tx.Exec(bg, `SELECT 1 FROM context_blocks WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, g.id)
		_ = tx.Rollback(bg)
		if g.probe != nil {
			return // the write below would wait for the transaction this hook runs in
		}
		c := g.content
		if _, _, err := store.UpdateBlock(bg, g.pool, g.id, store.UpdateBlockData{Content: &c}, []string{"private"}); err != nil {
			g.t.Errorf("content write in the gap: %v", err)
		}
	})
}

// TestEditsComputeBeforeTheLockAndRedoOnAChangedRow: an update's edits read
// the row without a lock and compute the edited content and its dates there
// (ExtractDates takes milliseconds on a large content, which a row lock would
// be held through, issue #421); the lock then confirms the version. A content
// write landing between the read and the lock makes the edits run again, on
// the content the row holds then: the concurrent write is not lost, and
// content_times is that of the content stored.
func TestEditsComputeBeforeTheLockAndRedoOnAChangedRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	b, err := store.UpsertBlock(ctx, pool, "i421", "i421-edit-gap", "Planned 2026-05-05, kept 2026-06-06.", nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	gap := &i421EditGap{t: t, pool: pool, id: b.ID, content: "Moved: 2026-05-05 still, plus 2026-07-07."}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = gap
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer traced.Close()

	repl := "2026-05-09"
	u, _, err := store.UpdateBlock(ctx, traced, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "2026-05-05", New: &repl}}}, []string{"private"})
	if err != nil || u == nil {
		t.Fatalf("edits: %v", err)
	}
	if !gap.ran.Load() {
		t.Fatalf("premise: no unlocked read of the edit target ran before the lock")
	}
	if gap.probe != nil {
		t.Errorf("the read of the edit target locked the row: %v", gap.probe)
	}
	want := "Moved: 2026-05-09 still, plus 2026-07-07."
	if u.Content != want {
		t.Errorf("content = %q, want %q (the edit applied to the content written in the gap)", u.Content, want)
	}
	i421AssertContentTimes(t, pool, b.ID, want, "after the edits")
}

// TestEditsRecheckTheVersionAfterWaitingForAnInFlightWrite: a content write is
// held at its COMMIT while an update's edits run; their unlocked read sees the
// content before it, and their lock waits for it. Once it committed, READ
// COMMITTED locks the newest row version and the version check fails, so the
// edits run again on the content written: an edit that matched only the
// earlier content is refused, one that matches only the newer applies, and
// content_times is that of the content stored.
func TestEditsRecheckTheVersionAfterWaitingForAnInFlightWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	repl := "2026-05-09"

	cases := []struct {
		name, initial, inflight string
		wantContent             string // "" = the edit is refused
	}{
		{"applies before and after", "Planned 2026-05-05, kept 2026-06-06.", "Moved: 2026-05-05 still, plus 2026-07-07.", "Moved: 2026-05-09 still, plus 2026-07-07."},
		{"refused on the read, applies on the locked row", "Nothing dated here.", "Now on 2026-05-05.", "Now on 2026-05-09."},
		{"applies on the read, refused on the locked row", "Planned 2026-05-05.", "No date any more.", ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := store.UpsertBlock(ctx, pool, "i421", fmt.Sprintf("i421-edit-inflight-%d", i), tc.initial, nil, nil, "private", false, store.SensitivityWrite{}, "")
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			holdPool, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool {
				return strings.HasPrefix(strings.TrimSpace(sql), "UPDATE context_blocks SET content = ")
			})
			writeDone := make(chan error, 1)
			go func() {
				c := tc.inflight
				_, _, err := store.UpdateBlock(ctx, holdPool, b.ID, store.UpdateBlockData{Content: &c}, []string{"private"})
				writeDone <- err
			}()
			writePID := hold.Wait(t)

			editDone := make(chan struct{})
			var got *store.Block
			var editErr error
			go func() {
				defer close(editDone)
				got, _, editErr = store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "2026-05-05", New: &repl}}}, []string{"private"})
			}()
			event := testdb.WaitBlockedBy(t, pool, writePID, editDone, "the edits", "transactionid")
			t.Logf("the edits wait on the held content write: wait_event=%s", event)
			hold.Release()
			if err := <-writeDone; err != nil {
				t.Fatalf("content write: %v", err)
			}
			select {
			case <-editDone:
			case <-time.After(testdb.CommitHoldBound):
				t.Fatalf("the edits did not finish within %s after the content write committed", testdb.CommitHoldBound)
			}
			if tc.wantContent == "" {
				var ce *store.ContentEditError
				if !errors.As(editErr, &ce) {
					t.Errorf("edits: err=%v (block %v), want a ContentEditError", editErr, got)
				}
				var content string
				if err := pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1`, b.ID).Scan(&content); err != nil || content != tc.inflight {
					t.Errorf("stored content = %q (%v), want the content write's %q", content, err, tc.inflight)
				}
				i421AssertContentTimes(t, pool, b.ID, tc.inflight, "after the refused edit")
				return
			}
			if editErr != nil || got == nil {
				t.Fatalf("edits: %v", editErr)
			}
			if got.Content != tc.wantContent {
				t.Errorf("content = %q, want %q", got.Content, tc.wantContent)
			}
			i421AssertContentTimes(t, pool, b.ID, tc.wantContent, "after the edits")
		})
	}
}
