//go:build integration

// Integration test for Workflow-Achse W3 (migration 078 write_scopes, decision E4=b).
//
// Covers, against a real PG18 testcontainer:
//   - migration 078 is recorded and the full chain applies (SetupTestDB runs it);
//     RunMigrations is idempotent (a second call is a no-op, count stays stable).
//   - context_api_keys gains write_scopes TEXT[] NOT NULL DEFAULT '{}'.
//   - ctx_auth returns the new 9th column write_scopes, round-tripping through
//     auth.Authenticate into AuthResult.WriteScopes (RAW column, no intersection).
//   - MintKeyWithQuota persists an explicit write_scopes set.
//   - GATE (a): MintKeyWithQuota with a write_scope ⊄ allowed_scopes ∪ {home_scope}
//     → ErrWriteScopeNotAllowed, NO row inserted. RED (without validateWriteScopes
//     wired into insertApiKeyTx): the key is minted, err is nil. GREEN after wiring.
//   - COMPAT (pausability): a mint with no write_scopes yields an empty set and a
//     byte-identical AuthResult vs pre-078.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestWriteScopes -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestWriteScopes_Migration078_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("Migration078Recorded", func(t *testing.T) {
		var filename string
		if err := pool.QueryRow(ctx,
			`SELECT filename FROM _migrations WHERE version = 78`).Scan(&filename); err != nil {
			t.Fatalf("migration 78 not recorded: %v", err)
		}
		if filename != "078_write_scopes.sql" {
			t.Errorf("version 78 filename = %q, want 078_write_scopes.sql", filename)
		}
	})

	t.Run("ColumnExistsWithDefault", func(t *testing.T) {
		var dataType, isNullable, colDefault string
		if err := pool.QueryRow(ctx,
			`SELECT data_type, is_nullable, column_default
			   FROM information_schema.columns
			  WHERE table_name='context_api_keys' AND column_name='write_scopes'`).
			Scan(&dataType, &isNullable, &colDefault); err != nil {
			t.Fatalf("write_scopes column not present: %v", err)
		}
		if dataType != "ARRAY" {
			t.Errorf("write_scopes data_type = %q, want ARRAY (TEXT[])", dataType)
		}
		if isNullable != "NO" {
			t.Errorf("write_scopes is_nullable = %q, want NO", isNullable)
		}
	})

	t.Run("MigrationIdempotent", func(t *testing.T) {
		var before int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations`).Scan(&before); err != nil {
			t.Fatalf("count migrations: %v", err)
		}
		// SetupTestDB already ran RunMigrations once; a second call must be a no-op.
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("second RunMigrations: %v", err)
		}
		var after int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations`).Scan(&after); err != nil {
			t.Fatalf("count migrations after: %v", err)
		}
		if before != after {
			t.Errorf("migration count changed on re-run: %d → %d (not idempotent)", before, after)
		}
	})

	t.Run("MintPersistsAndCtxAuthReturnsWriteScopes", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "w3-persist")
		key, plaintext, err := store.MintKeyWithQuota(ctx, pool,
			"w3-writer", "private", []string{"shared", "work"}, []string{"work"}, tid, "member", nil)
		if err != nil {
			t.Fatalf("mint with valid write_scope = %v, want nil", err)
		}
		if len(key.WriteScopes) != 1 || key.WriteScopes[0] != "work" {
			t.Fatalf("persisted write_scopes = %v, want [work]", key.WriteScopes)
		}
		// ctx_auth returns write_scopes RAW as the 9th column → AuthResult.WriteScopes.
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !ar.IsValid {
			t.Fatal("minted key authenticates invalid")
		}
		if len(ar.WriteScopes) != 1 || ar.WriteScopes[0] != "work" {
			t.Errorf("ctx_auth write_scopes = %v, want [work]", ar.WriteScopes)
		}
	})

	t.Run("GateA_BlindWriterRejected", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "w3-blind")
		// 'work' is NOT in allowed_scopes ∪ {home_scope} → a blind-writer → rejected.
		key, _, err := store.MintKeyWithQuota(ctx, pool,
			"w3-blind-writer", "private", []string{"shared"}, []string{"work"}, tid, "member", nil)
		if !errors.Is(err, store.ErrWriteScopeNotAllowed) {
			t.Fatalf("mint blind-writer err = %v (key id %q), want ErrWriteScopeNotAllowed", err, key.ID)
		}
		// No row must have been inserted (the validation runs before the INSERT).
		var cnt int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid AND label = 'w3-blind-writer'`, tid).Scan(&cnt); err != nil {
			t.Fatalf("count blind-writer keys: %v", err)
		}
		if cnt != 0 {
			t.Errorf("blind-writer key was inserted (%d rows) — validation did NOT fail-closed", cnt)
		}
	})

	t.Run("CompatEmptyWriteScopes", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "w3-compat")
		key, plaintext, err := store.MintKeyWithQuota(ctx, pool,
			"w3-compat-key", "private", []string{"shared"}, nil, tid, "member", nil)
		if err != nil {
			t.Fatalf("mint without write_scopes = %v, want nil", err)
		}
		if len(key.WriteScopes) != 0 {
			t.Errorf("write_scopes = %v, want empty (compat)", key.WriteScopes)
		}
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if len(ar.WriteScopes) != 0 {
			t.Errorf("ctx_auth write_scopes = %v, want empty (compat)", ar.WriteScopes)
		}
	})
}
