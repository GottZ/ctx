//go:build integration

// T6 gate — Rebuild-Dauer-Messpunkt (design/01 §6.8/§7-T6): the overview
// rebuild stays ONE global run; overview.include only trims the node set. This
// test seeds a synthetic corpus (community-structured chain segments, the
// Louvain-friendly shape) inside the testcontainer and logs the end-to-end
// Rebuild duration. It never asserts a time bound — it is the documented
// threshold measurement (§6.8: "der globale Rebuild kippt, wenn der
// Gesamt-Korpus die 6h-Kadenz sprengt"). The 1M live measurement is
// integrator scope (G39 corpus, cluster_bench_test.go).
//
// Size via CTX_T6_REBUILD_N (default 100000 nodes, ~1.95 edges/node).
package overview_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestT6RebuildDurationSynthetic(t *testing.T) {
	n := 100_000
	if v := os.Getenv("CTX_T6_REBUILD_N"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			n = p
		}
	}

	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Deterministic UUIDs from the series index; segment-of-20 chain edges
	// (i→i+1, i→i+2 within a segment) give clear communities so Louvain
	// converges the way a real topical corpus does.
	seedStart := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f2207-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'syn-' || i, 'synthetic t6 corpus', 'private'
		FROM generate_series(0, $1::int - 1) AS g(i)`, n); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		SELECT ('019f2207-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       ('019f2207-0000-7000-9000-' || lpad(to_hex(i + o), 12, '0'))::uuid,
		       'topical', 0.8, 0.8, 'private'
		FROM generate_series(0, $1::int - 1) AS g(i), (VALUES (1), (2)) AS off(o)
		WHERE (i % 20) + o < 20`, n); err != nil {
		t.Fatalf("seed links: %v", err)
	}
	var edgeN int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_dream_links`).Scan(&edgeN); err != nil {
		t.Fatalf("edge count: %v", err)
	}
	t.Logf("seeded %d nodes / %d edges in %s", n, edgeN, time.Since(seedStart).Round(time.Millisecond))

	start := time.Now()
	stats, err := overview.Rebuild(ctx, pool, 1.0,
		[]string{"audit-trail", "knowledge", "reference"},
		[]string{"audit-trail", "knowledge", "reference", "system-meta"})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	elapsed := time.Since(start)
	if stats.Skipped {
		t.Fatal("rebuild skipped (advisory lock) — unexpected in an isolated container")
	}
	if stats.NodeCount != n {
		t.Errorf("NodeCount = %d, want %d (type sieve must not drop default-typed nodes)", stats.NodeCount, n)
	}
	// THE measurement of this gate: full Rebuild (loadNodes + loadEdges +
	// Louvain + persist) wall time on the documented synthetic size.
	t.Logf("T6 rebuild duration: %s (nodes=%d, edges=%d, clusters=%d, modularity=%.4f)",
		elapsed.Round(time.Millisecond), stats.NodeCount, edgeN, stats.ClusterCount, stats.Modularity)
}
