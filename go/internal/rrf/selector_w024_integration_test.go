//go:build integration

// W02-4 gates G1, G2 and G4 (design/02-strategy-selektor.md §7 "W02-4",
// §4.3b, §4.6) against a real PG18 testcontainer:
//
//	G1: a MID-sized scope (probe > ExactMax, pg_stats estimate <= GreyMax)
//	    decides grey — the outcome the W02-2 stub could never produce.
//	G2: that grey decision carries the CLAMPED GreyScanTuples into
//	    hnsw.max_scan_tuples inside the ctx_rrf call (W02-1-G4 mechanic:
//	    current_setting observed in the same transaction as the call; the
//	    GUC is gone afterwards because SET LOCAL dies with the tx).
//	G4: migration 117 makes the statistics target effective — a >100-scope
//	    fixture corpus does NOT fit the MCV list at target 100 and DOES fit
//	    it at target 1000.
//
//	go test -tags=integration ./internal/rrf/ -run TestSelectorW024 -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// w024MCV reads the CURRENT most-common-values list of context_blocks.scope
// exactly the way §4.3b's catalog read does: schema-qualified, OID-joined,
// n_distinct and reltuples alongside. Returns nil when the column has no
// stats row at all.
func w024MCV(t *testing.T, pool *pgxpool.Pool) (vals []string, freqs []float32, nDistinct, reltuples float32) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT s.most_common_vals::text::text[], s.most_common_freqs, s.n_distinct, c.reltuples
		FROM pg_stats s
		JOIN pg_class c ON c.oid = 'public.context_blocks'::regclass
		WHERE s.schemaname = 'public' AND s.tablename = 'context_blocks' AND s.attname = 'scope'`).
		Scan(&vals, &freqs, &nDistinct, &reltuples)
	if err != nil {
		t.Fatalf("pg_stats catalog read: %v", err)
	}
	return vals, freqs, nDistinct, reltuples
}

// w024Bulk inserts n blocks into scope in ONE statement (generate_series) —
// the G1/G4 fixtures need hundreds of rows and per-row roundtrips dominate
// the test runtime otherwise. Embedding stays NULL for the pure-statistics
// fixtures (G4); pass withEmbedding for the retrieval fixtures (G1/G2).
func w024Bulk(t *testing.T, pool *pgxpool.Pool, scope string, n int, withEmbedding bool) {
	t.Helper()
	// One shared, constant embedding: G1/G2 assert the DECISION and the GUC,
	// never a ranking, so distinct vectors would be noise. The pure-statistics
	// fixture (G4) passes NULL.
	var embParam any
	if withEmbedding {
		embParam = pgvec.NewVector(w021Query())
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO context_blocks (category, title, content, scope, embedding, type_name, is_archived)
		SELECT 'knowledge', 'w024-' || $1 || '-' || g, 'w024-content', $1, $3::vector, 'knowledge', false
		FROM generate_series(1, $2) g`, scope, n, embParam)
	if err != nil {
		t.Fatalf("bulk insert %d rows into scope %q: %v", n, scope, err)
	}
}

func w024Analyze(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `ANALYZE context_blocks`); err != nil {
		t.Fatalf("ANALYZE context_blocks: %v", err)
	}
}

func w024Policy() rrf.SelectorPolicy {
	return rrf.SelectorPolicy{
		Enabled:        true,
		ExactMax:       64, // the mechanism floor — keeps the mid fixture small
		GreyMax:        65536,
		GreyScanTuples: 60000,
		StatsTTL:       60 * time.Second,
	}
}

// TestSelectorW024_G1_GreyOnMidScope is gate G1: a scope ABOVE the probe cap
// but with a pg_stats estimate BELOW GreyMax decides grey with reason
// stats<=grey_max. RED against the W02-3 state: the stub reports
// "unavailable", so the same fixture landed on {ann, stats_stale}.
func TestSelectorW024_G1_GreyOnMidScope(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := w021Query()

	// 70 blocks in the mid scope (probe cap is 64+1 → 65 > 64 → stats stage)
	// and a dominant neighbour scope so reltuples/frequencies are non-trivial.
	const (
		midScope = "w024mid"
		bigScope = "w024big"
		midN     = 70
		bigN     = 300
	)
	w024Bulk(t, pool, midScope, midN, true)
	w024Bulk(t, pool, bigScope, bigN, true)
	w024Analyze(t, pool)

	vals, freqs, nDistinct, reltuples := w024MCV(t, pool)
	t.Logf("catalog: mcv=%v freqs=%v n_distinct=%v reltuples=%v", vals, freqs, nDistinct, reltuples)

	res, dec, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{midScope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, w024Policy())
	if err != nil {
		t.Fatalf("rrf.Search (mid scope): %v", err)
	}

	if dec.Mode != rrf.ModeGrey || dec.Reason != rrf.ReasonStatsGrey {
		t.Errorf("decision = {%q, %q}, want {grey, stats<=grey_max} — RED against the W02-3 stub, which reported {ann, stats_stale}",
			dec.Mode, dec.Reason)
	}
	// The estimate is the pg_stats value (MCV freq × reltuples), not the
	// capped probe count 65 — the whole point of stage 2.
	if dec.Estimate < midN-2 || dec.Estimate > midN+2 {
		t.Errorf("estimate = %d, want ≈%d (MCV frequency × reltuples)", dec.Estimate, midN)
	}
	if len(res) == 0 {
		t.Error("grey path returned no rows")
	}
	t.Logf("G1 GREEN: decision %+v, %d rows", dec, len(res))

	// Contrast on the same corpus: the dominant scope is ABOVE GreyMax only
	// if GreyMax is lowered under it — do that with a policy copy, which
	// pins the second stats branch (stats>grey_max → plain ann).
	large := w024Policy()
	large.GreyMax = 100 // below the 300-row scope estimate
	_, decLarge, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{bigScope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, large)
	if err != nil {
		t.Fatalf("rrf.Search (big scope): %v", err)
	}
	if decLarge.Mode != rrf.ModeANN || decLarge.Reason != rrf.ReasonStatsLarge {
		t.Errorf("big-scope decision = {%q, %q}, want {ann, stats>grey_max}", decLarge.Mode, decLarge.Reason)
	}
	if decLarge.Estimate < bigN-5 || decLarge.Estimate > bigN+5 {
		t.Errorf("big-scope estimate = %d, want ≈%d", decLarge.Estimate, bigN)
	}
	t.Logf("G1 complement: big scope → %+v", decLarge)
}

// w024GucSpySQL is the spy table the G2 probe installs into the ctx_rrf body.
const w024GucSpySQL = `CREATE TABLE w024_guc_probe (mode TEXT, scan_tuples TEXT, iterative TEXT)`

// w024InstallGucSpy rewrites the LIVE ctx_rrf definition in place
// (pg_get_functiondef + string surgery, W02-1's g15MakeVariant mechanic) so
// that every call records the GUCs it set. The function keeps its name and
// signature, so rrf.Search — which cannot be handed a transaction and would
// otherwise never let the SET LOCAL be observed — runs straight into it.
// A no-op replacement fails loudly: a drifted body would make the gate
// vacuous.
func w024InstallGucSpy(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, w024GucSpySQL); err != nil {
		t.Fatalf("create spy table: %v", err)
	}
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = 'ctx_rrf'`).Scan(&def); err != nil {
		t.Fatalf("pg_get_functiondef(ctx_rrf): %v", err)
	}
	const marker = "RETURN QUERY"
	spy := `INSERT INTO w024_guc_probe VALUES (p_semantic_mode,
		current_setting('hnsw.max_scan_tuples', true),
		current_setting('hnsw.iterative_scan', true));
    ` + marker
	mutated := strings.Replace(def, marker, spy, 1)
	if mutated == def {
		t.Fatalf("spy injection was a no-op — marker %q missing, ctx_rrf body drifted?", marker)
	}
	if _, err := pool.Exec(ctx, mutated); err != nil {
		t.Fatalf("install spy variant of ctx_rrf: %v", err)
	}
}

func w024SpyRows(t *testing.T, pool *pgxpool.Pool) []struct{ Mode, Tuples, Iter string } {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT mode, COALESCE(scan_tuples, '<unset>'), COALESCE(iterative, '<unset>') FROM w024_guc_probe`)
	if err != nil {
		t.Fatalf("read spy table: %v", err)
	}
	defer rows.Close()
	var out []struct{ Mode, Tuples, Iter string }
	for rows.Next() {
		var r struct{ Mode, Tuples, Iter string }
		if err := rows.Scan(&r.Mode, &r.Tuples, &r.Iter); err != nil {
			t.Fatalf("scan spy row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("spy rows: %v", err)
	}
	return out
}

// TestSelectorW024_G2_GreyBudgetReachesGUC is gate G2: the grey decision maps
// onto ('ann', clamped GreyScanTuples, NULL) and that budget really arrives
// as hnsw.max_scan_tuples inside the call — observed in the call's own
// transaction (spy) and proven dead afterwards (SET LOCAL scope, W02-1-G4).
func TestSelectorW024_G2_GreyBudgetReachesGUC(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := w021Query()

	const midScope = "w024gucmid"
	w024Bulk(t, pool, midScope, 70, true)
	w024Bulk(t, pool, "w024gucbig", 300, true)
	w024Analyze(t, pool)
	w024InstallGucSpy(t, pool)

	// GreyScanTuples deliberately ABOVE the mechanism ceiling: the value that
	// reaches the GUC must be the CLAMPED one (§5.4), not the policy value.
	policy := w024Policy()
	policy.GreyScanTuples = 100_000_000

	_, dec, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{midScope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, policy)
	if err != nil {
		t.Fatalf("rrf.Search (grey): %v", err)
	}
	if dec.Mode != rrf.ModeGrey {
		t.Fatalf("decision = %+v, want grey (G2 needs the grey branch)", dec)
	}

	spy := w024SpyRows(t, pool)
	if len(spy) != 1 {
		t.Fatalf("spy rows = %+v, want exactly 1 ctx_rrf call", spy)
	}
	if spy[0].Mode != rrf.ModeANN {
		t.Errorf("SQL mode = %q, want ann (grey is ann + budget, §4.6 mapping)", spy[0].Mode)
	}
	if spy[0].Tuples != "200000" {
		t.Errorf("hnsw.max_scan_tuples inside the call = %q, want the CLAMPED 200000 (policy asked for 100000000)", spy[0].Tuples)
	}
	if spy[0].Iter != "relaxed_order" {
		t.Errorf("hnsw.iterative_scan inside the call = %q, want relaxed_order", spy[0].Iter)
	}
	t.Logf("G2 GREEN: grey call ran as mode=%s with hnsw.max_scan_tuples=%s, iterative_scan=%s",
		spy[0].Mode, spy[0].Tuples, spy[0].Iter)

	// SET LOCAL scope: the budget does not survive the call's transaction.
	var after *string
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('hnsw.max_scan_tuples', true)`).Scan(&after); err != nil {
		t.Fatalf("current_setting after the call: %v", err)
	}
	if after != nil && *after == "200000" {
		t.Error("hnsw.max_scan_tuples survived the ctx_rrf transaction — SET LOCAL semantics broken")
	}
	t.Logf("G2 GUC scope: after the call max_scan_tuples=%v", strPtr(after))

	// Contrast: a plain-ann (disabled policy) call sets NO budget at all —
	// the grey budget is a decision effect, not a constant.
	if _, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{midScope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{}); err != nil {
		t.Fatalf("rrf.Search (zero policy): %v", err)
	}
	spy = w024SpyRows(t, pool)
	if len(spy) != 2 {
		t.Fatalf("spy rows after the Ist call = %+v, want 2", spy)
	}
	if spy[1].Tuples == "200000" {
		t.Errorf("the Ist path set the grey budget %q — plain ann must leave hnsw.max_scan_tuples alone", spy[1].Tuples)
	}
	t.Logf("G2 contrast: Ist call ran as mode=%s with max_scan_tuples=%s", spy[1].Mode, spy[1].Tuples)
}

// TestSelectorW024_G4_StatisticsTargetEffective is gate G4: with the shipped
// default target (100) the MCV list of context_blocks.scope CANNOT hold a
// >100-scope corpus; after migration 117 (target 1000 + ANALYZE) it holds all
// of them. The pre-state is the genuine pre-117 database (migration chain
// capped at 116), not a simulated one.
func TestSelectorW024_G4_StatisticsTargetEffective(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 116)
	ctx := context.Background()

	// 150 scopes with DELIBERATELY skewed row counts (2..6 rows, cycling):
	// at target 100 Postgres cannot keep all 150 distinct values and falls
	// back to its "significantly more common than average" filter, so only a
	// subset survives. At target 1000 every value appears more than once and
	// the whole distinct set fits — Postgres keeps all of them.
	const scopes = 150
	want := make([]string, 0, scopes)
	for i := 0; i < scopes; i++ {
		s := fmt.Sprintf("w024t%03d", i)
		want = append(want, s)
		w024Bulk(t, pool, s, 2+i%5, false)
	}
	w024Analyze(t, pool)

	// PG17+ stores the "cluster default" as NULL rather than -1, so the
	// pre-state assert is on nil (design/03 W11 lesson: pin the version's
	// actual catalog encoding, not the remembered one).
	target := w024StatTarget(t, pool)
	if target != nil {
		t.Fatalf("pre-117 attstattarget = %d, want NULL (cluster default) — the RED state is not the shipped one", *target)
	}

	before, _, ndBefore, _ := w024MCV(t, pool)
	missing := w024Missing(want, before)
	if len(missing) == 0 {
		t.Fatalf("G4 RED failed: at the default target the MCV list already covers all %d fixture scopes (%d entries) — the gate would be vacuous",
			scopes, len(before))
	}
	t.Logf("G4 RED (pre-117, attstattarget=NULL → cluster default 100): MCV holds %d entries, %d of %d fixture scopes missing (e.g. %v), n_distinct=%v",
		len(before), len(missing), scopes, missing[:min(5, len(missing))], ndBefore)

	// Apply migration 117 through the real runner (file body, not a paraphrase).
	body, err := migrations.FS.ReadFile("117_selector_scope_statistics.sql")
	if err != nil {
		t.Fatalf("read migration 117: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("migration 117 is empty")
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply remaining migrations (117): %v", err)
	}

	if target = w024StatTarget(t, pool); target == nil || *target != 1000 {
		t.Errorf("attstattarget after 117 = %v, want 1000", target)
	}

	after, _, ndAfter, reltuples := w024MCV(t, pool)
	if missing := w024Missing(want, after); len(missing) != 0 {
		t.Errorf("after migration 117 the MCV list still misses %d fixture scopes (e.g. %v)",
			len(missing), missing[:min(5, len(missing))])
	} else {
		t.Logf("G4 GREEN: after 117 the MCV list holds all %d fixture scopes (%d entries), n_distinct=%v, reltuples=%v",
			scopes, len(after), ndAfter, reltuples)
	}

	// The migration's own ANALYZE is what makes the target effective — no
	// second ANALYZE ran between the two measurements.
	if len(after) <= len(before) {
		t.Errorf("MCV list did not grow across the migration: %d → %d", len(before), len(after))
	}
}

// w024Missing returns the entries of want that are absent from got.
func w024Missing(want, got []string) []string {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[g] = true
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// w024StatTarget reads pg_attribute.attstattarget for context_blocks.scope.
// nil = cluster default (PG17+ encoding).
func w024StatTarget(t *testing.T, pool *pgxpool.Pool) *int {
	t.Helper()
	var target *int
	if err := pool.QueryRow(context.Background(), `
		SELECT a.attstattarget FROM pg_attribute a
		WHERE a.attrelid = 'public.context_blocks'::regclass AND a.attname = 'scope'`).Scan(&target); err != nil {
		t.Fatalf("read attstattarget: %v", err)
	}
	return target
}
