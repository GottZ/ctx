//go:build integration

// Pinned-survival contract (M119, dream-link curation wave 2026-07-26):
// the replace sweep (replaceStaleLinks, driven through the export seam so the
// REAL production DELETE runs) must never delete a pinned link — on both
// sweep shapes (keptTargets non-empty AND the keptTargets-empty clear-out) —
// while unpinned links keep the pre-M119 semantics including the supersedes
// snapshot revert.
//
//	go test -tags=integration ./internal/dream/ -run TestReplaceStaleLinks_Pinned -count=1 -v
package dream_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedPinBlock(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 VALUES ('learnings', $1, 'content of '||$1, 'private', 'knowledge')
		 RETURNING id::text`, title,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func seedPinLink(t *testing.T, pool *pgxpool.Pool, src, tgt, relationship string, pinned bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, pinned)
		 VALUES ($1::uuid, $2::uuid, $3, 0.8, 0.8, 'private', $4)`,
		src, tgt, relationship, pinned,
	)
	if err != nil {
		t.Fatalf("seed link %s->%s: %v", src, tgt, err)
	}
}

func linkExists(t *testing.T, pool *pgxpool.Pool, src, tgt string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_dream_links
		 WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`, src, tgt,
	).Scan(&n); err != nil {
		t.Fatalf("count link: %v", err)
	}
	return n > 0
}

func sweepInTx(t *testing.T, pool *pgxpool.Pool, src string, kept []string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := dream.ReplaceStaleLinksForTest(ctx, tx, src, kept); err != nil {
		t.Fatalf("replaceStaleLinks: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestReplaceStaleLinks_PinnedSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	src := seedPinBlock(t, pool, "pin-sweep-src")
	pinnedTgt := seedPinBlock(t, pool, "pin-sweep-pinned")
	staleTgt := seedPinBlock(t, pool, "pin-sweep-stale")
	keptTgt := seedPinBlock(t, pool, "pin-sweep-kept")
	supTgt := seedPinBlock(t, pool, "pin-sweep-sup")

	seedPinLink(t, pool, src, pinnedTgt, "topical", true)
	seedPinLink(t, pool, src, staleTgt, "topical", false)
	seedPinLink(t, pool, src, keptTgt, "topical", false)
	seedPinLink(t, pool, src, supTgt, "supersedes", false)
	// ApplySupersedes side-effect on the UNPINNED supersedes target — the
	// revert must keep working for unpinned links (pre-M119 semantics).
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`,
		src, supTgt); err != nil {
		t.Fatalf("mark snapshot: %v", err)
	}

	// Sweep 1: keptTargets non-empty — pinned survives although NOT kept.
	sweepInTx(t, pool, src, []string{keptTgt})

	if !linkExists(t, pool, src, pinnedTgt) {
		t.Error("pinned link deleted by replace sweep (kept-list shape)")
	}
	if linkExists(t, pool, src, staleTgt) {
		t.Error("unpinned stale link survived the sweep")
	}
	if !linkExists(t, pool, src, keptTgt) {
		t.Error("kept link deleted")
	}
	if linkExists(t, pool, src, supTgt) {
		t.Error("unpinned supersedes link survived the sweep")
	}
	var lifecycle string
	var supersededBy *string
	if err := pool.QueryRow(ctx,
		`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, supTgt,
	).Scan(&lifecycle, &supersededBy); err != nil {
		t.Fatalf("read sup target: %v", err)
	}
	if lifecycle != "knowledge" || supersededBy != nil {
		t.Errorf("unpinned supersedes revert broken: lifecycle=%q superseded_by=%v", lifecycle, supersededBy)
	}

	// Sweep 2: keptTargets EMPTY (deliberate clear-out) — pinned still survives.
	sweepInTx(t, pool, src, nil)

	if !linkExists(t, pool, src, pinnedTgt) {
		t.Error("pinned link deleted by replace sweep (empty-kept clear-out shape)")
	}
	if linkExists(t, pool, src, keptTgt) {
		t.Error("unpinned kept link survived the empty-kept clear-out")
	}
}
