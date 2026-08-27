//go:build integration

// C2-2 gates — the two consumer fixes the OPS-W1 review left as auflagen A2 (#1,
// SearchBlocks) and A3 (#2, issue FTS).
//
// Migration 145 turned idx_context_ts_de / idx_context_ts_en into PARTIAL GIN
// indexes over `type_name NOT IN ('checkpoint','system-meta')`. A partial index
// is only reachable when the planner can PROVE its predicate from the query's
// own quals. Both consumers state their type restriction through a BIND
// parameter (`type_name = ANY($n)` / `type_name = $2`), and a parameter carries
// no proof under a GENERIC plan. pgx runs the extended protocol with a
// statement cache, so the generic plan is production-reachable from the 6th
// execution per connection (plancache.c choose_custom_plan) — the review
// measured the two resulting outliers at 100 000 rows: 11,5× (SearchBlocks) and
// 18,2× (issue FTS).
//
// The gate tool is therefore the one the review's auflage names:
// `SET LOCAL plan_cache_mode = force_generic_plan` + PREPARE / EXPLAIN EXECUTE.
// A plain EXPLAIN folds the parameters into constants, proves the implication
// itself and shows a healthy plan — which is exactly why the existing W6 gate
// could not see the regression.
//
// Every EXPLAIN here runs the statement PRODUCTION runs: the exported builder
// hook (SearchBlocksSQLForTest) and the exported issue consts. No SQL is copied
// into this file except the deny conjunct itself, which is spelled out so that a
// drift away from the index predicate breaks the gate instead of travelling
// with it.
//
// Run: `go test -tags=integration -p 1 ./internal/store/ -run TestC22 -count=1 -v`.
package store_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	c22Scope  = "c22"
	c22Needle = "seltenwortxy"
	c22Rows   = 100000

	// The two conjuncts under test, spelled as the production statements carry
	// them (blocks.go FTS branch / issues_read.go).
	c22SearchRemove = ` AND type_name NOT IN ('checkpoint','system-meta')`
	c22IssueRemove  = `AND b.type_name NOT IN ('checkpoint','system-meta')`

	// The index predicate in PostgreSQL's normal form (pinned identically by
	// internal/rrf TestOPSW1PredicateNormalForm).
	c22IndexPredicate = `(type_name <> ALL (ARRAY['checkpoint'::text, 'system-meta'::text]))`
)

// c22Seed writes the review's N2/N3 fixture: 100 000 rows in one scope, the
// live type mix (deny-listed types dominant), the needle on every 499th row.
// 499 and the 13-row type cycle are coprime, so the needle lands on EVERY type
// — without that the identity gates below would compare two empty sets (the
// vacuum class the OPS-W1 review found and closed in its own fixture).
func c22Seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, type_name, workflow_status)
		SELECT 'c22', 'c22 block ' || s,
		       'c22 body ' || s || ' ' || md5(s::text)
		           || CASE WHEN s % 499 = 0 THEN ' `+c22Needle+`' ELSE '' END,
		       $1,
		       CASE s % 13
		           WHEN 0 THEN 'checkpoint' WHEN 1 THEN 'checkpoint' WHEN 2 THEN 'checkpoint'
		           WHEN 3 THEN 'checkpoint' WHEN 4 THEN 'checkpoint' WHEN 5 THEN 'checkpoint'
		           WHEN 6 THEN 'system-meta' WHEN 7 THEN 'issue' WHEN 8 THEN 'issue'
		           WHEN 9 THEN 'reference' ELSE 'knowledge'
		       END,
		       CASE WHEN s % 13 IN (7,8) THEN 'open' ELSE NULL END
		FROM generate_series(1, `+strconv.Itoa(c22Rows)+`) s`, c22Scope); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Plan inspection
// ---------------------------------------------------------------------------

type c22Node struct {
	NodeType  string    `json:"Node Type"`
	IndexName string    `json:"Index Name"`
	RelName   string    `json:"Relation Name"`
	TotalCost float64   `json:"Total Cost"`
	Plans     []c22Node `json:"Plans"`
}

func c22Flatten(n c22Node, out *[]string) {
	label := n.NodeType
	switch {
	case n.IndexName != "":
		label += " on " + n.IndexName
	case n.RelName != "":
		label += " on " + n.RelName
	}
	*out = append(*out, label)
	for _, child := range n.Plans {
		c22Flatten(child, out)
	}
}

// c22Querier is the slice of pgxpool.Pool / pgx.Tx the EXPLAIN helper needs —
// the generic-plan probes must run inside ONE transaction (PREPARE plus
// SET LOCAL plan_cache_mode), everything else runs on the pool.
type c22Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func c22Explain(t *testing.T, ctx context.Context, q c22Querier, label, stmt string, args ...any) ([]string, float64) {
	t.Helper()
	var raw []byte
	if err := q.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+stmt, args...).Scan(&raw); err != nil {
		t.Fatalf("%s: explain: %v\n%s", label, err, stmt)
	}
	var wrapper []struct {
		Plan c22Node `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("%s: decode plan: %v", label, err)
	}
	var flat []string
	c22Flatten(wrapper[0].Plan, &flat)
	t.Logf("%s: cost=%.2f  %s", label, wrapper[0].Plan.TotalCost, strings.Join(flat, " | "))
	return flat, wrapper[0].Plan.TotalCost
}

// c22GenericPlan PREPAREs stmt, forces the generic plan and EXPLAINs the
// EXECUTE. sig is the PREPARE parameter list, execArgs the literal argument
// list — both must match the statement's bind parameters in order.
func c22GenericPlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, name, sig, stmt, execArgs string) ([]string, float64) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("%s: begin: %v", label, err)
	}
	// Rollback as a defer, never as a tail call: a t.Fatalf with an open
	// transaction leaves the pool connection checked out and deadlocks Close.
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("%s: force_generic_plan: %v", label, err)
	}
	if _, err := tx.Exec(ctx, "PREPARE "+name+sig+" AS "+stmt); err != nil {
		t.Fatalf("%s: prepare: %v\n%s", label, err, stmt)
	}
	return c22Explain(t, ctx, tx, label, "EXECUTE "+name+execArgs)
}

func c22UsesFTSIndex(plan []string) bool {
	for _, node := range plan {
		if strings.Contains(node, "idx_context_ts_de") || strings.Contains(node, "idx_context_ts_en") {
			return true
		}
	}
	return false
}

func c22SeqScansBlocks(plan []string) bool {
	for _, node := range plan {
		if node == "Seq Scan on context_blocks" {
			return true
		}
	}
	return false
}

// c22IDs runs a statement through an id-only wrapper so the production and the
// stripped form are read the same way, ranks included.
func c22IDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, stmt string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT t.id::text FROM ("+stmt+") t", args...)
	if err != nil {
		t.Fatalf("%s: query: %v", label, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("%s: scan: %v", label, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows: %v", label, err)
	}
	return ids
}

// c22Strip removes the deny conjunct from a statement — the pre-fix form, used
// both as the negative probe and as the identity reference. Exactly one
// occurrence, so a statement that never carried it fails loudly instead of
// being compared against itself.
func c22Strip(t *testing.T, label, stmt, remove string) string {
	t.Helper()
	if n := strings.Count(stmt, remove); n != 1 {
		t.Fatalf("%s: statement carries the deny conjunct %d times, want exactly 1:\n%s", label, n, stmt)
	}
	return strings.Replace(stmt, remove, "", 1)
}

// ---------------------------------------------------------------------------
// The gates
// ---------------------------------------------------------------------------

func TestC22PartialFTSConsumers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	c22Seed(t, ctx, pool)

	// Premise: the indexes really are partial in this database. Without it every
	// plan below would be green for the wrong reason.
	t.Run("premise_indexes_are_partial", func(t *testing.T) {
		for _, idx := range []string{"idx_context_ts_de", "idx_context_ts_en"} {
			var pred *string
			if err := pool.QueryRow(ctx, `
				SELECT pg_get_expr(i.indpred, i.indrelid)
				  FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
				 WHERE c.relname = $1`, idx).Scan(&pred); err != nil {
				t.Fatalf("%s: predicate: %v", idx, err)
			}
			got := "(none)"
			if pred != nil {
				got = *pred
			}
			if got != c22IndexPredicate {
				t.Fatalf("%s predicate = %s, want %s (migration 145 not applied as expected)", idx, got, c22IndexPredicate)
			}
			t.Logf("%s predicate = %s", idx, got)
		}
	})

	c22SearchBlocksGates(t, ctx, pool)
	c22IssueFTSGates(t, ctx, pool)
	// LAST — it rebuilds the two indexes and therefore changes the database for
	// everything after it. The per-test database (testdb.SetupTestDB) is
	// isolated, so the blast radius ends with this test function.
	c22AboveGuardGates(t, ctx, pool)
}

// c22AboveGuardGates is the second branch the briefing asks for: ABOVE the mass
// guard of migration 145 the indexes are NOT rebuilt and stay FULL, while the
// statements carry the conjunct all the same. A full index satisfies every
// predicate a partial one satisfies, so nothing has to be proven — the question
// is only whether the added conjunct costs anything there. The review measured
// that state for the RRF functions (Z1: 697,39 against 703,52, identical plan
// shape); this gate measures it for the two consumer statements.
//
// The state is reproduced by rebuilding both indexes WITHOUT the predicate,
// which is exactly what the above branch leaves behind (145:284-289 warns and
// leaves the existing full index alone). It reproduces the index shape, not the
// migration's decision path — that branch is gated in internal/rrf
// (TestOPSW1MassGuard).
func c22AboveGuardGates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Run("above_the_guard_full_indexes_cost_nothing", func(t *testing.T) {
		for _, idx := range [][2]string{{"idx_context_ts_de", "ts_de"}, {"idx_context_ts_en", "ts_en"}} {
			if _, err := pool.Exec(ctx, "DROP INDEX "+idx[0]); err != nil {
				t.Fatalf("drop %s: %v", idx[0], err)
			}
			if _, err := pool.Exec(ctx, "CREATE INDEX "+idx[0]+" ON context_blocks USING GIN("+idx[1]+")"); err != nil {
				t.Fatalf("recreate %s full: %v", idx[0], err)
			}
			var pred *string
			if err := pool.QueryRow(ctx, `
				SELECT pg_get_expr(i.indpred, i.indrelid)
				  FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
				 WHERE c.relname = $1`, idx[0]).Scan(&pred); err != nil {
				t.Fatalf("%s: predicate: %v", idx[0], err)
			}
			if pred != nil {
				t.Fatalf("%s is still partial (%s) — the above-branch premise is not reproduced", idx[0], *pred)
			}
		}
		if _, err := pool.Exec(ctx, "ANALYZE context_blocks"); err != nil {
			t.Fatalf("analyze: %v", err)
		}

		// (1) SearchBlocks, types opt-in.
		stmt, _ := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
			Query: c22Needle, ReadScopes: []string{c22Scope}, Limit: 20, Compact: true,
			Types: []string{"knowledge", "reference"},
		})
		const searchSig = "(text[],text,text[],uuid[],int)"
		const searchArgs = "('{" + c22Scope + "}','" + c22Needle + "','{knowledge,reference}','{}',20)"
		planProd, costProd := c22GenericPlan(t, ctx, pool, "above A2 production (generic)", "c22_above_n2", searchSig, stmt, searchArgs)
		planBare, costBare := c22GenericPlan(t, ctx, pool, "above A2 without conjunct (generic)", "c22_above_n2_bare",
			searchSig, c22Strip(t, "above A2 strip", stmt, c22SearchRemove), searchArgs)
		c22AssertNoRegression(t, "above A2", planProd, costProd, planBare, costBare)

		// (2) Issue FTS.
		const issueSig = "(text,text,text,text,timestamptz,uuid,text[],int)"
		const issueArgs = "('" + c22Scope + "','issue','','" + c22Needle + "'," +
			"'2999-01-01'::timestamptz,'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid,NULL::text[],20)"
		planIssue, costIssue := c22GenericPlan(t, ctx, pool, "above A3 production (generic)", "c22_above_n3",
			issueSig, store.IssueSearchUpdatedSQL, issueArgs)
		planIssueBare, costIssueBare := c22GenericPlan(t, ctx, pool, "above A3 without conjunct (generic)", "c22_above_n3_bare",
			issueSig, c22Strip(t, "above A3 strip", store.IssueSearchUpdatedSQL, c22IssueRemove), issueArgs)
		c22AssertNoRegression(t, "above A3", planIssue, costIssue, planIssueBare, costIssueBare)
	})
}

// c22AssertNoRegression is the above-branch criterion: with a FULL index the
// statement carrying the conjunct must still reach the FTS index, must not
// sequential-scan where the bare form did not, and must not cost measurably
// more. 5 % headroom absorbs the extra qual's own evaluation cost; a plan-shape
// change would blow far past it.
func c22AssertNoRegression(t *testing.T, label string, plan []string, cost float64, bare []string, bareCost float64) {
	t.Helper()
	if !c22UsesFTSIndex(plan) {
		t.Errorf("%s: full index, and the statement still misses the FTS index: %s", label, strings.Join(plan, " | "))
	}
	if c22SeqScansBlocks(plan) && !c22SeqScansBlocks(bare) {
		t.Errorf("%s: the conjunct introduced a Seq Scan: %s", label, strings.Join(plan, " | "))
	}
	if cost > bareCost*1.05 {
		t.Errorf("%s: cost %.2f exceeds the conjunct-free form %.2f by more than 5 %%", label, cost, bareCost)
	}
	t.Logf("%s: cost %.2f with the conjunct, %.2f without it", label, cost, bareCost)
}

// c22SearchBlocksGates is auflage A2: the browse/FTS statement declares the
// index predicate whenever the caller's own type opt-in already implies it.
func c22SearchBlocksGates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	const sig = "(text[],text,text[],uuid[],int)"

	optIn := func(types, exclude []string) (string, []any) {
		return store.SearchBlocksSQLForTest(store.SearchBlocksParams{
			Query: c22Needle, ReadScopes: []string{c22Scope}, Limit: 20, Compact: true,
			Types: types, TypesExclude: exclude,
		})
	}

	// (A2-1) types opt-in: the partial GIN is reachable under a GENERIC plan.
	t.Run("A2_types_opt_in_rides_the_partial_gin", func(t *testing.T) {
		stmt, _ := optIn([]string{"knowledge", "reference"}, nil)
		plan, cost := c22GenericPlan(t, ctx, pool, "A2 types opt-in (generic)", "c22_n2_types", sig, stmt,
			"('{"+c22Scope+"}','"+c22Needle+"','{knowledge,reference}','{}',20)")
		if !c22UsesFTSIndex(plan) {
			t.Errorf("generic plan names neither FTS GIN index (cost %.2f): %s", cost, strings.Join(plan, " | "))
		}
		if c22SeqScansBlocks(plan) {
			t.Errorf("generic plan seq-scans context_blocks (cost %.2f): %s", cost, strings.Join(plan, " | "))
		}
	})

	// (A2-2) typesExclude opt-in: the same, through the other filter shape.
	t.Run("A2_types_exclude_opt_in_rides_the_partial_gin", func(t *testing.T) {
		stmt, _ := optIn(nil, []string{"checkpoint", "system-meta"})
		plan, cost := c22GenericPlan(t, ctx, pool, "A2 typesExclude opt-in (generic)", "c22_n2_excl", sig, stmt,
			"('{"+c22Scope+"}','"+c22Needle+"','{checkpoint,system-meta}','{}',20)")
		if !c22UsesFTSIndex(plan) {
			t.Errorf("generic plan names neither FTS GIN index (cost %.2f): %s", cost, strings.Join(plan, " | "))
		}
		if c22SeqScansBlocks(plan) {
			t.Errorf("generic plan seq-scans context_blocks (cost %.2f): %s", cost, strings.Join(plan, " | "))
		}
	})

	// (A2-3) The negative probe: WITHOUT the conjunct — the statement this path
	// ran before the fix — the same query loses the FTS index under a generic
	// plan. This is what makes A2-1/A2-2 a measurement instead of a coincidence.
	t.Run("A2_without_the_conjunct_the_index_drops_out", func(t *testing.T) {
		stmt, _ := optIn([]string{"knowledge", "reference"}, nil)
		stripped := c22Strip(t, "A2 strip", stmt, c22SearchRemove)
		plan, cost := c22GenericPlan(t, ctx, pool, "A2 stripped (generic)", "c22_n2_strip", sig, stripped,
			"('{"+c22Scope+"}','"+c22Needle+"','{knowledge,reference}','{}',20)")
		if c22UsesFTSIndex(plan) {
			t.Errorf("stripped statement still rides the FTS index (cost %.2f) — the conjunct carries nothing: %s",
				cost, strings.Join(plan, " | "))
		}
	})

	// (A2-4) The D5 asymmetry: without a type opt-in nothing is declared, and
	// deny-listed blocks stay findable through browse/search.
	t.Run("A2_no_type_filter_keeps_deny_types_browseable", func(t *testing.T) {
		stmt, _ := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
			Query: c22Needle, ReadScopes: []string{c22Scope}, Limit: 50, Compact: true,
		})
		if strings.Contains(stmt, c22SearchRemove) {
			t.Fatalf("unfiltered browse statement declares the deny conjunct — result set would change:\n%s", stmt)
		}
		rows, err := store.SearchBlocks(ctx, pool, c22Needle, []string{c22Scope}, "", nil, 50, true, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		deny := 0
		for _, r := range rows {
			if r.TypeName == "checkpoint" || r.TypeName == "system-meta" {
				deny++
			}
		}
		if deny == 0 {
			t.Errorf("no deny-listed block among %d unfiltered hits — browse lost the D5 asymmetry (or the fixture is vacuous)", len(rows))
		}
		t.Logf("unfiltered browse: %d hits, %d of them deny-listed types", len(rows), deny)
	})

	// (A2-5) A type opt-in that ASKS for a deny-listed type must not opt in —
	// the conjunct would empty the result set.
	t.Run("A2_opt_in_for_a_deny_type_stays_out", func(t *testing.T) {
		stmt, _ := optIn([]string{"checkpoint"}, nil)
		if strings.Contains(stmt, c22SearchRemove) {
			t.Fatalf("types=[checkpoint] declares the deny conjunct — the result set collapses to empty:\n%s", stmt)
		}
		rows, err := store.SearchBlocks(ctx, pool, c22Needle, []string{c22Scope}, "", nil, 50, true, nil, nil,
			[]string{"checkpoint"}, nil, nil)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(rows) == 0 {
			t.Errorf("types=[checkpoint] returned 0 hits — the deny-listed type became unsearchable")
		}
		t.Logf("types=[checkpoint]: %d hits", len(rows))
	})

	// (A2-6) Set identity: the fix is a planner opt-in, not a filter. For every
	// filter shape that opts in, the production statement and its stripped form
	// return the same ids in the same order.
	t.Run("A2_set_identity_across_the_opt_in", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			types   []string
			exclude []string
		}{
			{"types=[knowledge,reference]", []string{"knowledge", "reference"}, nil},
			{"types=[knowledge]", []string{"knowledge"}, nil},
			{"types=[issue,reference]", []string{"issue", "reference"}, nil},
			{"typesExclude=[checkpoint,system-meta]", nil, []string{"checkpoint", "system-meta"}},
		} {
			stmt, args := optIn(tc.types, tc.exclude)
			stripped := c22Strip(t, tc.name, stmt, c22SearchRemove)
			got := c22IDs(t, ctx, pool, tc.name+" (production)", stmt, args...)
			want := c22IDs(t, ctx, pool, tc.name+" (stripped)", stripped, args...)
			if len(want) == 0 {
				t.Errorf("%s: reference form returned 0 ids — identity would be vacuous", tc.name)
				continue
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: id sequence changed: %d vs %d ids", tc.name, len(got), len(want))
				continue
			}
			t.Logf("%s: %d ids, identical with and without the conjunct", tc.name, len(got))
		}
	})
}

// c22IssueFTSGates is auflage A3: the issue FTS statements declare the index
// predicate, which is a strict no-op beside their own `type_name = $2` equality.
func c22IssueFTSGates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	const sig = "(text,text,text,text,timestamptz,uuid,text[],int)"
	const execArgs = "('" + c22Scope + "','issue','','" + c22Needle + "'," +
		"'2999-01-01'::timestamptz,'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid,NULL::text[],20)"

	stmts := []struct {
		name string
		sql  string
	}{
		{"IssueSearchUpdatedSQL", store.IssueSearchUpdatedSQL},
		{"IssueSearchCreatedSQL", store.IssueSearchCreatedSQL},
	}

	// (A3-1) Both statements ride the FTS GIN under a GENERIC plan.
	t.Run("A3_issue_fts_rides_the_gin_under_generic_plan", func(t *testing.T) {
		for i, s := range stmts {
			plan, cost := c22GenericPlan(t, ctx, pool, "A3 "+s.name+" (generic)",
				"c22_n3_"+strconv.Itoa(i), sig, s.sql, execArgs)
			if !c22UsesFTSIndex(plan) {
				t.Errorf("%s: generic plan names neither FTS GIN index (cost %.2f): %s",
					s.name, cost, strings.Join(plan, " | "))
			}
			if c22SeqScansBlocks(plan) {
				t.Errorf("%s: generic plan seq-scans context_blocks (cost %.2f): %s",
					s.name, cost, strings.Join(plan, " | "))
			}
		}
	})

	// (A3-2) The negative probe: the pre-fix form loses the FTS indexes under a
	// generic plan — the 18,2× outlier the review measured.
	t.Run("A3_without_the_conjunct_the_index_drops_out", func(t *testing.T) {
		stripped := c22Strip(t, "A3 strip", store.IssueSearchUpdatedSQL, c22IssueRemove)
		plan, cost := c22GenericPlan(t, ctx, pool, "A3 stripped (generic)", "c22_n3_strip", sig, stripped, execArgs)
		if c22UsesFTSIndex(plan) {
			t.Errorf("stripped issue statement still rides the FTS index (cost %.2f) — the conjunct carries nothing: %s",
				cost, strings.Join(plan, " | "))
		}
	})

	// (A3-3) Set identity on a fixture that CONTAINS deny-listed rows carrying
	// the same needle: the conjunct is redundant beside `type_name = $2`, so the
	// ids do not move.
	t.Run("A3_set_identity", func(t *testing.T) {
		var denyHits int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM context_blocks
			 WHERE scope = $1 AND type_name IN ('checkpoint','system-meta')
			   AND (ts_de @@ plainto_tsquery('german', $2) OR ts_en @@ plainto_tsquery('english', $2))`,
			c22Scope, c22Needle).Scan(&denyHits); err != nil {
			t.Fatalf("deny census: %v", err)
		}
		if denyHits == 0 {
			t.Fatalf("fixture carries no deny-listed row matching the needle — the identity claim would be vacuous")
		}
		t.Logf("fixture carries %d deny-listed rows matching the needle", denyHits)

		top := pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
		args := []any{c22Scope, "issue", "", c22Needle, top, "ffffffff-ffff-ffff-ffff-ffffffffffff", nil, 20}
		for _, s := range stmts {
			stripped := c22Strip(t, s.name, s.sql, c22IssueRemove)
			got := c22IDs(t, ctx, pool, s.name+" (production)", s.sql, args...)
			want := c22IDs(t, ctx, pool, s.name+" (stripped)", stripped, args...)
			if len(want) == 0 {
				t.Errorf("%s: reference form returned 0 ids — identity would be vacuous", s.name)
				continue
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: id sequence changed: %d vs %d ids", s.name, len(got), len(want))
				continue
			}
			t.Logf("%s: %d ids, identical with and without the conjunct", s.name, len(got))
		}
	})
}
