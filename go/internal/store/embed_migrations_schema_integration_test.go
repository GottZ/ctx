//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-3 schema
// (migration 114): context_embed_models (registry) + context_embed_migrations
// (statemachine rows) + the single-active partial-unique index + the FK
// nachzug onto context_embed_failures.migration_id (deferred from migration
// 113, W04-2, because context_embed_migrations did not exist yet). The
// application-level create/CAS-transition behavior these tables carry lives
// in internal/embedmigration's own integration tests (create_integration_test.go,
// state.go's Transition) — this file pins the DB objects themselves, mirroring
// embed_failures_integration_test.go's "schema_objects_present" contract.
//
// Run: go test -tags=integration ./internal/store/ -run TestEmbedMigrationsSchema -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestEmbedMigrationsSchema_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("tables_and_index_present", func(t *testing.T) {
		for _, table := range []string{"context_embed_models", "context_embed_migrations"} {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, table,
			).Scan(&n); err != nil {
				t.Fatalf("table probe %s: %v", table, err)
			}
			if n != 1 {
				t.Errorf("table %s missing", table)
			}
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE tablename = 'context_embed_migrations' AND indexname = 'idx_embed_migration_single_active'`,
		).Scan(&n); err != nil {
			t.Fatalf("index probe: %v", err)
		}
		if n != 1 {
			t.Errorf("idx_embed_migration_single_active missing")
		}
	})

	t.Run("bootstrap_model_row_seeded", func(t *testing.T) {
		var dims int
		var matryoshka bool
		if err := pool.QueryRow(ctx,
			`SELECT stored_dims, matryoshka FROM context_embed_models WHERE model_key = 'qwen3-embedding-8b'`,
		).Scan(&dims, &matryoshka); err != nil {
			t.Fatalf("read seeded model row: %v", err)
		}
		if dims != 1024 {
			t.Errorf("stored_dims = %d, want 1024", dims)
		}
		if !matryoshka {
			t.Errorf("matryoshka = false, want true")
		}
	})

	t.Run("from_model_equals_to_model_rejected", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_embed_migrations (from_model, to_model, to_backend)
			 VALUES ('qwen3-embedding-8b', 'qwen3-embedding-8b', 'whatever')`)
		if err == nil {
			t.Fatalf("insert with from_model==to_model succeeded, want CHECK violation")
		}
	})

	t.Run("single_active_index_blocks_second_active_row", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims) VALUES ('schema-test-b', 'x', 1024, 1024)`,
		); err != nil {
			t.Fatalf("seed second model: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status)
			 VALUES ('qwen3-embedding-8b', 'schema-test-b', 'whatever', 'pending')`,
		); err != nil {
			t.Fatalf("seed first active migration: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims) VALUES ('schema-test-c', 'x', 1024, 1024)`,
		); err != nil {
			t.Fatalf("seed third model: %v", err)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status)
			 VALUES ('qwen3-embedding-8b', 'schema-test-c', 'whatever', 'running')`,
		)
		if err == nil {
			t.Fatalf("second concurrently-active migration insert succeeded, want unique-index violation")
		}
	})

	t.Run("fk_nachzug_on_embed_failures_migration_id", func(t *testing.T) {
		var blockID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', 'w04-3-fk-nachzug', 'body', 'w04-3') RETURNING id::text`).Scan(&blockID); err != nil {
			t.Fatalf("seed block: %v", err)
		}
		// A random, non-existent migration_id must be rejected by the FK
		// nachzug'd in migration 114 — this table (113) had no such
		// constraint at creation time, so this pins that the ALTER TABLE ...
		// ADD CONSTRAINT ... VALIDATE actually took.
		_, err := pool.Exec(ctx,
			`INSERT INTO context_embed_failures (block_id, migration_id, last_error, last_class, next_attempt_at)
			 VALUES ($1, gen_random_uuid(), 'x', 'wire', now())`,
			blockID,
		)
		if err == nil {
			t.Fatalf("insert with nonexistent migration_id succeeded, want FK violation")
		}
	})

	t.Run("next_columns_present_on_context_blocks", func(t *testing.T) {
		for _, col := range []string{"embedding_next", "embed_model_next"} {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM information_schema.columns WHERE table_name = 'context_blocks' AND column_name = $1`, col,
			).Scan(&n); err != nil {
				t.Fatalf("column probe %s: %v", col, err)
			}
			if n != 1 {
				t.Errorf("context_blocks.%s missing", col)
			}
		}
	})
}
