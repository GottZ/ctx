//go:build integration

// Issue #414: the temporal review writes only while the block holds the
// content it picked. When phase 1 had to write and the write was not applied
// (the content was replaced, or the block is gone), the review's merge would
// be dropped by the same guard, so ValidateTemporal stops before the LLM call.
//
// Run with:
//
//	go test -tags=integration ./internal/dream/ -run TestValidateTemporalSkipsReviewWhenPhase1IsDropped -count=1 -v
package dream

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestValidateTemporalSkipsReviewWhenPhase1IsDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// A block without temporal rows, so phase 1 has to write.
	picked := "Offsite on 2026-06-02."
	b, err := store.UpsertBlock(ctx, pool, "i414", "i414-dream-skip", picked, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	info := &BlockInfo{ID: b.ID, Title: b.Title, Category: b.Category, Content: picked, Scope: b.Scope, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}

	// The content is replaced after the pick.
	replaced := "Offsite moved to 2026-09-08."
	if u, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &replaced}, []string{"private"}); err != nil || u == nil {
		t.Fatalf("update: %v", err)
	}

	// No router: reaching the LLM review would dereference it.
	if err := ValidateTemporal(ctx, pool, nil, llm.Options{}, info); err != nil {
		t.Fatalf("validate: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_temporal WHERE block_id = $1`, b.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Errorf("phase 1 wrote %d rows for the picked content over the replaced one", rows)
	}
}
