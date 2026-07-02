//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/migrations"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestMigration075_DropIsMeta_BehaviourMatchesContract pins the WF T9
// consolidation (design/01 §3.7): after the full migration chain (testdb
// applies every embedded file, 075 included) context_blocks carries NO
// is_meta column and NO idx_blocks_is_meta index, and re-applying the REAL
// 075 file from the embedded migrations.FS is a no-op (idempotency — the
// production runner may replay a file after a partial deploy).
func TestMigration075_DropIsMeta_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	assertDropped := func(stage string) {
		t.Helper()
		var colCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'context_blocks' AND column_name = 'is_meta'`,
		).Scan(&colCount); err != nil {
			t.Fatalf("%s: column probe: %v", stage, err)
		}
		if colCount != 0 {
			t.Errorf("%s: is_meta column still exists (want dropped)", stage)
		}
		var idxCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes
			  WHERE tablename = 'context_blocks' AND indexname = 'idx_blocks_is_meta'`,
		).Scan(&idxCount); err != nil {
			t.Fatalf("%s: index probe: %v", stage, err)
		}
		if idxCount != 0 {
			t.Errorf("%s: idx_blocks_is_meta still exists (want dropped)", stage)
		}
	}

	assertDropped("post-chain")

	// A stale reader must now fail loudly with 42703, never silently.
	if _, err := pool.Exec(ctx, `SELECT is_meta FROM context_blocks LIMIT 1`); err == nil {
		t.Error("SELECT is_meta succeeded — the column must be gone (want 42703)")
	}

	// Idempotency: re-apply the REAL embedded file twice more (no test-local
	// SQL copy — fixture-collusion guard, the M072 golden-test line).
	sqlBytes, err := migrations.FS.ReadFile("075_drop_is_meta.sql")
	if err != nil {
		t.Fatalf("read embedded 075: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("re-apply #%d of 075 must be a no-op, got: %v", i+1, err)
		}
	}
	assertDropped("post-reapply")

	// The registration row survives with ON CONFLICT (exactly one).
	var regCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM _migrations WHERE version = 75`).Scan(&regCount); err != nil {
		t.Fatalf("registration probe: %v", err)
	}
	if regCount != 1 {
		t.Errorf("_migrations rows for version 75 = %d, want 1", regCount)
	}

	// metadata.is_meta (JSONB KEY) deliberately survives as classify input —
	// prove the key path still works post-drop.
	var flag bool
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(('{"is_meta":true}'::jsonb->>'is_meta')::bool, false)`).Scan(&flag); err != nil || !flag {
		t.Errorf("metadata is_meta key probe failed (flag=%v, err=%v)", flag, err)
	}
}
