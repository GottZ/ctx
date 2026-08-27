//go:build integration

// Gate A02-6, probe 8 — the MEASURING POINT for wave A02-M1 (design/02 §7.2,
// last line): rows_dropped_dup/rows_seen and rows_dropped_cred/rows_seen over
// the real corpus.
//
// WHY IT IS AN ENV-GATED TEST AND NOT A SCRIPT. The numbers come out of the arm
// itself, and the arm needs a pgx POOL — the read-only psql access this project
// grants against the live database cannot drive it, and pointing it AT the live
// pool would be a write path (distill_run, distill_seen) into a production
// database. So the corpus is copied into a throwaway testcontainer and measured
// there: same code, same reader, same selection, no live write.
//
// The excerpt is handed in as a plain COPY file, and where it comes from is the
// operator's business — it never lives in the repository (BA13), and the dump
// is switched OFF for this run, so nothing but counters leaves it.
//
// The filters are the ones the measurement was actually run with — scope and
// category included, because the reader reads exactly that slice and an excerpt
// wider than the reader's own predicate would count blocks the arm never sees.
//
//	bash /compose/n8n/.gotmp/psqlctx.sh -c "COPY (SELECT id, category, title, \
//	  content, scope, type_name, metadata, created_at FROM context_blocks \
//	  WHERE type_name='checkpoint' AND NOT is_archived \
//	    AND scope='private' AND category='compaction-checkpoints') TO STDOUT" \
//	  > /var/lib/ctx/a02-6-m1/blocks.copy
//	CTX_A02_6_M1_COPY=/var/lib/ctx/a02-6-m1/blocks.copy \
//	  go test -tags=integration ./internal/events/ -run TestDistillMeasureM1 -count=1 -v -timeout 30m
package events

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestDistillMeasureM1(t *testing.T) {
	path := os.Getenv("CTX_A02_6_M1_COPY")
	if path == "" {
		t.Skip("CTX_A02_6_M1_COPY unset — the measuring point needs a corpus excerpt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)
	dfTruncate(t, pool)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open excerpt: %v", err)
	}
	defer f.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	start := time.Now()
	tag, err := conn.Conn().PgConn().CopyFrom(ctx, f, `COPY context_blocks
	    (id, category, title, content, scope, type_name, metadata, created_at) FROM STDIN`)
	if err != nil {
		t.Fatalf("copy excerpt: %v", err)
	}
	t.Logf("excerpt: %d blocks in %s", tag.RowsAffected(), time.Since(start).Round(time.Millisecond))

	var roots int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT metadata->>'root_session_id') FROM context_blocks
		 WHERE type_name = 'checkpoint' AND metadata ? 'source_block_ids'`).Scan(&roots); err != nil {
		t.Fatalf("count roots: %v", err)
	}

	cfg := a6Config("", 4000, 200) // dump OFF — the measurement is the ledger
	cfg.Distill.MaxSessionsPerRun = roots + 10
	cfg.Distill.CtxSessionHorizon = 0 // no horizon: the whole excerpt is the corpus
	run := time.Now()
	dfScheduler(pool, cfg, nil).distillOnce(ctx, dfNoDemand)
	elapsed := time.Since(run)

	var runs, seen, selected, cred, dup int64
	var chars int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(rows_seen), 0), COALESCE(sum(rows_selected), 0),
		       COALESCE(sum(rows_dropped_cred), 0), COALESCE(sum(rows_dropped_dup), 0),
		       COALESCE(sum(chars_selected), 0)
		  FROM distill_run WHERE outcome <> 'skipped'`).
		Scan(&runs, &seen, &selected, &cred, &dup, &chars); err != nil {
		t.Fatalf("aggregate ledger: %v", err)
	}
	if seen == 0 {
		t.Fatal("the instrument measured nothing — a report over it would be worthless")
	}
	pct := func(n int64) float64 { return 100 * float64(n) / float64(seen) }
	t.Logf("A02-M1 measuring point over %d roots / %d runs in %s:", roots, runs, elapsed.Round(time.Millisecond))
	t.Logf("  rows_seen        = %d chunks", seen)
	t.Logf("  rows_selected    = %d (%.2f %%)", selected, pct(selected))
	t.Logf("  rows_dropped_dup = %d (%.2f %%)", dup, pct(dup))
	t.Logf("  rows_dropped_cred= %d (%.2f %%)", cred, pct(cred))
	t.Logf("  below min_row_runes = %d (%.2f %%)", seen-selected-dup-cred, pct(seen-selected-dup-cred))
	t.Logf("  chars_selected   = %d runes", chars)
}
