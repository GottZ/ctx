//go:build integration

// Migration 181 (issue #341): the guard's dirty counter and its two triggers
// are gone, context_guard_state and all its columns stay (an older binary
// reads and writes them), and the documented way back restores the catalog of
// migration 180 exactly.
package store_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// guardCatalog is everything migration 181 touches, as text: the function's
// definition and settings, the two triggers' definitions, the state table's
// columns.
func guardCatalog(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	var def, cfg, comment *string
	if err := pool.QueryRow(ctx, `SELECT pg_get_functiondef(p.oid), array_to_string(coalesce(p.proconfig, '{}'), '|'),
			obj_description(p.oid, 'pg_proc')
		  FROM pg_proc p WHERE p.proname = 'mark_guard_dirty'`).Scan(&def, &cfg, &comment); err == nil {
		out["function"] = *def
		out["proconfig"] = *cfg
		if comment != nil {
			out["comment"] = *comment
		}
	}
	rows, err := pool.Query(ctx, `SELECT tgname, pg_get_triggerdef(oid), tgenabled::text FROM pg_trigger
		WHERE tgrelid = 'context_blocks'::regclass AND tgname IN ('trg_guard_dirty', 'trg_guard_dirty_upd')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, d, en string
		if err := rows.Scan(&name, &d, &en); err != nil {
			t.Fatal(err)
		}
		out["trigger "+name] = d + " enabled=" + en
	}
	rows.Close()
	var cols []string
	crows, err := pool.Query(ctx, `SELECT column_name || ' ' || data_type || ' ' || coalesce(column_default, '') FROM information_schema.columns
		WHERE table_name = 'context_guard_state' AND table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	for crows.Next() {
		var c string
		if err := crows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	crows.Close()
	sort.Strings(cols)
	out["columns"] = strings.Join(cols, "; ")
	return out
}

// migration181WayBack is the way back documented in the migration header and
// docs/operations.md, "Migration 181".
const migration181WayBack = `
SET LOCAL lock_timeout = '3s';
CREATE OR REPLACE FUNCTION mark_guard_dirty() RETURNS trigger
LANGUAGE plpgsql
SET enable_nestloop = off
SET jit = off
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    n bigint;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT count(*) INTO n
          FROM new_rows w
         WHERE w.category IS DISTINCT FROM 'index';
    ELSE
        SELECT count(*) INTO n
          FROM new_rows w
         WHERE w.category IS DISTINCT FROM 'index'
           AND NOT EXISTS (SELECT 1
                             FROM old_rows o
                            WHERE o.id = w.id
                              AND o.metadata ? 'guard_status'
                              AND w.metadata ? 'guard_status'
                              AND o.content = w.content);
    END IF;
    IF n > 0 THEN
        UPDATE context_guard_state
           SET dirty_since = COALESCE(dirty_since, now()),
               pending_count = pending_count + n;
    END IF;
    RETURN NULL;
END;
$$;
COMMENT ON FUNCTION mark_guard_dirty() IS
    'trg_guard_dirty / trg_guard_dirty_upd (migration 177): adds the statement''s blocks the guard has to look at to context_guard_state.pending_count in one update; skips category index and updates of a checked block (guard_status in old and new metadata) with unchanged content.';
CREATE TRIGGER trg_guard_dirty
    AFTER INSERT ON context_blocks
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION mark_guard_dirty();
CREATE TRIGGER trg_guard_dirty_upd
    AFTER UPDATE ON context_blocks
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION mark_guard_dirty();
DELETE FROM _migrations WHERE version = 181;
`

func TestMigration181_DropsTheCounterKeepsTheTable(t *testing.T) {
	// A database at 181: the way back of 181 takes back 181 only, so a later
	// migration (182, issue #360) would stay in _migrations above it.
	head := testdb.SetupTestDBUpTo(t, 181)
	at180 := testdb.SetupTestDBUpTo(t, 180)
	ctx := context.Background()

	got := guardCatalog(t, head)
	for _, k := range []string{"function", "proconfig", "trigger trg_guard_dirty", "trigger trg_guard_dirty_upd"} {
		if v, ok := got[k]; ok {
			t.Errorf("after 181 the catalog still has %s: %s", k, v)
		}
	}
	want180 := guardCatalog(t, at180)
	if got["columns"] != want180["columns"] {
		t.Errorf("context_guard_state columns after 181: %s, want those of 180: %s", got["columns"], want180["columns"])
	}
	var rows int
	if err := head.QueryRow(ctx, `SELECT count(*)::int FROM context_guard_state WHERE id`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("state row after 181: %d rows, err=%v, want the one row", rows, err)
	}

	// A block write on 181 leaves the state row as it was.
	if _, err := head.Exec(ctx, `UPDATE context_guard_state SET pending_count = 7, dirty_since = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := head.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('m181', 't', 'c', 'private')`); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := head.QueryRow(ctx, `SELECT pending_count FROM context_guard_state`).Scan(&pending); err != nil || pending != 7 {
		t.Errorf("a block insert on 181 changed pending_count: %d (err=%v), want 7", pending, err)
	}

	// The way back restores the catalog of 180, settings and comment included.
	tx, err := head.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, migration181WayBack); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("way back: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	back := guardCatalog(t, head)
	for k, v := range want180 {
		if back[k] != v {
			t.Errorf("after the way back %s =\n%s\nwant (180)\n%s", k, back[k], v)
		}
	}
	for k := range back {
		if _, ok := want180[k]; !ok {
			t.Errorf("after the way back the catalog has %s, 180 has not", k)
		}
	}
	if len(want180) < 6 {
		t.Errorf("fixture: the catalog of 180 has %d entries, want function, proconfig, comment, two triggers, columns", len(want180))
	}
	var max int
	if err := head.QueryRow(ctx, `SELECT max(version) FROM _migrations`).Scan(&max); err != nil || max != 180 {
		t.Errorf("after the way back max(_migrations.version) = %d (err=%v), want 180", max, err)
	}
}
