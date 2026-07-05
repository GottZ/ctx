//go:build integration

// B-W5 DB gates (internal package: persist and the meta SQL are unexported).
// Proves the 088 line: per-scope meta rows written by the run that owns them
// (global = all, scoped = filter scopes only — leak B1-m1), real scopes only
// (no sentinel, B3-M1), the legacy singleton row migrated onto real scopes
// with computed_at preserved (B3-M2), and the graph_cluster_member(scope)
// index carrying the scoped teardown (B-W3 finding).
package overview

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// metaComputedAt reads max(computed_at) over the given scopes — byte-for-byte
// the store/overview.go read semantics (B-W5); nil = no partition built.
func metaComputedAt(t *testing.T, pool *pgxpool.Pool, scopes []string) *time.Time {
	t.Helper()
	var p *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT max(computed_at) FROM graph_overview_meta WHERE scope = ANY($1)`,
		scopes).Scan(&p); err != nil {
		t.Fatalf("meta read: %v", err)
	}
	return p
}

func TestMetaScope_B5(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	types := []string{"knowledge"}

	const (
		P1 = "019d0000-0000-7000-9000-0000000000f1" // private
		P2 = "019d0000-0000-7000-9000-0000000000f2" // private
		W1 = "019d0000-0000-7000-9000-0000000000f3" // work
		W2 = "019d0000-0000-7000-9000-0000000000f4" // work
	)
	insBlockB3(t, pool, P1, "private", "M-P1")
	insBlockB3(t, pool, P2, "private", "M-P2")
	insBlockB3(t, pool, W1, "work", "M-W1")
	insBlockB3(t, pool, W2, "work", "M-W2")
	insLinkB3(t, pool, P1, P2, 0.9)
	insLinkB3(t, pool, W1, W2, 0.8)

	if _, err := Rebuild(ctx, pool, Options{
		Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
	}); err != nil {
		t.Fatalf("global rebuild: %v", err)
	}

	t.Run("global run writes real per-scope rows, no sentinel (B3-M1)", func(t *testing.T) {
		rows, err := pool.Query(ctx, `SELECT scope FROM graph_overview_meta ORDER BY scope`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		if strings.Join(got, ",") != "private,work" {
			t.Fatalf("meta scopes = %v, want [private work] (real scopes, no sentinel)", got)
		}
		// The B3-M1 red probe inverted: with a sentinel scope this per-scope
		// read would be nil despite fresh data.
		if metaComputedAt(t, pool, []string{"private"}) == nil {
			t.Fatal("per-scope read nil despite a fresh global run — sentinel-scope symptom")
		}
	})

	var workBefore time.Time
	if err := pool.QueryRow(ctx, `SELECT computed_at FROM graph_overview_meta WHERE scope = 'work'`).Scan(&workBefore); err != nil {
		t.Fatalf("work computed_at before: %v", err)
	}

	t.Run("scoped run refreshes only its scopes (leak B1-m1)", func(t *testing.T) {
		cl, scopes := scopedInput(t, pool, types, []string{"private"})
		if _, err := persist(ctx, pool, cl, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"private"},
		}, scopes); err != nil {
			t.Fatalf("scoped persist: %v", err)
		}
		var privateAt, workAfter time.Time
		if err := pool.QueryRow(ctx, `SELECT computed_at FROM graph_overview_meta WHERE scope = 'private'`).Scan(&privateAt); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT computed_at FROM graph_overview_meta WHERE scope = 'work'`).Scan(&workAfter); err != nil {
			t.Fatal(err)
		}
		if !privateAt.After(workBefore) {
			t.Fatalf("private computed_at %v not refreshed past %v", privateAt, workBefore)
		}
		if !workAfter.Equal(workBefore) {
			t.Fatalf("scoped private run moved work computed_at: %v → %v (foreign-partition write)", workBefore, workAfter)
		}
	})

	t.Run("foreign-scope read stays nil where the legacy LIMIT-1 read leaks", func(t *testing.T) {
		// shared was never built: the scoped read answers nil…
		if p := metaComputedAt(t, pool, []string{"shared"}); p != nil {
			t.Fatalf("shared read = %v, want nil (never built)", p)
		}
		// …while the pre-B-W5 unscoped LIMIT-1 read (documented red probe)
		// would have served ANOTHER partition's computed_at as shared's.
		var leaked time.Time
		if err := pool.QueryRow(ctx, `SELECT computed_at FROM graph_overview_meta LIMIT 1`).Scan(&leaked); err != nil || leaked.IsZero() {
			t.Fatalf("leak-probe precondition broken: legacy read err=%v at=%v", err, leaked)
		}
	})

	t.Run("empty partition still records its rebuild", func(t *testing.T) {
		cl, scopes := scopedInput(t, pool, types, []string{"shared"}) // no shared blocks
		if _, err := persist(ctx, pool, cl, Options{
			Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
			ScopeFilter: []string{"shared"},
		}, scopes); err != nil {
			t.Fatalf("empty scoped persist: %v", err)
		}
		var clusterN int
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT cluster_n, computed_at FROM graph_overview_meta WHERE scope = 'shared'`).Scan(&clusterN, &at); err != nil {
			t.Fatalf("empty partition meta row: %v", err)
		}
		if clusterN != 0 || at.IsZero() {
			t.Fatalf("empty partition row cluster_n=%d computed_at=%v, want 0 + fresh", clusterN, at)
		}
	})

	t.Run("088 replay migrates the singleton row onto real scopes (B3-M2)", func(t *testing.T) {
		// Rebuild the pre-088 shape: singleton PK, no scope column, one
		// legacy row with a FIXED computed_at that must survive.
		legacyAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		for _, sql := range []string{
			`ALTER TABLE graph_overview_meta DROP CONSTRAINT graph_overview_meta_pkey`,
			`ALTER TABLE graph_overview_meta DROP COLUMN scope`,
			`DELETE FROM graph_overview_meta`,
			`ALTER TABLE graph_overview_meta ADD COLUMN singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton)`,
		} {
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatalf("pre-088 shape (%s): %v", sql, err)
			}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph_overview_meta (singleton, computed_at, modularity, cluster_n, node_n, edge_n, resolution)
			VALUES (true, $1, 0.5, 7, 42, 9, 1.0)`, legacyAt); err != nil {
			t.Fatalf("legacy singleton row: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM _migrations WHERE version = 88`); err != nil {
			t.Fatal(err)
		}

		// Re-apply the real 088 file (embedded FS = what the runner executes).
		sql, err := migrations.FS.ReadFile("088_meta_scope_pk.sql")
		if err != nil {
			t.Fatalf("read embedded 088: %v", err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("re-apply 088: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		// One row per REAL node scope (private + work — the shared partition
		// has no node rows), computed_at + stats preserved from the legacy row.
		rows, err := pool.Query(ctx, `SELECT scope, computed_at, node_n FROM graph_overview_meta ORDER BY scope`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var scopes []string
		for rows.Next() {
			var s string
			var at time.Time
			var nodeN int
			if err := rows.Scan(&s, &at, &nodeN); err != nil {
				t.Fatal(err)
			}
			if !at.Equal(legacyAt) || nodeN != 42 {
				t.Fatalf("scope %s: computed_at=%v node_n=%d, want legacy %v/42 preserved (B3-M2)", s, at, nodeN, legacyAt)
			}
			scopes = append(scopes, s)
		}
		if strings.Join(scopes, ",") != "private,work" {
			t.Fatalf("post-088 scopes = %v, want [private work] (DISTINCT node scopes)", scopes)
		}
		// Boot-rebuild verdict preserved: rows exist ⇒ overviewNeverBuilt
		// (count(*) == 0) stays false — no spurious boot rebuild.
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_overview_meta`).Scan(&n); err != nil || n == 0 {
			t.Fatalf("post-088 meta count = %d (err=%v), want > 0", n, err)
		}
	})

	t.Run("member(scope) index exists and carries the scoped teardown", func(t *testing.T) {
		var one int
		if err := pool.QueryRow(ctx,
			`SELECT 1 FROM pg_indexes WHERE tablename = 'graph_cluster_member' AND indexname = 'idx_gcm_scope'`).Scan(&one); err != nil {
			t.Fatalf("idx_gcm_scope missing: %v", err)
		}
		// Enough rows that the planner has a real choice, then ANALYZE.
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_blocks (id, scope, category, title, content)
			SELECT uuidv7(), 'bulk', 'learnings', 'bulk-' || i, 'bulk fixture'
			  FROM generate_series(1, 2000) i`); err != nil {
			t.Fatalf("bulk blocks: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
			SELECT id, '019d0000-0000-7000-9000-00000000eeee', 'bulk'
			  FROM context_blocks WHERE scope = 'bulk'`); err != nil {
			t.Fatalf("bulk members: %v", err)
		}
		if _, err := pool.Exec(ctx, `ANALYZE graph_cluster_member`); err != nil {
			t.Fatal(err)
		}
		rows, err := pool.Query(ctx, `EXPLAIN DELETE FROM graph_cluster_member WHERE scope = ANY('{private}')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if !strings.Contains(plan.String(), "idx_gcm_scope") {
			t.Fatalf("scoped teardown DELETE does not use idx_gcm_scope:\n%s", plan.String())
		}
	})
}
