//go:build integration

// B-W1b gates for migration 139_rrf_gen17_tiebreak.sql (Achse 04 §4.3):
// ctx_rrf Generation 17 replaces Generation 16's bare `ORDER BY r.score DESC`
// (134_rrf_gen16_ann_embedding_filter.sql:360) with `ORDER BY r.score DESC,
// cb.id`.
//
// The fixture, the 54 generated queries and the call surface are B-W1's
// (arms_parity_integration_test.go) — reused verbatim, not re-cut, because
// that fixture is the one whose tie population was measured (13/54 queries,
// 17 groups of 2). The only thing added here is the ORDER: fuseArmsOrdered
// (gen17_tiebreak_test.go) sorts by score descending AND id ascending, which
// is what Generation 17 promises and Generation 16 does not.
//
// Ties here are EXACT, never eps-close. Postgres compares float8 bit-wise in
// ORDER BY, so a pair that is merely within 1e-12 gets a fully defined order
// with no tiebreak involved and would prove nothing. Every group this file
// reasons about is built with `==`.
//
//	go test -tags=integration ./internal/rrf/ -run TestBW1bGen17 -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// bw1bGen16File / bw1bGen17File are the two migration bodies this file
// installs and re-installs inside one test database to compare the
// generations without paying for a second container.
const (
	bw1bGen16File = "134_rrf_gen16_ann_embedding_filter.sql"
	bw1bGen17File = "139_rrf_gen17_tiebreak.sql"
)

// bw1bPerturb is a second session configuration whose purpose is to make the
// planner build a DIFFERENT plan for the same query.
//
// It is NOT a semantics-free knob set, and the gate does not pretend it is.
// Two layers of ctx_rrf are plan-sensitive on their own, both OUTSIDE this
// migration's scope:
//
//   - the ann arm is approximate (HNSW, relaxed_order), so a different scan
//     can return a different top-75;
//   - each of the four arms ranks with `ROW_NUMBER() OVER (ORDER BY <score>)`
//     and no tiebreak of its own, so blocks with an equal ts_rank_cd or an
//     equal trigram similarity can swap RANKS when the input order changes —
//     which moves the fused score, not just the order.
//
// The comparison below therefore only judges the ORDER of queries whose
// delivered (id, score) multiset came out identical in both sessions; the rest
// are counted and reported separately as the arm-level finding they are.
var bw1bPerturb = []string{
	"SET enable_hashjoin = off",
	"SET enable_mergejoin = off",
	"SET enable_bitmapscan = off",
	"SET work_mem = '64kB'",
}

// ---------------------------------------------------------------------------
// Shared measurement
// ---------------------------------------------------------------------------

// bw1bRun is one measurement of the whole query set against whatever ctx_rrf
// currently is.
type bw1bRun struct {
	label string
	// ids[q] is the delivered id sequence for query q.
	ids [][]string
	// scores[q] is the delivered score sequence for query q.
	scores [][]float64
	// tieGroups counts exact-score groups of size > 1 that lie fully inside
	// the delivered window.
	tieGroups int
	// tieRows is the number of rows those groups cover.
	tieRows int
	// orderViolations counts tie groups whose delivered order is NOT the
	// id-ascending one. This is the red/green needle.
	orderViolations int
	// firstViolation records one violation verbatim for the log.
	firstViolation string
	// seqMismatch counts queries whose full delivered sequence differs from
	// the offline (score DESC, id ASC) fusion.
	seqMismatch int
}

// bw1bMeasure calls ctx_rrf and ctx_rrf_arms with identical arguments in ONE
// transaction per query (the GUC seam B-W1 pins), recomputes the fusion
// offline in Generation 17 order, and reports where the database disagrees.
func bw1bMeasure(t *testing.T, ctx context.Context, conn *pgxpool.Conn, fx bw1Fixture, label string) bw1bRun {
	t.Helper()
	run := bw1bRun{label: label}

	for qi, q := range bw1Queries() {
		emb := bw1Embedding(qi)
		args := bw1Args(q, emb, fx.granted, bw1Limit)

		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("%s/%s: begin: %v", label, q.name, err)
		}
		live, err := bw1CallRRF(ctx, tx, args)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s/%s: ctx_rrf call: %v", label, q.name, err)
		}
		arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, "")
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s/%s: ctx_rrf_arms call: %v", label, q.name, err)
		}
		_ = tx.Rollback(ctx)

		gotIDs := make([]string, len(live))
		gotScores := make([]float64, len(live))
		for i, r := range live {
			gotIDs[i] = r.id
			gotScores[i] = r.score
		}
		run.ids = append(run.ids, gotIDs)
		run.scores = append(run.scores, gotScores)

		want := fuseArmsOrdered(arms, armsLiveWeights, armsRRFK)
		n := bw1Limit
		if len(want) < n {
			n = len(want)
		}
		if len(live) != n {
			t.Fatalf("%s/%s: ctx_rrf returned %d rows, offline fusion of %d candidates expects %d",
				label, q.name, len(live), len(arms), n)
		}
		wantIDs := idsOf(want[:n])
		if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
			run.seqMismatch++
		}

		// Exact tie groups, computed on the offline fusion (whose scores are
		// bit-identical to ctx_rrf's — B-W1 gate (a), max |delta| = 0).
		for _, g := range bw1bExactTieGroups(want) {
			if g[1] > n {
				continue // group cut by p_limit: the tail is not observable here
			}
			run.tieGroups++
			run.tieRows += g[1] - g[0]
			deliveredGroup := gotIDs[g[0]:g[1]]
			if !sort.StringsAreSorted(deliveredGroup) {
				run.orderViolations++
				if run.firstViolation == "" {
					run.firstViolation = fmt.Sprintf("%s positions [%d,%d) score %.17g: delivered %v, id-ascending would be %v",
						q.name, g[0], g[1], want[g[0]].Score, deliveredGroup, wantIDs[g[0]:g[1]])
				}
			}
		}
	}
	return run
}

// bw1bExactTieGroups partitions an already-sorted fusion into runs of rows
// whose scores are EXACTLY equal. Unlike armsTieGroups (which takes an eps and
// is right for the B-W1 set comparison) this uses `==`, because only bit-equal
// scores leave Postgres's ORDER BY anything to decide.
func bw1bExactTieGroups(rows []fusedRow) [][2]int {
	var groups [][2]int
	for i := 0; i < len(rows); {
		j := i + 1
		for j < len(rows) && rows[j].Score == rows[i].Score {
			j++
		}
		if j-i > 1 {
			groups = append(groups, [2]int{i, j})
		}
		i = j
	}
	return groups
}

// bw1bInstall replaces ctx_rrf with the body of the named migration file. The
// file text is executed verbatim out of the embedded FS — never a paraphrase,
// so a drifted migration cannot be papered over by the test.
func bw1bInstall(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file string) {
	t.Helper()
	raw, err := migrations.Section(file)
	if err != nil {
		t.Fatalf("read embedded %s: %v", file, err)
	}
	if _, err := pool.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("install %s: %v", file, err)
	}
}

// bw1bInstalledOrderBy reads the ORDER BY of the live function body back out
// of the catalog, so every assertion below is anchored to what the database
// actually runs rather than to which file the test believes it ran.
func bw1bInstalledOrderBy(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var def string
	err := pool.QueryRow(ctx,
		`SELECT pg_get_functiondef(p.oid) FROM pg_proc p
		 JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE p.proname = 'ctx_rrf' AND n.nspname = 'public'`).Scan(&def)
	if err != nil {
		t.Fatalf("pg_get_functiondef(ctx_rrf): %v", err)
	}
	idx := strings.LastIndex(def, "ORDER BY r.score DESC")
	if idx < 0 {
		t.Fatal("installed ctx_rrf body has no final `ORDER BY r.score DESC` at all — the function is not the one under test")
	}
	line := def[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	return strings.TrimSpace(line)
}

// ---------------------------------------------------------------------------
// Gate (a) + (b): red on Generation 16, green on Generation 17
// ---------------------------------------------------------------------------

// TestBW1bGen17TieOrderRedGreen is the load-bearing gate. It starts on a
// database whose migration chain is capped at 138 — a genuine pre-139 state,
// not a simulated one — measures the tie ordering, then applies 139 through
// the real runner and measures again.
//
// Red side, two independent needles:
//
//  1. Within an exact-score tie group, is the delivered order id-ascending?
//     Under Generation 16 it is whatever the sort node happened to produce.
//  2. Does the delivered sequence change when the planner is pushed onto a
//     different plan (hashjoin/mergejoin/bitmapscan off, work_mem 64kB)?
//     Semantics are untouched by those knobs, so any difference is ordering.
//
// Green side: zero order violations, zero sequence mismatches against the
// offline (score DESC, id ASC) fusion, and plan-independent delivery — plus
// the migration's own promise, that the candidate SET and every score are
// unchanged from Generation 16.
func TestBW1bGen17TieOrderRedGreen(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 138)
	ctx := context.Background()

	if got := bw1bInstalledOrderBy(t, ctx, pool); got != "ORDER BY r.score DESC" {
		t.Fatalf("pre-139 ctx_rrf ends with %q, want the bare Generation 16 form — the RED state is not the shipped one", got)
	}

	fx := bw1SeedCorpus(t, pool)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	perturbed, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire perturbed: %v", err)
	}
	defer perturbed.Release()
	for _, s := range bw1bPerturb {
		if _, err := perturbed.Exec(ctx, s); err != nil {
			t.Fatalf("perturb %q: %v", s, err)
		}
	}

	// ---- RED -------------------------------------------------------------
	red := bw1bMeasure(t, ctx, conn, fx, "gen16")
	redPerturbed := bw1bMeasure(t, ctx, perturbed, fx, "gen16-perturbed")
	redPlanOrder, redPlanInput := bw1bSequenceDiff(red, redPerturbed)

	t.Logf("RED (chain capped at 138, ctx_rrf = Generation 16): %d exact tie groups covering %d rows, %d groups NOT delivered id-ascending, %d/54 queries whose full sequence differs from the offline (score DESC, id ASC) fusion",
		red.tieGroups, red.tieRows, red.orderViolations, red.seqMismatch)
	t.Logf("RED plan probe (hashjoin/mergejoin/bitmapscan off, work_mem 64kB): %d/54 queries reordered at identical rows and scores, %d/54 queries the arms fed differently (approximate ANN + untiebroken per-arm ROW_NUMBER — out of this migration's scope)",
		redPlanOrder, redPlanInput)
	if red.firstViolation != "" {
		t.Logf("RED sample violation: %s", red.firstViolation)
	}
	if red.tieGroups == 0 {
		t.Fatal("RED is vacuous: the fixture produced no exact-score tie at all, so there is nothing for a tiebreak to decide — sharpen the fixture, do not weaken the gate")
	}
	if red.orderViolations == 0 && redPlanOrder == 0 {
		t.Fatalf("RED failed: Generation 16 delivered all %d tie groups id-ascending AND stayed stable across plans — the gate would pass on the unfixed body",
			red.tieGroups)
	}

	// ---- apply 139 -------------------------------------------------------
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply remaining migrations (139): %v", err)
	}
	if got := bw1bInstalledOrderBy(t, ctx, pool); got != "ORDER BY r.score DESC, cb.id" {
		t.Fatalf("post-139 ctx_rrf ends with %q, want %q", got, "ORDER BY r.score DESC, cb.id")
	}

	// ---- GREEN -----------------------------------------------------------
	green := bw1bMeasure(t, ctx, conn, fx, "gen17")
	greenPerturbed := bw1bMeasure(t, ctx, perturbed, fx, "gen17-perturbed")
	greenPlanOrder, greenPlanInput := bw1bSequenceDiff(green, greenPerturbed)

	t.Logf("GREEN (after 139, ctx_rrf = Generation 17): %d exact tie groups covering %d rows, %d order violations, %d sequence mismatches",
		green.tieGroups, green.tieRows, green.orderViolations, green.seqMismatch)
	t.Logf("GREEN plan probe: %d/54 queries reordered at identical rows and scores, %d/54 fed differently by the arms (unchanged in kind from RED: %d)",
		greenPlanOrder, greenPlanInput, redPlanInput)
	if green.orderViolations != 0 {
		t.Errorf("Generation 17 left %d tie groups out of id-ascending order; first: %s", green.orderViolations, green.firstViolation)
	}
	if green.seqMismatch != 0 {
		t.Errorf("Generation 17 disagreed with the offline (score DESC, id ASC) fusion on %d of 54 queries", green.seqMismatch)
	}
	if greenPlanOrder != 0 {
		t.Errorf("Generation 17 delivered %d of 54 queries in a different ORDER under a different plan, at identical rows and scores — the final order is still not total", greenPlanOrder)
	}
	if green.tieGroups != red.tieGroups || green.tieRows != red.tieRows {
		t.Errorf("tie population changed across the migration: %d groups/%d rows before, %d/%d after — 139 must not touch scores",
			red.tieGroups, red.tieRows, green.tieGroups, green.tieRows)
	}

	// ---- the migration's own promise: same candidates, same scores --------
	bw1bAssertSameSetAndScores(t, red, green)
}

// bw1bSequenceDiff compares two runs of the same corpus under two different
// plans and splits the differences into the two classes that matter:
//
//	orderDiff — same delivered rows, same scores, DIFFERENT sequence. This is
//	            the undefined tie order and the only thing migration 139 owns.
//	inputDiff — the delivered (id, score) multiset itself differs, so the
//	            arms handed the fusion different material. Out of scope here
//	            (approximate ANN + untiebroken ROW_NUMBER per arm); counted
//	            and reported, never asserted on.
func bw1bSequenceDiff(a, b bw1bRun) (orderDiff, inputDiff int) {
	for q := range a.ids {
		if q >= len(b.ids) {
			break
		}
		if bw1bSameMultiset(a.ids[q], a.scores[q], b.ids[q], b.scores[q]) {
			if strings.Join(a.ids[q], ",") != strings.Join(b.ids[q], ",") {
				orderDiff++
			}
			continue
		}
		inputDiff++
	}
	return orderDiff, inputDiff
}

// bw1bSameMultiset reports whether two deliveries carry the same (id, score)
// pairs, order disregarded.
func bw1bSameMultiset(idsA []string, scoresA []float64, idsB []string, scoresB []float64) bool {
	if len(idsA) != len(idsB) {
		return false
	}
	pairs := func(ids []string, scores []float64) []string {
		out := make([]string, len(ids))
		for i := range ids {
			out[i] = fmt.Sprintf("%s@%.17g", ids[i], scores[i])
		}
		sort.Strings(out)
		return out
	}
	return strings.Join(pairs(idsA, scoresA), ",") == strings.Join(pairs(idsB, scoresB), ",")
}

// bw1bAssertSameSetAndScores is the byte-equality half of the migration's
// promise: Generation 17 may reorder inside a tie and nothing else. The
// candidate set per query must be identical, and the score sequence must be
// identical value for value — not within a tolerance, exactly.
func bw1bAssertSameSetAndScores(t *testing.T, before, after bw1bRun) {
	t.Helper()
	qs := bw1Queries()
	movedRows, movedQueries := 0, 0
	for q := range before.ids {
		a, b := before.ids[q], after.ids[q]
		if len(a) != len(b) {
			t.Errorf("%s: %d rows before, %d after", qs[q].name, len(a), len(b))
			continue
		}
		sa, sb := append([]string(nil), a...), append([]string(nil), b...)
		sort.Strings(sa)
		sort.Strings(sb)
		if strings.Join(sa, ",") != strings.Join(sb, ",") {
			t.Errorf("%s: candidate SET changed across 139\n before: %v\n after:  %v", qs[q].name, sa, sb)
		}
		// Score SEQUENCE, position by position: reordering happens only
		// inside equal-score runs, so the sequence itself cannot move.
		for i := range before.scores[q] {
			if before.scores[q][i] != after.scores[q][i] {
				t.Errorf("%s position %d: score %.17g before, %.17g after — 139 must be score-neutral",
					qs[q].name, i, before.scores[q][i], after.scores[q][i])
				break
			}
		}
		if strings.Join(a, ",") != strings.Join(b, ",") {
			movedQueries++
			for i := range a {
				if a[i] != b[i] {
					movedRows++
				}
			}
		}
	}
	t.Logf("139 effect on the fixture: %d/54 queries changed their delivered ORDER, %d row positions moved; candidate sets and score sequences identical",
		movedQueries, movedRows)
}

// ---------------------------------------------------------------------------
// Gate (b2): strict parity and repeat stability on the full chain
// ---------------------------------------------------------------------------

// TestBW1bGen17StrictOrderParity runs on a database with the COMPLETE
// migration chain (the shipped state, not a capped one) and drops every
// tie tolerance B-W1 had to carry: over all 54 queries the delivered ids, the
// delivered order and the delivered scores must match the offline fusion
// exactly, |delta score| = 0. It then calls the same query three times in a
// row and requires the identical sequence each time.
func TestBW1bGen17StrictOrderParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if got := bw1bInstalledOrderBy(t, ctx, pool); got != "ORDER BY r.score DESC, cb.id" {
		t.Fatalf("full chain installs ctx_rrf ending with %q, want the Generation 17 form", got)
	}

	fx := bw1SeedCorpus(t, pool)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	var maxDelta float64
	var rows int
	for qi, q := range bw1Queries() {
		emb := bw1Embedding(qi)
		args := bw1Args(q, emb, fx.granted, bw1Limit)

		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", q.name, err)
		}
		live, err := bw1CallRRF(ctx, tx, args)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: ctx_rrf: %v", q.name, err)
		}
		arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, "")
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: ctx_rrf_arms: %v", q.name, err)
		}
		_ = tx.Rollback(ctx)

		want := fuseArmsOrdered(arms, armsLiveWeights, armsRRFK)
		n := bw1Limit
		if len(want) < n {
			n = len(want)
		}
		if len(live) != n {
			t.Fatalf("%s: %d delivered rows, %d expected", q.name, len(live), n)
		}
		rows += n
		for i := 0; i < n; i++ {
			if live[i].id != want[i].ID {
				t.Errorf("%s position %d: ctx_rrf delivered %s, offline fusion has %s (scores %.17g / %.17g)",
					q.name, i, live[i].id, want[i].ID, live[i].score, want[i].Score)
			}
			d := live[i].score - want[i].Score
			if d < 0 {
				d = -d
			}
			if d > maxDelta {
				maxDelta = d
			}
			if d != 0 {
				t.Errorf("%s position %d (%s): score %.17g vs %.17g — strict parity allows no delta at all",
					q.name, i, live[i].id, live[i].score, want[i].Score)
			}
		}

		// Repeat stability: three consecutive calls, identical sequence.
		var seqs []string
		for rep := 0; rep < 3; rep++ {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("%s rep %d: begin: %v", q.name, rep, err)
			}
			r, err := bw1CallRRF(ctx, tx, args)
			_ = tx.Rollback(ctx)
			if err != nil {
				t.Fatalf("%s rep %d: ctx_rrf: %v", q.name, rep, err)
			}
			ids := make([]string, len(r))
			for i := range r {
				ids[i] = r[i].id
			}
			seqs = append(seqs, strings.Join(ids, ","))
		}
		if seqs[0] != seqs[1] || seqs[1] != seqs[2] {
			t.Errorf("%s: three consecutive calls returned different sequences:\n 1: %s\n 2: %s\n 3: %s",
				q.name, seqs[0], seqs[1], seqs[2])
		}
	}
	t.Logf("strict parity: 54 queries, %d delivered rows, ids+order+scores identical to the offline (score DESC, id ASC) fusion, max |score delta| = %.17g; 3x repeat stable on every query",
		rows, maxDelta)
}

// ---------------------------------------------------------------------------
// Gate (c): plan shape
// ---------------------------------------------------------------------------

// TestBW1bGen17ExplainPlanShape compares the plan of Generation 16's and
// Generation 17's projection on the fixture.
//
// A plpgsql body cannot be EXPLAINed from the client — `EXPLAIN SELECT * FROM
// ctx_rrf(...)` only ever shows `Function Scan on ctx_rrf`. The gate therefore
// lifts the RETURN QUERY statement out of each migration file verbatim,
// substitutes the p_* parameters with the same $1..$18 the function receives,
// and EXPLAINs that. Same SQL text the function runs, same arguments, same
// database — the one thing it cannot reproduce is plpgsql's plan cache, which
// is irrelevant to the node SHAPE being compared.
func TestBW1bGen17ExplainPlanShape(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	fx := bw1SeedCorpus(t, pool)
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	q := bw1Queries()[0]
	args := bw1Args(q, bw1Embedding(0), fx.granted, bw1Limit)

	before := bw1bExplain(t, ctx, pool, bw1bGen16File, args)
	after := bw1bExplain(t, ctx, pool, bw1bGen17File, args)

	t.Logf("gate (c) Generation 16 plan (node lines only):\n%s", strings.Join(bw1bNodeLines(before), "\n"))
	t.Logf("gate (c) Generation 17 plan (node lines only):\n%s", strings.Join(bw1bNodeLines(after), "\n"))

	nBefore, nAfter := bw1bNodeCensus(before), bw1bNodeCensus(after)
	if bw1bCensusString(nBefore) != bw1bCensusString(nAfter) {
		t.Errorf("plan node census changed:\n before: %s\n after:  %s\n— 139 is meant to extend a sort key, not to move the plan",
			bw1bCensusString(nBefore), bw1bCensusString(nAfter))
	}
	t.Logf("gate (c) node census identical: %s", bw1bCensusString(nBefore))

	// The one difference that MUST be there: the projection's sort key gained
	// cb.id. Sort keys are logged truncated — one of them inlines the whole
	// 1024-dimension query vector.
	keyBefore := bw1bSortKeys(before)
	keyAfter := bw1bSortKeys(after)
	if len(keyBefore) != len(keyAfter) {
		t.Fatalf("plan carries %d sort keys before and %d after — that is a plan change, not a key extension", len(keyBefore), len(keyAfter))
	}
	if len(keyAfter) == 0 {
		t.Fatal("Generation 17 plan carries no sort key line at all — the EXPLAIN extraction is broken, not the migration")
	}
	changed := 0
	for i := range keyAfter {
		if keyBefore[i] == keyAfter[i] {
			continue
		}
		changed++
		t.Logf("gate (c) sort key %d changed:\n before: %s\n after:  %s",
			i, bw1bTail(keyBefore[i], 90), bw1bTail(keyAfter[i], 90))
		if !strings.HasSuffix(keyAfter[i], ", cb.id") {
			t.Errorf("sort key %d changed to something other than a trailing `, cb.id`: %s", i, bw1bTail(keyAfter[i], 200))
		}
		if keyAfter[i] != keyBefore[i]+", cb.id" {
			t.Errorf("sort key %d is not Generation 16's key plus `, cb.id`", i)
		}
	}
	if changed != 1 {
		t.Errorf("%d sort keys changed, want exactly 1 (the final projection's)", changed)
	}
	for i, k := range keyAfter {
		t.Logf("gate (c) sort key %d (tail): %s", i, bw1bTail(k, 70))
	}
}

// bw1bNodeLines keeps only the node lines of a plan, dropping every detail
// line — one of which inlines a 1024-dimension vector literal.
func bw1bNodeLines(plan []string) []string {
	var out []string
	for i, line := range plan {
		if i == 0 || strings.HasPrefix(strings.TrimSpace(line), "->") {
			out = append(out, line)
		}
	}
	return out
}

// bw1bCensusString renders a node census deterministically for comparison and
// for the log.
func bw1bCensusString(census map[string]int) string {
	keys := make([]string, 0, len(census))
	for k := range census {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, census[k]))
	}
	return strings.Join(parts, ", ")
}

// bw1bTail returns the last n characters of s, which is where an ORDER BY
// extension shows up and where the vector literal does not.
func bw1bTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// bw1bParam ties a p_* parameter of ctx_rrf to its slot in the bw1Args slice
// and to the cast the extracted query needs so a NULL never leaves the planner
// guessing (the same casts bw1ArgList carries).
type bw1bParam struct {
	name string
	arg  int    // 0-based index into bw1Args' 18-element slice
	cast string // appended after the placeholder; empty for the embedding
}

// bw1bParams is ordered LONGEST NAME FIRST among overlapping prefixes, so that
// p_query_spaced and p_query_or are substituted before p_query, and
// p_categories_exclude before p_category. p_scan_tuples appears only in the
// plpgsql prologue (set_config) and therefore never survives into the
// extracted query — the renumbering below drops it rather than passing an
// argument no placeholder refers to, which is what PostgreSQL rejects with
// 42P18.
var bw1bParams = []bw1bParam{
	{"p_categories_exclude", 12, "::text[]"},
	{"p_granted_block_ids", 14, "::uuid[]"},
	{"p_damped_factors", 11, "::double precision[]"},
	{"p_query_spaced", 2, "::text"},
	{"p_semantic_mode", 15, "::text"},
	{"p_types_visible", 9, "::text[]"},
	{"p_types_exclude", 13, "::text[]"},
	{"p_damped_types", 10, "::text[]"},
	{"p_scan_tuples", 16, "::int"},
	{"p_exact_cap", 17, "::int"},
	{"p_embedding", 0, ""},
	{"p_query_or", 8, "::text"},
	{"p_temporal", 7, "::text"},
	{"p_category", 4, "::text"},
	{"p_scopes", 3, "::text[]"},
	{"p_limit", 6, "::int"},
	{"p_query", 1, "::text"},
	{"p_tags", 5, "::text[]"},
}

// bw1bExplain lifts the RETURN QUERY statement out of a migration file and
// EXPLAINs it with the function's own arguments.
func bw1bExplain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file string, args []any) []string {
	t.Helper()
	raw, err := migrations.Section(file)
	if err != nil {
		t.Fatalf("read embedded %s: %v", file, err)
	}
	body := string(raw)
	start := strings.Index(body, "RETURN QUERY")
	if start < 0 {
		t.Fatalf("%s: no RETURN QUERY — migration drifted?", file)
	}
	body = body[start+len("RETURN QUERY"):]
	end := strings.Index(body, "LIMIT p_limit;")
	if end < 0 {
		t.Fatalf("%s: no `LIMIT p_limit;` terminator — migration drifted?", file)
	}
	query := body[:end+len("LIMIT p_limit;")]
	var used []any
	for _, p := range bw1bParams {
		if !strings.Contains(query, p.name) {
			continue
		}
		used = append(used, args[p.arg])
		query = strings.ReplaceAll(query, p.name, fmt.Sprintf("$%d%s", len(used), p.cast))
	}
	if strings.Contains(query, "p_") {
		t.Fatalf("%s: unsubstituted parameter left in the extracted query:\n%s", file, query)
	}

	rows, err := pool.Query(ctx, "EXPLAIN (COSTS OFF) "+query, used...)
	if err != nil {
		t.Fatalf("EXPLAIN of %s body: %v", file, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN line: %v", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	return out
}

// bw1bNodeCensus counts plan node types, ignoring indentation and every
// per-node detail line. In EXPLAIN output the root node is the first line and
// every other node line starts with "->"; anything else is a detail of the
// node above it. Two plans with the same census run the same node types the
// same number of times — which is the "no new sort node, no scan-method
// change" claim 139's header makes.
func bw1bNodeCensus(plan []string) map[string]int {
	census := map[string]int{}
	for i, line := range plan {
		trimmed := strings.TrimSpace(line)
		if i != 0 && !strings.HasPrefix(trimmed, "->") {
			continue
		}
		// EXPLAIN writes "->" followed by TWO spaces, so trim again.
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "->"))
		if j := strings.Index(trimmed, " ("); j >= 0 {
			trimmed = trimmed[:j]
		}
		census[trimmed]++
	}
	return census
}

// bw1bSortKeys collects every `Sort Key:` detail line.
func bw1bSortKeys(plan []string) []string {
	var out []string
	for _, line := range plan {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Sort Key:") {
			out = append(out, trimmed)
		}
	}
	return out
}
