//go:build integration

// Integration coverage for W01-2 gate (c): the degradation fixture. 5k random
// 1024d vectors, the HNSW index rebuilt deliberately BAD (m=4,
// ef_construction=16) and probed with ef_search=10 — the harness MUST report
// recall < 1.0. A harness that reports 1.0 against this fixture is broken
// (it would be measuring exact-vs-exact, the §5.1 self-deception class).
// Doubles as the end-to-end RunOnce path: stratification → loo sampling →
// probes → aggregation → persisted rows with environment stamps.
//
// Run with:
//
//	go test -tags=integration ./internal/recall/ -run TestDegradation -count=1 -v
package recall_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDegradationFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Seed 5000 random vectors on ONE session so setseed() makes the corpus
	// reproducible. The CROSS JOIN + GROUP BY shape is deliberate: random()
	// is evaluated once per (i,j) pair — an uncorrelated scalar subquery
	// would run ONCE as an initplan and every row would carry the same vector.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT setseed(0.42)"); err != nil {
		t.Fatalf("setseed: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, type_name, embedding)
		SELECT 'bench', 'deg-'||q.i, 'degradation fixture body', 'deg', 'knowledge', q.v
		FROM (
			SELECT i, ('['||string_agg((random()*2-1)::text, ',' ORDER BY j)||']')::vector(1024) AS v
			FROM generate_series(1, 5000) i
			CROSS JOIN generate_series(1, 1024) j
			GROUP BY i
		) q`); err != nil {
		t.Fatalf("seed 5k vectors: %v", err)
	}
	conn.Release()

	// Rebuild the production index deliberately degraded.
	if _, err := pool.Exec(ctx, "DROP INDEX idx_embedding_hnsw"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE INDEX idx_embedding_hnsw
		ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops)
		WITH (m = 4, ef_construction = 16)`); err != nil {
		t.Fatalf("rebuild degraded index: %v", err)
	}
	if _, err := pool.Exec(ctx, "VACUUM ANALYZE context_blocks"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}

	cfg := recall.Config{
		KList:             "10",
		QueriesPerStratum: 20,
		StrataBounds:      "4096,65536", // 5000 rows in scope "deg" => medium
		EfSearch:          10,
		ExactBudgetMS:     0, // unlimited — the budget path is not under test here
		LegTimeoutMS:      60000,
		Epsilon:           0,
	}
	res, err := recall.RunOnce(ctx, pool, cfg, blocktype.NewRegistry())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.RunGroup == "" || len(res.Rows) == 0 {
		t.Fatalf("empty run result: %+v", res)
	}

	var checked bool
	for _, row := range res.Rows {
		if row.Stratum != "medium" {
			continue
		}
		checked = true
		if !row.Valid {
			t.Fatalf("medium stratum invalid: %v", row.Meta["invalid_reason"])
		}
		if row.Scope == nil || *row.Scope != "deg" {
			t.Errorf("medium scope = %v, want deg", row.Scope)
		}
		if row.CorpusEmbedded != 5000 {
			t.Errorf("corpus_embedded = %d, want 5000", row.CorpusEmbedded)
		}
		if row.RecallAvg == nil {
			t.Fatal("recall_avg is NULL on a valid row")
		}
		// THE gate: a bad index at ef_search=10 on random high-dim vectors
		// cannot deliver perfect recall over 20 queries. 1.0 here means the
		// harness compared exact against exact.
		if *row.RecallAvg >= 1.0 {
			t.Errorf("degraded index reports recall_avg=%v — the harness is broken (measuring exact-vs-exact?)", *row.RecallAvg)
		}
		if *row.RecallAvg <= 0 {
			t.Errorf("recall_avg=%v — nothing was found at all, fixture or probe broken", *row.RecallAvg)
		}
		t.Logf("degraded index (m=4, ef_c=16, ef_search=10, 5k random vectors): recall_avg=%.4f recall_min=%.4f n=%d",
			*row.RecallAvg, *row.RecallMin, row.NQueries)

		// Environment stamps present (§4.2.6).
		for _, key := range []string{"pgvector_version", "pg_version", "index_reloptions", "strata_bounds", "epsilon", "n_eff_min"} {
			if _, ok := row.Meta[key]; !ok {
				t.Errorf("meta[%s] missing on a valid row", key)
			}
		}
		if reloptions, _ := row.Meta["index_reloptions"].(string); reloptions != "" {
			t.Logf("index_reloptions stamp: %s", reloptions)
		}
	}
	if !checked {
		t.Fatal("no medium-stratum row in the run result")
	}

	// The rows really landed in context_recall_runs (one per stratum×k,
	// shared run_group).
	var persisted int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_recall_runs WHERE run_group = $1`, res.RunGroup,
	).Scan(&persisted); err != nil {
		t.Fatalf("count persisted: %v", err)
	}
	if persisted != len(res.Rows) {
		t.Errorf("persisted %d rows, run result carries %d", persisted, len(res.Rows))
	}
}
