//go:build integration

// ChannelProbe (status.channel_probe_interval) gates against a real
// PG18+TimescaleDB+pgvector testcontainer (Evokoa-Clean-Room design/03 §4.7,
// W03-8). Gate numbers reference the wave brief.
//
//	go test -tags=integration ./internal/handler/ -run TestChannelProbe -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/testdb"
)

// probeTestModel/probeTestForeignModel are the two embed-model identities
// exercised by the Gate 2/3/5 fixtures below — distinct strings so a
// wrong-model cache row is unambiguously "foreign".
const (
	probeTestModel        = "probe-test-model"
	probeTestForeignModel = "probe-test-FOREIGN-model"
)

// insertProbeBlock inserts one embedded, retrieval-visible context_blocks
// row (type_name='knowledge', the builtin type every other integration test
// in this package already relies on with no extra registry setup, e.g.
// internal/rrf's w021Insert).
func insertProbeBlock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope, title, content string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, embedding, scope, type_name)
		 VALUES ('t', $1, $2, $3::vector, $4, 'knowledge')`,
		title, content, vecLit(1024), scope,
	); err != nil {
		t.Fatalf("insert probe block: %v", err)
	}
}

// insertProbeCacheRow inserts one context_embed_cache row for model, keyed
// by a hash of textPreview (unique per distinct text, satisfying the
// (text_hash, model) PK across multiple rows in the same test).
func insertProbeCacheRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, model, textPreview string, hitCount int) {
	t.Helper()
	sum := sha256.Sum256([]byte(model + "|" + textPreview))
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview, hit_count, last_access)
		 VALUES ($1, $2, $3::vector, $4, $5, now())`,
		sum[:], model, vecLit(1024), textPreview, hitCount,
	); err != nil {
		t.Fatalf("insert probe cache row: %v", err)
	}
}

// TestChannelProbeActiveGate2 is Gate 2 ("Aktiv-Probe"): with a seeded,
// embedded, retrieval-visible block and a matching context_embed_cache row
// for the current model, runChannelProbe must return all four channel
// latencies non-nil plus a MeasuredAt timestamp. The fulltext/trigram
// channels are exercised with a real match (title/content share the cache
// row's text_preview word) so all four channels do REAL work, not just
// "the query executed over zero candidate rows".
func TestChannelProbeActiveGate2(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insertProbeBlock(t, ctx, pool, "private", "Widget Baseline", "widget baseline retrieval fixture content")
	insertProbeCacheRow(t, ctx, pool, probeTestModel, "widget", 5)

	before := time.Now()
	row := runChannelProbe(ctx, pool, probeTestModel, []string{"private"}, []string{"knowledge"})
	after := time.Now()

	if row == nil {
		t.Fatalf("runChannelProbe returned nil — want a populated probeRow (cache row + matching block are seeded)")
	}
	if row.SemanticMs == nil {
		t.Error("SemanticMs is nil, want a measured latency")
	}
	if row.FtsDeMs == nil {
		t.Error("FtsDeMs is nil, want a measured latency")
	}
	if row.FtsEnMs == nil {
		t.Error("FtsEnMs is nil, want a measured latency")
	}
	if row.TrigramMs == nil {
		t.Error("TrigramMs is nil, want a measured latency")
	}
	if row.MeasuredAt.Before(before) || row.MeasuredAt.After(after) {
		t.Errorf("MeasuredAt = %v, want between %v and %v", row.MeasuredAt, before, after)
	}
}

// TestChannelProbeCacheEmptyGate3 is Gate 3 ("Cache-leer"): with no
// context_embed_cache row at all — or a row that exists only for a
// DIFFERENT model — runChannelProbe returns nil, never an error, and
// (documented structurally below) can never make an embed/LLM wire call.
func TestChannelProbeCacheEmptyGate3(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	insertProbeBlock(t, ctx, pool, "private", "Widget Baseline", "widget baseline retrieval fixture content")

	t.Run("no_cache_row_at_all", func(t *testing.T) {
		row := runChannelProbe(ctx, pool, probeTestModel, []string{"private"}, []string{"knowledge"})
		if row != nil {
			t.Errorf("runChannelProbe = %+v, want nil (empty cache — documented skip, never an error)", row)
		}
	})

	t.Run("cache_row_present_but_wrong_model", func(t *testing.T) {
		insertProbeCacheRow(t, ctx, pool, probeTestForeignModel, "widget", 5)
		row := runChannelProbe(ctx, pool, probeTestModel, []string{"private"}, []string{"knowledge"})
		if row != nil {
			t.Errorf("runChannelProbe = %+v, want nil (only a FOREIGN-model row exists — the WHERE model=$1 filter must reject it)", row)
		}
	})
}

// TestChannelProbeNoWireCallStructuralGate3 documents Gate 3's second half
// as a structural proof rather than a mock call-counter (the wave brief's
// own suggested fallback: "die Probe hat strukturell keinen
// Backend-Zugriff"). runChannelProbe's ENTIRE parameter surface is
// (ctx, pool, embedModel string, scopes, visibleTypes []string) — no
// embed.Client, *backends.Pool, dispatch.Admitter or any other handle
// capable of issuing an embed/LLM wire call is threaded in anywhere along
// its call path (verified by reading status_db.go: runChannelProbe only
// ever touches pgxpool.Pool/pgx.Tx). A function that never receives a
// client cannot call one — there is nothing to mock or count, which is
// exactly Gate 3's "kein Embed-Wire-Call" requirement satisfied by
// construction, independent of the cache-hit/cache-miss branch under test
// in TestChannelProbeCacheEmptyGate3 above.
func TestChannelProbeNoWireCallStructuralGate3(t *testing.T) {
	t.Log("structural proof, not a runtime assertion — see the doc comment: " +
		"runChannelProbe(ctx context.Context, pool *pgxpool.Pool, embedModel string, scopes, visibleTypes []string) *probeRow " +
		"carries no embed/LLM client parameter anywhere in its signature or call graph")
}

// TestChannelProbeModelFilterGate5 is Gate 5 ("Modell-Filter-Probe"): a
// FOREIGN-model cache row with the highest hit_count must NOT win the probe
// — the model-filtered row wins, or the probe skips (Gate 3), but a foreign
// row is never used. Reproducing the design doc's literal "andere
// Dimension" framing is not possible under the CURRENT schema
// (context_embed_cache.embedding is a fixed vector(1024) column — every row,
// any model, is exactly 1024-dim by construction; inserting a
// different-length literal is rejected by Postgres before this code ever
// runs). This test instead proves the load-bearing mechanism directly: (a)
// the SAME SQL runChannelProbe uses (WHERE model = $1 ORDER BY hit_count
// DESC) resolves to the target-model row even though a higher-hit_count
// foreign-model row exists, and (b) the end-to-end call succeeds (no SQL
// error, non-nil result) when given the target model — the "kein
// Dimensions-Fehler" half of the gate, satisfied here by construction
// rather than by exercising an unreachable schema state.
func TestChannelProbeModelFilterGate5(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	insertProbeBlock(t, ctx, pool, "private", "Widget Baseline", "widget baseline retrieval fixture content")

	insertProbeCacheRow(t, ctx, pool, probeTestForeignModel, "foreign-text", 1000) // highest hit_count, WRONG model
	insertProbeCacheRow(t, ctx, pool, probeTestModel, "widget", 1)                 // lowest hit_count, TARGET model

	// (a) the literal lookup query, isolated from the rest of runChannelProbe.
	var gotPreview string
	if err := pool.QueryRow(ctx, `
		SELECT text_preview FROM context_embed_cache
		 WHERE model = $1
		 ORDER BY hit_count DESC, last_access DESC
		 LIMIT 1`, probeTestModel,
	).Scan(&gotPreview); err != nil {
		t.Fatalf("model-filtered lookup: %v", err)
	}
	if gotPreview != "widget" {
		t.Fatalf("model-filtered lookup returned text_preview=%q, want the TARGET model's row (%q) — the foreign row's hit_count=1000 must not win once the model filter is applied", gotPreview, "widget")
	}

	// (b) end-to-end: no error, a real result, using the target model.
	row := runChannelProbe(ctx, pool, probeTestModel, []string{"private"}, []string{"knowledge"})
	if row == nil {
		t.Fatalf("runChannelProbe(target model) = nil, want a populated probeRow")
	}
	if row.SemanticMs == nil {
		t.Error("SemanticMs is nil — a dimension mismatch (had the foreign row been used) would have surfaced as a failed query here")
	}
}

// probeScatterVecFloats generates a deterministic-but-scattered 1024-dim
// embedding from seed, using math/rand rather than vecLit's cyclic d%7
// pattern. This distinction mattered empirically while writing Gate 4: an
// EARLIER version of this fixture used a d%7-cyclic pattern shifted by seed
// (a phase-rotation of the SAME repeating sequence for every row) — those
// vectors are near-collinear in 1024-dim space (barely distinguishable by
// cosine distance), which degenerated the HNSW ANN scan into returning ZERO
// rows in BOTH the with- and without-SET-LOCAL cases alike (a vacuous,
// same-on-both-sides non-signal, not the expected collapse). Genuine
// per-dimension randomness (this version) reproduces the real, sharply
// asymmetric ROT signal this wave's development first observed with a
// throwaway math/rand-based fixture (2 rows without SET LOCAL vs. 75 with
// it, 3000 noise / 80 target rows) — "scattered" has to mean actually
// scattered, not just phase-shifted.
func probeScatterVecFloats(seed int) []float32 {
	r := rand.New(rand.NewSource(int64(seed)))
	v := make([]float32, 1024)
	for d := range v {
		v[d] = r.Float32()
	}
	return v
}

// probeScatterVecLit renders probeScatterVecFloats(seed) as a pgvector text
// literal, for call sites that need a `::vector::halfvec(1024)`-castable SQL
// string rather than a typed pgvec.HalfVector bind parameter (the
// referenceStmt wrapper below, which — like the live ctx_rrf definition it
// extracts from — expects a castable literal in that position).
func probeScatterVecLit(seed int) string {
	v := probeScatterVecFloats(seed)
	var b strings.Builder
	b.WriteByte('[')
	for d, f := range v {
		if d > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%.6f", f)
	}
	b.WriteByte(']')
	return b.String()
}

// insertProbeBlockVec is insertProbeBlock with an explicit embedding literal
// (Gate 4's scattered corpus) instead of the shared static vecLit(1024).
func insertProbeBlockVec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope, title, embLit string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, embedding, scope, type_name)
		 VALUES ('t', $1, $1, $2::vector, $3, 'knowledge')`,
		title, embLit, scope,
	); err != nil {
		t.Fatalf("insert scattered probe block: %v", err)
	}
}

// extractCTEBody pulls the balanced-paren body of "name AS (" ... ")" out of
// a pg_get_functiondef() text — the SAME "extract via string surgery"
// technique internal/rrf/gen15_w021_integration_test.go's g15MakeVariant
// already establishes for isolating pieces of ctx_rrf's live definition.
func extractCTEBody(def, name string) (string, bool) {
	marker := name + " AS ("
	idx := strings.Index(def, marker)
	if idx < 0 {
		return "", false
	}
	start := idx + len(marker)
	depth := 1
	for i := start; i < len(def); i++ {
		switch def[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return def[start:i], true
			}
		}
	}
	return "", false
}

// TestChannelProbePlanParityGate4 is Gate 4 ("Plan-Paritäts-Gate"). It has
// three parts, and the middle one required an empirical correction to the
// design doc's literal wording (documented inline, the SAME posture
// status_db_integration_test.go's TestStatusDBHNSWReltuplesDenominator
// already takes for a different pgvector/PG18 empirical surprise):
//
//  1. STRUCTURAL parity: channelProbeSemanticSQL's predicate/order/limit is
//     checked against the semantic_ann CTE extracted LIVE from
//     pg_get_functiondef('ctx_rrf') — the canonical source of truth — via
//     substring checks on the load-bearing fragments. This is drift-proof:
//     if 112 (or its successor) changes the CTE's shape, this assertion is
//     what would need to change too.
//  2. SCAN-TYPE parity (the literal Gate ask): EXPLAIN of the probe's OWN
//     semantic statement (in a probe-TX, WITH the SET LOCAL) and EXPLAIN of
//     the LIVE-extracted semantic_ann CTE (wrapped standalone via a `params`
//     CTE cross-join — Postgres never exposes a PL/pgSQL function's internal
//     plan through a bare `EXPLAIN SELECT * FROM ctx_rrf(...)`, which only
//     ever shows an opaque "Function Scan on ctx_rrf" node — so the
//     extracted-CTE wrapper is the only way to get a comparable plan at
//     all) both show "Index Scan using idx_embedding_hnsw". `enable_seqscan
//     = off; enable_sort = off` is forced for BOTH sides (the W02-1-G3
//     technique, internal/rrf/gen15_w021_integration_test.go) — on this
//     tiny testcontainer corpus the planner otherwise picks a cheap
//     type_name-index-then-sort plan for EITHER statement regardless of any
//     hnsw.* GUC, which would make the comparison vacuous rather than red.
//  3. EMPIRICAL CORRECTION (documented, not the design doc's literal
//     wording): live-probed against PG18/pgvector 0.8.2, `SET LOCAL
//     hnsw.iterative_scan` does NOT change the bare-EXPLAIN plan NODE TYPE
//     at all once enable_seqscan is forced off — both with and without it,
//     EXPLAIN shows the identical "Index Scan using idx_embedding_hnsw"
//     text (pgvector's iterative-scan feature is a RUNTIME candidate-
//     rescan behavior of the index AM, not a plan-shape decision the
//     planner encodes into the EXPLAIN tree). What DOES diverge — sharply —
//     is the ROW COUNT actually returned under a scope/type filter that
//     excludes most of the ANN search's nearest candidates: a live
//     measurement during this wave's development (3000 scope='other' noise
//     rows + 80 scope='private' target rows, scattered embeddings, LIMIT
//  75. returned 2 rows WITHOUT the SET LOCAL and 75 WITH it — the
//     "weicht der Plan ab" ROT signal design/03 §4.7 asks for is real, it
//     just manifests as a recall/row-count collapse (EXPLAIN ANALYZE actual
//     rows), not a different plan-tree TEXT. This test asserts BOTH: the
//     GREEN scan-type parity (part 2) AND the ROT row-count collapse
//     without the SET LOCAL, reproduced live in-container rather than
//     merely cited from the design's development notes.
func TestChannelProbePlanParityGate4(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_functiondef(oid) FROM pg_proc WHERE proname = 'ctx_rrf'`,
	).Scan(&def); err != nil {
		t.Fatalf("pg_get_functiondef(ctx_rrf): %v", err)
	}
	cte, ok := extractCTEBody(def, "semantic_ann")
	if !ok {
		t.Fatalf("semantic_ann CTE not found in the live ctx_rrf definition — 112_rrf_gen15_dual_arm.sql's CTE name changed?")
	}

	// --- Part 1: structural parity against the LIVE-extracted CTE ---
	for _, frag := range []string{
		"cb.type_name = ANY(p_types_visible)",
		"cb.scope = ANY(p_scopes)",
		"cb.embedding::halfvec(1024) <=> p_embedding",
		"NOT cb.is_archived",
		"LIMIT 75",
	} {
		if !strings.Contains(cte, frag) {
			t.Errorf("live semantic_ann CTE no longer contains %q — channelProbeSemanticSQL (status_db.go) has drifted from the canonical source and needs updating", frag)
		}
	}
	for _, frag := range []string{
		"cb.type_name = ANY($1)",
		"cb.scope = ANY($2)",
		"cb.embedding::halfvec(1024) <=> $3",
		"NOT cb.is_archived",
		"LIMIT 75",
	} {
		if !strings.Contains(channelProbeSemanticSQL, frag) {
			t.Errorf("channelProbeSemanticSQL no longer contains %q", frag)
		}
	}

	// Seed a scope/type-selective corpus with SCATTERED embeddings (per Gate
	// 4's own ROT part: a clustered corpus — e.g. every row sharing vecLit's
	// one static literal — makes the ANN scan trivially satisfy the LIMIT
	// regardless of iterative_scan, which would make the row-count
	// comparison below vacuous rather than red; this exact shape — 3000
	// noise / 80 target rows, scattered — is what this wave's development
	// verified live before writing this assertion, see the doc comment).
	for i := 0; i < 3000; i++ {
		insertProbeBlockVec(t, ctx, pool, "other", fmt.Sprintf("noise-%d", i), probeScatterVecLit(i))
	}
	for i := 0; i < 80; i++ {
		insertProbeBlockVec(t, ctx, pool, "private", fmt.Sprintf("target-%d", i), probeScatterVecLit(10000+i))
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	queryVecLit := probeScatterVecLit(999999)                           // for the referenceStmt's explicit $3::vector::halfvec(1024) cast
	queryVecTyped := pgvec.NewHalfVector(probeScatterVecFloats(999999)) // for channelProbeSemanticSQL's uncast $3 (matches production's bind type)

	var debugCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE scope='private' AND type_name='knowledge' AND NOT is_archived`).Scan(&debugCount); err != nil {
		t.Fatalf("debug count: %v", err)
	}
	t.Logf("DEBUG: %d rows match scope=private/type_name=knowledge/not-archived", debugCount)

	explainPlan := func(sql string, withSetLocal bool, args ...any) string {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// W02-1-G3 technique: forces deterministic HNSW selection on a tiny
		// testcontainer corpus (internal/rrf/gen15_w021_integration_test.go).
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_sort = off`); err != nil {
			t.Fatalf("set local seqscan/sort off: %v", err)
		}
		if withSetLocal {
			if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
				t.Fatalf("set local iterative_scan: %v", err)
			}
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan explain line: %v", err)
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		return b.String()
	}

	// --- Part 2: scan-type parity ---
	probePlan := explainPlan(channelProbeSemanticSQL, true, []string{"knowledge"}, []string{"private"}, queryVecTyped)
	t.Logf("probe semantic statement EXPLAIN (WITH SET LOCAL):\n%s", probePlan)
	if !strings.Contains(probePlan, "Index Scan using idx_embedding_hnsw") {
		t.Errorf("probe semantic statement did not use idx_embedding_hnsw:\n%s", probePlan)
	}

	crossJoined := strings.Replace(cte, "FROM context_blocks cb", "FROM context_blocks cb, params", 1)
	if crossJoined == cte {
		t.Fatalf("cross-join substitution was a no-op — the extracted CTE text no longer matches the expected FROM-clause shape")
	}
	referenceStmt := fmt.Sprintf(`
WITH params AS (
  SELECT $1::text[] AS p_types_visible,
         $2::text[] AS p_scopes,
         NULL::text[] AS p_types_exclude,
         NULL::text AS p_category,
         NULL::text[] AS p_tags,
         NULL::text[] AS p_categories_exclude,
         NULL::uuid[] AS p_granted_block_ids,
         'ann'::text AS p_semantic_mode,
         $3::vector::halfvec(1024) AS p_embedding
),
semantic_ann AS (%s)
SELECT * FROM semantic_ann`, crossJoined)
	referencePlan := explainPlan(referenceStmt, true, []string{"knowledge"}, []string{"private"}, queryVecLit)
	t.Logf("ctx_rrf-extracted semantic_ann EXPLAIN (WITH SET LOCAL):\n%s", referencePlan)
	if !strings.Contains(referencePlan, "Index Scan using idx_embedding_hnsw") {
		t.Errorf("ctx_rrf-extracted semantic_ann did not use idx_embedding_hnsw:\n%s", referencePlan)
	}

	// --- Part 3: empirical ROT correction, reproduced live ---
	rowCount := func(withSetLocal bool) int {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off; SET LOCAL enable_sort = off`); err != nil {
			t.Fatalf("set local seqscan/sort off: %v", err)
		}
		if withSetLocal {
			if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
				t.Fatalf("set local iterative_scan: %v", err)
			}
		}
		rows, err := tx.Query(ctx, channelProbeSemanticSQL, []string{"knowledge"}, []string{"private"}, queryVecTyped)
		if err != nil {
			t.Fatalf("row-count query: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("row-count iteration: %v", err)
		}
		return n
	}
	withN := rowCount(true)
	withoutN := rowCount(false)
	t.Logf("ROT signal: rows returned WITH SET LOCAL=%d, WITHOUT=%d (design/03 §4.7's \"weicht der Plan ab\" — same EXPLAIN scan-type, collapsed row count/recall without it)", withN, withoutN)
	if withoutN >= withN {
		t.Errorf("expected WITHOUT SET LOCAL to return fewer rows than WITH it (recall collapse under a selective filter) — got without=%d, with=%d; the ROT signal did not reproduce", withoutN, withN)
	}
}
