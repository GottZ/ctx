//go:build integration

// GHSA 2026-10-31 §2 — raw read paths redact the guard's matched-block key:
// GetBlock and non-compact SearchBlocks drop metadata.guard_matched_id when
// the matched block lies outside the caller's read scopes (the rule GuardList
// applies to matched_id). The similarity and the cross-scope flag stay, as on
// GuardList. §F-§2 extends the rule to the paths that still returned the key
// verbatim: the UpdateBlock RETURNING (both execution paths) and the issue
// projection (GetIssue, ListComments, the comment page, the UpdateIssueBlock
// RETURNING).
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

// TestGuardMatchedIDRedacted_WritePathsAndIssues_Integration covers the
// paths §F-§2 found unredacted. The write paths redact against the write
// scopes they bind, the issue readers against their read scopes.
func TestGuardMatchedIDRedacted_WritePathsAndIssues_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		target = "019f3310-0000-7000-9000-00000000b001" // in scope work
		writer = "019f3310-0000-7000-9000-00000000b002" // in scope private, matched → target
	)
	for _, r := range []struct{ id, scope, meta string }{
		{target, "work", `{}`},
		{writer, "private", `{"guard_status": "needs_review", "guard_similarity": 0.9311, "guard_matched_id": "` + target + `", "guard_is_cross_scope": true, "other": "kept"}`},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, metadata, lifecycle_state, type_name, guard_status)
			 VALUES ($1::uuid, 'learnings', $1, 'body guard redact write probe', $2, $3::jsonb, 'knowledge', 'knowledge', 'needs_review')`,
			r.id, r.scope, r.meta); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	private := []string{"private"}
	both := []string{"private", "work"}
	hasMatch := func(label string, meta map[string]any, want bool) {
		t.Helper()
		v, has := meta["guard_matched_id"]
		switch {
		case want && v != target:
			t.Errorf("%s: guard_matched_id = %v, want %s for a caller of the matched scope", label, v, target)
		case !want && has:
			t.Errorf("%s: guard_matched_id %v returned to a caller who cannot reach the matched block", label, v)
		}
		if meta["other"] != "kept" || meta["guard_similarity"] == nil {
			t.Errorf("%s: unrelated metadata keys were disturbed: %v", label, meta)
		}
	}

	// UpdateBlock, plain path: the RETURNING carried the key verbatim.
	u, _, err := store.UpdateBlock(ctx, pool, writer, store.UpdateBlockData{Tags: []string{"probe"}}, private)
	if err != nil || u == nil {
		t.Fatalf("UpdateBlock(private): u=%v err=%v", u, err)
	}
	hasMatch("UpdateBlock(private)", u.Metadata, false)
	u, _, err = store.UpdateBlock(ctx, pool, writer, store.UpdateBlockData{Tags: []string{"probe", "two"}}, both)
	if err != nil || u == nil {
		t.Fatalf("UpdateBlock(private,work): u=%v err=%v", u, err)
	}
	hasMatch("UpdateBlock(private,work)", u.Metadata, true)

	// UpdateBlock, scope-move path (one transaction with the link sweep):
	// the same query string, so the same redaction.
	scope := "private"
	u, _, err = store.UpdateBlock(ctx, pool, writer, store.UpdateBlockData{Scope: &scope}, private)
	if err != nil || u == nil {
		t.Fatalf("UpdateBlock(scope move, private): u=%v err=%v", u, err)
	}
	hasMatch("UpdateBlock(scope move, private)", u.Metadata, false)

	// The redaction is a projection: the stored row keeps the key.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT metadata->>'guard_matched_id' FROM context_blocks WHERE id = $1::uuid`, writer).Scan(&stored); err != nil || stored != target {
		t.Fatalf("stored guard_matched_id = %q (err %v), want %s", stored, err, target)
	}

	// A knowledge block re-typed to issue (manage update `type`) keeps its
	// guard metadata and becomes reachable through the issue readers.
	issueType := store.IssueTypeName
	if u, _, err = store.UpdateBlock(ctx, pool, writer, store.UpdateBlockData{Type: &issueType}, both); err != nil || u == nil {
		t.Fatalf("UpdateBlock(type issue): u=%v err=%v", u, err)
	}
	iss, err := store.GetIssue(ctx, pool, writer, private, nil)
	if err != nil {
		t.Fatalf("GetIssue(private): %v", err)
	}
	hasMatch("GetIssue(private)", iss.Metadata, false)
	if iss, err = store.GetIssue(ctx, pool, writer, both, nil); err != nil {
		t.Fatalf("GetIssue(private,work): %v", err)
	}
	hasMatch("GetIssue(private,work)", iss.Metadata, true)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ui, err := store.UpdateIssueBlock(ctx, tx, writer, store.IssueUpdate{Tags: []string{"probe", "issue"}}, nil, private)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("UpdateIssueBlock(private): %v", err)
	}
	hasMatch("UpdateIssueBlock(private)", ui.Metadata, false)
	c, err := store.InsertCommentBlock(ctx, tx, writer, store.CommentFields{Author: "probe", Content: "comment body"}, private)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertCommentBlock: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Comments are not guard-checked; plant the decision keys on one to pin
	// the comment projections to the same redaction.
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET metadata = metadata || jsonb_build_object(
		     'guard_similarity', 0.9, 'guard_matched_id', $2::text, 'other', 'kept')
		  WHERE id = $1::uuid`, c.ID, target); err != nil {
		t.Fatalf("plant comment guard keys: %v", err)
	}
	comments, err := store.ListComments(ctx, pool, writer, private, nil)
	if err != nil || len(comments) != 1 {
		t.Fatalf("ListComments(private): n=%d err=%v", len(comments), err)
	}
	hasMatch("ListComments(private)", comments[0].Metadata, false)
	if comments, err = store.ListComments(ctx, pool, writer, both, nil); err != nil || len(comments) != 1 {
		t.Fatalf("ListComments(private,work): n=%d err=%v", len(comments), err)
	}
	hasMatch("ListComments(private,work)", comments[0].Metadata, true)
	page, _, err := store.ListCommentsPage(ctx, pool, writer, "private", 0, nil)
	if err != nil || len(page) != 1 {
		t.Fatalf("ListCommentsPage(private): n=%d err=%v", len(page), err)
	}
	hasMatch("ListCommentsPage(private)", page[0].Metadata, false)
}
