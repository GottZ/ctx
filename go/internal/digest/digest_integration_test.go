//go:build integration

package digest_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRunDigest_TopicMapClassifiedAsSystemMeta verifies the Welle 47 W47-NEU-A
// fix: digest.RunDigest must produce a topic-map block with
// type_name='system-meta' + is_meta=TRUE. Pre-fix the block defaulted to
// type_name='knowledge' (CRAG Phase 6, COMPARISON.md DB-Query proof).
//
// Mechanism: digest.RunDigest sets metadata.is_meta=true → store.UpsertBlock
// persists it → ClassifyBlockAfterUpsert branch 1 (is_meta flag) sets
// type_name='system-meta'. ctx_rrf M036/M048 then hard-excludes the block.
func TestRunDigest_TopicMapClassifiedAsSystemMeta(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	homeScope := "private"
	readScopes := []string{homeScope}

	// Need at least one block so RunDigest produces a topic-map. The empty-
	// corpus branch returns early with no upsert.
	seedDigestBlock(t, pool, homeScope, "test-block-1")

	if err := digest.RunDigest(ctx, pool, homeScope, readScopes); err != nil {
		t.Fatalf("RunDigest: %v", err)
	}

	var (
		typeName string
		isMeta    bool
		blockType *string
	)
	err := pool.QueryRow(ctx,
		`SELECT type_name, is_meta, lifecycle_state FROM context_blocks
		 WHERE category = 'index' AND title = $1 AND NOT is_archived`,
		"topic-map-"+homeScope,
	).Scan(&typeName, &isMeta, &blockType)
	if err != nil {
		t.Fatalf("read topic-map block: %v", err)
	}

	if typeName != "system-meta" {
		t.Errorf("type_name = %q, want %q (W47-NEU-A: topic-map must be hard-excluded by ctx_rrf)", typeName, "system-meta")
	}
	if !isMeta {
		t.Error("is_meta = false, want true (W47-NEU-A: topic-map carries metadata.is_meta=true)")
	}
}

// TestRunDigest_IdempotentReClassification covers the re-run case: calling
// RunDigest twice must keep type_name='system-meta' (no flip back to
// 'knowledge'). Guards against an accidental UPDATE that resets type_name
// on metadata refresh.
func TestRunDigest_IdempotentReClassification(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	homeScope := "private"
	readScopes := []string{homeScope}

	seedDigestBlock(t, pool, homeScope, "test-block-1")

	if err := digest.RunDigest(ctx, pool, homeScope, readScopes); err != nil {
		t.Fatalf("first RunDigest: %v", err)
	}
	if err := digest.RunDigest(ctx, pool, homeScope, readScopes); err != nil {
		t.Fatalf("second RunDigest: %v", err)
	}

	var (
		typeName string
		isMeta    bool
		count     int
	)
	err := pool.QueryRow(ctx,
		`SELECT type_name, is_meta, COUNT(*) OVER () FROM context_blocks
		 WHERE category = 'index' AND title = $1 AND NOT is_archived
		 LIMIT 1`,
		"topic-map-"+homeScope,
	).Scan(&typeName, &isMeta, &count)
	if err != nil {
		t.Fatalf("read topic-map block: %v", err)
	}

	if count != 1 {
		t.Errorf("topic-map block count = %d, want 1 (upsert must dedupe by category+title+scope)", count)
	}
	if typeName != "system-meta" {
		t.Errorf("type_name after re-run = %q, want %q (classification must be idempotent)", typeName, "system-meta")
	}
	if !isMeta {
		t.Error("is_meta after re-run = false, want true")
	}
}

// seedDigestBlock inserts one ordinary content block so RunDigest has
// something to index.
func seedDigestBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
		 VALUES ('decisions', $1, 'seed-content', $2, now(), now())`,
		title, scope,
	)
	if err != nil {
		t.Fatalf("seed digest block: %v", err)
	}
}
