//go:build integration

// M-W1 gates for migration 142_arms_typename.sql (Achse 05 §4.3, §7 M-W1):
// ctx_rrf_arms grows a NINTH return column, type_name, so a dump knows the type
// composition of its own candidates. type_factor is already in the return but
// is NOT injective — two types can carry the same damping factor and every
// undamped type carries 1.0 — so the factor cannot answer "how many of the top
// ten are catalog blocks?" and the name can.
//
// The load-bearing claim is TYPE PARITY: for every id the function returns,
// type_name equals context_blocks.type_name of that same id. Nothing about a
// projection guarantees that — a constant, a stale join or the wrong alias all
// produce a plausible-looking column — so the gate proves it against the table
// on a fixture that spans FOUR types, and TestMW1ArmsTypeNameConstantProbe
// installs the constant variant to show the gate is not vacuous.
//
// The RED anchor is TestMW1ArmsTypeNameAbsentAt141: the same statement against
// the chain capped at 141 fails with SQLSTATE 42703. It is a permanent test
// rather than a one-off transcript, so the claim "142 is what introduces the
// column" stays checkable after 142 has landed.
//
//	go test -tags=integration ./internal/rrf/ -run TestMW1Arms -count=1 -v
package rrf_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// mw1Migration is the file under test. The probe reads it out of the embedded
// FS, so a renamed file fails loudly instead of silently skipping the gate.
const mw1Migration = "142_arms_typename.sql"

// mw1PrevVersion is the chain cap for the RED anchor: the highest migration
// that does NOT know the column.
const mw1PrevVersion = 141

// mw1AllFourTypes widens the allowlist to every type the B-W1 fixture writes,
// including `checkpoint`. bw1VisibleTypes covers three; only with the fourth
// does the parity assertion actually span four distinct names, which is what
// §4.3 asks for. Widening the allowlist is a per-call argument, not a registry
// change — the retrieval policy of `checkpoint` is untouched by this test.
var mw1AllFourTypes = []string{"knowledge", "reference", "audit-trail", "checkpoint"}

// mw1Row is one (id, type_name) pair as the function projects it. type_name is
// a POINTER so a NULL arrives as a test failure with a name attached rather
// than as a scan error — a LEFT JOIN that missed is exactly the defect this
// gate has to be able to describe.
type mw1Row struct {
	ID       string
	TypeName *string
}

// mw1CallArmsTypeName runs `SELECT id, type_name FROM <fn>(…)` over the shared
// 18-argument surface. fn is a parameter so the constant probe can call its own
// installed copy through the identical statement.
func mw1CallArmsTypeName(ctx context.Context, tx pgx.Tx, fn string, args []any) ([]mw1Row, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT id, type_name FROM %s(%s)`, fn, bw1ArgList), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mw1Row
	for rows.Next() {
		var r mw1Row
		if err := rows.Scan(&r.ID, &r.TypeName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// mw1TruthFromTable reads the type of every fixture block straight from
// context_blocks. The parity gate compares against THIS, not against the
// in-memory fixture map: the claim is about the table, and a fixture map only
// says what the test meant to write.
func mw1TruthFromTable(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id::text, type_name FROM context_blocks`)
	if err != nil {
		t.Fatalf("read context_blocks: %v", err)
	}
	defer rows.Close()
	truth := map[string]string{}
	for rows.Next() {
		var id, tn string
		if err := rows.Scan(&id, &tn); err != nil {
			t.Fatalf("scan context_blocks: %v", err)
		}
		truth[id] = tn
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate context_blocks: %v", err)
	}
	return truth
}

// mw1AssertParity is the gate itself: every returned row carries a non-NULL
// type_name equal to the table's. It returns the mismatch count and the
// distinct names it saw, so the constant probe can reuse the same comparison
// and assert the OPPOSITE outcome from the same code path.
func mw1AssertParity(t *testing.T, label string, rows []mw1Row, truth map[string]string) (int, []string) {
	t.Helper()
	seen := map[string]bool{}
	mismatch := 0
	for _, r := range rows {
		want, known := truth[r.ID]
		if !known {
			t.Errorf("%s: id %s is not in context_blocks at all", label, r.ID)
			mismatch++
			continue
		}
		if r.TypeName == nil {
			t.Errorf("%s: id %s came back with type_name NULL, want %q", label, r.ID, want)
			mismatch++
			continue
		}
		seen[*r.TypeName] = true
		if *r.TypeName != want {
			mismatch++
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return mismatch, names
}

// mw1CountParityOnly repeats the comparison WITHOUT failing the test — the
// constant probe needs the number, not a red test.
func mw1CountParityOnly(rows []mw1Row, truth map[string]string) int {
	mismatch := 0
	for _, r := range rows {
		if r.TypeName == nil || *r.TypeName != truth[r.ID] {
			mismatch++
		}
	}
	return mismatch
}

// ---------------------------------------------------------------------------
// RED anchor
// ---------------------------------------------------------------------------

// TestMW1ArmsTypeNameAbsentAt141 pins the pre-142 state: the column does not
// exist, and the statement the parity gate runs fails with 42703
// (undefined_column). Capping the chain at 141 keeps this reproducible forever
// instead of only in the wave report.
func TestMW1ArmsTypeNameAbsentAt141(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, mw1PrevVersion)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := bw1Query{name: "red", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	args := bw1Args(q, bw1Embedding(0), nil, bw1Limit)

	_, err = mw1CallArmsTypeName(ctx, tx, "ctx_rrf_arms", args)
	if err == nil {
		t.Fatalf("chain capped at %d already returns type_name — the RED anchor is vacuous", mw1PrevVersion)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want a PgError, got %T: %v", err, err)
	}
	t.Logf("RED at migration %d: SQLSTATE %s — %s", mw1PrevVersion, pgErr.Code, pgErr.Message)
	if pgErr.Code != "42703" {
		t.Errorf("SQLSTATE = %s, want 42703 (undefined_column)", pgErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Gate: type parity over four types
// ---------------------------------------------------------------------------

// TestMW1ArmsTypeNameParity is the §4.3 gate. It runs the projection twice —
// once under the production-shaped allowlist (three types) and once with
// `checkpoint` added, so the assertion spans all four fixture types — and
// compares every row against context_blocks. It then repeats the check through
// the GO seam (rrf.ArmRanksTx → rrf.ArmRow.TypeName), because a correct SQL
// column that the Go scanner drops on the floor is the same defect one layer up.
func TestMW1ArmsTypeNameParity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bw1SeedCorpus(t, pool)
	truth := mw1TruthFromTable(t, pool)

	q := bw1Query{name: "parity", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	emb := bw1Embedding(0)

	call := func(label string, allowlist []string) ([]mw1Row, []string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", label, err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		args := bw1Args(q, emb, nil, bw1BigLimit)
		args[9] = allowlist
		rows, err := mw1CallArmsTypeName(ctx, tx, "ctx_rrf_arms", args)
		if err != nil {
			t.Fatalf("%s: ctx_rrf_arms: %v", label, err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s: no candidates — the parity assertion would be vacuous", label)
		}
		mismatch, names := mw1AssertParity(t, label, rows, truth)
		t.Logf("%s: %d rows, %d type mismatches, distinct type_name = %v", label, len(rows), mismatch, names)
		if mismatch != 0 {
			t.Errorf("%s: %d of %d rows disagree with context_blocks.type_name", label, mismatch, len(rows))
		}
		return rows, names
	}

	// Run A: the allowlist the live path uses. `checkpoint` is retrieval
	// 'excluded' and must NOT appear.
	rowsA, namesA := call("gate (a) three visible types", bw1VisibleTypes)
	for _, n := range namesA {
		if n == "checkpoint" {
			t.Errorf("gate (a): excluded type checkpoint surfaced under the production allowlist")
		}
	}
	if len(namesA) != 3 {
		t.Errorf("gate (a): %d distinct types in the result (%v), want 3 — fixture too thin", len(namesA), namesA)
	}

	// Run B: all four. The point of the wider allowlist is the fourth NAME in
	// the assertion, not a policy statement about checkpoint.
	rowsB, namesB := call("gate (b) four visible types", mw1AllFourTypes)
	if len(namesB) != 4 {
		t.Errorf("gate (b): %d distinct types in the result (%v), want 4", len(namesB), namesB)
	}
	if len(rowsB) <= len(rowsA) {
		t.Errorf("gate (b): widening the allowlist did not add candidates (%d vs %d)", len(rowsB), len(rowsA))
	}

	// Go seam: the same column through rrf.ArmRow.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin ro tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, dec, err := bw2Search(ctx, tx, pool, emb, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("SearchTx: %v", err)
	}
	armRows, err := bw2Arms(ctx, tx, dec, rrf.SelectorPolicy{}, emb)
	if err != nil {
		t.Fatalf("ArmRanksTx: %v", err)
	}
	if len(armRows) == 0 {
		t.Fatal("ArmRanksTx returned no rows — the seam assertion would be vacuous")
	}
	goSeen := map[string]bool{}
	goMismatch := 0
	for _, r := range armRows {
		goSeen[r.TypeName] = true
		if r.TypeName != truth[r.ID] {
			goMismatch++
			t.Errorf("gate (c) Go seam: id %s carries TypeName %q, table says %q", r.ID, r.TypeName, truth[r.ID])
		}
	}
	names := make([]string, 0, len(goSeen))
	for n := range goSeen {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("gate (c) Go seam: %d ArmRows, %d mismatches, distinct TypeName = %v", len(armRows), goMismatch, names)
	if goSeen[""] {
		t.Error("gate (c) Go seam: at least one ArmRow.TypeName is the empty string — the scanner did not pick the column up")
	}
}

// ---------------------------------------------------------------------------
// Negative probe: the constant variant
// ---------------------------------------------------------------------------

// mw1InstallConstantProbe loads 142 out of the embedded FS, renames the
// function and replaces the type_name PROJECTION with a constant.
//
// The surgery is confined to the text AFTER `RETURN QUERY`, exactly as
// bw1MakeProbe does it for 137: the file header discusses `tf.type_name::TEXT`
// in prose, and a probe that rewrote a comment would install an unmutated body
// and pass while asserting nothing. The occurrence count inside the body is
// asserted too, so a drifted migration fails the probe loudly instead of
// turning it green by accident.
func mw1InstallConstantProbe(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	raw, err := migrations.Section(mw1Migration)
	if err != nil {
		t.Fatalf("read embedded %s: %v", mw1Migration, err)
	}
	def := strings.ReplaceAll(string(raw), "ctx_rrf_arms", name)
	cut := strings.Index(def, "RETURN QUERY")
	if cut < 0 {
		t.Fatalf("marker %q not found in %s", "RETURN QUERY", mw1Migration)
	}
	head, tail := def[:cut], def[cut:]

	const needle = "tf.type_name::TEXT"
	if n := strings.Count(tail, needle); n != 1 {
		t.Fatalf("probe: %s carries %d occurrences of %q after RETURN QUERY, want exactly 1 — migration drifted?",
			mw1Migration, n, needle)
	}
	mutated := head + strings.Replace(tail, needle, "'knowledge'::TEXT", 1)
	if mutated == def {
		t.Fatal("probe: mutation was a no-op")
	}
	if _, err := pool.Exec(context.Background(), mutated); err != nil {
		t.Fatalf("install probe %s: %v", name, err)
	}
}

// TestMW1ArmsTypeNameConstantProbe is the §7 negative probe: a body that
// projects a constant instead of the block's type must make the parity gate
// red. If it stayed green, TestMW1ArmsTypeNameParity would be asserting nothing
// about where the value comes from.
//
// The probe also pins that the mutation changed ONLY the projection: the id set
// of the constant variant must be identical to the green one, so the red does
// not come from a candidate set that drifted.
func TestMW1ArmsTypeNameConstantProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bw1SeedCorpus(t, pool)
	truth := mw1TruthFromTable(t, pool)

	const probeFn = "ctx_rrf_arms_mw1_const_probe"
	mw1InstallConstantProbe(t, pool, probeFn)

	q := bw1Query{name: "probe", text: bw1FixedQuery, spaced: bw1FixedQuery, mode: "ann"}
	emb := bw1Embedding(0)

	run := func(fn string) []mw1Row {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		args := bw1Args(q, emb, nil, bw1BigLimit)
		args[9] = mw1AllFourTypes
		rows, err := mw1CallArmsTypeName(ctx, tx, fn, args)
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		return rows
	}

	green := run("ctx_rrf_arms")
	probe := run(probeFn)

	if n := mw1CountParityOnly(green, truth); n != 0 {
		t.Fatalf("precondition: the green body already has %d type mismatches", n)
	}
	mismatch := mw1CountParityOnly(probe, truth)
	t.Logf("negative probe: constant 'knowledge' projection → %d of %d rows disagree with context_blocks (green: 0 of %d)",
		mismatch, len(probe), len(green))
	if mismatch == 0 {
		t.Error("RED probe stayed green: a constant type_name produced no parity mismatch")
	}

	idSet := func(rows []mw1Row) map[string]bool {
		s := map[string]bool{}
		for _, r := range rows {
			s[r.ID] = true
		}
		return s
	}
	bw1AssertSameSet(t, "negative probe candidate set", idSet(green), idSet(probe))
}
