//go:build integration

// GHSA 2026-10-31 §2 — raw read paths redact the guard's matched-block key:
// GetBlock and non-compact SearchBlocks drop metadata.guard_matched_id when
// the matched block lies outside the caller's read scopes (the rule GuardList
// applies to matched_id). The similarity and the cross-scope flag stay, as on
// GuardList.
//
//	go test -tags=integration ./internal/store/ -run TestGuardMatchedIDRedacted -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGuardMatchedIDRedacted_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		target = "019f3310-0000-7000-9000-00000000a001" // in scope work
		writer = "019f3310-0000-7000-9000-00000000a002" // in scope private, matched → target
		bogus  = "019f3310-0000-7000-9000-00000000a003" // metadata carries a non-uuid matched id
	)
	for _, r := range []struct{ id, scope, meta string }{
		{target, "work", `{}`},
		{writer, "private", `{"guard_similarity": 0.9311, "guard_matched_id": "` + target + `", "guard_is_cross_scope": true, "other": "kept"}`},
		{bogus, "private", `{"guard_matched_id": "not-a-uuid", "other": "kept"}`},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, metadata, lifecycle_state, type_name)
			 VALUES ($1::uuid, 'learnings', $1, 'body guard redact probe', $2, $3::jsonb, 'knowledge', 'knowledge')`,
			r.id, r.scope, r.meta); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// Caller can read private only: the matched block (work) is not readable
	// → guard_matched_id gone, the rest of metadata intact.
	b, err := store.GetBlock(ctx, pool, nil, writer, []string{"private"}, nil)
	if err != nil || b == nil {
		t.Fatalf("GetBlock(private): b=%v err=%v", b, err)
	}
	if _, has := b.Metadata["guard_matched_id"]; has {
		t.Errorf("guard_matched_id returned to a caller who cannot read the matched block: %v", b.Metadata)
	}
	if b.Metadata["other"] != "kept" || b.Metadata["guard_similarity"] == nil {
		t.Errorf("unrelated metadata keys were disturbed: %v", b.Metadata)
	}

	// Caller can read work too → the key is returned verbatim.
	b, err = store.GetBlock(ctx, pool, nil, writer, []string{"private", "work"}, nil)
	if err != nil || b == nil {
		t.Fatalf("GetBlock(private,work): b=%v err=%v", b, err)
	}
	if b.Metadata["guard_matched_id"] != target {
		t.Errorf("guard_matched_id = %v, want %s for a reader of the matched scope", b.Metadata["guard_matched_id"], target)
	}

	// A malformed matched id never errors the read (the uuid cast is guarded)
	// and is treated as not readable.
	b, err = store.GetBlock(ctx, pool, nil, bogus, []string{"private"}, nil)
	if err != nil || b == nil {
		t.Fatalf("GetBlock(bogus): b=%v err=%v", b, err)
	}
	if _, has := b.Metadata["guard_matched_id"]; has || b.Metadata["other"] != "kept" {
		t.Errorf("malformed guard_matched_id handling: %v", b.Metadata)
	}

	// Non-compact search returns metadata through the same expression.
	res, err := store.SearchBlocks(ctx, pool, nil, "guard redact probe", []string{"private"}, "", nil, 10, false, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("SearchBlocks: %v", err)
	}
	seen := 0
	for _, p := range res {
		if p.ID == writer {
			seen++
			if _, has := p.Metadata["guard_matched_id"]; has {
				t.Errorf("search (private) leaks guard_matched_id: %v", p.Metadata)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("search did not return the writer block (seen=%d)", seen)
	}
	res, err = store.SearchBlocks(ctx, pool, nil, "guard redact probe", []string{"private", "work"}, "", nil, 10, false, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("SearchBlocks: %v", err)
	}
	for _, p := range res {
		if p.ID == writer && p.Metadata["guard_matched_id"] != target {
			t.Errorf("search (private,work) dropped a readable guard_matched_id: %v", p.Metadata)
		}
	}
}
