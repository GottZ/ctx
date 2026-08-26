//go:build integration

// V-W4 gates for migration 140_trigram_gist_knn.sql (Achse 05, Defekt S4):
// the fourth RRF arm moves from a threshold scan to a KNN lookup over a new
// GiST trigram index.
//
// The gate is deliberately staged inside ONE database per test: every test
// starts at migration 139 (SetupTestDBUpTo), records the RED state of the arm
// as it stands today, then runs the real migration runner to apply 140 and
// records the GREEN state against the SAME rows. A red state measured on a
// different fixture would prove nothing.
//
// The CTE under test lives inside a plpgsql body and can therefore not be
// EXPLAINed through the function — `EXPLAIN SELECT * FROM ctx_rrf(...)` only
// ever shows `Function Scan on ctx_rrf`. Like the Generation 17 gate
// (gen17_tiebreak_integration_test.go:660) these tests lift the CTE text
// verbatim out of the migration file and substitute the p_* parameters with
// $1..$n, so the SQL that is planned and the SQL the function runs are the
// same characters. Nothing here re-types the CTE by hand; a drift in the
// migration therefore drifts the test with it instead of past it.
//
//	go test -tags=integration ./internal/rrf/ -run TestVW4 -count=1 -v
package rrf_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	vw4Prev = "139_rrf_gen17_tiebreak.sql" // ctx_rrf before this wave
	vw4Arms = "137_rrf_arms.sql"           // ctx_rrf_arms before this wave
	vw4New  = "140_trigram_gist_knn.sql"   // the wave

	vw4Index = "idx_trgm_title_gist"
	vw4Scope = "vw4"
	vw4Type  = "knowledge"

	// vw4Cap and vw4Threshold are ctx_rrf's literals in the trigram arm. They
	// are asserted against, never used to build the SQL — the SQL comes out of
	// the migration file.
	vw4Cap       = 30
	vw4Threshold = 0.05

	// vw4BigRows is the brief's mandatory fixture floor. Below it the planner
	// picks a sequential scan for the KNN query as well (cheaper on a small
	// heap) and the plan gate would be vacuous.
	vw4BigRows = 100000

	// vw4CorpusOrder is what "rows in Korpus-Größenordnung" means for the red
	// assertion: a scan estimate at least this large is not a capped arm.
	vw4CorpusOrder = 1000

	// vw4NoHexQuery shares no trigram with a hexadecimal string: every one of
	// its trigrams contains at least one character outside [0-9a-f]. The noise
	// rows below are pure md5, so their similarity to this query is exactly
	// zero — which is what keeps the graded fixture's similarity ladder clean.
	vw4NoHexQuery   = "wüst grün zügig"
	vw4SparseQuery  = "gnitzy prunkvoll wispern"
	vw4GradedRows   = 80
	vw4SparseGraded = 8
)

// ---------------------------------------------------------------------------
// Lifting the CTE out of the migration
// ---------------------------------------------------------------------------

// vw4Params maps the plpgsql parameter names that occur inside trigram_title
// to the placeholder cast they need. The order fixes the $n numbering and must
// match vw4Args. p_categories_exclude precedes p_category on purpose:
// substituting the shorter name first would corrupt the longer one.
var vw4Params = []struct{ name, cast string }{
	{"p_query", "::text"},
	{"p_types_visible", "::text[]"},
	{"p_types_exclude", "::text[]"},
	{"p_scopes", "::text[]"},
	{"p_granted_block_ids", "::uuid[]"},
	{"p_categories_exclude", "::text[]"},
	{"p_category", "::text"},
	{"p_tags", "::text[]"},
	{"p_trgm_threshold", "::double precision"},
	{"p_cap_trigram", "::int"},
}

// vw4Args are the values for vw4Params, in the same order.
func vw4Args(query string) []any {
	return []any{
		query,              // p_query
		[]string{vw4Type},  // p_types_visible
		nil,                // p_types_exclude
		[]string{vw4Scope}, // p_scopes
		nil,                // p_granted_block_ids
		nil,                // p_categories_exclude
		nil,                // p_category
		nil,                // p_tags
		vw4Threshold,       // p_trgm_threshold
		vw4Cap,             // p_cap_trigram
	}
}

// vw4CTE extracts the FIRST trigram_title CTE of a migration file (ctx_rrf's;
// ctx_rrf_arms' copy follows it in 140) and wraps it into a standalone
// statement. `strip` optionally removes the one line that contains it — the
// negative probe uses that to delete the threshold post-filter.
func vw4CTE(t *testing.T, file, query, strip string) (string, []any) {
	t.Helper()
	raw, err := migrations.Section(file)
	if err != nil {
		t.Fatalf("read embedded %s: %v", file, err)
	}
	body := string(raw)
	const open = "    trigram_title AS (\n"
	const closeMark = "\n    ),\n    block_mass AS ("
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("%s: no trigram_title CTE — migration drifted?", file)
	}
	j := strings.Index(body[i:], closeMark)
	if j < 0 {
		t.Fatalf("%s: trigram_title CTE has no block_mass terminator — migration drifted?", file)
	}
	cte := body[i+len(open) : i+j]

	if strip != "" {
		var kept []string
		hits := 0
		for _, line := range strings.Split(cte, "\n") {
			if strings.Contains(line, strip) {
				hits++
				continue
			}
			kept = append(kept, line)
		}
		if hits != 1 {
			t.Fatalf("%s: strip %q matched %d lines, want exactly 1", file, strip, hits)
		}
		cte = strings.Join(kept, "\n")
	}

	stmt := "SELECT * FROM (\n" + cte + "\n) trigram_title ORDER BY rank"
	var used []any
	all := vw4Args(query)
	for k, p := range vw4Params {
		if !strings.Contains(stmt, p.name) {
			continue
		}
		used = append(used, all[k])
		stmt = strings.ReplaceAll(stmt, p.name, fmt.Sprintf("$%d%s", len(used), p.cast))
	}
	if strings.Contains(stmt, "p_") {
		t.Fatalf("%s: unsubstituted parameter left in the lifted CTE:\n%s", file, stmt)
	}
	return stmt, used
}

// vw4Ranked is one row of the lifted CTE.
type vw4Ranked struct {
	ID   string
	Rank int64
}

func vw4RunCTE(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file, query string) []vw4Ranked {
	t.Helper()
	stmt, args := vw4CTE(t, file, query, "")
	rows, err := pool.Query(ctx, stmt, args...)
	if err != nil {
		t.Fatalf("%s CTE: %v", file, err)
	}
	defer rows.Close()
	var out []vw4Ranked
	for rows.Next() {
		var r vw4Ranked
		if err := rows.Scan(&r.ID, &r.Rank); err != nil {
			t.Fatalf("%s CTE scan: %v", file, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s CTE rows: %v", file, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Plan inspection
// ---------------------------------------------------------------------------

// vw4Node is the slice of an EXPLAIN (FORMAT JSON) node this gate reasons
// about; the rest of the node is deliberately not decoded.
type vw4Node struct {
	NodeType    string    `json:"Node Type"`
	IndexName   string    `json:"Index Name"`
	PlanRows    float64   `json:"Plan Rows"`
	StartupCost float64   `json:"Startup Cost"`
	TotalCost   float64   `json:"Total Cost"`
	RelName     string    `json:"Relation Name"`
	Plans       []vw4Node `json:"Plans"`
}

// find returns the first node in pre-order for which pred holds.
func (n vw4Node) find(pred func(vw4Node) bool) (vw4Node, bool) {
	if pred(n) {
		return n, true
	}
	for _, c := range n.Plans {
		if hit, ok := c.find(pred); ok {
			return hit, true
		}
	}
	return vw4Node{}, false
}

func vw4IsSeqScan(n vw4Node) bool {
	return strings.Contains(n.NodeType, "Seq Scan") && n.RelName == "context_blocks"
}

func vw4ExplainJSON(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file, query string) vw4Node {
	t.Helper()
	stmt, args := vw4CTE(t, file, query, "")
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+stmt, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN JSON of %s CTE: %v", file, err)
	}
	var wrapper []struct {
		Plan vw4Node `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("decode EXPLAIN JSON: %v", err)
	}
	if len(wrapper) != 1 {
		t.Fatalf("EXPLAIN JSON returned %d plans, want 1", len(wrapper))
	}
	return wrapper[0].Plan
}

func vw4ExplainText(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file, query string) string {
	t.Helper()
	stmt, args := vw4CTE(t, file, query, "")
	rows, err := pool.Query(ctx, "EXPLAIN "+stmt, args...)
	if err != nil {
		t.Fatalf("EXPLAIN of %s CTE: %v", file, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN line: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// vw4SeedBig writes n visible rows in one statement. Titles carry a stable
// prefix plus two md5 digests so the corpus has real trigram variation instead
// of n copies of one string (which would make every distance equal and the KNN
// order meaningless).
func vw4SeedBig(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) time.Duration {
	t.Helper()
	start := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, type_name)
		SELECT 'vw4',
		       'vw4 block ' || s || ' ' || md5(s::text) || ' ' || md5((s * 7919)::text),
		       'vw4 fixture body ' || s,
		       $1, $2
		FROM generate_series(1, $3) s`, vw4Scope, vw4Type, n); err != nil {
		t.Fatalf("seed %d rows: %v", n, err)
	}
	vw4Analyze(t, ctx, pool)
	return time.Since(start)
}

func vw4Analyze(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// vw4SeedGraded writes rows whose trigram similarity to `query` decreases with
// the length of an appended hexadecimal filler, plus `noise` rows of pure md5.
//
// Two properties matter and are established here rather than assumed:
//   - the query shares no trigram with a hex string, so every noise row has
//     similarity exactly 0 and can never enter the arm;
//   - filler growth occasionally leaves the trigram union unchanged, which
//     produces EQUAL similarities. Those duplicates are deleted, because a tie
//     that straddles the cap has no defined winner under EITHER form and would
//     make a set comparison meaningless rather than failing.
func vw4SeedGraded(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, graded, noise int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, type_name)
		SELECT 'vw4',
		       $1 || ' ' || left(f.filler, 10 + s),
		       'graded ' || s,
		       $2, $3
		FROM generate_series(1, $4) s,
		     (SELECT string_agg(md5(k::text), ' ') AS filler FROM generate_series(1, 16) k) f`,
		query, vw4Scope, vw4Type, graded); err != nil {
		t.Fatalf("seed graded rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM context_blocks a
		 WHERE a.scope = $1
		   AND similarity(a.title, $2) > $3
		   AND EXISTS (SELECT 1 FROM context_blocks b
		                WHERE b.scope = $1
		                  AND b.id <> a.id
		                  AND similarity(b.title, $2) = similarity(a.title, $2)
		                  AND b.id < a.id)`, vw4Scope, query, vw4Threshold); err != nil {
		t.Fatalf("de-duplicate graded similarities: %v", err)
	}
	if noise > 0 {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_blocks (category, title, content, scope, type_name)
			SELECT 'vw4', md5(s::text) || ' ' || md5((s * 104729)::text),
			       'noise ' || s, $1, $2
			FROM generate_series(1, $3) s`, vw4Scope, vw4Type, noise); err != nil {
			t.Fatalf("seed noise rows: %v", err)
		}
	}
	vw4Analyze(t, ctx, pool)
}

// vw4Similarities returns the similarity values of the rows above the
// threshold, descending.
func vw4Similarities(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) []float64 {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT similarity(title, $1)::float8 AS sim
		  FROM context_blocks
		 WHERE NOT is_archived AND scope = $2 AND similarity(title, $1) > $3
		 ORDER BY sim DESC`, query, vw4Scope, vw4Threshold)
	if err != nil {
		t.Fatalf("similarity census: %v", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var s float64
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan similarity: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("similarity census rows: %v", err)
	}
	return out
}

func vw4AssertStrictlyGraded(t *testing.T, sims []float64, query string) {
	t.Helper()
	for i := 1; i < len(sims); i++ {
		if sims[i] >= sims[i-1] {
			t.Fatalf("fixture for %q is not strictly graded: sim[%d] = %.17g >= sim[%d] = %.17g — a tie across the cap has no defined winner under either form",
				query, i, sims[i], i-1, sims[i-1])
		}
	}
}

func vw4ApplyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Duration {
	t.Helper()
	start := time.Now()
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migration %s: %v", vw4New, err)
	}
	d := time.Since(start)

	var version int
	if err := pool.QueryRow(ctx, "SELECT max(version) FROM _migrations").Scan(&version); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if version < 140 {
		t.Fatalf("migration chain stopped at %d — 140 did not land", version)
	}
	return d
}

func vw4IndexAM(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var am string
	err := pool.QueryRow(ctx, `
		SELECT am.amname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_am am ON am.oid = c.relam
		 WHERE n.nspname = 'public' AND c.relname = $1`, name).Scan(&am)
	if err != nil {
		return ""
	}
	return am
}

// ---------------------------------------------------------------------------
// Gate 1 + 2 + 3: red plan, index-scan plan, capped estimate at >= 100k rows
// ---------------------------------------------------------------------------

func TestVW4TrigramKNNPlanShape(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 139)
	ctx := context.Background()

	seedFor := vw4SeedBig(t, ctx, pool, vw4BigRows)
	var seeded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM context_blocks WHERE scope = $1", vw4Scope).Scan(&seeded); err != nil {
		t.Fatalf("count fixture: %v", err)
	}
	if seeded < vw4BigRows {
		t.Fatalf("fixture has %d rows, the brief requires >= %d", seeded, vw4BigRows)
	}
	t.Logf("fixture: %d rows seeded + ANALYZE in %s", seeded, seedFor.Round(time.Millisecond))

	const q = "vw4 block 4242 knn probe"

	// --- RED: the arm as migration 139 leaves it ----------------------------
	if am := vw4IndexAM(t, ctx, pool, vw4Index); am != "" {
		t.Fatalf("%s already exists (access method %q) before 140 — the red state is not the red state", vw4Index, am)
	}
	redText := vw4ExplainText(t, ctx, pool, vw4Prev, q)
	t.Logf("RED (139 trigram_title, %d rows):\n%s", seeded, redText)

	if strings.Contains(redText, vw4Index) {
		t.Fatalf("RED plan already names %s:\n%s", vw4Index, redText)
	}
	redScan, ok := vw4ExplainJSON(t, ctx, pool, vw4Prev, q).find(vw4IsSeqScan)
	if !ok {
		t.Fatalf("RED plan has no sequential scan on context_blocks — expected the S4 defect:\n%s", redText)
	}
	if redScan.PlanRows < vw4CorpusOrder {
		t.Fatalf("RED scan estimates %.0f rows — below the corpus order of magnitude (%d); the fixture is not exercising the defect",
			redScan.PlanRows, vw4CorpusOrder)
	}
	t.Logf("RED gate 1: %s on context_blocks, Plan Rows = %.0f (corpus order), no %s in the plan",
		redScan.NodeType, redScan.PlanRows, vw4Index)

	// --- build 140 ----------------------------------------------------------
	migFor := vw4ApplyMigrations(t, ctx, pool)
	am := vw4IndexAM(t, ctx, pool, vw4Index)
	if am != "gist" {
		t.Fatalf("%s has access method %q after 140, want \"gist\"", vw4Index, am)
	}
	var idxSize string
	if err := pool.QueryRow(ctx, "SELECT pg_size_pretty(pg_relation_size($1::regclass))", vw4Index).Scan(&idxSize); err != nil {
		t.Fatalf("index size: %v", err)
	}
	t.Logf("migration 140 applied in %s; %s is a %s index of %s over %d rows",
		migFor.Round(time.Millisecond), vw4Index, am, idxSize, seeded)

	// --- GREEN 1: the plan uses the index for the ORDERING ------------------
	greenText := vw4ExplainText(t, ctx, pool, vw4New, q)
	t.Logf("GREEN (140 trigram_title, same %d rows):\n%s", seeded, greenText)

	if !strings.Contains(greenText, "Index Scan using "+vw4Index) {
		t.Fatalf("GREEN plan does not use %s for an index scan:\n%s", vw4Index, greenText)
	}
	if !strings.Contains(greenText, "Order By:") || !strings.Contains(greenText, "<->") {
		t.Fatalf("GREEN plan has no `Order By: ... <-> ...` line — the index is used as a filter, not for KNN:\n%s", greenText)
	}
	greenPlan := vw4ExplainJSON(t, ctx, pool, vw4New, q)
	if _, bad := greenPlan.find(vw4IsSeqScan); bad {
		t.Errorf("GREEN plan still contains a sequential scan on context_blocks:\n%s", greenText)
	}

	// --- GREEN 2/3: the capped sub-plan is estimated at <= 30 rows ----------
	knnScan, ok := greenPlan.find(func(n vw4Node) bool { return n.IndexName == vw4Index })
	if !ok {
		t.Fatalf("GREEN JSON plan has no node on %s:\n%s", vw4Index, greenText)
	}
	limit, ok := greenPlan.find(func(n vw4Node) bool {
		if n.NodeType != "Limit" {
			return false
		}
		_, hit := n.find(func(c vw4Node) bool { return c.IndexName == vw4Index })
		return hit
	})
	if !ok {
		t.Fatalf("GREEN plan has no Limit node above the %s scan:\n%s", vw4Index, greenText)
	}
	if limit.PlanRows > vw4Cap {
		t.Errorf("KNN sub-plan estimates %.0f rows, the cap is %d", limit.PlanRows, vw4Cap)
	}
	if greenPlan.PlanRows > vw4Cap {
		t.Errorf("GREEN root node estimates %.0f rows, the cap is %d", greenPlan.PlanRows, vw4Cap)
	}
	// The scan node UNDER a Limit reports its full estimate — Postgres shows
	// the early stop in the cost, not in the row count. The cost ratio is
	// therefore the load-bearing number, and it is exactly what the red plan
	// cannot produce: its sort has to consume everything before the first row
	// leaves.
	ratio := limit.TotalCost / knnScan.TotalCost
	if ratio > 0.05 {
		t.Errorf("KNN sub-plan costs %.2f of the %.2f the underlying scan would cost (%.1f%%) — the scan is not stopped early",
			limit.TotalCost, knnScan.TotalCost, ratio*100)
	}
	t.Logf("GREEN gate 2/3: Limit Plan Rows = %.0f (cap %d), root Plan Rows = %.0f, Limit cost %.2f of scan cost %.2f (%.3f%%)",
		limit.PlanRows, vw4Cap, greenPlan.PlanRows, limit.TotalCost, knnScan.TotalCost, ratio*100)

	// The GIN index has been there the whole time and stays useless for the
	// old predicate — recording that keeps the "why not just add `%`" question
	// answered inside the gate.
	stillRed := vw4ExplainJSON(t, ctx, pool, vw4Prev, q)
	if scan, ok := stillRed.find(vw4IsSeqScan); ok {
		t.Logf("for the record, 139's CTE keeps its %s (Plan Rows %.0f) even with %s present — a GIN trigram index carries `%%`/similarity, never `<->`",
			scan.NodeType, scan.PlanRows, vw4Index)
	}
}

// ---------------------------------------------------------------------------
// Gate 4a: set identity where at least `cap` rows clear the threshold
// ---------------------------------------------------------------------------

func TestVW4TrigramKNNSetIdentity(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 139)
	ctx := context.Background()

	vw4SeedGraded(t, ctx, pool, vw4NoHexQuery, vw4GradedRows, 5000)
	sims := vw4Similarities(t, ctx, pool, vw4NoHexQuery)
	if len(sims) < vw4Cap {
		t.Fatalf("only %d rows above the threshold, this branch needs at least %d", len(sims), vw4Cap)
	}
	vw4AssertStrictlyGraded(t, sims, vw4NoHexQuery)
	t.Logf("fixture: %d rows above the threshold, all similarities distinct (%.6f .. %.6f), 5000 noise rows at similarity 0",
		len(sims), sims[0], sims[len(sims)-1])

	before := vw4RunCTE(t, ctx, pool, vw4Prev, vw4NoHexQuery)
	vw4ApplyMigrations(t, ctx, pool)
	after := vw4RunCTE(t, ctx, pool, vw4New, vw4NoHexQuery)

	if len(before) != vw4Cap || len(after) != vw4Cap {
		t.Fatalf("cap branch: 139 delivered %d rows, 140 delivered %d, want %d each", len(before), len(after), vw4Cap)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("position %d differs: 139 = (%s, rank %d), 140 = (%s, rank %d)",
				i, before[i].ID, before[i].Rank, after[i].ID, after[i].Rank)
		}
	}
	t.Logf("gate 4a: with %d rows above the threshold, 139 and 140 deliver the identical %d ids in the identical rank order",
		len(sims), len(after))
}

// ---------------------------------------------------------------------------
// Gate 4b/4c: the post-filter, and the proof that it is load-bearing
// ---------------------------------------------------------------------------

func TestVW4TrigramKNNPostFilter(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 139)
	ctx := context.Background()

	vw4SeedGraded(t, ctx, pool, vw4SparseQuery, vw4SparseGraded, 5000)
	sims := vw4Similarities(t, ctx, pool, vw4SparseQuery)
	if len(sims) == 0 || len(sims) >= vw4Cap {
		t.Fatalf("the sparse fixture put %d rows above the threshold — it must be between 1 and %d", len(sims), vw4Cap-1)
	}
	vw4AssertStrictlyGraded(t, sims, vw4SparseQuery)

	before := vw4RunCTE(t, ctx, pool, vw4Prev, vw4SparseQuery)
	vw4ApplyMigrations(t, ctx, pool)
	after := vw4RunCTE(t, ctx, pool, vw4New, vw4SparseQuery)

	if len(after) != len(sims) {
		t.Fatalf("sparse branch: %d rows above the threshold but 140 delivered %d — the KNN block fetches %d and the post-filter must cut the rest",
			len(sims), len(after), vw4Cap)
	}
	if len(before) != len(after) {
		t.Errorf("sparse branch: 139 delivered %d rows, 140 delivered %d", len(before), len(after))
	}
	for i := range after {
		if i < len(before) && before[i] != after[i] {
			t.Errorf("sparse position %d differs: 139 = (%s, rank %d), 140 = (%s, rank %d)",
				i, before[i].ID, before[i].Rank, after[i].ID, after[i].Rank)
		}
		if after[i].Rank != int64(i+1) {
			t.Errorf("sparse position %d carries rank %d — ROW_NUMBER is evaluated after WHERE, so the ranks must be 1..k with no gap", i, after[i].Rank)
		}
	}
	t.Logf("gate 4b: %d rows above the threshold, 139 and 140 both deliver those %d with ranks 1..%d",
		len(sims), len(after), len(after))

	// Negative probe: the same CTE with the post-filter line removed.
	stmt, args := vw4CTE(t, vw4New, vw4SparseQuery, "WHERE similarity(knn.title, p_query)")
	var leaked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ("+stmt+") probe", args...).Scan(&leaked); err != nil {
		t.Fatalf("no-post-filter probe: %v", err)
	}
	if leaked <= len(after) {
		t.Fatalf("dropping the threshold post-filter returned %d rows, the filtered form returns %d — the post-filter is not load-bearing and gate 4b proves nothing",
			leaked, len(after))
	}
	t.Logf("gate 4c (negative): dropping `WHERE similarity(...) > threshold` lifts the arm from %d to %d rows — the surplus %d sit below the threshold and would enter the fusion",
		len(after), leaked, leaked-len(after))
}

// ---------------------------------------------------------------------------
// The index guard: pre-built index and the large-table branch
// ---------------------------------------------------------------------------

// TestVW4IndexGuard pins the two branches of 140's DO block that the runbook
// in its header promises but that a normal migration run never reaches.
//
// Both matter at the target scale: `CREATE INDEX` without CONCURRENTLY holds a
// SHARE lock on context_blocks for the whole build and RunMigrations sits in
// ctxd's boot path, so at 1M+ titles an unguarded build would block writers
// for the duration of a boot. The guard turns that into an operator decision.
func TestVW4IndexGuard(t *testing.T) {
	t.Run("pre-built index is a no-op", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 139)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			"CREATE INDEX "+vw4Index+" ON context_blocks USING GIST (title gist_trgm_ops)"); err != nil {
			t.Fatalf("pre-build index: %v", err)
		}
		var before uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", vw4Index).Scan(&before); err != nil {
			t.Fatalf("read index oid: %v", err)
		}

		vw4ApplyMigrations(t, ctx, pool)

		var after uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", vw4Index).Scan(&after); err != nil {
			t.Fatalf("read index oid after 140: %v", err)
		}
		if before != after {
			t.Errorf("140 replaced the pre-built index (oid %d -> %d) — an operator's out-of-band CONCURRENTLY build must survive the migration", before, after)
		}
		t.Logf("guard branch 1: %s pre-built out-of-band, 140 left oid %d untouched", vw4Index, after)
	})

	t.Run("large table warns instead of building", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 139)
		ctx := context.Background()

		// The guard reads reltuples. Faking it is the only way to reach the
		// >= 500 000 branch without seeding half a million rows, and it tests
		// exactly the predicate the guard evaluates.
		tag, err := pool.Exec(ctx, "UPDATE pg_class SET reltuples = 1000000 WHERE relname = 'context_blocks'")
		if err != nil {
			t.Skipf("cannot fake reltuples on this cluster (%v) — the large-table branch stays unproven here", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("faked reltuples on %d rows of pg_class, want 1", tag.RowsAffected())
		}

		vw4ApplyMigrations(t, ctx, pool)

		if am := vw4IndexAM(t, ctx, pool, vw4Index); am != "" {
			t.Errorf("140 built %s (%s) although the table reports 1000000 rows — the boot path would have blocked writers for the build", vw4Index, am)
		}
		// The migration must still LAND: the function bodies are the half that
		// works without the index, and a chain that stops here would take the
		// whole deploy with it. `< 140` rather than `!= 140` (V-W7, migration
		// 141): the claim is that 140 landed, and the chain keeps growing past
		// it — an equality here would go red for every later migration without
		// saying anything about this guard.
		var version int
		if err := pool.QueryRow(ctx, "SELECT max(version) FROM _migrations").Scan(&version); err != nil {
			t.Fatalf("read _migrations: %v", err)
		}
		if version < 140 {
			t.Fatalf("_migrations tops out at %d — 140 must land even when it declines to build the index", version)
		}
		t.Logf("guard branch 2: at a reported 1000000 rows 140 lands without %s; the arm keeps the pre-140 sequential scan until the operator builds it out-of-band", vw4Index)
	})
}

// ---------------------------------------------------------------------------
// The guard checks the DEFINITION, not the name (review V-W4, finding #1)
// ---------------------------------------------------------------------------

// vw4MigrateCapturingNotices completes the migration chain over a pool of its
// own making so the server's NOTICE/WARNING traffic is observable. testdb's
// pool has no notice handler, and the guard's only signal in the "name taken,
// index useless" case IS the warning — without capturing it the branch cannot
// be gated at all.
func vw4MigrateCapturingNotices(t *testing.T, ctx context.Context, dsn string) []string {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	var mu sync.Mutex
	var notices []string
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, n.Severity+": "+n.Message)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open notice-capturing pool: %v", err)
	}
	defer pool.Close()

	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT max(version) FROM _migrations").Scan(&version); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	// `< 140`, not `!= 140` — see the note in TestVW4IndexGuard (V-W7).
	if version < 140 {
		t.Fatalf("_migrations tops out at %d — 140 must land in every guard branch", version)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), notices...)
}

func vw4GuardWarnings(notices []string) []string {
	var out []string
	for _, n := range notices {
		if strings.HasPrefix(n, "WARNING") && strings.Contains(n, vw4Index) {
			out = append(out, n)
		}
	}
	return out
}

// TestVW4IndexGuardDefinition pins that the guard adopts a pre-built index
// only when that index can actually carry `ORDER BY title <-> …`.
//
// Reviewed finding #1: a name-only check accepts a foreign index of the same
// name — a GIN, an INVALID leftover of an aborted CONCURRENTLY build, a
// partial index, a different column — the migration then lands silently and
// the arm stays on the sequential scan this wave exists to remove. The guard
// must not fail the boot over it (115's rule: warn, never EXCEPTION), so the
// warning is the whole signal and is asserted here.
func TestVW4IndexGuardDefinition(t *testing.T) {
	t.Run("foreign GIN under the name warns and is not adopted", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 139)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			"CREATE INDEX "+vw4Index+" ON context_blocks USING GIN (title gin_trgm_ops)"); err != nil {
			t.Fatalf("pre-build the foreign GIN: %v", err)
		}

		warnings := vw4GuardWarnings(vw4MigrateCapturingNotices(t, ctx, dsn))
		if len(warnings) == 0 {
			t.Fatalf("140 landed on a %s that is a GIN without a single warning — the guard checks the name, not the definition, and the trigram arm is back on a sequential scan", vw4Index)
		}
		for _, w := range warnings {
			t.Logf("guard warning: %s", w)
		}
		if !strings.Contains(warnings[0], "amname=gin") {
			t.Errorf("the warning does not name the offending access method: %s", warnings[0])
		}
		// The migration must neither drop nor replace a foreign index: that is
		// the operator's object, and 140 has no business deleting it.
		if am := vw4IndexAM(t, ctx, pool, vw4Index); am != "gin" {
			t.Errorf("%s is %q after 140 — the guard must warn about a foreign index, never rewrite it", vw4Index, am)
		}
	})

	t.Run("correct GiST is adopted silently", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 139)
		ctx := context.Background()

		if _, err := pool.Exec(ctx,
			"CREATE INDEX "+vw4Index+" ON context_blocks USING GIST (title gist_trgm_ops)"); err != nil {
			t.Fatalf("pre-build the index: %v", err)
		}

		if w := vw4GuardWarnings(vw4MigrateCapturingNotices(t, ctx, dsn)); len(w) != 0 {
			t.Errorf("a correctly pre-built index must be adopted without a word, got: %v", w)
		}
		if am := vw4IndexAM(t, ctx, pool, vw4Index); am != "gist" {
			t.Errorf("%s is %q after 140, want \"gist\"", vw4Index, am)
		}
	})
}

// ---------------------------------------------------------------------------
// The inner tiebreak `, cb.id` (review V-W4, finding #2)
// ---------------------------------------------------------------------------

// vw4RunCTEUnder runs the lifted CTE inside a transaction with the given
// `SET LOCAL` statements applied, and returns the plan alongside the rows so a
// caller can prove that the perturbation actually moved the plan.
func vw4RunCTEUnder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, gucs ...string) ([]vw4Ranked, string) {
	t.Helper()
	stmt, args := vw4CTE(t, vw4New, query, "")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, g := range gucs {
		if _, err := tx.Exec(ctx, g); err != nil {
			t.Fatalf("%s: %v", g, err)
		}
	}
	var plan []string
	rows, err := tx.Query(ctx, "EXPLAIN "+stmt, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, line)
	}
	rows.Close()

	var out []vw4Ranked
	rows, err = tx.Query(ctx, stmt, args...)
	if err != nil {
		t.Fatalf("run CTE: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r vw4Ranked
		if err := rows.Scan(&r.ID, &r.Rank); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out = append(out, r)
	}
	return out, strings.Join(plan, "\n")
}

// TestVW4TrigramInnerTiebreak is the trigram-arm counterpart to migration
// 139's plan-perturbation gate (gen17_tiebreak_integration_test.go,
// bw1bSequenceDiff): with a similarity tie straddling the cap, the delivered
// ids must not depend on which plan the planner picks.
//
// The fixture gives 60 rows ONE identical title, so ranks 13..72 are a single
// exact tie group and the cap at 30 cuts through it. Ids come from
// gen_random_uuid() (v4), not the uuidv7 default, so heap order carries no
// information about id order — without `, cb.id` in the inner ORDER BY the two
// plans pick different members and the gate is red.
func TestVW4TrigramInnerTiebreak(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const q = "vw4 tiebreak anchor"

	if _, err := pool.Exec(ctx, `
		WITH f AS (SELECT string_agg(md5(k::text), ' ') AS filler FROM generate_series(1, 8) k)
		INSERT INTO context_blocks (id, category, title, content, scope, type_name)
		SELECT gen_random_uuid(), 'vw4tb' || s,
		       CASE WHEN s <= 12 THEN $1 || ' ' || left(f.filler, 10 + s)
		            ELSE $1 || ' ' || left(f.filler, 60) END,
		       'tiebreak ' || s, $2, $3
		FROM f, generate_series(1, 72) s`, q, vw4Scope, vw4Type); err != nil {
		t.Fatalf("seed tie fixture: %v", err)
	}
	vw4Analyze(t, ctx, pool)

	sims := vw4Similarities(t, ctx, pool, q)
	tie := 0
	for _, s := range sims {
		if s == sims[len(sims)-1] {
			tie++
		}
	}
	if len(sims) <= vw4Cap || tie < 2 || len(sims)-tie >= vw4Cap {
		t.Fatalf("fixture is vacuous: %d rows above the threshold, tie group of %d at the tail — the group must straddle rank %d", len(sims), tie, vw4Cap)
	}
	t.Logf("fixture: %d rows above the threshold, one exact tie group of %d spanning ranks %d..%d — the cap at %d cuts through it",
		len(sims), tie, len(sims)-tie+1, len(sims), vw4Cap)

	indexed, planA := vw4RunCTEUnder(t, ctx, pool, q)
	seq, planB := vw4RunCTEUnder(t, ctx, pool, q, "SET LOCAL enable_indexscan = off", "SET LOCAL enable_bitmapscan = off")

	if !strings.Contains(planA, "Index Scan using "+vw4Index) {
		t.Fatalf("plan A does not use the GiST index — the two plans are not two plans:\n%s", planA)
	}
	if strings.Contains(planB, "Index Scan using "+vw4Index) {
		t.Fatalf("plan B still uses the GiST index — the perturbation did not take:\n%s", planB)
	}
	if len(indexed) != vw4Cap || len(seq) != vw4Cap {
		t.Fatalf("plan A delivered %d rows, plan B %d, want %d each", len(indexed), len(seq), vw4Cap)
	}
	for i := range indexed {
		if indexed[i] != seq[i] {
			t.Errorf("position %d differs between plans: index scan = (%s, rank %d), sequential scan = (%s, rank %d) — the selection at the cap boundary is plan-dependent, which is exactly what the inner `, cb.id` is there to prevent",
				i, indexed[i].ID, indexed[i].Rank, seq[i].ID, seq[i].Rank)
		}
	}
	t.Logf("inner tiebreak: %d ids identical in identical rank order across an index-scan and a sequential-scan plan", len(indexed))
}

// ---------------------------------------------------------------------------
// Gate 5: parity between the two bodies, and its sensitivity
// ---------------------------------------------------------------------------

// vw4FusionMismatch runs the B-W1 query set and counts the delivered positions
// where ctx_rrf's score differs from the offline fusion of ctx_rrf_arms' ranks.
// Zero means the two bodies agree. `armsExtra` is appended to the
// ctx_rrf_arms call so a caller can push the sister off the live parameters on
// purpose.
func vw4FusionMismatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fx bw1Fixture, armsExtra string) (bad, positions int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	for qi, q := range bw1Queries() {
		args := bw1Args(q, bw1Embedding(qi), fx.granted, bw1Limit)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", q.name, err)
		}
		live, err := bw1CallRRF(ctx, tx, args)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: ctx_rrf: %v", q.name, err)
		}
		arms, err := bw1CallArms(ctx, tx, "ctx_rrf_arms", args, armsExtra)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: ctx_rrf_arms: %v", q.name, err)
		}
		_ = tx.Rollback(ctx)

		want := fuseArms(arms, armsLiveWeights, armsRRFK)
		n := bw1Limit
		if len(want) < n {
			n = len(want)
		}
		if len(live) != n {
			bad++
			positions += n
			continue
		}
		for i := 0; i < n; i++ {
			positions++
			if math.Abs(live[i].score-want[i].Score) > bw1Eps {
				bad++
			}
		}
	}
	return bad, positions
}

func vw4InstallBody(t *testing.T, ctx context.Context, pool *pgxpool.Pool, file string) {
	t.Helper()
	raw, err := migrations.Section(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := pool.Exec(ctx, string(raw)); err != nil {
		t.Fatalf("install %s: %v", file, err)
	}
}

func TestVW4TrigramKNNArmsParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if am := vw4IndexAM(t, ctx, pool, vw4Index); am != "gist" {
		t.Fatalf("%s is %q on a fully migrated database, want \"gist\"", vw4Index, am)
	}
	fx := bw1SeedCorpus(t, pool)

	// P1: after 140 both bodies carry the same trigram arm.
	bad, positions := vw4FusionMismatch(t, ctx, pool, fx, "")
	if bad != 0 {
		t.Errorf("fusion parity: %d of %d delivered positions disagree between ctx_rrf and the offline fusion of ctx_rrf_arms", bad, positions)
	}
	t.Logf("gate 5a: fusion parity holds on %d delivered positions across %d queries after 140 changed BOTH bodies",
		positions, len(bw1Queries()))

	// Negative probe A (deterministic): push ctx_rrf_arms' trigram arm off
	// ctx_rrf's cap. If the gate stayed green here it could not detect an
	// asymmetric change to this arm at all — which is the drift 137:76-83
	// names as the reason the parity test exists.
	const capExtra = ", p_cap_trigram => 5"
	bad, positions = vw4FusionMismatch(t, ctx, pool, fx, capExtra)
	if bad == 0 {
		t.Fatalf("with %s the sister sees a different trigram arm, yet all %d positions still agree — the parity gate is blind to the arm this wave changes",
			strings.TrimPrefix(capExtra, ", "), positions)
	}
	t.Logf("gate 5b (negative): %s makes %d of %d positions disagree — the gate is sensitive to the trigram arm",
		strings.TrimPrefix(capExtra, ", "), bad, positions)

	// Negative probe B (the one the brief names): leave 137's body in place
	// for ctx_rrf_arms while ctx_rrf carries 140's. The two forms are
	// set-identical wherever no similarity tie straddles the cap, so what
	// makes this probe red is the fixture's tie population — 220 blocks whose
	// titles share a fixed shape produce plenty of them, and the old form's
	// `ORDER BY similarity DESC` has no tiebreak to resolve them with.
	vw4InstallBody(t, ctx, pool, vw4Arms)
	bad, positions = vw4FusionMismatch(t, ctx, pool, fx, "")
	if bad == 0 {
		t.Errorf("ctx_rrf at 140 against ctx_rrf_arms at 137 agrees on all %d positions — an asymmetric wave would have shipped unnoticed on this fixture", positions)
	}
	t.Logf("gate 5c (negative): ctx_rrf at 140 against ctx_rrf_arms at 137 disagrees on %d of %d positions",
		bad, positions)

	// Restore, and prove the restore worked — a leaked 137 body would make
	// every later test in this binary measure the wrong function.
	vw4InstallBody(t, ctx, pool, vw4New)
	bad, positions = vw4FusionMismatch(t, ctx, pool, fx, "")
	if bad != 0 {
		t.Errorf("after restoring 140, %d of %d positions still disagree", bad, positions)
	}
}
