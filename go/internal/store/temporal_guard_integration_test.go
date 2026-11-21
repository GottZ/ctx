//go:build integration

// Issue #414: the temporal writes (RefreshTemporal, PopulateTemporal) apply
// only while the block holds the content their dates were read from. The
// handler tests (update_afterwork_stale_integration_test.go) drive the
// afterwork of the write doors; these drive the two other callers — the
// dream's temporal review (PopulateTemporal with the content it picked) and
// the backfill — through the store functions themselves.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestPopulateTemporal|TestRefreshTemporal|TestBackfillTemporal' -count=1 -v
package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i414Sources is the sorted set of source times of a block's dimension rows.
func i414Sources(t *testing.T, pool *pgxpool.Pool, id string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT DISTINCT source_time FROM context_temporal WHERE block_id = $1 AND dimension <> '_none'`, id)
	if err != nil {
		t.Fatalf("read temporal rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s time.Time
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s.UTC().Format(time.RFC3339Nano))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	slices.Sort(out)
	return out
}

func i414Want(createdAt time.Time, dates ...time.Time) []string {
	out := []string{createdAt.UTC().Format(time.RFC3339Nano)}
	for _, d := range dates {
		out = append(out, d.UTC().Format(time.RFC3339Nano))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// i414LockWait bounds every wait of the tests with a second connection: for a
// write to start waiting on the held lock, and for it to finish once the lock
// is released.
const i414LockWait = 10 * time.Second

func i414Day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// TestPopulateTemporalSkipsReplacedContent is the dream's form of the race:
// the review reads a block, waits on its LLM, and writes the merged dates of
// the content it read. When an update replaced that content meanwhile (and
// its afterwork wrote the new content's data), the review writes nothing.
func TestPopulateTemporalSkipsReplacedContent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	picked := "Retro on 2026-02-10."
	b, err := store.UpsertBlock(ctx, pool, "i414", "i414-dream", picked, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}

	// The review read `picked`; an update replaces it and runs its afterwork.
	replaced := "Retro moved to 2026-06-30."
	u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &replaced}, []string{"private"})
	if err != nil || u == nil {
		t.Fatalf("update: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, u.ID, u.Content, u.CreatedAt); err != nil || !applied {
		t.Fatalf("update afterwork: applied=%v err=%v", applied, err)
	}

	// The review's late write: the picked content's dates plus a finding.
	merged := append(store.ExtractDates(picked), i414Day("2026-02-11"))
	applied, err := store.PopulateTemporal(ctx, pool, b.ID, picked, merged, b.CreatedAt)
	if err != nil {
		t.Fatalf("late populate: %v", err)
	}
	if applied {
		t.Errorf("late populate applied the dates of a replaced content")
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, i414Day("2026-06-30")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v (the update's content)", got, want)
	}

	// The same write for the content the block holds applies.
	merged = append(store.ExtractDates(replaced), i414Day("2026-07-01"))
	applied, err = store.PopulateTemporal(ctx, pool, b.ID, replaced, merged, b.CreatedAt)
	if err != nil || !applied {
		t.Fatalf("current populate: applied=%v err=%v", applied, err)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, i414Day("2026-06-30"), i414Day("2026-07-01")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v", got, want)
	}
}

// TestPopulateTemporalWaitsForInFlightContentWrite holds a content write open
// on a second connection while the dream's write (PopulateTemporal with the
// content it picked) runs: the guard has to wait for that write and, once it
// committed, must answer for the new content and drop the rows. A guard that
// read the row without locking it would see the committed old content, commit
// its rows, and leave them beside the new content's.
func TestPopulateTemporalWaitsForInFlightContentWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	picked := "Standup on 2026-04-14."
	b, err := store.UpsertBlock(ctx, pool, "i414", "i414-dream-inflight", picked, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	if _, err := holder.Exec(ctx, `UPDATE context_blocks SET content = $1, updated_at = now() WHERE id = $2`, "Standup moved to 2026-05-19.", b.ID); err != nil {
		t.Fatalf("holder write: %v", err)
	}

	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		merged := append(store.ExtractDates(picked), i414Day("2026-04-15"))
		applied, err := store.PopulateTemporal(ctx, pool, b.ID, picked, merged, b.CreatedAt)
		done <- result{applied, err}
	}()
	// Hook: release the holder only once the guard waits on it. A write that
	// finishes first did not wait for the in-flight write, and one that has
	// not waited by the deadline proves nothing either: both fail.
	waited := false
	for deadline := time.Now().Add(i414LockWait); !waited && time.Now().Before(deadline); {
		select {
		case r := <-done:
			t.Fatalf("populate finished (applied=%v err=%v) while the content write was still in flight: its guard did not wait for the row lock", r.applied, r.err)
		default:
		}
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&waited); err != nil {
			t.Fatalf("poll lock wait: %v", err)
		}
		if !waited {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !waited {
		t.Fatalf("populate never waited on the in-flight content write within %s", i414LockWait)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(i414LockWait):
		t.Fatalf("populate did not finish within %s after the content write committed", i414LockWait)
	}
	if r.err != nil {
		t.Fatalf("populate: %v", r.err)
	}
	if r.applied {
		t.Errorf("populate applied the picked content's dates over the content committed while it waited")
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, i414Day("2026-04-14")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v (the store afterwork's, untouched)", got, want)
	}
}

// TestRefreshTemporalYieldsToAContentWriteDuringItsRows: while the afterwork
// writes the dimension rows it holds no lock on the block row, so a content
// update of the block lands without waiting for it; the afterwork then finds
// the new content and drops both of its writes — content_times and rows — as
// one. A second connection holds the block's dimension rows, which keeps the
// afterwork inside its row writes until the test releases them.
func TestRefreshTemporalYieldsToAContentWriteDuringItsRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	initial := "Draft on 2026-01-20."
	b, err := store.UpsertBlock(ctx, pool, "i414", "i414-yield", initial, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}
	contentA := "Draft due 2026-03-03."
	a, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &contentA}, []string{"private"})
	if err != nil || a == nil {
		t.Fatalf("update A: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	if _, err := holder.Exec(ctx, `DELETE FROM context_temporal WHERE block_id = $1`, b.ID); err != nil {
		t.Fatalf("holder delete: %v", err)
	}

	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		applied, err := store.RefreshTemporal(ctx, pool, a.ID, a.Content, a.CreatedAt)
		done <- result{applied, err}
	}()
	// Hook: wait until A's afterwork waits on the held rows; one that
	// finishes first never reached them.
	blocked := false
	for deadline := time.Now().Add(i414LockWait); !blocked && time.Now().Before(deadline); {
		select {
		case r := <-done:
			t.Fatalf("A's afterwork finished (applied=%v err=%v) without waiting on the held dimension rows", r.applied, r.err)
		default:
		}
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
			t.Fatalf("poll lock wait: %v", err)
		}
		if !blocked {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !blocked {
		t.Fatalf("A's afterwork never waited on the held dimension rows")
	}

	// The block row is free: a lock attempt that refuses to wait succeeds.
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	if _, err := probe.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, b.ID); err != nil {
		_ = probe.Rollback(ctx)
		t.Fatalf("the afterwork holds the block row while it writes the dimension rows: %v", err)
	}
	_ = probe.Rollback(ctx)

	// So a content update lands now, while A's afterwork is mid-way.
	contentB := "Draft due 2026-08-08."
	bb, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &contentB}, []string{"private"})
	if err != nil || bb == nil {
		t.Fatalf("update B: %v", err)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(i414LockWait):
		t.Fatalf("A's afterwork did not finish within %s after the rows were released", i414LockWait)
	}
	if r.err != nil {
		t.Fatalf("A's afterwork: %v", r.err)
	}
	if r.applied {
		t.Errorf("A's afterwork applied over content B")
	}
	var times []time.Time
	if err := pool.QueryRow(ctx, `SELECT COALESCE(content_times, '{}') FROM context_blocks WHERE id = $1`, b.ID).Scan(&times); err != nil {
		t.Fatalf("read content_times: %v", err)
	}
	if len(times) != 1 || !times[0].Equal(i414Day("2026-01-20")) {
		t.Errorf("content_times = %v, want the initial content's [2026-01-20]: A's afterwork must drop content_times together with its rows", times)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, i414Day("2026-01-20")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v (the initial content's)", got, want)
	}

	if applied, err := store.RefreshTemporal(ctx, pool, bb.ID, bb.Content, bb.CreatedAt); err != nil || !applied {
		t.Fatalf("B's afterwork: applied=%v err=%v", applied, err)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, i414Day("2026-08-08")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v (content B)", got, want)
	}
}

// TestBackfillTemporalFillsBlocksWithoutRows: the backfill writes the data of
// the content a block holds to a block that has none, through RefreshTemporal.
func TestBackfillTemporalFillsBlocksWithoutRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	content := "Kick-off 2026-09-01, review 2026-09-15."
	b, err := store.UpsertBlock(ctx, pool, "i414", "i414-backfill", content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if got := i414Sources(t, pool, b.ID); len(got) != 0 {
		t.Fatalf("fixture already has temporal rows: %v", got)
	}

	n, err := store.BackfillTemporal(ctx, pool)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n < 1 {
		t.Errorf("backfill processed %d blocks, want at least 1", n)
	}
	if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, i414Day("2026-09-01"), i414Day("2026-09-15")); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v", got, want)
	}
	var times []time.Time
	if err := pool.QueryRow(ctx, `SELECT COALESCE(content_times, '{}') FROM context_blocks WHERE id = $1`, b.ID).Scan(&times); err != nil {
		t.Fatalf("read content_times: %v", err)
	}
	if len(times) != 2 {
		t.Errorf("content_times = %v, want the two dates of the content", times)
	}
}
