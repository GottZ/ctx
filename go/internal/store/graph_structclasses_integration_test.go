//go:build integration

// Graph-structural wave GA6/W1 gates (design/01 §4.6, §7-W1): the class-filter
// parameter foundation. W1-G1 pins the one REAL legacy bug path: the Q2
// (inducedEdges) class bind used nilIfEmpty, collapsing a non-nil-EMPTY
// LinkClasses filter ("no classes selected") to SQL NULL — which means "all
// five". Unreachable today (egoCSV never yields non-nil-empty), an active
// break once the handler's vocabulary split (dream vs. structural classes)
// lands: `link_class=references` would then mean dream non-nil-empty +
// structural {references} — and the dream side must match NOTHING.
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	w1BlockA = "019f6017-0001-7000-9000-0000000000aa"
	w1BlockB = "019f6017-0001-7000-9000-0000000000ab"
)

func w1Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for id, title := range map[string]string{w1BlockA: "W1 A", w1BlockB: "W1 B"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope)
			 VALUES ($1::uuid,'learnings',$2,'w1 fixture','private')`, id, title); err != nil {
			t.Fatalf("insert block %s: %v", id, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid,$2::uuid,'topical',0.9,0.9,'private')`, w1BlockA, w1BlockB); err != nil {
		t.Fatalf("insert dream link: %v", err)
	}
}

// TestInducedEdges_EmptyClassFilter_W1G1: a non-nil-EMPTY LinkClasses filter
// must match NOTHING on the display set (Q2). RED against the nilIfEmpty
// binding (empty collapses to NULL = all five → the topical edge leaks
// through the "nothing selected" filter); green with displayClasses.
func TestInducedEdges_EmptyClassFilter_W1G1(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w1Seed(t, pool)
	ctx := context.Background()

	ids := []string{w1BlockA, w1BlockB}
	index := map[string]int{w1BlockA: 0, w1BlockB: 1}

	// Control first (positive control against a vacuously green assert):
	// nil = "no filter" must deliver the topical edge.
	ctrl, _, err := store.InducedEdgesForTest(ctx, pool, ids, index,
		store.EgoParams{LinkClasses: nil, EdgeLimit: 100})
	if err != nil {
		t.Fatalf("inducedEdges (nil filter): %v", err)
	}
	if len(ctrl) != 1 {
		t.Fatalf("control broken: nil filter returned %d edges, want 1 (fixture edge)", len(ctrl))
	}

	// W1-G1: non-nil-EMPTY = "no classes selected" — must match NOTHING.
	edges, _, err := store.InducedEdgesForTest(ctx, pool, ids, index,
		store.EgoParams{LinkClasses: []string{}, EdgeLimit: 100})
	if err != nil {
		t.Fatalf("inducedEdges (empty filter): %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("W1-G1 FAILED: non-nil-empty LinkClasses returned %d dream edge(s), want 0 — the class bind collapsed the empty filter to SQL NULL (= all five); `link_class=<structural>` would show ALL dream edges after the vocabulary split", len(edges))
	}
}
