//go:build integration

// Graph-structural wave GB2 gates (design/01 §4.4/§4.5, §7-W3): Q2s
// (inducedStructEdges) + the edge-budget arbitration with dream floor.
// StructEdges appear ONLY between nodes that entered the graph via dream
// traversal (Q1s stays unwired until GB3) — nodes/degrees byte-stable.
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	w3Report  = "019f6017-0031-7000-9000-0000000000aa" // focus: a Tagesbericht
	w3Source  = "019f6017-0031-7000-9000-0000000000ab" // its source block
	w3Outside = "019f6017-0031-7000-9000-0000000000ac" // structurally linked but NOT in the node set (no dream path)
)

func w3Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for id, title := range map[string]string{w3Report: "W3 report", w3Source: "W3 source"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope)
			 VALUES ($1::uuid,'learnings',$2,'w3 fixture','private')`, id, title); err != nil {
			t.Fatalf("insert block %s: %v", id, err)
		}
	}
	// Collision pair: dream edge AND structural facts on the same directed pair.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid,$2::uuid,'topical',0.9,0.9,'private')`, w3Report, w3Source); err != nil {
		t.Fatalf("insert dream link: %v", err)
	}
	// Two classes on one pair (parallel-class arm) with DISTINCT origins —
	// the second origin (manual) discriminates the per-tuple origin index
	// against a hardcoded Origin:0 (GB2 review: single-origin fixtures cannot
	// tell the mapping from a constant).
	for cls, origin := range map[string]string{"references": "system", "duplicate-of": "manual"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
			 VALUES ($1::uuid,$2::uuid,$3,'private',$4)`, w3Report, w3Source, cls, origin); err != nil {
			t.Fatalf("insert structural link %s: %v", cls, err)
		}
	}
	// Out-of-set arm: w3Outside is structurally referenced by the report but
	// has NO dream path — it never enters the node set (Q1s stays unwired
	// until GB3), so Q2s must never deliver this edge. This row is also the
	// substrate of the W3-G1 red probe (b): removing one side's $1 node-set
	// predicate ALONE already goes red — the legend is built before the
	// defensive index skip, so the leaked row's origin (forge-sync) appears in
	// Origins and trips the legend assert; neutralizing the skip additionally
	// makes the tuple itself appear (3 instead of 2). The pre-skip legend
	// construction is thus deliberately the probe's tripwire — do not "fix" it
	// without re-proving the probe (GB2 review interlock note).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid,'learnings','W3 outside','w3 fixture','private')`, w3Outside); err != nil {
		t.Fatalf("insert outside block: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'references','private','forge-sync')`, w3Report, w3Outside); err != nil {
		t.Fatalf("insert outside structural link: %v", err)
	}
}

func w3Ego(t *testing.T, pool *pgxpool.Pool, p store.EgoParams) *store.EgoResult {
	t.Helper()
	p.Focus = w3Report
	if p.Hops == 0 {
		p.Hops = 1
	}
	if p.PerNodeCap == 0 {
		p.PerNodeCap = 25
	}
	if p.Limit == 0 {
		p.Limit = 500
	}
	if p.EdgeLimit == 0 {
		p.EdgeLimit = 4000
	}
	res, err := store.EgoGraph(context.Background(), pool, p, []string{"private"}, nil, []string{"knowledge"})
	if err != nil {
		t.Fatalf("EgoGraph: %v", err)
	}
	return res
}

// TestEgoGraph_StructEdgesInduced is W3-G1: a report and its source, connected
// via dream AND per structural reference — Edges carries the dream edge,
// StructEdges the tuples with correct class/origin indexes; the parallel-class
// arm (references + duplicate-of on the same pair) yields 2 tuples. Red
// proofs on the wave itself (documented in the commit body): (a) swapping the
// class/origin index mapping in a code double goes red; (b) removing ONE
// side's node-set predicate from Q2s lets an out-of-set endpoint edge appear.
func TestEgoGraph_StructEdgesInduced(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w3Seed(t, pool)

	res := w3Ego(t, pool, store.EgoParams{})

	if len(res.Edges) != 1 {
		t.Fatalf("dream Edges = %d, want 1 (byte-stable dream side)", len(res.Edges))
	}
	if len(res.StructEdges) != 2 {
		t.Fatalf("StructEdges = %d, want 2 (references + duplicate-of, parallel classes on one pair)", len(res.StructEdges))
	}
	// Legends are built AFTER truncation from the DELIVERED tuples (E3
	// dynamic): sorted distinct.
	wantRels := []string{"duplicate-of", "references"}
	if len(res.StructRels) != 2 || res.StructRels[0] != wantRels[0] || res.StructRels[1] != wantRels[1] {
		t.Fatalf("StructRels = %v, want %v (sorted distinct delivered classes)", res.StructRels, wantRels)
	}
	// Sorted distinct DELIVERED origins — the outside edge's forge-sync must
	// NOT appear (it never ships), the two in-set origins must.
	wantOrigins := []string{"manual", "system"}
	if len(res.Origins) != 2 || res.Origins[0] != wantOrigins[0] || res.Origins[1] != wantOrigins[1] {
		t.Fatalf("Origins = %v, want %v (sorted distinct delivered origins; forge-sync stays out)", res.Origins, wantOrigins)
	}
	// Tuple indexes resolve against nodes + legends.
	nodeIdx := map[string]int{}
	for i, n := range res.Nodes {
		nodeIdx[n.ID] = i
	}
	// Per-tuple class→origin mapping (references came from system,
	// duplicate-of from manual) — discriminates a hardcoded Origin index.
	wantOriginByClass := map[string]string{"references": "system", "duplicate-of": "manual"}
	seen := map[string]bool{}
	for _, e := range res.StructEdges {
		if e.Src != nodeIdx[w3Report] || e.Dst != nodeIdx[w3Source] {
			t.Errorf("tuple endpoints [%d,%d] do not match report->source [%d,%d]", e.Src, e.Dst, nodeIdx[w3Report], nodeIdx[w3Source])
		}
		if e.Class < 0 || e.Class >= len(res.StructRels) {
			t.Fatalf("class index %d out of legend range %v", e.Class, res.StructRels)
		}
		if e.Origin < 0 || e.Origin >= len(res.Origins) {
			t.Fatalf("origin index %d out of legend range %v", e.Origin, res.Origins)
		}
		cls := res.StructRels[e.Class]
		seen[cls] = true
		if got := res.Origins[e.Origin]; got != wantOriginByClass[cls] {
			t.Errorf("class %s resolves origin %q, want %q (per-tuple mapping)", cls, got, wantOriginByClass[cls])
		}
	}
	if !seen["references"] || !seen["duplicate-of"] {
		t.Errorf("delivered classes = %v, want both references and duplicate-of", seen)
	}
}

// TestEgoGraph_StructEdges_TruncationLegend is the edge_limit=1 probe: the
// legends must describe only the DELIVERED tuples, never the pre-truncation
// set. (At limit 1 the integer floor is 0 — fact precedence carries the
// single slot; the floor protects dream only from EdgeLimit >= 2.)
func TestEgoGraph_StructEdges_TruncationLegend(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w3Seed(t, pool)

	res := w3Ego(t, pool, store.EgoParams{EdgeLimit: 1})

	// Budget 1 with |D|=1,|S|=2: dreamFloor=min(1,0)=0 → structural gets
	// min(2, 1)=1 slot, dream 0. (EdgeLimit/2 = 0 at limit 1 — integer floor;
	// fact precedence carries the single slot.)
	if !res.Truncated {
		t.Error("Truncated = false, want true (edge budget overflow)")
	}
	total := len(res.Edges) + len(res.StructEdges)
	if total != 1 {
		t.Fatalf("delivered edges = %d (dream %d + struct %d), want exactly the budget 1", total, len(res.Edges), len(res.StructEdges))
	}
	if len(res.StructEdges) == 1 {
		if len(res.StructRels) != 1 {
			t.Errorf("StructRels = %v, want exactly the ONE delivered class (legend after truncation)", res.StructRels)
		}
	}
}

// TestEgoGraph_StructClassesNone_SkipsQ2s: the §4.6 filter contract on the
// wired path — StructClasses non-nil-EMPTY ("none selected") delivers NO
// structural edges and empty legends, dream side untouched.
func TestEgoGraph_StructClassesNone_SkipsQ2s(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w3Seed(t, pool)

	res := w3Ego(t, pool, store.EgoParams{StructClasses: []string{}, LinkClasses: nil})
	// normalizeClassFilters: StructClasses set → LinkClasses becomes [] too;
	// "no classes at all" — both sides empty. Use explicit both-sided params
	// for the discriminating case instead:
	if len(res.StructEdges) != 0 {
		t.Errorf("StructClasses=[] delivered %d structural edges, want 0", len(res.StructEdges))
	}

	res2 := w3Ego(t, pool, store.EgoParams{StructClasses: []string{}, LinkClasses: []string{"topical"}})
	if len(res2.StructEdges) != 0 || len(res2.StructRels) != 0 {
		t.Errorf("struct none + dream topical: StructEdges=%d StructRels=%v, want empty", len(res2.StructEdges), res2.StructRels)
	}
	if len(res2.Edges) != 1 {
		t.Errorf("dream side broken by struct skip: Edges=%d, want 1", len(res2.Edges))
	}
}
