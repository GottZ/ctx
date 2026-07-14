//go:build integration

package dream

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/testdb"
)

// GD3 gate (design/04 §7 W04-6, decision E14): fetchDailyDreamLinks is
// scope-filtered — dream links in a FOREIGN scope no longer count. Red probe:
// written BEFORE the build; the scope parameter is a compile error against
// HEAD (global signature), and semantically the foreign rows below would
// inflate the count from 2 to 4 under the old global aggregation.
func TestFetchDailyDreamLinks_ScopeFiltered(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	from := time.Date(2026, 7, 13, 3, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope)
		SELECT ('019f60d3-0001-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'gd3 block ' || i, 'x',
		       CASE WHEN i < 8 THEN 'gd3-own' ELSE 'gd3-foreign' END
		FROM generate_series(0, 15) AS i`); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	// 2 own-scope rows in-window; 2 foreign-scope rows SAME class/window
	// (the discriminator); 3 own-scope rows pinning the FULL [from, to)
	// semantics (GD2 hardening standard): after the window, before it, and
	// at exactly `to` (exclusive upper bound).
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, scope, created_at)
		VALUES
		  ('019f60d3-0001-7000-9000-000000000000', '019f60d3-0001-7000-9000-000000000001', 'topical', 0.9, 'gd3-own',     $1),
		  ('019f60d3-0001-7000-9000-000000000000', '019f60d3-0001-7000-9000-000000000002', 'topical', 0.9, 'gd3-own',     $1),
		  ('019f60d3-0001-7000-9000-000000000008', '019f60d3-0001-7000-9000-000000000009', 'topical', 0.9, 'gd3-foreign', $1),
		  ('019f60d3-0001-7000-9000-000000000008', '019f60d3-0001-7000-9000-00000000000a', 'topical', 0.9, 'gd3-foreign', $1),
		  ('019f60d3-0001-7000-9000-000000000000', '019f60d3-0001-7000-9000-000000000003', 'topical', 0.9, 'gd3-own',     $2),
		  ('019f60d3-0001-7000-9000-000000000000', '019f60d3-0001-7000-9000-000000000004', 'topical', 0.9, 'gd3-own',     $3),
		  ('019f60d3-0001-7000-9000-000000000000', '019f60d3-0001-7000-9000-000000000005', 'topical', 0.9, 'gd3-own',     $4)`,
		from.Add(time.Hour), to.Add(time.Hour), from.Add(-time.Hour), to); err != nil {
		t.Fatalf("seed dream links: %v", err)
	}

	stats, err := fetchDailyDreamLinks(ctx, pool, "gd3-own", from, to)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	total := 0
	for _, s := range stats {
		total += s.Count
	}
	if total != 2 {
		t.Fatalf("own-scope window count = %d, want 2 (global aggregation would return 4): %+v", total, stats)
	}
}
