//go:build integration

// Integration test for the block-workbench W6 list badge: SearchBlocks must
// carry each result's sensitivity level so the /blocks hit list can render the
// badge (the column exists since migration 055 — NO migration here, just the
// compact SELECT + scan).
//
// RED: the BlockPreview.Sensitivity field exists (stub) but the SearchBlocks
// compact SELECT does not select the column, so it stays empty — this test
// fails until GREEN adds `sensitivity` to the SELECT list and the rows.Scan.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestSearchBlocksSensitivity -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertSearchBlock seeds one block and stamps its sensitivity via a follow-up
// UPDATE (the upsert path's SensitivityWrite only UPGRADES on conflict; a plain
// post-insert UPDATE is the unambiguous way to pin an exact level for the
// assertion — mirrors insertAuditBlock's follow-up-UPDATE trick).
func insertSearchBlock(t *testing.T, pool *pgxpool.Pool, scope, title, sensitivity string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'content of ' || $1, $2)
		 RETURNING id`, title, scope).Scan(&id); err != nil {
		t.Fatalf("insert block %s: %v", title, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET sensitivity = $2 WHERE id = $1`, id, sensitivity); err != nil {
		t.Fatalf("stamp sensitivity %s: %v", sensitivity, err)
	}
	return id
}

func TestSearchBlocksSensitivity_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	internalID := insertSearchBlock(t, pool, "private", "w6-internal-block", "internal")
	credsID := insertSearchBlock(t, pool, "private", "w6-credentials-block", "credentials")

	// Compact search is the /blocks list path (compact=true → preview shape).
	// Empty query → updated_at DESC over the home scope.
	got, err := store.SearchBlocks(ctx, pool, "", []string{"private"}, "", nil, 50, true, nil, nil)
	if err != nil {
		t.Fatalf("search blocks: %v", err)
	}

	bySens := map[string]string{} // id -> sensitivity carried in the preview
	for _, bp := range got {
		bySens[bp.ID] = bp.Sensitivity
	}

	if bySens[internalID] != "internal" {
		t.Errorf("compact SearchBlocks dropped sensitivity for the internal block: got %q, want %q",
			bySens[internalID], "internal")
	}
	if bySens[credsID] != "credentials" {
		t.Errorf("compact SearchBlocks dropped sensitivity for the credentials block: got %q, want %q",
			bySys(bySens, credsID), "credentials")
	}
}

// bySys is a tiny readability helper so the error message above reads cleanly.
func bySys(m map[string]string, id string) string { return m[id] }
