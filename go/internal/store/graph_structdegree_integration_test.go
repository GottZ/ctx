//go:build integration

// Graph-structural wave GB4 gates (design/01 §4.7, §7-W5-G1): the visible
// degree (Q3) counts structural link ROWS in BOTH directions — K5 semantics
// (rows per direction per class, the UNION-ALL continuation; NOT distinct
// neighbors — the badge math breaks on collision pairs otherwise) — behind
// the visibility predicate at the far endpoint (a raw count would be an
// existence oracle, the fillDegrees doc comment's leak class).
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestFillDegrees_StructuralBothDirections is W5-G1. RED on the GB3 state:
// degree counts only dream rows → 0 for both probe blocks.
func TestFillDegrees_StructuralBothDirections(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		outOnly   = "019f6017-0051-7000-9000-000000000001" // exactly ONE structural OUT edge
		inOnly    = "019f6017-0051-7000-9000-000000000002" // exactly ONE structural IN edge
		collision = "019f6017-0051-7000-9000-000000000003" // dream + structural on the same pair → 2 rows
		invisTgt  = "019f6017-0051-7000-9000-000000000004" // invisible far endpoint (leak arm fwd)
		invisSrc  = "019f6017-0051-7000-9000-000000000005" // invisible far endpoint (leak arm rev)
	)
	mk := func(id, scope string) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope)
			 VALUES ($1::uuid,'learnings',$1,'w5 fixture',$2)`, id, scope); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mk(outOnly, "private")
	mk(inOnly, "private")
	mk(collision, "private")
	mk(invisTgt, "w5foreign")
	mk(invisSrc, "w5foreign")

	link := func(src, dst, table string) {
		var q string
		if table == "dream" {
			q = `INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			     VALUES ($1::uuid,$2::uuid,'topical',0.9,0.9,'private')`
		} else {
			q = `INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
			     VALUES ($1::uuid,$2::uuid,'references','private','system')`
		}
		if _, err := pool.Exec(ctx, q, src, dst); err != nil {
			t.Fatalf("link %s->%s (%s): %v", src, dst, table, err)
		}
	}
	link(outOnly, inOnly, "struct")   // outOnly: 1 OUT; inOnly: 1 IN
	link(collision, inOnly, "dream")  // collision pair: dream row ...
	link(collision, inOnly, "struct") // ... + structural row ...
	// Parallel-class arm (review LOW): references AND duplicate-of on the
	// SAME pair = 2 rows in the SAME structural leg — an accidental
	// DISTINCT nb_id in the raw subquery would collapse them.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'duplicate-of','private','system')`, collision, inOnly); err != nil {
		t.Fatalf("parallel-class link: %v", err)
	}
	link(outOnly, invisTgt, "struct") // leak arm fwd: invisible target must NOT count
	link(invisSrc, outOnly, "struct") // leak arm rev: invisible source must NOT count

	deg := func(focus string) int {
		res, err := store.EgoGraph(ctx, pool,
			store.EgoParams{Focus: focus, Hops: 1, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000},
			[]string{"private"}, nil, []string{"knowledge"})
		if err != nil {
			t.Fatalf("EgoGraph(%s): %v", focus, err)
		}
		for _, n := range res.Nodes {
			if n.ID == focus {
				return n.Degree
			}
		}
		t.Fatalf("focus %s missing", focus)
		return -1
	}

	// W5-G1 core + leak arms: outOnly carries ONE visible OUT row (to inOnly)
	// plus one invisible OUT (foreign target) and one invisible IN (foreign
	// source) — the invisible incidences must not count in either direction.
	if got := deg(outOnly); got != 1 {
		t.Errorf("outOnly degree = %d, want 1 (one visible OUT row; invisible incidences must not count — leak arms)", got)
	}
	// inOnly: struct IN from outOnly + dream IN + references IN + duplicate-of
	// IN from collision = 4 rows.
	if got := deg(inOnly); got != 4 {
		t.Errorf("inOnly degree = %d, want 4 (K5: link ROWS of both classes)", got)
	}
	// collision: dream + references + duplicate-of on the SAME pair = 3 rows
	// (parallel classes count per row — a DISTINCT-neighbor collapse in either
	// leg breaks the badge math).
	if got := deg(collision); got != 3 {
		t.Errorf("collision degree = %d, want 3 (K5 row semantics incl. parallel classes)", got)
	}
}
