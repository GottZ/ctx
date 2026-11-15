//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// migration172Indexes are the two lookup indexes, in the form PostgreSQL
// deparses them; the DO block of migration 172 adopts exactly these.
var migration172Indexes = []struct{ name, def string }{
	{"idx_embed_failures_block", "CREATE INDEX idx_embed_failures_block ON public.context_embed_failures USING btree (block_id)"},
	{"idx_write_log_matched_block", "CREATE INDEX idx_write_log_matched_block ON public.context_write_log USING btree (matched_block_id) WHERE (matched_block_id IS NOT NULL)"},
}

// migrate172 completes the chain from 171 over a pool of its own, so the
// warnings of the DO block are observable (testdb's pool has no notice
// handler), and returns the warnings that name one of the two indexes.
func migrate172(t *testing.T, ctx context.Context, dsn string) []string {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var warnings []string
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		if n.Severity == "WARNING" && (strings.Contains(n.Message, "idx_embed_failures_block") || strings.Contains(n.Message, "idx_write_log_matched_block")) {
			warnings = append(warnings, n.Message)
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM _migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	// `<`, not `!=`: the claim is that 172 landed, whatever follows it.
	if version < 172 {
		t.Fatalf("_migrations tops out at %d: 172 must land in every branch of its DO block", version)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), warnings...)
}

// migration172Def is the deparsed definition of a public index, "" when
// there is none of that name.
func migration172Def(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var def string
	err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(c.oid) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = $1`, name).Scan(&def)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return def
}

func migration172OIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var oids []uint32
	if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY relname) FROM pg_class
		WHERE relname IN ('idx_embed_failures_block', 'idx_write_log_matched_block')`).Scan(&oids); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(oids)
}

// TestMigration172 pins the ways through the DO block of migration 172: on a
// fresh chain it builds both indexes, and the deparsed definition is the one
// it compares against (otherwise an out-of-band build would never be
// adopted); the runbook's out-of-band DDL is adopted as it is; a same-named
// index of another definition is left alone with a warning; a table above
// the row threshold gets a warning instead of an inline build.
func TestMigration172(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh_chain_builds_the_adopted_definition", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		for _, ix := range migration172Indexes {
			if def := migration172Def(t, ctx, pool, ix.name); def != ix.def {
				t.Errorf("%s deparses as\n  %s\nthe migration adopts\n  %s", ix.name, def, ix.def)
			}
		}
	})

	t.Run("out_of_band_build_is_adopted", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 171)
		// The runbook's DDL, verbatim.
		for _, st := range []string{
			`CREATE INDEX CONCURRENTLY idx_write_log_matched_block
			    ON context_write_log (matched_block_id)
			    WHERE matched_block_id IS NOT NULL`,
			`CREATE INDEX CONCURRENTLY idx_embed_failures_block
			    ON context_embed_failures (block_id)`,
		} {
			if _, err := pool.Exec(ctx, st); err != nil {
				t.Fatalf("out-of-band build: %v", err)
			}
		}
		before := migration172OIDs(t, ctx, pool)
		if w := migrate172(t, ctx, dsn); len(w) != 0 {
			t.Errorf("an out-of-band build must be adopted without a word, got: %v", w)
		}
		if after := migration172OIDs(t, ctx, pool); after != before || strings.Count(after, " ") != 1 {
			t.Errorf("index oids %s before the migration, %s after: the out-of-band indexes were not adopted as they were", before, after)
		}
	})

	t.Run("foreign_definition_is_left_alone", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 171)
		if _, err := pool.Exec(ctx, `CREATE INDEX idx_write_log_matched_block ON context_write_log (block_id)`); err != nil {
			t.Fatal(err)
		}
		w := migrate172(t, ctx, dsn)
		if len(w) != 1 || !strings.Contains(w[0], "idx_write_log_matched_block exists but is not") {
			t.Errorf("warnings %q, want one naming the foreign idx_write_log_matched_block", w)
		}
		if def, want := migration172Def(t, ctx, pool, "idx_write_log_matched_block"), "CREATE INDEX idx_write_log_matched_block ON public.context_write_log USING btree (block_id)"; def != want {
			t.Errorf("the foreign index became %q, want it untouched", def)
		}
		// The other index of the migration is built all the same.
		if def := migration172Def(t, ctx, pool, "idx_embed_failures_block"); def != migration172Indexes[0].def {
			t.Errorf("idx_embed_failures_block is %q, want built", def)
		}
	})

	// The threshold is per table (pg_class.reltuples of the indexed table):
	// at it the migration warns and leaves that index to the operator, one
	// row below it builds; the other table's index is built either way.
	// Faking reltuples reaches both sides without seeding millions of rows.
	for _, c := range []struct {
		table, index string
		rows         int
		warns        bool
	}{
		{"context_write_log", "idx_write_log_matched_block", 3_000_000, true},
		{"context_write_log", "idx_write_log_matched_block", 2_999_999, false},
		{"context_embed_failures", "idx_embed_failures_block", 500_000, true},
		{"context_embed_failures", "idx_embed_failures_block", 499_999, false},
	} {
		t.Run(fmt.Sprintf("threshold_%s_%d", c.table, c.rows), func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 171)
			tag, err := pool.Exec(ctx, `UPDATE pg_class SET reltuples = $1 WHERE oid = $2::regclass`, c.rows, "public."+c.table)
			if err != nil {
				t.Skipf("cannot fake reltuples on this cluster (%v): the threshold branch stays unproven here", err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatalf("faked reltuples on %d rows of pg_class, want 1", tag.RowsAffected())
			}
			w := migrate172(t, ctx, dsn)
			def := migration172Def(t, ctx, pool, c.index)
			if c.warns {
				if want := fmt.Sprintf("%s is missing at %d rows of %s", c.index, c.rows, c.table); len(w) != 1 || !strings.Contains(w[0], want) {
					t.Errorf("warnings %q, want one containing %q", w, want)
				}
				if def != "" {
					t.Errorf("172 built %q although %s reports %d rows", def, c.table, c.rows)
				}
			} else {
				if len(w) != 0 {
					t.Errorf("warnings %q below the threshold, want none", w)
				}
				if def == "" {
					t.Errorf("%s not built at %d rows of %s, below the threshold", c.index, c.rows, c.table)
				}
			}
			for _, ix := range migration172Indexes {
				if ix.name != c.index && migration172Def(t, ctx, pool, ix.name) != ix.def {
					t.Errorf("%s not built: the threshold of %s must not hold it back", ix.name, c.table)
				}
			}
		})
	}
}
