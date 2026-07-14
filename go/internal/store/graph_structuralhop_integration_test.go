//go:build integration

// Graph-structural wave GB1 gates (design/01 §4.2, §7-W2): structuralHopNeighbors
// (Q1s) — the UNWIRED structural hop building block — and the M104 traversal
// indexes. Security doctrine: red proof PER LEG (a copy-paste error in exactly
// one leg must turn the gate provably red), counting-channel probes in both
// directions, fail-closed loud.
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	w2Scope        = "w2"
	w2ForeignScope = "w2foreign"
)

func w2Block(t *testing.T, pool *pgxpool.Pool, id, title, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid,'learnings',$2,'w2 fixture',$3)`, id, title, scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func w2Link(t *testing.T, pool *pgxpool.Pool, src, dst, createdAt string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		 VALUES ($1::uuid,$2::uuid,'references',$3,'system',$4::timestamptz)`,
		src, dst, w2Scope, createdAt); err != nil {
		t.Fatalf("insert structural link %s->%s: %v", src, dst, err)
	}
}

func w2Params(cap int) store.EgoParams {
	return store.EgoParams{Hops: 1, PerNodeCap: cap, Limit: 500, EdgeLimit: 4000}
}

// TestStructuralHop_VisibilityInLegs is W2-G1: four fixture arms over four
// separate seeds. Red proof PER LEG (documented in the wave commit body):
// replacing the visibility predicate with TRUE in ONLY the forward leg turns
// arms (i)+(iii) red while (ii)+(iv) stay green; only the reverse leg → the
// mirror image.
func TestStructuralHop_VisibilityInLegs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{w2Scope}
	types := []string{"knowledge"}

	const (
		s1 = "019f6017-0021-7000-9000-0000000000a1" // seed arm (i)
		x1 = "019f6017-0021-7000-9000-0000000000b1" // invisible fwd target (foreign scope)
		x2 = "019f6017-0021-7000-9000-0000000000b2" // invisible fwd target (type off allowlist)
		s2 = "019f6017-0021-7000-9000-0000000000a2" // seed arm (ii)
		y1 = "019f6017-0021-7000-9000-0000000000c1" // invisible foreign SOURCE referencing s2 (the Tagesbericht case)
		s3 = "019f6017-0021-7000-9000-0000000000a3" // seed arm (iii)
		v1 = "019f6017-0021-7000-9000-0000000000d1" // visible OLDER fwd neighbor
		u1 = "019f6017-0021-7000-9000-0000000000e1" // invisible NEWER fwd neighbor
		s4 = "019f6017-0021-7000-9000-0000000000a4" // seed arm (iv)
		v2 = "019f6017-0021-7000-9000-0000000000d2" // visible OLDER rev source
		u2 = "019f6017-0021-7000-9000-0000000000e2" // invisible NEWER rev source
	)
	w2Block(t, pool, s1, "seed1", w2Scope)
	w2Block(t, pool, x1, "fwd foreign", w2ForeignScope)
	w2Block(t, pool, x2, "fwd type-invisible", w2Scope)
	w2Block(t, pool, s2, "seed2", w2Scope)
	w2Block(t, pool, y1, "rev foreign source", w2ForeignScope)
	w2Block(t, pool, s3, "seed3", w2Scope)
	w2Block(t, pool, v1, "visible older fwd", w2Scope)
	w2Block(t, pool, u1, "invisible newer fwd", w2ForeignScope)
	w2Block(t, pool, s4, "seed4", w2Scope)
	w2Block(t, pool, v2, "visible older rev", w2Scope)
	w2Block(t, pool, u2, "invisible newer rev", w2ForeignScope)
	// x2 is scope-visible but its TYPE is off the allowlist — the live
	// system-meta constellation (§5.2-B2): both invisibility flavors per leg.
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET type_name = 'system-meta' WHERE id = $1::uuid`, x2); err != nil {
		t.Fatalf("retype x2: %v", err)
	}

	w2Link(t, pool, s1, x1, "2026-07-01T10:00:00Z") // (i) fwd: invisible scope
	w2Link(t, pool, s1, x2, "2026-07-01T11:00:00Z") // (i) fwd: invisible type
	w2Link(t, pool, y1, s2, "2026-07-01T10:00:00Z") // (ii) rev: invisible foreign source
	w2Link(t, pool, s3, v1, "2026-07-01T08:00:00Z") // (iii) visible, OLDER
	w2Link(t, pool, s3, u1, "2026-07-02T08:00:00Z") // (iii) invisible, NEWER
	w2Link(t, pool, v2, s4, "2026-07-01T08:00:00Z") // (iv) visible, OLDER
	w2Link(t, pool, u2, s4, "2026-07-02T08:00:00Z") // (iv) invisible, NEWER

	run := func(seed string, cap int) map[string]bool {
		t.Helper()
		cands, err := store.StructuralHopNeighborsForTest(ctx, pool, []string{seed}, scopes, nil, types, w2Params(cap))
		if err != nil {
			t.Fatalf("structuralHopNeighbors(%s): %v", seed, err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[store.HopCandidateID(c)] = true
			if conf := store.HopCandidateConf(c); conf != 1 {
				t.Errorf("candidate conf = %v, want constant 1 (facts, 076)", conf)
			}
		}
		return got
	}

	// (i) forward leg: neither the foreign-scope nor the type-invisible
	// target is ever delivered.
	if got := run(s1, 25); len(got) != 0 {
		t.Errorf("arm (i) forward-leg leak: invisible targets delivered: %v", got)
	}
	// (ii) reverse leg: the invisible foreign source never appears as an
	// in-edge neighbor (the Tagesbericht case).
	if got := run(s2, 25); len(got) != 0 {
		t.Errorf("arm (ii) reverse-leg leak: invisible foreign source delivered: %v", got)
	}
	// (iii) counting channel forward, cap=1: the NEWER invisible edge must
	// not eat the only slot — the visible older neighbor is delivered.
	if got := run(s3, 1); !got[v1] || len(got) != 1 {
		t.Errorf("arm (iii) forward counting channel: got %v, want exactly {%s} (invisible newer edge consumed the cap slot?)", got, v1)
	}
	// (iv) counting channel reverse, cap=1: mirror image.
	if got := run(s4, 1); !got[v2] || len(got) != 1 {
		t.Errorf("arm (iv) reverse counting channel: got %v, want exactly {%s}", got, v2)
	}

	// (v) E5 ordering pin (GB1 review F2): the returned slice is newest-first
	// by link created_at — GB3 consumes the ORDER positionally as the ranking
	// substitute (conf is a constant 1), so a silent DESC→ASC regression must
	// go red HERE, not first in the merge wave.
	const (
		s5 = "019f6017-0021-7000-9000-0000000000a5"
		o1 = "019f6017-0021-7000-9000-0000000000f1" // oldest link
		o2 = "019f6017-0021-7000-9000-0000000000f2" // middle link
		o3 = "019f6017-0021-7000-9000-0000000000f3" // newest link
	)
	w2Block(t, pool, s5, "seed5", w2Scope)
	w2Block(t, pool, o1, "ordered oldest", w2Scope)
	w2Block(t, pool, o2, "ordered middle", w2Scope)
	w2Block(t, pool, o3, "ordered newest", w2Scope)
	w2Link(t, pool, s5, o1, "2026-07-01T01:00:00Z")
	w2Link(t, pool, s5, o2, "2026-07-02T01:00:00Z")
	w2Link(t, pool, o3, s5, "2026-07-03T01:00:00Z") // reverse edge, newest of all
	cands, err := store.StructuralHopNeighborsForTest(ctx, pool, []string{s5}, scopes, nil, types, w2Params(25))
	if err != nil {
		t.Fatalf("structuralHopNeighbors(s5): %v", err)
	}
	var order []string
	for _, c := range cands {
		order = append(order, store.HopCandidateID(c))
	}
	want := []string{o3, o2, o1}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("arm (v) E5 ordering: got %v, want newest-first %v", order, want)
	}
}

// TestStructuralHop_FailClosedLoud is W2-G2: empty read scopes and an empty
// visible-types allowlist are LOUD Go errors, never a silent empty graph
// (which would mask a wiring bug — the SQL alone returns 0 rows). Red probe:
// removing the RequireScopes / len(visibleTypes) guards in a test double
// yields an empty graph instead of an error.
func TestStructuralHop_FailClosedLoud(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	frontier := []string{"019f6017-0022-7000-9000-000000000001"}

	if _, err := store.StructuralHopNeighborsForTest(ctx, pool, frontier, []string{}, nil, []string{"knowledge"}, w2Params(25)); err == nil {
		t.Error("empty readScopes returned a silent empty graph, want a loud error (RequireScopes)")
	}
	if _, err := store.StructuralHopNeighborsForTest(ctx, pool, frontier, []string{w2Scope}, nil, nil, w2Params(25)); err == nil {
		t.Error("empty visibleTypes returned a silent empty graph, want a loud error")
	}
}

// TestStructuralHop_IndexPlan is W2-G3: the EXPLAIN gate at REPRESENTATIVE
// volume (>=10^4 rows + a degree hub + ANALYZE — at toy volume the planner
// picks bitmap/seq scans regardless, design/01 §4.1 measurement). The red
// probe is PERMANENTLY encoded: dropping the M104 indexes inside a rolled-back
// tx re-creates the pre-M104 state and must produce a Sort over
// sl.created_at. Assertions are limited to the LEGS (index usage, no sort) —
// never the exact whole-plan shape (pgx generic-vs-custom plans).
func TestStructuralHop_IndexPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Seed ~10^4 structural rows over 160 blocks (PK surface needs distinct
	// pairs) + one degree-10^3 hub, then ANALYZE (stale stats flip plans
	// between autovacuum runs — the documented flake source).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f6017-0023-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'w2 seed block ' || i, 'x', 'w2'
		FROM generate_series(0, 159) AS i`); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		SELECT ('019f6017-0023-7000-9000-' || lpad(to_hex(a), 12, '0'))::uuid,
		       ('019f6017-0023-7000-9000-' || lpad(to_hex(b), 12, '0'))::uuid,
		       'references', 'w2', 'system',
		       now() - make_interval(mins => a * 160 + b)
		FROM generate_series(0, 79) AS a, generate_series(80, 159) AS b
		WHERE a <> b
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed links (6400 pairs): %v", err)
	}
	// Filler blocks BEFORE the hubs (the forward hub targets them): the BLOCK
	// cardinality must be representative too — with a tiny context_blocks the
	// planner flips to blocks-first (idx_blocks_type_name + PK join into the
	// links), which at the 1M+ target would be the pathological plan.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f6017-0024-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'w2 filler ' || i, 'x', 'w2'
		FROM generate_series(0, 19999) AS i`); err != nil {
		t.Fatalf("seed filler blocks: %v", err)
	}
	// Degree hub in BOTH directions (the EXPLAIN frontier is the hub): block 0
	// referenced by every other block (reverse leg's worst case) AND block 0
	// referencing 2000 filler blocks (forward leg's worst case). Without a fat
	// forward side the planner's PK-merge-join + sort over a few dozen rows
	// costs the same as the index path and the gate can't discriminate.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		SELECT ('019f6017-0023-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       '019f6017-0023-7000-9000-000000000000'::uuid,
		       'duplicate-of', 'w2', 'system',
		       now() - make_interval(secs => i)
		FROM generate_series(1, 159) AS i
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed rev hub: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		SELECT '019f6017-0023-7000-9000-000000000000'::uuid,
		       ('019f6017-0024-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'references', 'w2', 'system',
		       now() - make_interval(secs => i)
		FROM generate_series(0, 1999) AS i
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed fwd hub: %v", err)
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_structural_links`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total < 6000 {
		t.Fatalf("seed too small for a representative plan: %d rows", total)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_structural_links; ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	frontier := []string{"019f6017-0023-7000-9000-000000000000"}
	binds := []any{
		frontier, []string{"w2"}, nil, 25, nil, nil, nil, []string{}, []string{"knowledge"},
	}

	plan := explainPlan(t, pool, ctx, false, binds)
	for _, idx := range []string{"idx_struct_links_graph_fwd", "idx_struct_links_graph_rev"} {
		if !strings.Contains(plan, idx) {
			t.Errorf("W2-G3: plan does not use %s:\n%s", idx, plan)
		}
	}
	if hasLegSortNode(plan) {
		t.Errorf("W2-G3: explicit Sort NODE over sl.created_at in the legs (index order not used):\n%s", plan)
	}

	// PERMANENT red probe: pre-M104 state inside a rolled-back tx — the plan
	// MUST degrade to an explicit sort; if it does not, the green assertions
	// above prove nothing.
	preM104 := explainPlan(t, pool, ctx, true, binds)
	if strings.Contains(preM104, "idx_struct_links_graph_fwd") || strings.Contains(preM104, "idx_struct_links_graph_rev") {
		t.Error("red probe broken: dropped indexes still appear in the plan")
	}
	if !hasLegSortNode(preM104) {
		t.Errorf("red probe did not degrade to a leg Sort node — the green assertion is vacuous:\n%s", preM104)
	}

	// K4 consolidation evidence (decision, review F3: PREFERENCE alone does not
	// prove REPLACEABILITY — the drop probe does): after dropping
	// idx_struct_links_rev (in-tx, rolled back), graph_rev must still serve the
	// StructuralNeighbors reverse lookup via an index scan (link_class as a
	// filter instead of an index cond is acceptable — the prefix carries the
	// selectivity). M104 executes the consolidation; this assert keeps the
	// decision's evidence alive.
	consolidationQ := `EXPLAIN (COSTS OFF)
		SELECT sl.source_block_id, sl.link_class FROM context_structural_links sl
		WHERE sl.target_block_id = '019f6017-0023-7000-9000-000000000000'::uuid
		  AND sl.link_class = ANY('{references,duplicate-of}')`
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DROP INDEX IF EXISTS idx_struct_links_rev`); err != nil {
		t.Fatalf("drop rev index (K4 probe): %v", err)
	}
	var consolidation strings.Builder
	rows, err := tx.Query(ctx, consolidationQ)
	if err != nil {
		t.Fatalf("consolidation explain: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		consolidation.WriteString(line + "\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	plan = consolidation.String()
	if !strings.Contains(plan, "idx_struct_links_graph_rev") {
		t.Errorf("K4 consolidation broken: without idx_struct_links_rev the reverse lookup does not use graph_rev:\n%s", plan)
	}
	t.Logf("K4 consolidation evidence (StructuralNeighbors reverse lookup WITHOUT idx_struct_links_rev):\n%s", plan)
}

// hasLegSortNode reports whether the plan contains a Sort NODE whose key is
// the bare per-leg ordering (sl/sl_1 .created_at DESC). A "Sort Key:
// sl.created_at DESC" line under a *Merge Append* is the index ORDER being
// merged (the wanted shape); the same line under a *Sort* node means PG had
// to sort a leg explicitly (pre-M104 shape). The DISTINCT-ON sort above the
// hop keys on "sl.target_block_id, sl.created_at DESC" and stays allowed —
// it runs over at most 2·cap·|frontier| rows by construction.
func hasLegSortNode(plan string) bool {
	lines := strings.Split(plan, "\n")
	for i := 1; i < len(lines); i++ {
		key := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(key, "Sort Key: sl") || !strings.Contains(key, ".created_at DESC") {
			continue
		}
		if strings.HasPrefix(key, "Sort Key: sl.target_block_id") {
			continue // the DISTINCT ON sort (allowed, tiny by construction)
		}
		prev := strings.TrimSpace(lines[i-1])
		if strings.HasSuffix(prev, "Sort") && !strings.HasSuffix(prev, "Merge Append") {
			return true
		}
	}
	return false
}

// explainPlan runs EXPLAIN over the SHIPPED Q1s SQL (store.StructuralHopSQLForTest —
// no test-local copy) inside one tx with enable_seqscan off (plan stability at
// seeded volume); preM104 drops the M104 indexes first and rolls back.
func explainPlan(t *testing.T, pool *pgxpool.Pool, ctx context.Context, preM104 bool, binds []any) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	if preM104 {
		if _, err := tx.Exec(ctx, `DROP INDEX idx_struct_links_graph_fwd, idx_struct_links_graph_rev`); err != nil {
			t.Fatalf("drop indexes (red probe): %v", err)
		}
	}
	rows, err := tx.Query(ctx, "EXPLAIN (COSTS OFF) "+store.StructuralHopSQLForTest(), binds...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		b.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return b.String()
}
