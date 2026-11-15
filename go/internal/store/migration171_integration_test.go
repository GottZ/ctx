//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// migration171Indexes are the two pick indexes and their key columns, as the
// migration's DO block lists them.
var migration171Indexes = []struct{ name, cols string }{
	{"idx_embedding_pending_scope_sens", "scope, sensitivity, created_at, id"},
	{"idx_embedding_pending_sens", "sensitivity, created_at, id"},
}

// migration171Def is the definition the DO block of migration 171 adopts, in
// the form PostgreSQL deparses it.
func migration171Def(name, cols string) string {
	return fmt.Sprintf("CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE ((embedding IS NULL) AND (NOT is_archived))", name, cols)
}

// TestMigration171 pins the three ways through the DO block of migration
// 171: on a fresh chain it builds both indexes, and the deparsed definition
// is the one it compares against (otherwise an out-of-band build would never
// be adopted); an index built out of band by the runbook's DDL is adopted as
// it is; a same-named index of another definition is left alone.
func TestMigration171(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh_chain_builds_the_adopted_definition", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		for _, ix := range migration171Indexes {
			var def string
			if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef($1::regclass)`, ix.name).Scan(&def); err != nil {
				t.Fatalf("%s: %v", ix.name, err)
			}
			if want := migration171Def(ix.name, ix.cols); def != want {
				t.Errorf("%s deparses as\n  %s\nthe migration adopts\n  %s", ix.name, def, want)
			}
		}
	})

	t.Run("out_of_band_build_is_adopted", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 170)
		for _, ix := range migration171Indexes {
			if _, err := pool.Exec(ctx, fmt.Sprintf(
				`CREATE INDEX %s ON context_blocks (%s) WHERE embedding IS NULL AND NOT is_archived`, ix.name, ix.cols)); err != nil {
				t.Fatalf("out-of-band build: %v", err)
			}
		}
		var before []uint32
		if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY relname) FROM pg_class
			WHERE relname IN ('idx_embedding_pending_scope_sens', 'idx_embedding_pending_sens')`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		var after []uint32
		if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY relname) FROM pg_class
			WHERE relname IN ('idx_embedding_pending_scope_sens', 'idx_embedding_pending_sens')`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(before) != fmt.Sprint(after) || len(after) != 2 {
			t.Errorf("index oids %v before the migration, %v after: the out-of-band indexes were not adopted as they were", before, after)
		}
	})

	t.Run("foreign_definition_is_left_alone", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 170)
		if _, err := pool.Exec(ctx, `CREATE INDEX idx_embedding_pending_sens ON context_blocks (sensitivity)`); err != nil {
			t.Fatal(err)
		}
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('idx_embedding_pending_sens'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if want := "CREATE INDEX idx_embedding_pending_sens ON public.context_blocks USING btree (sensitivity)"; def != want {
			t.Errorf("the foreign index became %q, want it untouched", def)
		}
		// The other index of the migration is built all the same.
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'idx_embedding_pending_scope_sens'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("idx_embedding_pending_scope_sens: %d (%v), want built", n, err)
		}
	})
}
