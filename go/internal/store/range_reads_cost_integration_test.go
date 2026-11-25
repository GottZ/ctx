//go:build integration

package store_test

// Cost pins of issue #536 (first half, no migration): statements that read a
// whole range where their answer needs a page. Each pin traces the statement
// the store function sends — with the SET LOCAL settings of the transaction it
// ran in — and EXPLAINs it again under random_page_cost 1.1 and 4
// (testdb.RandomPageCosts), on its custom and its generic plan, on two stores
// alike but for the size of one other scope (rrSmall and rrLarge blocks, ten
// times as many). A pin holds the statement to a bound in buffers that its own
// page sets, and to the same cost on both stores: a statement that walks the
// other scope's blocks, a bitmap of every hit of a word in the corpus, or a
// sequential scan of the table reads in proportion to the other scope and
// fails both.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestRangeReads' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// The two stores: rr-other holds rrSmall or rrLarge blocks, newer than every
// block of the other scopes, every tenth an issue in backlog, every one
// carrying the word rrCommon.
const (
	rrSmall = 10_000
	rrLarge = 100_000

	rrRead  = "rr-read"  // 300 blocks over store.UpsertBlock, older than rr-other
	rrProj  = "rr-proj"  // rrProjIssues issues in three statuses, older than rr-other
	rrOther = "rr-other" // the large scope, newer than the rest
	rrX2    = "rr-x2"    // 10 000 pending blocks of rrPtx
	rrGuard = "rr-guard" // rrGuardPairs pairs the guard flagged

	rrPtx        = "rr-ptx"
	rrProjIssues = 20_000
	rrGuardPairs = 20
	rrCommon     = "zebrafrage" // in every issue of rr-proj and every block of rr-other
	rrRare       = "seltenwort" // in every 200th issue of rr-proj, nowhere else
)

// rrSeed fills a store: the guard's pairs first (store.UpsertBlock,
// store.StoreEmbedding, guard.RunGuardBatch — the guard checks them before
// any other block exists), then rr-read over store.UpsertBlock, rr-proj's
// issues, the blocks of rrPtx (rr-other's row of the type excludes it, so the
// 2 500 there are marked on insert; rr-x2's row lifts it, so its 10 000 wait
// for a vector, also once a _global row excludes the name) and
// rr-other in bulk (INSERT … SELECT, the production triggers run; the
// per-row path would take minutes at the large size).
func rrSeed(n int) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		for i := 0; i < rrGuardPairs; i++ {
			for j, vec := range [][]float32{gkVec(2*i, 1.0, 0, 0), gkVec(2*i, 0.96, 2*i+1, 0.28)} {
				title := fmt.Sprintf("rr-guard-%02d-%d", i, j)
				b, err := store.UpsertBlock(ctx, pool, "learnings", title, "guard list body of "+title, nil, nil, rrGuard, true, store.SensitivityWrite{}, "knowledge")
				if err != nil {
					return fmt.Errorf("upsert %s: %w", title, err)
				}
				if err := store.StoreEmbedding(ctx, pool, b.ID, "rr-model", vec); err != nil {
					return fmt.Errorf("embed %s: %w", title, err)
				}
			}
		}
		processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 4*rrGuardPairs+10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400})
		if err != nil {
			return fmt.Errorf("guard batch: %w", err)
		}
		if processed != 2*rrGuardPairs {
			return fmt.Errorf("guard batch: processed %d, want %d", processed, 2*rrGuardPairs)
		}
		for i := 0; i < 300; i++ {
			cat := "learnings"
			if i%2 == 1 {
				cat = "decisions"
			}
			if _, err := store.UpsertBlock(ctx, pool, cat, fmt.Sprintf("rr read %d", i), fmt.Sprintf("read body %d", i), nil, nil, rrRead, true, store.SensitivityWrite{Value: "internal"}, ""); err != nil {
				return fmt.Errorf("upsert read %d: %w", i, err)
			}
		}
		for scope, policy := range map[string]string{rrOther: "excluded", rrX2: "full-pass"} {
			cfg, _ := json.Marshal(map[string]any{"retrieval": map[string]any{"policy": policy}})
			if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: rrPtx, Scope: scope, DisplayName: rrPtx, Config: cfg}, nil, "rr-seed"); err != nil {
				return fmt.Errorf("type row %s: %w", scope, err)
			}
		}
		for _, st := range []string{
			`INSERT INTO context_blocks (category, title, content, scope, type_name, workflow_status, created_at, updated_at)
			 SELECT 'issue', 'rr issue ' || i, 'issue body ' || i || ' ` + rrCommon + `' || CASE WHEN i % 200 = 0 THEN ' ` + rrRare + `' ELSE '' END,
			        '` + rrProj + `', 'issue', (ARRAY['backlog','in-progress','done'])[1 + i % 3],
			        now() - interval '2 days' + make_interval(secs => i), now() - interval '2 days' + make_interval(secs => i)
			   FROM generate_series(1, ` + strconv.Itoa(rrProjIssues) + `) i`,
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 SELECT 'ptx', 'rr ptx other ' || i, 'x', '` + rrOther + `', '` + rrPtx + `' FROM generate_series(1, 2500) i`,
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 SELECT 'ptx', 'rr ptx x2 ' || i, 'x', '` + rrX2 + `', '` + rrPtx + `' FROM generate_series(1, 10000) i`,
			`INSERT INTO context_blocks (category, title, content, scope, type_name, workflow_status, created_at, updated_at)
			 SELECT CASE WHEN i % 10 = 0 THEN 'issue' ELSE 'learnings' END, 'rr other ' || i, 'other body ' || i || ' ` + rrCommon + `', '` + rrOther + `',
			        CASE WHEN i % 10 = 0 THEN 'issue' ELSE 'knowledge' END, CASE WHEN i % 10 = 0 THEN 'backlog' END,
			        clock_timestamp(), clock_timestamp()
			   FROM generate_series(1, ` + strconv.Itoa(n) + `) i`,
		} {
			if _, err := pool.Exec(ctx, st); err != nil {
				return fmt.Errorf("%w\n%s", err, st)
			}
		}
		if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		_, err = pool.Exec(ctx, `VACUUM (ANALYZE) context_blocks`)
		return err
	}
}

// rrStores returns the small and the large store (each built once per test
// binary).
func rrStores(t *testing.T) (small, large *pgxpool.Pool) {
	t.Helper()
	small = testdb.SetupSeededTestDB(t, fmt.Sprintf("i536-rr-%d-%d", rrSmall, rrProjIssues), rrSeed(rrSmall))
	large = testdb.SetupSeededTestDB(t, fmt.Sprintf("i536-rr-%d-%d", rrLarge, rrProjIssues), rrSeed(rrLarge))
	return small, large
}

// rrTraced is a pool on pool's database whose statements the returned
// tracer records.
func rrTraced(t *testing.T, pool *pgxpool.Pool) (*pgxpool.Pool, *reviewTracer) {
	t.Helper()
	tr := &reviewTracer{}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = tr
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, tr
}

// rrStmt is one traced statement: its text, its arguments (the query mode
// option taken out) and the SET LOCAL settings of the transaction it ran in.
type rrStmt struct {
	sql   string
	args  []any
	mode  pgx.QueryExecMode // the option it was sent with, 0 = the pool's default
	local map[string]string
}

// rrFind returns the last traced statement match admits.
func rrFind(t *testing.T, traced []reviewStmt, match func(sql string) bool) rrStmt {
	t.Helper()
	var found *rrStmt
	var local map[string]string
	for _, st := range traced {
		low := strings.ToLower(st.sql)
		switch {
		case strings.HasPrefix(low, "begin") || low == "commit" || low == "rollback":
			local = nil
			continue
		case strings.HasPrefix(st.sql, "SET LOCAL "):
			local = setLocals(t, st.sql)
			continue
		case !match(st.sql):
			continue
		}
		s := rrStmt{sql: st.sql, local: local}
		for _, a := range st.args {
			if m, ok := a.(pgx.QueryExecMode); ok {
				s.mode = m
				continue
			}
			s.args = append(s.args, a)
		}
		found = &s
	}
	if found == nil {
		t.Fatal("the statement was not traced")
	}
	return *found
}

// rrCost is what one EXPLAIN (ANALYZE, BUFFERS) execution read.
type rrCost struct {
	buffers int64
	plan    string
}

var rrIndexes = regexp.MustCompile(`"Index Name": "[^"]+"`)

func (c rrCost) indexes() string { return strings.Join(rrIndexes.FindAllString(c.plan, -1), " ") }

// rrLiteral renders one argument as a literal of EXECUTE.
func rrLiteral(t *testing.T, a any) string {
	t.Helper()
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	stamp := func(tm time.Time) string { return q(tm.UTC().Format("2006-01-02 15:04:05.999999") + "+00") }
	switch v := a.(type) {
	case nil:
		return "NULL"
	case string:
		return q(v)
	case []string:
		if v == nil {
			return "NULL" // pgx sends a nil slice as NULL
		}
		el := make([]string, len(v))
		for i, e := range v {
			el[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(e) + `"`
		}
		return q("{" + strings.Join(el, ",") + "}")
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	case time.Time:
		return stamp(v)
	case *time.Time:
		if v == nil {
			return "NULL"
		}
		return stamp(*v)
	case pgtype.Timestamptz:
		switch {
		case !v.Valid:
			return "NULL"
		case v.InfinityModifier == pgtype.Infinity:
			return q("infinity")
		case v.InfinityModifier == pgtype.NegativeInfinity:
			return q("-infinity")
		}
		return stamp(v.Time)
	}
	t.Fatalf("no literal for an argument of type %T", a)
	return ""
}

// rrPlanners keeps one pool per database and settings: a fresh connection
// reads the catalog on its first statements, which a measurement would count.
type rrPlanners struct {
	t     *testing.T
	pools map[string]*pgxpool.Pool
}

func (r *rrPlanners) pool(db *pgxpool.Pool, rpc string, local map[string]string) *pgxpool.Pool {
	settings := map[string]string{"random_page_cost": rpc}
	for k, v := range local {
		settings[k] = v
	}
	keys := make([]string, 0, len(settings))
	for k, v := range settings {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	key := fmt.Sprintf("%p|%s", db, strings.Join(keys, ","))
	if r.pools == nil {
		r.pools = map[string]*pgxpool.Pool{}
	}
	if r.pools[key] == nil {
		r.pools[key] = testdb.WithSessionSettings(r.t, db, settings)
	}
	return r.pools[key]
}

// explain executes st once unmeasured and once under EXPLAIN (ANALYZE,
// BUFFERS) as a prepared statement with its arguments as literals, under
// plan_cache_mode force_custom_plan or force_generic_plan, on a connection
// with random_page_cost rpc and st's SET LOCAL settings.
func (r *rrPlanners) explain(db *pgxpool.Pool, rpc string, generic bool, st rrStmt) rrCost {
	t := r.t
	t.Helper()
	ctx := context.Background()
	conn, err := r.pool(db, rpc, st.local).Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	mode := "force_custom_plan"
	if generic {
		mode = "force_generic_plan"
	}
	for _, s := range []string{"SET plan_cache_mode = " + mode, "PREPARE rr_explain AS " + st.sql} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v\n%s", strings.SplitN(s, " ", 2)[0], err, st.sql)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, "DEALLOCATE rr_explain")
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode")
	}()
	lits := make([]string, len(st.args))
	for i, a := range st.args {
		lits[i] = rrLiteral(t, a)
	}
	exec := "EXECUTE rr_explain"
	if len(lits) > 0 {
		exec += "(" + strings.Join(lits, ", ") + ")"
	}
	if _, err := conn.Exec(ctx, exec, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("execute: %v\n%s", err, st.sql)
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+exec, pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, st.sql)
	}
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain json: %v", err)
	}
	hit, _ := doc[0].Plan["Shared Hit Blocks"].(float64)
	read, _ := doc[0].Plan["Shared Read Blocks"].(float64)
	if generic && len(st.args) > 0 && !regexp.MustCompile(`\$[0-9]+`).Match(raw) {
		t.Fatalf("the plan references no parameter, so it is not the generic plan:\n%s", raw)
	}
	return rrCost{buffers: int64(hit + read), plan: string(raw)}
}

// rrPlans are the four plans a pin reads.
var rrPlans = func() []struct {
	rpc     string
	generic bool
} {
	var out []struct {
		rpc     string
		generic bool
	}
	for _, rpc := range testdb.RandomPageCosts {
		for _, g := range []bool{false, true} {
			out = append(out, struct {
				rpc     string
				generic bool
			}{rpc, g})
		}
	}
	return out
}()

func rrPlanName(rpc string, generic bool) string {
	if generic {
		return "generic plan at random_page_cost " + rpc
	}
	return "custom plan at random_page_cost " + rpc
}

// rrHold checks one statement's costs on both stores: at most bound buffers
// each, and the large store at most slack above the small one.
func rrHold(t *testing.T, label, plan string, small, large rrCost, bound, slack int64) {
	t.Helper()
	t.Logf("%s (%s): %d buffers at %d other blocks, %d at %d; %s", label, plan, small.buffers, rrSmall, large.buffers, rrLarge, large.indexes())
	if small.buffers > bound || large.buffers > bound {
		t.Errorf("%s (%s): %d / %d buffers at %d / %d other blocks, more than %d: it reads more than its page",
			label, plan, small.buffers, large.buffers, rrSmall, rrLarge, bound)
	}
	if large.buffers > small.buffers+slack {
		t.Errorf("%s (%s): %d buffers at %d other blocks against %d at %d: its cost follows another scope's blocks",
			label, plan, large.buffers, rrLarge, small.buffers, rrSmall)
	}
}

// TestRangeReadsFullGraphSeed (issue #536 §4): the flat seed of the graph's
// load-all reads the read scopes' own newest rows and the granted blocks by
// primary key. Before, the generic plan at either value walked
// idx_context_created from the newest block of the corpus and read every
// block of rr-other (8 593 buffers at 100 000 other blocks), and so did the
// custom plan when a granted block was newer than the read scope and the
// limit small. A reader of few scopes gets the merge form (one arm per scope,
// full sorts off, the generic plan), a category filter the planned form (the
// planner weighs the category, a custom plan on every execution); each is
// pinned on the plan it runs.
func TestRangeReadsFullGraphSeed(t *testing.T) {
	small, large := rrStores(t)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	grants := func(pool *pgxpool.Pool) []string {
		rows, err := pool.Query(ctx, `(SELECT id::text FROM context_blocks WHERE scope = $1 AND type_name = 'knowledge' ORDER BY created_at DESC LIMIT 3)
			UNION ALL (SELECT id::text FROM context_blocks WHERE scope = $2 ORDER BY created_at LIMIT 2)`, rrOther, rrRead)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	know := []string{"knowledge"}
	// What a page may read: per read scope the rows of its page (at most
	// limit + 1, at most the scope's rows) and a descent, four buffers per
	// granted block (a primary-key probe), and a constant for the plan's top
	// — listArmBound's measure (an index walk counts a buffer per row it
	// fetches). The other scope's 100 000 blocks are in none of it.
	pageBound := func(rows []int, grants int) int64 {
		b := int64(16 + 4*grants)
		for _, r := range rows {
			b += listArmBound(r)
		}
		return b
	}
	for _, c := range []struct {
		name    string
		scopes  []string
		grants  bool
		cats    []string
		limit   int
		bound   int64
		planned bool
	}{
		{"read scope older than the other scope", []string{rrRead}, false, nil, 200, pageBound([]int{201}, 0), false},
		{"read scope, granted blocks newer than it, limit 4", []string{rrRead}, true, nil, 4, pageBound([]int{5}, 5), false},
		{"read scope named twice beside the flagged scope, granted blocks", []string{rrRead, rrGuard, rrRead}, true, nil, 200,
			pageBound([]int{201, 2 * rrGuardPairs}, 5), false},
		{"the large scope read", []string{rrOther}, false, nil, 200, pageBound([]int{201}, 0), false},
		{"read scope, a category half of it carries", []string{rrRead}, false, []string{"decisions"}, 100, pageBound([]int{300}, 0), true},
		{"read scope, a category none of its blocks carries", []string{rrRead}, false, []string{"issue"}, 100, pageBound([]int{300}, 0), true},
	} {
		var st [2]rrStmt
		for i, pool := range []*pgxpool.Pool{small, large} {
			traced, tr := rrTraced(t, pool)
			var g []string
			if c.grants {
				g = grants(pool)
			}
			tr.take()
			res, err := store.FullGraph(ctx, traced, store.EgoParams{Limit: c.limit, EdgeLimit: 4000, Categories: c.cats}, c.scopes, g, know)
			if err != nil {
				t.Fatalf("%s: FullGraph: %v", c.name, err)
			}
			if want := c.limit; c.cats != nil && c.cats[0] == "issue" {
				if len(res.Nodes) != 0 {
					t.Fatalf("%s: %d nodes, want none", c.name, len(res.Nodes))
				}
			} else if len(res.Nodes) != want {
				t.Fatalf("%s: %d nodes, want a full page of %d", c.name, len(res.Nodes), want)
			}
			st[i] = rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "left(") && strings.Contains(sql, "context_blocks") })
			merge := strings.Contains(st[i].sql, "UNION ALL")
			switch {
			case c.planned && (merge || len(st[i].local) != 0 || st[i].mode != pgx.QueryExecModeCacheDescribe):
				t.Errorf("%s: the seed ran the merge form, with settings %v or in mode %v, want the planned form planned on every execution", c.name, st[i].local, st[i].mode)
			case !c.planned && (!merge || st[i].local["enable_sort"] != "off" || st[i].local["enable_incremental_sort"] == "off" ||
				st[i].local["plan_cache_mode"] != "force_generic_plan" || st[i].mode != pgx.QueryExecModeCacheStatement):
				t.Errorf("%s: the seed ran without the merge form, with settings %v or in mode %v, want full sorts off on the generic plan of a prepared statement", c.name, st[i].local, st[i].mode)
			}
		}
		for _, p := range rrPlans {
			if p.generic == c.planned {
				continue // the plan the form runs: the merge form's generic, the planned form's custom
			}
			// The small and the large store may differ in the plan of the
			// page (a bitmap and a sort, or a walk that counts a buffer per
			// row), not in what it reads: the slack is the bound.
			rrHold(t, c.name, rrPlanName(p.rpc, p.generic),
				planners.explain(small, p.rpc, p.generic, st[0]), planners.explain(large, p.rpc, p.generic, st[1]), c.bound, c.bound)
		}
	}
}

// TestRangeReadsEmbedExcludedStarScopes (issue #536 §3): a '*' entry of the
// embed-excluded reconcile lists the scopes of the name's pending blocks one
// descent of idx_embedding_pending_type_scope per scope, with sorts off.
// rr-other holds 2 500 marked blocks of the name beside the other scope's
// pending blocks of another type, rr-x2 10 000 pending blocks of the name.
// Before, the statement read every pending entry of the name (65 buffers on
// the index at 100 000 other blocks) or, on the production image's generic
// plan at 4, every pending block of the scopes through idx_context_scope.
func TestRangeReadsEmbedExcludedStarScopes(t *testing.T) {
	small, large := rrStores(t)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	var st [2]rrStmt
	for i, pool := range []*pgxpool.Pool{small, large} {
		traced, tr := rrTraced(t, pool)
		// An excluded _global row of the name queues it with the group '*'
		// (176_embed_excluded_owner.sql); rr-other's blocks stay marked, rr-x2's
		// row keeps its blocks pending.
		createType(t, pool, rrPtx, "_global", "excluded")
		tr.take()
		if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		st[i] = rrFind(t, tr.take(), func(sql string) bool {
			return strings.Contains(sql, "SELECT scope::text FROM") && strings.Contains(sql, "embedding_stale") && strings.Contains(sql, "embedding IS NULL")
		})
		if st[i].local["enable_sort"] != "off" {
			t.Errorf("the scopes of the '*' entry were read with sorts on (settings %v)", st[i].local)
		}
		if got := markedCount(t, pool, rrOther, rrPtx); got != 2500 {
			t.Fatalf("%d of rr-other's blocks of the name marked, want 2500", got)
		}
	}
	for _, p := range rrPlans {
		// Two scopes hold a block of the name: a descent each (metapage, root,
		// leaf), the end of the range, and the stale half.
		rrHold(t, "scopes of a '*' entry", rrPlanName(p.rpc, p.generic),
			planners.explain(small, p.rpc, p.generic, st[0]), planners.explain(large, p.rpc, p.generic, st[1]), 20, 3)
	}
}

// TestRangeReadsIssueSearch (issue #536 §5): the issue search plans with its
// word on every execution — an unnamed statement (QueryExecModeCacheDescribe),
// so a connection never switches it to the generic plan, which built a bitmap
// of every block of the corpus carrying the word — and the scope stated as a
// range keeps the custom plan off a walk of idx_context_created through the
// corpus: the project's 20 000 issues are older than rr-other's blocks, and
// the created search's custom plan at random_page_cost 4 walked every one of
// them. The word rrCommon is in every block of rr-other and every issue of
// the project, rrRare in every 200th issue of the project. Each custom plan
// reads at most what the statement before read on its custom plan (the
// plans that were right stay right) and the same on both stores; what it
// reads beside the project is the type's bitmap over the corpus's issues (a
// few index pages, as before).
func TestRangeReadsIssueSearch(t *testing.T) {
	small, large := rrStores(t)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	for _, c := range []struct {
		name, word, sort string
	}{
		{"common word, sorted by update", rrCommon, store.SortUpdated},
		{"common word, sorted by creation", rrCommon, store.SortCreated},
		{"rare word, sorted by update", rrRare, store.SortUpdated},
		{"rare word, sorted by creation", rrRare, store.SortCreated},
	} {
		var st, old [2]rrStmt
		for i, pool := range []*pgxpool.Pool{small, large} {
			traced, tr := rrTraced(t, pool)
			tr.take()
			rows, _, err := store.SearchIssues(ctx, traced, store.IssueReadQuery{Scope: rrProj, Q: c.word, Sort: c.sort, Limit: 50})
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if len(rows) != 50 {
				t.Fatalf("%s: %d issues, want a page of 50", c.name, len(rows))
			}
			st[i] = rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "plainto_tsquery") })
			if st[i].mode != pgx.QueryExecModeCacheDescribe {
				t.Errorf("%s: sent in query mode %v, want an unnamed statement planned on every execution (QueryExecModeCacheDescribe)", c.name, st[i].mode)
			}
			old[i] = rrStmt{sql: rrOldSearchUpdated, args: st[i].args}
			if c.sort == store.SortCreated {
				old[i].sql = rrOldSearchCreated
			}
		}
		for _, rpc := range testdb.RandomPageCosts {
			s, l := planners.explain(small, rpc, false, st[0]), planners.explain(large, rpc, false, st[1])
			os, ol := planners.explain(small, rpc, false, old[0]), planners.explain(large, rpc, false, old[1])
			plan := rrPlanName(rpc, false)
			t.Logf("%s (%s): the statement before read %d / %d", c.name, plan, os.buffers, ol.buffers)
			// Not above what the custom plan before read (the plans that were
			// right stay right), nor above a page of 50 rows.
			for _, x := range []struct {
				size     int
				got, old rrCost
			}{{rrSmall, s, os}, {rrLarge, l, ol}} {
				if bound := max(x.old.buffers, listArmBound(50)+16) + 5; x.got.buffers > bound {
					t.Errorf("%s (%s): %d buffers at %d other blocks, more than %d (the statement before: %d)", c.name, plan, x.got.buffers, x.size, bound, x.old.buffers)
				}
			}
			// The same on both stores: the bitmaps of the scope and of the
			// type gain index pages with the corpus (an index page per ~1 300
			// entries: 9 000 more issues on the large store), nothing more.
			rrHold(t, c.name, plan, s, l, 1<<62, 25)
		}
	}
	// What the planner keeps of the search on a connection after more than
	// five executions: no prepared statement, so no generic plan to switch to.
	cfg := small.Config()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	for i := 0; i < 8; i++ {
		if _, _, err := store.SearchIssues(ctx, one, store.IssueReadQuery{Scope: rrProj, Q: rrCommon, Limit: 50}); err != nil {
			t.Fatal(err)
		}
	}
	var prepared, generic int64
	if err := one.QueryRow(ctx, `SELECT count(*), COALESCE(sum(generic_plans), 0) FROM pg_prepared_statements WHERE statement LIKE '%plainto_tsquery%'`,
		pgx.QueryExecModeSimpleProtocol).Scan(&prepared, &generic); err != nil {
		t.Fatal(err)
	}
	if prepared != 0 || generic != 0 {
		t.Errorf("after 8 searches on one connection: %d prepared search statements with %d generic plans, want none", prepared, generic)
	}
}

// TestRangeReadsIssueLists (issue #536 §7): a page of the issue lists walks
// its workflow index and stops after the page, in a transaction with sorts
// off. The planner estimates the project's issues of a status as if scope,
// type and status were independent — a few dozen here — and at
// random_page_cost 4 a sort of them looked cheaper: the status list read
// every issue of the status through a bitmap and sorted it, the created
// list's custom plan walked idx_context_created from the newest block of
// rr-other.
func TestRangeReadsIssueLists(t *testing.T) {
	small, large := rrStores(t)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		call func(pool *pgxpool.Pool) error
		sql  string
	}{
		{"status list, first page", func(pool *pgxpool.Pool) error {
			_, _, err := store.ListIssues(ctx, pool, store.IssueListQuery{Scopes: []string{rrProj}, Status: "backlog", Limit: 50})
			return err
		}, "ORDER BY b.updated_at DESC"},
		{"board column, first page", func(pool *pgxpool.Pool) error {
			_, err := store.BoardColumns(ctx, pool, rrProj, []string{"backlog"}, nil, 50)
			return err
		}, "ORDER BY b.updated_at DESC"},
		{"created list, all statuses, first page", func(pool *pgxpool.Pool) error {
			_, _, err := store.ListIssuesByCreated(ctx, pool, store.IssueReadQuery{Scope: rrProj, Limit: 50})
			return err
		}, "ORDER BY b.created_at DESC"},
		{"created list, one status, first page", func(pool *pgxpool.Pool) error {
			_, _, err := store.ListIssuesByCreated(ctx, pool, store.IssueReadQuery{Scope: rrProj, Status: "done", Limit: 50})
			return err
		}, "ORDER BY b.created_at DESC"},
	} {
		var st [2]rrStmt
		for i, pool := range []*pgxpool.Pool{small, large} {
			traced, tr := rrTraced(t, pool)
			tr.take()
			if err := c.call(traced); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			st[i] = rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, c.sql) && !strings.Contains(sql, "plainto_tsquery") })
		}
		for _, p := range rrPlans {
			// 50 rows of the walk: their index pages and heap pages.
			rrHold(t, c.name, rrPlanName(p.rpc, p.generic),
				planners.explain(small, p.rpc, p.generic, st[0]), planners.explain(large, p.rpc, p.generic, st[1]), 30, 3)
		}
	}
}

// TestRangeReadsGuardList (issue #536, comment of 2026-11-24 03:23Z): the
// guard list reads its page of flagged blocks and looks up each one's
// matched block by primary key. Before, the join compared mb.id::text with
// the key, which no index serves, and read every row of context_blocks once
// per list (a hash join over a sequential scan).
func TestRangeReadsGuardList(t *testing.T) {
	small, large := rrStores(t)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	var st [2]rrStmt
	for i, pool := range []*pgxpool.Pool{small, large} {
		traced, tr := rrTraced(t, pool)
		tr.take()
		items, err := store.GuardList(ctx, traced, []string{rrGuard}, "", "", nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2*rrGuardPairs {
			t.Fatalf("guard list: %d items, want %d", len(items), 2*rrGuardPairs)
		}
		st[i] = rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "guard_matched_id") })
	}
	for _, p := range rrPlans {
		// The scope's 40 flagged rows and 40 primary-key probes of a few
		// pages each; on the larger store the key is one level deeper, a
		// buffer more per probe.
		rrHold(t, "guard list of 40 flagged blocks", rrPlanName(p.rpc, p.generic),
			planners.explain(small, p.rpc, p.generic, st[0]), planners.explain(large, p.rpc, p.generic, st[1]), 300, 2*rrGuardPairs+5)
	}
}

// TestRangeReadsResolveBlockID (issue #536, comment of 2026-11-24 17:25Z): a
// block id prefix reads the range of context_blocks_pkey its ids lie in, not
// the reader's rows. Before, `id::text LIKE prefix || '%'` had no index path
// and read every visible row of the reader (5 696 buffers for a reader of
// 100 000 blocks). The range holds every id of the corpus that starts with
// the prefix, in any scope: the ids of a uuidv7 lead with their creation time
// in milliseconds, so a prefix of 18 characters or more spans less than a
// millisecond of writes (the cases here), one of 8 characters 65 seconds of
// them — few on a store that grows by writes, every block of a bulk load
// (this fixture's 100 000 rows were written within a minute). The lookup is
// an unnamed statement planned with its range on every execution: a generic
// plan cannot tell a narrow range from a wide one (it walked the reader's
// scopes on idx_blocks_scope_updated, 1 079 buffers for a reader of 100 000
// blocks), so only custom plans are read here, and a connection keeps no
// prepared statement of it.
func TestRangeReadsResolveBlockID(t *testing.T) {
	small, large := rrStores(t)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		cut    int
		scopes []string
	}{
		{"18 characters, reader of the other scope", 18, []string{rrOther}},
		{"19 characters ending on a hyphen, reader of the other scope", 19, []string{rrOther}},
		{"24 characters, reader of the other scope and the read scope", 24, []string{rrOther, rrRead}},
	} {
		var st [2]rrStmt
		for i, pool := range []*pgxpool.Pool{small, large} {
			var id string
			if err := pool.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1 ORDER BY id OFFSET 777 LIMIT 1`, rrOther).Scan(&id); err != nil {
				t.Fatal(err)
			}
			traced, tr := rrTraced(t, pool)
			tr.take()
			_, matches, err := store.ResolveBlockID(ctx, traced, id[:c.cut], c.scopes, nil)
			if err != nil && !errors.Is(err, store.ErrAmbiguousID) {
				t.Fatalf("%s: %v", c.name, err)
			}
			if len(matches) == 0 {
				t.Fatalf("%s: the prefix of %s resolves to no block", c.name, id)
			}
			st[i] = rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "SELECT id, title, category, tags, scope, updated_at") })
			if st[i].mode != pgx.QueryExecModeCacheDescribe {
				t.Errorf("%s: sent in query mode %v, want an unnamed statement planned on every execution (QueryExecModeCacheDescribe)", c.name, st[i].mode)
			}
		}
		for _, rpc := range testdb.RandomPageCosts {
			// At most 11 matches (the cap): their index and heap pages.
			rrHold(t, c.name, rrPlanName(rpc, false),
				planners.explain(small, rpc, false, st[0]), planners.explain(large, rpc, false, st[1]), 40, 3)
		}
	}
	// A connection keeps no prepared statement of the lookup, so it never
	// switches it to a generic plan.
	cfg := small.Config()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	var id string
	if err := small.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE scope = $1 ORDER BY id LIMIT 1`, rrRead).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, _, err := store.ResolveBlockID(ctx, one, id[:20], []string{rrRead}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var prepared int64
	if err := one.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements WHERE statement LIKE '%tags, scope, updated_at%'`,
		pgx.QueryExecModeSimpleProtocol).Scan(&prepared); err != nil {
		t.Fatal(err)
	}
	if prepared != 0 {
		t.Errorf("after 8 lookups on one connection: %d prepared statements of the lookup, want none", prepared)
	}
}
