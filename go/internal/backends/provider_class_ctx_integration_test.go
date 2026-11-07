//go:build integration

package backends_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m166File       = "166_backend_provider_class_ctx.sql"
	m166Version    = 166
	m166Constraint = "context_backends_provider_class_check"
	// m166CtxRow is the peer row of the design gate (design/04 §7 F-W5 (a)):
	// a ctx instance under its model key, LAN locality.
	m166CtxRow = `INSERT INTO context_backends (name, base_url, protocol, provider_class, api_key_ref, locality)
	              VALUES ('ctx-it', 'http://peer:8077', 'openai', 'ctx', 'peer-key', 'lan')`
	// m166WayBack is the way back of the migration header and
	// docs/operations.md "Migration 166", statement for statement.
	m166WayBack = `DELETE FROM context_backends WHERE provider_class = 'ctx';
ALTER TABLE context_backends DROP CONSTRAINT context_backends_provider_class_check;
ALTER TABLE context_backends ADD CONSTRAINT context_backends_provider_class_check
    CHECK (provider_class IN ('generic','llamacpp','openrouter'));
DELETE FROM _migrations WHERE version = 166;`
)

// m166Catalog is everything 166 can touch, as text: every CHECK of
// context_backends with its definition, and the provider_class column default.
func m166Catalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'context_backends'::regclass AND contype = 'c' ORDER BY conname`)
	if err != nil {
		t.Fatalf("read CHECKs: %v", err)
	}
	defs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect CHECKs: %v", err)
	}
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT column_default FROM information_schema.columns
		  WHERE table_name = 'context_backends' AND column_name = 'provider_class'`).Scan(&def); err != nil {
		t.Fatalf("read provider_class default: %v", err)
	}
	return strings.Join(defs, "\n") + "\ndefault " + def
}

// m166ExecTwice runs the migration body as the runner does — one transaction,
// so SET LOCAL lock_timeout holds — a second time on a database that already
// carries 166 (the idempotency half of M-F2).
func m166ExecTwice(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := migrations.Section(m166File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", m166File, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatalf("166 a second time: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func m166CheckViolation(t *testing.T, err error, what string) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("%s: want SQLSTATE 23514, got %v", what, err)
	}
	return pgErr
}

// TestMigration166ProviderClassCtx is gate G1 (design/04 §7 F-W5 (a)) on the
// real migration chain: at 165 the inline baseline CHECK refuses a ctx row;
// 166 — applied by the runner — admits it under the explicitly named
// constraint, still refuses an unknown class with that name, leaves exactly
// one CHECK on the column and its DEFAULT 'generic' alone, writes no row,
// and runs a second time without error to the identical catalog.
func TestMigration166ProviderClassCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m166Version-1)

	_, err := pool.Exec(ctx, m166CtxRow)
	pgErr := m166CheckViolation(t, err, "ctx row at 165")
	t.Logf("at 165: SQLSTATE %s constraint %q", pgErr.Code, pgErr.ConstraintName)
	at165 := m166Catalog(ctx, t, pool)
	var rows165 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_backends`).Scan(&rows165); err != nil {
		t.Fatalf("count at 165: %v", err)
	}

	if err := store.RunMigrationsUpTo(ctx, pool, m166Version); err != nil {
		t.Fatalf("apply 166: %v", err)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = $1`, m166Version).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("_migrations rows of 166 = %d (err %v), want 1", recorded, err)
	}
	var rows166 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_backends`).Scan(&rows166); err != nil || rows166 != rows165 {
		t.Fatalf("context_backends rows %d after 166, %d at 165 (err %v) — 166 writes no row", rows166, rows165, err)
	}
	after := m166Catalog(ctx, t, pool)
	t.Logf("catalog after 166:\n%s", after)

	t.Run("named_check_admits_ctx", func(t *testing.T) {
		var def string
		if err := pool.QueryRow(ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			  WHERE conrelid = 'context_backends'::regclass AND conname = $1`, m166Constraint).Scan(&def); err != nil {
			t.Fatalf("constraint %s not found after 166: %v", m166Constraint, err)
		}
		for _, want := range []string{"generic", "llamacpp", "openrouter", "ctx"} {
			if !strings.Contains(def, "'"+want+"'") {
				t.Errorf("constraint %q lacks %q", def, want)
			}
		}
	})
	t.Run("exactly_one_check_names_provider_class", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_constraint
			  WHERE conrelid = 'context_backends'::regclass AND contype = 'c'
			    AND pg_get_constraintdef(oid) LIKE '%provider_class%'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("CHECKs mentioning provider_class = %d (err %v), want exactly 1", n, err)
		}
	})
	t.Run("default_generic_unchanged", func(t *testing.T) {
		was, now := at165[strings.LastIndex(at165, "\ndefault "):], after[strings.LastIndex(after, "\ndefault "):]
		if was != now || !strings.Contains(now, "'generic'") {
			t.Errorf("provider_class default %q after 166, %q at 165 — want 'generic' unchanged", now, was)
		}
	})
	t.Run("ctx_row_inserts", func(t *testing.T) {
		if _, err := pool.Exec(ctx, m166CtxRow); err != nil {
			t.Fatalf("ctx row refused after 166: %v", err)
		}
	})
	t.Run("unknown_class_names_the_constraint", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_backends (name, base_url, provider_class) VALUES ('grpc-it', 'http://x', 'grpc')`)
		pgErr := m166CheckViolation(t, err, "provider_class 'grpc'")
		if pgErr.ConstraintName != m166Constraint {
			t.Errorf("violated constraint = %q, want %q", pgErr.ConstraintName, m166Constraint)
		}
	})
	// In a transaction that is rolled back: the way back leaves the 165
	// catalog and refuses a ctx row again.
	t.Run("way_back_is_clean", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, m166WayBack); err != nil {
			t.Fatalf("way back: %v", err)
		}
		var defs string
		if err := tx.QueryRow(ctx,
			`SELECT string_agg(conname || ' ' || pg_get_constraintdef(oid), E'\n' ORDER BY conname) FROM pg_constraint
			  WHERE conrelid = 'context_backends'::regclass AND contype = 'c'`).Scan(&defs); err != nil {
			t.Fatalf("read CHECKs: %v", err)
		}
		if want := at165[:strings.LastIndex(at165, "\ndefault ")]; defs != want {
			t.Errorf("CHECKs after the way back:\n%s\nwant the 165 catalog\n%s", defs, want)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 166`).Scan(&n); err != nil || n != 0 {
			t.Errorf("_migrations rows of 166 after the way back = %d (err %v), want 0", n, err)
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT probe`); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		_, err = tx.Exec(ctx, m166CtxRow)
		m166CheckViolation(t, err, "ctx row after the way back")
	})
	t.Run("idempotent_second_run", func(t *testing.T) {
		m166ExecTwice(ctx, t, pool)
		if again := m166Catalog(ctx, t, pool); again != after {
			t.Errorf("catalog after a second run differs:\n%s\nwant\n%s", again, after)
		}
	})
}

// TestMigration166FreshChain pins the same end state on a database built by
// the whole chain at once (testdb.SetupTestDB, every migration in order):
// the named CHECK admits ctx and nothing else new.
func TestMigration166FreshChain(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	if _, err := pool.Exec(ctx, m166CtxRow); err != nil {
		t.Fatalf("ctx row refused on a fresh chain: %v", err)
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO context_backends (name, base_url, provider_class) VALUES ('grpc-it', 'http://x', 'grpc')`)
	if pgErr := m166CheckViolation(t, err, "provider_class 'grpc' on a fresh chain"); pgErr.ConstraintName != m166Constraint {
		t.Errorf("violated constraint = %q, want %q", pgErr.ConstraintName, m166Constraint)
	}
	cat := m166Catalog(ctx, t, pool)
	if n := strings.Count(cat, "(provider_class"); n != 1 || !strings.Contains(cat, "default 'generic'") {
		t.Errorf("fresh chain catalog: %d CHECK(s) mention provider_class, default line %q — want 1 and 'generic':\n%s",
			n, cat[strings.LastIndex(cat, "\ndefault ")+1:], cat)
	}
}

// TestMigration166CatalogEdgeCases pins the catalog lookup of the DO block on
// a hand-modified database at 165 (Review 1 finding 7): a renamed baseline
// constraint is found by its definition and replaced. (A second, hand-made
// CHECK mentioning provider_class is Review 1 finding 1 — a handoff, see the
// F-W5 report.)
func TestMigration166CatalogEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	t.Run("renamed_baseline_constraint", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, m166Version-1)
		if _, err := pool.Exec(ctx, `ALTER TABLE context_backends RENAME CONSTRAINT context_backends_provider_class_check TO legacy_pc_check`); err != nil {
			t.Fatalf("rename at 165: %v", err)
		}
		if err := store.RunMigrationsUpTo(ctx, pool, m166Version); err != nil {
			t.Fatalf("apply 166 over a renamed constraint: %v", err)
		}
		cat := m166Catalog(ctx, t, pool)
		if strings.Count(cat, "(provider_class") != 1 || !strings.Contains(cat, m166Constraint+" CHECK") || strings.Contains(cat, "legacy_pc_check") {
			t.Errorf("catalog after 166 over a renamed constraint:\n%s", cat)
		}
		if _, err := pool.Exec(ctx, m166CtxRow); err != nil {
			t.Errorf("ctx row refused: %v", err)
		}
	})
}
