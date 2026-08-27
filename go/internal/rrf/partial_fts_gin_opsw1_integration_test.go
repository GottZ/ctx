//go:build integration

// OPS-W1 gates for migration 145_partial_fts_gin.sql (Achse 05 / D-05 F-22):
// the two FTS tsvector GIN indexes become PARTIAL over the hard deny-list
// `type_name NOT IN ('checkpoint','system-meta')`, and both retrieval functions
// carry the same conjunct so the planner can prove the implication.
//
// Staged inside ONE database per test, exactly like the V-W4 gate
// (trigram_knn_integration_test.go:6-11): every test starts at migration 144
// (SetupTestDBUpTo), records the RED state, runs the real migration runner to
// apply 145, and records the GREEN state against the SAME rows.
//
// The CTEs under test live inside plpgsql bodies and can therefore not be
// EXPLAINed through the function — `EXPLAIN SELECT * FROM ctx_rrf(...)` only
// ever shows `Function Scan on ctx_rrf`. Like V-W4 and the Generation 17 gate
// these tests lift the CTE text VERBATIM out of the migration file and
// substitute the p_* parameters, so the SQL that is planned and the SQL the
// function runs are the same characters. A drift in the migration drifts the
// test with it instead of past it.
//
//	go test -tags=integration ./internal/rrf/ -run TestOPSW1 -count=1 -v
package rrf_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	opsw1PrevRRF  = "140_trigram_gist_knn.sql" // ctx_rrf before this wave
	opsw1PrevArms = "142_arms_typename.sql"    // ctx_rrf_arms before this wave
	opsw1New      = "145_partial_fts_gin.sql"  // the wave

	opsw1IdxDe = "idx_context_ts_de"
	opsw1IdxEn = "idx_context_ts_en"

	opsw1Scope = "opsw1"

	// opsw1TargetPred is the normal form PostgreSQL prints for
	// `WHERE type_name NOT IN ('checkpoint','system-meta')`. Migration 145's DO
	// block compares against this exact string, and
	// TestOPSW1PredicateNormalForm pins it against a freshly built index — a PG
	// version that changes the normal form turns the test red instead of turning
	// the guard silent.
	opsw1TargetPred = `(type_name <> ALL (ARRAY['checkpoint'::text, 'system-meta'::text]))`

	// opsw1Needle appears in a small, fixed share of the fixture. Its whole job
	// is to make the FTS predicate the SELECTIVE one: without it the planner
	// picks a sequential scan for every variant and the plan gate is vacuous.
	opsw1Needle = "seltenwortxy"

	// opsw1BigRows is the fixture floor of the plan gate, mirroring V-W4's
	// vw4BigRows: below it a sequential scan is genuinely cheaper and the gate
	// would prove nothing.
	opsw1BigRows = 100000

	// opsw1MidRows is what the generic-plan probe needs — enough rows that the
	// FTS predicate is the selective one, not enough to pay the big fixture's
	// seed time twice.
	opsw1MidRows = 40000

	// opsw1SmallRows is the fixture of the set-identity and shadow gates: they
	// need enough NEEDLE hits per type to fill a 100-row arm, not size.
	opsw1SmallRows = 20000

	// opsw1TypeCycle is the modulus the fixture's type mix runs on: 7 of 13
	// residues are deny-listed, the rest split between visible and
	// shadow-measurable types. Prime, so any needle divisor that is not a
	// multiple of it spreads the needle evenly across every type.
	opsw1TypeCycle = 13

	// opsw1NeedleBig / opsw1NeedleMid are the needle divisors, both prime and
	// both different from opsw1TypeCycle. The big fixture needs a SELECTIVE
	// needle (a bitmap over 20 % of the heap is not what a plan gate is about);
	// the smaller fixtures need enough hits per type to fill an arm.
	opsw1NeedleBig = 499
	opsw1NeedleMid = 53
)

// ---------------------------------------------------------------------------
// Lifting the two FTS CTEs out of a migration file
// ---------------------------------------------------------------------------

// opsw1Subst maps the plpgsql parameter names occurring inside fulltext_de /
// fulltext_en to the SQL text they are replaced by. p_types_visible is the ONE
// name that stays a real placeholder — it is the parameter whose implication is
// under test. The longer names come first: substituting p_query before
// p_query_spaced would corrupt the latter.
var opsw1Subst = []struct{ name, repl string }{
	{"p_categories_exclude", "NULL::text[]"},
	{"p_granted_block_ids", "NULL::uuid[]"},
	{"p_types_visible", "@VISIBLE@"},
	{"p_types_exclude", "NULL::text[]"},
	{"p_query_spaced", "'" + opsw1Needle + "'::text"},
	{"p_cap_fts_de", "100"},
	{"p_cap_fts_en", "100"},
	{"p_query_or", "NULL::text"},
	{"p_temporal", "NULL::text"},
	{"p_category", "NULL::text"},
	{"p_scopes", "ARRAY['" + opsw1Scope + "']::text[]"},
	{"p_query", "'" + opsw1Needle + "'::text"},
	{"p_tags", "NULL::text[]"},
}

// opsw1Lang selects which of the two FTS CTEs is lifted.
type opsw1Lang struct {
	cte   string // "fulltext_de" / "fulltext_en"
	next  string // the CTE that terminates it in the file
	index string // the GIN index that arm depends on
}

var (
	opsw1De = opsw1Lang{cte: "fulltext_de", next: "fulltext_en", index: opsw1IdxDe}
	opsw1En = opsw1Lang{cte: "fulltext_en", next: "trigram_title", index: opsw1IdxEn}
)

// opsw1CTE extracts one FTS CTE from a migration file and wraps it into a
// standalone statement. `occurrence` picks the function: 0 = ctx_rrf, 1 =
// ctx_rrf_arms (in 145 both live in one file, in the pre-wave state they live in
// two different files and occurrence is always 0). `strip` optionally removes
// the one line containing it — the negative probe uses that to delete the
// static deny-list conjunct. `visible` is the SQL text p_types_visible becomes.
func opsw1CTE(t *testing.T, file string, lang opsw1Lang, occurrence int, strip, visible string) string {
	t.Helper()
	raw, err := migrations.Section(file)
	if err != nil {
		t.Fatalf("read embedded %s: %v", file, err)
	}
	body := string(raw)
	open := "    " + lang.cte + " AS (\n"
	closeMark := "\n    ),\n    " + lang.next + " AS ("

	from := 0
	for n := 0; n <= occurrence; n++ {
		i := strings.Index(body[from:], open)
		if i < 0 {
			t.Fatalf("%s: no %s CTE at occurrence %d — migration drifted?", file, lang.cte, n)
		}
		from += i + len(open)
	}
	j := strings.Index(body[from:], closeMark)
	if j < 0 {
		t.Fatalf("%s: %s CTE has no %s terminator — migration drifted?", file, lang.cte, lang.next)
	}
	cte := body[from : from+j]

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
			t.Fatalf("%s/%s: strip %q matched %d lines, want exactly 1", file, lang.cte, strip, hits)
		}
		cte = strings.Join(kept, "\n")
	}

	stmt := "SELECT * FROM (\n" + cte + "\n) " + lang.cte + " ORDER BY rank"
	for _, s := range opsw1Subst {
		stmt = strings.ReplaceAll(stmt, s.name, s.repl)
	}
	stmt = strings.ReplaceAll(stmt, "@VISIBLE@", visible)
	if strings.Contains(stmt, "p_") {
		t.Fatalf("%s/%s: unsubstituted parameter left in the lifted CTE:\n%s", file, lang.cte, stmt)
	}
	return stmt
}

// opsw1VisibleLiteral renders a type list as a SQL array literal — the CUSTOM
// plan case, where the planner sees constants and can fold them.
func opsw1VisibleLiteral(types []string) string {
	quoted := make([]string, len(types))
	for i, tn := range types {
		quoted[i] = "'" + tn + "'"
	}
	return "ARRAY[" + strings.Join(quoted, ",") + "]::text[]"
}

// ---------------------------------------------------------------------------
// Plan inspection
// ---------------------------------------------------------------------------

type opsw1Node struct {
	NodeType  string      `json:"Node Type"`
	IndexName string      `json:"Index Name"`
	PlanRows  float64     `json:"Plan Rows"`
	TotalCost float64     `json:"Total Cost"`
	RelName   string      `json:"Relation Name"`
	Plans     []opsw1Node `json:"Plans"`
}

func (n opsw1Node) find(pred func(opsw1Node) bool) (opsw1Node, bool) {
	if pred(n) {
		return n, true
	}
	for _, c := range n.Plans {
		if hit, ok := c.find(pred); ok {
			return hit, true
		}
	}
	return opsw1Node{}, false
}

func opsw1IsSeqScan(n opsw1Node) bool {
	return strings.Contains(n.NodeType, "Seq Scan") && n.RelName == "context_blocks"
}

func opsw1ExplainText(t *testing.T, ctx context.Context, q pgxQuerier, stmt string) string {
	t.Helper()
	rows, err := q.Query(ctx, "EXPLAIN "+stmt)
	if err != nil {
		t.Fatalf("EXPLAIN: %v\nstatement:\n%s", err, stmt)
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

func opsw1ExplainJSON(t *testing.T, ctx context.Context, q pgxQuerier, stmt string) opsw1Node {
	t.Helper()
	var raw []byte
	if err := q.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+stmt).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN JSON: %v\nstatement:\n%s", err, stmt)
	}
	var wrapper []struct {
		Plan opsw1Node `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("decode EXPLAIN JSON: %v", err)
	}
	if len(wrapper) != 1 {
		t.Fatalf("EXPLAIN JSON returned %d plans, want 1", len(wrapper))
	}
	return wrapper[0].Plan
}

// pgxQuerier is the small slice of pgxpool.Pool / pgx.Tx these helpers need —
// the generic-plan probes have to run inside ONE transaction (PREPARE plus
// SET LOCAL plan_cache_mode), the rest runs on the pool.
type pgxQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// opsw1Seed writes n rows carrying all four classes this wave reasons about —
// visible types, the two deny-listed ones, and the two shadow-measurable
// derivative types — with the needle on every needleEvery-th row.
//
// The type is derived from `s % opsw1TypeCycle` and the needle from
// `s % needleEvery`, with BOTH moduli coprime. That is not cosmetic: the first
// draft assigned checkpoint on `s % 2 = 0` and the needle on `s % 500 = 0`, so
// every single needle row was a checkpoint and the arms came back empty after
// the wave — a gate that compares two empty sets and passes. Coprime moduli
// spread the needle evenly across every type.
func opsw1Seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n, needleEvery int) time.Duration {
	t.Helper()
	if needleEvery%opsw1TypeCycle == 0 || opsw1TypeCycle%needleEvery == 0 {
		t.Fatalf("needleEvery %d and the type cycle %d are not coprime — the needle would land on a type subset", needleEvery, opsw1TypeCycle)
	}
	start := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, type_name)
		SELECT 'opsw1',
		       'opsw1 block ' || s,
		       'opsw1 fixture body ' || s || ' ' || md5(s::text)
		           || CASE WHEN s % $3 = 0 THEN ' `+opsw1Needle+`' ELSE '' END,
		       $1,
		       CASE s % `+fmt.Sprint(opsw1TypeCycle)+`
		           WHEN 0 THEN 'checkpoint'
		           WHEN 1 THEN 'checkpoint'
		           WHEN 2 THEN 'checkpoint'
		           WHEN 3 THEN 'checkpoint'
		           WHEN 4 THEN 'checkpoint'
		           WHEN 5 THEN 'checkpoint'
		           WHEN 6 THEN 'system-meta'
		           WHEN 7 THEN 'catalog'
		           WHEN 8 THEN 'insight'
		           WHEN 9 THEN 'reference'
		           ELSE 'knowledge'
		       END
		FROM generate_series(1, $2) s`, opsw1Scope, n, needleEvery); err != nil {
		t.Fatalf("seed %d rows: %v", n, err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// The fixture claim, verified rather than assumed: the needle has to reach
	// visible AND deny-listed AND shadow-measurable rows, or the gates below
	// compare subsets of nothing.
	var vis, deny, shadow int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE type_name IN ('knowledge','reference')),
		       count(*) FILTER (WHERE type_name IN ('checkpoint','system-meta')),
		       count(*) FILTER (WHERE type_name IN ('catalog','insight'))
		  FROM context_blocks
		 WHERE content LIKE '%`+opsw1Needle+`%'`).Scan(&vis, &deny, &shadow); err != nil {
		t.Fatalf("needle census: %v", err)
	}
	if vis == 0 || deny == 0 || shadow == 0 {
		t.Fatalf("needle census: %d visible, %d deny-listed, %d shadow-measurable rows — every class must be represented", vis, deny, shadow)
	}
	t.Logf("fixture: %d rows, needle on %d visible / %d deny-listed / %d shadow-measurable rows, seeded + ANALYZE in %s",
		n, vis, deny, shadow, time.Since(start).Round(time.Millisecond))
	return time.Since(start)
}

func opsw1ApplyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Duration {
	t.Helper()
	start := time.Now()
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migration %s: %v", opsw1New, err)
	}
	d := time.Since(start)
	var version int
	if err := pool.QueryRow(ctx, "SELECT max(version) FROM _migrations").Scan(&version); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if version < 145 {
		t.Fatalf("migration chain stopped at %d — 145 did not land", version)
	}
	return d
}

type opsw1IndexState struct {
	AM      string
	Valid   bool
	Pred    *string
	Def     string
	SizeKiB int64
}

func opsw1Index(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) (opsw1IndexState, bool) {
	t.Helper()
	var st opsw1IndexState
	err := pool.QueryRow(ctx, `
		SELECT am.amname, i.indisvalid, pg_get_expr(i.indpred, i.indrelid),
		       pg_get_indexdef(c.oid), pg_relation_size(c.oid) / 1024
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_am        am ON am.oid = c.relam
		  JOIN pg_index      i ON i.indexrelid = c.oid
		 WHERE n.nspname = 'public' AND c.relname = $1`, name).
		Scan(&st.AM, &st.Valid, &st.Pred, &st.Def, &st.SizeKiB)
	if err != nil {
		return opsw1IndexState{}, false
	}
	return st, true
}

func opsw1PredOf(st opsw1IndexState) string {
	if st.Pred == nil {
		return "(keins)"
	}
	return *st.Pred
}

// ---------------------------------------------------------------------------
// Gate 1 + 2 + 5: red plan (full GIN), green plan (partial GIN), sizes
// ---------------------------------------------------------------------------

func TestOPSW1PlanShapeAndSize(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	seedFor := opsw1Seed(t, ctx, pool, opsw1BigRows, opsw1NeedleBig)
	var seeded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM context_blocks WHERE scope = $1", opsw1Scope).Scan(&seeded); err != nil {
		t.Fatalf("count fixture: %v", err)
	}
	if seeded < opsw1BigRows {
		t.Fatalf("fixture has %d rows, the gate requires >= %d", seeded, opsw1BigRows)
	}
	t.Logf("fixture: %d rows seeded + ANALYZE in %s", seeded, seedFor.Round(time.Millisecond))

	visible := opsw1VisibleLiteral([]string{"knowledge", "reference"})

	// --- RED: both indexes are FULL, and the plan says so --------------------
	for _, name := range []string{opsw1IdxDe, opsw1IdxEn} {
		st, ok := opsw1Index(t, ctx, pool, name)
		if !ok {
			t.Fatalf("%s does not exist at migration 144 — the red state is not the red state", name)
		}
		if st.Pred != nil {
			t.Fatalf("%s is ALREADY partial at 144 (%s) — the red state is not the red state", name, *st.Pred)
		}
		t.Logf("RED gate 1/5: %s is %s, valid=%v, predicate=%s, %d KiB — %s",
			name, st.AM, st.Valid, opsw1PredOf(st), st.SizeKiB, st.Def)
	}
	redDeKiB, _ := opsw1Index(t, ctx, pool, opsw1IdxDe)
	redEnKiB, _ := opsw1Index(t, ctx, pool, opsw1IdxEn)

	for _, c := range []struct {
		file string
		lang opsw1Lang
	}{
		{opsw1PrevRRF, opsw1De}, {opsw1PrevRRF, opsw1En},
		{opsw1PrevArms, opsw1De}, {opsw1PrevArms, opsw1En},
	} {
		stmt := opsw1CTE(t, c.file, c.lang, 0, "", visible)
		text := opsw1ExplainText(t, ctx, pool, stmt)
		if !strings.Contains(text, "Bitmap Index Scan on "+c.lang.index) {
			t.Fatalf("RED plan of %s/%s does not use the FULL %s:\n%s", c.file, c.lang.cte, c.lang.index, text)
		}
		if strings.Contains(text, "type_name <> ALL") {
			t.Fatalf("RED plan of %s/%s already carries a deny-list recheck — 144 has no partial index:\n%s",
				c.file, c.lang.cte, text)
		}
		t.Logf("RED gate 1 (%s/%s):\n%s", c.file, c.lang.cte, text)
	}

	// --- build 145 -----------------------------------------------------------
	migFor := opsw1ApplyMigrations(t, ctx, pool)

	greenDe, ok := opsw1Index(t, ctx, pool, opsw1IdxDe)
	if !ok {
		t.Fatalf("%s vanished with 145", opsw1IdxDe)
	}
	greenEn, ok := opsw1Index(t, ctx, pool, opsw1IdxEn)
	if !ok {
		t.Fatalf("%s vanished with 145", opsw1IdxEn)
	}
	for name, st := range map[string]opsw1IndexState{opsw1IdxDe: greenDe, opsw1IdxEn: greenEn} {
		if st.AM != "gin" {
			t.Errorf("%s has access method %q after 145, want \"gin\"", name, st.AM)
		}
		if !st.Valid {
			t.Errorf("%s is INVALID after 145", name)
		}
		if st.Pred == nil || *st.Pred != opsw1TargetPred {
			t.Fatalf("%s predicate after 145 = %s, want %s", name, opsw1PredOf(st), opsw1TargetPred)
		}
	}
	t.Logf("migration 145 applied in %s", migFor.Round(time.Millisecond))

	// --- GATE 5: sizes -------------------------------------------------------
	var denyRows, visRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE type_name IN ('checkpoint','system-meta')), count(*)
		  FROM context_blocks`).Scan(&denyRows, &visRows); err != nil {
		t.Fatalf("row census: %v", err)
	}
	// This fixture is ROW-proportional, not live-shaped: every row carries about
	// the same amount of text, so the shrink tracks the row share (~50 %). The
	// live corpus is lopsided in TEXT mass (97,2 % of it is checkpoint) —
	// TestOPSW1LiveShapedIndexSize measures that shape.
	t.Logf("GREEN gate 5 (row-proportional fixture): %s %d KiB -> %d KiB (%.1f %%), %s %d KiB -> %d KiB (%.1f %%); "+
		"%d of %d rows carry a deny-listed type (%.1f %%)",
		opsw1IdxDe, redDeKiB.SizeKiB, greenDe.SizeKiB, 100*float64(greenDe.SizeKiB)/float64(redDeKiB.SizeKiB),
		opsw1IdxEn, redEnKiB.SizeKiB, greenEn.SizeKiB, 100*float64(greenEn.SizeKiB)/float64(redEnKiB.SizeKiB),
		denyRows, visRows, 100*float64(denyRows)/float64(visRows))
	if greenDe.SizeKiB >= redDeKiB.SizeKiB {
		t.Errorf("%s did not shrink: %d KiB -> %d KiB", opsw1IdxDe, redDeKiB.SizeKiB, greenDe.SizeKiB)
	}
	if greenEn.SizeKiB >= redEnKiB.SizeKiB {
		t.Errorf("%s did not shrink: %d KiB -> %d KiB", opsw1IdxEn, redEnKiB.SizeKiB, greenEn.SizeKiB)
	}

	// --- GATE 2: the partial index is used, in both languages, both functions -
	for _, c := range []struct {
		occ  int
		name string
		lang opsw1Lang
	}{
		{0, "ctx_rrf", opsw1De}, {0, "ctx_rrf", opsw1En},
		{1, "ctx_rrf_arms", opsw1De}, {1, "ctx_rrf_arms", opsw1En},
	} {
		stmt := opsw1CTE(t, opsw1New, c.lang, c.occ, "", visible)
		text := opsw1ExplainText(t, ctx, pool, stmt)
		if !strings.Contains(text, "Bitmap Index Scan on "+c.lang.index) {
			t.Fatalf("GREEN plan of %s/%s does not use %s:\n%s", c.name, c.lang.cte, c.lang.index, text)
		}
		if !strings.Contains(text, "type_name <> ALL") {
			t.Fatalf("GREEN plan of %s/%s carries no deny-list recheck — the index in the plan is not the PARTIAL one:\n%s",
				c.name, c.lang.cte, text)
		}
		if _, bad := opsw1ExplainJSON(t, ctx, pool, stmt).find(opsw1IsSeqScan); bad {
			t.Errorf("GREEN plan of %s/%s still contains a sequential scan on context_blocks:\n%s", c.name, c.lang.cte, text)
		}
		t.Logf("GREEN gate 2 (%s/%s):\n%s", c.name, c.lang.cte, text)
	}
}

// ---------------------------------------------------------------------------
// Gate 5: the size, measured on a LIVE-SHAPED fixture
// ---------------------------------------------------------------------------

// opsw1LiveShape mirrors the live census of 2026-08-27 (context_store), scaled
// down by ten in ROW count while keeping each type's characteristic text size —
// which is what a tsvector GIN index actually costs. Live:
//
//	checkpoint    5 961 rows   185 MB   97,2 %
//	knowledge     1 425 rows   4,2 MB    2,2 %
//	audit-trail     289 rows   760 kB    0,4 %
//	reference       134 rows   304 kB    0,2 %
//	system-meta      25 rows    71 kB    0,0 %
var opsw1LiveShape = []struct {
	typeName string
	rows     int
	textKiB  int
}{
	{"checkpoint", 596, 31},
	{"knowledge", 142, 3},
	{"audit-trail", 29, 3},
	{"reference", 13, 2},
	{"system-meta", 3, 3},
}

// TestOPSW1LiveShapedIndexSize is gate 5. The row-proportional fixture of the
// plan gate cannot show the real effect: the deny-listed types are expensive
// because of their TEXT, not their row count.
func TestOPSW1LiveShapedIndexSize(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	for _, s := range opsw1LiveShape {
		// repeat() of a 32-char md5 gets deduplicated by to_tsvector into one
		// lexeme, so the body is built from DISTINCT digests — that is what makes
		// the tsvector, and therefore the index entry, actually large.
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_blocks (category, title, content, scope, type_name)
			SELECT 'opsw1', 'opsw1 ' || $1 || ' ' || s,
			       (SELECT string_agg(md5((s * 1000003 + k)::text), ' ')
			          FROM generate_series(1, $2::int * 1024 / 33) k),
			       $3, $1
			FROM generate_series(1, $4::int) s`, s.typeName, s.textKiB, opsw1Scope, s.rows); err != nil {
			t.Fatalf("seed %s: %v", s.typeName, err)
		}
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var denyText, allText, denyRows, allRows int64
	if err := pool.QueryRow(ctx, `
		SELECT coalesce(sum(length(coalesce(title,'') || ' ' || content))
		         FILTER (WHERE type_name IN ('checkpoint','system-meta')), 0),
		       coalesce(sum(length(coalesce(title,'') || ' ' || content)), 0),
		       count(*) FILTER (WHERE type_name IN ('checkpoint','system-meta')),
		       count(*)
		  FROM context_blocks`).Scan(&denyText, &allText, &denyRows, &allRows); err != nil {
		t.Fatalf("text census: %v", err)
	}
	textShare := 100 * float64(denyText) / float64(allText)
	if textShare < 90 {
		t.Fatalf("the fixture puts only %.1f %% of the text mass on the deny-listed types — live it is 97,2 %%, so this is not the live shape", textShare)
	}

	redDe, _ := opsw1Index(t, ctx, pool, opsw1IdxDe)
	redEn, _ := opsw1Index(t, ctx, pool, opsw1IdxEn)

	migFor := opsw1ApplyMigrations(t, ctx, pool)

	greenDe, _ := opsw1Index(t, ctx, pool, opsw1IdxDe)
	greenEn, _ := opsw1Index(t, ctx, pool, opsw1IdxEn)

	for name, pair := range map[string][2]opsw1IndexState{
		opsw1IdxDe: {redDe, greenDe},
		opsw1IdxEn: {redEn, greenEn},
	} {
		red, green := pair[0], pair[1]
		if green.Pred == nil || *green.Pred != opsw1TargetPred {
			t.Fatalf("%s is not the target index after 145: %s", name, opsw1PredOf(green))
		}
		saved := 100 * (1 - float64(green.SizeKiB)/float64(red.SizeKiB))
		t.Logf("gate 5 (live-shaped): %s %d KiB -> %d KiB, %.1f %% saved", name, red.SizeKiB, green.SizeKiB, saved)
		if saved < 80 {
			t.Errorf("%s only shrank by %.1f %% while the deny-listed types carry %.1f %% of the text mass — F-22's premise does not hold on this shape",
				name, saved, textShare)
		}
	}
	t.Logf("gate 5 fixture: %d of %d rows deny-listed (%.1f %%), %.1f %% of the text mass; migration ran in %s",
		denyRows, allRows, 100*float64(denyRows)/float64(allRows), textShare, migFor.Round(time.Millisecond))
}

// ---------------------------------------------------------------------------
// Gate 2b: the implication is what carries the index — the generic-plan proof
// ---------------------------------------------------------------------------

// TestOPSW1ImplicationIsLoadBearing is the negative probe of gate 2: with the
// static conjunct removed, the SAME CTE stops using the partial FTS index.
//
// The probe has to force a GENERIC plan to show it, and that is the finding this
// gate exists to record: as long as the plan cache serves a CUSTOM plan,
// PostgreSQL folds p_types_visible into a constant array and proves the
// implication ITSELF — the partial index is used even without the conjunct (the
// "for the record" log line below shows exactly that). The moment the cache
// switches to the generic plan (possible from the 6th execution per connection,
// plancache.c choose_custom_plan), p_types_visible stays a Param, no proof is
// possible, and the FTS index drops out of the plan. Without the static conjunct
// the index usage would therefore hang on a per-connection cache decision —
// visible in production as a latency outlier, invisible in a single EXPLAIN.
//
// What the stripped form falls back to is NOT necessarily a sequential scan:
// on this schema the planner reaches for idx_blocks_type_name and demotes the
// FTS predicate to a heap Filter. The load-bearing assertion is therefore
// "the FTS index is absent from the plan", plus the cost ratio between the two
// forms — a sequential scan is one possible fallback, not the claim.
func TestOPSW1ImplicationIsLoadBearing(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	opsw1Seed(t, ctx, pool, opsw1MidRows, opsw1NeedleMid)
	opsw1ApplyMigrations(t, ctx, pool)

	const conjunct = "AND cb.type_name NOT IN ('checkpoint','system-meta')"

	for _, lang := range []opsw1Lang{opsw1De, opsw1En} {
		withConj := opsw1CTE(t, opsw1New, lang, 0, "", "$1::text[]")
		withoutConj := opsw1CTE(t, opsw1New, lang, 0, conjunct, "$1::text[]")
		if withConj == withoutConj {
			t.Fatalf("%s: stripping %q changed nothing — the gate would prove nothing", lang.cte, conjunct)
		}
		opsw1ImplicationProbe(t, ctx, pool, lang, withConj, withoutConj)
	}
}

// opsw1ImplicationProbe runs one language's generic-plan probe in its own
// transaction. It is a function rather than a loop body so the rollback is a
// defer: a t.Fatalf with an open transaction would leave the pool's connection
// checked out and deadlock the pool's Close in the test cleanup (observed as a
// 10-minute package timeout while building this gate).
func opsw1ImplicationProbe(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lang opsw1Lang, withConj, withoutConj string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Statement names carry the language: PREPARE lives in the SESSION, and a
	// pooled connection can outlive the transaction that created it.
	nameWith := "opsw1_with_" + lang.cte
	nameWithout := "opsw1_without_" + lang.cte

	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plan: %v", err)
	}
	if _, err := tx.Exec(ctx, "PREPARE "+nameWith+"(text[]) AS "+withConj); err != nil {
		t.Fatalf("prepare with-conjunct: %v", err)
	}
	if _, err := tx.Exec(ctx, "PREPARE "+nameWithout+"(text[]) AS "+withoutConj); err != nil {
		t.Fatalf("prepare without-conjunct: %v", err)
	}

	redStmt := "EXECUTE " + nameWithout + "(ARRAY['knowledge','reference'])"
	greenStmt := "EXECUTE " + nameWith + "(ARRAY['knowledge','reference'])"
	redText := opsw1ExplainText(t, ctx, tx, redStmt)
	greenText := opsw1ExplainText(t, ctx, tx, greenStmt)
	redCost := opsw1ExplainJSON(t, ctx, tx, redStmt).TotalCost
	greenCost := opsw1ExplainJSON(t, ctx, tx, greenStmt).TotalCost

	if strings.Contains(redText, lang.index) {
		t.Errorf("%s WITHOUT the static conjunct still names %s under a generic plan — the conjunct is not load-bearing and gate 2 proves nothing:\n%s",
			lang.cte, lang.index, redText)
	}
	if !strings.Contains(greenText, "Bitmap Index Scan on "+lang.index) {
		t.Errorf("%s WITH the static conjunct does not use %s under a generic plan:\n%s", lang.cte, lang.index, greenText)
	}
	if !strings.Contains(greenText, "type_name <> ALL") {
		t.Errorf("%s: the green plan carries no deny-list recheck — the index it uses is not the PARTIAL one:\n%s", lang.cte, greenText)
	}
	if redCost <= greenCost {
		t.Errorf("%s: the stripped form is estimated at %.2f and the conjunct form at %.2f — without a cost gap the conjunct buys nothing",
			lang.cte, redCost, greenCost)
	}
	t.Logf("gate 2b (%s, force_generic_plan), total cost %.2f -> %.2f (%.1fx)\n  RED   (conjunct stripped):\n%s\n  GREEN (conjunct present):\n%s",
		lang.cte, redCost, greenCost, redCost/greenCost, redText, greenText)

	// For the record: under a CUSTOM plan the planner proves the implication on
	// its own, so the stripped form ALSO uses the index. That is exactly why the
	// generic plan is the honest probe — and why this is a log line, not a gate.
	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_custom_plan"); err != nil {
		t.Fatalf("force custom plan: %v", err)
	}
	customText := opsw1ExplainText(t, ctx, tx, redStmt)
	t.Logf("gate 2b, for the record (%s, force_custom_plan, conjunct stripped): %s in plan = %v",
		lang.cte, lang.index, strings.Contains(customText, lang.index))
}

// ---------------------------------------------------------------------------
// Gate 3: set identity across the wave
// ---------------------------------------------------------------------------

func opsw1IDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmt string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, stmt)
	if err != nil {
		t.Fatalf("run CTE: %v\nstatement:\n%s", err, stmt)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		var rank int64
		if err := rows.Scan(&id, &rank); err != nil {
			t.Fatalf("scan CTE row: %v", err)
		}
		out = append(out, fmt.Sprintf("%s#%d", id, rank))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("CTE rows: %v", err)
	}
	return out
}

// TestOPSW1SetIdentity is gate 3. On a fixture carrying visible, deny-listed
// AND shadow-measurable types, both FTS arms deliver the identical id/rank
// sequence before and after the wave — for every type list that a real caller
// can produce.
//
// "Can produce" is the whole content of the no-op claim, and it has two halves:
// the production list is Set.VisibleTypes(), which never contains an excluded
// type (blocktype/set.go:92-95); the measurement list is that same slice widened
// by shadow types, and checkpoint/system-meta can never be among them
// (handler/query_shadow.go:50-53). Both halves are pinned in Go by
// TestOPSW1DenyTypesAreNeverVisible and TestOPSW1ShadowDenyIsFailClosed
// (internal/handler); this test is the SQL half.
func TestOPSW1SetIdentity(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	// 20k rows keeps the arms full (both LIMIT 100) while staying quick.
	opsw1Seed(t, ctx, pool, opsw1SmallRows, opsw1NeedleMid)

	lists := map[string][]string{
		"production":        {"knowledge", "reference", "audit-trail"},
		"shadow-catalog":    {"knowledge", "reference", "audit-trail", "catalog"},
		"shadow-both":       {"knowledge", "reference", "audit-trail", "catalog", "insight"},
		"single-visible":    {"knowledge"},
		"only-shadow-types": {"catalog", "insight"},
	}

	type probe struct {
		label string
		prev  string
		occ   int
		lang  opsw1Lang
	}
	probes := []probe{
		{"ctx_rrf/de", opsw1PrevRRF, 0, opsw1De},
		{"ctx_rrf/en", opsw1PrevRRF, 0, opsw1En},
		{"ctx_rrf_arms/de", opsw1PrevArms, 1, opsw1De},
		{"ctx_rrf_arms/en", opsw1PrevArms, 1, opsw1En},
	}

	before := map[string][]string{}
	for name, types := range lists {
		vis := opsw1VisibleLiteral(types)
		for _, p := range probes {
			key := name + " " + p.label
			before[key] = opsw1IDs(t, ctx, pool, opsw1CTE(t, p.prev, p.lang, 0, "", vis))
		}
	}

	opsw1ApplyMigrations(t, ctx, pool)

	nonEmpty := 0
	for name, types := range lists {
		vis := opsw1VisibleLiteral(types)
		for _, p := range probes {
			key := name + " " + p.label
			after := opsw1IDs(t, ctx, pool, opsw1CTE(t, opsw1New, p.lang, p.occ, "", vis))
			if len(before[key]) != len(after) {
				t.Errorf("%s: 144 delivered %d rows, 145 delivered %d", key, len(before[key]), len(after))
				continue
			}
			for i := range after {
				if before[key][i] != after[i] {
					t.Errorf("%s position %d differs: 144 = %s, 145 = %s", key, i, before[key][i], after[i])
				}
			}
			if len(after) > 0 {
				nonEmpty++
			}
			t.Logf("gate 3 (%s): %d ids, identical before and after", key, len(after))
		}
	}
	if nonEmpty < len(lists)*len(probes) {
		t.Fatalf("only %d of %d probes returned rows — an empty arm compares equal to an empty arm and proves nothing",
			nonEmpty, len(lists)*len(probes))
	}
}

// ---------------------------------------------------------------------------
// Gate 4: the shadow probe — the negative probe of the deny-list CHOICE
// ---------------------------------------------------------------------------

// TestOPSW1ShadowTypeStaysFindable is gate 4, the probe that makes the
// design decision ("hard deny-list, NOT all excluded types") load-bearing:
//
//   - a `catalog` block — retrieval.policy = excluded, shadow_measurable — must
//     still be findable through the FTS arm once the measurement seam widens
//     p_types_visible with it;
//   - a scratch variant of the migration whose index predicate excludes ALL
//     excluded types makes exactly that probe RED.
//
// Without the second half the first would only show that today's predicate
// happens to work, not that the narrower one was NECESSARY.
func TestOPSW1ShadowTypeStaysFindable(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	opsw1Seed(t, ctx, pool, opsw1SmallRows, opsw1NeedleMid)
	opsw1ApplyMigrations(t, ctx, pool)

	// measureVisibleTypesFor(visible, ["catalog","insight"]) — the seam's own
	// output shape (handler/query_shadow.go:80-91): the production list plus the
	// shadow names, deduplicated.
	measure := opsw1VisibleLiteral([]string{"knowledge", "reference", "audit-trail", "catalog", "insight"})

	for _, lang := range []opsw1Lang{opsw1De, opsw1En} {
		stmt := opsw1CTE(t, opsw1New, lang, 0, "", measure)
		ids := opsw1IDs(t, ctx, pool, stmt)
		if len(ids) == 0 {
			t.Fatalf("%s: the widened arm returned nothing — the fixture cannot prove anything", lang.cte)
		}

		var shadowHits int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (`+stmt+`) arm
			  JOIN context_blocks cb ON cb.id = arm.id
			 WHERE cb.type_name IN ('catalog','insight')`).Scan(&shadowHits); err != nil {
			t.Fatalf("count shadow hits: %v", err)
		}
		if shadowHits == 0 {
			t.Fatalf("%s: not one catalog/insight block came back through the widened arm — the partial index dropped the shadow types",
				lang.cte)
		}
		t.Logf("gate 4 (%s): %d of %d arm rows are shadow-measurable types (catalog/insight)", lang.cte, shadowHits, len(ids))

		// The plan has to be the PARTIAL index for this to say anything about
		// the shipped state: a sequential scan would find the shadow rows too.
		text := opsw1ExplainText(t, ctx, pool, stmt)
		if !strings.Contains(text, "Bitmap Index Scan on "+lang.index) {
			t.Fatalf("%s: the widened arm did not use %s, so gate 4 says nothing about the partial index:\n%s",
				lang.cte, lang.index, text)
		}
	}
}

// TestOPSW1AllExcludedPredicateBreaksTheShadowArm is the second half of gate 4,
// run as its own test because it needs a database whose migration 145 carries a
// DIFFERENT predicate. It installs the scratch variant by hand — the same DROP +
// CREATE the migration does, with 'catalog' and 'insight' added to the
// deny-list — and shows the widened arm loses exactly those rows.
func TestOPSW1AllExcludedPredicateBreaksTheShadowArm(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	opsw1Seed(t, ctx, pool, opsw1SmallRows, opsw1NeedleMid)
	opsw1ApplyMigrations(t, ctx, pool)

	measure := opsw1VisibleLiteral([]string{"knowledge", "reference", "audit-trail", "catalog", "insight"})

	for _, lang := range []opsw1Lang{opsw1De, opsw1En} {
		col := "ts_de"
		if lang.cte == "fulltext_en" {
			col = "ts_en"
		}
		stmt := opsw1CTE(t, opsw1New, lang, 0, "", measure)

		var shipped int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (`+stmt+`) arm
			  JOIN context_blocks cb ON cb.id = arm.id
			 WHERE cb.type_name IN ('catalog','insight')`).Scan(&shipped); err != nil {
			t.Fatalf("count shadow hits (shipped predicate): %v", err)
		}
		if shipped == 0 {
			t.Fatalf("%s: the shipped predicate already delivers no shadow rows — nothing to break", lang.cte)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		// The scratch variant of the wave: "alle excluded" instead of the hard
		// deny-list. Rolled back at the end of the loop body, so the shipped
		// state survives.
		if _, err := tx.Exec(ctx, "DROP INDEX "+lang.index); err != nil {
			t.Fatalf("drop shipped index: %v", err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE INDEX %s ON context_blocks USING GIN(%s)
			   WHERE type_name NOT IN ('checkpoint','system-meta','catalog','insight')`,
			lang.index, col)); err != nil {
			t.Fatalf("create scratch index: %v", err)
		}
		// The query keeps the SHIPPED conjunct; the arm is now planned against
		// the wider index predicate. A partial index whose predicate is NOT
		// implied cannot be used at all, so this variant either scans
		// sequentially (correct but unindexed) or — with the matching wider
		// conjunct — silently loses the shadow rows. The scratch conjunct below
		// is what a "alle excluded" wave would have shipped in the functions.
		scratchStmt := strings.ReplaceAll(stmt,
			"cb.type_name NOT IN ('checkpoint','system-meta')",
			"cb.type_name NOT IN ('checkpoint','system-meta','catalog','insight')")
		if scratchStmt == stmt {
			t.Fatalf("%s: scratch substitution matched nothing", lang.cte)
		}
		var broken int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM (`+scratchStmt+`) arm
			  JOIN context_blocks cb ON cb.id = arm.id
			 WHERE cb.type_name IN ('catalog','insight')`).Scan(&broken); err != nil {
			t.Fatalf("count shadow hits (scratch predicate): %v", err)
		}
		if broken != 0 {
			t.Errorf("%s: the \"alle excluded\" variant still returns %d shadow rows — the design choice would not have been load-bearing",
				lang.cte, broken)
		}
		explain := opsw1ExplainText(t, ctx, tx, scratchStmt)
		t.Logf("gate 4 negative (%s): shipped predicate delivers %d catalog/insight rows, the \"alle excluded\" variant delivers %d\n%s",
			lang.cte, shipped, broken, explain)

		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// The predicate normal form, pinned against a freshly built index
// ---------------------------------------------------------------------------

// TestOPSW1PredicateNormalForm pins the string migration 145's DO block compares
// against. The guard recognises an already-built target index by its predicate
// in PostgreSQL's normal form; if a PG version ever prints that form
// differently, the guard would stop recognising a correct index and silently
// warn instead. This test turns that into a red test.
func TestOPSW1PredicateNormalForm(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`CREATE INDEX opsw1_normalform_probe ON context_blocks USING GIN(ts_de)
		   WHERE type_name NOT IN ('checkpoint','system-meta')`); err != nil {
		t.Fatalf("build probe index: %v", err)
	}
	var got string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_expr(i.indpred, i.indrelid)
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = 'opsw1_normalform_probe'`).Scan(&got); err != nil {
		t.Fatalf("read probe predicate: %v", err)
	}
	if got != opsw1TargetPred {
		t.Fatalf("PostgreSQL prints the predicate as\n  %s\nbut migration 145 compares against\n  %s\n"+
			"— the DO block's recognition of an out-of-band built index is broken; update both together", got, opsw1TargetPred)
	}

	// The DO block carries the same string inside a plpgsql literal, so its single
	// quotes are doubled there. Comparing the ESCAPED form is what makes this a
	// drift gate on the migration rather than on this file alone.
	raw, err := migrations.Section(opsw1New)
	if err != nil {
		t.Fatalf("read %s: %v", opsw1New, err)
	}
	escaped := strings.ReplaceAll(opsw1TargetPred, "'", "''")
	if !strings.Contains(string(raw), escaped) {
		t.Fatalf("%s does not contain the normal form as a plpgsql literal (%q) — the DO block and this test have drifted apart",
			opsw1New, escaped)
	}
	t.Logf("predicate normal form pinned: %s", got)
}

// ---------------------------------------------------------------------------
// The index guard: the branches a normal migration run never reaches
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The mass guard: the axis the rebuild cost actually runs on (review A1 / #4)
// ---------------------------------------------------------------------------

// opsw1MassGuardLiteral is the byte threshold migration 145 compares against,
// spelled as the decimal literal the file carries so the scratch substitution
// below has one unambiguous target. 256 MiB.
const opsw1MassGuardLiteral = "268435456"

// opsw1HeavyRows / opsw1HeavyChunks build the fixture this gate needs: FEW rows
// carrying a LOT of text. 600 rows × 960 distinct md5 digests ≈ 31 kB each, and
// distinct digests are the worst case for a tsvector GIN (every lexeme unique),
// so the resulting index is large while the row count stays four orders of
// magnitude below the row threshold the first version of this migration used.
//
// That gap IS the finding: a row-count guard waves a corpus through whose
// rebuild cost is measured in tens of seconds under ACCESS EXCLUSIVE. Measured
// on this image, 6 000 such rows (181 MB text) produce a 669 MB GIN and take
// 25,86 s to build — ~26 MB of index per second.
const (
	opsw1HeavyRows   = 600
	opsw1HeavyChunks = 960
)

// opsw1SeedHeavy writes rows that are few but textually heavy, and returns the
// resulting size of the FULL idx_context_ts_de in bytes.
func opsw1SeedHeavy(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (rows int, indexBytes int64) {
	t.Helper()
	start := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, type_name)
		SELECT 'opsw1', 'opsw1 heavy ' || s,
		       (SELECT string_agg(md5((s::bigint * 1000003 + k)::text), ' ')
		          FROM generate_series(1, $3::int) k),
		       $1,
		       CASE WHEN s % 3 = 0 THEN 'knowledge' ELSE 'checkpoint' END
		FROM generate_series(1, $2::int) s`, opsw1Scope, opsw1HeavyRows, opsw1HeavyChunks); err != nil {
		t.Fatalf("seed heavy fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM context_blocks").Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT pg_relation_size('public.idx_context_ts_de'::regclass)").Scan(&indexBytes); err != nil {
		t.Fatalf("read index size: %v", err)
	}
	t.Logf("heavy fixture: %d rows, idx_context_ts_de %d bytes (%.1f MiB), seeded in %s",
		rows, indexBytes, float64(indexBytes)/1024/1024, time.Since(start).Round(time.Millisecond))
	return rows, indexBytes
}

// opsw1ScratchMigration returns migration 145 with its mass threshold replaced
// by `threshold`. Installing a variant of the shipped file is the house pattern
// for probing a branch a normal run never reaches (142:109-111
// TestMW1ArmsTypeNameConstantProbe, and this file's own gate-4 negative half).
func opsw1ScratchMigration(t *testing.T, threshold int64) string {
	t.Helper()
	raw, err := migrations.Section(opsw1New)
	if err != nil {
		t.Fatalf("read %s: %v", opsw1New, err)
	}
	body := string(raw)
	if n := strings.Count(body, opsw1MassGuardLiteral); n != 1 {
		t.Fatalf("%s contains the mass threshold literal %q %d times, want exactly 1 — "+
			"the guard does not run on a byte axis (review A1/#4) or the literal was reformatted",
			opsw1New, opsw1MassGuardLiteral, n)
	}
	return strings.Replace(body, opsw1MassGuardLiteral, fmt.Sprint(threshold), 1)
}

// opsw1RunCapturingNotices executes one SQL text over a notice-capturing pool
// and returns the WARNING lines. testdb's pool has no notice handler, and in
// the "too large to rebuild inline" branch the warning IS the entire signal.
func opsw1RunCapturingNotices(t *testing.T, ctx context.Context, dsn, sql string) []string {
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

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("run scratch migration: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit scratch migration: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var warnings []string
	for _, n := range notices {
		if strings.HasPrefix(n, "WARNING") {
			warnings = append(warnings, n)
		}
	}
	return warnings
}

// TestOPSW1MassGuard is the A1 gate: the rebuild guard has to schwell on the
// axis the rebuild cost runs on.
//
// The first version of this migration carried `reltuples < 500000`, inherited
// from 115/140 where the build cost genuinely is per-row (HNSW / GiST over
// `title`). For a tsvector GIN it is not: this wave's own gate 5 measures 53,9 %
// savings on a row-proportional fixture against 97,0 % on a live-shaped one —
// the same row count, an entirely different mass. A row threshold therefore
// waves through exactly the corpus this guard exists to stop: at live shape
// (7 834 rows / 81 MB GIN) a bestand just under 500 000 rows carries roughly
// 5 GB of GIN, and the migration would have rebuilt both indexes inline, in one
// transaction, under ACCESS EXCLUSIVE, inside ctxd's boot path.
// `SET LOCAL lock_timeout` does not help: it bounds the ACQUISITION of the lock,
// never its hold time.
//
// Both branches are probed against the SAME fixture, which is what makes the
// axis change visible: 600 rows — four orders of magnitude below the old row
// threshold — and the guard still refuses once the threshold sits under the
// index's actual size.
func TestOPSW1MassGuard(t *testing.T) {
	t.Run("below the threshold the migration rebuilds inline", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 144)
		ctx := context.Background()

		rows, indexBytes := opsw1SeedHeavy(t, ctx, pool)
		// Threshold generously above the measured size: this branch must build.
		warnings := opsw1RunCapturingNotices(t, ctx, dsn, opsw1ScratchMigration(t, indexBytes*4))

		for _, name := range []string{opsw1IdxDe, opsw1IdxEn} {
			st, ok := opsw1Index(t, ctx, pool, name)
			if !ok {
				t.Fatalf("%s vanished", name)
			}
			if st.Pred == nil || *st.Pred != opsw1TargetPred {
				t.Errorf("%s was NOT rebuilt below the threshold: predicate=%s", name, opsw1PredOf(st))
			}
		}
		if len(warnings) != 0 {
			t.Errorf("below the threshold the guard must not warn, got: %v", warnings)
		}
		t.Logf("A1 branch 'below': %d rows / %d bytes index, threshold %d ⇒ both indexes partial, no warning",
			rows, indexBytes, indexBytes*4)
	})

	t.Run("above the threshold it warns and leaves the full index alone", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 144)
		ctx := context.Background()

		rows, indexBytes := opsw1SeedHeavy(t, ctx, pool)
		if indexBytes < 4*1024*1024 {
			t.Fatalf("heavy fixture produced only %d bytes of index — too small to straddle a threshold meaningfully", indexBytes)
		}
		var oidDeBefore, oidEnBefore uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxDe).Scan(&oidDeBefore); err != nil {
			t.Fatalf("read de oid: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxEn).Scan(&oidEnBefore); err != nil {
			t.Fatalf("read en oid: %v", err)
		}

		// Threshold just UNDER the measured size: this branch must refuse.
		threshold := indexBytes / 2
		warnings := opsw1RunCapturingNotices(t, ctx, dsn, opsw1ScratchMigration(t, threshold))

		for name, before := range map[string]uint32{opsw1IdxDe: oidDeBefore, opsw1IdxEn: oidEnBefore} {
			st, ok := opsw1Index(t, ctx, pool, name)
			if !ok {
				t.Fatalf("%s vanished — above the threshold the migration must not touch it", name)
			}
			if st.Pred != nil {
				t.Errorf("%s was rebuilt as partial ABOVE the threshold (predicate=%s) — the guard did not hold",
					name, *st.Pred)
			}
			var after uint32
			if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", name).Scan(&after); err != nil {
				t.Fatalf("read %s oid after: %v", name, err)
			}
			if after != before {
				t.Errorf("%s was replaced above the threshold (oid %d -> %d)", name, before, after)
			}
		}

		if len(warnings) < 2 {
			t.Fatalf("above the threshold the guard must WARN per index (got %d warnings): %v", len(warnings), warnings)
		}
		for _, w := range warnings {
			if !strings.Contains(w, "Runbook") {
				t.Errorf("warning does not point at the runbook: %q", w)
			}
		}
		// The functions must still be replaced — only the index rebuild is
		// deferred. Otherwise a large corpus would get the space penalty of the
		// old index AND lose the conjunct.
		var srcDe string
		if err := pool.QueryRow(ctx,
			`SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			  WHERE n.nspname = 'public' AND p.proname = 'ctx_rrf'`).Scan(&srcDe); err != nil {
			t.Fatalf("read ctx_rrf source: %v", err)
		}
		if !strings.Contains(srcDe, "NOT IN ('checkpoint','system-meta')") {
			t.Error("ctx_rrf did not receive the static conjunct although only the index rebuild was deferred")
		}

		t.Logf("A1 branch 'above': %d rows (four orders of magnitude below the old 500 000-row threshold) / %d bytes index, threshold %d ⇒ no rebuild, %d warnings, functions still updated\n  %s",
			rows, indexBytes, threshold, len(warnings), strings.Join(warnings, "\n  "))
	})
}

// TestOPSW1IndexGuard pins the DO block's branches. They matter at the target
// scale: DROP INDEX takes ACCESS EXCLUSIVE and CREATE INDEX a SHARE lock on
// context_blocks for the whole migration transaction, and RunMigrations sits in
// ctxd's boot path — at 1M+ blocks an unguarded rebuild would block writers for
// the duration of a boot. The guard turns that into an operator decision.
func TestOPSW1IndexGuard(t *testing.T) {
	t.Run("pre-built target index is a no-op", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 144)
		ctx := context.Background()

		// The operator's out-of-band sequence from the file header, collapsed:
		// build the partial index under the final name.
		if _, err := pool.Exec(ctx, "DROP INDEX "+opsw1IdxDe); err != nil {
			t.Fatalf("drop full index: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`CREATE INDEX `+opsw1IdxDe+` ON context_blocks USING GIN(ts_de)
			   WHERE type_name NOT IN ('checkpoint','system-meta')`); err != nil {
			t.Fatalf("pre-build partial index: %v", err)
		}
		var before uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxDe).Scan(&before); err != nil {
			t.Fatalf("read index oid: %v", err)
		}

		opsw1ApplyMigrations(t, context.Background(), pool)

		var after uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxDe).Scan(&after); err != nil {
			t.Fatalf("read index oid after 145: %v", err)
		}
		if before != after {
			t.Errorf("145 replaced the pre-built index (oid %d -> %d) — an operator's out-of-band CONCURRENTLY build must survive the migration",
				before, after)
		}
		// The sibling was NOT pre-built and must have been converted.
		st, ok := opsw1Index(t, ctx, pool, opsw1IdxEn)
		if !ok || st.Pred == nil || *st.Pred != opsw1TargetPred {
			t.Errorf("%s was not converted while %s was adopted: %s", opsw1IdxEn, opsw1IdxDe, opsw1PredOf(st))
		}
		t.Logf("guard branch (a1): pre-built %s adopted unchanged (oid %d), %s converted", opsw1IdxDe, after, opsw1IdxEn)
	})

	t.Run("foreign index under the name is left alone, loudly", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 144)
		ctx := context.Background()

		// A btree under the FTS index's name: right table, wrong everything else.
		if _, err := pool.Exec(ctx, "DROP INDEX "+opsw1IdxEn); err != nil {
			t.Fatalf("drop full index: %v", err)
		}
		if _, err := pool.Exec(ctx, "CREATE INDEX "+opsw1IdxEn+" ON context_blocks (category)"); err != nil {
			t.Fatalf("create foreign index: %v", err)
		}
		var before uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxEn).Scan(&before); err != nil {
			t.Fatalf("read index oid: %v", err)
		}

		opsw1ApplyMigrations(t, ctx, pool)

		st, ok := opsw1Index(t, ctx, pool, opsw1IdxEn)
		if !ok {
			t.Fatalf("%s vanished — the migration must never drop a foreign index", opsw1IdxEn)
		}
		if st.AM != "btree" || st.Pred != nil {
			t.Errorf("%s was modified: amname=%s predicate=%s — the guard must leave a foreign index untouched",
				opsw1IdxEn, st.AM, opsw1PredOf(st))
		}
		var after uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxEn).Scan(&after); err != nil {
			t.Fatalf("read index oid after 145: %v", err)
		}
		if before != after {
			t.Errorf("%s was replaced (oid %d -> %d)", opsw1IdxEn, before, after)
		}
		// The sibling still had to be converted: one warning does not abort the run.
		sib, ok := opsw1Index(t, ctx, pool, opsw1IdxDe)
		if !ok || sib.Pred == nil || *sib.Pred != opsw1TargetPred {
			t.Errorf("%s was not converted although only %s was blocked: %s", opsw1IdxDe, opsw1IdxEn, opsw1PredOf(sib))
		}
		t.Logf("guard branch (a3): foreign btree under %s left untouched (oid %d), %s converted anyway", opsw1IdxEn, after, opsw1IdxDe)
	})

	t.Run("missing index is rebuilt as the partial one", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 144)
		ctx := context.Background()

		if _, err := pool.Exec(ctx, "DROP INDEX "+opsw1IdxDe); err != nil {
			t.Fatalf("drop index: %v", err)
		}
		opsw1ApplyMigrations(t, ctx, pool)

		st, ok := opsw1Index(t, ctx, pool, opsw1IdxDe)
		if !ok {
			t.Fatalf("%s was not rebuilt", opsw1IdxDe)
		}
		if st.AM != "gin" || st.Pred == nil || *st.Pred != opsw1TargetPred {
			t.Errorf("%s rebuilt wrong: amname=%s predicate=%s", opsw1IdxDe, st.AM, opsw1PredOf(st))
		}
		t.Logf("guard branch (name free): %s rebuilt as %s", opsw1IdxDe, st.Def)
	})

	t.Run("second run is a no-op", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 144)
		ctx := context.Background()

		opsw1ApplyMigrations(t, ctx, pool)
		var deBefore, enBefore uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxDe).Scan(&deBefore); err != nil {
			t.Fatalf("read de oid: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxEn).Scan(&enBefore); err != nil {
			t.Fatalf("read en oid: %v", err)
		}

		// Re-running the file itself (not the runner, which skips a registered
		// version) is the honest idempotence probe.
		raw, err := migrations.Section(opsw1New)
		if err != nil {
			t.Fatalf("read %s: %v", opsw1New, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("re-run %s: %v", opsw1New, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit re-run: %v", err)
		}

		var deAfter, enAfter uint32
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxDe).Scan(&deAfter); err != nil {
			t.Fatalf("read de oid after: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT oid FROM pg_class WHERE relname = $1", opsw1IdxEn).Scan(&enAfter); err != nil {
			t.Fatalf("read en oid after: %v", err)
		}
		if deBefore != deAfter || enBefore != enAfter {
			t.Errorf("re-running 145 rebuilt an index: de %d -> %d, en %d -> %d", deBefore, deAfter, enBefore, enAfter)
		}
		t.Logf("idempotence: re-running %s left both index oids untouched (de %d, en %d)", opsw1New, deAfter, enAfter)
	})
}

// ---------------------------------------------------------------------------
// The named consequence: a tenant overlay that lifts the deny-list
// ---------------------------------------------------------------------------

// TestOPSW1TenantOverlayShadowsFTS pins the ONE case in which the static
// conjunct is NOT a no-op, so a later change cannot walk past it silently.
//
// D6 "Overlay gewinnt" (blocktype/registry.go:252-256, pinned by the T12 probe)
// lets a tenant row overwrite the _global policy of the same name, and the type
// write path does not check a new name against the _global builtins
// (handler/types_write.go:150-172). A tenant can therefore lift `checkpoint` to
// full-pass, and Set.VisibleTypes() of THAT tenant then contains it — which is
// precisely the input for which the conjunct removes rows the other conjuncts
// would have let through.
//
// What the corpus loses in that case is the FTS contribution of those blocks;
// the semantic and trigram arms still deliver them. The live registry carries 11
// rows, all scope='_global', so the case is empty today. This test does not
// judge the trade-off — it makes it visible and measured.
func TestOPSW1TenantOverlayShadowsFTS(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 144)
	ctx := context.Background()

	opsw1Seed(t, ctx, pool, opsw1SmallRows, opsw1NeedleMid)
	opsw1ApplyMigrations(t, ctx, pool)

	overlay := opsw1VisibleLiteral([]string{"knowledge", "checkpoint"})

	for _, lang := range []opsw1Lang{opsw1De, opsw1En} {
		shipped := opsw1CTE(t, opsw1New, lang, 0, "", overlay)
		prevFile := opsw1PrevRRF
		prev := opsw1CTE(t, prevFile, lang, 0, "", overlay)

		var truth int
		col := "ts_de"
		dict := "german"
		if lang.cte == "fulltext_en" {
			col, dict = "ts_en", "english"
		}
		if err := pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT count(*) FROM context_blocks
			 WHERE NOT is_archived AND scope = $1
			   AND type_name IN ('knowledge','checkpoint')
			   AND %s @@ plainto_tsquery('%s', $2)`, col, dict), opsw1Scope, opsw1Needle).Scan(&truth); err != nil {
			t.Fatalf("truth count: %v", err)
		}

		var afterCheckpoints, beforeCheckpoints int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (`+shipped+`) arm JOIN context_blocks cb ON cb.id = arm.id
			 WHERE cb.type_name = 'checkpoint'`).Scan(&afterCheckpoints); err != nil {
			t.Fatalf("count checkpoints after 145: %v", err)
		}
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (`+prev+`) arm JOIN context_blocks cb ON cb.id = arm.id
			 WHERE cb.type_name = 'checkpoint'`).Scan(&beforeCheckpoints); err != nil {
			t.Fatalf("count checkpoints before 145: %v", err)
		}

		if beforeCheckpoints == 0 {
			t.Fatalf("%s: the 144 form already delivers no checkpoint rows for an overlay list — the fixture proves nothing", lang.cte)
		}
		if afterCheckpoints != 0 {
			t.Errorf("%s: the 145 form delivered %d checkpoint rows for an overlay list — the conjunct's consequence is not what this test documents; re-read the migration header",
				lang.cte, afterCheckpoints)
		}
		t.Logf("named consequence (%s): with checkpoint lifted into p_types_visible the arm delivers %d checkpoint rows after 145 (144: %d; %d rows would match the raw predicate)",
			lang.cte, afterCheckpoints, beforeCheckpoints, truth)
	}
}
