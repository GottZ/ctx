//go:build integration

// Integration tests for migration 052 (admin tier + home_scope VARCHAR(50)
// + ctx_auth DROP/CREATE) against a real PG18 testcontainer.
//
// Covers the G03 gate probes:
//   - _migrations records version 52
//   - ctx_auth returns is_admin end-to-end (default false, true after the
//     documented per-id bootstrap UPDATE)
//   - home_scope VARCHAR(50) round-trip: a >20-char scope is storable AND
//     survives ctx_auth's internal variables (003 had VARCHAR(20) there)
//   - invalid key keeps the sentinel shape, is_admin=false
//   - stock-data probe: no existing key carries a reserved '_'-scope
//
// Run with:
//
//	go test -tags=integration ./internal/auth/ -count=1 -v
package auth_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestAdminTier_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("MigrationVersion52Recorded", func(t *testing.T) {
		var filename string
		if err := pool.QueryRow(ctx,
			`SELECT filename FROM _migrations WHERE version = 52`).Scan(&filename); err != nil {
			t.Fatalf("migration 52 not recorded: %v", err)
		}
		if filename != "052_api_key_admin.sql" {
			t.Errorf("version 52 filename = %q", filename)
		}
	})

	t.Run("CtxAuth_IsAdminDefaultFalse_BootstrapPerID", func(t *testing.T) {
		key, plaintext, err := store.CreateApiKey(ctx, pool, "admin-it", "private", nil)
		if err != nil {
			t.Fatalf("create key: %v", err)
		}

		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !ar.IsValid {
			t.Fatal("key invalid")
		}
		if ar.IsAdmin {
			t.Error("fresh key is_admin = true, want false (no auto-promotion)")
		}

		// Documented admin bootstrap: one-time UPDATE per key id.
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, key.ID); err != nil {
			t.Fatalf("bootstrap update: %v", err)
		}

		ar, err = auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate after bootstrap: %v", err)
		}
		if !ar.IsAdmin {
			t.Error("is_admin = false after bootstrap UPDATE, want true")
		}
		if ar.ApiKeyID != key.ID {
			t.Errorf("api_key_id = %s, want %s", ar.ApiKeyID, key.ID)
		}
	})

	t.Run("HomeScope_Varchar50_RoundTrip", func(t *testing.T) {
		// 26 chars — would fail INSERT (value too long) and be truncated by
		// ctx_auth's internal variables under the 003 VARCHAR(20) regime.
		longScope := "tenant:crag-research-q1-26"
		if len(longScope) <= 20 || len(longScope) > 50 {
			t.Fatalf("test scope length %d outside (20,50]", len(longScope))
		}

		_, plaintext, err := store.CreateApiKey(ctx, pool, "long-scope-it", longScope, nil)
		if err != nil {
			t.Fatalf("create key with %d-char home_scope: %v", len(longScope), err)
		}

		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if ar.HomeScope != longScope {
			t.Errorf("home_scope round-trip = %q, want %q", ar.HomeScope, longScope)
		}
		if len(ar.ReadScopes) == 0 || ar.ReadScopes[0] != longScope {
			t.Errorf("read_scopes[0] = %v, want %q", ar.ReadScopes, longScope)
		}
	})

	t.Run("InvalidKey_SentinelWithIsAdminFalse", func(t *testing.T) {
		ar, err := auth.Authenticate(ctx, pool, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if ar.IsValid || ar.IsAdmin {
			t.Errorf("invalid key: is_valid=%v is_admin=%v, want false/false", ar.IsValid, ar.IsAdmin)
		}
	})

	t.Run("StockDataProbe_NoReservedScopes", func(t *testing.T) {
		// The same probe the deploy gate runs against the live DB before
		// relying on the '_' sentinel (see README admin bootstrap section).
		var n int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_api_keys
			 WHERE home_scope LIKE '\_%'
			    OR EXISTS (SELECT 1 FROM unnest(allowed_scopes) s WHERE s LIKE '\_%')`,
		).Scan(&n)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if n != 0 {
			t.Errorf("found %d keys with reserved '_'-scopes, want 0", n)
		}
	})
}
