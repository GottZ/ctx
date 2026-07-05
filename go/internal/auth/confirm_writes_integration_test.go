//go:build integration

// Integration test for F6-C6 D-W4 (migration 090 confirm_writes capability,
// decisions D-E1/E2) against a real PG18 testcontainer.
//
// Covers the D-W4 gate probes:
//   - _migrations records version 90 exactly once (silent-skip dedup trap:
//     a duplicate version number would be skipped without an error)
//   - context_api_keys gains confirm_writes BOOLEAN NOT NULL DEFAULT false
//   - key WITHOUT the flag: AuthResult.ConfirmWrites=false (fail-open default,
//     D-E2 — no behavioural break for existing keys)
//   - key WITH the flag (per-id opt-in UPDATE, is_admin/052 convention):
//     AuthResult.ConfirmWrites=true end-to-end through auth.Authenticate
//   - invalid key keeps the sentinel shape, confirm_writes=false
//   - ROLLBACK COMPAT: the pre-090 named 9-column SELECT (the 078-era auth.go
//     query) still runs against the new function — an old binary keeps
//     working. Control probe: naming a nonexistent column DOES error, so the
//     compat probe is not vacuous.
//
// Run with:
//
//	go test -tags=integration ./internal/auth/ -run TestConfirmWrites -count=1 -v
package auth_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestConfirmWrites_Migration090_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("Migration090RecordedOnce", func(t *testing.T) {
		var filename string
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT filename FROM _migrations WHERE version = 90`).Scan(&filename); err != nil {
			t.Fatalf("migration 90 not recorded: %v", err)
		}
		if filename != "090_confirm_writes_capability.sql" {
			t.Errorf("version 90 filename = %q, want 090_confirm_writes_capability.sql", filename)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM _migrations WHERE version = 90`).Scan(&count); err != nil {
			t.Fatalf("count migration 90: %v", err)
		}
		if count != 1 {
			t.Errorf("migration 90 recorded %d times, want exactly 1", count)
		}
	})

	t.Run("ColumnExistsWithDefaultFalse", func(t *testing.T) {
		var dataType, isNullable, colDefault string
		if err := pool.QueryRow(ctx,
			`SELECT data_type, is_nullable, column_default
			   FROM information_schema.columns
			  WHERE table_name='context_api_keys' AND column_name='confirm_writes'`).
			Scan(&dataType, &isNullable, &colDefault); err != nil {
			t.Fatalf("confirm_writes column not present: %v", err)
		}
		if dataType != "boolean" {
			t.Errorf("data_type = %q, want boolean", dataType)
		}
		if isNullable != "NO" {
			t.Errorf("is_nullable = %q, want NO", isNullable)
		}
		if colDefault != "false" {
			t.Errorf("column_default = %q, want false", colDefault)
		}
	})

	t.Run("DefaultFalse_OptInPerID", func(t *testing.T) {
		key, plaintext, err := store.CreateApiKey(ctx, pool, "confirm-it", "private", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}

		// Key WITHOUT the flag: fail-open default (D-E2), direct path intact.
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !ar.IsValid {
			t.Fatal("key invalid")
		}
		if ar.ConfirmWrites {
			t.Error("fresh key confirm_writes = true, want false (fail-open default, D-E2)")
		}

		// Per-id opt-in (same bootstrap convention as is_admin, 052).
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET confirm_writes = true WHERE id = $1::uuid`, key.ID); err != nil {
			t.Fatalf("opt-in update: %v", err)
		}

		ar, err = auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate after opt-in: %v", err)
		}
		if !ar.ConfirmWrites {
			t.Error("confirm_writes = false after opt-in UPDATE, want true")
		}
		if ar.ApiKeyID != key.ID {
			t.Errorf("api_key_id = %s, want %s", ar.ApiKeyID, key.ID)
		}
	})

	t.Run("InvalidKeySentinelFalse", func(t *testing.T) {
		ar, err := auth.Authenticate(ctx, pool, "deadbeef")
		if err != nil {
			t.Fatalf("authenticate invalid key: %v", err)
		}
		if ar.IsValid {
			t.Error("invalid key IsValid = true")
		}
		if ar.ConfirmWrites {
			t.Error("invalid key confirm_writes = true, want false (sentinel)")
		}
	})

	t.Run("RollbackCompat_Pre090NamedSelect", func(t *testing.T) {
		// The 078-era auth.go query, byte-identical column list (9 columns,
		// no confirm_writes). An old binary issues exactly this against the
		// new function — it must keep working because 090 appended the new
		// column at the END of the RETURNS TABLE.
		_, plaintext, err := store.CreateApiKey(ctx, pool, "compat-it", "private", nil, "")
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		var (
			apiKeyID   *string
			homeScope  string
			isValid    bool
			discardTA  []string
			discardRS  []string
			isAdmin    bool
			tenantID   *string
			tenantRole string
			writeSc    []string
		)
		if err := pool.QueryRow(ctx,
			`SELECT api_key_id, home_scope, allowed_scopes, read_scopes, is_valid, is_admin, tenant_id, tenant_role, write_scopes FROM ctx_auth($1)`,
			plaintext,
		).Scan(&apiKeyID, &homeScope, &discardTA, &discardRS, &isValid, &isAdmin, &tenantID, &tenantRole, &writeSc); err != nil {
			t.Fatalf("pre-090 named SELECT failed against new ctx_auth (rollback compat broken): %v", err)
		}
		if !isValid {
			t.Error("pre-090 SELECT: is_valid = false, want true")
		}

		// Control probe: a nonexistent column DOES error — the compat probe
		// above is not vacuously green.
		if _, err := pool.Exec(ctx,
			`SELECT no_such_column FROM ctx_auth($1)`, plaintext); err == nil {
			t.Error("control probe: SELECT of nonexistent column succeeded, expected error")
		}
	})
}
