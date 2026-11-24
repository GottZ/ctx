//go:build integration

package store_test

// Migration 194, part 1 (issue #271): idx_blocks_dream_eligible holds the
// dream-eligible rows (the predicate of dream.dreamEligibleWhere without its
// type list) under (scope, type_name), the read of dream.QueueDepth. Built
// inline below 500 000 rows under SHARE (a reader goes on, a writer waits);
// above, on an INVALID index of the name, a same-named index of another
// definition and a relation of the name that is not an index the file warns
// and takes no lock on context_blocks, and after the runbook it adopts the
// index. Its columns are columns other indexes of context_blocks already
// carry, so it takes no update off the HOT path.
//
//	go test -tags=integration ./internal/store/ -run 'TestMigration194DreamIndex|TestDreamEligibleIndexTakesNoUpdateOffHOT' -count=1 -v

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	dreamEligibleIndex = "idx_blocks_dream_eligible"
	dreamEligibleDef   = `CREATE INDEX idx_blocks_dream_eligible ON public.context_blocks USING btree (scope, type_name) WHERE ((NOT is_archived) AND ((lifecycle_state)::text = ANY ((ARRAY['knowledge'::character varying, 'canonical'::character varying, 'synthesis'::character varying])::text[])))`
	// m194DDL is the out-of-band build of the header of 194.
	m194DDL   = `CREATE INDEX CONCURRENTLY idx_blocks_dream_eligible ON context_blocks (scope, type_name) WHERE NOT is_archived AND lifecycle_state IN ('knowledge', 'canonical', 'synthesis')`
	m194Read  = `SELECT count(*) FROM context_blocks WHERE category = 'm194'`
	m194Write = `UPDATE context_blocks SET updated_at = updated_at WHERE category = 'm194'`
)

// m194DreamSeed writes, through the production write path, blocks the index
// holds (knowledge in two scopes, a synthesis block) and blocks it leaves out
// (an archived one, a snapshot), and returns how many it holds.
func m194DreamSeed(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	write := func(scope, title string) string {
		b, err := store.UpsertBlock(ctx, pool, "m194", title, "body of "+title, nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("write %s: %v", title, err)
		}
		return b.ID
	}
	write("m194-a", "m194 one")
	write("m194-b", "m194 two")
	syn := write("m194-a", "m194 synthesis")
	m182Exec(t, pool, `UPDATE context_blocks SET lifecycle_state = 'synthesis' WHERE id = $1`, syn)
	snap := write("m194-a", "m194 snapshot")
	m182Exec(t, pool, `UPDATE context_blocks SET lifecycle_state = 'snapshot' WHERE id = $1`, snap)
	if _, err := store.DeleteBlock(ctx, pool, write("m194-b", "m194 archived"), []string{"m194-b"}); err != nil {
		t.Fatal(err)
	}
	return 3
}

func TestMigration194DreamIndex(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration194File)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 193)
		want := m194DreamSeed(t, pool)
		if err := store.RunMigrationsUpTo(ctx, pool, 194); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		ix := m182Index(t, pool, dreamEligibleIndex)
		if ix.def != dreamEligibleDef || !ix.valid {
			t.Errorf("%s deparses as\n  %s (valid %v)\nwant\n  %s", dreamEligibleIndex, ix.def, ix.valid, dreamEligibleDef)
		}
		if ix.entries != want {
			t.Errorf("%s holds %d entries, want %d (the live blocks of the dream lifecycles)", dreamEligibleIndex, ix.entries, want)
		}
	})

	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 193)
		m194DreamSeed(t, pool)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m194Read, m194Write)
		t.Logf("migration 194 inline: %s; locks %v; reader waited %v, writer waited %v", run.took, run.modes, run.readerWaited, run.writerWaited)
		if run.err != nil {
			t.Fatal(run.err)
		}
		if !m182Holds(run.modes, "ShareLock") || m182Holds(run.modes, "AccessExclusiveLock") || m182Holds(run.modes, "ExclusiveLock") {
			t.Errorf("the inline build holds %v on context_blocks, want SHARE and nothing stronger", run.modes)
		}
		if run.readerWaited || !run.writerWaited {
			t.Errorf("reader waited %v (want false), writer waited %v (want true)", run.readerWaited, run.writerWaited)
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings: %v", run.warnings)
		}
		before := m182Index(t, pool, dreamEligibleIndex)
		again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m194Read, m194Write)
		if again.err != nil || len(again.warnings) != 0 || len(again.modes) != 0 {
			t.Errorf("second application: err %v, warnings %v, locks %v; want a no-op", again.err, again.warnings, again.modes)
		}
		if after := m182Index(t, pool, dreamEligibleIndex); after.oid != before.oid {
			t.Errorf("second application rebuilt the index: oid %d -> %d", before.oid, after.oid)
		}
	})

	for _, c := range []struct {
		name      string
		setup     []string
		reltuples float64
		clear     string
	}{
		{"above_threshold", nil, 600000, ""},
		{"invalid", []string{m194DDL[len("CREATE INDEX CONCURRENTLY "):], `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_blocks_dream_eligible'::regclass`}, 0,
			`DROP INDEX CONCURRENTLY idx_blocks_dream_eligible`},
		{"foreign_definition", []string{`idx_blocks_dream_eligible ON context_blocks (scope, type_name) WHERE NOT is_archived`}, 0,
			`ALTER INDEX idx_blocks_dream_eligible RENAME TO idx_blocks_dream_eligible_foreign`},
		{"view", []string{`CREATE VIEW idx_blocks_dream_eligible AS SELECT 1 AS x`}, 0,
			`ALTER VIEW idx_blocks_dream_eligible RENAME TO idx_blocks_dream_eligible_foreign`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 193)
			m194DreamSeed(t, pool)
			if c.reltuples > 0 {
				m193Analysed(t, pool)
			}
			for _, s := range c.setup {
				if !strings.HasPrefix(s, "UPDATE") && !strings.HasPrefix(s, "CREATE") {
					s = "CREATE INDEX " + s
				}
				m182Exec(t, pool, s)
			}
			before := m182Index(t, pool, dreamEligibleIndex)
			kind := m18xRelkind(t, pool, dreamEligibleIndex)
			run := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m194Read, m194Write)
			t.Logf("%s: %s; locks %v; warnings %v", c.name, run.took, run.modes, run.warnings)
			if run.err != nil {
				t.Fatal(run.err)
			}
			if m18xWarns(run, dreamEligibleIndex) != 1 {
				t.Errorf("warnings %v, want one naming %s", run.warnings, dreamEligibleIndex)
			}
			if m182Holds(run.modes, "ShareLock") || run.writerWaited {
				t.Errorf("the file holds %v on context_blocks (writer waited %v)", run.modes, run.writerWaited)
			}
			if after := m182Index(t, pool, dreamEligibleIndex); after.found != before.found || after.oid != before.oid {
				t.Errorf("the file changed the index: %+v -> %+v", before, after)
			}
			if k := m18xRelkind(t, pool, dreamEligibleIndex); k != kind {
				t.Errorf("the file changed what holds the name: relkind %q -> %q", kind, k)
			}
			if c.clear != "" {
				m182Exec(t, pool, c.clear)
			}
			m182Exec(t, pool, m194DDL)
			again := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m194Read, m194Write)
			if again.err != nil || m18xWarns(again, dreamEligibleIndex) != 0 || len(again.modes) != 0 {
				t.Errorf("after the runbook: err %v, warnings %v, locks %v", again.err, again.warnings, again.modes)
			}
			if ix := m182Index(t, pool, dreamEligibleIndex); ix.def != dreamEligibleDef || !ix.valid {
				t.Errorf("after the runbook: %s (valid %v)", ix.def, ix.valid)
			}
		})
	}
}

// TestDreamEligibleIndexTakesNoUpdateOffHOT: PostgreSQL writes an update
// without new index entries (HOT) only when no column that an index's key,
// expression or predicate names changes. Every column idx_blocks_dream_eligible
// names is named by another index of context_blocks already, so no update
// that was HOT before 194 stops being HOT — not the dream pick's claim
// (dream_cooldown_until alone), not RestampBackoff. A key with a dream column
// (the variant the issue rejected) fails here.
func TestDreamEligibleIndexTakesNoUpdateOffHOT(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	cols := func(where string) map[string]bool {
		t.Helper()
		rows, err := pool.Query(context.Background(), `
			SELECT DISTINCT a.attname::text
			  FROM pg_index i
			  JOIN pg_class ic ON ic.oid = i.indexrelid
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum > 0 AND NOT a.attisdropped
			 WHERE i.indrelid = 'public.context_blocks'::regclass AND `+where+`
			   AND (a.attnum = ANY(i.indkey)
			        OR COALESCE(pg_get_expr(i.indpred, i.indrelid), '') ~ ('\m' || a.attname || '\M')
			        OR COALESCE(pg_get_expr(i.indexprs, i.indrelid), '') ~ ('\m' || a.attname || '\M'))`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			out[c] = true
		}
		return out
	}
	mine := cols(`ic.relname = '` + dreamEligibleIndex + `'`)
	others := cols(`ic.relname <> '` + dreamEligibleIndex + `'`)
	if len(mine) == 0 {
		t.Fatalf("%s names no column: the index is missing", dreamEligibleIndex)
	}
	for c := range mine {
		if !others[c] {
			t.Errorf("%s names %s, which no other index of context_blocks names: an update of it is no longer HOT", dreamEligibleIndex, c)
		}
	}
	// The dream pick's claim and RestampBackoff write dream_cooldown_until
	// alone (dream.pickBlockSQL, dream.RestampBackoff).
	if others["dream_cooldown_until"] || mine["dream_cooldown_until"] {
		t.Errorf("an index of context_blocks names dream_cooldown_until: the dream pick's claim and RestampBackoff lose their HOT updates")
	}
}

// TestMigration194DreamIndexInlineBuildWriterWait is the measurement behind
// the threshold of part 1 of 194 (not a gate): a store at 193 grown to each
// size of CTX_M194_ROWS (default 250000,500000) of rows of live width (about
// 1.5 KB of heap; per row of a linkable type three of other types, 1 in 13
// archived, 1 in 10 in a lifecycle outside the predicate), a writer
// inserting a block every 2 ms, and the file's CREATE INDEX in one
// transaction, three times per size; it logs the build time and the longest
// wait of the writer. Set CTX_M194_DREAM_MEASURE=1.
func TestMigration194DreamIndexInlineBuildWriterWait(t *testing.T) {
	if os.Getenv("CTX_M194_DREAM_MEASURE") == "" {
		t.Skip("measurement; set CTX_M194_DREAM_MEASURE=1")
	}
	ctx := context.Background()
	sizes := []int{250000, 500000}
	if v := os.Getenv("CTX_M194_ROWS"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			var n int
			if _, err := fmt.Sscan(f, &n); err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, n)
		}
	}
	build := "CREATE INDEX " + m194DDL[len("CREATE INDEX CONCURRENTLY "):]
	pool := testdb.SetupTestDBUpTo(t, 193)
	m182Exec(t, pool, `DROP INDEX IF EXISTS idx_embedding_hnsw`)
	have := 0
	for _, rows := range sizes {
		t0 := time.Now()
		m182Exec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
		m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`)
		m182Exec(t, pool, `INSERT INTO context_blocks (category, title, content, scope, type_name, lifecycle_state, is_archived, metadata, created_at, updated_at)
			SELECT 'm194w', 'm194w ' || g, repeat('lorem ipsum dolor sit amet ', 40) || g, 'm194-' || (g % 50),
			       CASE WHEN g % 4 = 0 THEN 'knowledge' ELSE 'm194-type-' || (g % 3) END,
			       CASE WHEN g % 10 = 0 THEN 'snapshot' ELSE 'knowledge' END, g % 13 = 0,
			       jsonb_build_object('pad', repeat('m', 260)), now() - g * interval '1 second', now()
			  FROM generate_series($1::int + 1, $2::int) g`, have, rows)
		have = rows
		m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
		m182Exec(t, pool, `VACUUM ANALYZE context_blocks`)
		var heap int64
		if err := pool.QueryRow(ctx, `SELECT pg_relation_size('context_blocks')`).Scan(&heap); err != nil {
			t.Fatal(err)
		}
		t.Logf("seed to %d rows, heap %d MB, in %s", rows, heap>>20, time.Since(t0).Round(time.Second))
		for round := 1; round <= 3; round++ {
			m182Exec(t, pool, `DROP INDEX IF EXISTS idx_blocks_dream_eligible`)
			stop := make(chan struct{})
			var longest time.Duration
			var writes int
			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					w0 := time.Now()
					if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('m194w', $1, 'writer', 'm194-w')`, fmt.Sprintf("m194 writer %d %d %d", rows, round, i)); err != nil {
						t.Errorf("writer: %v", err)
						return
					}
					if d := time.Since(w0); d > longest {
						longest = d
					}
					writes++
					time.Sleep(2 * time.Millisecond)
				}
			}()
			time.Sleep(200 * time.Millisecond)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
				t.Fatal(err)
			}
			b0 := time.Now()
			if _, err := tx.Exec(ctx, build); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			took := time.Since(b0)
			time.Sleep(200 * time.Millisecond)
			close(stop)
			<-done
			ix := m182Index(t, pool, dreamEligibleIndex)
			var size string
			_ = pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_relation_size('idx_blocks_dream_eligible'))`).Scan(&size)
			var load string
			if b, err := os.ReadFile("/proc/loadavg"); err == nil {
				load = strings.Join(strings.Fields(string(b))[:3], " ")
			}
			t.Logf("MEASURE|rows=%d|heap_mb=%d|round=%d|build=%s|writer_longest_wait=%s|writes=%d|index_entries=%d|index_size=%s|valid=%v|load=%s",
				rows, heap>>20, round, took.Round(time.Millisecond), longest.Round(time.Millisecond), writes, ix.entries, size, ix.valid, load)
			if !ix.valid || ix.def != dreamEligibleDef {
				t.Errorf("the build left %s (valid=%v)", ix.def, ix.valid)
			}
		}
	}
}
