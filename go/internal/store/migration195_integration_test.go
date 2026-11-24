//go:build integration

package store_test

// Migration 195 (issue #237): idx_blocks_scope_updated and
// idx_blocks_scope_category_updated, the ranges browse and recent read per
// scope (store/blocks_list.go). Built inline below 500 000 rows under SHARE
// (a reader goes on, a writer waits); above, on an INVALID index of a name,
// a same-named index of another definition and a relation of a name that is
// not an index the file warns and takes no lock on context_blocks, and after
// the runbook it adopts the index. Their columns are columns other indexes
// of context_blocks already carry, so they take no update off the HOT path,
// and they name no embedding column, so they stay outside the re-embed
// cutover.
//
//	go test -tags=integration ./internal/store/ -run 'TestMigration195|TestBrowseIndexes' -count=1 -v

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const migration195File = "195_blocks_scope_updated_index.sql"

// m195Index is one index of migration 195: its name, its definition as
// PostgreSQL deparses it, and the runbook's out-of-band build.
type m195Index struct {
	name, def, ddl string
}

var m195Indexes = []m195Index{
	{"idx_blocks_scope_updated",
		`CREATE INDEX idx_blocks_scope_updated ON public.context_blocks USING btree (scope, updated_at DESC, id DESC) WHERE (NOT is_archived)`,
		`CREATE INDEX CONCURRENTLY idx_blocks_scope_updated ON context_blocks (scope, updated_at DESC, id DESC) WHERE NOT is_archived`},
	{"idx_blocks_scope_category_updated",
		`CREATE INDEX idx_blocks_scope_category_updated ON public.context_blocks USING btree (scope, category, updated_at DESC, id DESC) WHERE (NOT is_archived)`,
		`CREATE INDEX CONCURRENTLY idx_blocks_scope_category_updated ON context_blocks (scope, category, updated_at DESC, id DESC) WHERE NOT is_archived`},
}

const (
	m195Read  = `SELECT count(*) FROM context_blocks WHERE category = 'm195'`
	m195Write = `UPDATE context_blocks SET updated_at = updated_at WHERE category = 'm195'`
)

// m195Seed writes, through the production write path, blocks the indexes
// hold (in two scopes) and one they leave out (archived), and returns how
// many they hold.
func m195Seed(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	write := func(scope, title string) string {
		b, err := store.UpsertBlock(ctx, pool, "m195", title, "body of "+title, nil, nil, scope, true, store.SensitivityWrite{}, "knowledge")
		if err != nil {
			t.Fatalf("write %s: %v", title, err)
		}
		return b.ID
	}
	write("m195-a", "m195 one")
	write("m195-a", "m195 two")
	write("m195-b", "m195 three")
	if _, err := store.DeleteBlock(ctx, pool, write("m195-b", "m195 archived"), []string{"m195-b"}); err != nil {
		t.Fatal(err)
	}
	return 3
}

// m195Listing is the browse page of m195-a and m195-b, through the store.
func m195Listing(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := store.SearchBlocks(context.Background(), pool, nil, "", []string{"m195-a", "m195-b"}, "", nil, 50, true, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Title
	}
	return out
}

func TestMigration195(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration195File)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 194)
		want := m195Seed(t, pool)
		before := m195Listing(t, pool)
		if err := store.RunMigrationsUpTo(ctx, pool, 195); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		for _, ix := range m195Indexes {
			got := m182Index(t, pool, ix.name)
			if got.def != ix.def || !got.valid {
				t.Errorf("%s deparses as\n  %s (valid %v)\nwant\n  %s", ix.name, got.def, got.valid, ix.def)
			}
			if got.entries != want {
				t.Errorf("%s holds %d entries, want %d (the live blocks)", ix.name, got.entries, want)
			}
		}
		store.ResetListIndexReadingsForTest()
		if after := m195Listing(t, pool); strings.Join(after, "|") != strings.Join(before, "|") {
			t.Errorf("the listing on 195 is %v, on 194 it was %v", after, before)
		}
		// Applied twice by the runner: nothing to do.
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations again: %v", err)
		}
	})

	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 194)
		m195Seed(t, pool)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m195Read, m195Write)
		t.Logf("migration 195 inline: %s; locks %v; reader waited %v, writer waited %v", run.took, run.modes, run.readerWaited, run.writerWaited)
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
		before := map[string]uint32{}
		for _, ix := range m195Indexes {
			got := m182Index(t, pool, ix.name)
			if got.def != ix.def || !got.valid {
				t.Errorf("%s: %s (valid %v)", ix.name, got.def, got.valid)
			}
			before[ix.name] = got.oid
		}
		again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m195Read, m195Write)
		if again.err != nil || len(again.warnings) != 0 || len(again.modes) != 0 {
			t.Errorf("second application: err %v, warnings %v, locks %v; want a no-op", again.err, again.warnings, again.modes)
		}
		for _, ix := range m195Indexes {
			if after := m182Index(t, pool, ix.name); after.oid != before[ix.name] {
				t.Errorf("second application rebuilt %s: oid %d -> %d", ix.name, before[ix.name], after.oid)
			}
		}
	})

	// One index stands, the other is missing: the file builds the missing one
	// and leaves the standing one alone.
	t.Run("one_of_two", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 194)
		m195Seed(t, pool)
		m182Exec(t, pool, m195Indexes[0].ddl)
		kept := m182Index(t, pool, m195Indexes[0].name)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m195Read, m195Write)
		if run.err != nil || len(run.warnings) != 0 {
			t.Fatalf("err %v, warnings %v", run.err, run.warnings)
		}
		if got := m182Index(t, pool, m195Indexes[0].name); got.oid != kept.oid {
			t.Errorf("the standing index was rebuilt")
		}
		if got := m182Index(t, pool, m195Indexes[1].name); got.def != m195Indexes[1].def || !got.valid {
			t.Errorf("the missing index: %s (valid %v)", got.def, got.valid)
		}
	})

	for _, ix := range m195Indexes {
		bare := ix.ddl[len("CREATE INDEX CONCURRENTLY "):]
		for _, c := range []struct {
			name      string
			setup     []string
			reltuples float64
			clear     string
		}{
			{"above_threshold", nil, 600000, ""},
			{"invalid", []string{"CREATE INDEX " + bare, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = '` + ix.name + `'::regclass`}, 0,
				`DROP INDEX CONCURRENTLY ` + ix.name},
			{"foreign_definition", []string{`CREATE INDEX ` + ix.name + ` ON context_blocks (scope, updated_at DESC, id DESC)`}, 0,
				`ALTER INDEX ` + ix.name + ` RENAME TO ` + ix.name + `_foreign`},
			{"view", []string{`CREATE VIEW ` + ix.name + ` AS SELECT 1 AS x`}, 0,
				`ALTER VIEW ` + ix.name + ` RENAME TO ` + ix.name + `_foreign`},
		} {
			t.Run(ix.name+"/"+c.name, func(t *testing.T) {
				pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 194)
				m195Seed(t, pool)
				if c.reltuples > 0 {
					m193Analysed(t, pool)
				}
				for _, s := range c.setup {
					m182Exec(t, pool, s)
				}
				before := m182Index(t, pool, ix.name)
				kind := m18xRelkind(t, pool, ix.name)
				run := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m195Read, m195Write)
				t.Logf("%s: %s; locks %v; warnings %v", c.name, run.took, run.modes, run.warnings)
				if run.err != nil {
					t.Fatal(run.err)
				}
				if m18xWarns(run, ix.name+" ") != 1 {
					t.Errorf("warnings %v, want one naming %s", run.warnings, ix.name)
				}
				if after := m182Index(t, pool, ix.name); after.found != before.found || after.oid != before.oid {
					t.Errorf("the file changed the index: %+v -> %+v", before, after)
				}
				if k := m18xRelkind(t, pool, ix.name); k != kind {
					t.Errorf("the file changed what holds the name: relkind %q -> %q", kind, k)
				}
				if c.reltuples > 0 {
					// Above the threshold nothing is built: no SHARE lock, no
					// writer waits.
					if m182Holds(run.modes, "ShareLock") || run.writerWaited {
						t.Errorf("the file holds %v on context_blocks (writer waited %v)", run.modes, run.writerWaited)
					}
				}
				if c.clear != "" {
					m182Exec(t, pool, c.clear)
				}
				// The runbook builds what is missing; the file then adopts it.
				for _, other := range m195Indexes {
					if !m182Index(t, pool, other.name).found {
						m182Exec(t, pool, other.ddl)
					}
				}
				again := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m195Read, m195Write)
				if again.err != nil || len(again.warnings) != 0 || len(again.modes) != 0 {
					t.Errorf("after the runbook: err %v, warnings %v, locks %v", again.err, again.warnings, again.modes)
				}
				if got := m182Index(t, pool, ix.name); got.def != ix.def || !got.valid {
					t.Errorf("after the runbook: %s (valid %v)", got.def, got.valid)
				}
			})
		}
	}
}

// TestBrowseIndexesTakeNoUpdateOffHOT: PostgreSQL writes an update without
// new index entries (HOT) only when no column that an index's key,
// expression or predicate names changes. Every column the two indexes of
// 195 name is named by another index of context_blocks already
// (idx_blocks_workflow_board names scope, updated_at, id and is_archived;
// idx_context_category the category), so no update that was HOT before 195
// stops being HOT, and an update that writes updated_at was not HOT before
// 195 either. Neither names an embedding column, so they are not in the
// attnum class of the re-embed cutover (TestAttnumClassIsTheCutoverList).
func TestBrowseIndexesTakeNoUpdateOffHOT(t *testing.T) {
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
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, c := range out {
			m[c] = true
		}
		return m
	}
	names := `('idx_blocks_scope_updated', 'idx_blocks_scope_category_updated')`
	mine := cols(`ic.relname IN ` + names)
	others := cols(`ic.relname NOT IN ` + names)
	for _, want := range []string{"scope", "category", "updated_at", "id", "is_archived"} {
		if !mine[want] {
			t.Errorf("the indexes of 195 do not name %s: are they missing?", want)
		}
	}
	for c := range mine {
		if !others[c] {
			t.Errorf("an index of 195 names %s, which no other index of context_blocks names: an update of it is no longer HOT", c)
		}
		if strings.HasPrefix(c, "embedding") {
			t.Errorf("an index of 195 names %s: it would belong to the attnum class of the re-embed cutover", c)
		}
	}
}

// TestBrowseIndexReadinessIsTheMigrationsDefinition: the readiness the
// listings read (store.listIndexReadySQL) compares with the definitions
// migration 195 adopts, so an index the file would warn about is never read
// as standing; and the listings follow a drop and a rebuild once the reading
// is renewed.
func TestBrowseIndexReadinessIsTheMigrationsDefinition(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration195File)
	if err != nil {
		t.Fatal(err)
	}
	for _, ix := range m195Indexes {
		lit := "'" + strings.ReplaceAll(ix.def, "'", "''") + "'"
		if !strings.Contains(string(body), lit) {
			t.Errorf("migration 195 does not adopt %s", ix.def)
		}
	}
	pool := testdb.SetupTestDB(t)
	m195Seed(t, pool)
	tr := &listTracer{}
	tp := listTracedPool(t, pool, tr)
	walks := func() bool {
		t.Helper()
		if _, err := store.SearchBlocks(ctx, tp, nil, "", []string{"m195-a"}, "", nil, 10, true, nil, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		sql, _ := tr.last(t)
		return strings.Contains(sql, "CROSS JOIN LATERAL")
	}
	store.ResetListIndexReadingsForTest()
	if !walks() {
		t.Fatal("with both indexes standing the browse does not walk")
	}
	for _, c := range []struct {
		name  string
		setup []string
	}{
		{"dropped", []string{`DROP INDEX idx_blocks_scope_updated`}},
		{"invalid", []string{`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_blocks_scope_updated'::regclass`}},
		{"foreign_definition", []string{`DROP INDEX idx_blocks_scope_updated`, `CREATE INDEX idx_blocks_scope_updated ON context_blocks (scope, updated_at DESC, id DESC)`}},
	} {
		m182Exec(t, pool, `DROP INDEX IF EXISTS idx_blocks_scope_updated`)
		m182Exec(t, pool, m195Indexes[0].ddl)
		for _, s := range c.setup {
			m182Exec(t, pool, s)
		}
		// Within the reading's minute the listing keeps its form (and its
		// rows); a renewed reading sees the change.
		if !walks() {
			t.Errorf("%s: the reading of the last minute was not kept", c.name)
		}
		store.ResetListIndexReadingsForTest()
		if walks() {
			t.Errorf("%s: the browse walks without idx_blocks_scope_updated as 195 builds it", c.name)
		}
		m182Exec(t, pool, `DROP INDEX IF EXISTS idx_blocks_scope_updated`)
		m182Exec(t, pool, m195Indexes[0].ddl)
		store.ResetListIndexReadingsForTest()
		if !walks() {
			t.Errorf("%s: after the runbook the browse does not walk", c.name)
		}
	}
}

// TestMigration195InlineBuildWriterWait is the measurement behind the
// threshold of 195 (not a gate): a store at 194 grown to each size of
// CTX_M195_ROWS (default 250000,500000) of rows of live width (about 1.5 KB
// of heap, 50 scopes, 1 in 13 archived), a writer inserting a block every
// 2 ms, and the file's two CREATE INDEX in one transaction (as the file
// builds them inline), three times per size; it logs the build time and the
// longest wait of the writer. With CTX_M195_CIC=1 it runs the runbook's two
// CREATE INDEX CONCURRENTLY instead. Set CTX_M195_MEASURE=1.
func TestMigration195InlineBuildWriterWait(t *testing.T) {
	if os.Getenv("CTX_M195_MEASURE") == "" {
		t.Skip("measurement; set CTX_M195_MEASURE=1")
	}
	ctx := context.Background()
	sizes := []int{250000, 500000}
	if v := os.Getenv("CTX_M195_ROWS"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			var n int
			if _, err := fmt.Sscan(f, &n); err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, n)
		}
	}
	cic := os.Getenv("CTX_M195_CIC") != ""
	pool := testdb.SetupTestDBUpTo(t, 194)
	m182Exec(t, pool, `DROP INDEX IF EXISTS idx_embedding_hnsw`)
	have := 0
	for _, rows := range sizes {
		t0 := time.Now()
		m182Exec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
		m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`)
		m182Exec(t, pool, `INSERT INTO context_blocks (category, title, content, scope, type_name, is_archived, metadata, created_at, updated_at)
			SELECT 'm195w-' || (g % 20), 'm195w ' || g, repeat('lorem ipsum dolor sit amet ', 40) || g, 'm195-' || (g % 50),
			       CASE WHEN g % 7 = 3 THEN 'checkpoint' ELSE 'knowledge' END, g % 13 = 0,
			       jsonb_build_object('pad', repeat('m', 260)), now() - g * interval '1 second', now() - (g / 4) * interval '1 second'
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
			for _, ix := range m195Indexes {
				m182Exec(t, pool, `DROP INDEX IF EXISTS `+ix.name)
			}
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
					if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('m195w', $1, 'writer', 'm195-w')`, fmt.Sprintf("m195 writer %d %d %d", rows, round, i)); err != nil {
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
			b0 := time.Now()
			if cic {
				for _, ix := range m195Indexes {
					m182Exec(t, pool, ix.ddl)
				}
			} else {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, st := range []string{`SET LOCAL lock_timeout = '3s'`, "CREATE INDEX " + m195Indexes[0].ddl[len("CREATE INDEX CONCURRENTLY "):],
					"CREATE INDEX " + m195Indexes[1].ddl[len("CREATE INDEX CONCURRENTLY "):]} {
					if _, err := tx.Exec(ctx, st); err != nil {
						_ = tx.Rollback(ctx)
						t.Fatal(err)
					}
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			took := time.Since(b0)
			time.Sleep(200 * time.Millisecond)
			close(stop)
			<-done
			var sizes []string
			for _, ix := range m195Indexes {
				got := m182Index(t, pool, ix.name)
				if !got.valid || got.def != ix.def {
					t.Errorf("the build left %s (valid=%v)", got.def, got.valid)
				}
				var size string
				_ = pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_relation_size($1::regclass))`, ix.name).Scan(&size)
				sizes = append(sizes, ix.name+"="+size)
			}
			var load string
			if b, err := os.ReadFile("/proc/loadavg"); err == nil {
				load = strings.Join(strings.Fields(string(b))[:3], " ")
			}
			t.Logf("MEASURE|rows=%d|heap_mb=%d|round=%d|concurrently=%v|build=%s|writer_longest_wait=%s|writes=%d|%s|load=%s",
				rows, heap>>20, round, cic, took.Round(time.Millisecond), longest.Round(time.Millisecond), writes, strings.Join(sizes, "|"), load)
		}
	}
}
