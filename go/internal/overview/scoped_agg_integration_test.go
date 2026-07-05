//go:build integration

// B-W3 DB gates (internal package: persist and the agg SQL are unexported).
// Proves the B1-C1 fix: scoped teardown + scoped aggregation are an atomic
// pair — a scoped run neither touches a foreign partition nor collides with
// it, and the EXACT historical breakage (scoped DELETE + unscoped
// re-aggregation) reproduces SQLSTATE 23505 as the red probe.
package overview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// insBlockB3/insLinkB3: internal-package fixture helpers (the exported-test
// helpers live in package overview_test and are not importable from here).
func insBlockB3(t *testing.T, pool *pgxpool.Pool, id, scope, title string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO context_blocks (id, scope, category, title, content)
		VALUES ($1::uuid, $2, 'learnings', $3, 'b-w3 fixture')`,
		id, scope, title); err != nil {
		t.Fatalf("insBlockB3 %s: %v", title, err)
	}
}

func insLinkB3(t *testing.T, pool *pgxpool.Pool, src, dst string, conf float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		VALUES ($1::uuid, $2::uuid, 'topical', $3, $3, 'private')`,
		src, dst, conf); err != nil {
		t.Fatalf("insLinkB3: %v", err)
	}
}

// snapshotPartition renders every member/node/edge row of one scope as sorted
// text lines — byte-comparable across runs.
func snapshotPartition(t *testing.T, pool *pgxpool.Pool, scope string) string {
	t.Helper()
	ctx := context.Background()
	var sb strings.Builder
	for _, q := range []string{
		`SELECT 'm|' || block_id::text || '|' || cluster_id::text || '|' || scope
		   FROM graph_cluster_member WHERE scope = $1 ORDER BY block_id`,
		`SELECT 'n|' || cluster_id::text || '|' || scope || '|' || size::text || '|' || category_counts::text
		   FROM graph_cluster_node WHERE scope = $1 ORDER BY cluster_id`,
		`SELECT 'e|' || cluster_a::text || '|' || cluster_b::text || '|' || scope_s || '|' || scope_t || '|' || link_count::text
		   FROM graph_cluster_edge WHERE scope_s = $1 AND scope_t = $1 ORDER BY cluster_a, cluster_b`,
	} {
		rows, err := pool.Query(ctx, q, scope)
		if err != nil {
			t.Fatalf("snapshot %s: %v", scope, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			sb.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return sb.String()
}

// scopedInput builds clustering+nodeScopes for ONE partition the way B-W6's
// scoped loading will: load everything, cut to the filter. Deterministic
// single-cluster layout is fine — the gates assert partition hygiene, not
// community quality.
func scopedInput(t *testing.T, pool *pgxpool.Pool, types, scopes []string) (clustering, map[string]string) {
	t.Helper()
	ctx := context.Background()
	nodeUUIDs, nodeScopes, err := loadNodes(ctx, pool, types)
	if err != nil {
		t.Fatalf("loadNodes: %v", err)
	}
	edges, err := loadEdges(ctx, pool)
	if err != nil {
		t.Fatalf("loadEdges: %v", err)
	}
	in := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		in[s] = struct{}{}
	}
	var cutIDs []string
	cutScopes := make(map[string]string)
	for _, id := range nodeUUIDs {
		if _, ok := in[nodeScopes[id]]; ok {
			cutIDs = append(cutIDs, id)
			cutScopes[id] = nodeScopes[id]
		}
	}
	var cutEdges []rawEdge
	for _, e := range edges {
		if _, s := cutScopes[e.src]; s {
			if _, d := cutScopes[e.dst]; d {
				cutEdges = append(cutEdges, e)
			}
		}
	}
	return computeClustering(cutIDs, cutEdges, 1.0), cutScopes
}

// TestScopedAggregation_B1C1 is the B-W3 gate suite over one two-scope corpus.
func TestScopedAggregation_B1C1(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	types := []string{"knowledge"}

	const (
		P1 = "019d0000-0000-7000-9000-0000000000a1" // private
		P2 = "019d0000-0000-7000-9000-0000000000a2" // private
		P3 = "019d0000-0000-7000-9000-0000000000a3" // private
		W1 = "019d0000-0000-7000-9000-0000000000e1" // work
		W2 = "019d0000-0000-7000-9000-0000000000e2" // work
	)
	insBlockB3(t, pool, P1, "private", "P1")
	insBlockB3(t, pool, P2, "private", "P2")
	insBlockB3(t, pool, P3, "private", "P3")
	insBlockB3(t, pool, W1, "work", "W1")
	insBlockB3(t, pool, W2, "work", "W2")
	insLinkB3(t, pool, P1, P2, 0.9)
	insLinkB3(t, pool, P2, P3, 0.8)
	insLinkB3(t, pool, W1, W2, 0.7)
	insLinkB3(t, pool, P3, W1, 0.6) // cross-partition mixed edge

	// Global run (nil filter — the unchanged pre-B path).
	if _, err := Rebuild(ctx, pool, Options{
		Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
	}); err != nil {
		t.Fatalf("global rebuild: %v", err)
	}
	workBefore := snapshotPartition(t, pool, "work")
	if workBefore == "" {
		t.Fatal("fixture: work partition empty after global run")
	}

	t.Run("scoped run leaves the foreign partition byte-identical, no PK conflict", func(t *testing.T) {
		cl, scopes := scopedInput(t, pool, types, []string{"private"})
		stats, err := persist(ctx, pool, cl, Options{
			Resolution: 1.0, VisibleTypes: types, ScopeFilter: []string{"private"},
		}, scopes)
		if err != nil {
			t.Fatalf("scoped persist: %v", err) // 23505 here = B1-C1 regression
		}
		if stats.Skipped {
			t.Fatalf("scoped persist skipped: %s", stats.SkipReason)
		}
		if got := snapshotPartition(t, pool, "work"); got != workBefore {
			t.Fatalf("work partition changed by a private-scoped run:\nbefore:\n%s\nafter:\n%s", workBefore, got)
		}
		if p := snapshotPartition(t, pool, "private"); p == "" {
			t.Fatal("private partition empty after scoped run")
		}
	})

	t.Run("red probe: scoped DELETE + UNSCOPED aggregation reproduces 23505 (B1-C1)", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // probe rolls back, tables stay intact
		if _, err := tx.Exec(ctx, `DELETE FROM graph_cluster_node WHERE scope = 'private'`); err != nil {
			t.Fatal(err)
		}
		// The historical bug: the aggregation still scans ALL members, so it
		// re-emits the surviving work rows ⇒ (cluster_id, scope) PK conflict.
		_, err = tx.Exec(ctx, nodeAggSQL, types)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("unscoped re-aggregation after scoped delete: err=%v, want SQLSTATE 23505 (the B1-C1 breakage)", err)
		}
	})

	t.Run("input-purity guard fails loud on out-of-filter input", func(t *testing.T) {
		cl, scopes := scopedInput(t, pool, types, []string{"private", "work"})
		_, err := persist(ctx, pool, cl, Options{
			Resolution: 1.0, VisibleTypes: types, ScopeFilter: []string{"private"},
		}, scopes)
		if err == nil || !strings.Contains(err.Error(), "outside ScopeFilter") {
			t.Fatalf("out-of-filter input: err=%v, want loud input-purity error", err)
		}
	})

	t.Run("EXPLAIN documentation probe (B2-MAJOR-2)", func(t *testing.T) {
		for name, q := range map[string]string{
			"member teardown": `DELETE FROM graph_cluster_member WHERE scope = ANY($1)`,
			"node agg scoped": nodeAggScopedSQL,
			"edge agg scoped": edgeAggScopedSQL,
		} {
			var args []any
			if strings.Contains(q, "$2") {
				args = []any{types, []string{"private"}}
			} else {
				args = []any{[]string{"private"}}
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(ctx, "EXPLAIN "+q, args...)
			if err != nil {
				t.Fatalf("EXPLAIN %s: %v", name, err)
			}
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan.WriteString("  " + line + "\n")
			}
			rows.Close()
			_ = tx.Rollback(ctx)
			t.Logf("EXPLAIN %s:\n%s", name, plan.String())
		}
	})
}
