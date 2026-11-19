//go:build integration

// Issue #225, cost pin: the request filter adds conjuncts to the three
// post-fusion statements that can add a block — the graph neighbour hydrate,
// the cluster injection and the fold's parent hydrate. They are evaluated on
// rows each statement already reads (the context_blocks row it joins for the
// visibility predicate), so the filter must not cost buffers and the statements
// must not start to grow with the corpus.
//
// The statements are the production ones: each stage runs once on a pool whose
// tracer records what it sends, and that SQL is explained with its own
// arguments — the custom plan, and the generic plan a driver-cached statement
// switches to after five executions. Measured at 10 000 and 100 000 blocks,
// with the stage inputs (one seed's 60 links, one cluster of 200, 100 folded
// children) the same at both sizes, half of every input excluded.
//
// The corpus is seeded with set-based SQL: no production path writes 100 000
// blocks with links and a cluster map in test time, and the statements under
// test read only the rows, not how they were written.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryPostStageFilterCost -count=1 -v
package handler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	pfcScope   = "pfc"
	pfcSeedID  = "019e2250-0000-7000-9000-0000000000f1"
	pfcCluster = "019e2250-0000-7000-9000-0000000000f2"
)

// pfcTrace records every statement a stage sends.
type pfcTrace struct {
	mu   sync.Mutex
	stmt []pfcStmt
}

type pfcStmt struct {
	sql  string
	args []any
}

func (r *pfcTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmt = append(r.stmt, pfcStmt{sql: d.SQL, args: append([]any(nil), d.Args...)})
	return ctx
}

func (r *pfcTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take returns the one recorded statement containing marker and resets the log.
func (r *pfcTrace) take(t *testing.T, marker string) pfcStmt {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var hit []pfcStmt
	for _, s := range r.stmt {
		if strings.Contains(s.sql, marker) {
			hit = append(hit, s)
		}
	}
	r.stmt = nil
	if len(hit) != 1 {
		t.Fatalf("%d recorded statements contain %q, want 1", len(hit), marker)
	}
	return hit[0]
}

// pfcSeed writes n filler blocks plus the fixed stage inputs: a seed with 60
// links (30 to "keep", 30 to "drop" neighbours), a cluster of 200 (100/100) and
// 100 children whose parents are half "keep", half "drop".
func pfcSeed(t *testing.T, pool *pgxpool.Pool, n int) (childIDs []string) {
	t.Helper()
	ctx := context.Background()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("filler", `INSERT INTO context_blocks (category, title, content, scope, tags)
		SELECT 'filler', 'pfc-filler-' || g, 'filler ' || g, $1, ARRAY['t'] FROM generate_series(1, $2) g`, pfcScope, n)
	exec("seed", `INSERT INTO context_blocks (id, category, title, content, scope, tags)
		VALUES ($1::uuid, 'keep', 'pfc-seed', 'seed', $2, ARRAY['t'])`, pfcSeedID, pfcScope)
	exec("neighbours", `INSERT INTO context_blocks (category, title, content, scope, tags)
		SELECT CASE WHEN g % 2 = 0 THEN 'keep' ELSE 'drop' END, 'pfc-nb-' || g, 'nb ' || g, $1, ARRAY['t']
		  FROM generate_series(1, 60) g`, pfcScope)
	exec("links", `INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		SELECT $1::uuid, id, 'factual', 0.9, 0.9, $2 FROM context_blocks WHERE title LIKE 'pfc-nb-%'`, pfcSeedID, pfcScope)
	exec("members", `INSERT INTO context_blocks (category, title, content, scope, tags, quality_score)
		SELECT CASE WHEN g % 2 = 0 THEN 'keep' ELSE 'drop' END, 'pfc-member-' || g, 'member ' || g, $1, ARRAY['t'], g
		  FROM generate_series(1, 200) g`, pfcScope)
	exec("membership", `INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
		SELECT id, $1::uuid, $2 FROM context_blocks WHERE title LIKE 'pfc-member-%'`, pfcCluster, pfcScope)
	exec("node", `INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts)
		VALUES ($1::uuid, $2, 200, $3::uuid, 'repr', 1, '{"keep":100}'::jsonb)`, pfcCluster, pfcScope, pfcSeedID)
	exec("parents", `INSERT INTO context_blocks (category, title, content, scope, tags)
		SELECT CASE WHEN g % 2 = 0 THEN 'keep' ELSE 'drop' END, 'pfc-parent-' || g, 'parent ' || g, $1, ARRAY['t']
		  FROM generate_series(1, 100) g`, pfcScope)
	exec("children", `INSERT INTO context_blocks (category, title, content, scope, tags, parent_id)
		SELECT 'keep', 'pfc-child-' || g, 'child ' || g, $1, ARRAY['t'],
		       (SELECT id FROM context_blocks WHERE title = 'pfc-parent-' || g)
		  FROM generate_series(1, 100) g`, pfcScope)
	for _, tbl := range []string{"context_blocks", "context_dream_links", "graph_cluster_member"} {
		exec("analyze", "ANALYZE "+tbl)
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE 'pfc-child-%' ORDER BY title`)
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan child: %v", err)
		}
		childIDs = append(childIDs, id)
	}
	return childIDs
}

// pfcGenericArgs renders a *string as the string the generic plan's literal
// needs (testdb.ExplainGeneric has no literal for a pointer).
func pfcGenericArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if p, ok := a.(*string); ok && p != nil {
			out[i] = *p
			continue
		}
		out[i] = a
	}
	return out
}

func TestQueryPostStageFilterCost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	keep := "keep"
	filter := rrf.RequestFilter{Category: &keep, Tags: []string{"t"}, CategoriesExclude: []string{"drop"}, TypesExclude: []string{"insight"}}
	types := []string{"knowledge"}
	scopes := []string{pfcScope}

	graphCfg := rrf.GraphConfig{
		Enabled: true, Directed: true, HopDepth: 1, SeedCount: 5, SeedScoreFloor: 0.5,
		PerSeedCap: 60, MaxInjected: 60, MinConfidence: 0.75, MinConfidenceRecurrent: 0.8,
		BoostWeight: 0.2, WeightFactual: 0.9, NewPlacementFrac: 0.6,
	}
	clusterCfg := rrf.ClusterConfig{
		Enabled: true, SeedCount: 10, TopClusters: 2, MinShare: 0.1, BoostWeight: 0.12,
		MaxStaleness: 24 * time.Hour, InjectMax: 50,
	}
	fresh := rrf3Fresh{at: time.Now().Add(-time.Minute)}

	type stage struct{ name, marker string }
	stages := []stage{
		{"graph neighbour hydrate", "FROM context_dream_links"},
		{"cluster injection", "JOIN context_blocks cb ON cb.id = m.block_id"},
		{"fold parent hydrate", "WITH children AS"},
	}
	sizes := []int{10_000, 100_000}
	// cost[size][stage][filtered][generic]
	var cost [2][3][2][2]testdb.PlanCost
	for si, n := range sizes {
		pool := testdb.SetupTestDB(t)
		childIDs := pfcSeed(t, pool, n)
		tr := &pfcTrace{}
		pc := pool.Config()
		pc.ConnConfig.Tracer = tr
		tp, err := pgxpool.NewWithConfig(context.Background(), pc)
		if err != nil {
			t.Fatalf("tracing pool: %v", err)
		}
		t.Cleanup(tp.Close)
		ctx := context.Background()
		seed := []rrf.SearchResult{{ID: pfcSeedID, RRFScore: 1.0, Scope: pfcScope}}
		h := &QueryHandler{pool: tp}

		var members []rrf.SearchResult
		rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE 'pfc-member-%' ORDER BY title LIMIT 3`)
		if err != nil {
			t.Fatalf("members: %v", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan member: %v", err)
			}
			members = append(members, rrf.SearchResult{ID: id, RRFScore: 1.0 - float64(len(members))*0.01, Scope: pfcScope})
		}
		rows.Close()

		for fi, f := range []rrf.RequestFilter{{}, filter} {
			got := map[string]int{}
			out, err := rrf.GraphExpandCached(ctx, tp, seed, scopes, nil, types, f, graphCfg, rrf.ExpandCache{})
			if err != nil {
				t.Fatalf("graph: %v", err)
			}
			got[stages[0].name] = len(out) - len(seed)
			st := tr.take(t, stages[0].marker)
			cost[si][0][fi][0] = testdb.ExplainCustom(t, pool, st.sql, st.args...)
			cost[si][0][fi][1] = testdb.ExplainGeneric(t, pool, st.sql, pfcGenericArgs(st.args)...)

			out, _, err = rrf.ClusterBoost(ctx, tp, members, nil, scopes, nil, types, f, clusterCfg, fresh)
			if err != nil {
				t.Fatalf("cluster: %v", err)
			}
			got[stages[1].name] = len(out) - len(members)
			st = tr.take(t, stages[1].marker)
			cost[si][1][fi][0] = testdb.ExplainCustom(t, pool, st.sql, st.args...)
			cost[si][1][fi][1] = testdb.ExplainGeneric(t, pool, st.sql, pfcGenericArgs(st.args)...)

			folds, _, err := h.foldParentData(ctx, childIDs, types, scopes, nil, f)
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			kept := 0
			for _, cf := range folds {
				if cf.visible && !cf.excluded {
					kept++
				}
			}
			got[stages[2].name] = kept
			st = tr.take(t, stages[2].marker)
			cost[si][2][fi][0] = testdb.ExplainCustom(t, pool, st.sql, st.args...)
			cost[si][2][fi][1] = testdb.ExplainGeneric(t, pool, st.sql, pfcGenericArgs(st.args)...)

			// Premise: the filter halves what each stage would add, so the
			// statements below are measured doing real work in both arms.
			want := map[string]int{stages[0].name: 60, stages[1].name: 50, stages[2].name: 100}
			if fi == 1 {
				want = map[string]int{stages[0].name: 30, stages[1].name: 50, stages[2].name: 50}
			}
			for name, w := range want {
				if got[name] != w {
					t.Errorf("n=%d filtered=%v %s: %d added, want %d", n, fi == 1, name, got[name], w)
				}
			}
		}
		for i, s := range stages {
			t.Logf("n=%d %-24s custom %d -> %d buffers, generic %d -> %d buffers (unfiltered -> filtered)",
				n, s.name, cost[si][i][0][0].Buffers, cost[si][i][1][0].Buffers, cost[si][i][0][1].Buffers, cost[si][i][1][1].Buffers)
		}
	}
	for i, s := range stages {
		for p, plan := range []string{"custom", "generic"} {
			for si, n := range sizes {
				unf, fil := cost[si][i][0][p].Buffers, cost[si][i][1][p].Buffers
				if fil > unf+8 {
					t.Errorf("%s, %s plan, n=%d: %d buffers filtered vs %d unfiltered — the filter costs reads\n%s",
						s.name, plan, n, fil, unf, cost[si][i][1][p].Plan)
				}
			}
			small, large := cost[0][i][1][p].Buffers, cost[1][i][1][p].Buffers
			if limit := small + small/2 + 8; large > limit {
				t.Errorf("%s, %s plan: %d buffers at %d blocks, %d at %d — above %d, the filtered statement grows with the corpus\n%s",
					s.name, plan, small, sizes[0], large, sizes[1], limit, cost[1][i][1][p].Plan)
			}
		}
	}
}
