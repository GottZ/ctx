//go:build integration

// Integration tests for WF T11 aggregate-to-parent fold (design/01 §4.6) against
// a live PG18 (testdb). Two layers:
//
//   - TestFoldAggregates_FullChain: real ctx_rrf ranking → QueryHandler
//     .foldAggregates. Every block shares one embedding so the vector arm ranks
//     them equally and FOLD — not relevance — is isolated. Proves the wired path:
//     parents delivered, their comments folded away (fold-negativ via the
//     merge/collapse branch — RED before the fold, comments appear alongside),
//     orphan kept, cross-scope child kept raw with NO foreign-parent leak.
//   - TestFoldParentData_Hydration: the batched DB half directly — visible parent
//     hydrated (row carried), parent_id NULL ⇒ orphan, foreign-scope parent NOT
//     hydrated (fail-closed). This is the hydrate-branch proof independent of
//     which blocks ctx_rrf chose to rank.
//
//	go test -tags=integration ./internal/handler/ -run 'TestFoldAggregates|TestFoldParentData' -count=1 -v
package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func foldEmbedding() []float32 {
	e := make([]float32, 1024)
	for i := range e {
		e[i] = 0.1
	}
	return e
}

func foldInsert(t *testing.T, pool *pgxpool.Pool, id, scope, typeName, parent string, embedding []float32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var vec, parPtr any
	if embedding != nil {
		vec = pgvec.NewVector(embedding)
	}
	if parent != "" {
		parPtr = parent
	}
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, type_name, parent_id)
		 VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$7,$8,$9::uuid)`,
		id, "fold", fmt.Sprintf("fold-title-%s", id[len(id)-4:]), "fold body neutral", scope, vec, now, typeName, parPtr)
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// foldTestSet: knowledge = full-pass default; comment = aggregate-to-parent.
// Built directly (NewSet skips the write-time cross-field validator) so the fold
// consumer runs before I-D unlocks parent.mode.
func foldTestSet(t *testing.T) *blocktype.Set {
	t.Helper()
	set, err := blocktype.NewSet([]blocktype.Policy{
		{Name: "knowledge", Scope: "_global", IsDefault: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass}},
		{Name: "comment", Scope: "_global",
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalAggregateToParent},
			Parent:    blocktype.ParentPolicy{Mode: blocktype.ParentModeOptional}},
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

const (
	foldHome    = "fold-home"
	foldForeign = "fold-foreign"

	fIssueA    = "019f1100-0000-7000-9000-0000000000a1"
	fCommentA  = "019f1100-0000-7000-9000-0000000000a2"
	fIssueB    = "019f1100-0000-7000-9000-0000000000b1"
	fCommentB1 = "019f1100-0000-7000-9000-0000000000b2"
	fCommentB2 = "019f1100-0000-7000-9000-0000000000b3"
	fOrphan    = "019f1100-0000-7000-9000-0000000000c1"
	fForeignI  = "019f1100-0000-7000-9000-0000000000d1"
	fCommentX  = "019f1100-0000-7000-9000-0000000000d2"
)

func foldSeedCorpus(t *testing.T, pool *pgxpool.Pool) {
	emb := foldEmbedding()
	foldInsert(t, pool, fIssueA, foldHome, "knowledge", "", emb)
	foldInsert(t, pool, fCommentA, foldHome, "comment", fIssueA, emb)
	foldInsert(t, pool, fIssueB, foldHome, "knowledge", "", emb)
	foldInsert(t, pool, fCommentB1, foldHome, "comment", fIssueB, emb)
	foldInsert(t, pool, fCommentB2, foldHome, "comment", fIssueB, emb)
	foldInsert(t, pool, fOrphan, foldHome, "comment", "", emb)
	foldInsert(t, pool, fForeignI, foldForeign, "knowledge", "", emb) // foreign scope
	foldInsert(t, pool, fCommentX, foldHome, "comment", fForeignI, emb)
}

func TestFoldAggregates_FullChain_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	foldSeedCorpus(t, pool)

	set := foldTestSet(t)
	readScopes := []string{foldHome}
	visible := set.VisibleTypes()

	results, err := rrf.Search(ctx, pool, foldEmbedding(), "zzqqxx", "zzqqxx",
		readScopes, nil, nil, 50, "", "", visible, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("rrf.Search: %v", err)
	}

	present := func(rs []rrf.SearchResult) map[string]int {
		m := map[string]int{}
		for _, r := range rs {
			m[r.ID]++
		}
		return m
	}
	pre := present(results)
	// Pre-fold: every home comment ranks as ITSELF (this is what the fold undoes).
	for _, id := range []string{fCommentA, fCommentB1, fCommentB2, fOrphan, fCommentX} {
		if pre[id] == 0 {
			t.Fatalf("pre-fold: comment %s did not rank; got %v", id, pre)
		}
	}
	if pre[fForeignI] != 0 {
		t.Fatalf("pre-fold: foreign-scope issue must be out of read scope; got %v", pre)
	}

	h := &QueryHandler{pool: pool}
	folded := h.foldAggregates(ctx, results, set.AggregateTypes(), visible, readScopes, nil, "fold-it")
	post := present(folded)

	// Fold-negativ + merge: parents delivered, their comments folded away.
	if post[fIssueA] != 1 || post[fCommentA] != 0 {
		t.Errorf("merge A: issueA=%d commentA=%d, want 1/0", post[fIssueA], post[fCommentA])
	}
	// Collapse: exactly ONE issueB, both comments gone.
	if post[fIssueB] != 1 {
		t.Errorf("collapse: issueB count=%d, want 1", post[fIssueB])
	}
	if post[fCommentB1]+post[fCommentB2] != 0 {
		t.Errorf("collapse: B comments leaked (b1=%d b2=%d)", post[fCommentB1], post[fCommentB2])
	}
	// Orphan: kept raw.
	if post[fOrphan] != 1 {
		t.Errorf("orphan: count=%d, want 1", post[fOrphan])
	}
	// Cross-scope invisible parent: child kept raw, foreign parent never delivered.
	if post[fCommentX] != 1 {
		t.Errorf("cross-scope: commentX count=%d, want 1 (kept raw)", post[fCommentX])
	}
	if post[fForeignI] != 0 {
		t.Errorf("LEAK: foreign-scope parent %s delivered through its child", fForeignI)
	}
}

func TestFoldParentData_Hydration_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	foldSeedCorpus(t, pool)

	set := foldTestSet(t)
	h := &QueryHandler{pool: pool}

	folds, hydrated, err := h.foldParentData(ctx,
		[]string{fCommentA, fOrphan, fCommentX}, set.VisibleTypes(), []string{foldHome}, nil)
	if err != nil {
		t.Fatalf("foldParentData: %v", err)
	}

	// Visible parent: fold decision points at it AND the row is hydrated.
	if cf := folds[fCommentA]; cf.parentID != fIssueA || !cf.visible {
		t.Errorf("commentA fold = %+v, want parent=%s visible=true", cf, fIssueA)
	}
	if p, ok := hydrated[fIssueA]; !ok || p.ID != fIssueA || p.Title == "" || p.TypeName != "knowledge" {
		t.Errorf("issueA not hydrated correctly: ok=%v %+v", ok, p)
	}
	// Orphan: parent_id NULL ⇒ empty fold, no hydration.
	if cf := folds[fOrphan]; cf.parentID != "" {
		t.Errorf("orphan fold = %+v, want empty parentID", cf)
	}
	// Foreign-scope parent: decision carries the id but visible=false and the
	// row is NOT hydrated (fail-closed — no leak surface).
	if cf := folds[fCommentX]; cf.parentID != fForeignI || cf.visible {
		t.Errorf("commentX fold = %+v, want parent=%s visible=false", cf, fForeignI)
	}
	if _, ok := hydrated[fForeignI]; ok {
		t.Errorf("LEAK: foreign-scope parent hydrated despite failing visibility")
	}
}
