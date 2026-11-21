//go:build integration

// Issue #428, second point: BackfillTemporal skips a block whose content
// changed between its read and its write and relies on the afterwork of the
// write that changed it. When that afterwork did not run (it failed, or the
// writer has none) and the block was in the last batch, the backfill returned
// without the block having rows and reported nothing. The changes here are
// made by a content write without its afterwork, between the backfill's read
// and its write (a tracer on the backfill's pool), so the order does not
// depend on timing.
//
//	go test -tags=integration ./internal/store/ -run TestBackfillTemporalRereads -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i428Changer replaces a block's content (store.UpdateBlock, no afterwork)
// each time a read of the backfill that returned it has ended, up to max times.
type i428Changer struct {
	t        *testing.T
	pool     *pgxpool.Pool
	id       string
	max      int32
	changes  atomic.Int32
	contents []string
}

func (c *i428Changer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM context_blocks cb") && strings.Contains(data.SQL, "context_temporal ct") {
		return context.WithValue(ctx, i428ReadKey{}, true)
	}
	return ctx
}

type i428ReadKey struct{}

func (c *i428Changer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(i428ReadKey{}) == nil || data.Err != nil {
		return
	}
	n := c.changes.Add(1)
	if n > c.max {
		return
	}
	content := fmt.Sprintf("Changed after the backfill's read %d, on 2026-12-%02d.", n, n)
	if _, _, err := store.UpdateBlock(context.Background(), c.pool, c.id, store.UpdateBlockData{Content: &content}, []string{"private"}); err != nil {
		c.t.Errorf("content change %d: %v", n, err)
		return
	}
	c.contents = append(c.contents, content)
}

func i428BackfillPool(t *testing.T, dsn string, c *i428Changer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = c
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// i428Pending counts the live blocks without temporal rows: what a backfill
// has to write.
func i428Pending(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks b WHERE NOT b.is_archived
		   AND NOT EXISTS (SELECT 1 FROM context_temporal t WHERE t.block_id = b.id)`).Scan(&n); err != nil {
		t.Fatalf("count pending blocks: %v", err)
	}
	return n
}

func TestBackfillTemporalRereadsABlockWhoseContentChanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	t.Run("changed once", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "i428", "i428-backfill-once", "Read by the backfill, 2026-09-01.", nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		pending := i428Pending(t, pool)
		c := &i428Changer{t: t, pool: pool, id: b.ID, max: 1}
		n, err := store.BackfillTemporal(ctx, i428BackfillPool(t, dsn, c))
		if err != nil {
			t.Fatalf("backfill: %v", err)
		}
		if len(c.contents) != 1 {
			t.Fatalf("premise: %d content changes between the backfill's read and its write, want 1", len(c.contents))
		}
		if n != pending {
			t.Errorf("backfill processed %d blocks, want %d (every pending block, the changed one with its current content)", n, pending)
		}
		if got, want := i414Sources(t, pool, b.ID), i414Want(b.CreatedAt, store.ExtractDates(c.contents[0])...); !slices.Equal(got, want) {
			t.Errorf("source times = %v, want %v (the current content's)", got, want)
		}
		i421AssertContentTimes(t, pool, b.ID, c.contents[0], "after the backfill")
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1`, b.ID); err != nil {
			t.Fatalf("archive: %v", err)
		}
	})

	t.Run("changed on every read", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "i428", "i428-backfill-always", "Read by the backfill, 2026-09-02.", nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		pending := i428Pending(t, pool)
		c := &i428Changer{t: t, pool: pool, id: b.ID, max: 1000}
		n, err := store.BackfillTemporal(ctx, i428BackfillPool(t, dsn, c))
		if err == nil || !strings.Contains(err.Error(), "1 block") {
			t.Errorf("backfill of a block whose content changed on every read: n=%d err=%v; want an error that reports the one block left without rows", n, err)
		}
		if n != pending-1 {
			t.Errorf("backfill processed %d blocks, want %d (all pending blocks but the one that kept changing)", n, pending-1)
		}
		if got := i414Sources(t, pool, b.ID); len(got) != 0 {
			t.Errorf("source times = %v, want none (no write ever saw its current content)", got)
		}
		if got := c.changes.Load(); got < 2 {
			t.Errorf("the backfill read the block %d times, want it re-read after the first change", got)
		}
	})
}

// i428BackfillBound bounds a backfill run in TestBackfillTemporalReadsAFailingBlockOnce:
// a run that read its failing blocks again and again would not end by itself.
const i428BackfillBound = 30 * time.Second

// TestBackfillTemporalReadsAFailingBlockOnce: a block whose temporal write
// fails (a trigger of the fixture refuses its rows) is read once in a run and
// reported in the backfill's error. There are more such blocks than one batch
// holds, so a backfill that read them again would read the same full batch
// for ever; the run is bounded, and that fails the test instead of hanging it.
func TestBackfillTemporalReadsAFailingBlockOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const failing = 55 // more than one batch of the backfill (50)
	if _, err := pool.Exec(ctx, `
		CREATE TABLE i428_failing (block_id uuid PRIMARY KEY);
		CREATE FUNCTION i428_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF EXISTS (SELECT 1 FROM i428_failing WHERE block_id = NEW.block_id) THEN
		    RAISE EXCEPTION 'i428: temporal row of % refused', NEW.block_id;
		  END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER i428_refuse BEFORE INSERT ON context_temporal
		  FOR EACH ROW EXECUTE FUNCTION i428_refuse();`); err != nil {
		t.Fatalf("install the refusing trigger: %v", err)
	}
	items := make([]store.BatchUpsert, failing)
	for i := range items {
		items[i] = store.BatchUpsert{Category: "i428", Title: fmt.Sprintf("i428-failing-%02d", i),
			Content: fmt.Sprintf("Failing block %d, dated 2026-10-%02d.", i, 1+i%28), Scope: "private"}
	}
	res, err := store.UpsertBlocks(ctx, pool, items, "", nil, nil)
	if err != nil || len(res) != failing {
		t.Fatalf("write the failing blocks: %v (%d results)", err, len(res))
	}
	for _, r := range res {
		if _, err := pool.Exec(ctx, `INSERT INTO i428_failing VALUES ($1)`, r.Block.ID); err != nil {
			t.Fatalf("mark a failing block: %v", err)
		}
	}
	written, err := store.UpsertBlock(ctx, pool, "i428", "i428-writable", "Writable, dated 2026-11-11.", nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("write the writable block: %v", err)
	}
	pending := i428Pending(t, pool)

	runCtx, cancel := context.WithTimeout(ctx, i428BackfillBound)
	defer cancel()
	n, err := store.BackfillTemporal(runCtx, pool)
	if runCtx.Err() != nil {
		t.Fatalf("the backfill did not end within %s: it read its failing blocks again (n=%d err=%v)", i428BackfillBound, n, err)
	}
	want := fmt.Sprintf("%d block(s) left after an error", failing)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("backfill error = %v, want it to report %q", err, want)
	}
	if n != pending-failing {
		t.Errorf("backfill processed %d blocks, want %d (every pending block but the failing ones)", n, pending-failing)
	}
	if got := i414Sources(t, pool, written.ID); len(got) == 0 {
		t.Errorf("the writable block got no temporal rows")
	}
}
