//go:build integration

// Integration test for MT wave T33a (Achse 04-W1): the context_backends scope
// dimension (migration 062) + the additive read path (Backend.Scope, loaded by
// loadBackendsSQL/scanBackend). Today every backend row is '_global' (shared);
// the column + the UNIQUE swap (name → (scope,name)) let two tenants each own a
// backend of the same name without colliding with the shared one. Chain() does
// not yet filter on scope (04-W2), so ctxd is behaviorally identical — this pins
// the schema swap, the scope load, and the per-tenant audit scope.
//
// External test package (backends_test) on purpose: internal/testdb imports
// internal/store which imports internal/backends, so an internal test would
// cycle. NewPool/Reload/Snapshot are exported, which is all the scope-load probe
// needs.
//
// Run with:
//
//	go test -tags=integration ./internal/backends/ -run 'TestBackends.*Tenant|TestBackends_|TestScanBackend' -count=1 -v
package backends_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func insertBackend(t *testing.T, pool *pgxpool.Pool, name, baseURL, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_backends (name, base_url, scope) VALUES ($1, $2, $3)`,
		name, baseURL, scope); err != nil {
		t.Fatalf("insert backend %s@%s: %v", name, scope, err)
	}
}

func pgCodeOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestBackendsTenantMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// scope column present.
	var hasScope bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		   WHERE table_name='context_backends' AND column_name='scope')`).Scan(&hasScope); err != nil {
		t.Fatalf("check scope column: %v", err)
	}
	if !hasScope {
		t.Error("migration 062 did not add context_backends.scope")
	}

	// UNIQUE swap: uq_backends_scope_name present, uq_backends_name gone.
	for name, want := range map[string]bool{"uq_backends_scope_name": true, "uq_backends_name": false} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, name).Scan(&exists); err != nil {
			t.Fatalf("check constraint %s: %v", name, err)
		}
		if exists != want {
			t.Errorf("constraint %s exists=%v, want %v (UNIQUE swap)", name, exists, want)
		}
	}

	// idx_backends_scope present.
	var hasIdx bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname='idx_backends_scope')`).Scan(&hasIdx); err != nil {
		t.Fatalf("check idx: %v", err)
	}
	if !hasIdx {
		t.Error("migration 062 idx_backends_scope missing")
	}

	// 2x-idempotent: a second full migration run is a no-op (per-version skip).
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations (idempotency): %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 62`).Scan(&count); err != nil {
		t.Fatalf("count migration 62: %v", err)
	}
	if count != 1 {
		t.Errorf("_migrations version 62 rows = %d, want exactly 1 (idempotent)", count)
	}
}

func TestBackends_UniqueSwapScopeName(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// Same name in two different scopes is now allowed (UNIQUE is (scope,name)).
	insertBackend(t, pool, "swap", "http://a", "_global")
	insertBackend(t, pool, "swap", "http://b", "t33-a")

	// Duplicate (scope, name) is rejected with 23505.
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_backends (name, base_url, scope) VALUES ('swap', 'http://c', '_global')`)
	if code := pgCodeOf(err); code != "23505" {
		t.Fatalf("duplicate (scope,name) insert: code = %q, want 23505 (uq_backends_scope_name)", code)
	}
}

func TestScanBackend_LoadsScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	insertBackend(t, pool, "t33-scoped", "http://x", "t33-tenant")

	p := backends.NewPool(pool, nil)
	if err := p.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	var got string
	for _, b := range p.Snapshot() {
		if b.Name == "t33-scoped" {
			got = b.Scope
		}
	}
	if got != "t33-tenant" {
		t.Fatalf("scanned Scope = %q, want t33-tenant (loadBackendsSQL/scanBackend)", got)
	}
}

func TestBackendAudit_RecordsScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	insertBackend(t, pool, "t33-audit", "http://x", "t33-tenant")

	// The audit trigger must record the backend's scope, not a hardcoded
	// '_global' — so a tenant-private backend mutation is tenant-attributed.
	var auditScope string
	if err := pool.QueryRow(context.Background(),
		`SELECT scope FROM context_settings_audit
		  WHERE entity_type='backend' AND entity_key='t33-audit'
		  ORDER BY created_at DESC LIMIT 1`).Scan(&auditScope); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if auditScope != "t33-tenant" {
		t.Fatalf("audit scope = %q, want t33-tenant (COALESCE(scope), not hardcoded _global)", auditScope)
	}
}
