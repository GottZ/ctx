//go:build integration

// Integration test for MT wave T27 (Achse 03-W1): store.LoadSettingOverridesMulti
// (the per-tenant settings resolution foundation) + migration 064's scope-leading
// read indexes. The function loads several scopes in one query and orders by key
// then by each scope's position in the input slice — the LAST scope listed wins
// per key (for {_global, tenant}, tenant beats _global). It is fail-closed like
// rrf.Search: an empty slice or empty element is rejected, never an unscoped
// ANY('{}'). No consumer yet (the Go-side precedence merge is a later wave); this
// pins the SELECT contract + the array_position ordering + the index existence.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestLoadSettingOverridesMulti|TestSettingsTenantIndexes' -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func insertSetting(t *testing.T, pool *pgxpool.Pool, key, scope, jsonValue string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, $3::jsonb)`,
		key, scope, jsonValue); err != nil {
		t.Fatalf("insert setting %s@%s: %v", key, scope, err)
	}
}

func TestLoadSettingOverridesMulti_FailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Empty slice → error, NOT a silent ANY('{}') that matches nothing.
	if _, err := store.LoadSettingOverridesMulti(ctx, pool, nil); err == nil {
		t.Error("empty scope slice must be rejected (fail-closed), got nil error")
	}
	if _, err := store.LoadSettingOverridesMulti(ctx, pool, []string{}); err == nil {
		t.Error("empty scope slice must be rejected (fail-closed), got nil error")
	}
	// Any empty element → error (an empty scope string is unscoped).
	if _, err := store.LoadSettingOverridesMulti(ctx, pool, []string{store.GlobalScope, ""}); err == nil {
		t.Error("empty scope element must be rejected (fail-closed), got nil error")
	}
}

func TestLoadSettingOverridesMulti_ArrayPositionOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Two-scope common case: tenant beats _global per key.
	t.Run("tenant_beats_global", func(t *testing.T) {
		insertSetting(t, pool, "t27.two", store.GlobalScope, `1`)
		insertSetting(t, pool, "t27.two", "t27-tenant", `2`)

		got, err := store.LoadSettingOverridesMulti(ctx, pool, []string{store.GlobalScope, "t27-tenant"})
		if err != nil {
			t.Fatalf("multi: %v", err)
		}
		rows := rowsForKey(got, "t27.two")
		if len(rows) != 2 {
			t.Fatalf("got %d rows for key, want 2 (both scopes)", len(rows))
		}
		// tenant is LAST in the slice → highest array_position → first row.
		if rows[0].Scope != "t27-tenant" {
			t.Fatalf("first row scope = %q, want t27-tenant (array_position precedence)", rows[0].Scope)
		}
	})

	// Three scopes, INSERTION order deliberately != array_position order: the
	// result must follow the slice position, not the row/insertion order.
	t.Run("three_scopes_deterministic_not_insertion_order", func(t *testing.T) {
		// insert in order _global, A, B
		insertSetting(t, pool, "t27.three", store.GlobalScope, `10`)
		insertSetting(t, pool, "t27.three", "t27-a", `20`)
		insertSetting(t, pool, "t27.three", "t27-b", `30`)

		// call with slice {_global, B, A} → A is last (pos 3) → A wins, then B, then _global.
		got, err := store.LoadSettingOverridesMulti(ctx, pool,
			[]string{store.GlobalScope, "t27-b", "t27-a"})
		if err != nil {
			t.Fatalf("multi: %v", err)
		}
		rows := rowsForKey(got, "t27.three")
		if len(rows) != 3 {
			t.Fatalf("got %d rows for key, want 3", len(rows))
		}
		want := []string{"t27-a", "t27-b", store.GlobalScope}
		for i, w := range want {
			if rows[i].Scope != w {
				t.Fatalf("row[%d] scope = %q, want %q (array_position order, not insertion order)", i, rows[i].Scope, w)
			}
		}
	})
}

// rowsForKey filters the multi-scope result down to one key, preserving order.
func rowsForKey(all []store.SettingOverride, key string) []store.SettingOverride {
	var out []store.SettingOverride
	for _, o := range all {
		if o.Key == key {
			out = append(out, o)
		}
	}
	return out
}

func TestSettingsTenantIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, idx := range []string{"idx_settings_scope_key", "idx_secrets_scope_name"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx).Scan(&exists); err != nil {
			t.Fatalf("check index %s: %v", idx, err)
		}
		if !exists {
			t.Errorf("migration 064 index %s missing", idx)
		}
	}

	// 2x-idempotent: SetupTestDB already ran RunMigrations once; a second full
	// run is a no-op (per-version EXISTS skip + IF NOT EXISTS / ON CONFLICT).
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations (idempotency): %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 64`).Scan(&count); err != nil {
		t.Fatalf("count migration 64: %v", err)
	}
	if count != 1 {
		t.Errorf("_migrations version 64 rows = %d, want exactly 1 (idempotent)", count)
	}
}
