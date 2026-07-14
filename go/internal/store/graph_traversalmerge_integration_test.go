//go:build integration

// Graph-structural wave GB3 gates (design/01 §4.3, §7-W4): the traversal
// merge — THE wave that fixes the user symptom ("im graph sind die ganzen
// tagesberichte ohne edges abgelegt"). Q1s joins the hop loop, takeHopMerged
// zips both candidate classes fairly (E2), short circuits are symmetric.
package store_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const w4Report = "019f6017-0041-7000-9000-000000000000"

func w4Block(t *testing.T, pool *pgxpool.Pool, id, title, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid,'learnings',$2,'w4 fixture',$3)`, id, title, scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func w4StructLink(t *testing.T, pool *pgxpool.Pool, src, dst string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'references','private','system')`, src, dst); err != nil {
		t.Fatalf("insert structural link: %v", err)
	}
}

func w4Ego(t *testing.T, pool *pgxpool.Pool, p store.EgoParams) *store.EgoResult {
	t.Helper()
	if p.Focus == "" {
		p.Focus = w4Report
	}
	if p.Hops == 0 {
		p.Hops = 2
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

// TestEgoGraph_ReportSourcesReachable is W4-G1 — the core acceptance test and
// the direct negative probe of the user symptom: a report with 3 structural
// references and ZERO dream edges (the 35 edge-less live reports as the
// pattern). Focus = report ⇒ the 3 sources appear at hop 1 and the 3 fact
// edges ship. RED on the GB2 state by nature (traversal doesn't know
// structural yet — nodes = {focus}, StructEdges empty).
func TestEgoGraph_ReportSourcesReachable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w4Block(t, pool, w4Report, "w4 report", "private")
	sources := []string{
		"019f6017-0041-7000-9000-000000000001",
		"019f6017-0041-7000-9000-000000000002",
		"019f6017-0041-7000-9000-000000000003",
	}
	for i, s := range sources {
		w4Block(t, pool, s, fmt.Sprintf("w4 source %d", i), "private")
		w4StructLink(t, pool, w4Report, s)
	}

	res := w4Ego(t, pool, store.EgoParams{})

	if len(res.Nodes) != 4 {
		t.Fatalf("Nodes = %d, want 4 (report + 3 structurally-reached sources) — the user symptom", len(res.Nodes))
	}
	for _, n := range res.Nodes[1:] {
		if n.Hop != 1 {
			t.Errorf("source %s Hop = %d, want 1", n.ID, n.Hop)
		}
	}
	if len(res.StructEdges) != 3 {
		t.Errorf("StructEdges = %d, want 3", len(res.StructEdges))
	}
	if len(res.Edges) != 0 {
		t.Errorf("dream Edges = %d, want 0 (report has none)", len(res.Edges))
	}
}

// TestEgoGraph_StructSiblingEdges (GB2 review carry): a structural edge
// between two hop-1 SIBLINGS (both reached via the focus) ships in
// StructEdges — the induced set covers the whole node set, not only
// focus-adjacent pairs.
func TestEgoGraph_StructSiblingEdges(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w4Block(t, pool, w4Report, "w4 report", "private")
	a := "019f6017-0041-7000-9000-00000000000a"
	b := "019f6017-0041-7000-9000-00000000000b"
	w4Block(t, pool, a, "w4 sibling a", "private")
	w4Block(t, pool, b, "w4 sibling b", "private")
	w4StructLink(t, pool, w4Report, a)
	w4StructLink(t, pool, w4Report, b)
	w4StructLink(t, pool, a, b) // the sibling edge

	res := w4Ego(t, pool, store.EgoParams{})
	if len(res.StructEdges) != 3 {
		t.Fatalf("StructEdges = %d, want 3 (2 focus + 1 sibling edge)", len(res.StructEdges))
	}
}

// TestEgoGraph_GrantOnlyLeaf_Structural is W4-G3: a grant-only node reached
// via a STRUCTURAL edge appears as a leaf — its own in-scope neighbors do
// NOT (the frontier exclusion is class-agnostic). Red probe: commenting the
// isGrantOnlyVisible filter out of the frontier build delivers the grantee's
// neighbor over the grant bridge.
func TestEgoGraph_GrantOnlyLeaf_Structural(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w4Block(t, pool, w4Report, "w4 report", "private")
	granted := "019f6017-0041-7000-9000-0000000000c1" // foreign scope, granted
	behind := "019f6017-0041-7000-9000-0000000000c2"  // caller-READABLE, but only reachable THROUGH the granted block
	w4Block(t, pool, granted, "w4 granted", "w4foreign")
	// behind lives in the caller's OWN readable scope (the T41 break path:
	// the leak is the delivery of in-scope blocks over the foreign grant
	// bridge — revealing the foreign block's private link chain). A foreign
	// `behind` would be visibility-blocked anyway and prove nothing.
	w4Block(t, pool, behind, "w4 behind grant", "private")
	// Cross-scope structural row planted via raw SQL (write path would refuse;
	// read paths gate on context_blocks.scope + grants — exactly what we probe).
	w4StructLink(t, pool, w4Report, granted)
	w4StructLink(t, pool, granted, behind)

	res, err := store.EgoGraph(context.Background(), pool,
		store.EgoParams{Focus: w4Report, Hops: 3, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000},
		[]string{"private"}, []string{granted}, []string{"knowledge"})
	if err != nil {
		t.Fatalf("EgoGraph: %v", err)
	}
	got := map[string]bool{}
	for _, n := range res.Nodes {
		got[n.ID] = true
	}
	if !got[granted] {
		t.Error("granted node missing — the grant OR-arm should make it a visible leaf")
	}
	if got[behind] {
		t.Error("grantee's own neighbor delivered over the grant bridge — frontier exclusion broken for the structural class")
	}
}

// TestEgoGraph_InvisibleFocus_Structural is the A5 amendment gate: an
// invisible focus (foreign scope, NO grant) answers ErrNotVisible even when
// a structural edge connects it to a visible block — no node list, no edges,
// no existence oracle.
func TestEgoGraph_InvisibleFocus_Structural(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	foreign := "019f6017-0041-7000-9000-0000000000d1"
	visible := "019f6017-0041-7000-9000-0000000000d2"
	w4Block(t, pool, foreign, "w4 foreign focus", "w4foreign")
	w4Block(t, pool, visible, "w4 visible", "private")
	w4StructLink(t, pool, foreign, visible)

	// A5 exact arm: the caller even holds a grant on the NEIGHBOR — the focus
	// stays invisible (hydrateFocus ORs only the focus id against the grant
	// set, never a neighbor's), still an indistinguishable 404.
	_, err := store.EgoGraph(context.Background(), pool,
		store.EgoParams{Focus: foreign, Hops: 2, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000},
		[]string{"private"}, []string{visible}, []string{"knowledge"})
	if err == nil {
		t.Fatal("invisible focus with a structural edge to a granted visible block answered a graph, want ErrNotVisible")
	}
}

// TestEgoGraph_ClassCombinatorics is W4-G4's matrix arm: nil/nil, dream-only,
// structural-only, mixed, both-none — over the WIRED traversal path.
func TestEgoGraph_ClassCombinatorics(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	w4Block(t, pool, w4Report, "w4 report", "private")
	dreamN := "019f6017-0041-7000-9000-0000000000e1"
	structN := "019f6017-0041-7000-9000-0000000000e2"
	w4Block(t, pool, dreamN, "w4 dream neighbor", "private")
	w4Block(t, pool, structN, "w4 struct neighbor", "private")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid,$2::uuid,'topical',0.9,0.9,'private')`, w4Report, dreamN); err != nil {
		t.Fatalf("insert dream link: %v", err)
	}
	w4StructLink(t, pool, w4Report, structN)

	cases := []struct {
		name           string
		link, structIn []string
		wantNodes      int
		wantDream      bool
		wantStruct     bool
	}{
		{"nil/nil — everything", nil, nil, 3, true, true},
		{"dream-only filter", []string{"topical"}, []string{}, 2, true, false},
		{"structural-only filter", []string{}, []string{"references"}, 2, false, true},
		{"mixed subset", []string{"topical"}, []string{"references"}, 3, true, true},
		{"both none", []string{}, []string{}, 1, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := w4Ego(t, pool, store.EgoParams{LinkClasses: tc.link, StructClasses: tc.structIn})
			if len(res.Nodes) != tc.wantNodes {
				t.Errorf("Nodes = %d, want %d", len(res.Nodes), tc.wantNodes)
			}
			if (len(res.Edges) > 0) != tc.wantDream {
				t.Errorf("dream edges present = %v, want %v", len(res.Edges) > 0, tc.wantDream)
			}
			if (len(res.StructEdges) > 0) != tc.wantStruct {
				t.Errorf("struct edges present = %v, want %v", len(res.StructEdges) > 0, tc.wantStruct)
			}
		})
	}
}

// countingTracer counts link-table roundtrips (W4-G4 + GB5 query-count arms —
// proves the SYMMETRIC short circuits: a skipped class costs zero roundtrips
// across hop (Q1/Q1s) AND induced (Q2/Q2s), not just zero rows). Q3
// (fillDegrees) reads both tables in ONE query by design and is excluded via
// its unique "AS n(id)" unnest alias.
type countingTracer struct {
	dreamHops, structHops       atomic.Int64
	dreamQueries, structQueries atomic.Int64
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "AS n(id)") {
		return ctx // Q3 — both tables by design, not a class roundtrip
	}
	isHop := strings.Contains(data.SQL, "WITH hop AS")
	if strings.Contains(data.SQL, "context_dream_links") {
		c.dreamQueries.Add(1)
		if isHop {
			c.dreamHops.Add(1)
		}
	}
	if strings.Contains(data.SQL, "context_structural_links") {
		c.structQueries.Add(1)
		if isHop {
			c.structHops.Add(1)
		}
	}
	return ctx
}
func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestEgoGraph_ShortCircuitRoundtrips is W4-G4's query-count arm. Red probe:
// removing either short-circuit arm in a test double makes its counter > 0.
func TestEgoGraph_ShortCircuitRoundtrips(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	base := testdb.SetupTestDB(t)
	w4Block(t, base, w4Report, "w4 report", "private")

	tracer := &countingTracer{}
	cfg, err := pgxpool.ParseConfig(base.Config().ConnString())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)

	run := func(link, structIn []string) (int64, int64) {
		tracer.dreamHops.Store(0)
		tracer.structHops.Store(0)
		tracer.dreamQueries.Store(0)
		tracer.structQueries.Store(0)
		if _, err := store.EgoGraph(context.Background(), pool,
			store.EgoParams{Focus: w4Report, Hops: 2, PerNodeCap: 25, Limit: 500, EdgeLimit: 4000,
				LinkClasses: link, StructClasses: structIn},
			[]string{"private"}, nil, []string{"knowledge"}); err != nil {
			t.Fatalf("EgoGraph: %v", err)
		}
		return tracer.dreamQueries.Load(), tracer.structQueries.Load()
	}

	// GB5: the counters cover Q1+Q2 (dream) and Q1s+Q2s (structural) — a
	// skipped class costs ZERO roundtrips of any kind (hop AND induced).
	if d, s := run([]string{}, []string{"references"}); d != 0 {
		t.Errorf("structural-only: %d dream roundtrips (hop+induced), want 0 (short circuit dead)", d)
	} else if s == 0 {
		t.Error("structural-only: 0 structural roundtrips — control broken, tracer sees nothing")
	}
	if d, s := run([]string{"topical"}, []string{}); s != 0 {
		t.Errorf("dream-only: %d structural roundtrips (hop+induced), want 0 (short circuit dead)", s)
	} else if d == 0 {
		t.Error("dream-only: 0 dream roundtrips — control broken")
	}
}
