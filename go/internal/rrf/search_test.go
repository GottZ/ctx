//go:build integration

package rrf_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertExcludeTestBlock writes a context_blocks row with the supplied category
// and type_name. Each block uses the same shared embedding so the semantic
// channel ranks them all identically modulo per-block ROW_NUMBER ordering.
// Title varies per id so the uq_context_category_title_scope unique constraint
// is satisfied.
// testVisibleTypes is the M072-seed visibility allowlist (VisibleTypes of
// the builtin set: everything except the excluded system-meta). The T5
// signature makes the allowlist an explicit parameter — these tests exercise
// category/type excludes and the grant arm, so they pass the seed-equivalent
// list. Allowlist-specific gates live in search_t5_policy_integration_test.go.
var testVisibleTypes = []string{"audit-trail", "knowledge", "reference"}

func insertExcludeTestBlock(t *testing.T, pool *pgxpool.Pool, id, category, typeName string, embedding []float32, ts time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec := pgvec.NewVector(embedding)
	title := fmt.Sprintf("rrf-exclude-test-title-%s", id[len(id)-1:])
	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, type_name)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, $8)`,
		id, category, title, "rrf-exclude-test-content",
		"private", vec, ts, typeName,
	)
	if err != nil {
		t.Fatalf("insert exclude-test block %s: %v", id, err)
	}
}

// TestSearch_CategoriesExclude verifies that passing categoriesExclude to
// rrf.Search hides blocks whose category appears in the exclude list, while
// non-excluded blocks pass through. Trigger: CRAG-Bench S38c topic-map-private
// slot-stealing finding (category='index' would be excluded by
// CategoriesExclude=["index"]).
//
// Setup:
//
//	A: category='knowledge', type_name='knowledge'    → kept
//	B: category='index',     type_name='knowledge'    → excluded when ['index']
//	C: category='reference', type_name='knowledge'    → kept
//
// Contract:
//
//	default (no exclude):              A, B, C all in result.
//	categoriesExclude=['index']:       A, C in result; B excluded.
//	categoriesExclude=['index','ref']: A in result; B, C excluded.
func TestSearch_CategoriesExclude(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}

	const (
		idA = "019d0048-0000-7000-9000-00000000000a"
		idB = "019d0048-0000-7000-9000-00000000000b"
		idC = "019d0048-0000-7000-9000-00000000000c"
	)

	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	insertExcludeTestBlock(t, pool, idA, "knowledge", "knowledge", embedding, now)
	insertExcludeTestBlock(t, pool, idB, "index", "knowledge", embedding, now)
	insertExcludeTestBlock(t, pool, idC, "reference", "knowledge", embedding, now)

	// Baseline: no exclude. All three blocks should appear.
	results, _, err := rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search baseline failed: %v", err)
	}
	gotIDs := idSet(results)
	for _, want := range []string{idA, idB, idC} {
		if !gotIDs[want] {
			t.Fatalf("baseline missing block %s; got=%v", want, gotIDs)
		}
	}

	// Exclude category='index' (the CRAG-Bench Slot-Stealer scenario).
	results, _, err = rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, []string{"index"}, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search exclude=['index'] failed: %v", err)
	}
	gotIDs = idSet(results)
	if gotIDs[idB] {
		t.Errorf("idB (category=index) leaked through exclude=['index']: got=%v", gotIDs)
	}
	for _, want := range []string{idA, idC} {
		if !gotIDs[want] {
			t.Errorf("exclude=['index'] removed non-target block %s; got=%v", want, gotIDs)
		}
	}

	// Multi-value exclude: ['index','reference'].
	results, _, err = rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, []string{"index", "reference"}, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search exclude=['index','reference'] failed: %v", err)
	}
	gotIDs = idSet(results)
	if gotIDs[idB] || gotIDs[idC] {
		t.Errorf("multi-exclude leaked: got=%v", gotIDs)
	}
	if !gotIDs[idA] {
		t.Errorf("multi-exclude removed wrong block; idA missing; got=%v", gotIDs)
	}
}

// TestSearch_BlockRolesExclude verifies that passing blockRolesExclude to
// rrf.Search hides blocks whose type_name appears in the exclude list.
//
// Setup (type_name CHECK constraint from M035 allows
// 'system-meta','audit-trail','reference','knowledge' — 'system-meta' is
// always hard-excluded by ctx_rrf, so we don't use it):
//
//	A: type_name='knowledge'    → kept
//	B: type_name='audit-trail'  → excluded when ['audit-trail']
//	C: type_name='reference'    → kept (unless excluded)
func TestSearch_BlockRolesExclude(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}

	const (
		idA = "019d0049-0000-7000-9000-00000000000a"
		idB = "019d0049-0000-7000-9000-00000000000b"
		idC = "019d0049-0000-7000-9000-00000000000c"
	)

	now := time.Date(2026, 5, 27, 13, 0, 0, 0, time.UTC)
	insertExcludeTestBlock(t, pool, idA, "test_role_x", "knowledge", embedding, now)
	insertExcludeTestBlock(t, pool, idB, "test_role_y", "audit-trail", embedding, now)
	insertExcludeTestBlock(t, pool, idC, "test_role_z", "reference", embedding, now)

	// Baseline: no exclude.
	results, _, err := rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search baseline failed: %v", err)
	}
	gotIDs := idSet(results)
	for _, want := range []string{idA, idB, idC} {
		if !gotIDs[want] {
			t.Fatalf("baseline missing block %s; got=%v", want, gotIDs)
		}
	}

	// Exclude type_name='audit-trail'.
	results, _, err = rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, []string{"audit-trail"}, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search exclude-role=['audit-trail'] failed: %v", err)
	}
	gotIDs = idSet(results)
	if gotIDs[idB] {
		t.Errorf("idB (type_name=audit-trail) leaked through exclude: got=%v", gotIDs)
	}
	for _, want := range []string{idA, idC} {
		if !gotIDs[want] {
			t.Errorf("exclude-role removed non-target block %s; got=%v", want, gotIDs)
		}
	}

	// Multi-role exclude: ['audit-trail','reference'] — only knowledge remains.
	results, _, err = rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, []string{"audit-trail", "reference"}, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search multi-role-exclude failed: %v", err)
	}
	gotIDs = idSet(results)
	if gotIDs[idB] || gotIDs[idC] {
		t.Errorf("multi-role-exclude leaked: got=%v", gotIDs)
	}
	if !gotIDs[idA] {
		t.Errorf("multi-role-exclude removed wrong block; idA missing; got=%v", gotIDs)
	}
}

// TestSearch_ExcludeCombined verifies that categoriesExclude and
// blockRolesExclude compose with AND-semantics — a block is excluded if EITHER
// list rejects it (union of exclusions, not intersection).
func TestSearch_ExcludeCombined(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}

	const (
		idA = "019d004a-0000-7000-9000-00000000000a" // category=index, role=knowledge — excluded by category
		idB = "019d004a-0000-7000-9000-00000000000b" // category=test, role=audit-trail — excluded by role
		idC = "019d004a-0000-7000-9000-00000000000c" // category=test, role=knowledge — passes both
	)

	now := time.Date(2026, 5, 27, 14, 0, 0, 0, time.UTC)
	insertExcludeTestBlock(t, pool, idA, "index", "knowledge", embedding, now)
	insertExcludeTestBlock(t, pool, idB, "test_combo", "audit-trail", embedding, now)
	insertExcludeTestBlock(t, pool, idC, "test_combo_ok", "knowledge", embedding, now)

	results, _, err := rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil,
		[]string{"index"}, []string{"audit-trail"}, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search combined exclude failed: %v", err)
	}
	gotIDs := idSet(results)
	if gotIDs[idA] {
		t.Errorf("combined exclude: idA (category=index) leaked: got=%v", gotIDs)
	}
	if gotIDs[idB] {
		t.Errorf("combined exclude: idB (role=audit-trail) leaked: got=%v", gotIDs)
	}
	if !gotIDs[idC] {
		t.Errorf("combined exclude: idC (passes both lists) was removed: got=%v", gotIDs)
	}
}

// TestSearch_ExcludeEmptyIsNoOp verifies that empty exclude slices behave
// identically to nil — neither should filter out any rows.
func TestSearch_ExcludeEmptyIsNoOp(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.1
	}

	const idA = "019d004b-0000-7000-9000-00000000000a"

	insertExcludeTestBlock(t, pool, idA, "knowledge", "knowledge",
		embedding, time.Date(2026, 5, 27, 15, 0, 0, 0, time.UTC))

	// nil + nil
	res1, _, err := rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("nil+nil: %v", err)
	}

	// empty + empty
	res2, _, err := rrf.Search(ctx, pool, embedding, "zzqqxx", "zzqqxx",
		[]string{"private"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, []string{}, []string{}, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("empty+empty: %v", err)
	}

	if len(res1) != len(res2) {
		t.Errorf("nil-vs-empty exclude differ in result count: nil=%d, empty=%d", len(res1), len(res2))
	}
	if !idSet(res1)[idA] || !idSet(res2)[idA] {
		t.Errorf("block missing in baseline result; nil-set=%v empty-set=%v", idSet(res1), idSet(res2))
	}
}

func idSet(results []rrf.SearchResult) map[string]bool {
	m := make(map[string]bool, len(results))
	for _, r := range results {
		m[r.ID] = true
	}
	return m
}
