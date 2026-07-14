//go:build integration

package dream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
)

// GD2 gate c (scope negative, design/04 §4.3/W04-5): fetchDailyStructuralLinks
// is scope-filtered like fetchDailyDecisions — rows in a FOREIGN scope must
// not count, same class or not. Red without the scope bind: the foreign rows
// below share the window and the class, so a global aggregation would return
// count 5 instead of 3.
func TestFetchDailyStructuralLinks_ScopeNegative(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	from := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f60d2-0001-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'gd2 block ' || i, 'x',
		       CASE WHEN i < 8 THEN 'gd2-own' ELSE 'gd2-foreign' END
		FROM generate_series(0, 15) AS i`); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	// 3 own-scope rows inside the window (2× references/system, 1× duplicate-of/forge-sync)
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		VALUES
		  ('019f60d2-0001-7000-9000-000000000000', '019f60d2-0001-7000-9000-000000000001', 'references',   'gd2-own', 'system',     $1),
		  ('019f60d2-0001-7000-9000-000000000000', '019f60d2-0001-7000-9000-000000000002', 'references',   'gd2-own', 'system',     $1),
		  ('019f60d2-0001-7000-9000-000000000001', '019f60d2-0001-7000-9000-000000000002', 'duplicate-of', 'gd2-own', 'forge-sync', $1)`,
		from.Add(time.Hour)); err != nil {
		t.Fatalf("seed own rows: %v", err)
	}
	// 2 foreign-scope rows, SAME class/window (the discriminator) + 3 own-scope
	// rows pinning the FULL [from, to) semantics (review hardening): one after
	// the window, one before it, one at exactly `to` (exclusive upper bound).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		VALUES
		  ('019f60d2-0001-7000-9000-000000000008', '019f60d2-0001-7000-9000-000000000009', 'references', 'gd2-foreign', 'system', $1),
		  ('019f60d2-0001-7000-9000-000000000008', '019f60d2-0001-7000-9000-00000000000a', 'references', 'gd2-foreign', 'system', $1),
		  ('019f60d2-0001-7000-9000-000000000000', '019f60d2-0001-7000-9000-000000000003', 'references', 'gd2-own',     'system', $2),
		  ('019f60d2-0001-7000-9000-000000000000', '019f60d2-0001-7000-9000-000000000004', 'references', 'gd2-own',     'system', $3),
		  ('019f60d2-0001-7000-9000-000000000000', '019f60d2-0001-7000-9000-000000000005', 'references', 'gd2-own',     'system', $4)`,
		from.Add(2*time.Hour), to.Add(time.Hour), from.Add(-time.Hour), to); err != nil {
		t.Fatalf("seed foreign/outside rows: %v", err)
	}

	stats, err := fetchDailyStructuralLinks(ctx, pool, "gd2-own", from, to)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	total := 0
	byKey := map[string]int{}
	for _, s := range stats {
		total += s.Count
		byKey[s.LinkClass+"|"+s.Origin] = s.Count
	}
	if total != 3 {
		t.Fatalf("own-scope window count = %d, want 3 (foreign scope or out-of-window rows leaked in): %+v", total, stats)
	}
	if byKey["references|system"] != 2 || byKey["duplicate-of|forge-sync"] != 1 {
		t.Fatalf("per-(class,origin) split wrong: %+v", stats)
	}
}

// GD2 gate d (EXPLAIN @100k, design/04 §7 W04-5): at representative volume the
// window aggregation must ride idx_struct_links_scope_created (index-only,
// index or bitmap scan — all legitimate) and must NOT seq-scan the table.
// Seed via SQL bulk over 320 blocks (the PK surface needs distinct pairs; 100k
// single PutStructuralLink calls would each take a FOR-SHARE lock without any
// insight), then ANALYZE (stale stats flip planners between autovacuum runs —
// the documented flake source).
func TestFetchDailyStructuralLinks_IndexPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f60d2-0002-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'gd2 plan block ' || i, 'x', 'gd2-plan'
		FROM generate_series(0, 319) AS i`); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	// 320×319 ≈ 102k directed pairs, capped at 100k — timestamps spread over
	// ~70 days so the one-day window is selective (the index's raison d'être).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin, created_at)
		SELECT ('019f60d2-0002-7000-9000-' || lpad(to_hex(a), 12, '0'))::uuid,
		       ('019f60d2-0002-7000-9000-' || lpad(to_hex(b), 12, '0'))::uuid,
		       'references', 'gd2-plan', 'system',
		       now() - make_interval(mins => a * 320 + b)
		FROM generate_series(0, 319) AS a, generate_series(0, 319) AS b
		WHERE a <> b
		LIMIT 100000`); err != nil {
		t.Fatalf("seed 100k links: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_structural_links`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var plan strings.Builder
	rows, err := pool.Query(ctx, `
		EXPLAIN SELECT link_class, origin, COUNT(*)::int FROM context_structural_links
		WHERE scope = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY 1, 2 ORDER BY COUNT(*) DESC`,
		"gd2-plan", time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}

	p := plan.String()
	if !strings.Contains(p, "idx_struct_links_scope_created") {
		t.Fatalf("plan does not use idx_struct_links_scope_created:\n%s", p)
	}
	if strings.Contains(p, "Seq Scan on context_structural_links") {
		t.Fatalf("plan seq-scans the table:\n%s", p)
	}
}
