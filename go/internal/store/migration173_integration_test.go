//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// migration173Indexes are the two stale-pick indexes and their key columns, as
// the migration's DO block lists them.
var migration173Indexes = []struct{ name, cols string }{
	{"idx_embedding_stale_scope_sens", "scope, sensitivity, created_at, id"},
	{"idx_embedding_stale_sens", "sensitivity, created_at, id"},
}

func migration173Def(name, cols string) string {
	return fmt.Sprintf("CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE (embedding_stale AND (NOT is_archived))", name, cols)
}

// TestMigration173 pins migration 173 (issue #198, part B): the marker column
// is a NOT NULL boolean that defaults to false, so every existing row reads
// as fresh; the two pick indexes are built on a fresh chain in the deparsed
// form the DO block adopts; the runbook's out-of-band preparation (column and
// indexes before the binary) is adopted as it is; a same-named index of
// another definition is left alone.
func TestMigration173(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh_chain", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		var typ, def string
		var notNull bool
		if err := pool.QueryRow(ctx,
			`SELECT format_type(a.atttypid, a.atttypmod), a.attnotnull, pg_get_expr(d.adbin, d.adrelid)
			   FROM pg_attribute a
			   JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			  WHERE a.attrelid = 'context_blocks'::regclass AND a.attname = 'embedding_stale'`).
			Scan(&typ, &notNull, &def); err != nil {
			t.Fatalf("embedding_stale: %v", err)
		}
		if typ != "boolean" || !notNull || def != "false" {
			t.Errorf("embedding_stale is %s not_null=%v default=%s, want boolean NOT NULL DEFAULT false", typ, notNull, def)
		}
		for _, ix := range migration173Indexes {
			var got string
			if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef($1::regclass)`, ix.name).Scan(&got); err != nil {
				t.Fatalf("%s: %v", ix.name, err)
			}
			if want := migration173Def(ix.name, ix.cols); got != want {
				t.Errorf("%s deparses as\n  %s\nthe migration adopts\n  %s", ix.name, got, want)
			}
		}
	})

	t.Run("out_of_band_preparation_is_adopted", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 172)
		if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ADD COLUMN embedding_stale boolean NOT NULL DEFAULT false`); err != nil {
			t.Fatalf("out-of-band column: %v", err)
		}
		for _, ix := range migration173Indexes {
			if _, err := pool.Exec(ctx, fmt.Sprintf(
				`CREATE INDEX %s ON context_blocks (%s) WHERE embedding_stale AND NOT is_archived`, ix.name, ix.cols)); err != nil {
				t.Fatalf("out-of-band build: %v", err)
			}
		}
		oids := func() string {
			var o []uint32
			if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY relname) FROM pg_class
				WHERE relname IN ('idx_embedding_stale_scope_sens', 'idx_embedding_stale_sens')`).Scan(&o); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprint(o)
		}
		before := oids()
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if after := oids(); after != before {
			t.Errorf("index oids %s before the migration, %s after: the out-of-band indexes were not adopted as they were", before, after)
		}
	})

	t.Run("foreign_definition_is_left_alone", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 172)
		if _, err := pool.Exec(ctx, `CREATE INDEX idx_embedding_stale_sens ON context_blocks (sensitivity)`); err != nil {
			t.Fatal(err)
		}
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef('idx_embedding_stale_sens'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if want := "CREATE INDEX idx_embedding_stale_sens ON public.context_blocks USING btree (sensitivity)"; def != want {
			t.Errorf("the foreign index became %q, want it untouched", def)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'idx_embedding_stale_scope_sens'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("idx_embedding_stale_scope_sens: %d (%v), want built", n, err)
		}
	})
}
