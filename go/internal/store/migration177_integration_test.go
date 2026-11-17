//go:build integration

// Migration 177 gates (issue #336): the statement-level guard trigger adds to
// context_guard_state what the row-level trigger of 113 added, writes the
// state row once per statement instead of once per row, and takes it only
// after the foreign-key checks of every row of its statement.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestMigration177' -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// g177RowTriggerVersion is the last schema with the row-level trg_guard_dirty:
// the reference every count of the statement-level triggers is compared with.
const g177RowTriggerVersion = 176

// g177Preset is the dirty_since a case starts from when it checks that an
// earlier mark survives.
var g177Preset = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// g177Mark is what one transaction did to context_guard_state.
type g177Mark struct {
	delta   int64  // pending_count after the commit minus before (0)
	dirty   string // dirty_since after the commit: null, tx (the writer's now()), kept (g177Preset) or the value
	updates int64  // updates of the state row inside the writing transaction
}

// g177Step is one write inside the transaction of a case; p is the case's
// title prefix.
type g177Step func(ctx context.Context, tx pgx.Tx, p string) error

func g177SQL(sql string, args ...any) g177Step {
	return func(ctx context.Context, tx pgx.Tx, p string) error {
		_, err := tx.Exec(ctx, strings.ReplaceAll(sql, "{p}", p), args...)
		return err
	}
}

func g177Exec(t *testing.T, pool *pgxpool.Pool, p string, sqls ...string) {
	t.Helper()
	for _, s := range sqls {
		if _, err := pool.Exec(context.Background(), strings.ReplaceAll(s, "{p}", p)); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// g177Reset puts the state row to pending_count 0 and dirty_since NULL (or
// g177Preset).
func g177Reset(t *testing.T, pool *pgxpool.Pool, preset bool) {
	t.Helper()
	var start any
	if preset {
		start = g177Preset
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_guard_state SET pending_count = 0, dirty_since = $1`, start); err != nil {
		t.Fatalf("reset guard state: %v", err)
	}
}

// g177Read reads the state row after a write and classifies dirty_since
// against the writing transaction's now().
func g177Read(t *testing.T, pool *pgxpool.Pool, txNow time.Time) (int64, string) {
	t.Helper()
	var delta int64
	var dirty *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT pending_count, dirty_since FROM context_guard_state`).Scan(&delta, &dirty); err != nil {
		t.Fatalf("read guard state: %v", err)
	}
	switch {
	case dirty == nil:
		return delta, "null"
	case dirty.Equal(txNow):
		return delta, "tx"
	case dirty.Equal(g177Preset):
		return delta, "kept"
	default:
		return delta, dirty.UTC().Format(time.RFC3339Nano)
	}
}

// g177StateUpdates reads the backend's update count of context_guard_state.
func g177StateUpdates(t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var n int64
	if err := tx.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT n_tup_upd FROM pg_stat_xact_user_tables
		                   WHERE relid = 'context_guard_state'::regclass), 0)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// g177Run commits setup, resets the state row, runs steps in one transaction
// and reports what that transaction did to the state row.
func g177Run(t *testing.T, pool *pgxpool.Pool, p string, setup []string, preset bool, steps ...g177Step) g177Mark {
	t.Helper()
	ctx := context.Background()
	g177Exec(t, pool, p, setup...)
	g177Reset(t, pool, preset)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var txNow time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txNow); err != nil {
		t.Fatal(err)
	}
	// The backend's not yet flushed table counts can still hold the reset
	// above; the difference within this transaction is the steps' share.
	updatesBefore := g177StateUpdates(t, tx)
	for i, s := range steps {
		if err := s(ctx, tx, p); err != nil {
			t.Fatalf("%s: step %d: %v", p, i, err)
		}
	}
	var m g177Mark
	m.updates = g177StateUpdates(t, tx) - updatesBefore
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("%s: commit: %v", p, err)
	}
	m.delta, m.dirty = g177Read(t, pool, txNow)
	return m
}

// g177Triggers maps the guard triggers on context_blocks to their level.
func g177Triggers(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT tgname, CASE WHEN tgtype & 1 = 1 THEN 'row' ELSE 'statement' END
		   FROM pg_trigger
		  WHERE tgrelid = 'context_blocks'::regclass AND tgname LIKE 'trg_guard_dirty%'`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var v [2]string
		err := r.Scan(&v[0], &v[1])
		return v, err
	})
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, v := range got {
		m[v[0]] = v[1]
	}
	return m
}

// g177Pair returns a database on the row-trigger schema and one on the head
// schema, and checks that each carries the trigger shape it stands for.
func g177Pair(t *testing.T) (rowTrig, head *pgxpool.Pool) {
	t.Helper()
	rowTrig = testdb.SetupTestDBUpTo(t, g177RowTriggerVersion)
	head = testdb.SetupTestDB(t)
	if got := g177Triggers(t, rowTrig); fmt.Sprint(got) != "map[trg_guard_dirty:row]" {
		t.Fatalf("schema %d: guard triggers %v, want the row-level trg_guard_dirty", g177RowTriggerVersion, got)
	}
	if got := g177Triggers(t, head); fmt.Sprint(got) != "map[trg_guard_dirty:statement trg_guard_dirty_upd:statement]" {
		t.Fatalf("head schema: guard triggers %v, want trg_guard_dirty and trg_guard_dirty_upd statement-level", got)
	}
	return rowTrig, head
}

// The guard's two verdict writes (internal/guard/guard.go, mark checked and
// auto-archive), over every row of the case instead of one id.
const (
	g177GuardMarkChecked = `UPDATE context_blocks SET
		guard_status = 'clean',
		metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
			'guard_status', 'clean'::text, 'guard_checked_at', '2026-11-17T00:00:00Z'::text,
			'guard_similarity', 0.5::float8, 'guard_matched_id', ''::text, 'guard_is_cross_scope', false,
			'guard_threshold_duplicate', 0.98::float8, 'guard_threshold_review', 0.92::float8)
		WHERE title LIKE '{p} %'`
	g177GuardArchive = `UPDATE context_blocks SET
		is_archived = true, guard_status = 'archived_dup',
		metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
			'guard_status', 'near_duplicate'::text, 'guard_checked_at', '2026-11-17T00:00:00Z'::text,
			'guard_similarity', 0.99::float8, 'guard_matched_id', ''::text, 'guard_is_cross_scope', false),
		updated_at = now()
		WHERE title LIKE '{p} %'`
)

// Seed rows of a case: plain (no guard_status in metadata), checked (the
// guard's verdict in metadata), category index, metadata NULL.
func g177Plain(name string) string {
	return `INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', '{p} ` + name + `', 'x', 'private')`
}

func g177Checked(name string) string {
	return `INSERT INTO context_blocks (category, title, content, scope, guard_status, metadata)
	        VALUES ('learnings', '{p} ` + name + `', 'x', 'private', 'clean', '{"guard_status": "clean"}')`
}

func g177Index(name string) string {
	return `INSERT INTO context_blocks (category, title, content, scope) VALUES ('index', '{p} ` + name + `', 'x', 'private')`
}

func g177NullMeta(name string) string {
	return `INSERT INTO context_blocks (category, title, content, scope, metadata) VALUES ('learnings', '{p} ` + name + `', 'x', 'private', NULL)`
}

func g177Copy(n, everyIndex int) g177Step {
	return func(ctx context.Context, tx pgx.Tx, p string) error {
		rows := make([][]any, 0, n)
		for i := 1; i <= n; i++ {
			cat := "learnings"
			if i%everyIndex == 0 {
				cat = "index"
			}
			rows = append(rows, []any{cat, fmt.Sprintf("%s %d", p, i), "x", "private"})
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"context_blocks"},
			[]string{"category", "title", "content", "scope"}, pgx.CopyFromRows(rows))
		return err
	}
}

type g177Case struct {
	name   string
	setup  []string
	preset bool
	steps  []g177Step
	want   int64
}

func g177Cases() []g177Case {
	return []g177Case{
		{name: "insert one", steps: []g177Step{g177SQL(`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', '{p} a', 'x', 'private')`)}, want: 1},
		{name: "insert one index", steps: []g177Step{g177SQL(`INSERT INTO context_blocks (category, title, content, scope) VALUES ('index', '{p} a', 'x', 'private')`)}, want: 0},
		{name: "insert many with index rows", steps: []g177Step{g177SQL(
			`INSERT INTO context_blocks (category, title, content, scope)
			 SELECT CASE WHEN g % 5 = 0 THEN 'index' ELSE 'learnings' END, '{p} ' || g, 'x', 'private' FROM generate_series(1, 50) g`)}, want: 40},
		{name: "insert no row", steps: []g177Step{g177SQL(
			`INSERT INTO context_blocks (category, title, content, scope) SELECT 'learnings', '{p} a', 'x', 'private' WHERE false`)}, want: 0},
		{name: "insert on conflict do nothing", setup: []string{g177Plain("a"), g177Plain("b")}, steps: []g177Step{g177SQL(
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', '{p} a', 'y', 'private'), ('learnings', '{p} b', 'y', 'private')
			 ON CONFLICT (category, title, scope) WHERE NOT is_archived DO NOTHING`)}, want: 0},
		{name: "upsert inserts and updates", setup: []string{g177Plain("a"), g177Checked("b")}, steps: []g177Step{g177SQL(
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('learnings', '{p} a', 'y', 'private'), ('learnings', '{p} b', 'x', 'private'),
			        ('learnings', '{p} c', 'x', 'private'), ('index', '{p} d', 'x', 'private')
			 ON CONFLICT (category, title, scope) WHERE NOT is_archived DO UPDATE SET content = EXCLUDED.content`)}, want: 2},
		{name: "upsert only a skipped update", setup: []string{g177Checked("b")}, steps: []g177Step{g177SQL(
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', '{p} b', 'x', 'private')
			 ON CONFLICT (category, title, scope) WHERE NOT is_archived DO UPDATE SET content = EXCLUDED.content`)}, want: 0},
		{name: "guard verdict first check", setup: []string{g177Plain("a"), g177Plain("b"), g177NullMeta("c"), g177Index("d")},
			steps: []g177Step{g177SQL(g177GuardMarkChecked)}, want: 3},
		{name: "guard verdict re-check", setup: []string{g177Checked("a"), g177Checked("b")}, steps: []g177Step{g177SQL(g177GuardMarkChecked)}, want: 0},
		{name: "guard archive first check", setup: []string{g177Plain("a"), g177Index("b")}, steps: []g177Step{g177SQL(g177GuardArchive)}, want: 1},
		{name: "guard archive re-check", setup: []string{g177Checked("a")}, steps: []g177Step{g177SQL(g177GuardArchive)}, want: 0},
		{name: "content change of checked rows", setup: []string{g177Checked("a"), g177Checked("b"), g177Checked("c")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET content = content || ' changed' WHERE title LIKE '{p} %'`)}, want: 3},
		{name: "guard_status leaves metadata", setup: []string{g177Checked("a"), g177Checked("b")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET metadata = metadata - 'guard_status' WHERE title LIKE '{p} %'`)}, want: 2},
		{name: "metadata NULL", setup: []string{g177NullMeta("a"), g177NullMeta("b"), g177Checked("c")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET tags = ARRAY['t'] WHERE title LIKE '{p} %'`)}, want: 2},
		{name: "metadata becomes NULL", setup: []string{g177Checked("a")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET metadata = NULL WHERE title LIKE '{p} %'`)}, want: 1},
		{name: "update no row", setup: []string{g177Plain("a")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET content = 'y' WHERE title LIKE '{p} %' AND false`)}, want: 0},
		{name: "update into index", setup: []string{g177Plain("a"), g177Plain("b")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET category = 'index' WHERE title LIKE '{p} %'`)}, want: 0},
		{name: "update out of index", setup: []string{g177Index("a"), g177Index("b")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET category = 'learnings' WHERE title LIKE '{p} %'`)}, want: 2},
		{name: "update of every kind", setup: []string{g177Plain("a"), g177Plain("b"), g177Checked("c"), g177Checked("d"), g177Index("e"), g177NullMeta("f")},
			steps: []g177Step{g177SQL(`UPDATE context_blocks SET metadata = COALESCE(metadata, '{}'::jsonb) || '{"k": 1}' WHERE title LIKE '{p} %'`)}, want: 3},
		{name: "delete", setup: []string{g177Plain("a"), g177Checked("b")},
			steps: []g177Step{g177SQL(`DELETE FROM context_blocks WHERE title LIKE '{p} %'`)}, want: 0},
		{name: "merge", setup: []string{g177Plain("a"), g177Checked("b"), g177Plain("e")}, steps: []g177Step{g177SQL(
			`MERGE INTO context_blocks t
			 USING (VALUES ('{p} a', 'y', false), ('{p} b', 'x', false), ('{p} e', 'x', true), ('{p} c', 'x', false)) AS s(title, content, del)
			    ON t.title = s.title AND t.scope = 'private' AND NOT t.is_archived
			 WHEN MATCHED AND s.del THEN DELETE
			 WHEN MATCHED THEN UPDATE SET content = s.content
			 WHEN NOT MATCHED THEN INSERT (category, title, content, scope) VALUES ('learnings', s.title, s.content, 'private')`)}, want: 2},
		{name: "copy", steps: []g177Step{g177Copy(30, 3)}, want: 20},
		{name: "two statements in one transaction", steps: []g177Step{
			g177SQL(`INSERT INTO context_blocks (category, title, content, scope) SELECT 'learnings', '{p} ' || g, 'x', 'private' FROM generate_series(1, 3) g`),
			g177SQL(`UPDATE context_blocks SET content = 'y' WHERE title LIKE '{p} %'`)}, want: 6},
		{name: "earlier mark kept", preset: true, steps: []g177Step{g177SQL(`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', '{p} a', 'x', 'private')`)}, want: 1},
		{name: "earlier mark, nothing qualifies", preset: true, setup: []string{g177Checked("a")}, steps: []g177Step{g177SQL(g177GuardMarkChecked)}, want: 0},
		{name: "foreign-key action updates blocks", setup: []string{
			`INSERT INTO context_sources (file_path, file_hash) VALUES ('{p}', 'h')`,
			`INSERT INTO context_blocks (category, title, content, scope, source_id)
			 SELECT 'learnings', '{p} ' || g, 'x', 'private', (SELECT id FROM context_sources WHERE file_path = '{p}') FROM generate_series(1, 2) g`,
			`INSERT INTO context_blocks (category, title, content, scope, source_id, guard_status, metadata)
			 VALUES ('learnings', '{p} c', 'x', 'private', (SELECT id FROM context_sources WHERE file_path = '{p}'), 'clean', '{"guard_status": "clean"}')`,
		}, steps: []g177Step{g177SQL(`DELETE FROM context_sources WHERE file_path = '{p}'`)}, want: 2},
		// A writable CTE updates the blocks, the foreign-key action of the
		// source delete updates them again in the same statement: both
		// versions of each id are in the transition tables.
		{name: "same blocks updated twice in one statement", setup: []string{
			`INSERT INTO context_sources (file_path, file_hash) VALUES ('{p}', 'h')`,
			`INSERT INTO context_blocks (category, title, content, scope, source_id)
			 VALUES ('learnings', '{p} a', 'x', 'private', (SELECT id FROM context_sources WHERE file_path = '{p}'))`,
			`INSERT INTO context_blocks (category, title, content, scope, source_id, guard_status, metadata)
			 VALUES ('learnings', '{p} b', 'x', 'private', (SELECT id FROM context_sources WHERE file_path = '{p}'), 'clean', '{"guard_status": "clean"}')`,
		}, steps: []g177Step{g177SQL(
			`WITH u AS (UPDATE context_blocks SET tags = ARRAY['t'] WHERE title LIKE '{p} %' RETURNING id)
			 DELETE FROM context_sources WHERE file_path = '{p}' AND EXISTS (SELECT 1 FROM u)`)}, want: 2},
	}
}

// TestMigration177_CountsWhatTheRowTriggerCounted runs every case on the
// schema before 177 (row-level trigger, the reference) and on the head schema
// and requires the same pending_count increment and the same dirty_since.
// The row-level reference wrote the state row once per counted row; the
// statement-level triggers write it at most once per trigger firing and not
// at all when nothing counts.
func TestMigration177_CountsWhatTheRowTriggerCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	rowTrig, head := g177Pair(t)
	for i, c := range g177Cases() {
		p := fmt.Sprintf("g177c%02d", i)
		ref := g177Run(t, rowTrig, p, c.setup, c.preset, c.steps...)
		got := g177Run(t, head, p, c.setup, c.preset, c.steps...)
		if ref.delta != c.want {
			t.Errorf("%s (%s): row trigger counted %d, the case expects %d", c.name, p, ref.delta, c.want)
		}
		if got.delta != ref.delta || got.dirty != ref.dirty {
			t.Errorf("%s (%s): statement triggers delta=%d dirty_since=%s, row trigger delta=%d dirty_since=%s",
				c.name, p, got.delta, got.dirty, ref.delta, ref.dirty)
		}
		if ref.updates != ref.delta {
			t.Errorf("%s (%s): row trigger wrote the state row %d times for %d counted rows", c.name, p, ref.updates, ref.delta)
		}
		if (got.updates == 0) != (got.delta == 0) || got.updates > int64(2*len(c.steps)) {
			t.Errorf("%s (%s): statement triggers wrote the state row %d times for %d counted rows in %d statements",
				c.name, p, got.updates, got.delta, len(c.steps))
		}
	}
}

// TestMigration177_StoreWritesCountAsBefore: the store's own write functions
// (upsert new and existing, update, delete) move the counter on the head
// schema exactly as on the row-trigger schema.
func TestMigration177_StoreWritesCountAsBefore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	rowTrig, head := g177Pair(t)
	scopes := []string{"private"}
	ids := map[*pgxpool.Pool]string{}
	steps := []struct {
		name string
		run  func(pool *pgxpool.Pool) error
	}{
		{"upsert new", func(pool *pgxpool.Pool) error {
			b, err := store.UpsertBlock(ctx, pool, "learnings", "g177 store a", "one", nil, nil, "private", true, store.SensitivityWrite{}, "")
			if err == nil {
				ids[pool] = b.ID
			}
			return err
		}},
		{"upsert changed content", func(pool *pgxpool.Pool) error {
			_, err := store.UpsertBlock(ctx, pool, "learnings", "g177 store a", "two", nil, nil, "private", true, store.SensitivityWrite{}, "")
			return err
		}},
		{"upsert same content", func(pool *pgxpool.Pool) error {
			_, err := store.UpsertBlock(ctx, pool, "learnings", "g177 store a", "two", nil, nil, "private", true, store.SensitivityWrite{}, "")
			return err
		}},
		{"upsert index", func(pool *pgxpool.Pool) error {
			_, err := store.UpsertBlock(ctx, pool, "index", "g177 store index", "map", nil, nil, "private", true, store.SensitivityWrite{}, "")
			return err
		}},
		{"guard verdict", func(pool *pgxpool.Pool) error {
			_, err := pool.Exec(ctx, strings.ReplaceAll(g177GuardMarkChecked, "{p}", "g177 store"))
			return err
		}},
		{"update metadata", func(pool *pgxpool.Pool) error {
			_, _, err := store.UpdateBlock(ctx, pool, ids[pool], store.UpdateBlockData{Metadata: map[string]any{"k": "v"}}, scopes)
			return err
		}},
		{"delete", func(pool *pgxpool.Pool) error {
			_, err := store.DeleteBlock(ctx, pool, ids[pool], scopes)
			return err
		}},
	}
	var total int64
	for _, s := range steps {
		var marks [2]int64
		for i, pool := range []*pgxpool.Pool{rowTrig, head} {
			g177Reset(t, pool, false)
			if err := s.run(pool); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			marks[i], _ = g177Read(t, pool, time.Time{})
		}
		if marks[0] != marks[1] {
			t.Errorf("%s: statement triggers counted %d, row trigger %d", s.name, marks[1], marks[0])
		}
		total += marks[0]
	}
	if total == 0 {
		t.Fatal("no store write moved the counter: the comparison compared nothing")
	}
}

// TestMigration177_StateRowWrittenOncePerStatement: a 500-row insert and a
// 500-row update are two updates of the state row, not one per row.
func TestMigration177_StateRowWrittenOncePerStatement(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	m := g177Run(t, pool, "g177once", nil, false,
		g177SQL(`INSERT INTO context_blocks (category, title, content, scope)
		         SELECT 'learnings', '{p} ' || g, 'x', 'private' FROM generate_series(1, 500) g`),
		g177SQL(`UPDATE context_blocks SET content = content || ' changed' WHERE title LIKE '{p} %'`))
	if m.delta != 1000 || m.dirty != "tx" || m.updates != 2 {
		t.Fatalf("pending_count +%d, dirty_since %s, %d updates of the state row; want +1000, tx, 2", m.delta, m.dirty, m.updates)
	}
}

// g177UpdateTriggerMs runs sql under EXPLAIN ANALYZE on c and returns the time
// PostgreSQL reports for trg_guard_dirty_upd.
func g177UpdateTriggerMs(t *testing.T, c *pgx.Conn, sql string) float64 {
	t.Helper()
	var plan []byte
	if err := c.QueryRow(context.Background(), "EXPLAIN (ANALYZE, FORMAT JSON) "+sql).Scan(&plan); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var p []struct {
		Triggers []struct {
			Name string  `json:"Trigger Name"`
			Time float64 `json:"Time"`
		} `json:"Triggers"`
	}
	if err := json.Unmarshal(plan, &p); err != nil || len(p) != 1 {
		t.Fatalf("explain output: %v", err)
	}
	for _, tr := range p[0].Triggers {
		if tr.Name == "trg_guard_dirty_upd" {
			return tr.Time
		}
	}
	t.Fatalf("no trg_guard_dirty_upd in %s", plan)
	return 0
}

// TestMigration177_LargeUpdateAfterAOneRowUpdate: the UPDATE trigger's join
// has no parameters, so a backend keeps the plan of its first call. After a
// one-row update a nested loop over old_rows would make the next large update
// on the same backend compare every old row with every new row (10 000 rows:
// 17.6 s in the trigger against 15.9 ms on a fresh backend, measured). The
// trigger time of a 4 000-row update after a one-row update must stay in the
// range of the same update on a fresh backend.
func TestMigration177_LargeUpdateAfterAOneRowUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	g177Exec(t, pool, "g177plan",
		`INSERT INTO context_blocks (category, title, content, scope)
		 SELECT 'g177plan' || (g / 4000), '{p} ' || g, 'x', 'private' FROM generate_series(0, 8000) g`)
	conn := func() *pgx.Conn {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close(ctx) })
		return c
	}
	warm := conn()
	g177UpdateTriggerMs(t, warm, `UPDATE context_blocks SET content = content || ' u' WHERE title = 'g177plan 8000'`)
	afterOne := g177UpdateTriggerMs(t, warm, `UPDATE context_blocks SET content = content || ' u' WHERE category = 'g177plan0'`)
	fresh := g177UpdateTriggerMs(t, conn(), `UPDATE context_blocks SET content = content || ' u' WHERE category = 'g177plan1'`)
	t.Logf("trg_guard_dirty_upd for 4000 rows: %.1f ms after a one-row update, %.1f ms on a fresh backend", afterOne, fresh)
	if afterOne > 10*fresh+100 {
		t.Fatalf("4000-row update after a one-row update: %.1f ms in the trigger, %.1f ms on a fresh backend", afterOne, fresh)
	}
}

// TestMigration177_PrimaryKeyChangeIsCounted pins a declared difference: an
// UPDATE that changes a block's id finds no old row under the new id, so the
// skip rule cannot apply and the row is counted; the row-level trigger saw OLD
// and NEW of the same row and skipped it. No code path changes a block's id.
func TestMigration177_PrimaryKeyChangeIsCounted(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	rowTrig, head := g177Pair(t)
	step := g177SQL(`UPDATE context_blocks SET id = uuidv7() WHERE title LIKE '{p} %'`)
	ref := g177Run(t, rowTrig, "g177pk", []string{g177Checked("a")}, false, step)
	got := g177Run(t, head, "g177pk", []string{g177Checked("a")}, false, step)
	if ref.delta != 0 || got.delta != 1 {
		t.Fatalf("id change of a checked block: row trigger %d, statement triggers %d; want 0 and 1", ref.delta, got.delta)
	}
}

// TestMigration177_AppliesOnARowTriggerDatabase: blocks written before the
// migration stay, the migration replaces the triggers on a populated
// database, and the next writes count through the new ones.
func TestMigration177_AppliesOnARowTriggerDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, g177RowTriggerVersion)
	g177Exec(t, pool, "g177mig", g177Plain("a"), g177Checked("b"), g177Index("c"))
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := g177Triggers(t, pool); fmt.Sprint(got) != "map[trg_guard_dirty:statement trg_guard_dirty_upd:statement]" {
		t.Fatalf("after the migration: guard triggers %v", got)
	}
	m := g177Run(t, pool, "g177mig", nil, false,
		g177SQL(`UPDATE context_blocks SET metadata = COALESCE(metadata, '{}'::jsonb) || '{"k": 1}' WHERE title LIKE '{p} %'`))
	if m.delta != 1 || m.updates != 1 {
		t.Fatalf("update of a plain, a checked and an index block: +%d in %d updates, want +1 in 1", m.delta, m.updates)
	}
}

// g177WaitForLock waits until the backend pid waits for a heavyweight lock.
func g177WaitForLock(t *testing.T, pool *pgxpool.Pool, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(context.Background(),
			`SELECT COALESCE((SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid = $1), false)`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d never waited for a lock", pid)
}

// TestMigration177_StateRowAfterForeignKeyChecks: a two-row insert whose
// second row's parent is locked by another transaction waits in the
// foreign-key check of that row. The row-level trigger had locked the state
// row in the first row's turn already, so the waiting insert held it: every
// other block writer queued behind it, and the transaction holding the parent
// closed a deadlock (40P01) with its next block write. The statement-level
// trigger runs after the foreign-key checks of all rows, so the state row is
// free while the insert waits and the other transaction's write goes through.
func TestMigration177_StateRowAfterForeignKeyChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	var p1, p2 string
	for _, v := range []*string{&p1, &p2} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'g177 parent ' || uuidv7(), 'x', 'private') RETURNING id::text`).Scan(v); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := false
	defer func() {
		_ = holder.Rollback(ctx)
		if !finished {
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
		}
		writer.Release()
	}()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, p2); err != nil {
		t.Fatal(err)
	}

	pid := writer.Conn().PgConn().PID()
	go func() {
		_, err := writer.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, parent_id)
			 VALUES ('learnings', 'g177 child 1', 'x', 'private', $1), ('learnings', 'g177 child 2', 'x', 'private', $2)`, p1, p2)
		done <- err
	}()
	g177WaitForLock(t, pool, pid)

	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, probeErr := probe.Exec(ctx, `SELECT 1 FROM context_guard_state FOR NO KEY UPDATE NOWAIT`)
	_ = probe.Rollback(ctx)
	if probeErr != nil {
		t.Fatalf("the state row is held by an insert that waits for a foreign-key lock: %v", probeErr)
	}

	if _, err := holder.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'g177 holder write', 'x', 'private')`); err != nil {
		t.Fatalf("block write of the transaction holding the parent: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("two-row insert: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("two-row insert did not finish after the parent was released")
	}
}
