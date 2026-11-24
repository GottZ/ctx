//go:build integration

package store_test

// The other readers of the order updated_at DESC (issue #237, not a gate):
// the measurement behind the sentence each gets in the issue's close. On a
// store of CTX_I237_ROWS blocks (default 1000000, grown as in
// TestBrowseIndexMeasure) and as many blobs (1 in 5 in one scope, 1 000 in
// another, the rest over 100 scopes), it EXPLAINs the statements
// store.ListBlobs and store.ResolveBlockID send (traced from the store
// functions) for a reader of the big and of the small scope, at
// random_page_cost 1.1 and 4, custom and generic plan, with and without the
// indexes of migration 195. Set CTX_I237_OTHERS_MEASURE=1.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBrowseIndexOtherReadersMeasure(t *testing.T) {
	if os.Getenv("CTX_I237_OTHERS_MEASURE") == "" {
		t.Skip("measurement; set CTX_I237_OTHERS_MEASURE=1")
	}
	ctx := context.Background()
	rows := 1000000
	if v := os.Getenv("CTX_I237_ROWS"); v != "" {
		if _, err := fmt.Sscan(v, &rows); err != nil {
			t.Fatal(err)
		}
	}
	pool := testdb.SetupTestDB(t)
	i237Seed(t, pool, 0, rows)
	t0 := time.Now()
	m182Exec(t, pool, `INSERT INTO context_blobs (category, title, filename, mime_type, file_size, storage_type, file_path, scope, created_at, updated_at)
		SELECT 'blobs', 'blob ' || g, 'f' || g || '.bin', 'application/octet-stream', 1024, 'fs', '/blobs/' || g,
		       CASE WHEN g % ($1::int / 1000) = 0 THEN 'i237-small'
		            WHEN (g::bigint * 2654435761) % 5 = 0 THEN 'i237-big'
		            ELSE 'i237-o' || lpad((g % 100)::text, 3, '0') END,
		       ts, ts
		  FROM (SELECT g, timestamptz '2026-11-01 00:00:00+00' - g * interval '1 second' AS ts FROM generate_series(1, $1::int) g) s`, rows)
	m182Exec(t, pool, `VACUUM ANALYZE context_blobs`)
	t.Logf("blobs seeded in %s", time.Since(t0).Round(time.Second))
	var prefix string
	if err := pool.QueryRow(ctx, `SELECT left(id::text, 8) FROM context_blocks WHERE scope = 'i237-big' ORDER BY id LIMIT 1 OFFSET 1000`).Scan(&prefix); err != nil {
		t.Fatal(err)
	}
	tr := &listTracer{}
	tp := listTracedPool(t, pool, tr)
	// The browse of a reader of every scope, at the largest page (50 rows and
	// the probe row): the bound K x LIMIT of the walk at its widest.
	r, err := pool.Query(ctx, `SELECT DISTINCT scope FROM context_blocks WHERE scope LIKE 'i237-%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	every, err := pgx.CollectRows(r, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{10, 50} {
		if _, _, err := store.SearchBlocksPage(ctx, tp, nil, "", every, "", nil, nil, limit, true, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		sql, args := tr.last(t)
		for _, rpc := range testdb.RandomPageCosts {
			rp := testdb.WithRandomPageCost(t, pool, rpc)
			for _, plan := range []string{"custom", "generic"} {
				explain := testdb.ExplainCustom
				if plan == "generic" {
					explain = testdb.ExplainGeneric
				}
				pc := explain(t, rp, sql, args...)
				t.Logf("MEASURE-WIDE|rows=%d|scopes=%d|limit=%d|rpc=%s|plan=%s|buffers=%d|ms=%.1f", rows, len(every), limit, rpc, plan, pc.Buffers, pc.ExecMs)
			}
		}
	}
	for _, phase := range []string{"with", "without"} {
		if phase == "without" {
			for _, ix := range m195Indexes {
				m182Exec(t, pool, `DROP INDEX IF EXISTS `+ix.name)
			}
			m182Exec(t, pool, `ANALYZE context_blocks`)
		}
		for _, reader := range []string{"i237-big", "i237-small"} {
			scopes := []string{reader}
			if _, err := store.ListBlobs(ctx, tp, scopes, 50); err != nil {
				t.Fatal(err)
			}
			blobSQL, blobArgs := tr.lastOn(t, "FROM context_blobs")
			// A prefix of eight characters of a uuidv7 is its time: it
			// matches many blocks, and the ambiguous answer is the normal one.
			if _, _, err := store.ResolveBlockID(ctx, tp, prefix, scopes, nil); err != nil && !errors.Is(err, store.ErrAmbiguousID) {
				t.Fatal(err)
			}
			resolveSQL, resolveArgs := tr.last(t)
			for _, rpc := range testdb.RandomPageCosts {
				rp := testdb.WithRandomPageCost(t, pool, rpc)
				for _, plan := range []string{"custom", "generic"} {
					explain := testdb.ExplainCustom
					if plan == "generic" {
						explain = testdb.ExplainGeneric
					}
					b := explain(t, rp, blobSQL, blobArgs...)
					r := explain(t, rp, resolveSQL, resolveArgs...)
					t.Logf("MEASURE-OTHERS|rows=%d|indexes=%s|reader=%s|rpc=%s|plan=%s|ListBlobs=%d buffers %.1f ms|ResolveBlockID=%d buffers %.1f ms",
						rows, phase, reader, rpc, plan, b.Buffers, b.ExecMs, r.Buffers, r.ExecMs)
					if os.Getenv("CTX_I237_PLANS") != "" {
						t.Logf("PLAN ListBlobs\n%s\nPLAN ResolveBlockID\n%s", i237PlanShape(b.Plan), i237PlanShape(r.Plan))
					}
				}
			}
		}
	}
}
