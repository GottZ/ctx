//go:build integration

// Integration regression for the first-boot backend-pool seeding path
// (Bootstrap): migration 062 swapped uq_backends_name (name) →
// uq_backends_scope_name (scope, name), but the seeding INSERT still
// inferred ON CONFLICT (name) — SQLSTATE 42P10 on every fresh database,
// context_backends stays empty and every LLM role answers 503. A populated
// pool returns before the INSERT, which is why live deployments never hit
// it: the path only runs on the empty table a fresh install has.
//
// Since A02-W5 (design/02 §4.1d) the same file carries the conditional half:
// an input identical to the comparison base leaves the table EMPTY, because
// the replacement seed paths (`ctx backends seed`, the init wizard) go
// through the running API and would otherwise always arrive second. The
// comparison base against the REAL registry defaults lives in
// cmd/ctxd/seeddefaults_integration_test.go — internal/backends must not
// import internal/config (F1 layering rule, enforced by depguard).
//
// External test package like backends_tenant_integration_test.go (testdb
// imports store which imports backends — an internal test would cycle).
//
// Run with:
//
//	go test -tags=integration ./internal/backends/ -run TestBootstrap -count=1 -v
package backends_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedFixture is the configured input of an operator who really did set the
// env tuples; comparisonBase stands in for the registry defaults. They differ
// in every host, so the fixture always counts as configured.
func seedFixture() backends.BootstrapInput {
	return backends.BootstrapInput{
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
}

func comparisonBase() backends.BootstrapInput {
	return backends.BootstrapInput{
		Chat: backends.Backend{
			Host:     "http://localhost:11434",
			Protocol: backends.ProtocolOllama,
			Model:    "default-chat-model",
		},
		Embed: backends.Backend{
			Host:     "http://localhost:11434",
			Protocol: backends.ProtocolOllama,
			Model:    "default-embed-model",
		},
	}
}

// captureBootstrapLog swaps the process logger for a buffer so the
// deprecation WARN of the dying path can be asserted as the observable it is.
func captureBootstrapLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func countBootstrapRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_backends`).Scan(&count); err != nil {
		t.Fatalf("count context_backends: %v", err)
	}
	return count
}

func TestBootstrapSeedsEmptyPool(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	buf := captureBootstrapLog(t)

	in := seedFixture()

	inserted, err := backends.Bootstrap(ctx, pool, in, comparisonBase())
	if err != nil {
		t.Fatalf("Bootstrap on empty context_backends: %v", err)
	}
	if inserted == 0 {
		t.Fatal("Bootstrap inserted 0 rows on an empty pool")
	}
	if count := countBootstrapRows(ctx, t, pool); count != inserted {
		t.Fatalf("row count %d != inserted %d", count, inserted)
	}

	// A02-W5: the window is only a deprecation window if the dying path says
	// so where it is really used.
	out := buf.String()
	if !strings.Contains(out, "deprecation=env_backend_seed") {
		t.Errorf("log = %q, want the deprecation attribute on a seed that actually ran", out)
	}
	if !strings.Contains(out, "ctx backends seed") {
		t.Errorf("log = %q, want the migration target named — a deprecation without a successor is a dead end", out)
	}

	// Populated table: the guard must return without touching the pool.
	again, err := backends.Bootstrap(ctx, pool, in, comparisonBase())
	if err != nil {
		t.Fatalf("Bootstrap on populated pool: %v", err)
	}
	if again != 0 {
		t.Fatalf("second Bootstrap inserted %d rows, want 0", again)
	}
}

// TestBootstrapDefaultIdenticalInputLeavesPoolEmpty is the A02-W5 gate against
// a real database: an unconfigured install — input identical to the
// comparison base — leaves context_backends EMPTY.
//
// Before this wave the same input wrote two rows (herbert-chat, llama-embed)
// pointing at the default host, which is dead by construction in the
// canonical compose install (localhost inside the ctx container is the
// container) and pre-emptied every replacement seed path. The deprecation
// WARN must stay silent here too: an operator who never used the env path
// must not be told to migrate off it.
//
// Mutation probe: revert Bootstrap to the unconditional probe and both the
// row count and the WARN assertion go red.
func TestBootstrapDefaultIdenticalInputLeavesPoolEmpty(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	buf := captureBootstrapLog(t)

	inserted, err := backends.Bootstrap(ctx, pool, comparisonBase(), comparisonBase())
	if err != nil {
		t.Fatalf("Bootstrap on default config: %v, want a silent no-op", err)
	}
	if inserted != 0 {
		t.Errorf("inserted = %d on an unconfigured install, want 0", inserted)
	}
	if count := countBootstrapRows(ctx, t, pool); count != 0 {
		t.Errorf("context_backends holds %d rows, want an empty pool for the replacement seed path", count)
	}
	if out := buf.String(); strings.Contains(out, "deprecation=env_backend_seed") {
		t.Errorf("log = %q, want no deprecation warning — nobody used the env path here", out)
	}
}

// TestBootstrapDefaultIdenticalInputLeavesExistingRows: the conditional check
// returns before the EXISTS probe, so it has to be provably harmless on a
// POPULATED pool as well — the overwhelming majority of the installation
// base. Nothing is inserted, nothing is deleted; the rows an operator seeded
// stay untouched.
func TestBootstrapDefaultIdenticalInputLeavesExistingRows(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO context_backends (name, base_url, protocol, roles, model_map, priority, enabled)
		VALUES ('chat-primary', 'http://backend.example.com', 'openai', $1, '{}'::jsonb, 100, true)`,
		[]string{backends.RoleSynthesis}); err != nil {
		t.Fatalf("seed fixture row: %v", err)
	}

	inserted, err := backends.Bootstrap(ctx, pool, comparisonBase(), comparisonBase())
	if err != nil {
		t.Fatalf("Bootstrap on populated pool with default config: %v", err)
	}
	if inserted != 0 {
		t.Errorf("inserted = %d, want 0", inserted)
	}
	if count := countBootstrapRows(ctx, t, pool); count != 1 {
		t.Errorf("row count = %d, want the single pre-existing row untouched", count)
	}
}
