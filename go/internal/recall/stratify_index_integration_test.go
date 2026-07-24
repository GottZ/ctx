//go:build integration

// Integration coverage for Achse 01 W01-1b (K3 conflict resolution): the
// partial covering index idx_blocks_stratify_covering that backs the
// recall_check stratification/loo access pattern (Achse 01) and — read-only,
// no index of its own per K3 — the future Strategy-Selector cardinality
// estimate (Achse 02). Migration 111.
//
// Three probes: (1) the index exists with the exact predicate/columns after
// the fresh-DB migration chain; (2) the stratification per-scope count
// (scope + type filter) is Index Only Scan-capable; (3) the DISTINCT scope
// probe also plans through the same index.
//
// Run with:
//
//	go test -tags=integration ./internal/recall/ -count=1 -v
package recall_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const stratifyIdxName = "idx_blocks_stratify_covering"

func TestStratifyCoveringIndexExists(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var indexdef string
	err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = $1`, stratifyIdxName,
	).Scan(&indexdef)
	if err != nil {
		t.Fatalf("index %s does not exist after migrations: %v", stratifyIdxName, err)
	}
	t.Logf("indexdef: %s", indexdef)

	if !strings.Contains(indexdef, "scope") || !strings.Contains(indexdef, "type_name") {
		t.Errorf("indexdef %q does not cover both scope and type_name", indexdef)
	}
	if !strings.Contains(indexdef, "is_archived") || !strings.Contains(indexdef, "embedding IS NOT NULL") {
		t.Errorf("indexdef %q does not carry the exact partial predicate (NOT is_archived AND embedding IS NOT NULL)", indexdef)
	}
}

func TestStratifyCoveringIndexExplainCount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	scopes := []string{"private", "shared", "w01-1b"}
	vec := func(fill float32) []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = fill
		}
		return v
	}

	for i := 0; i < 50; i++ {
		scope := scopes[i%len(scopes)]
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("w01-1b-seed-%d", i), "body",
			nil, nil, scope, false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed upsert %d: %v", i, err)
		}
		// Embed ~two thirds of the rows so the WHERE embedding IS NOT NULL
		// predicate actually discriminates.
		if i%3 != 0 {
			if err := store.StoreEmbedding(ctx, pool, b.ID, "seed-model", vec(0.1)); err != nil {
				t.Fatalf("seed embed %d: %v", i, err)
			}
		}
	}

	if _, err := pool.Exec(ctx, "VACUUM ANALYZE context_blocks"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// K3 mini-table note (same rationale as W04-1's pending-index gate): at
	// ~50 seeded rows the planner may legitimately prefer a Seq Scan on cost
	// grounds. enable_seqscan=off (session-local) is used to prove the
	// index is present, predicate-matched, and Index-Only-capable — not
	// that the planner freely picks it at trivial scale.
	if _, err := conn.Exec(ctx, "SET enable_seqscan=off"); err != nil {
		t.Fatalf("set enable_seqscan=off: %v", err)
	}

	rows, err := conn.Query(ctx,
		`EXPLAIN (FORMAT TEXT) SELECT count(*) FROM context_blocks
		 WHERE NOT is_archived AND embedding IS NOT NULL
		   AND scope = 'private' AND type_name = ANY(ARRAY['learnings','knowledge'])`)
	if err != nil {
		t.Fatalf("explain count: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	t.Logf("stratification count plan (enable_seqscan=off, K3 mini-table note):\n%s", plan.String())
	if !strings.Contains(plan.String(), "Index Only Scan using "+stratifyIdxName) {
		t.Errorf("plan does not use Index Only Scan using %s — index missing or predicate mismatch:\n%s", stratifyIdxName, plan.String())
	}
}

func TestStratifyCoveringIndexExplainDistinctScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	vec := func(fill float32) []float32 {
		v := make([]float32, 1024)
		for i := range v {
			v[i] = fill
		}
		return v
	}
	scopes := []string{"private", "shared", "w01-1b"}
	for i := 0; i < 30; i++ {
		scope := scopes[i%len(scopes)]
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("w01-1b-distinct-seed-%d", i), "body",
			nil, nil, scope, false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed upsert %d: %v", i, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "seed-model", vec(0.2)); err != nil {
			t.Fatalf("seed embed %d: %v", i, err)
		}
	}

	if _, err := pool.Exec(ctx, "VACUUM ANALYZE context_blocks"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan=off"); err != nil {
		t.Fatalf("set enable_seqscan=off: %v", err)
	}

	rows, err := conn.Query(ctx,
		`EXPLAIN (FORMAT TEXT) SELECT DISTINCT scope FROM context_blocks
		 WHERE NOT is_archived AND embedding IS NOT NULL`)
	if err != nil {
		t.Fatalf("explain distinct scope: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	t.Logf("distinct-scope plan (enable_seqscan=off):\n%s", plan.String())
	if !strings.Contains(plan.String(), "Index Only Scan using "+stratifyIdxName) {
		t.Errorf("plan does not use Index Only Scan using %s — index missing or predicate mismatch:\n%s", stratifyIdxName, plan.String())
	}
}
