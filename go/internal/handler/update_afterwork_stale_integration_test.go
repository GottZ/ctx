//go:build integration

// Issue #414: the temporal afterwork of a write (content_times on the block and
// the rows of context_temporal) is derived from the block the write's own
// statement returned, and it runs after that statement committed. Two content
// updates A, B of one block that commit in that order can finish their
// afterwork in the order B, A; the stale afterwork must then be skipped, so the
// block keeps the temporal data of the content it holds.
//
// The writes go through the production functions the update doors call one
// after the other (store.UpdateBlock, then finishBlockUpdate) and the ones the
// store batch calls (store.UpsertBlock, then finishStoredBlock); a test holds
// one afterwork back by calling it later.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestUpdateAfterwork -count=1 -v
package handler

import (
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i414Temporal reads what the temporal afterwork wrote for a block: the
// content_times column and the distinct source times of its context_temporal
// rows, each as sorted UTC strings.
func i414Temporal(t *testing.T, f *i406Fixture, id string) (contentTimes, sources []string) {
	t.Helper()
	var times []time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(content_times, '{}') FROM context_blocks WHERE id = $1`, id).Scan(&times); err != nil {
		t.Fatalf("read content_times: %v", err)
	}
	rows, err := f.pool.Query(f.ctx,
		`SELECT DISTINCT source_time FROM context_temporal WHERE block_id = $1 AND dimension <> '_none'`, id)
	if err != nil {
		t.Fatalf("read temporal rows: %v", err)
	}
	defer rows.Close()
	var srcs []time.Time
	for rows.Next() {
		var s time.Time
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan temporal row: %v", err)
		}
		srcs = append(srcs, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("temporal rows: %v", err)
	}
	return i414Strings(times), i414Strings(srcs)
}

// i414Derived is what the afterwork derives from content for a block created
// at createdAt: the extracted dates, and those dates plus the creation time as
// the source times of the dimension rows.
func i414Derived(content string, createdAt time.Time) (contentTimes, sources []string) {
	dates := store.ExtractDates(content)
	return i414Strings(dates), i414Strings(append([]time.Time{createdAt}, dates...))
}

func i414Strings(ts []time.Time) []string {
	out := make([]string, 0, len(ts))
	for _, x := range ts {
		s := x.UTC().Format(time.RFC3339Nano)
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

func i414CreatedAt(t *testing.T, f *i406Fixture, id string) time.Time {
	t.Helper()
	var c time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT created_at FROM context_blocks WHERE id = $1`, id).Scan(&c); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	return c
}

// i414AssertDerivedFrom fails unless the block's temporal data is the one its
// afterwork derives from content.
func i414AssertDerivedFrom(t *testing.T, f *i406Fixture, id, content, label string) {
	t.Helper()
	gotTimes, gotSources := i414Temporal(t, f, id)
	wantTimes, wantSources := i414Derived(content, i414CreatedAt(t, f, id))
	if !slices.Equal(gotTimes, wantTimes) {
		t.Errorf("%s: content_times = %v, want %v (derived from %q)", label, gotTimes, wantTimes, content)
	}
	if !slices.Equal(gotSources, wantSources) {
		t.Errorf("%s: temporal source times = %v, want %v (derived from %q)", label, gotSources, wantSources, content)
	}
}

// i414Update runs the write half of a content update (what every update door
// runs before its afterwork) and returns the block it returned.
func i414Update(t *testing.T, f *i406Fixture, id string, data store.UpdateBlockData) *store.Block {
	t.Helper()
	b, _, err := store.UpdateBlock(f.ctx, f.pool, id, data, writableBlockScopes(f.directAR))
	if err != nil || b == nil {
		t.Fatalf("update %s: %v (block %v)", id, err, b)
	}
	return b
}

func i414Content(s string) store.UpdateBlockData { return store.UpdateBlockData{Content: &s} }

const (
	i414Initial  = "Initial note, written 2026-01-05."
	i414ContentA = "Plan for the review on 2026-03-15."
	i414ContentB = "The review moved to 2026-07-20."
)

// TestUpdateAfterworkStaleWriteSkipped is the interleaving of the issue: A and
// B commit in that order, B's afterwork finishes first, A's afterwork runs
// last. The block holds B's content and must keep the temporal data derived
// from it.
func TestUpdateAfterworkStaleWriteSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i414-stale", i414Initial, nil)
	i414AssertDerivedFrom(t, f, id, i414Initial, "after the store")

	dataA, dataB := i414Content(i414ContentA), i414Content(i414ContentB)
	blockA := i414Update(t, f, id, dataA)
	blockB := i414Update(t, f, id, dataB)
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i414-b")
	i414AssertDerivedFrom(t, f, id, i414ContentB, "after B's afterwork")

	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i414-a")
	i414AssertDerivedFrom(t, f, id, i414ContentB, "after A's late afterwork")
}

// TestUpdateAfterworkStaleEditsSkipped is the same interleaving with A written
// as edits (issue #406), the cheap form of an update the issue names as the
// likely source of close-together updates.
func TestUpdateAfterworkStaleEditsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i414-edits", i414ContentA, nil)

	repl := "2026-04-01"
	dataA := store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "2026-03-15", New: &repl}}}
	blockA := i414Update(t, f, id, dataA)
	dataB := i414Content(i414ContentB)
	blockB := i414Update(t, f, id, dataB)
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i414-b")
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i414-a")
	i414AssertDerivedFrom(t, f, id, i414ContentB, "after A's late afterwork")
}

// TestUpdateAfterworkWaitsForInFlightContentWrite holds a content write open
// on a second connection while A's afterwork runs: the afterwork has to wait
// for that write and, once it commits, must not apply A's data over the new
// content. The write's own afterwork then brings the block to its content.
func TestUpdateAfterworkWaitsForInFlightContentWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i414-inflight", i414Initial, nil)

	dataA := i414Content(i414ContentA)
	blockA := i414Update(t, f, id, dataA)

	// The in-flight content write: the row's content replaced, not committed.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	var holderPID int32
	if err := tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE context_blocks SET content = $1, updated_at = now() WHERE id = $2`, i414ContentB, id); err != nil {
		t.Fatalf("holder write: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i414-a")
	}()
	// Hook: release the holder once A's afterwork waits on it (or has
	// finished without waiting, which the assertions below then judge).
	waited := false
poll:
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		select {
		case <-done:
			break poll
		default:
		}
		if err := f.pool.QueryRow(f.ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&waited); err != nil {
			t.Fatalf("poll lock wait: %v", err)
		}
		if waited {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	<-done
	if !waited {
		t.Logf("A's afterwork finished without waiting on the in-flight write")
	}

	// A's afterwork wrote nothing: the block still has the data of the
	// content before A, neither column nor rows of A.
	i414AssertDerivedFrom(t, f, id, i414Initial, "after A's afterwork over the in-flight write")

	blockB, err := store.GetBlock(f.ctx, f.pool, nil, id, writableBlockScopes(f.directAR), nil)
	if err != nil || blockB == nil {
		t.Fatalf("read block B: %v", err)
	}
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, i414Content(i414ContentB), blockB, "i414-b")
	i414AssertDerivedFrom(t, f, id, i414ContentB, "after B's afterwork")
}

// TestUpdateAfterworkSingleUpdateWrites is the normal case: one content update
// and its afterwork write the data derived from the new content.
func TestUpdateAfterworkSingleUpdateWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i414-single", i414Initial, nil)

	// Content with a multi-byte character, so the version comparison sees the
	// bytes the database hashed.
	content := "Überprüfung am 2026-05-04 — Zusammenfassung folgt."
	data := i414Content(content)
	b := i414Update(t, f, id, data)
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, data, b, "i414")
	i414AssertDerivedFrom(t, f, id, content, "after the update")
}

// TestUpdateAfterworkTagsOnlyKeepsTemporal: an update that does not change the
// content (tags, metadata) leaves the temporal data of the content update
// before it standing — also when it commits between that content update and
// its afterwork.
func TestUpdateAfterworkTagsOnlyKeepsTemporal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)

	t.Run("after the afterwork", func(t *testing.T) {
		id := f.block(t, "i414-tags-after", i414Initial, nil)
		dataA := i414Content(i414ContentA)
		blockA := i414Update(t, f, id, dataA)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i414-a")

		dataC := store.UpdateBlockData{Tags: []string{"i414"}, MetadataMerge: map[string]any{"i414": "c"}}
		blockC := i414Update(t, f, id, dataC)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataC, blockC, "i414-c")
		i414AssertDerivedFrom(t, f, id, i414ContentA, "after the tags update")
	})

	t.Run("between the update and its afterwork", func(t *testing.T) {
		id := f.block(t, "i414-tags-between", i414Initial, nil)
		dataA := i414Content(i414ContentA)
		blockA := i414Update(t, f, id, dataA)

		dataC := store.UpdateBlockData{Tags: []string{"i414"}, MetadataMerge: map[string]any{"i414": "c"}}
		blockC := i414Update(t, f, id, dataC)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataC, blockC, "i414-c")

		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i414-a")
		i414AssertDerivedFrom(t, f, id, i414ContentA, "after A's afterwork")
	})
}

// TestStoreAfterworkStaleWriteSkipped is the store path's form of the issue:
// two upserts of one identity commit in order A, B and their afterwork
// (finishStoredBlock, the batch store's; /api/store, the store tool, ingest
// and the confirm of a staged store run the same store call) finishes B, A.
func TestStoreAfterworkStaleWriteSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	set := f.mcpCfg.Blocktypes.SnapshotForRequest(f.ctx)
	upsert := func(content string) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(f.ctx, f.pool, "i414", "i414-store", content, nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil || b == nil {
			t.Fatalf("upsert: %v", err)
		}
		return b
	}
	first := upsert(i414Initial)
	finishStoredBlock(f.ctx, f.pool, set, first, "i414-0")

	blockA := upsert(i414ContentA)
	blockB := upsert(i414ContentB)
	if blockA.ID != first.ID || blockB.ID != first.ID {
		t.Fatalf("upserts landed on different rows: %s %s %s", first.ID, blockA.ID, blockB.ID)
	}
	finishStoredBlock(f.ctx, f.pool, set, blockB, "i414-b")
	finishStoredBlock(f.ctx, f.pool, set, blockA, "i414-a")
	i414AssertDerivedFrom(t, f, first.ID, i414ContentB, "after A's late afterwork")
}
