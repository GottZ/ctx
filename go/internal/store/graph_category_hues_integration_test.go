//go:build integration

// Integration probes for Web-UX U02-W5 (per-category graph hue overrides,
// migration 093 + store CRUD) against a real PG18 testcontainer. Each sub-probe
// maps to a §A4-W5 gate:
//
//   - precedence PER CATEGORY (tenant beats _global for the SAME category, a
//     global-only category still surfaces) — red against a whole-map semantic
//   - round-trip Upsert → Load → Delete → Load(empty)
//   - the SMALLINT/CHECK range guard (hue 360 rejected at the DB)
//   - migration idempotency (093 applied twice is a no-op)
//   - audit row is written with metadata.via='api' when the tx actor is set
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/store/ \
//	  -run 'TestCategoryHues|TestMigration093' -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// upsertHueTx runs one UpsertCategoryHue in its own committed tx (helper).
func upsertHueTx(t *testing.T, pool *pgxpool.Pool, scope, category string, hue int16, by *string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertCategoryHue(ctx, tx, scope, category, hue, by); err != nil {
		t.Fatalf("upsert %s@%s=%d: %v", category, scope, hue, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func deleteHueTx(t *testing.T, pool *pgxpool.Pool, scope, category string, by *string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	found, err := store.DeleteCategoryHue(ctx, tx, scope, category, by)
	if err != nil {
		t.Fatalf("delete %s@%s: %v", category, scope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return found
}

// TestCategoryHues_FailClosed: an empty scope slice / element is rejected, never
// a silent ANY('{}') that resolves to the empty map.
func TestCategoryHues_FailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := store.LoadCategoryHues(ctx, pool, nil); err == nil {
		t.Error("empty scope slice must be rejected (fail-closed), got nil error")
	}
	if _, err := store.LoadCategoryHues(ctx, pool, []string{}); err == nil {
		t.Error("empty scope slice must be rejected (fail-closed), got nil error")
	}
	if _, err := store.LoadCategoryHues(ctx, pool, []string{store.GlobalScope, ""}); err == nil {
		t.Error("empty scope element must be rejected (fail-closed), got nil error")
	}
}

// TestCategoryHues_PrecedencePerCategory is the load-bearing gate: global sets
// A=100, tenant sets B=200; the tenant view {_global, tenant} must resolve to
// {A:100, B:200} — the global-only A survives AND the tenant's B is present. A
// whole-map semantic (tenant map REPLACES global) would drop A → red.
// Additionally, the SAME category set in both scopes resolves to the TENANT hue.
func TestCategoryHues_PrecedencePerCategory(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	upsertHueTx(t, pool, "_global", "A", 100, nil)
	upsertHueTx(t, pool, "tenant-a", "B", 200, nil)
	// Same category C overridden in BOTH scopes — tenant must win.
	upsertHueTx(t, pool, "_global", "C", 10, nil)
	upsertHueTx(t, pool, "tenant-a", "C", 300, nil)

	// readScopes ordering: {_global, tenant} — LAST (tenant) wins per category.
	m, err := store.LoadCategoryHues(ctx, pool, []string{"_global", "tenant-a"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m["A"] != 100 {
		t.Errorf("A = %d, want 100 (global-only category must surface — whole-map semantic would drop it)", m["A"])
	}
	if m["B"] != 200 {
		t.Errorf("B = %d, want 200 (tenant override)", m["B"])
	}
	if m["C"] != 300 {
		t.Errorf("C = %d, want 300 (tenant beats _global for the SAME category)", m["C"])
	}
	if len(m) != 3 {
		t.Errorf("resolved map size = %d, want 3 (%v)", len(m), m)
	}

	// The operator view {_global} alone sees only the global rows (A=100, C=10).
	og, err := store.LoadCategoryHues(ctx, pool, []string{"_global"})
	if err != nil {
		t.Fatalf("load global: %v", err)
	}
	if og["A"] != 100 || og["C"] != 10 || len(og) != 2 {
		t.Errorf("global-only view = %v, want {A:100, C:10}", og)
	}
	// A foreign tenant that never overrode B must NOT see tenant-a's B.
	if _, ok := og["B"]; ok {
		t.Errorf("global-only view leaked tenant B: %v", og)
	}
}

// TestCategoryHues_RoundTrip: Upsert → Load → replace → Delete → Load(empty).
func TestCategoryHues_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	upsertHueTx(t, pool, "_global", "decisions", 210, nil)
	m, _ := store.LoadCategoryHues(ctx, pool, []string{"_global"})
	if m["decisions"] != 210 {
		t.Fatalf("after upsert: decisions = %d, want 210", m["decisions"])
	}
	// Upsert again (ON CONFLICT) replaces the value in place.
	upsertHueTx(t, pool, "_global", "decisions", 42, nil)
	m, _ = store.LoadCategoryHues(ctx, pool, []string{"_global"})
	if m["decisions"] != 42 {
		t.Fatalf("after re-upsert: decisions = %d, want 42", m["decisions"])
	}
	// Delete returns found=true; a second delete found=false.
	if !deleteHueTx(t, pool, "_global", "decisions", nil) {
		t.Fatal("first delete found=false, want true")
	}
	if deleteHueTx(t, pool, "_global", "decisions", nil) {
		t.Fatal("second delete found=true, want false")
	}
	m, _ = store.LoadCategoryHues(ctx, pool, []string{"_global"})
	if len(m) != 0 {
		t.Fatalf("after delete: map = %v, want empty", m)
	}
}

// TestCategoryHues_RangeGuard: the SMALLINT CHECK (0..359) rejects 360 at the DB.
// The Go guard rejects it before the DB; a direct INSERT bypassing Go still hits
// the CHECK (defense in depth).
func TestCategoryHues_RangeGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Go-layer guard.
	tx, _ := pool.Begin(ctx)
	if err := store.UpsertCategoryHue(ctx, tx, "_global", "x", 360, nil); err == nil {
		t.Error("Go guard: hue=360 must be rejected")
	}
	_ = tx.Rollback(ctx)

	// DB CHECK backstop (raw INSERT).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_graph_category_hues (scope, category, hue) VALUES ('_global','y',360)`); err == nil {
		t.Error("DB CHECK: hue=360 must violate the 0..359 constraint")
	}
	// A control char in category must violate the category CHECK.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_graph_category_hues (scope, category, hue) VALUES ('_global', E'a\tb', 10)`); err == nil {
		t.Error("DB CHECK: control char in category must be rejected")
	}
}

// TestCategoryHues_Audit: an attributed upsert writes a context_settings_audit
// row with entity_type='graph_category_hue' and metadata.via='api'; an
// unattributed (nil actor) write records via='sql'.
func TestCategoryHues_Audit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// A real key row so ctx.api_key_id::uuid casts + attribution resolves. The
	// key's home_scope is irrelevant to the audit row (attribution rides the
	// actor UUID); '_'-prefixed scopes are reserved by CreateApiKey, so use a
	// plain tenant scope for the fixture.
	key, _, err := store.CreateApiKey(ctx, pool, "hue-admin", "huetest:main", nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	upsertHueTx(t, pool, "_global", "decisions", 210, &key.ID)

	var entityType, via, entityKey, scope, action string
	err = pool.QueryRow(ctx,
		`SELECT entity_type, entity_key, scope, action, COALESCE(metadata->>'via','')
		   FROM context_settings_audit
		  WHERE entity_type = 'graph_category_hue'
		  ORDER BY created_at DESC LIMIT 1`).Scan(&entityType, &entityKey, &scope, &action, &via)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if entityKey != "decisions" || scope != "_global" || action != "create" || via != "api" {
		t.Errorf("audit row = {entity_key:%q scope:%q action:%q via:%q}, want {decisions _global create api}",
			entityKey, scope, action, via)
	}

	// Unattributed write ⇒ via='sql'.
	upsertHueTx(t, pool, "_global", "learnings", 20, nil)
	var sqlVia string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(metadata->>'via','') FROM context_settings_audit
		  WHERE entity_type='graph_category_hue' AND entity_key='learnings'
		  ORDER BY created_at DESC LIMIT 1`).Scan(&sqlVia); err != nil {
		t.Fatalf("read sql audit: %v", err)
	}
	if sqlVia != "sql" {
		t.Errorf("unattributed via = %q, want sql", sqlVia)
	}
}

// TestMigration093_Idempotent: running 093 a second time is a no-op (the
// migration runner is idempotent by design; the CREATE OR REPLACE / IF NOT
// EXISTS / DROP-then-CREATE trigger pattern is safe to re-apply). We re-exec the
// key DDL fragments directly to prove no error on the second pass.
func TestMigration093_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The table already exists (migration ran at setup). Re-applying the
	// idempotent fragments must not error.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS context_graph_category_hues (
		    scope VARCHAR(50) NOT NULL DEFAULT '_global',
		    category TEXT NOT NULL,
		    hue SMALLINT NOT NULL,
		    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		    updated_by UUID,
		    CONSTRAINT uq_graph_cat_hue_scope_cat UNIQUE (scope, category))`,
		`DROP TRIGGER IF EXISTS trg_graph_category_hues_audit ON context_graph_category_hues`,
		`CREATE TRIGGER trg_graph_category_hues_audit
		    AFTER INSERT OR UPDATE OR DELETE ON context_graph_category_hues
		    FOR EACH ROW EXECUTE FUNCTION audit_graph_category_hues_write()`,
	}
	for i, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("idempotent re-apply stmt %d: %v", i, err)
		}
	}
	// The _migrations row exists exactly once (PK on version).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 93`).Scan(&n); err != nil {
		t.Fatalf("count migration row: %v", err)
	}
	if n != 1 {
		t.Errorf("_migrations version 93 count = %d, want 1", n)
	}
}
