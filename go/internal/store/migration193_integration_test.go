//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	migration193File = "193_embed_pending_type_index.sql"
	// m193DDL is the out-of-band build of the header of 193.
	m193DDL   = `CREATE INDEX CONCURRENTLY idx_embedding_pending_type_scope ON context_blocks (type_name, scope, id) WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded`
	m193Read  = `SELECT count(*) FROM context_blocks WHERE category = 'm193'`
	m193Write = `UPDATE context_blocks SET updated_at = updated_at WHERE category = 'm193'`
)

// m193Seed writes, through the production write paths, blocks of two names in
// two scopes that wait for a vector, one that got its vector, one archived and
// one of a type its tenant excludes (marked by the trigger on insert), and
// returns how many of them the predicate of 193 holds.
func m193Seed(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	ownerTenant(t, pool, "m193-x", "m193-x")
	createType(t, pool, "m193-ex", "m193-x", "excluded")
	write := func(scope, title, typ string) string {
		b, err := store.UpsertBlock(ctx, pool, "m193", title, "body of "+title, nil, nil, scope, true, store.SensitivityWrite{}, typ)
		if err != nil {
			t.Fatalf("write %s: %v", title, err)
		}
		return b.ID
	}
	for i, typ := range []string{"knowledge", "reference", "knowledge", "reference"} {
		write([]string{"m193-a", "m193-b"}[i%2], fmt.Sprintf("m193 waiting %d", i), typ)
	}
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = 0.03
	}
	if err := store.StoreEmbedding(ctx, pool, write("m193-a", "m193 embedded", "knowledge"), "m193", vec); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteBlock(ctx, pool, write("m193-a", "m193 archived", "knowledge"), []string{"m193-a"}); err != nil {
		t.Fatal(err)
	}
	marked := write("m193-x", "m193 excluded", "m193-ex")
	if !marker(t, pool, marked) {
		t.Fatalf("fixture: the block of the excluded type is not marked")
	}
	return 4
}

// TestMigration193 applies migration 193 (issue #527) to a database at 192:
// idx_embedding_pending_type_scope is built with the definition the file and
// store.embedExcludedPendingIndexReadySQL compare against, holding the blocks
// that wait for a vector and are not marked, and no block row is written.
// Below the threshold it is built inline under SHARE (a reader goes on, a
// writer waits) and a second application changes nothing; an index built out
// of band with the runbook is adopted; above the threshold, on an INVALID
// index of the name, on a same-named index of another definition and on a
// relation of the name that is not an index the file only warns and takes no
// lock on context_blocks, and after the runbook it adopts the index.
// lock_timeout bounds the wait for SHARE beside an open writer.
func TestMigration193(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration193File)
	if err != nil {
		t.Fatalf("read %s: %v", migration193File, err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 192)
		want := m193Seed(t, pool)
		before := m182Fingerprint(t, pool)
		if err := store.RunMigrationsUpTo(ctx, pool, 193); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if m182Fingerprint(t, pool) != before {
			t.Errorf("migration 193 wrote rows of context_blocks")
		}
		if def, valid := pendingTypeIndexState(t, pool); def != pendingTypeIndexDef || !valid {
			t.Errorf("idx_embedding_pending_type_scope deparses as\n  %s (valid and ready %v)\nwant\n  %s", def, valid, pendingTypeIndexDef)
		}
		if ix := m182Index(t, pool, pendingTypeIndex); ix.entries != want {
			t.Errorf("idx_embedding_pending_type_scope holds %d entries, want %d (the waiting, unmarked, live blocks)", ix.entries, want)
		}
	})

	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 192)
		m193Seed(t, pool)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m193Read, m193Write)
		t.Logf("migration 193 inline: %s; locks %v; reader waited %v, writer waited %v", run.took, run.modes, run.readerWaited, run.writerWaited)
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
		ix := m182Index(t, pool, pendingTypeIndex)
		if ix.def != pendingTypeIndexDef || !ix.valid {
			t.Errorf("after the file: %s (valid=%v)", ix.def, ix.valid)
		}
		again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m193Read, m193Write)
		if again.err != nil || len(again.modes) != 0 || again.writerWaited || len(again.warnings) != 0 {
			t.Errorf("second application: err %v, locks %v, writer waited %v, warnings %v", again.err, again.modes, again.writerWaited, again.warnings)
		}
		if ix2 := m182Index(t, pool, pendingTypeIndex); ix2.oid != ix.oid {
			t.Errorf("the second application rebuilt the index: oid %d -> %d", ix.oid, ix2.oid)
		}
	})

	t.Run("built_out_of_band", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 192)
		m182Exec(t, pool, m193DDL)
		before := m182Index(t, pool, pendingTypeIndex)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m193Read, m193Write)
		if run.err != nil || len(run.modes) != 0 || run.writerWaited || len(run.warnings) != 0 {
			t.Errorf("over the index built out of band: err %v, locks %v, writer waited %v, warnings %v", run.err, run.modes, run.writerWaited, run.warnings)
		}
		if ix := m182Index(t, pool, pendingTypeIndex); ix.oid != before.oid || ix.def != pendingTypeIndexDef {
			t.Errorf("not adopted: oid %d -> %d, %s", before.oid, ix.oid, ix.def)
		}
	})

	for _, c := range []struct {
		name      string
		setup     []string
		reltuples float64
		clear     string // the runbook's step for what holds the name
	}{
		{"above_threshold", nil, 600000, ""},
		{"invalid", []string{m193DDL[len("CREATE INDEX CONCURRENTLY "):], `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_embedding_pending_type_scope'::regclass`}, 0,
			`DROP INDEX CONCURRENTLY idx_embedding_pending_type_scope`},
		{"foreign_definition", []string{`idx_embedding_pending_type_scope ON context_blocks (type_name, scope, id) WHERE embedding IS NULL AND NOT is_archived`}, 0,
			`ALTER INDEX idx_embedding_pending_type_scope RENAME TO idx_embedding_pending_type_scope_foreign`},
		{"view", []string{`CREATE VIEW idx_embedding_pending_type_scope AS SELECT 1 AS x`}, 0,
			`ALTER VIEW idx_embedding_pending_type_scope RENAME TO idx_embedding_pending_type_scope_foreign`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 192)
			m193Seed(t, pool)
			if c.reltuples > 0 {
				// ctx_migration_rows trusts an estimate only for a table
				// analysed on record with at least 32 pages of statistics.
				m193Analysed(t, pool)
			}
			for _, s := range c.setup {
				if !strings.HasPrefix(s, "UPDATE") && !strings.HasPrefix(s, "CREATE") {
					s = "CREATE INDEX " + s
				}
				m182Exec(t, pool, s)
			}
			before := m182Index(t, pool, pendingTypeIndex)
			kind := m18xRelkind(t, pool, pendingTypeIndex)
			run := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m193Read, m193Write)
			t.Logf("%s: %s; locks %v; warnings %v", c.name, run.took, run.modes, run.warnings)
			if run.err != nil {
				t.Fatal(run.err)
			}
			if len(run.warnings) != 1 || m18xWarns(run, pendingTypeIndex) != 1 {
				t.Errorf("warnings %v, want one naming %s", run.warnings, pendingTypeIndex)
			}
			if m182Holds(run.modes, "ShareLock") || run.writerWaited {
				t.Errorf("the file holds %v on context_blocks (writer waited %v)", run.modes, run.writerWaited)
			}
			if after := m182Index(t, pool, pendingTypeIndex); after.found != before.found || after.oid != before.oid {
				t.Errorf("the file changed the index: %+v -> %+v", before, after)
			}
			if k := m18xRelkind(t, pool, pendingTypeIndex); k != kind {
				t.Errorf("the file changed what holds the name: relkind %q -> %q", kind, k)
			}
			if c.clear != "" {
				m182Exec(t, pool, c.clear)
			}
			m182Exec(t, pool, m193DDL)
			again := m18xApply(t, dsn, pool, body, "context_blocks", c.reltuples, m193Read, m193Write)
			if again.err != nil || len(again.warnings) != 0 || len(again.modes) != 0 {
				t.Errorf("after the runbook: err %v, warnings %v, locks %v", again.err, again.warnings, again.modes)
			}
			if def, valid := pendingTypeIndexState(t, pool); def != pendingTypeIndexDef || !valid {
				t.Errorf("after the runbook: %s (valid and ready %v)", def, valid)
			}
		})
	}

	t.Run("lock_timeout", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 192)
		m193Seed(t, pool)
		hold, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, m193Write); err != nil {
			t.Fatal(err)
		}
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, "", "")
		var pgErr *pgconn.PgError
		if !errors.As(run.err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("the file beside an open writer: %v, want 55P03", run.err)
		}
		_ = hold.Rollback(ctx)
		if ix := m182Index(t, pool, pendingTypeIndex); ix.found {
			t.Errorf("the failed file left %s", ix.def)
		}
	})
}

// m193Analysed gives context_blocks what ctx_migration_rows needs to trust an
// estimate (migration 191): an ANALYZE on record and at least 32 pages of
// statistics; m18xApply then sets the estimate itself.
func m193Analysed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	m182Exec(t, pool, `ANALYZE context_blocks`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var on bool
		if err := pool.QueryRow(ctx, `SELECT pg_stat_get_last_analyze_time('context_blocks'::regclass) IS NOT NULL`).Scan(&on); err != nil {
			t.Fatal(err)
		}
		if on {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no ANALYZE of context_blocks on record")
		}
		time.Sleep(50 * time.Millisecond)
	}
	m182Exec(t, pool, `UPDATE pg_class SET relpages = 64 WHERE oid = 'context_blocks'::regclass`)
}

// TestMigration193InlineBuildWriterWait is the measurement behind the
// threshold of 193 (not a gate): a store at 192 grown to each size of
// CTX_M193_ROWS (default 250000,500000,1000000) of rows of live width (about
// 1.5 KB of heap, 19 of 20 without a vector, so in the index), a writer
// inserting a block every 2 ms, and the inline build of 193 — the file's
// CREATE INDEX statement, in one transaction — three times per size; it logs
// the build time and the longest wait of the writer. Set CTX_M193_MEASURE=1.
func TestMigration193InlineBuildWriterWait(t *testing.T) {
	if os.Getenv("CTX_M193_MEASURE") == "" {
		t.Skip("measurement; set CTX_M193_MEASURE=1")
	}
	ctx := context.Background()
	sizes := []int{250000, 500000, 1000000}
	if v := os.Getenv("CTX_M193_ROWS"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			var n int
			if _, err := fmt.Sscan(f, &n); err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, n)
		}
	}
	build := "CREATE INDEX " + m193DDL[len("CREATE INDEX CONCURRENTLY "):]
	pool := testdb.SetupTestDBUpTo(t, 192)
	m182Exec(t, pool, `DROP INDEX IF EXISTS idx_embedding_hnsw`)
	have := 0
	for _, rows := range sizes {
		t0 := time.Now()
		m182Exec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
		m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`)
		m182Exec(t, pool, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
			SELECT 'm193w', 'm193w ' || g, repeat('lorem ipsum dolor sit amet ', 40) || g, 'm193-' || (g % 100),
			       'm193-type-' || (g % 20),
			       CASE WHEN g % 20 = 0 THEN array_fill(0.01::real, ARRAY[1024])::vector(1024) END, CASE WHEN g % 20 = 0 THEN 'q527' END,
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
			m182Exec(t, pool, `DROP INDEX IF EXISTS idx_embedding_pending_type_scope`)
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
					if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('m193w', $1, 'writer', 'm193-w')`, fmt.Sprintf("m193 writer %d %d %d", rows, round, i)); err != nil {
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
			ix := m182Index(t, pool, pendingTypeIndex)
			var load string
			if b, err := os.ReadFile("/proc/loadavg"); err == nil {
				load = strings.Join(strings.Fields(string(b))[:3], " ")
			}
			t.Logf("MEASURE|rows=%d|heap_mb=%d|round=%d|build=%s|writer_longest_wait=%s|writes=%d|index_entries=%d|index_valid=%v|load=%s",
				rows, heap>>20, round, took.Round(time.Millisecond), longest.Round(time.Millisecond), writes, ix.entries, ix.valid, load)
			if !ix.valid || ix.def != pendingTypeIndexDef {
				t.Errorf("the build left %s (valid=%v)", ix.def, ix.valid)
			}
		}
	}
}
