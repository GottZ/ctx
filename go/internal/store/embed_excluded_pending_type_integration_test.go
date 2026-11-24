//go:build integration

// Issue #527, migration 193: the embed-excluded mark batch and the mark's
// left check read the range of their name and scope of
// idx_embedding_pending_type_scope, not every pending block of the scope.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedMark(CostIndependentOfOtherPending|WithoutThePendingTypeIndex)' -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// pendingTypeIndex is the index of migration 193.
const pendingTypeIndex = "idx_embedding_pending_type_scope"

// pendingTypeStatementBound bounds the shared buffers of one mark batch or
// left check of the steady state on the index of 193: the descent of the
// range (metapage, root, leaf) and the pages of the few entries of the name
// and scope. Measured 4 and 6 on the fixture below; a statement that reads
// the scope's pending blocks of other names reads 209 at the small size.
const pendingTypeStatementBound = 20

// isMarkPending reports whether a traced statement is the mark batch or the
// mark's left check (either form): they select the unmarked pending blocks.
func isMarkPending(sql string) bool {
	return strings.Contains(sql, "embedding IS NULL AND NOT is_archived AND NOT embed_excluded") &&
		!strings.Contains(sql, "SELECT scope::text FROM context_blocks")
}

// markForm names the form of a traced mark statement: "193" for the
// statements on the range of name and scope, "before193" for the scope's
// range of idx_embedding_pending_scope_sens.
func markForm(sql string) string {
	switch {
	case strings.Contains(sql, "(type_name, scope, id) >") || strings.Contains(sql, "(type_name, scope) >="):
		return "193"
	case strings.Contains(sql, "(scope, sensitivity, created_at, id) >") || strings.Contains(sql, "AND scope >= $2::text AND scope <= $2::text"):
		return "before193"
	}
	return "unknown"
}

// statementCost is the replay of one traced mark statement under one plan.
type statementCost struct {
	head    string
	buffers int64
	indexes []string
}

// replayEach reads every traced statement keep admits again, each by itself
// under every value of testdb.RandomPageCosts, on its custom and its generic
// plan, under the SET LOCAL settings of the traced transaction it ran in,
// and returns the costs per plan ("custom random_page_cost=4", …).
func (r *entryReplay) replayEach(label string, traced []reviewStmt, keep func(sql string) bool) map[string][]statementCost {
	t, ctx := r.t, context.Background()
	t.Helper()
	out := map[string][]statementCost{}
	var local map[string]string
	for _, st := range traced {
		switch {
		case st.sql == "begin" || st.sql == "commit" || st.sql == "rollback":
			local = nil
			continue
		case strings.HasPrefix(st.sql, "SET LOCAL "):
			local = setLocals(t, st.sql)
			continue
		case !keep(st.sql):
			continue
		}
		head := strings.Join(strings.Fields(st.sql), " ")
		if len(head) > 70 {
			head = head[:70]
		}
		for _, rpc := range testdb.RandomPageCosts {
			p := r.planner(rpc, local)
			if _, err := p.Exec(ctx, st.sql, st.args...); err != nil {
				t.Fatalf("%s: warm-up: %v", label, err)
			}
			plans := map[string]testdb.PlanCost{"custom": testdb.ExplainCustom(t, p, st.sql, st.args...)}
			// The pass without a created_at of the releases before 193 is
			// constant false since migration 186 (NOT NULL): its plan names no
			// parameter, so it has no generic plan to read.
			if !strings.Contains(st.sql, "created_at IS NULL") {
				plans["generic"] = testdb.ExplainGeneric(t, p, st.sql, st.args...)
			}
			for form, c := range plans {
				plan := form + " random_page_cost=" + rpc
				ix := indexNames.FindAllString(c.Plan, -1)
				out[plan] = append(out[plan], statementCost{head, c.Buffers, ix})
				t.Logf("   %-70s %-26s %5d buffers on %v", head, plan, c.Buffers, ix)
			}
		}
	}
	return out
}

// TestEmbedExcludedMarkCostIndependentOfOtherPending (the cost pin of
// migration 193): tenant A's scope holds N blocks of another type that wait
// for a vector, and M blocks of the name pm-x, written while A's row of the
// name was lifted; tenant B's scope holds N pending blocks of pm-x (B has no
// row of the name: they stay pending and unmarked). A narrows its row to
// `excluded`, the reconcile marks A's M blocks, a VACUUM removes the entries
// the marked rows left, and every mark batch and left check of that pass is
// read again — what each later batch and check of the scope reads. Under
// random_page_cost 1.1 and 4 (testdb.RandomPageCosts), custom and generic
// plan, each statement reads idx_embedding_pending_type_scope within
// pendingTypeStatementBound buffers, as much with N = M = 2 000 as with
// 20 000: the range of name and scope holds neither the other names of the
// scope, nor the name's blocks of another scope, nor the scope's marked
// blocks of the name. Before 193 each statement read the scope's range of
// idx_embedding_pending_scope_sens, every pending block of the scope. Every
// batch ran with sorts off, and B's rows are not rewritten.
func TestEmbedExcludedMarkCostIndependentOfOtherPending(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	traced, tracer := r3Traced(t, dsn)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "pm-a", "pm-a")
	ownerTenant(t, pool, "pm-b", "pm-b")
	// Three blocks of the name through the write path; the rest of the
	// fixture in bulk below.
	ownerSeed(t, pool, "pm-a", "pm-x", 3)
	reconcile(t, pool)
	batchSettingsHook(t, pool)
	replay := newEntryReplay(t, pool)
	var row *store.BlockTypeRow
	worst := map[string][]int64{}
	from := 0
	for _, n := range []int{2_000, 20_000} {
		for _, st := range []string{
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'pm', 'pm other ' || i, 'x', 'knowledge', 'pm-a', (ARRAY['public','internal'])[1 + i % 2], now()
			   FROM generate_series($1::int + 1, $2::int) i`,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'pm', 'pm name ' || i, 'x', 'pm-x', 'pm-a', (ARRAY['public','internal'])[1 + i % 2], now()
			   FROM generate_series($1::int + 1, $2::int) i`,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'pm', 'pm b ' || i, 'x', 'pm-x', 'pm-b', 'public', now()
			   FROM generate_series($1::int + 1, $2::int) i`,
		} {
			if _, err := pool.Exec(ctx, st, from, n); err != nil {
				t.Fatal(err)
			}
		}
		from = n
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
			t.Fatal(err)
		}
		reconcile(t, pool)
		var otherA, nameA, nameB int
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE scope = 'pm-a' AND type_name <> 'pm-x'),
			        count(*) FILTER (WHERE scope = 'pm-a' AND type_name = 'pm-x'), count(*) FILTER (WHERE scope = 'pm-b')
			   FROM context_blocks WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded`).Scan(&otherA, &nameA, &nameB); err != nil {
			t.Fatal(err)
		}
		if otherA != n || nameA != n+3 || nameB != n {
			t.Fatalf("fixture: A holds %d/%d pending blocks of other names and %d/%d of pm-x, B %d/%d of pm-x", otherA, n, nameA, n+3, nameB, n)
		}
		stB := scopeState(t, pool, "pm-b")
		label := fmt.Sprintf("mark: A's pm-x excluded, N = M = %d", n)
		if row == nil {
			row = createType(t, pool, "pm-x", "pm-a", "excluded")
		} else {
			updateType(t, pool, row.ID, "pm-a", "excluded")
		}
		tracer.take()
		if _, err := pool.Exec(ctx, `DELETE FROM pin_batch_settings`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
			t.Fatalf("%s: reconcile: %v", label, err)
		}
		assertBatchesRanWithoutSorts(t, pool, label)
		if got := markedCount(t, pool, "pm-a", "pm-x"); got != n+3 {
			t.Fatalf("%s: %d/%d of A's pm-x marked", label, got, n+3)
		}
		if scopeState(t, pool, "pm-b") != stB {
			t.Errorf("%s: A's entry rewrote B's rows", label)
		}
		if _, err := pool.Exec(ctx, `VACUUM context_blocks`); err != nil {
			t.Fatal(err)
		}
		costs := replay.replayEach(label, tracer.take(), isMarkPending)
		for plan, cs := range costs {
			var max int64
			for _, c := range cs {
				max = maxInt64(max, c.buffers)
				if !strings.Contains(strings.Join(c.indexes, " "), `"`+pendingTypeIndex+`"`) {
					t.Errorf("%s (%s): %q reads %v, not %s", label, plan, c.head, c.indexes, pendingTypeIndex)
				}
			}
			worst[plan] = append(worst[plan], max)
		}
		// A lift clears A's blocks of the name for the next size.
		updateType(t, pool, row.ID, "pm-a", "full-pass")
		reconcile(t, pool)
		if got := markedCount(t, pool, "pm-a", "pm-x"); got != 0 {
			t.Fatalf("%s: %d of A's pm-x still marked after the lift", label, got)
		}
	}
	if len(worst) == 0 {
		t.Fatal("no mark statement was traced")
	}
	for plan, w := range worst {
		if len(w) != 2 {
			t.Errorf("%s: %d sizes measured, want 2", plan, len(w))
			continue
		}
		small, large := w[0], w[1]
		t.Logf("%s: the costliest mark statement reads %d buffers at N = M = 2 000 and %d at 20 000", plan, small, large)
		if large > small+small/2+5 {
			t.Errorf("%s: a mark statement reads %d buffers at N = M = 2 000 and %d at 20 000: it walks blocks outside its name and scope", plan, small, large)
		}
		for _, b := range []int64{small, large} {
			if b > pendingTypeStatementBound {
				t.Errorf("%s: a mark statement reads %d buffers, above %d: it does not read the range of its name and scope", plan, b, pendingTypeStatementBound)
			}
		}
	}
}

func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}

// TestEmbedExcludedMarkWithoutThePendingTypeIndex: while
// idx_embedding_pending_type_scope does not stand as migration 193 builds it
// — dropped (a corpus above the threshold before its out-of-band build),
// INVALID (an aborted CONCURRENTLY build), or another definition under the
// name — the reconcile sends the mark and its left check of the release
// before 193 and marks every block; once the index stands, the next pass
// sends the statements of 193.
func TestEmbedExcludedMarkWithoutThePendingTypeIndex(t *testing.T) {
	const build = `CREATE INDEX ` + pendingTypeIndex + ` ON context_blocks (type_name, scope, id)
		WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded`
	for _, c := range []struct {
		name  string
		setup []string
		clear string
	}{
		{"dropped", []string{`DROP INDEX ` + pendingTypeIndex}, ""},
		{"invalid", []string{`UPDATE pg_index SET indisvalid = false WHERE indexrelid = '` + pendingTypeIndex + `'::regclass`},
			`DROP INDEX ` + pendingTypeIndex},
		{"foreign_definition", []string{`DROP INDEX ` + pendingTypeIndex,
			`CREATE INDEX ` + pendingTypeIndex + ` ON context_blocks (type_name, scope, id) WHERE embedding IS NULL AND NOT is_archived`},
			`DROP INDEX ` + pendingTypeIndex},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			traced, tracer := r3Traced(t, dsn)
			ctx := context.Background()
			reconcile(t, pool)
			ownerTenant(t, pool, "pw-a", "pw-a")
			ids := ownerSeed(t, pool, "pw-a", "pw-x", 3)
			for _, s := range c.setup {
				if _, err := pool.Exec(ctx, s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			// forms runs one traced pass and returns the forms of the mark
			// statements it sent.
			forms := func(label string) map[string]int {
				t.Helper()
				tracer.take()
				if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
					t.Fatalf("%s: reconcile: %v", label, err)
				}
				got := map[string]int{}
				for _, st := range tracer.take() {
					if isMarkPending(st.sql) {
						got[markForm(st.sql)]++
					}
				}
				return got
			}
			row := createType(t, pool, "pw-x", "pw-a", "excluded")
			got := forms("without the index")
			if got["before193"] == 0 || got["193"] != 0 || got["unknown"] != 0 {
				t.Errorf("without the index of 193 the pass sent the mark statements %v, want those before 193 only", got)
			}
			for _, id := range ids {
				if !marker(t, pool, id) {
					t.Errorf("block %s not marked by the statements before 193", id)
				}
			}
			if c.clear != "" {
				if _, err := pool.Exec(ctx, c.clear); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, build); err != nil {
				t.Fatal(err)
			}
			updateType(t, pool, row.ID, "pw-a", "full-pass")
			reconcile(t, pool)
			updateType(t, pool, row.ID, "pw-a", "excluded")
			got = forms("with the index")
			if got["193"] == 0 || got["before193"] != 0 || got["unknown"] != 0 {
				t.Errorf("with the index of 193 the pass sent the mark statements %v, want those of 193 only", got)
			}
			for _, id := range ids {
				if !marker(t, pool, id) {
					t.Errorf("block %s not marked by the statements of 193", id)
				}
			}
		})
	}
}

// pendingTypeIndexDef is the definition of the index of 193 as PostgreSQL
// deparses it (store.embedExcludedPendingIndexReadySQL compares with it).
const pendingTypeIndexDef = `CREATE INDEX idx_embedding_pending_type_scope ON public.context_blocks USING btree (type_name, scope, id) WHERE ((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))`

// pendingTypeIndexState reads the index of 193: its definition and validity.
func pendingTypeIndexState(t *testing.T, pool *pgxpool.Pool) (def string, valid bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT pg_get_indexdef(indexrelid), indisvalid AND indisready FROM pg_index
		 WHERE indexrelid = to_regclass('public.`+pendingTypeIndex+`')`).Scan(&def, &valid); err != nil {
		t.Fatalf("%s: %v", pendingTypeIndex, err)
	}
	return def, valid
}
