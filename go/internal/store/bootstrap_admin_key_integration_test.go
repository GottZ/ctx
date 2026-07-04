//go:build integration

// Integration test for wave PV10a (design 06 §3.6): the fail-closed first-key
// bootstrap CTX_BOOTSTRAP_ADMIN_KEY. BOTH paths against a real DB:
//
//	empty table    ⇒ a server-admin key is minted, ctx_auth resolves it as
//	                 is_admin=true under the expected label;
//	populated table ⇒ NOTHING is minted (created=false), the incumbent key set
//	                 is untouched — the fail-closed direction (never inject a
//	                 credential into a real deployment).
//
// The verify-lens mutation that drops the `WHERE NOT EXISTS` guard turns the
// populated case red (it would mint a second, unexpected key).
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestBootstrapAdminKey -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func bootstrapKeyCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_api_keys`).Scan(&n); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	return n
}

func TestBootstrapAdminKey(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// 1. EMPTY table ⇒ a server-admin key is minted and authenticates.
	t.Run("empty_table_mints_server_admin", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		// A fresh test DB is seeded with the migration/test key set, so it is
		// NOT empty. Clear it to exercise the genuine fresh-DB path.
		if _, err := pool.Exec(ctx, `TRUNCATE context_api_keys CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if n := bootstrapKeyCount(t, ctx, pool); n != 0 {
			t.Fatalf("precondition: table not empty (%d keys)", n)
		}

		const plaintext = "pv10a-empty-path-plaintext-key-0001"
		const label = "e2e-bootstrap-testrun-empty"
		created, keyID, err := store.BootstrapAdminKey(ctx, pool, plaintext, label)
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if !created {
			t.Fatal("empty table: created=false, want a minted key")
		}
		if keyID == "" {
			t.Fatal("empty table: minted key id is empty")
		}
		if n := bootstrapKeyCount(t, ctx, pool); n != 1 {
			t.Fatalf("after mint: %d keys, want exactly 1", n)
		}

		// The minted key authenticates as a server-admin under the label.
		var (
			apiKeyID string
			isValid  bool
			isAdmin  bool
		)
		if err := pool.QueryRow(ctx,
			`SELECT api_key_id, is_valid, is_admin FROM ctx_auth($1)`, plaintext).
			Scan(&apiKeyID, &isValid, &isAdmin); err != nil {
			t.Fatalf("ctx_auth(minted plaintext): %v", err)
		}
		if !isValid {
			t.Error("minted key does not authenticate (is_valid=false)")
		}
		if !isAdmin {
			t.Error("minted key is not a server-admin (is_admin=false)")
		}
		if apiKeyID != keyID {
			t.Errorf("ctx_auth api_key_id %q != returned keyID %q", apiKeyID, keyID)
		}
		var gotLabel string
		if err := pool.QueryRow(ctx,
			`SELECT label FROM context_api_keys WHERE id = $1::uuid`, keyID).Scan(&gotLabel); err != nil {
			t.Fatalf("read label: %v", err)
		}
		if gotLabel != label {
			t.Errorf("label = %q, want %q", gotLabel, label)
		}
	})

	// 2. POPULATED table ⇒ NOTHING is minted (fail-closed). Mutation dropping
	//    the WHERE NOT EXISTS guard turns this red.
	t.Run("populated_table_mints_nothing", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		before := bootstrapKeyCount(t, ctx, pool)
		if before == 0 {
			// Guarantee a non-empty table regardless of harness seeding.
			if _, _, err := store.CreateApiKey(ctx, pool, "pv10a-incumbent", "private", nil, store.DefaultTenantID); err != nil {
				t.Fatalf("seed incumbent: %v", err)
			}
			before = bootstrapKeyCount(t, ctx, pool)
		}
		if before == 0 {
			t.Fatal("precondition: table still empty after seeding")
		}

		const plaintext = "pv10a-populated-path-plaintext-key-0002"
		const label = "e2e-bootstrap-testrun-populated"
		created, keyID, err := store.BootstrapAdminKey(ctx, pool, plaintext, label)
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		if created {
			t.Error("populated table: created=true, want fail-closed (no mint)")
		}
		if keyID != "" {
			t.Errorf("populated table: keyID %q, want empty", keyID)
		}
		if after := bootstrapKeyCount(t, ctx, pool); after != before {
			t.Errorf("populated table: key count changed %d → %d (a key was injected!)", before, after)
		}
		// The env plaintext must NOT authenticate against a populated DB.
		var isValid bool
		if err := pool.QueryRow(ctx,
			`SELECT is_valid FROM ctx_auth($1)`, plaintext).Scan(&isValid); err != nil {
			t.Fatalf("ctx_auth(bootstrap plaintext on populated DB): %v", err)
		}
		if isValid {
			t.Error("bootstrap plaintext authenticates on a populated DB — the credential was injected")
		}
	})

	// 3. Empty inputs are rejected (defense-in-depth against a misconfigured env).
	t.Run("empty_inputs_rejected", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		if _, _, err := store.BootstrapAdminKey(ctx, pool, "", "e2e-bootstrap-x"); err == nil {
			t.Error("empty plaintext: err=nil, want rejection")
		}
		if _, _, err := store.BootstrapAdminKey(ctx, pool, "some-key", ""); err == nil {
			t.Error("empty label: err=nil, want rejection")
		}
	})
}
