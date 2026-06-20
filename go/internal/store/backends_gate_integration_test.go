//go:build integration

// Integration test for MT wave T37 (Achse 04-W5): the store-layer scope gate on
// context_backends mutations (design/04 §4.6/§5.5). UpdateBackend/DeleteBackend
// gained a scopes []string argument and a `scope = ANY($scopes)` predicate that
// fires fail-closed IN the statement — a foreign/_global row matches zero rows
// (found=false → 404 at the handler), with no fetch-then-write TOCTOU and no
// existence oracle. nil scopes = server-admin (no filter, authority over every
// tenant). CreateBackend now persists the scope column too.
//
// RED before T37: UpdateBackend/DeleteBackend took 4 args and addressed WHERE
// id=$N alone — the package would not COMPILE (signature change, the honest red
// of a store-gate wave), and a foreign-scope mutation would have succeeded.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestBackendScopeGate -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackendScopeGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	mkBackend := func(scope, name string) *backends.Backend {
		b := &backends.Backend{
			Name: name, Host: "https://api.example.com/v1",
			Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderGeneric,
			Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
			Roles:    []string{backends.RoleSynthesis},
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
			Enabled:  true, Scope: scope,
		}
		id, err := store.CreateBackend(ctx, tx, b, nil)
		if err != nil {
			t.Fatalf("create backend scope=%q: %v", scope, err)
		}
		b.ID = id
		return b
	}

	// CreateBackend persists the scope (was DEFAULT '_global' before T37).
	t.Run("create_persists_scope", func(t *testing.T) {
		b := mkBackend("tenant-a", "persist-probe")
		var got string
		if err := tx.QueryRow(ctx, `SELECT scope FROM context_backends WHERE id=$1`, b.ID).Scan(&got); err != nil {
			t.Fatalf("read back scope: %v", err)
		}
		if got != "tenant-a" {
			t.Fatalf("persisted scope = %q, want tenant-a (create wrote DEFAULT instead of the chosen scope)", got)
		}
	})

	t.Run("update_foreign_scope_no_rows", func(t *testing.T) {
		b := mkBackend("tenant-a", "upd-foreign")
		// tenant-admin of tenant-b: scopes=[tenant-b] must NOT touch tenant-a's row.
		found, err := store.UpdateBackend(ctx, tx, b, nil, []string{"tenant-b"})
		if err != nil {
			t.Fatalf("update (foreign scope): %v", err)
		}
		if found {
			t.Fatal("privilege escalation: tenant-b admin updated a tenant-a backend (scope gate did not fire)")
		}
	})

	t.Run("update_global_scope_blocked_for_tenant", func(t *testing.T) {
		b := mkBackend("_global", "upd-global")
		// tenant-admin of tenant-a (scopes=[tenant-a]) must NOT touch a _global row.
		found, err := store.UpdateBackend(ctx, tx, b, nil, []string{"tenant-a"})
		if err != nil {
			t.Fatalf("update (_global by tenant): %v", err)
		}
		if found {
			t.Fatal("tenant-admin mutated a shared _global backend (scope gate did not fire)")
		}
	})

	t.Run("update_own_scope_succeeds", func(t *testing.T) {
		b := mkBackend("tenant-a", "upd-own")
		found, err := store.UpdateBackend(ctx, tx, b, nil, []string{"tenant-a"})
		if err != nil {
			t.Fatalf("update (own scope): %v", err)
		}
		if !found {
			t.Fatal("tenant-a admin could not update its OWN backend (gate over-blocked)")
		}
	})

	t.Run("update_nil_scopes_is_server_admin", func(t *testing.T) {
		b := mkBackend("tenant-a", "upd-srv")
		// nil scopes = server-admin: no filter, every tenant's row reachable.
		found, err := store.UpdateBackend(ctx, tx, b, nil, nil)
		if err != nil {
			t.Fatalf("update (server-admin nil scopes): %v", err)
		}
		if !found {
			t.Fatal("server-admin (nil scopes) could not update a tenant-private backend")
		}
	})

	t.Run("delete_foreign_scope_no_rows", func(t *testing.T) {
		b := mkBackend("tenant-a", "del-foreign")
		name, found, err := store.DeleteBackend(ctx, tx, b.ID, nil, []string{"tenant-b"})
		if err != nil {
			t.Fatalf("delete (foreign scope): %v", err)
		}
		if found || name != "" {
			t.Fatalf("tenant-b admin deleted a tenant-a backend (name=%q found=%v)", name, found)
		}
	})

	t.Run("delete_own_scope_succeeds", func(t *testing.T) {
		b := mkBackend("tenant-a", "del-own")
		name, found, err := store.DeleteBackend(ctx, tx, b.ID, nil, []string{"tenant-a"})
		if err != nil {
			t.Fatalf("delete (own scope): %v", err)
		}
		if !found || name != "del-own" {
			t.Fatalf("tenant-a admin could not delete its OWN backend (name=%q found=%v)", name, found)
		}
	})

	t.Run("delete_nil_scopes_is_server_admin", func(t *testing.T) {
		b := mkBackend("tenant-b", "del-srv")
		name, found, err := store.DeleteBackend(ctx, tx, b.ID, nil, nil)
		if err != nil {
			t.Fatalf("delete (server-admin nil scopes): %v", err)
		}
		if !found || name != "del-srv" {
			t.Fatalf("server-admin (nil scopes) could not delete a tenant-private backend (name=%q found=%v)", name, found)
		}
	})
}
