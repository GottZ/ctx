//go:build integration

// Integration tests for Achse-02 Welle I-A: context_structural_links (migration
// 076), the PutStructuralLink/DeleteStructuralLink store
// layer, the parent_id FK+CASCADE (E8) and the parent_id write path
// (PutBlockParent). testcontainers PG18, full migration chain (T3 house
// pattern). pgCode is declared in tenants_hybrid_integration_test.go (same
// store_test package).
//
// Run: go test -tags=integration ./internal/store/ -run TestStructuralLinks -count=1 -v.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStructuralLinks_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seed := func(t *testing.T, scope, title string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 VALUES ('issue', $1, 'body', $2) RETURNING id::text`, title, scope).Scan(&id); err != nil {
			t.Fatalf("seed block %q in %q: %v", title, scope, err)
		}
		return id
	}
	count := func(t *testing.T, q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	inTx := func(t *testing.T, fn func(tx pgx.Tx) error) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit tx: %v", err)
		}
		return nil
	}

	// (1) PK carries link_class: two DIFFERENT classes on the same pair coexist;
	// the SAME class re-put is an idempotent no-op (ON CONFLICT DO NOTHING).
	t.Run("pk_allows_two_classes_same_pair", func(t *testing.T) {
		s := seed(t, "sl-pk", "s")
		tgt := seed(t, "sl-pk", "t")
		put := func(class string) error {
			return inTx(t, func(tx pgx.Tx) error {
				return store.PutStructuralLink(ctx, tx,
					store.StructuralLink{SourceID: s, TargetID: tgt, LinkClass: class, Origin: "manual"},
					[]string{"sl-pk"})
			})
		}
		if err := put("references"); err != nil {
			t.Fatalf("put references: %v", err)
		}
		if err := put("duplicate-of"); err != nil {
			t.Fatalf("put duplicate-of: %v", err)
		}
		if got := count(t, `SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid`, s); got != 2 {
			t.Fatalf("two classes: got %d rows, want 2", got)
		}
		if err := put("references"); err != nil { // duplicate class ⇒ upsert no-op
			t.Fatalf("re-put references: %v", err)
		}
		if got := count(t, `SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid`, s); got != 2 {
			t.Fatalf("re-put same class: got %d rows, want 2 (ON CONFLICT DO NOTHING)", got)
		}
	})

	// (2) self-loop rejected (unified error) and the table CHECK is real.
	t.Run("self_loop_rejected", func(t *testing.T) {
		s := seed(t, "sl-self", "s")
		err := inTx(t, func(tx pgx.Tx) error {
			return store.PutStructuralLink(ctx, tx,
				store.StructuralLink{SourceID: s, TargetID: s, LinkClass: "references"}, []string{"sl-self"})
		})
		if !errors.Is(err, store.ErrLinkScopeViolation) {
			t.Fatalf("self-loop: err=%v, want ErrLinkScopeViolation", err)
		}
		// Direct INSERT proves the CHECK (source != target) exists (23514).
		_, derr := pool.Exec(ctx,
			`INSERT INTO context_structural_links (source_block_id,target_block_id,link_class,scope)
			 VALUES ($1::uuid,$1::uuid,'x','sl-self')`, s)
		if pgCode(derr) != "23514" {
			t.Fatalf("direct self-loop insert: pgCode=%q want 23514 (check_violation)", pgCode(derr))
		}
	})

	// (3) CASCADE: deleting the source block clears its structural edges.
	t.Run("cascade_on_source_delete", func(t *testing.T) {
		s := seed(t, "sl-cas", "s")
		tgt := seed(t, "sl-cas", "t")
		if err := inTx(t, func(tx pgx.Tx) error {
			return store.PutStructuralLink(ctx, tx,
				store.StructuralLink{SourceID: s, TargetID: tgt, LinkClass: "references"}, []string{"sl-cas"})
		}); err != nil {
			t.Fatalf("put: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id=$1::uuid`, s); err != nil {
			t.Fatalf("delete source block: %v", err)
		}
		if got := count(t, `SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid OR target_block_id=$1::uuid`, s); got != 0 {
			t.Fatalf("cascade on source delete: got %d edge rows, want 0", got)
		}
	})

	// (4) E8: parent_id FK CASCADE — deleting the parent block removes children.
	t.Run("cascade_parent_id_on_delete", func(t *testing.T) {
		parent := seed(t, "sl-par", "p")
		child := seed(t, "sl-par", "c")
		if err := inTx(t, func(tx pgx.Tx) error {
			return store.PutBlockParent(ctx, tx, child, parent, []string{"sl-par"})
		}); err != nil {
			t.Fatalf("put parent: %v", err)
		}
		if got := count(t, `SELECT count(*) FROM context_blocks WHERE parent_id=$1::uuid`, parent); got != 1 {
			t.Fatalf("parent_id set: got %d children, want 1", got)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id=$1::uuid`, parent); err != nil {
			t.Fatalf("delete parent: %v", err)
		}
		if got := count(t, `SELECT count(*) FROM context_blocks WHERE id=$1::uuid`, child); got != 0 {
			t.Fatalf("E8 CASCADE: child survived parent delete, got %d, want 0", got)
		}
	})

	// (5) Scope gate: foreign-scope target ⇒ unified error, 0 rows.
	t.Run("scope_gate_foreign_target", func(t *testing.T) {
		s := seed(t, "sl-a", "s")
		foreign := seed(t, "sl-b", "t")
		err := inTx(t, func(tx pgx.Tx) error {
			return store.PutStructuralLink(ctx, tx,
				store.StructuralLink{SourceID: s, TargetID: foreign, LinkClass: "references"}, []string{"sl-a"})
		})
		if !errors.Is(err, store.ErrLinkScopeViolation) {
			t.Fatalf("foreign target: err=%v, want ErrLinkScopeViolation", err)
		}
		if got := count(t, `SELECT count(*) FROM context_structural_links WHERE source_block_id=$1::uuid`, s); got != 0 {
			t.Fatalf("foreign target: %d rows written, want 0", got)
		}
	})

	// (6) Scope gate: source not in writable scopes ⇒ unified error.
	t.Run("scope_gate_source_not_writable", func(t *testing.T) {
		s := seed(t, "sl-a", "s2")
		tgt := seed(t, "sl-a", "t2")
		err := inTx(t, func(tx pgx.Tx) error {
			return store.PutStructuralLink(ctx, tx,
				store.StructuralLink{SourceID: s, TargetID: tgt, LinkClass: "references"}, []string{"sl-other"})
		})
		if !errors.Is(err, store.ErrLinkScopeViolation) {
			t.Fatalf("non-writable source: err=%v, want ErrLinkScopeViolation", err)
		}
	})

	// (7) READ second line — PORTED (GB6/E7): the former StructuralNeighbors
	// visibility subtest retired with the function. Its invariant (an injected
	// cross-scope edge never surfaces a foreign neighbour; same-scope IS
	// returned) lives on in the ego batch readers' gates with per-leg red
	// proofs: TestStructuralHop_VisibilityInLegs (Q1s, W2-G1 arms i/ii) and
	// TestEgoGraph_StructEdgesInduced's out-of-set arm (Q2s).

	// (8) parent_id write path: comment-scope invariant — cross-scope parent
	// rejected; same-scope accepted.
	t.Run("parent_scope_invariant", func(t *testing.T) {
		child := seed(t, "sl-p", "c")
		foreignParent := seed(t, "sl-p-foreign", "p")
		err := inTx(t, func(tx pgx.Tx) error {
			return store.PutBlockParent(ctx, tx, child, foreignParent, []string{"sl-p"})
		})
		if !errors.Is(err, store.ErrLinkScopeViolation) {
			t.Fatalf("cross-scope parent: err=%v, want ErrLinkScopeViolation", err)
		}
		localParent := seed(t, "sl-p", "p2")
		if err := inTx(t, func(tx pgx.Tx) error {
			return store.PutBlockParent(ctx, tx, child, localParent, []string{"sl-p"})
		}); err != nil {
			t.Fatalf("same-scope parent: %v", err)
		}
	})
}
