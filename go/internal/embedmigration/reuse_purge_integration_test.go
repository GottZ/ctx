//go:build integration

// Integration tests for the Evokoa-Clean-Room-Plan Achse 04 W04-6 create-
// validation completion (design/04 §4.10 point 3) and the purge command:
//
//   - G-Rot 4 (reuse rules): after a rolled_back migration, create WITHOUT
//     ReuseExisting refuses (rest-data check, W04-3 Bestand) and WITH
//     ReuseExisting also refuses (rollback data is on record as suspicious).
//     NEW in W04-6: ReuseExisting requires the last migration to have ended
//     ABORTED — a `done`/absent last row means the leftover's provenance is
//     unknown. RED evidence: the aborted-requirement test fails against the
//     W04-3 Bestand (reuse after `done` sailed through) — see wave report.
//   - Purge: batched column-wise nulling clears every leftover row; refuses
//     while a non-terminal migration exists.
//
// Run: go test -tags=integration ./internal/embedmigration/ -run 'Reuse|Purge' -count=1 -v
package embedmigration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedTerminalMigration inserts a terminal context_embed_migrations row so
// the "last migration" probe of the reuse validation has a defined answer.
// created_at is forced monotonic via the offset so ORDER BY created_at DESC
// is deterministic even when two seeds land in the same microsecond.
func seedTerminalMigration(t *testing.T, pool *pgxpool.Pool, from, to, status string, minutesAgo int) {
	t.Helper()
	reasonCol := "abort_reason"
	if status == "rolled_back" {
		reasonCol = "rollback_reason"
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_embed_migrations
		     (from_model, to_model, to_backend, status, `+reasonCol+`, created_at, finished_at)
		 VALUES ($1, $2, 'backend-reuse', $3, 'test fixture', now() - make_interval(mins => $4), now())`,
		from, to, status, minutesAgo,
	); err != nil {
		t.Fatalf("seed terminal migration (%s): %v", status, err)
	}
}

// seedLeftoverNextBlock plants one context_blocks row carrying leftover
// embedding_next data labeled with the given model.
func seedLeftoverNextBlock(t *testing.T, pool *pgxpool.Pool, title, nextModel string) string {
	t.Helper()
	ctx := context.Background()
	var blockID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('issue', $1, 'body', 'w04-6') RETURNING id::text`, title).Scan(&blockID); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET embedding_next = $2::vector, embed_model_next = $3 WHERE id = $1`,
		blockID, pgVecLiteral1024(0.1), nextModel,
	); err != nil {
		t.Fatalf("seed leftover embedding_next: %v", err)
	}
	return blockID
}

func TestCreate_ReuseExisting_RequiresAbortedLastMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-reuse", 1024)
	seedBackend(t, pool, "backend-reuse", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "to-model-reuse"}})
	seedLeftoverNextBlock(t, pool, "w04-6-reuse", "to-model-reuse")

	params := embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: "to-model-reuse",
		ToBackend: "backend-reuse", ReuseExisting: true,
	}
	resetRows := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM context_embed_migrations`); err != nil {
			t.Fatalf("reset migration rows: %v", err)
		}
	}

	// Last migration ended `done` (or any non-aborted terminal): leftover
	// _next data has UNKNOWN provenance — reuse must refuse. This is the
	// W04-6 tightening (design §4.10: reuse only after ABORTED); against
	// the W04-3 Bestand this case sailed through (RED evidence).
	t.Run("reuse_after_done_refused", func(t *testing.T) {
		defer resetRows()
		seedTerminalMigration(t, pool, "qwen3-embedding-8b", "to-model-reuse", "done", 10)
		_, err := embedmigration.Create(ctx, pool, params, abundantDisk)
		if !errors.Is(err, embedmigration.ErrReuseRequiresAborted) {
			t.Fatalf("Create error = %v, want ErrReuseRequiresAborted", err)
		}
	})

	// No migration row at all but leftover data present: same unknown-
	// provenance class (somebody wrote _next data out-of-band) — refuse.
	t.Run("reuse_without_any_migration_row_refused", func(t *testing.T) {
		defer resetRows()
		_, err := embedmigration.Create(ctx, pool, params, abundantDisk)
		if !errors.Is(err, embedmigration.ErrReuseRequiresAborted) {
			t.Fatalf("Create error = %v, want ErrReuseRequiresAborted", err)
		}
	})

	// Rot 4 (§7 W04-6 row): after rolled_back, create WITHOUT reuse hits
	// the rest-data check; WITH reuse hits the rollback-specific refusal —
	// rollback data is aktenkundig suspicious, never silently reused.
	t.Run("after_rollback_both_paths_refused", func(t *testing.T) {
		defer resetRows()
		seedTerminalMigration(t, pool, "qwen3-embedding-8b", "to-model-reuse", "rolled_back", 5)

		noReuse := params
		noReuse.ReuseExisting = false
		if _, err := embedmigration.Create(ctx, pool, noReuse, abundantDisk); !errors.Is(err, embedmigration.ErrRestEmbeddingNextData) {
			t.Fatalf("Create (no reuse) error = %v, want ErrRestEmbeddingNextData", err)
		}
		if _, err := embedmigration.Create(ctx, pool, params, abundantDisk); !errors.Is(err, embedmigration.ErrReuseAfterRollback) {
			t.Fatalf("Create (reuse) error = %v, want ErrReuseAfterRollback", err)
		}
	})

	// The one allowed constellation: matching embed_model_next AND last
	// migration aborted — abort leftovers are unsuspicious partial work.
	t.Run("reuse_after_aborted_allowed", func(t *testing.T) {
		defer resetRows()
		seedTerminalMigration(t, pool, "qwen3-embedding-8b", "to-model-reuse", "aborted", 5)
		id, err := embedmigration.Create(ctx, pool, params, abundantDisk)
		if err != nil {
			t.Fatalf("Create error = %v, want success (reuse after aborted)", err)
		}
		if id == "" {
			t.Fatal("Create returned empty id")
		}
	})
}

func TestPurge_ClearsLeftoversInBatches_AndRefusesWhileActive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "to-model-purge", 1024)
	for i, title := range []string{"purge-a", "purge-b", "purge-c"} {
		_ = i
		seedLeftoverNextBlock(t, pool, title, "to-model-purge")
	}

	// Active (non-terminal) migration present: purge must refuse — it would
	// destroy in-flight work the statemachine still accounts for.
	var activeID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status)
		 VALUES ('qwen3-embedding-8b', 'to-model-purge', 'backend-purge', 'pending')
		 RETURNING id::text`).Scan(&activeID); err != nil {
		t.Fatalf("seed active migration: %v", err)
	}
	if _, err := embedmigration.Purge(ctx, pool, 2); !errors.Is(err, embedmigration.ErrPurgeActiveMigration) {
		t.Fatalf("Purge error = %v, want ErrPurgeActiveMigration", err)
	}

	// Terminal: batch size 2 over 3 rows forces the loop to take more than
	// one round — the count must still be exact and the table clean after.
	if _, err := pool.Exec(ctx,
		`UPDATE context_embed_migrations SET status = 'aborted', abort_reason = 'test' WHERE id = $1::uuid`,
		activeID); err != nil {
		t.Fatalf("terminalize migration: %v", err)
	}
	cleared, err := embedmigration.Purge(ctx, pool, 2)
	if err != nil {
		t.Fatalf("Purge error = %v", err)
	}
	if cleared != 3 {
		t.Errorf("Purge cleared = %d, want 3", cleared)
	}
	var rest int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE embedding_next IS NOT NULL OR embed_model_next IS NOT NULL`).
		Scan(&rest); err != nil {
		t.Fatalf("count leftovers: %v", err)
	}
	if rest != 0 {
		t.Errorf("leftover rows after purge = %d, want 0", rest)
	}
}
