//go:build integration

package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	clActiveID    = "019d0100-0000-7000-9000-000000000001"
	clArchSrcID   = "019d0100-0000-7000-9000-000000000002"
	clArchTgtID   = "019d0100-0000-7000-9000-000000000003"
	clActiveOther = "019d0100-0000-7000-9000-000000000004"
)

// archiveBlock flips is_archived=true on the given block id.
func archiveBlock(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, id)
	if err != nil {
		t.Fatalf("archive block %s: %v", id, err)
	}
}

// insertLink inserts one dream link with the supplied relationship.
func insertLink(t *testing.T, pool *pgxpool.Pool, src, tgt, rel string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
		(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		VALUES ($1::uuid, $2::uuid, $3, 0.9, 0.9, 'private', 5)`,
		src, tgt, rel,
	)
	if err != nil {
		t.Fatalf("insert link %s->%s: %v", src, tgt, err)
	}
}

// countAllLinks returns total number of context_dream_links rows.
func countAllLinks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_dream_links`).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	return n
}

// TestCleanupDanglingLinks_RemovesBothSides asserts that CleanupDanglingLinks
// deletes rows where either source_block_id OR target_block_id is archived.
// Welle 45 audit (2026-05-22): pre-extension version only checked target side,
// missing 2 links where the source had been archived. This test pins the
// extended behaviour.
//
// Setup: 4 blocks (1 archived source, 1 archived target, 2 active), 4 links:
//   - active → active        (must survive)
//   - active → archived_tgt  (must be deleted, target-side)
//   - archived_src → active  (must be deleted, source-side — new behaviour)
//   - archived_src → archived_tgt (must be deleted, both sides)
//
// Expectation: 4 starting links → 1 surviving link, 3 deleted.
func TestCleanupDanglingLinks_RemovesBothSides(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	now := time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, clActiveID, "private", "reference", "active", now, now)
	insertBlock(t, pool, clArchSrcID, "private", "reference", "arch-src", now, now)
	insertBlock(t, pool, clArchTgtID, "private", "reference", "arch-tgt", now, now)
	insertBlock(t, pool, clActiveOther, "private", "reference", "active-other", now, now)

	archiveBlock(t, pool, clArchSrcID)
	archiveBlock(t, pool, clArchTgtID)

	insertLink(t, pool, clActiveID, clActiveOther, "topical")     // survive
	insertLink(t, pool, clActiveID, clArchTgtID, "topical")        // delete (target archived)
	insertLink(t, pool, clArchSrcID, clActiveOther, "topical")     // delete (source archived, new behaviour)
	insertLink(t, pool, clArchSrcID, clArchTgtID, "topical")       // delete (both archived)

	if got := countAllLinks(t, pool); got != 4 {
		t.Fatalf("setup: expected 4 links, got %d", got)
	}

	removed, err := dream.CleanupDanglingLinks(ctx, pool)
	if err != nil {
		t.Fatalf("CleanupDanglingLinks: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed: want 3, got %d", removed)
	}

	if got := countAllLinks(t, pool); got != 1 {
		t.Errorf("surviving links: want 1, got %d", got)
	}

	// Idempotent: second call removes nothing.
	removed2, err := dream.CleanupDanglingLinks(ctx, pool)
	if err != nil {
		t.Fatalf("CleanupDanglingLinks (2nd): %v", err)
	}
	if removed2 != 0 {
		t.Errorf("second-run removed: want 0, got %d", removed2)
	}
}
