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
//
//   - TestFoldParentData_Hydration: the batched DB half directly — visible parent
//     hydrated (row carried), parent_id NULL ⇒ orphan, foreign-scope parent NOT
//     hydrated (fail-closed). This is the hydrate-branch proof independent of
//     which blocks ctx_rrf chose to rank.
//
//     go test -tags=integration ./internal/handler/ -run 'TestFoldAggregates|TestFoldParentData' -count=1 -v
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

	results, _, err := rrf.Search(ctx, pool, foldEmbedding(), "zzqqxx", "zzqqxx",
		readScopes, nil, nil, 50, "", "", visible, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
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

	// I-E §4.4: a folded parent carries matched_comment (id + preview of the best
	// child). issueA has exactly one comment ⇒ deterministic attribution.
	find := func(id string) *rrf.SearchResult {
		for i := range folded {
			if folded[i].ID == id {
				return &folded[i]
			}
		}
		return nil
	}
	if a := find(fIssueA); a == nil || a.MatchedComment == nil || a.MatchedComment.ID != fCommentA {
		t.Errorf("issueA matched_comment = %+v, want child %s (I-E §4.4)", a, fCommentA)
	} else if a.MatchedComment.Preview == "" {
		t.Error("issueA matched_comment preview empty")
	}
	// issueB collapses two comments ⇒ matched_comment is one of them (best-ranked).
	if b := find(fIssueB); b == nil || b.MatchedComment == nil ||
		(b.MatchedComment.ID != fCommentB1 && b.MatchedComment.ID != fCommentB2) {
		t.Errorf("issueB matched_comment = %+v, want one of b1/b2", b)
	}
	// A child kept raw (cross-scope) must NOT carry a matched_comment (it is the
	// comment itself, not a folded parent).
	if x := find(fCommentX); x != nil && x.MatchedComment != nil {
		t.Errorf("raw child commentX must not carry matched_comment, got %+v", x.MatchedComment)
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

const (
	scaleHome    = "scale-home"
	scaleForeign = "scale-foreign"
)

func scaleCommentID(ii, c int) string {
	return fmt.Sprintf("019f2201-%04d-7000-9000-%012d", ii, c)
}

// seedScaleCorpus plants 50 parent issues + ~1k comments: hot issues 0,1,2 with
// 100 comments each (300) and tail issues 3..49 with 15 each (705) = 1005
// comments. Parents are type 'knowledge' (a parent's type is immaterial to the
// fold); comments carry parent_id (FK 076). No embeddings — these tests drive the
// fold + over-fetch helper directly, not ctx_rrf.
func seedScaleCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name, created_at, updated_at)
		SELECT format('019f2200-0000-7000-9000-%s', lpad(ii::text,12,'0'))::uuid,
		       'issue', 'scale issue '||ii, 'issue body', $1, 'knowledge', now(), now()
		FROM generate_series(0,49) ii`, scaleHome); err != nil {
		t.Fatalf("seed issues: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name, parent_id, created_at, updated_at)
		SELECT format('019f2201-%s-7000-9000-%s', lpad(ii::text,4,'0'), lpad(c::text,12,'0'))::uuid,
		       'comment', 'scale comment '||ii||'-'||c, 'comment body '||ii||'-'||c, $1, 'comment',
		       format('019f2200-0000-7000-9000-%s', lpad(ii::text,12,'0'))::uuid, now(), now()
		FROM generate_series(0,49) ii,
		     LATERAL generate_series(1, CASE WHEN ii < 3 THEN 100 ELSE 15 END) c`, scaleHome); err != nil {
		t.Fatalf("seed comments: %v", err)
	}
}

// TestAggregateOverFetch_ScaleDiversity_Integration is the I-E scale gate
// (design/02 §4.4 / §7 I-E): 1k comments on 50 issues, a skewed candidate pool.
// Without over-fetch the base-200 window is monopolised by the hot threads and
// the fold collapses to <5 distinct parents (RED); with the ×2 over-fetch window
// the tail parents survive the collapse, the result reaches the user limit AND
// carries >=5 distinct parents (GREEN).
//
// Only the candidate ORDER is synthetic — foldParentData resolves every parent
// against the real DB (084+085 applied) and aggregateOverFetchLimit runs the real
// presence probe. ctx_rrf's per-arm candidate caps (semantic 75 / FTS 100/30)
// make this exact skew unreachable through the live function AND cap the pool at
// ~305, so the p_limit over-fetch is bounded by that ceiling; a per-parent cap
// INSIDE ctx_rrf's arms (Achse-01) is the complete fix. This test isolates what
// I-E owns: the fold + the over-fetch WINDOW.
func TestAggregateOverFetch_ScaleDiversity_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedScaleCorpus(t, pool)

	set := foldTestSet(t)
	h := &QueryHandler{pool: pool}
	readScopes := []string{scaleHome}
	aggTypes := set.AggregateTypes()
	visible := set.VisibleTypes()
	const userLimit = 5

	var results []rrf.SearchResult
	score := 100000.0
	mk := func(id, content string) {
		results = append(results, rrf.SearchResult{ID: id, RRFScore: score, TypeName: "comment", Content: content})
		score -= 1.0
	}
	for ii := 0; ii < 3; ii++ { // hot: 3 issues x 100 comments dominate the top
		for c := 1; c <= 100; c++ {
			mk(scaleCommentID(ii, c), fmt.Sprintf("hot comment %d-%d", ii, c))
		}
	}
	for c := 1; c <= 15; c++ { // tail: interleaved so the first tail rows span issues
		for ii := 3; ii <= 49; ii++ {
			mk(scaleCommentID(ii, c), fmt.Sprintf("tail comment %d-%d", ii, c))
		}
	}

	distinctParents := func(rs []rrf.SearchResult) int {
		seen := map[string]bool{}
		for _, r := range rs {
			seen[r.ID] = true
		}
		return len(seen)
	}
	truncate := func(rs []rrf.SearchResult, n int) []rrf.SearchResult {
		if len(rs) > n {
			return rs[:n]
		}
		return rs
	}

	// RED: base window (200) = hot only ⇒ <5 distinct parents, under-fills limit.
	baseWindow := truncate(results, 200)
	baseFolded := h.foldAggregates(ctx, baseWindow, aggTypes, visible, readScopes, nil, "scale-base")
	baseOut := truncate(baseFolded, userLimit)
	if p := distinctParents(baseOut); p >= 5 {
		t.Fatalf("base(200) distinct parents = %d, expected <5 (RED: hot thread monopolises the window)", p)
	} else {
		t.Logf("RED proof: base(200)-window fold ⇒ %d distinct parents in %d rows (< limit %d)", p, len(baseOut), userLimit)
	}

	// GREEN: helper widens 200 → 400 (comments present in scope); tail parents survive.
	of := h.aggregateOverFetchLimit(ctx, 200, aggTypes, readScopes)
	if of != 400 {
		t.Fatalf("aggregateOverFetchLimit = %d, want 400 (×2 widen when aggregate blocks present)", of)
	}
	ofWindow := truncate(results, of)
	ofFolded := h.foldAggregates(ctx, ofWindow, aggTypes, visible, readScopes, nil, "scale-of")
	ofOut := truncate(ofFolded, userLimit)
	if len(ofOut) != userLimit {
		t.Errorf("over-fetch out rows = %d, want %d (must reach the user limit)", len(ofOut), userLimit)
	}
	if p := distinctParents(ofOut); p < 5 {
		t.Errorf("over-fetch distinct parents = %d, want >=5", p)
	} else {
		t.Logf("GREEN: over-fetch(%d)-window fold ⇒ %d distinct parents in %d rows (reaches limit)", of, p, len(ofOut))
	}
}

// TestAggregateOverFetchLimit_EvalNeutral_Integration pins the eval-neutrality of
// the over-fetch: it widens ONLY when the queried read-scopes actually hold an
// aggregating-type block. A corpus without comments (the eval baseline) keeps the
// base limit ⇒ the rrf.Search call and every downstream stage stay byte-identical.
func TestAggregateOverFetchLimit_EvalNeutral_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := &QueryHandler{pool: pool}
	aggTypes := []string{"comment"}

	if got := h.aggregateOverFetchLimit(ctx, 200, aggTypes, []string{"empty-scope"}); got != 200 {
		t.Errorf("no-aggregate-block scope widened to %d, want base 200 (eval byte-identical)", got)
	}
	if got := h.aggregateOverFetchLimit(ctx, 200, nil, []string{"empty-scope"}); got != 200 {
		t.Errorf("no aggTypes widened to %d, want base 200 (no probe)", got)
	}

	seedScaleCorpus(t, pool)
	if got := h.aggregateOverFetchLimit(ctx, 200, aggTypes, []string{scaleHome}); got != 400 {
		t.Errorf("populated scope widened to %d, want 400", got)
	}
	// Scope-scoped: comments exist in scaleHome but NOT in scaleForeign ⇒ base.
	if got := h.aggregateOverFetchLimit(ctx, 200, aggTypes, []string{scaleForeign}); got != 200 {
		t.Errorf("foreign empty scope widened to %d, want base 200 (scope-scoped probe)", got)
	}
}
