//go:build integration

// I-B EXPLAIN + cursor gates (design/02 §3.3 Listen-Semantik / §6.2 / §7-I-B).
// The EXPLAIN gate EXPLAINs the EXPORTED production const store.WorkflowStatusListSQL
// — no SQL duplicated into the test (M072/M075 line; the store test package cannot
// be in-package because testdb imports store). Two planner claims are proven at
// 10k geseedeten issue-rows + non-issue backdrop:
//
//   - status-GIVEN  = ONE index range scan over idx_blocks_workflow_board,
//     NO Sort, NO Seq Scan (the board-column path).
//   - status-LOS    = k such range scans + Go merge; each is Sort-free. The
//     naive single ORDER BY over ALL scope issues is implemented here as a probe
//     and shown to require a Sort node — the differential the merge avoids.
package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// topCursor is the first-page keyset sentinel ListWorkflowBlocks passes into
// WorkflowStatusListSQL: (+infinity, max-uuid) so every row qualifies while the
// scan stays a clean ordered index range scan.
var (
	topUpdated = pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	topID      = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

func TestIBExplainAndCursor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// NOTIFY row triggers make bulk inserts quadratic (guard_explain line);
	// they do not influence planner shape, the only thing this gate measures.
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	// 10k issue rows across 3 statuses in scope 'expl', distinct updated_at.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name, workflow_status, updated_at)
		SELECT ('019f2211-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'issue', 'issue-' || i, 'x', 'expl', 'issue',
		       (ARRAY['backlog','in-progress','done'])[1 + (i % 3)],
		       now() - (i || ' seconds')::interval
		FROM generate_series(0, 9999) AS g(i)`); err != nil {
		t.Fatalf("seed 10k issues: %v", err)
	}
	// Backdrop the partial index MUST exclude: 3k knowledge rows (workflow_status
	// NULL) + 1k archived issue rows (NOT is_archived fails) — proves the index
	// carries only live workflow rows, and a foreign scope proves scope-prefixing.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name)
		SELECT ('019f2212-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'learnings', 'k-' || i, 'x', 'expl', 'knowledge'
		FROM generate_series(0, 2999) AS g(i)`); err != nil {
		t.Fatalf("seed knowledge backdrop: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name, workflow_status, is_archived)
		SELECT ('019f2213-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'issue', 'arch-' || i, 'x', 'expl', 'issue', 'done', true
		FROM generate_series(0, 999) AS g(i)`); err != nil {
		t.Fatalf("seed archived backdrop: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name, workflow_status, updated_at)
		SELECT ('019f2214-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'issue', 'other-' || i, 'x', 'other', 'issue', 'backlog',
		       now() - (i || ' seconds')::interval
		FROM generate_series(0, 4999) AS g(i)`); err != nil {
		t.Fatalf("seed foreign scope: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER USER`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	explain := func(name, q string, args ...any) string {
		t.Helper()
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+q, args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("%s: scan: %v", name, err)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", name, err)
		}
		plan := b.String()
		t.Logf("%s plan:\n%s", name, plan)
		return plan
	}

	scope := "expl"

	// (A) status-GIVEN: ONE index range scan, no Sort, no Seq Scan. THE production
	// const WorkflowStatusListSQL with a concrete status and a first-page cursor.
	for _, status := range []string{"backlog", "in-progress", "done"} {
		plan := explain("status-filtered("+status+")",
			store.WorkflowStatusListSQL, scope, "issue", status, topUpdated, topID, nil, 20)
		if !strings.Contains(plan, "idx_blocks_workflow_board") {
			t.Errorf("status=%s: plan does not name idx_blocks_workflow_board", status)
		}
		if strings.Contains(plan, "Seq Scan on context_blocks") {
			t.Errorf("status=%s: plan seq-scans context_blocks", status)
		}
		if strings.Contains(plan, "Sort") {
			t.Errorf("status=%s: plan contains a Sort node (should be index-ordered)", status)
		}
	}

	// (A') a cursor mid-page still rides the index range scan (keyset, no Sort).
	midCursor := time.Now().Add(-5000 * time.Second)
	plan := explain("status-filtered(cursor)",
		store.WorkflowStatusListSQL, scope, "issue", "backlog", midCursor, "019f2211-0000-7000-9000-000000000001", nil, 20)
	if !strings.Contains(plan, "idx_blocks_workflow_board") || strings.Contains(plan, "Sort") {
		t.Errorf("cursor page: want index scan without Sort")
	}

	// (B) NAIVE probe (NOT production): a single ORDER BY over ALL statuses of the
	// scope. The board index cannot order across workflow_status values (it is a
	// higher-order key than updated_at), so the planner MUST add a Sort. This is
	// the RED the per-status merge avoids.
	const naiveSQL = `
		SELECT id::text, workflow_status, updated_at
		FROM context_blocks
		WHERE scope = ANY($1::text[]) AND type_name = $2
		  AND workflow_status IS NOT NULL AND NOT is_archived
		ORDER BY updated_at DESC, id ASC
		LIMIT $3`
	naivePlan := explain("naive-order-by-all", naiveSQL, []string{scope}, "issue", 20)
	if !strings.Contains(naivePlan, "Sort") {
		t.Errorf("naive plan has NO Sort — the differential premise is wrong; plan:\n%s", naivePlan)
	}
	t.Logf("EXPLAIN differential proven: status-filtered = Sort-free index range scan; naive all-status ORDER BY = Sort node")
}

// TestIBCursorStability exercises the PUBLIC ListWorkflowBlocks over a live DB:
// paging in small windows (status-filtered AND per-status merge) reproduces a
// single full fetch exactly — lossless, duplicate-free, gap-free across page
// boundaries (§7-I-B cursor gate).
func TestIBCursorStability(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER USER`); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	// 55 issue rows across 3 statuses in scope 'curs'; rows share updated_at
	// (i/3) to exercise the id tie-break at page boundaries.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (id, category, title, content, scope, type_name, workflow_status, updated_at)
		SELECT ('019f2215-0000-7000-9000-' || lpad(to_hex(i), 12, '0'))::uuid,
		       'issue', 'c-' || i, 'x', 'curs', 'issue',
		       (ARRAY['backlog','in-progress','done'])[1 + (i % 3)],
		       'epoch'::timestamptz + ((i / 3) || ' seconds')::interval
		FROM generate_series(0, 54) AS g(i)`); err != nil {
		t.Fatalf("seed curs: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER USER`); err != nil {
		t.Fatalf("re-enable triggers: %v", err)
	}

	statuses := []string{"backlog", "in-progress", "done"}
	scopes := []string{"curs"}

	page := func(q store.WorkflowListQuery) []string {
		var got []string
		seen := map[string]bool{}
		for {
			rows, cur, err := store.ListWorkflowBlocks(ctx, pool, q)
			if err != nil {
				t.Fatalf("ListWorkflowBlocks: %v", err)
			}
			for _, r := range rows {
				if seen[r.ID] {
					t.Errorf("duplicate id %q across pages", r.ID)
				}
				seen[r.ID] = true
				got = append(got, r.ID)
			}
			if cur == nil {
				break
			}
			q.Cursor = cur
		}
		return got
	}

	// Merge path: full single fetch (limit 100 ≥ 55) vs paged (limit 7).
	fullMerge := page(store.WorkflowListQuery{Scopes: scopes, TypeName: "issue", Statuses: statuses, Limit: 100})
	if len(fullMerge) != 55 {
		t.Fatalf("merge full fetch = %d rows, want 55", len(fullMerge))
	}
	pagedMerge := page(store.WorkflowListQuery{Scopes: scopes, TypeName: "issue", Statuses: statuses, Limit: 7})
	assertSameOrder(t, "merge", pagedMerge, fullMerge)

	// Status-filtered path per status.
	for _, s := range statuses {
		full := page(store.WorkflowListQuery{Scopes: scopes, TypeName: "issue", Status: s, Limit: 100})
		paged := page(store.WorkflowListQuery{Scopes: scopes, TypeName: "issue", Status: s, Limit: 4})
		assertSameOrder(t, "status="+s, paged, full)
	}

	// The merge set count == the DB workflow-row count (no row lost or
	// double-counted between the two code paths).
	var dbCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks
		  WHERE scope='curs' AND type_name='issue' AND workflow_status IS NOT NULL AND NOT is_archived`,
	).Scan(&dbCount); err != nil {
		t.Fatalf("db count: %v", err)
	}
	if len(fullMerge) != dbCount {
		t.Errorf("merge count %d != DB workflow-row count %d", len(fullMerge), dbCount)
	}
}

func assertSameOrder(t *testing.T, label string, paged, full []string) {
	t.Helper()
	if len(paged) != len(full) {
		t.Fatalf("%s: paged %d rows, full %d rows", label, len(paged), len(full))
	}
	for i := range full {
		if paged[i] != full[i] {
			t.Errorf("%s: pos %d paged=%q full=%q", label, i, paged[i], full[i])
		}
	}
}
