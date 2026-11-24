//go:build integration

// Issue #434: the afterwork of a write takes the dates the write computed
// (RefreshBlockTemporal); every temporal writer without a write before it
// still extracts itself and writes what it wrote before — the startup
// backfill over blocks without rows, and the afterwork of a block that no
// write handed its dates to (one a read returned). The extractions are
// counted through the extraction every temporal writer uses
// (SetTemporalExtractionForTest), by content.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestTemporalHandover' -count=1 -v
package store_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// i434Counted stands a counting extraction in for the rest of the test and
// returns how often each content was extracted.
func i434Counted(t *testing.T) func(content string) int {
	t.Helper()
	var mu sync.Mutex
	n := map[string]int{}
	restore := store.SetTemporalExtractionForTest(func(content string) []time.Time {
		mu.Lock()
		n[content]++
		mu.Unlock()
		return store.ExtractDates(content)
	})
	t.Cleanup(restore)
	return func(content string) int {
		mu.Lock()
		defer mu.Unlock()
		return n[content]
	}
}

// i434ContentTimes reads the block's content_times as stored.
func i434ContentTimes(t *testing.T, id string, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) []time.Time {
	t.Helper()
	var times []time.Time
	if err := q.QueryRow(context.Background(), `SELECT COALESCE(content_times, '{}') FROM context_blocks WHERE id = $1`, id).Scan(&times); err != nil {
		t.Fatalf("content_times of %s: %v", id, err)
	}
	return times
}

// TestTemporalHandoverBackfillExtractsItself: the backfill writes the blocks
// no afterwork wrote rows for — one of the issue tracker, whose insert sets no
// content_times, and one of an internal writer that upserts without an
// afterwork (digest, dream, distill, root map) — from the extraction of the
// content it read: once per block, the content_times and the rows of that
// extraction.
func TestTemporalHandoverBackfillExtractsItself(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	count := i434Counted(t)

	const issueBody = "Issue body: due on 2031-04-05, review in Mai 2031."
	var issue *store.Block
	if err := i236Tx(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: "private", Title: "i434 issue", Content: issueBody}, []string{"private"})
		return err
	}); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	const internal = "Digest of the week, 12.06.2031."
	written, err := store.UpsertBlock(ctx, pool, "index", "i434 internal", internal, nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := i434ContentTimes(t, issue.ID, pool); len(got) != 0 {
		t.Fatalf("the issue insert set content_times %v; the backfill case needs a block without them", got)
	}
	if got := count(issueBody); got != 0 {
		t.Fatalf("the issue insert extracted %d times", got)
	}
	before := count(internal)

	if _, err := store.BackfillTemporal(ctx, pool); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	for _, c := range []struct {
		b       *store.Block
		content string
		before  int
	}{{issue, issueBody, 0}, {written, internal, before}} {
		if got := count(c.content) - c.before; got != 1 {
			t.Errorf("%s: the backfill extracted its content %d times, want 1", c.b.Title, got)
		}
		dates := store.ExtractDates(c.content)
		if got := i434ContentTimes(t, c.b.ID, pool); !slices.EqualFunc(got, dates, time.Time.Equal) {
			t.Errorf("%s: content_times = %v, want the extraction %v", c.b.Title, got, dates)
		}
		if got, want := i414Sources(t, pool, c.b.ID), i414Want(c.b.CreatedAt, dates...); !slices.Equal(got, want) {
			t.Errorf("%s: source times = %v, want %v", c.b.Title, got, want)
		}
	}
}

// TestTemporalHandoverReadBlockExtracts: a block a read returned carries no
// dates of a write, so its afterwork extracts the content it holds, once,
// and writes what RefreshTemporal writes for it.
func TestTemporalHandoverReadBlockExtracts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const content = "Read back: the launch on 2031-07-08."
	w, err := store.UpsertBlock(ctx, pool, "i434", "read back", content, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	count := i434Counted(t)
	read, err := store.GetBlock(ctx, pool, nil, w.ID, []string{"private"}, nil)
	if err != nil || read == nil {
		t.Fatalf("read: %v %v", read, err)
	}
	if applied, err := store.RefreshBlockTemporal(ctx, pool, read); err != nil || !applied {
		t.Fatalf("afterwork: applied=%v err=%v", applied, err)
	}
	if got := count(content); got != 1 {
		t.Errorf("the afterwork of a read block extracted %d times, want 1", got)
	}
	if got, want := i414Sources(t, pool, w.ID), i414Want(w.CreatedAt, store.ExtractDates(content)...); !slices.Equal(got, want) {
		t.Errorf("source times = %v, want %v", got, want)
	}
}

// TestTemporalHandoverWritesExtractOnce: each store function that writes a
// content extracts it once, and the afterwork of the block it returns not
// again — the count the handler doors see, here per write function.
func TestTemporalHandoverWritesExtractOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	count := i434Counted(t)
	scopes := []string{"private"}
	seed, err := store.UpsertBlock(ctx, pool, "i434", "update target", "Seed text.", nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	source := i434Source(t, pool)
	type write struct {
		name    string
		content string
		run     func(content string) (*store.Block, error)
	}
	repl := "on 2031-09-10"
	for _, w := range []write{
		{"UpsertBlock", "Upserted on 2031-01-02.", func(c string) (*store.Block, error) {
			return store.UpsertBlock(ctx, pool, "i434", "upserted", c, nil, nil, "private", false, store.SensitivityWrite{}, "")
		}},
		{"UpdateBlock content", "Updated on 2031-03-04.", func(c string) (*store.Block, error) {
			b, _, err := store.UpdateBlock(ctx, pool, seed.ID, store.UpdateBlockData{Content: &c}, scopes)
			return b, err
		}},
		{"UpdateBlock edits", "Updated on 2031-09-10.", func(string) (*store.Block, error) {
			b, _, err := store.UpdateBlock(ctx, pool, seed.ID, store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "on 2031-03-04", New: &repl}}}, scopes)
			return b, err
		}},
		{"UpsertBlocks", "Batch item on 2031-05-06.", func(c string) (*store.Block, error) {
			res, err := store.UpsertBlocks(ctx, pool, []store.BatchUpsert{{Category: "i434", Title: "batched", Content: c, Scope: "private"}}, "", nil, nil)
			if err != nil {
				return nil, err
			}
			return res[0].Block, nil
		}},
		{"InsertChunk", "Chunk on 2031-07-08.", func(c string) (*store.Block, error) {
			return store.InsertChunk(ctx, pool, source, 0, "i434", "chunk", c, nil, nil, "private")
		}},
	} {
		t.Run(w.name, func(t *testing.T) {
			b, err := w.run(w.content)
			if err != nil || b == nil {
				t.Fatalf("write: %v %v", b, err)
			}
			if got := count(w.content); got != 1 {
				t.Errorf("the write extracted %d times, want 1", got)
			}
			if applied, err := store.RefreshBlockTemporal(ctx, pool, b); err != nil || !applied {
				t.Fatalf("afterwork: applied=%v err=%v", applied, err)
			}
			if got := count(w.content); got != 1 {
				t.Errorf("write and afterwork extracted %d times, want 1", got)
			}
			if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, store.ExtractDates(w.content)...); !slices.Equal(got, want) {
				t.Errorf("source times = %v, want %v", got, want)
			}
		})
	}
}

// i434Source inserts a context_sources row in scope private for a chunk.
func i434Source(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/i434/chunk.md', 'i434', 'private') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return id
}
