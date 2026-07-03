//go:build integration

// Wave I-J recall gate (design/02 §4.7, §7-I-J): the filtered-ANN recall probe.
// A 0.99 same-scope issue duplicate next to 100k foreign-scope rows MUST still
// be found by the same-scope guard. The hazard (§4.7): plain filtered-ANN
// examines only ef_search index candidates; if the foreign rows crowd that
// window, the scope predicate filters them all out and the guard reports a
// false "clean" (duplicate detection silently dead). M074 sets
// hnsw.iterative_scan='relaxed_order' in the same-scope branch so pgvector
// iterates the graph until the predicate yields the same-scope neighbour.
//
// FINDING (design/02 §4.7 premise inversion — reported, not silently resolved):
// the relaxed_order GUC only affects plans that USE the HNSW index. With
// idx_context_scope present (001:197) the planner picks an EXACT scope-index
// scan + top-N sort for a SELECTIVE same-scope set — exactly the design's
// "100-block scope next to 100k foreign" shape. That path has perfect recall
// and iterative_scan is moot; the filtered-ANN hazard (and the GUC that defends
// it) only materialises once the same-scope set is large enough that the
// planner prefers the HNSW index (the 1M+/large-repo regime). So the gate's
// literal 100-block fixture does NOT go red without iterative_scan — the exact
// path finds the dup regardless. Recall is nonetheless correct end-to-end in
// both regimes. This test proves both:
//   - repo-small (100 blocks): ctx_guard_check finds the 0.99 dup among 100k
//     foreign (the literal fixture / core deliverable); EXPLAIN logs the exact
//     scope-index plan (finding evidence).
//   - repo-big (large scope, natural HNSW plan): EXPLAIN asserts the HNSW plan;
//     iterative_scan='off' MISSES the dup while 'relaxed_order' (M074) FINDS
//     it — the GUC is load-bearing exactly there; ctx_guard_check finds it too.
//
// M074 status verified in-tree: 074_guard_check_type_policy.sql:126 already sets
// relaxed_order in the same-scope branch — this wave adds NO migration (K1).
//
// Corpus documented in the wave return. Defaults: 100k foreign, 100 small-home,
// 8k big-home; override via CTX_IJ_RECALL_FOREIGN_N / _SMALL_N / _BIG_N.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/guard/ -run TestIJRecallFilteredANN -count=1 -v
package guard_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	ijSmallQuery = "019f2211-0000-7000-9000-00000000f001"
	ijSmallDup   = "019f2211-0000-7000-9000-00000000f002"
	ijBigQuery   = "019f2211-0000-7000-9000-00000000f003"
	ijBigDup     = "019f2211-0000-7000-9000-00000000f004"
)

// annSelect is the same-scope filtered-ANN SELECT that M074's function body
// runs (candidate filter + scope conjunct + halfvec cosine ORDER BY). The test
// runs it directly (sort enabled — relaxed_order needs the sort node) to show
// the recall delta between iterative_scan off and on; the production path goes
// through ctx_guard_check.
const annSelect = `
	SELECT cb.id::text
	FROM context_blocks cb
	WHERE cb.id != $2::uuid
	  AND NOT cb.is_archived
	  AND cb.embedding IS NOT NULL
	  AND cb.lifecycle_state != 'chunk'
	  AND cb.type_name = ANY($3::text[])
	  AND cb.scope = $4
	ORDER BY cb.embedding::halfvec(1024) <=> $1::halfvec(1024)
	LIMIT 1`

func envN(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return def
}

func TestIJRecallFilteredANN(t *testing.T) {
	foreignN := envN("CTX_IJ_RECALL_FOREIGN_N", 100_000)
	smallN := envN("CTX_IJ_RECALL_SMALL_N", 100)
	bigN := envN("CTX_IJ_RECALL_BIG_N", 8_000)

	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Issue policy from the registry — the same-scope value is RESOLVED through
	// the Set, not hard-coded, so the gate exercises the Go policy path. Since
	// Welle I-C the issue type is a builtin seed (migration 084 §4.1) already in
	// the DB — resolve it, do NOT re-insert (uq_block_types_name_scope collision).
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	set := reg.Snapshot()
	sameScopeOnly := set.GuardSameScopeOnly("issue")
	if !sameScopeOnly {
		t.Fatalf("resolved issue GuardSameScopeOnly = false, want true")
	}
	candidateTypes := set.GuardCandidateTypes()

	qEmb := unitVec1024(0, 1.0)
	t0 := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	seedStart := time.Now()
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	// Foreign rows (scope 'other'), two populations. The recall hazard is that a
	// handful of foreign rows land NEARER the query than the same-scope dup and
	// so fill the ef_search window; it is NOT that all 100k are nearer (that
	// would push the dup beyond relaxed_order's scan budget — an unrelated
	// failure). So:
	//   - nearForeignN DISTINCT rows at cosine (0.994, 0.999) to the query —
	//     each nearer than the 0.99 dup, > ef_search count ⇒ they own the
	//     initial window. Distinct axis-1 offsets (step > halfvec ULP) keep the
	//     HNSW graph navigable (identical vectors degenerate traversal).
	//   - the remaining foreign are orthogonal (axis 700, cosine 0) — table
	//     mass at the target scale, outside the query neighbourhood.
	const nearForeignN = 300
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, embedding, lifecycle_state, type_name)
		SELECT ('019f2212-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'projects', 'near-foreign-' || i, 'x', 'other',
		       ('[1,' || (0.05 + i * 0.0002)::text || repeat(',0', 1022) || ']')::vector(1024),
		       'knowledge', 'issue'
		FROM generate_series(0, $1::int - 1) AS g(i)`, nearForeignN); err != nil {
		t.Fatalf("seed near foreign: %v", err)
	}
	// The far mass MUST be distinct (not one repeated vector): 100k IDENTICAL
	// vectors form a degenerate HNSW hub that traps relaxed_order's graph walk
	// (observed: dup missed at 100k, found at 3k). Vary the unit spike's axis
	// (50 + i%950) → 950 distinct orthogonal-to-query directions, all cosine 0
	// to both the query (axis 0) and the dup (axes 0,1).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, embedding, lifecycle_state, type_name)
		SELECT ('019f2216-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'projects', 'far-foreign-' || i, 'x', 'other',
		       ('[' || repeat('0,', 50 + (i % 950)) || '1' || repeat(',0', 1023 - (50 + (i % 950))) || ']')::vector(1024),
		       'knowledge', 'issue'
		FROM generate_series(0, $1::int - $2::int - 1) AS g(i)`, foreignN, nearForeignN); err != nil {
		t.Fatalf("seed far foreign: %v", err)
	}
	// Low-similarity filler (axis 500) for the two home scopes.
	fillerVec := `('[' || repeat('0,', 500) || '1' || repeat(',0', 523) || ']')::vector(1024)`
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, embedding, lifecycle_state, type_name)
		SELECT ('019f2214-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'projects', 'small-filler-' || i, 'x', 'repo-small',
		       (SELECT `+fillerVec+`), 'knowledge', 'issue'
		FROM generate_series(0, $1::int - 1) AS g(i)`, smallN-2); err != nil {
		t.Fatalf("seed small filler: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, embedding, lifecycle_state, type_name)
		SELECT ('019f2215-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'projects', 'big-filler-' || i, 'x', 'repo-big',
		       (SELECT `+fillerVec+`), 'knowledge', 'issue'
		FROM generate_series(0, $1::int - 1) AS g(i)`, bigN-2); err != nil {
		t.Fatalf("seed big filler: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER USER`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}
	// Query + 0.99 duplicate per home scope.
	dupEmb := blendedVec1024(0, 1, 0.99, 0.14107)
	t7Insert(t, pool, ijSmallQuery, "small-query", "issue", "projects", "repo-small", qEmb, t0)
	t7Insert(t, pool, ijSmallDup, "small-dup", "issue", "projects", "repo-small", dupEmb, t0.Add(time.Hour))
	t7Insert(t, pool, ijBigQuery, "big-query", "issue", "projects", "repo-big", qEmb, t0.Add(2*time.Hour))
	t7Insert(t, pool, ijBigDup, "big-dup", "issue", "projects", "repo-big", dupEmb, t0.Add(3*time.Hour))
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("seeded %d foreign + %d repo-small + %d repo-big rows in %s",
		foreignN, smallN, bigN, time.Since(seedStart).Round(time.Millisecond))

	qVec := pgvec.NewVector(qEmb)

	// annMatch runs annSelect (natural planner, sort enabled) after setting
	// hnsw.iterative_scan to the given mode, in a tx-scoped SET LOCAL. Returns
	// the matched id, or "" when the filtered window yields no same-scope row.
	annMatch := func(mode, scope, queryID string) string {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin (%s/%s): %v", mode, scope, err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = '"+mode+"'"); err != nil {
			t.Fatalf("set iterative_scan=%s: %v", mode, err)
		}
		var matched string
		err = tx.QueryRow(ctx, annSelect, qVec, queryID, candidateTypes, scope).Scan(&matched)
		if errors.Is(err, pgx.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("annSelect (%s/%s): %v", mode, scope, err)
		}
		return matched
	}

	explainPlan := func(t *testing.T, scope, queryID string, setup ...string) string {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		for _, s := range setup {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatalf("setup %q: %v", s, err)
			}
		}
		rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+annSelect,
			qVec, queryID, candidateTypes, scope)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return b.String()
	}

	ctxGuardCheck := func(t *testing.T, queryID, wantDup string) {
		t.Helper()
		var (
			decision string
			matched  *string
			sim      float64
		)
		if err := pool.QueryRow(ctx,
			`SELECT decision, matched_id::text, top_similarity
			 FROM ctx_guard_check($1::uuid, $2::real, $3::real, $4::text[], $5::boolean)`,
			queryID, 0.97, 0.90, candidateTypes, sameScopeOnly,
		).Scan(&decision, &matched, &sim); err != nil {
			t.Fatalf("ctx_guard_check: %v", err)
		}
		if matched == nil || *matched != wantDup {
			t.Errorf("ctx_guard_check matched=%v, want %s (same-scope recall under crowding)", matched, wantDup)
		}
		if decision != "near_duplicate" {
			t.Errorf("ctx_guard_check decision=%q sim=%.4f, want near_duplicate", decision, sim)
		}
	}

	// CORE recall gate (literal 100-block fixture): ctx_guard_check with the
	// RESOLVED p_same_scope_only=TRUE finds the 0.99 dup among 100k foreign.
	t.Run("NaturalPlannerRecall_SmallScope", func(t *testing.T) {
		ctxGuardCheck(t, ijSmallQuery, ijSmallDup)
		plan := explainPlan(t, "repo-small", ijSmallQuery, "SET LOCAL hnsw.iterative_scan = 'relaxed_order'")
		t.Logf("repo-small natural plan (uses_hnsw=%v uses_scope_index=%v):\n%s",
			strings.Contains(plan, "idx_embedding_hnsw"), strings.Contains(plan, "idx_context_scope"), plan)
	})

	// HNSW-regime gate: at the large repo-big scope the natural planner prefers
	// the HNSW index — verified by EXPLAIN. This is the filtered-ANN regime the
	// design §4.7 targets and where iterative_scan is load-bearing.
	t.Run("BigScopeUsesHNSWPlan", func(t *testing.T) {
		plan := explainPlan(t, "repo-big", ijBigQuery, "SET LOCAL hnsw.iterative_scan = 'off'")
		t.Logf("repo-big natural plan:\n%s", plan)
		if !strings.Contains(plan, "idx_embedding_hnsw") {
			t.Fatalf("repo-big natural plan is NOT the HNSW plan (bigN=%d too small to flip the planner; raise CTX_IJ_RECALL_BIG_N):\n%s", bigN, plan)
		}
		if strings.Contains(plan, "Seq Scan on context_blocks") {
			t.Errorf("repo-big plan seq-scans context_blocks:\n%s", plan)
		}
	})

	// RED (HNSW plan, iterative_scan=off): the 100k foreign crowd the ef_search
	// window, the scope predicate filters them all out, the 0.99 dup is MISSED —
	// the false-clean §4.7 warns about.
	t.Run("RedWithoutIterativeScan_Misses", func(t *testing.T) {
		got := annMatch("off", "repo-big", ijBigQuery)
		t.Logf("repo-big HNSW plan, iterative_scan=off matched=%q (want NOT %s = miss)", got, ijBigDup)
		if got == ijBigDup {
			t.Errorf("iterative_scan=off FOUND the dup on the HNSW plan — crowding too weak (foreignN=%d bigN=%d)", foreignN, bigN)
		}
	})

	// GREEN (HNSW plan, relaxed_order = M074's GUC): the same query finds the dup.
	t.Run("GreenWithIterativeScan_Finds", func(t *testing.T) {
		got := annMatch("relaxed_order", "repo-big", ijBigQuery)
		if got != ijBigDup {
			t.Errorf("iterative_scan=relaxed_order matched=%q, want %s (recall under crowding)", got, ijBigDup)
		}
	})

	// GREEN via the production path in the HNSW regime: ctx_guard_check finds it.
	t.Run("NaturalPlannerRecall_BigScope_ViaCtxGuardCheck", func(t *testing.T) {
		ctxGuardCheck(t, ijBigQuery, ijBigDup)
	})
}
