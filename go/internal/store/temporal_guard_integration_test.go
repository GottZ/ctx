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
