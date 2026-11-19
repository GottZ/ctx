//go:build integration

// Issue #236, points two and four: the writers of a block's title or content
// outside UpdateBlock and UpsertBlock.
//
// Point four. A title or content change marks the block's vector stale in the
// transaction of the change (InvalidateEmbeddingTx), and a write that leaves
// both as they are does not: the embed input is unchanged, and marking the
// vector would send the block back to the backfill for nothing (#216, #198).
// UpdateIssueBlock and the forge pull updates marked it whenever the field was
// SENT; the forge push rename ("#L5 …" → "#12 …", the issue and its comments)
// did not mark it at all, so the vector of the earlier title stayed
// current-looking and out of the backfill's reach.
//
// Point two. Nothing pinned that the invalidation commits with the write: a
// statement-level trigger that raises on the memo delete of
// InvalidateEmbeddingTx makes every writer fail, and the row must then hold
// its old text and its vector, unmarked.
//
//	go test -tags=integration ./internal/store/ -run 'TestBlockTextWritersMarkTheVector|TestInvalidationCommitsWithTheWrite' -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i236Vec is a constant 1024-dim vector.
func i236Vec() []float32 {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = 0.25
	}
	return v
}

// i236State is what the assertions read: whether a serving vector is there,
// whether it is marked stale, and the text.
type i236State struct {
	hasVec, stale  bool
	title, content string
}

func i236Read(t *testing.T, pool *pgxpool.Pool, id string) i236State {
	t.Helper()
	var s i236State
	if err := pool.QueryRow(context.Background(),
		`SELECT embedding IS NOT NULL, embedding_stale, title, content FROM context_blocks WHERE id = $1::uuid`, id,
	).Scan(&s.hasVec, &s.stale, &s.title, &s.content); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s
}

func i236Embed(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := store.StoreEmbedding(context.Background(), pool, id, "i236-model", i236Vec()); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}
}

// i236Tx runs fn in a committed transaction.
func i236Tx(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func wantMarked(t *testing.T, what string, s i236State) {
	t.Helper()
	if s.hasVec && !s.stale {
		t.Errorf("%s: the vector of the earlier text is kept unmarked (embedding_stale=false)", what)
	}
}

func wantKept(t *testing.T, what string, s i236State) {
	t.Helper()
	if !s.hasVec || s.stale {
		t.Errorf("%s: has_vector=%v embedding_stale=%v, want the vector kept and current — title and content did not change", what, s.hasVec, s.stale)
	}
}

func TestBlockTextWritersMarkTheVector(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"
	writable := []string{scope}

	newIssue := func(t *testing.T, title string) *store.Block {
		t.Helper()
		var b *store.Block
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			var err error
			b, err = store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: title, Content: "issue body"}, writable)
			return err
		}); err != nil {
			t.Fatalf("insert issue: %v", err)
		}
		return b
	}
	newComment := func(t *testing.T, parentID, author string) *store.Block {
		t.Helper()
		var b *store.Block
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			var err error
			b, err = store.InsertCommentBlock(ctx, tx, parentID, store.CommentFields{Author: author, Content: "comment body"}, writable)
			return err
		}); err != nil {
			t.Fatalf("insert comment: %v", err)
		}
		return b
	}
	str := func(s string) *string { return &s }

	t.Run("issue update", func(t *testing.T) {
		b := newIssue(t, "i236 resend")
		i236Embed(t, pool, b.ID)
		// The client sends the form as it is: the same human title, the same
		// body, new tags.
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			_, err := store.UpdateIssueBlock(ctx, tx, b.ID, store.IssueUpdate{Title: str("i236 resend"), Content: str("issue body"), Tags: []string{"x"}}, nil, writable)
			return err
		}); err != nil {
			t.Fatalf("resend: %v", err)
		}
		wantKept(t, "issue update re-sending title and body", i236Read(t, pool, b.ID))

		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			_, err := store.UpdateIssueBlock(ctx, tx, b.ID, store.IssueUpdate{Content: str("issue body, edited")}, nil, writable)
			return err
		}); err != nil {
			t.Fatalf("edit: %v", err)
		}
		wantMarked(t, "issue update with a new body", i236Read(t, pool, b.ID))
	})

	t.Run("forge pull updates", func(t *testing.T) {
		c := store.ForgeIssueContent{Number: 41, Title: "pulled", Body: "forge body", WorkflowStatus: "open"}
		var b *store.Block
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			var err error
			b, err = store.PullCreateIssueBlock(ctx, tx, scope, c)
			return err
		}); err != nil {
			t.Fatalf("pull create: %v", err)
		}
		i236Embed(t, pool, b.ID)
		// A forge-ahead change of the state and the labels only.
		c.WorkflowStatus, c.Labels = "closed", []string{"bug"}
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			_, err := store.PullUpdateIssueBlock(ctx, tx, b.ID, c)
			return err
		}); err != nil {
			t.Fatalf("pull update (state): %v", err)
		}
		wantKept(t, "pull update of the state only", i236Read(t, pool, b.ID))
		c.Body = "forge body, edited"
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			_, err := store.PullUpdateIssueBlock(ctx, tx, b.ID, c)
			return err
		}); err != nil {
			t.Fatalf("pull update (body): %v", err)
		}
		wantMarked(t, "pull update with a new body", i236Read(t, pool, b.ID))

		cm := newComment(t, b.ID, "dana")
		i236Embed(t, pool, cm.ID)
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			_, err := store.PullUpdateCommentBlock(ctx, tx, cm.ID, "comment body")
			return err
		}); err != nil {
			t.Fatalf("pull update comment (same): %v", err)
		}
		wantKept(t, "comment pull update with the same body", i236Read(t, pool, cm.ID))
		if err := i236Tx(t, pool, func(tx pgx.Tx) error {
			_, err := store.PullUpdateCommentBlock(ctx, tx, cm.ID, "comment body, edited")
			return err
		}); err != nil {
			t.Fatalf("pull update comment (edited): %v", err)
		}
		wantMarked(t, "comment pull update with a new body", i236Read(t, pool, cm.ID))
	})

	t.Run("forge push rename", func(t *testing.T) {
		issue := newIssue(t, "i236 pushed")
		cm := newComment(t, issue.ID, "erin")
		i236Embed(t, pool, issue.ID, cm.ID)
		if err := store.FinalizePushCreateIssue(ctx, pool, issue.ID, 77, "h", []byte(`{}`)); err != nil {
			t.Fatalf("finalize push create: %v", err)
		}
		is, cs := i236Read(t, pool, issue.ID), i236Read(t, pool, cm.ID)
		if is.title != "#77 i236 pushed" || cs.title != "#77.cL1 erin" {
			t.Fatalf("titles after the push = %q / %q, want the forge number", is.title, cs.title)
		}
		wantMarked(t, "issue renamed by the push", is)
		wantMarked(t, "comment renamed by the push cascade", cs)
	})
}

func TestInvalidationCommitsWithTheWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "private"

	ub, err := store.UpsertBlock(ctx, pool, "learnings", "i236 atomic update", "before", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	up, err := store.UpsertBlock(ctx, pool, "learnings", "i236 atomic upsert", "before", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var issue *store.Block
	if err := i236Tx(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: "i236 atomic issue", Content: "before"}, []string{scope})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	i236Embed(t, pool, ub.ID, up.ID, issue.ID)

	// The memo delete is the last statement of InvalidateEmbeddingTx; a
	// statement trigger fires even when it deletes no row.
	for _, st := range []string{
		`CREATE FUNCTION i236_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'i236: memo delete refused'; END $$`,
		`CREATE TRIGGER i236_refuse BEFORE DELETE ON context_embed_failures
		 FOR EACH STATEMENT EXECUTE FUNCTION i236_refuse()`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("install trigger: %v", err)
		}
	}
	after := "after"
	wantUntouched := func(t *testing.T, what, id string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error although the invalidation failed", what)
		}
		s := i236Read(t, pool, id)
		if s.content != "before" || !s.hasVec || s.stale {
			t.Errorf("%s: content=%q has_vector=%v stale=%v, want the write rolled back with its invalidation (content before, vector kept and current)", what, s.content, s.hasVec, s.stale)
		}
	}

	_, _, err = store.UpdateBlock(ctx, pool, ub.ID, store.UpdateBlockData{Content: &after}, []string{scope})
	wantUntouched(t, "UpdateBlock", ub.ID, err)

	_, err = store.UpsertBlock(ctx, pool, "learnings", "i236 atomic upsert", "after", nil, nil, scope, true, store.SensitivityWrite{}, "")
	wantUntouched(t, "UpsertBlock", up.ID, err)

	err = i236Tx(t, pool, func(tx pgx.Tx) error {
		_, err := store.UpdateIssueBlock(ctx, tx, issue.ID, store.IssueUpdate{Content: &after}, nil, []string{scope})
		return err
	})
	wantUntouched(t, "UpdateIssueBlock", issue.ID, err)
}
