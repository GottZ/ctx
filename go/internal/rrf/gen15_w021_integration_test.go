//go:build integration

// W02-1 gates (design/02-strategy-selektor.md §7 "W02-1", §4.4, §5.1, §5.6)
// against a real PG18 testcontainer: the M112 ctx_rrf Generation 15 —
// dual-arm semantic CTE (ann/exact), one-time-filter gating, MATERIALIZED
// barrier, cap guard (TOCTOU, SQLSTATE 54000), fail-closed parameter
// validation, default compatibility of the legacy 15-arg call.
//
// RED-probe doctrine: every gate proves its red side first — either against
// the pre-112 state (G1: reconstructed in-container from the embedded 073
// body, plus the literal pre-file probe documented below) or against a
// deliberately mutilated TEST-LOCAL helper function derived from the live
// function definition (G2 skewed predicate, G3 barrier-less, G4 bound-less,
// G5 tiebreak-less, G6 guard-less). Helper variants are created via
// pg_get_functiondef + targeted string surgery, never as migrations.
//
// G1 pre-file RED probe (run 2026-07-24 BEFORE 112_rrf_gen15_dual_arm.sql
// existed, against the fresh chain ending at 111 — go:embed only carries
// files present at compile time):
//
//	SQLSTATE=42883 message="function ctx_rrf(unknown, unknown, unknown,
//	text[], unknown, unknown, integer, unknown, unknown, text[], unknown,
//	unknown, unknown, unknown, unknown, unknown, unknown, integer) does
//	not exist"
//
// The permanent in-suite red reconstruction below reproduces the same state
// by dropping the 18-param signature and re-executing the embedded 073 body
// (073 is the last pre-112 ctx_rrf definer — verified: no migration between
// 074 and 111 redefines the function).
//
//	go test -tags=integration ./internal/rrf/ -run TestGen15W021 -count=1 -v
package rrf_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// w021Insert writes a context_blocks row; emb == nil stores a NULL embedding
// (the Semantik-Delta-2 fixture class, §4.5).
func w021Insert(t *testing.T, pool *pgxpool.Pool, id, scope, typeName string, archived bool, emb []float32, ts time.Time) {
	t.Helper()
	var embParam interface{}
	if emb != nil {
		embParam = pgvec.NewVector(emb)
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, type_name, is_archived)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, $8, $9)`,
		id, "knowledge", "w021-title-"+id[len(id)-4:], "w021-content", scope, embParam, ts, typeName, archived)
	if err != nil {
		t.Fatalf("insert w021 block %s: %v", id, err)
	}
}

// w021Embedding returns the shared base embedding (all 0.1) with the first
// k+1 components raised to 0.9 — monotonically increasing distance to the
// base query vector, so semantic ranks are strictly ordered (no score ties;
// the G1 order assert stays valid without a tie caveat).
func w021Embedding(k int) []float32 {
	e := make([]float32, 1024)
	for i := range e {
		e[i] = 0.1
	}
	for i := 0; i <= k && i < len(e); i++ {
		e[i] = 0.9
	}
	return e
}

func w021Query() []float32 {
	e := make([]float32, 1024)
	for i := range e {
		e[i] = 0.1
	}
	return e
}

type g15Row struct {
	id    string
	score float64
	cos   *float64
}

type g15Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// g15Call invokes fn (ctx_rrf or a test-local variant) with the Gen-15
// parameter surface. mode/scanTuples/exactCap may be nil (SQL defaults).
func g15Call(ctx context.Context, q g15Querier, fn string, emb []float32,
	scopes, visible, exclude, granted []string, mode, scanTuples, exactCap interface{}) ([]g15Row, error) {
	var excludeParam, grantedParam interface{}
	if len(exclude) > 0 {
		excludeParam = exclude
	}
	if len(granted) > 0 {
		grantedParam = granted
	}
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT id, rrf_score, cosine_sim FROM %s($1, 'zzqqxx', 'zzqqxx', $2::text[],
			p_limit => 50,
			p_types_visible => $3::text[],
			p_types_exclude => $4::text[],
			p_granted_block_ids => $5::uuid[],
			p_semantic_mode => COALESCE($6, 'ann'),
			p_scan_tuples => $7::int,
			p_exact_cap => $8::int)`, fn),
		pgvNewHalfVec(emb), scopes, visible, excludeParam, grantedParam, mode, scanTuples, exactCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []g15Row
	for rows.Next() {
		var r g15Row
		if err := rows.Scan(&r.id, &r.score, &r.cos); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func g15IDSet(rows []g15Row) map[string]bool {
	s := make(map[string]bool, len(rows))
	for _, r := range rows {
		s[r.id] = true
	}
	return s
}

// g15MakeVariant derives a test-local mutilated function from the live Gen-15
// definition (pg_get_functiondef + string surgery). Every replacement must
// change the text — a silent no-op replacement means the migration body
// drifted and the red probe would be vacuous, so it fails loudly.
func g15MakeVariant(t *testing.T, pool *pgxpool.Pool, name string, mutate func(string) string) {
	t.Helper()
	ctx := context.Background()
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = 'ctx_rrf'`).Scan(&def); err != nil {
		t.Fatalf("pg_get_functiondef(ctx_rrf): %v", err)
	}
	renamed := strings.Replace(def, "FUNCTION public.ctx_rrf(", "FUNCTION public."+name+"(", 1)
	if renamed == def {
		t.Fatalf("variant %s: function-name replacement was a no-op", name)
	}
	mutated := mutate(renamed)
	if mutated == renamed {
		t.Fatalf("variant %s: body mutation was a no-op — migration body drifted?", name)
	}
	if _, err := pool.Exec(ctx, mutated); err != nil {
		t.Fatalf("create variant %s: %v", name, err)
	}
}

// replaceAfter replaces the first occurrence of old AFTER marker with new.
func replaceAfter(t *testing.T, s, marker, old, new string) string {
	t.Helper()
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("marker %q not found in function body", marker)
	}
	if !strings.Contains(s[i:], old) {
		t.Fatalf("%q not found after marker %q", old, marker)
	}
	return s[:i] + strings.Replace(s[i:], old, new, 1)
}

// TestGen15W021_G1_DefaultCompat is gate G1: the 18-arg call is 42883-red on
// the reconstructed Gen-14 state and green on Gen 15; the legacy 15-arg call
// returns the same result sets AND scores before/after the migration
// (default compatibility — W02-1 ships no Go change). Fixtures carry strictly
// distinct semantic distances, so the full row order is deterministic and the
// order assert needs no score-tie exclusion. Also pins single-signature and
// raw-body idempotency (double execution of the embedded 112 file).
func TestGen15W021_G1_DefaultCompat(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	emb := w021Query()

	ids := []string{
		"019f9001-0000-7000-9000-00000000a001",
		"019f9001-0000-7000-9000-00000000a002",
		"019f9001-0000-7000-9000-00000000a003",
		"019f9001-0000-7000-9000-00000000a004",
		"019f9001-0000-7000-9000-00000000a005",
	}
	for i, id := range ids {
		w021Insert(t, pool, id, "w021", "knowledge", false, w021Embedding(i), now)
	}

	call18 := func() (int, error) {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM ctx_rrf(
			$1, 'zzqqxx', 'zzqqxx', ARRAY['w021'], NULL, NULL, 10, NULL, NULL,
			ARRAY['knowledge'], NULL, NULL, NULL, NULL, NULL,
			'exact', NULL, 64)`, pgvNewHalfVec(emb)).Scan(&n)
		return n, err
	}
	call15 := func() []rrf.SearchResult {
		res, _, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
			[]string{"w021"}, nil, nil, 10, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
		if err != nil {
			t.Fatalf("15-arg rrf.Search: %v", err)
		}
		return res
	}

	// Green precondition on the fresh 112 chain: 18-arg call resolves.
	if n, err := call18(); err != nil || n != len(ids) {
		t.Fatalf("18-arg call on Gen 15: n=%d err=%v, want %d rows", n, err, len(ids))
	}

	// Reconstruct the pre-112 state: drop the 18-param signature, re-execute
	// the embedded Gen-14 body (073 — the last pre-112 definer).
	if _, err := pool.Exec(ctx,
		`DROP FUNCTION ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[], TEXT, INTEGER, INTEGER)`); err != nil {
		t.Fatalf("drop Gen-15 signature: %v", err)
	}
	body073, err := migrations.Section("073_rrf_policy_params.sql")
	if err != nil {
		t.Fatalf("read embedded 073: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body073)); err != nil {
		t.Fatalf("re-execute 073 body: %v", err)
	}

	// RED: 18-arg call against Gen 14 → SQLSTATE 42883.
	if _, err := call18(); err == nil {
		t.Fatal("G1 RED failed: 18-arg call succeeded against Gen 14")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42883" {
			t.Fatalf("G1 RED: want SQLSTATE 42883, got %v", err)
		}
		t.Logf("G1 RED: SQLSTATE=%s message=%q", pgErr.Code, pgErr.Message)
	}

	before := call15() // Gen-14 baseline

	// Migrate forward again: execute the embedded 112 body TWICE (raw-body
	// idempotency claim: DROP IF EXISTS both signatures + CREATE OR REPLACE).
	body112, err := migrations.Section("112_rrf_gen15_dual_arm.sql")
	if err != nil {
		t.Fatalf("read embedded 112: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, string(body112)); err != nil {
			t.Fatalf("execute 112 body (run %d): %v", i+1, err)
		}
	}
	var sigs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_proc WHERE proname = 'ctx_rrf'`).Scan(&sigs); err != nil {
		t.Fatalf("pg_proc: %v", err)
	}
	if sigs != 1 {
		t.Fatalf("ctx_rrf signatures after 112 double-run = %d, want exactly 1", sigs)
	}
	var args string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_function_identity_arguments(oid) FROM pg_proc WHERE proname = 'ctx_rrf'`).Scan(&args); err != nil {
		t.Fatalf("identity args: %v", err)
	}
	for _, want := range []string{"p_semantic_mode", "p_scan_tuples", "p_exact_cap"} {
		if !strings.Contains(args, want) {
			t.Errorf("Gen-15 signature misses %s (args: %s)", want, args)
		}
	}

	// GREEN: 18-arg call resolves again.
	if n, err := call18(); err != nil || n != len(ids) {
		t.Fatalf("18-arg call after 112: n=%d err=%v, want %d rows", n, err, len(ids))
	}

	// Default compatibility: identical sets, scores, cosines AND order (all
	// scores strictly distinct by fixture construction).
	after := call15()
	if len(before) != len(after) {
		t.Fatalf("15-arg row count before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.ID != a.ID {
			t.Errorf("row %d: id before=%s after=%s (order regression)", i, b.ID, a.ID)
			continue
		}
		if b.RRFScore != a.RRFScore {
			t.Errorf("row %d (%s): rrf_score before=%.17g after=%.17g", i, b.ID, b.RRFScore, a.RRFScore)
		}
		switch {
		case (b.CosineSim == nil) != (a.CosineSim == nil):
			t.Errorf("row %d (%s): cosine nil-ness differs", i, b.ID)
		case b.CosineSim != nil && *b.CosineSim != *a.CosineSim:
			t.Errorf("row %d (%s): cosine before=%.17g after=%.17g", i, b.ID, *b.CosineSim, *a.CosineSim)
		}
	}
	t.Logf("G1 GREEN: %d rows byte-equal in id/score/cosine across the migration", len(after))
}

// TestGen15W021_G2_ParitySentinel is the §5.1 permanent sentinel: identical
// call in both modes against the adversarial fixture corpus → equal result
// SETS. Since Migration 134 (ctx_rrf Generation 16) that is plain set
// equality: the declared Semantik-Delta 2 of Gen 15 — exact arm filters
// NULL embeddings, ann arm keeps the Gen-14 seq-scan behaviour — is gone,
// both arms carry the embedding filter, and the sentinel no longer needs an
// exception for the NULL-embedding fixture. RED: a skewed variant whose
// exact arm lost the type-allowlist conjunct MUST break parity (rogue block
// appears).
func TestGen15W021_G2_ParitySentinel(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
	emb := w021Query()

	const (
		home         = "w021h"
		foreign      = "w021f"
		idHome1      = "019f9002-0000-7000-9000-00000000b001"
		idHome2      = "019f9002-0000-7000-9000-00000000b002"
		idHome3      = "019f9002-0000-7000-9000-00000000b003"
		idRogue      = "019f9002-0000-7000-9000-00000000b004" // in-scope, rogue type
		idNull       = "019f9002-0000-7000-9000-00000000b005" // in-scope, NULL embedding
		idArch       = "019f9002-0000-7000-9000-00000000b006" // in-scope, archived
		idForeign    = "019f9002-0000-7000-9000-00000000b007" // foreign, not granted
		idGrantOnly  = "019f9002-0000-7000-9000-00000000b008" // foreign, granted
		idGrantExcl  = "019f9002-0000-7000-9000-00000000b009" // foreign, granted, excluded type
	)
	w021Insert(t, pool, idHome1, home, "knowledge", false, w021Embedding(0), now)
	w021Insert(t, pool, idHome2, home, "knowledge", false, w021Embedding(1), now)
	w021Insert(t, pool, idHome3, home, "knowledge", false, w021Embedding(2), now)
	w021Insert(t, pool, idRogue, home, "rogue", false, w021Embedding(3), now)
	w021Insert(t, pool, idNull, home, "knowledge", false, nil, now)
	w021Insert(t, pool, idArch, home, "knowledge", true, w021Embedding(4), now)
	w021Insert(t, pool, idForeign, foreign, "knowledge", false, w021Embedding(5), now)
	w021Insert(t, pool, idGrantOnly, foreign, "knowledge", false, w021Embedding(6), now)
	w021Insert(t, pool, idGrantExcl, foreign, "reference", false, w021Embedding(7), now)

	scopes := []string{home}
	exclude := []string{"reference"}
	granted := []string{idGrantOnly, idGrantExcl}
	expected := map[string]bool{idHome1: true, idHome2: true, idHome3: true, idGrantOnly: true}

	annRows, err := g15Call(ctx, pool, "ctx_rrf", emb, scopes, testVisibleTypes, exclude, granted, "ann", nil, nil)
	if err != nil {
		t.Fatalf("ann call: %v", err)
	}
	exactRows, err := g15Call(ctx, pool, "ctx_rrf", emb, scopes, testVisibleTypes, exclude, granted, "exact", nil, 64)
	if err != nil {
		t.Fatalf("exact call: %v", err)
	}
	annSet, exactSet := g15IDSet(annRows), g15IDSet(exactRows)

	// Hard exclusions hold in BOTH arms.
	for _, probe := range []struct{ id, label string }{
		{idRogue, "rogue-typed"}, {idArch, "archived"},
		{idForeign, "foreign non-granted"}, {idGrantExcl, "granted+excluded-type"},
	} {
		if annSet[probe.id] {
			t.Errorf("LEAK ann arm: %s block visible", probe.label)
		}
		if exactSet[probe.id] {
			t.Errorf("LEAK exact arm: %s block visible", probe.label)
		}
	}

	// NULL-embedding behaviour, asserted per arm. Migration 134 (ctx_rrf
	// Generation 16, Issue #40 Bug 5) retired the Gen-14 asymmetry: the ann
	// arm carries the same embedding filter as the exact arm, so neither arm
	// hands the NULL-embedding block a rank any more.
	if exactSet[idNull] {
		t.Error("exact arm surfaced the NULL-embedding block — embedding IS NOT NULL filter missing")
	}
	if annSet[idNull] {
		t.Error("ann arm surfaced the NULL-embedding block — Gen-16 embedding filter missing in the ann arm")
	}
	// Unreachable while the filter holds, kept as the second, independent
	// witness: a NULL-embedding block can never carry a cosine.
	for _, r := range annRows {
		if r.id == idNull && r.cos != nil {
			t.Errorf("NULL-embedding block carries non-NULL cosine %v in ann arm", *r.cos)
		}
	}

	// Parity: plain set equality — since Gen 16 both semantic arms are truly
	// set-equal on this fixture, so the sentinel needs no delta exception.
	if len(annSet) != len(expected) || len(exactSet) != len(expected) {
		t.Fatalf("parity size: ann=%v exact=%v want=%v", annSet, exactSet, expected)
	}
	for id := range expected {
		if !annSet[id] {
			t.Errorf("parity: %s missing in ann arm", id)
		}
		if !exactSet[id] {
			t.Errorf("parity: %s missing in exact arm", id)
		}
	}

	// RED probe: exact arm without the type-allowlist conjunct breaks parity.
	g15MakeVariant(t, pool, "ctx_rrf_g2_skewed", func(def string) string {
		return replaceAfter(t, def, "exact_pool AS MATERIALIZED",
			"AND cb.type_name = ANY(p_types_visible)", "")
	})
	skewedRows, err := g15Call(ctx, pool, "ctx_rrf_g2_skewed", emb, scopes, testVisibleTypes, exclude, granted, "exact", nil, 64)
	if err != nil {
		t.Fatalf("skewed exact call: %v", err)
	}
	skewedSet := g15IDSet(skewedRows)
	if !skewedSet[idRogue] {
		t.Error("G2 RED failed: skewed variant (allowlist conjunct removed from exact arm) did NOT surface the rogue block — sentinel would be blunt")
	} else {
		t.Logf("G2 RED: skewed exact arm surfaced rogue block %s — predicate divergence breaks parity, sentinel is sharp", idRogue)
	}
}

// TestGen15W021_G3_IndexUntouchability: with enable_seqscan=off +
// enable_sort=off (the only regime where the planner picks HNSW on a tiny
// corpus, Inventur §6.2), mode='ann' increments idx_embedding_hnsw.idx_scan,
// mode='exact' increments ZERO (MATERIALIZED barrier + one-time-filter gate).
// Stats assert runs after tx end via pg_stat_force_next_flush + poll (PG15+
// async stats). RED: a barrier-less variant (NOT MATERIALIZED pool with an
// ORDER-BY-distance shape) DOES touch the index in exact mode.
func TestGen15W021_G3_IndexUntouchability(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	emb := w021Query()
	for i := 0; i < 5; i++ {
		w021Insert(t, pool, fmt.Sprintf("019f9003-0000-7000-9000-00000000c%03d", i+1),
			"w021", "knowledge", false, w021Embedding(i), now)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	idxScan := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname = 'idx_embedding_hnsw'`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_user_indexes: %v", err)
		}
		return n
	}
	runInTx := func(fn, mode string, cap interface{}) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_sort = off`); err != nil {
			t.Fatalf("set local: %v", err)
		}
		if _, err := g15Call(ctx, tx, fn, emb, []string{"w021"}, testVisibleTypes, nil, nil, mode, nil, cap); err != nil {
			t.Fatalf("%s call in tx (mode=%s): %v", fn, mode, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// Force the writer backend to flush its pending stats (PG15+ async).
		if _, err := conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
			t.Fatalf("force flush: %v", err)
		}
	}
	pollAbove := func(floor int64) int64 {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if n := idxScan(); n > floor {
				return n
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("idx_scan never rose above %d within 15s", floor)
		return 0
	}

	s0 := idxScan()
	runInTx("ctx_rrf", "ann", nil)
	s1 := pollAbove(s0)
	dAnn := s1 - s0
	t.Logf("G3: ann call increments idx_embedding_hnsw.idx_scan by %d", dAnn)

	// exact call, then an ann marker call: once the marker's increment is
	// visible, the exact call's stats are flushed too (same backend, ordered) —
	// a zero-assert without a fixed sleep.
	runInTx("ctx_rrf", "exact", 64)
	runInTx("ctx_rrf", "ann", nil)
	s2 := pollAbove(s1)
	if s2-s1 != dAnn {
		t.Errorf("G3: exact call touched idx_embedding_hnsw (delta=%d, want %d from the ann marker alone)", s2-s1, dAnn)
	} else {
		t.Logf("G3 GREEN: exact call incremented idx_scan by ZERO (marker delta %d)", dAnn)
	}

	// RED probe: barrier-less exact arm (NOT MATERIALIZED + ORDER-BY-distance
	// pool shape) reaches the HNSW index — the same zero-assert would fail.
	g15MakeVariant(t, pool, "ctx_rrf_g3_nobarrier", func(def string) string {
		def = strings.Replace(def, "exact_pool AS MATERIALIZED (", "exact_pool AS NOT MATERIALIZED (", 1)
		return replaceAfter(t, def, "exact_pool AS NOT MATERIALIZED",
			"LIMIT p_exact_cap", "ORDER BY dist LIMIT 75")
	})
	s3 := idxScan()
	runInTx("ctx_rrf_g3_nobarrier", "exact", 64)
	runInTx("ctx_rrf", "ann", nil) // marker
	s4 := pollAbove(s3)
	if s4-s3 <= dAnn {
		t.Errorf("G3 RED failed: barrier-less exact arm did NOT touch idx_embedding_hnsw (delta=%d, marker=%d) — the MATERIALIZED barrier would be unproven", s4-s3, dAnn)
	} else {
		t.Logf("G3 RED: barrier-less exact arm touched idx_embedding_hnsw (delta=%d > marker %d) — barrier is load-bearing", s4-s3, dAnn)
	}
}

// TestGen15W021_G4_FailClosedValidation: unknown mode, non-positive and
// over-limit p_scan_tuples, and exact-without-cap all RAISE before any GUC or
// scan; a valid p_scan_tuples budget is SET-LOCAL-visible inside the calling
// tx and dead after it. RED: a bound-less body variant accepts 10^9 — the
// SQL-side upper bound (not the future Go clamp) is what rejects it.
func TestGen15W021_G4_FailClosedValidation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	emb := w021Query()
	w021Insert(t, pool, "019f9004-0000-7000-9000-00000000d001", "w021", "knowledge", false, w021Embedding(0), now)

	expectRaise := func(label string, mode, scanTuples, exactCap interface{}, wantSubstr string) {
		t.Helper()
		_, err := g15Call(ctx, pool, "ctx_rrf", emb, []string{"w021"}, testVisibleTypes, nil, nil, mode, scanTuples, exactCap)
		if err == nil {
			t.Fatalf("%s: call succeeded, want RAISE", label)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Fatalf("%s: want SQLSTATE P0001, got %v", label, err)
		}
		if !strings.Contains(pgErr.Message, wantSubstr) {
			t.Fatalf("%s: message %q misses %q", label, pgErr.Message, wantSubstr)
		}
		t.Logf("%s: SQLSTATE=%s message=%q", label, pgErr.Code, pgErr.Message)
	}

	expectRaise("mode=bogus", "bogus", nil, nil, "unknown semantic mode")
	expectRaise("scan_tuples=0", "ann", 0, nil, "invalid scan tuples budget")
	expectRaise("scan_tuples=-1", "ann", -1, nil, "invalid scan tuples budget")
	expectRaise("scan_tuples=10^9 (body upper bound)", "ann", 1000000000, nil, "invalid scan tuples budget")
	expectRaise("exact without cap", "exact", nil, nil, "requires positive p_exact_cap")
	expectRaise("exact cap=0", "exact", nil, 0, "requires positive p_exact_cap")
	expectRaise("exact cap=-5", "exact", nil, -5, "requires positive p_exact_cap")

	// SET-LOCAL visibility: budget live inside the tx, dead after tx end.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := g15Call(ctx, tx, "ctx_rrf", emb, []string{"w021"}, testVisibleTypes, nil, nil, "ann", 60000, nil); err != nil {
		t.Fatalf("ann call with scan_tuples=60000: %v", err)
	}
	var inTx string
	if err := tx.QueryRow(ctx, `SELECT current_setting('hnsw.max_scan_tuples')`).Scan(&inTx); err != nil {
		t.Fatalf("current_setting in tx: %v", err)
	}
	if inTx != "60000" {
		t.Errorf("hnsw.max_scan_tuples inside tx = %q, want 60000", inTx)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var afterTuples, afterScan *string
	if err := conn.QueryRow(ctx,
		`SELECT current_setting('hnsw.max_scan_tuples', true), current_setting('hnsw.iterative_scan', true)`).Scan(&afterTuples, &afterScan); err != nil {
		t.Fatalf("current_setting after tx: %v", err)
	}
	if afterTuples != nil && *afterTuples == "60000" {
		t.Error("hnsw.max_scan_tuples survived tx end — SET LOCAL semantics broken")
	}
	if afterScan != nil && *afterScan == "relaxed_order" {
		t.Error("hnsw.iterative_scan survived tx end — SET LOCAL semantics broken")
	}
	t.Logf("G4 GUC scope: in-tx=60000, after-tx tuples=%v scan=%v", strPtr(afterTuples), strPtr(afterScan))

	// RED probe: without the body upper bound the dangerous budget passes.
	g15MakeVariant(t, pool, "ctx_rrf_g4_unbounded", func(def string) string {
		return strings.Replace(def, "OR p_scan_tuples > 200000", "OR FALSE", 1)
	})
	if _, err := g15Call(ctx, pool, "ctx_rrf_g4_unbounded", emb, []string{"w021"}, testVisibleTypes, nil, nil, "ann", 1000000000, nil); err != nil {
		t.Errorf("G4 RED failed unexpectedly: bound-less variant rejected 10^9: %v", err)
	} else {
		t.Log("G4 RED: bound-less body accepted p_scan_tuples=10^9 — the in-body upper bound is the last line of defence, not decoration")
	}
}

func strPtr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// TestGen15W021_G5_TiebreakDeterminism: the exact arm's (dist, id) tiebreak
// makes distance-equal fixtures come back in ascending-id order, identically
// across runs (Ground-Truth reproducibility for Achse 01). RED: a
// tiebreak-less variant on the same fixtures (inserted in DESCENDING id
// order, so physical order deviates from id order) does not deliver the
// deterministic ascending order.
func TestGen15W021_G5_TiebreakDeterminism(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 16, 0, 0, 0, time.UTC)
	emb := w021Query()

	asc := []string{
		"019f9005-0000-7000-9000-00000000e001",
		"019f9005-0000-7000-9000-00000000e002",
		"019f9005-0000-7000-9000-00000000e003",
		"019f9005-0000-7000-9000-00000000e004",
		"019f9005-0000-7000-9000-00000000e005",
		"019f9005-0000-7000-9000-00000000e006",
	}
	// Insert in DESCENDING id order: physical (seq-scan) order ≠ id order.
	for i := len(asc) - 1; i >= 0; i-- {
		w021Insert(t, pool, asc[i], "w021", "knowledge", false, emb, now) // identical embedding ⇒ equal distance
	}

	order := func(fn string) []string {
		rows, err := g15Call(ctx, pool, fn, emb, []string{"w021"}, testVisibleTypes, nil, nil, "exact", nil, 64)
		if err != nil {
			t.Fatalf("%s exact call: %v", fn, err)
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.id
		}
		return ids
	}

	run1, run2 := order("ctx_rrf"), order("ctx_rrf")
	if fmt.Sprint(run1) != fmt.Sprint(run2) {
		t.Fatalf("two exact runs differ:\n  run1=%v\n  run2=%v", run1, run2)
	}
	if fmt.Sprint(run1) != fmt.Sprint(asc) {
		t.Fatalf("exact order = %v, want ascending ids %v (rank = (dist, id))", run1, asc)
	}
	t.Logf("G5 GREEN: two exact runs identical, ascending-id order on %d distance-equal fixtures", len(asc))

	// RED probe: tiebreak removed → order follows physical/plan order, not id.
	g15MakeVariant(t, pool, "ctx_rrf_g5_notiebreak", func(def string) string {
		if !strings.Contains(def, "ORDER BY ep.dist, ep.id") {
			t.Fatal("tiebreak ORDER BY not found in body")
		}
		return strings.ReplaceAll(def, "ORDER BY ep.dist, ep.id", "ORDER BY ep.dist")
	})
	red := order("ctx_rrf_g5_notiebreak")
	if fmt.Sprint(red) == fmt.Sprint(asc) {
		// Non-deterministic red: PG's sort MAY coincide with id order. Honest
		// report instead of a flaky failure — the green side above is the gate.
		t.Logf("G5 RED (best-effort): tiebreak-less order coincided with ascending ids this run (%v) — nondeterminism did not manifest", red)
	} else {
		t.Logf("G5 RED: tiebreak-less order deviates from ascending ids: %v", red)
	}
}

// TestGen15W021_G6_CapGuard (§5.6): scope with n blocks — p_exact_cap=n−1
// raises SQLSTATE 54000 exact_cap_hit (loud TOCTOU detection), p_exact_cap=n
// returns the full set; the grant addition counts against the cap. RED: a
// guard-less variant silently truncates to the pool LIMIT instead of raising.
func TestGen15W021_G6_CapGuard(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 17, 0, 0, 0, time.UTC)
	emb := w021Query()

	const n = 5
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("019f9006-0000-7000-9000-00000000f%03d", i+1)
		w021Insert(t, pool, ids[i], "w021g", "knowledge", false, w021Embedding(i), now)
	}
	const idForeignGrant = "019f9006-0000-7000-9000-00000000f101"
	w021Insert(t, pool, idForeignGrant, "w021g-foreign", "knowledge", false, w021Embedding(6), now)

	expect54000 := func(label string, granted []string, cap int) {
		t.Helper()
		_, err := g15Call(ctx, pool, "ctx_rrf", emb, []string{"w021g"}, testVisibleTypes, nil, granted, "exact", nil, cap)
		if err == nil {
			t.Fatalf("%s: call succeeded, want SQLSTATE 54000", label)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "54000" {
			t.Fatalf("%s: want SQLSTATE 54000, got %v", label, err)
		}
		if !strings.Contains(pgErr.Message, "exact_cap_hit") {
			t.Fatalf("%s: message %q misses exact_cap_hit", label, pgErr.Message)
		}
		t.Logf("%s: SQLSTATE=%s message=%q", label, pgErr.Code, pgErr.Message)
	}

	// cap = n−1 → guard fires (models the scope having grown past the cap).
	expect54000("cap=n-1", nil, n-1)

	// cap = n → full result set.
	rows, err := g15Call(ctx, pool, "ctx_rrf", emb, []string{"w021g"}, testVisibleTypes, nil, nil, "exact", nil, n)
	if err != nil {
		t.Fatalf("cap=n call: %v", err)
	}
	got := g15IDSet(rows)
	for _, id := range ids {
		if !got[id] {
			t.Errorf("cap=n: block %s missing from exact result", id)
		}
	}

	// Grant addition counts against the cap: n scope blocks + 1 grant > n.
	expect54000("cap=n with 1 grant", []string{idForeignGrant}, n)
	rows, err = g15Call(ctx, pool, "ctx_rrf", emb, []string{"w021g"}, testVisibleTypes, nil, []string{idForeignGrant}, "exact", nil, n+1)
	if err != nil {
		t.Fatalf("cap=n+1 with grant: %v", err)
	}
	if got := g15IDSet(rows); !got[idForeignGrant] {
		t.Errorf("cap=n+1: granted foreign block missing; got=%v", got)
	}

	// RED probe: guard disabled → the same cap=n−1 call silently truncates to
	// the pool LIMIT instead of raising (the "silent recall loss" failure mode
	// the guard exists to prevent).
	g15MakeVariant(t, pool, "ctx_rrf_g6_unguarded", func(def string) string {
		return strings.Replace(def, "> p_exact_cap THEN", "> p_exact_cap AND FALSE THEN", 1)
	})
	redRows, err := g15Call(ctx, pool, "ctx_rrf_g6_unguarded", emb, []string{"w021g"}, testVisibleTypes, nil, nil, "exact", nil, n-1)
	if err != nil {
		t.Fatalf("G6 RED: guard-less variant errored (%v) — expected silent truncation", err)
	}
	if len(redRows) != n-1 {
		t.Errorf("G6 RED: guard-less variant returned %d rows, want silent truncation to %d", len(redRows), n-1)
	} else {
		t.Logf("G6 RED: guard-less variant silently truncated %d blocks to %d rows without any error — the guard is what makes the race loud", n, n-1)
	}
}
