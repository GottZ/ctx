//go:build integration

// The embed-excluded mark's index of migration 193 (issue #527) in the
// attnum class of the re-embed cutover, and the contract that keeps that
// class and the cutover's lists alike.
//
//	go test -tags=integration ./internal/events/ -run 'TestAttnumClassIsTheCutoverList|TestEmbedCutoverPendingTypeIndex|TestEmbedRollbackKeepsIndexesTheSwapDidNotRename' -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// indexesOnColumn returns the indexes of context_blocks that depend on the
// column: PostgreSQL records a dependency of an index on every column of its
// key, its expressions and its predicate (pg_depend), so this is every index
// whose definition names the column, whatever the clause.
func indexesOnColumn(t *testing.T, pool *pgxpool.Pool, column string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT DISTINCT c.relname::text FROM pg_depend d
		   JOIN pg_class c ON c.oid = d.objid AND c.relkind = 'i'
		   JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		  WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
		    AND d.refobjid = 'context_blocks'::regclass AND a.attname = $1 AND NOT a.attisdropped
		  ORDER BY 1`, column)
	if err != nil {
		t.Fatalf("indexes on %s: %v", column, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("indexes on %s: %v", column, err)
	}
	return names
}

// cutoverClass splits cutoverIndexRenames into the canonical names (renamed
// onto _old by the swap) and the _next twins (renamed onto a canonical name).
func cutoverClass() (canonical, twins []string) {
	for _, r := range cutoverIndexRenames {
		if strings.HasSuffix(r[1], "_old") {
			canonical = append(canonical, r[0])
		} else {
			twins = append(twins, r[0])
		}
	}
	slices.Sort(canonical)
	slices.Sort(twins)
	return canonical, twins
}

// TestAttnumClassIsTheCutoverList: every index of context_blocks whose key,
// expression or predicate names the embedding column is a canonical name the
// cutover renames onto _old, and every such name is one — on a database at
// the last migration. And every index that names embedding_next once the
// verify gate's twins stand is a twin the cutover renames onto its canonical
// name, and every twin is one. An index of the class that a migration adds
// without its twin, rename pair and anchor would follow embedding_old at the
// swap while the statements mean the new column by name, and be dropped with
// that column at the cleanup.
func TestAttnumClassIsTheCutoverList(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	canonical, twins := cutoverClass()
	if got := indexesOnColumn(t, pool, "embedding"); !slices.Equal(got, canonical) {
		t.Errorf("indexes of context_blocks on embedding:\n  %v\nthe cutover renames onto _old:\n  %v", got, canonical)
	}
	ensureNextIndexes(t, pool)
	if got := indexesOnColumn(t, pool, "embedding_next"); !slices.Equal(got, twins) {
		t.Errorf("indexes of context_blocks on embedding_next once the verify gate's twins stand:\n  %v\nthe cutover renames onto a canonical name:\n  %v", got, twins)
	}
	// The rollback anchors and the optional names are names of the class.
	for _, name := range canonical {
		if !slices.Contains(cutoverOldIndexNames, name+"_old") {
			t.Errorf("%s_old is not in cutoverOldIndexNames: the cleanup would leave it and the rollback not see it", name)
		}
	}
	for _, old := range cutoverOldIndexNames {
		if !slices.Contains(canonical, strings.TrimSuffix(old, "_old")) {
			t.Errorf("cutoverOldIndexNames holds %s, whose canonical index the cutover does not rename", old)
		}
	}
	for name := range cutoverOptionalIndexes {
		if !slices.Contains(canonical, strings.TrimSuffix(name, "_old")) {
			t.Errorf("cutoverOptionalIndexes holds %s, which is no index of the class", name)
		}
	}
}

// markTracer records every statement a pool sends, with its arguments.
type markTracer struct {
	mu    sync.Mutex
	stmts []markStmt
}

type markStmt struct {
	sql  string
	args []any
}

func (m *markTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	m.mu.Lock()
	m.stmts = append(m.stmts, markStmt{d.SQL, d.Args})
	m.mu.Unlock()
	return ctx
}

func (m *markTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

var indexNameInPlan = regexp.MustCompile(`"Index Name": "([^"]+)"`)

// pendingTypeIndexDef is the index of migration 193 as PostgreSQL deparses
// it, the definition the reconcile's readiness check compares with.
const pendingTypeIndexDef = `CREATE INDEX idx_embedding_pending_type_scope ON public.context_blocks USING btree (type_name, scope, id) WHERE ((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))`

// TestEmbedCutoverPendingTypeIndex: a re-embed migration runs its verify gate
// (which builds the twin idx_embedding_pending_type_scope_next on
// embedding_next) and its confirm. After the swap
// idx_embedding_pending_type_scope exists, is valid and ready, carries
// exactly the definition of 193 and depends on the serving column — the
// column that was embedding_next — while idx_embedding_pending_type_scope_old
// depends on embedding_old. A reconcile of a tenant's excluded type then
// marks its blocks with the statements of 193, and their mark batch and left
// check read idx_embedding_pending_type_scope under random_page_cost 1.1 and
// 4, custom and generic plan, with sorts off as the reconcile runs them.
// After the cleanup the canonical index stands as before and the _old one is
// gone.
func TestEmbedCutoverPendingTypeIndex(t *testing.T) {
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	s := newIndexTestScheduler(pool)
	s.embedVerifySync = true
	seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
	migID := seedMigrationRow(t, pool, "verifying")
	seedCleanMigrated(t, pool, 3)
	runVerifyCycle(t, pool, s, verifyTestCfg())
	if rep := parseReport(t, readVerifyRow(t, pool, migID).Report); rep.Result != verifyGreen {
		t.Fatalf("verify result %q, want green", rep.Result)
	}
	if got := indexesOnColumn(t, pool, "embedding_next"); !slices.Contains(got, "idx_embedding_pending_type_scope_next") {
		t.Fatalf("the verify gate built no twin of idx_embedding_pending_type_scope on embedding_next: %v", got)
	}
	nextAttnum := attnumT(t, pool, "embedding_next")

	if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := attnumT(t, pool, "embedding"); got != nextAttnum {
		t.Fatalf("serving column has attnum %d after the swap, want %d (the former embedding_next)", got, nextAttnum)
	}
	assertPendingTypeIndex(t, pool, "after the swap")
	if got := indexesOnColumn(t, pool, "embedding_old"); !slices.Contains(got, "idx_embedding_pending_type_scope_old") {
		t.Errorf("idx_embedding_pending_type_scope_old does not follow embedding_old: indexes on it %v", got)
	}

	// The reconcile after the swap: a tenant's excluded type marks its
	// blocks with the statements of 193, on the canonical index.
	tn, err := store.CreateTenant(ctx, pool, "cpt-a", "cpt-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, "cpt-a", nil); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, title := range []string{"cpt one", "cpt two", "cpt three"} {
		b, err := store.UpsertBlock(ctx, pool, "cpt", title, "body of "+title, nil, nil, "cpt-a", true, store.SensitivityWrite{}, "cpt-x")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"retrieval": map[string]any{"policy": "excluded"}})
	if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "cpt-x", Scope: "cpt-a", DisplayName: "cpt-x", Config: cfg}, nil, "cpt-test"); err != nil {
		t.Fatal(err)
	}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &markTracer{}
	pcfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer traced.Close()
	if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
		t.Fatalf("reconcile after the swap: %v", err)
	}
	for _, id := range ids {
		var marked bool
		if err := pool.QueryRow(ctx, `SELECT embed_excluded FROM context_blocks WHERE id = $1`, id).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if !marked {
			t.Errorf("block %s not marked by the reconcile after the swap", id)
		}
	}
	var batch, left int
	for _, st := range tracer.stmts {
		var kind string
		switch {
		case strings.Contains(st.sql, "UPDATE context_blocks SET embed_excluded = true") && strings.Contains(st.sql, "(type_name, scope, id) >"):
			kind, batch = "mark batch", batch+1
		case strings.HasPrefix(strings.TrimSpace(st.sql), "SELECT EXISTS") && strings.Contains(st.sql, "embedding IS NULL") && strings.Contains(st.sql, "(type_name, scope) >="):
			kind, left = "left check", left+1
		default:
			continue
		}
		for _, rpc := range testdb.RandomPageCosts {
			p := testdb.WithSessionSettings(t, pool, map[string]string{"random_page_cost": rpc,
				"enable_sort": "off", "enable_incremental_sort": "off"})
			for form, c := range map[string]testdb.PlanCost{
				"custom":  testdb.ExplainCustom(t, p, st.sql, st.args...),
				"generic": testdb.ExplainGeneric(t, p, st.sql, st.args...),
			} {
				var used []string
				for _, m := range indexNameInPlan.FindAllStringSubmatch(c.Plan, -1) {
					used = append(used, m[1])
				}
				if !slices.Contains(used, "idx_embedding_pending_type_scope") {
					t.Errorf("%s (%s, random_page_cost=%s) after the swap reads %v, not idx_embedding_pending_type_scope", kind, form, rpc, used)
				}
			}
		}
	}
	if batch == 0 || left == 0 {
		t.Errorf("the reconcile after the swap sent %d mark batches and %d left checks of 193 (want both): the index of 193 does not stand as the reconcile checks it", batch, left)
	}

	if err := CleanupEmbedMigration(ctx, pool, migID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPendingTypeIndex(t, pool, "after the cleanup")
	if indexExistsT(t, pool, "idx_embedding_pending_type_scope_old") {
		t.Errorf("idx_embedding_pending_type_scope_old still exists after the cleanup")
	}
}

// assertPendingTypeIndex checks idx_embedding_pending_type_scope: valid,
// ready, the definition of 193, on the column named embedding.
func assertPendingTypeIndex(t *testing.T, pool *pgxpool.Pool, when string) {
	t.Helper()
	var def string
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT pg_get_indexdef(indexrelid), indisvalid AND indisready FROM pg_index
		 WHERE indexrelid = to_regclass('public.idx_embedding_pending_type_scope')`).Scan(&def, &ok); err != nil {
		t.Fatalf("%s: idx_embedding_pending_type_scope: %v", when, err)
	}
	if def != pendingTypeIndexDef || !ok {
		t.Errorf("%s: idx_embedding_pending_type_scope is\n  %s (valid and ready %v)\nwant\n  %s", when, def, ok, pendingTypeIndexDef)
	}
	if !slices.Contains(indexesOnColumn(t, pool, "embedding"), "idx_embedding_pending_type_scope") {
		t.Errorf("%s: idx_embedding_pending_type_scope does not depend on the serving column embedding", when)
	}
}

func attnumT(t *testing.T, pool *pgxpool.Pool, column string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT attnum FROM pg_attribute
		 WHERE attrelid = 'context_blocks'::regclass AND attname = $1 AND NOT attisdropped`, column).Scan(&n); err != nil {
		t.Fatalf("attnum of %s: %v", column, err)
	}
	return n
}

// withCutoverClassWithout narrows the cutover's lists to those of a release
// that did not know the named canonical indexes (no twin, no rename pair, no
// optional or _old name) until the returned function — also run at the end
// of the test — puts them back.
func withCutoverClassWithout(t *testing.T, names []string) func() {
	t.Helper()
	renames, optional, olds, sisters := cutoverIndexRenames, cutoverOptionalIndexes, cutoverOldIndexNames, verifySisterIndexes
	known := func(name string) bool {
		for _, n := range names {
			if name == n || name == n+"_old" || name == n+"_next" {
				return true
			}
		}
		return false
	}
	var nr [][2]string
	for _, r := range renames {
		if !known(r[0]) {
			nr = append(nr, r)
		}
	}
	no := map[string]bool{}
	for k, v := range optional {
		if !known(k) {
			no[k] = v
		}
	}
	var nold []string
	for _, o := range olds {
		if !known(o) {
			nold = append(nold, o)
		}
	}
	var ns []struct{ name, ddl string }
	for _, s := range sisters {
		if !known(s.name) {
			ns = append(ns, s)
		}
	}
	cutoverIndexRenames, cutoverOptionalIndexes, cutoverOldIndexNames, verifySisterIndexes = nr, no, nold, ns
	var once sync.Once
	restore := func() {
		once.Do(func() {
			cutoverIndexRenames, cutoverOptionalIndexes, cutoverOldIndexNames, verifySisterIndexes = renames, optional, olds, sisters
		})
	}
	t.Cleanup(restore)
	return restore
}

// TestEmbedRollbackKeepsIndexesTheSwapDidNotRename: a re-embed migration
// swapped by a release whose cutover did not know idx_blocks_stratify_covering
// and idx_embedding_pending_type_scope (v5.78.0 and before: no twins, no
// rename pairs) left both on embedding_old under their canonical names.
// Rolled back by this release, both stand on embedding again under their
// canonical names and no _next index of them is left — the rollback renames
// an optional canonical index back onto its twin's name only when it is the
// twin, on the column that becomes embedding_next. The rollback of a swap of
// this release still puts each twin back under its _next name on
// embedding_next and the canonical index on embedding.
func TestEmbedRollbackKeepsIndexesTheSwapDidNotRename(t *testing.T) {
	ctx := context.Background()
	grown := []string{"idx_blocks_stratify_covering", "idx_embedding_pending_type_scope"}

	t.Run("swap_by_a_release_before_issue_527", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		restore := withCutoverClassWithout(t, grown)
		migID := seedConfirmReady(t, pool, 0)
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm with the lists before issue #527: %v", err)
		}
		restore()
		onOld := indexesOnColumn(t, pool, "embedding_old")
		for _, ix := range grown {
			if !slices.Contains(onOld, ix) {
				t.Fatalf("fixture: %s does not follow embedding_old after the swap before issue #527: %v", ix, onOld)
			}
		}
		if _, err := RollbackEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID, "swap before issue 527"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		onServing := indexesOnColumn(t, pool, "embedding")
		for _, ix := range grown {
			if !slices.Contains(onServing, ix) {
				t.Errorf("%s is not on embedding after the rollback: indexes on embedding %v", ix, onServing)
			}
			if indexExistsT(t, pool, ix+"_next") {
				t.Errorf("%s_next exists after the rollback: the rollback renamed an index the swap had not", ix)
			}
		}
		assertPendingTypeIndex(t, pool, "after the rollback of a swap before issue #527")
	})

	t.Run("swap_of_this_release", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedConfirmReady(t, pool, 0)
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if _, err := RollbackEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID, "swap of this release"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		onServing, onNext := indexesOnColumn(t, pool, "embedding"), indexesOnColumn(t, pool, "embedding_next")
		for _, ix := range grown {
			if !slices.Contains(onServing, ix) {
				t.Errorf("%s is not on embedding after the rollback: %v", ix, onServing)
			}
			if !slices.Contains(onNext, ix+"_next") {
				t.Errorf("%s_next is not on embedding_next after the rollback: %v", ix, onNext)
			}
		}
		assertPendingTypeIndex(t, pool, "after the rollback of a swap of this release")
	})
}
