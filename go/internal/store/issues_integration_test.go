//go:build integration

// Integration tests for Achse-02 Welle I-D: the store issue/comment layer
// (issues.go) — InsertIssueBlock (#L<seq> per-scope local sequence),
// InsertCommentBlock (comment-scope invariant, composes PutBlockParent),
// UpdateIssueBlock (transition against POLICY DATA), GetIssue/ListIssues/
// ListComments (visibility isolation). testcontainers PG18, full migration chain
// incl. the 084 issue/comment seeds (T3 house pattern). pgCode is declared in
// tenants_hybrid_integration_test.go (same store_test package).
//
// Run: go test -tags=integration ./internal/store/ -run TestIssues -count=1 -v.
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// issueSet is the compiled-in builtin registry snapshot (issue = backlog/
// in-progress/done, forge open→backlog/closed→done — mirrors migration 084).
var issueSet = blocktype.NewRegistry().Snapshot()

func TestIssues_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	inTx := func(t *testing.T, fn func(tx pgx.Tx) error) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
	// insertIssue commits one issue and returns it.
	insertIssue := func(t *testing.T, scope, title string, writable []string) *store.Block {
		t.Helper()
		var b *store.Block
		err := inTx(t, func(tx pgx.Tx) error {
			var e error
			b, e = store.InsertIssueBlock(ctx, tx, store.IssueFields{
				Scope: scope, Title: title, Content: "body of " + title,
				Status: issueSet.WorkflowInitial(store.IssueTypeName),
			}, writable)
			return e
		})
		if err != nil {
			t.Fatalf("insert issue %q in %q: %v", title, scope, err)
		}
		return b
	}

	t.Run("local_seq_per_scope_and_status", func(t *testing.T) {
		a1 := insertIssue(t, "iss-a", "first", []string{"iss-a"})
		a2 := insertIssue(t, "iss-a", "second", []string{"iss-a"})
		b1 := insertIssue(t, "iss-b", "first", []string{"iss-b"})
		if !strings.HasPrefix(a1.Title, "#L1 ") || !strings.HasPrefix(a2.Title, "#L2 ") {
			t.Fatalf("per-scope seq broken: %q / %q", a1.Title, a2.Title)
		}
		if !strings.HasPrefix(b1.Title, "#L1 ") {
			t.Fatalf("scope B seq not independent: %q", b1.Title)
		}
		if a1.TypeName != store.IssueTypeName || a1.WorkflowStatus != "backlog" {
			t.Fatalf("issue type/status = %q/%q, want issue/backlog", a1.TypeName, a1.WorkflowStatus)
		}
	})

	t.Run("insert_foreign_scope_rejected", func(t *testing.T) {
		err := inTx(t, func(tx pgx.Tx) error {
			_, e := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: "iss-x", Title: "nope"}, []string{"iss-a"})
			return e
		})
		if !errors.Is(err, store.ErrIssueScope) {
			t.Fatalf("foreign scope: err=%v, want ErrIssueScope", err)
		}
	})

	t.Run("comment_scope_invariant", func(t *testing.T) {
		parent := insertIssue(t, "iss-c", "with comments", []string{"iss-c"})
		var c *store.Block
		if err := inTx(t, func(tx pgx.Tx) error {
			var e error
			c, e = store.InsertCommentBlock(ctx, tx, parent.ID,
				store.CommentFields{Author: "alice", Content: "hi"}, []string{"iss-c"})
			return e
		}); err != nil {
			t.Fatalf("same-scope comment: %v", err)
		}
		if c.Scope != "iss-c" || c.TypeName != store.CommentTypeName {
			t.Fatalf("comment scope/type = %q/%q, want iss-c/comment", c.Scope, c.TypeName)
		}
		if !strings.HasPrefix(c.Title, "#L1.cL1 ") {
			t.Fatalf("comment title = %q, want #L1.cL1 prefix", c.Title)
		}
		// Comment on a parent whose scope the caller cannot write ⇒ uniform
		// ErrLinkScopeViolation (the comment-scope invariant, rot ohne Parent-
		// Scope-Zwang: without deriving scope from the parent this would insert a
		// cross-scope comment).
		err := inTx(t, func(tx pgx.Tx) error {
			_, e := store.InsertCommentBlock(ctx, tx, parent.ID,
				store.CommentFields{Author: "eve", Content: "x"}, []string{"iss-other"})
			return e
		})
		if !errors.Is(err, store.ErrLinkScopeViolation) {
			t.Fatalf("cross-scope comment: err=%v, want ErrLinkScopeViolation", err)
		}
		// Empty parent ⇒ orphan prevention (required-parent comment type).
		err = inTx(t, func(tx pgx.Tx) error {
			_, e := store.InsertCommentBlock(ctx, tx, "", store.CommentFields{Content: "x"}, []string{"iss-c"})
			return e
		})
		if !errors.Is(err, store.ErrCommentParentRequired) {
			t.Fatalf("empty parent: err=%v, want ErrCommentParentRequired", err)
		}
	})

	t.Run("update_transition_policy_data", func(t *testing.T) {
		iss := insertIssue(t, "iss-u", "movable", []string{"iss-u"})
		inProg := "in-progress"
		// Valid transition backlog→in-progress.
		if err := inTx(t, func(tx pgx.Tx) error {
			_, e := store.UpdateIssueBlock(ctx, tx, iss.ID, store.IssueUpdate{Status: &inProg}, issueSet, []string{"iss-u"})
			return e
		}); err != nil {
			t.Fatalf("valid transition: %v", err)
		}
		// Invalid target status ⇒ blocktype.ErrInvalidTransition (→ handler 422).
		bogus := "shipped"
		err := inTx(t, func(tx pgx.Tx) error {
			_, e := store.UpdateIssueBlock(ctx, tx, iss.ID, store.IssueUpdate{Status: &bogus}, issueSet, []string{"iss-u"})
			return e
		})
		if !errors.Is(err, blocktype.ErrInvalidTransition) {
			t.Fatalf("bogus status: err=%v, want ErrInvalidTransition", err)
		}
		// POLICY-DATA swap proof (I-B parity): the SAME UpdateIssueBlock call with
		// a Set whose issue config INCLUDES "shipped" now ACCEPTS it — no Go
		// change, the validity flipped with the data.
		shippedCfg := `{"v":1,"workflow":{"states":["backlog","in-progress","shipped"],"initial":"backlog"}}`
		altIssue, e := blocktype.DecodePolicy("issue", "_global", true, false, []byte(shippedCfg))
		if e != nil {
			t.Fatalf("decode alt issue: %v", e)
		}
		def, e := blocktype.DecodePolicy("knowledge", "_global", true, true, []byte(`{"v":1}`))
		if e != nil {
			t.Fatalf("decode knowledge: %v", e)
		}
		altSet, e := blocktype.NewSet([]blocktype.Policy{def, altIssue})
		if e != nil {
			t.Fatalf("new set: %v", e)
		}
		if err := inTx(t, func(tx pgx.Tx) error {
			_, e := store.UpdateIssueBlock(ctx, tx, iss.ID, store.IssueUpdate{Status: &bogus}, altSet, []string{"iss-u"})
			return e
		}); err != nil {
			t.Fatalf("policy-data swap: shipped should be valid under altSet, got %v", err)
		}
	})

	t.Run("update_foreign_scope_not_found", func(t *testing.T) {
		iss := insertIssue(t, "iss-w", "guarded", []string{"iss-w"})
		newTitle := "renamed"
		err := inTx(t, func(tx pgx.Tx) error {
			_, e := store.UpdateIssueBlock(ctx, tx, iss.ID, store.IssueUpdate{Title: &newTitle}, issueSet, []string{"iss-elsewhere"})
			return e
		})
		if !errors.Is(err, store.ErrIssueNotFound) {
			t.Fatalf("foreign-scope update: err=%v, want ErrIssueNotFound (no oracle)", err)
		}
	})

	t.Run("list_and_get_isolation", func(t *testing.T) {
		insertIssue(t, "iss-l", "one", []string{"iss-l"})
		insertIssue(t, "iss-l", "two", []string{"iss-l"})
		states := issueSet.WorkflowStates(store.IssueTypeName)
		rows, _, err := store.ListIssues(ctx, pool, store.IssueListQuery{Scopes: []string{"iss-l"}, Statuses: states, Limit: 50})
		if err != nil {
			t.Fatalf("list own scope: %v", err)
		}
		if len(rows) < 2 {
			t.Fatalf("own-scope list returned %d, want ≥2", len(rows))
		}
		// Cross-tenant: a foreign readScope sees NOTHING.
		foreign, _, err := store.ListIssues(ctx, pool, store.IssueListQuery{Scopes: []string{"iss-foreign"}, Statuses: states, Limit: 50})
		if err != nil {
			t.Fatalf("list foreign scope: %v", err)
		}
		if len(foreign) != 0 {
			t.Fatalf("cross-tenant LEAK: foreign scope saw %d issue rows", len(foreign))
		}
		// GetIssue visibility: own scope ok, foreign scope ⇒ ErrIssueNotFound.
		got := insertIssue(t, "iss-g", "gettable", []string{"iss-g"})
		if _, err := store.GetIssue(ctx, pool, got.ID, []string{"iss-g"}, nil); err != nil {
			t.Fatalf("get own: %v", err)
		}
		if _, err := store.GetIssue(ctx, pool, got.ID, []string{"iss-foreign"}, nil); !errors.Is(err, store.ErrIssueNotFound) {
			t.Fatalf("get foreign: err=%v, want ErrIssueNotFound", err)
		}
	})
}
