//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// migration175Def is the definition the DO block of migration 175 adopts for
// the two pick indexes, in the form PostgreSQL deparses it.
func migration175Def(name, cols string) string {
	return fmt.Sprintf("CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE ((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))", name, cols)
}

// TestMigration175 pins the ways through the DO block of migration 175 for
// the two pick indexes: on a fresh chain (and over migration 171's
// definition below the row threshold) it builds the new definition; an index
// built out of band with the runbook's DDL is adopted as it is; a same-named
// index of another definition is left alone.
func TestMigration175(t *testing.T) {
	ctx := context.Background()
	t.Run("fresh_chain_and_171_definition_get_the_new_definition", func(t *testing.T) {
		pool := testdb.SetupTestDB(t) // 171 builds, 175 rebuilds
		for _, ix := range migration171Indexes {
			var def string
			if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef($1::regclass)`, ix.name).Scan(&def); err != nil {
				t.Fatalf("%s: %v", ix.name, err)
			}
			if want := migration175Def(ix.name, ix.cols); def != want {
				t.Errorf("%s deparses as\n  %s\nthe migration adopts\n  %s", ix.name, def, want)
			}
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('idx_blocks_embed_excluded'::regclass)`).Scan(&def); err != nil {
			t.Fatalf("idx_blocks_embed_excluded: %v", err)
		}
		if want := "CREATE INDEX idx_blocks_embed_excluded ON public.context_blocks USING btree (type_name, id) WHERE (embed_excluded AND (NOT is_archived))"; def != want {
			t.Errorf("idx_blocks_embed_excluded deparses as\n  %s\nwant\n  %s", def, want)
		}
	})

	t.Run("out_of_band_build_is_adopted", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 174)
		if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS embed_excluded boolean NOT NULL DEFAULT false`); err != nil {
			t.Fatal(err)
		}
		for _, ix := range migration171Indexes {
			for _, st := range []string{
				fmt.Sprintf(`DROP INDEX %s`, ix.name),
				fmt.Sprintf(`CREATE INDEX %s ON context_blocks (%s) WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded`, ix.name, ix.cols),
			} {
				if _, err := pool.Exec(ctx, st); err != nil {
					t.Fatalf("out-of-band build: %v", err)
				}
			}
		}
		read := func() string {
			var v []uint32
			if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY relname) FROM pg_class
				WHERE relname IN ('idx_embedding_pending_scope_sens', 'idx_embedding_pending_sens')`).Scan(&v); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprint(v)
		}
		before := read()
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if after := read(); before != after {
			t.Errorf("index oids %s before the migration, %s after: the out-of-band indexes were not adopted as they were", before, after)
		}
	})

	t.Run("foreign_definition_is_left_alone", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 174)
		for _, st := range []string{`DROP INDEX idx_embedding_pending_sens`, `CREATE INDEX idx_embedding_pending_sens ON context_blocks (sensitivity)`} {
			if _, err := pool.Exec(ctx, st); err != nil {
				t.Fatal(err)
			}
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
		if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('idx_embedding_pending_scope_sens'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if want := migration175Def("idx_embedding_pending_scope_sens", "scope, sensitivity, created_at, id"); def != want {
			t.Errorf("idx_embedding_pending_scope_sens is %q, want the new definition", def)
		}
	})
}
