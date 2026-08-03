//go:build integration

// Integration regression for the first-boot backend-pool seeding path
// (Bootstrap): migration 062 swapped uq_backends_name (name) →
// uq_backends_scope_name (scope, name), but the seeding INSERT still
// inferred ON CONFLICT (name) — SQLSTATE 42P10 on every fresh database,
// context_backends stays empty and every LLM role answers 503. A populated
// pool returns before the INSERT, which is why live deployments never hit
// it: the path only runs on the empty table a fresh install has.
//
// External test package like backends_tenant_integration_test.go (testdb
// imports store which imports backends — an internal test would cycle).
//
// Run with:
//
//	go test -tags=integration ./internal/backends/ -run TestBootstrapSeedsEmptyPool -count=1 -v
package backends_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBootstrapSeedsEmptyPool(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	in := backends.BootstrapInput{
		Chat: backends.Backend{
			Host:     "http://chat.internal:11434",
			Protocol: backends.ProtocolOllama,
			Model:    "chat-model",
		},
		Embed: backends.Backend{
			Host:     "http://embed.internal:11434",
			Protocol: backends.ProtocolOllama,
			Model:    "embed-model",
		},
	}

	inserted, err := backends.Bootstrap(ctx, pool, in)
	if err != nil {
		t.Fatalf("Bootstrap on empty context_backends: %v", err)
	}
	if inserted == 0 {
		t.Fatal("Bootstrap inserted 0 rows on an empty pool")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_backends`).Scan(&count); err != nil {
		t.Fatalf("count context_backends: %v", err)
	}
	if count != inserted {
		t.Fatalf("row count %d != inserted %d", count, inserted)
	}

	// Populated table: the guard must return without touching the pool.
	again, err := backends.Bootstrap(ctx, pool, in)
	if err != nil {
		t.Fatalf("Bootstrap on populated pool: %v", err)
	}
	if again != 0 {
		t.Fatalf("second Bootstrap inserted %d rows, want 0", again)
	}
}
