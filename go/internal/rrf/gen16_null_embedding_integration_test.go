//go:build integration

// A-W2 gate (Issue #40 "Bug 5"): the ctx_rrf Generation 16 ann arm must not
// hand ranks to blocks whose embedding is NULL. Until Generation 15 the
// filter lived in the exact arm only ("Semantik-Delta 2"), which froze the
// Gen-14 behaviour: under a sequential-scan plan `ORDER BY <=>` sorts NULL
// distances last, so as soon as fewer than 75 embedded candidates exist the
// remainder of the semantic channel — the heaviest RRF weight (0.45) — fills
// up with unembedded rows in heap order. They carry a NULL cosine and still
// outscore a perfect full-text rank-1 hit.
//
// RED-probe doctrine of this suite (see gen15_w021_integration_test.go): the
// red side is permanent and in-suite. It runs the same fixture against a
// TEST-LOCAL variant derived from the live function definition with the ann
// arm's filter surgically removed — never against a migration.
//
// Historic RED probe, run against the unmigrated tree (chain ending at 133,
// Generation 15) before 134_rrf_gen16_ann_embedding_filter.sql existed:
//
//	returned=75 null_rows=55 first_null_rank=21
//
// which reproduces findings §4d of the issue analysis exactly.
//
//	go test -tags=integration ./internal/rrf/ -run TestGen16AW2 -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// aw2Call invokes fn in ann mode with a limit above the semantic arm's own
// LIMIT 75, so the assertion sees the whole semantic channel rather than a
// truncated head (g15Call caps at 50 and would hide 25 of the 55 leaked rows).
func aw2Call(ctx context.Context, q g15Querier, fn string, emb []float32, scopes, visible []string) ([]g15Row, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT id, rrf_score, cosine_sim FROM %s($1, 'zzqqxx', 'zzqqxx', $2::text[],
			p_limit => 200,
			p_types_visible => $3::text[],
			p_semantic_mode => 'ann')`, fn),
		pgvNewHalfVec(emb), scopes, visible)
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

// aw2NullStats reports how many rows carry a NULL cosine and at which 1-based
// position the first of them sits (0 = none), which is the shape findings §4d
// measured against the live database.
func aw2NullStats(rows []g15Row) (nullRows, firstNullRank int) {
	for i, r := range rows {
		if r.cos == nil {
			nullRows++
			if firstNullRank == 0 {
				firstNullRank = i + 1
			}
		}
	}
	return nullRows, firstNullRank
}

// TestGen16AW2_ANNArmExcludesNullEmbeddings is the fresh-install fixture: a
// visible type with 20 embedded and 55 unembedded blocks, forced onto a
// sequential-scan plan (the plan under which the leak exists at all — under
// the HNSW index scan pgvector never returns unindexed NULL vectors, which is
// why the live corpus never saw it).
//
// GREEN after Gen 16: not a single result carries a NULL cosine, and the 20
// embedded blocks still occupy ranks 1-20 in unchanged distance order — the
// filter removes the junk without touching the good part of the channel.
// RED (permanent, in-suite): the same fixture against a variant whose ann arm
// lost the filter leaks all 55, starting at rank 21.
func TestGen16AW2_ANNArmExcludesNullEmbeddings(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	emb := w021Query()

	const (
		scope     = "aw2s"
		embedded  = 20
		unembeded = 55
	)
	// Strictly increasing distance to the query vector → ranks 1..20 are
	// deterministic and the order assert needs no tie caveat.
	wantOrder := make([]string, embedded)
	for i := 0; i < embedded; i++ {
		wantOrder[i] = fmt.Sprintf("019fa402-0000-7000-9000-0000000030%02d", i+1)
		w021Insert(t, pool, wantOrder[i], scope, "knowledge", false, w021Embedding(i), now)
	}
	for i := 0; i < unembeded; i++ {
		w021Insert(t, pool, fmt.Sprintf("019fa402-0000-7000-9000-0000000031%02d", i+1),
			scope, "knowledge", false, nil, now)
	}

	// The leak needs a heap-order plan; an HNSW index scan would hide it.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	inSeqScanTx := func(fn string) []g15Row {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, aw2SeqScanGUCs); err != nil {
			t.Fatalf("set local: %v", err)
		}
		rows, err := aw2Call(ctx, tx, fn, emb, []string{scope}, testVisibleTypes)
		if err != nil {
			t.Fatalf("%s call in seq-scan tx: %v", fn, err)
		}
		return rows
	}

	// Proof that the fixture really exercises the heap-order path — otherwise
	// a planner change would make both sides of this gate vacuous.
	aw2AssertNoHNSWScan(t, pool, conn, emb, scope)

	rows := inSeqScanTx("ctx_rrf")
	nullRows, firstNullRank := aw2NullStats(rows)
	t.Logf("Gen 16: returned=%d null_rows=%d first_null_rank=%d", len(rows), nullRows, firstNullRank)
	if nullRows != 0 {
		t.Errorf("ann arm surfaced %d NULL-embedding rows (first at rank %d) — embedding IS NOT NULL filter missing",
			nullRows, firstNullRank)
	}

	// Non-regression: the good part of the channel is untouched.
	if len(rows) < embedded {
		t.Fatalf("ann arm returned %d rows, want at least the %d embedded fixtures", len(rows), embedded)
	}
	for i := 0; i < embedded; i++ {
		if rows[i].id != wantOrder[i] {
			t.Errorf("rank %d: id = %s, want %s (embedded order regression)", i+1, rows[i].id, wantOrder[i])
		}
	}

	// RED probe: the same call against an ann arm without the filter. The
	// surgery is scoped to the text BEFORE the exact arm, so it can never
	// silently mutilate the exact arm's own filter instead.
	g15MakeVariant(t, pool, "ctx_rrf_aw2_unfiltered", func(def string) string {
		return aw2StripANNFilter(t, def)
	})
	redRows := inSeqScanTx("ctx_rrf_aw2_unfiltered")
	redNulls, redFirst := aw2NullStats(redRows)
	t.Logf("RED (unfiltered ann arm): returned=%d null_rows=%d first_null_rank=%d",
		len(redRows), redNulls, redFirst)
	if redNulls != unembeded {
		t.Errorf("RED probe: unfiltered ann arm leaked %d NULL-embedding rows, want %d", redNulls, unembeded)
	}
	if redFirst != embedded+1 {
		t.Errorf("RED probe: first NULL row at rank %d, want %d", redFirst, embedded+1)
	}
}

// aw2StripANNFilter removes the embedding filter from the ann arm only. It
// splits the definition at the exact arm's CTE header and works on the head,
// so the exact arm keeps its own filter no matter how the two arms drift; the
// head must contain the filter exactly once, otherwise the red probe would be
// mutilating something else than it claims.
func aw2StripANNFilter(t *testing.T, def string) string {
	t.Helper()
	const (
		exactMarker = "exact_pool AS MATERIALIZED"
		filter      = "AND cb.embedding IS NOT NULL"
	)
	cut := strings.Index(def, exactMarker)
	if cut < 0 {
		t.Fatalf("marker %q not found in function body", exactMarker)
	}
	head, tail := def[:cut], def[cut:]
	if n := strings.Count(head, filter); n != 1 {
		t.Fatalf("ann arm carries %d occurrences of %q, want exactly 1", n, filter)
	}
	return strings.Replace(head, filter, "", 1) + tail
}

// aw2HNSWIndexes reads the names of every HNSW index on context_blocks from
// the catalog instead of hard-coding idx_embedding_hnsw — the gate's real
// subject is "no vector index was used", and an index rename must not turn
// that into a silent pass.
func aw2HNSWIndexes(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT indexname FROM pg_indexes
		 WHERE tablename = 'context_blocks' AND indexdef ILIKE '%USING hnsw%'`)
	if err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_indexes rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no HNSW index on context_blocks — the plan probe would assert nothing")
	}
	return out
}

// aw2AssertNoHNSWScan pins the precondition of the whole gate. The load-bearing
// half is the vector index: under an HNSW scan pgvector never returns an
// unindexed NULL vector, so both sides of the gate would be vacuous. That half
// is asserted structurally (no `Index Scan using <hnsw index>` in the plan) and
// survives a planner or statistics change.
//
// The second half — that the plan is specifically a Seq Scan — is a
// consequence of the GUCs above, not an independent property, and it is
// asserted because it is what the design prescribes. Note that disabling index
// scans alone is NOT enough: the planner then reaches the heap by a Bitmap Heap
// Scan over idx_context_scope_active (observed, statistics-dependent), which
// leaks exactly the same way but makes a naive `Seq Scan` pin flaky. Hence
// enable_bitmapscan = off in aw2SeqScanGUCs.
const aw2SeqScanGUCs = `SET LOCAL enable_indexscan = off;
	SET LOCAL enable_indexonlyscan = off;
	SET LOCAL enable_bitmapscan = off`

func aw2AssertNoHNSWScan(t *testing.T, pool *pgxpool.Pool, conn *pgxpool.Conn, emb []float32, scope string) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin (plan probe): %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, aw2SeqScanGUCs); err != nil {
		t.Fatalf("set local (plan probe): %v", err)
	}
	rows, err := tx.Query(ctx,
		`EXPLAIN (COSTS OFF) SELECT cb.id FROM context_blocks cb
		 WHERE NOT cb.is_archived AND cb.scope = $2
		 ORDER BY cb.embedding::halfvec(1024) <=> $1 LIMIT 75`,
		pgvNewHalfVec(emb), scope)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan += line + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}

	for _, idx := range aw2HNSWIndexes(t, pool) {
		if strings.Contains(plan, "Index Scan using "+idx) || strings.Contains(plan, "Index Only Scan using "+idx) {
			t.Fatalf("fixture reaches context_blocks through the HNSW index %s — gate would be vacuous:\n%s", idx, plan)
		}
	}
	if !strings.Contains(plan, "Seq Scan on context_blocks") {
		t.Fatalf("fixture does not use a sequential scan — GUCs no longer force the heap-order path:\n%s", plan)
	}
}
