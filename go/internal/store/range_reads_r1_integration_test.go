//go:build integration

package store_test

// Review round 1 of issue #536 (PR #558): what the lists' transaction holds
// when their statements run, the guard list's page before its probes, and
// the rows the search and the created list answer against the base's
// statements.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestRangeReadsR1' -count=1 -v

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
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

// r1ConnState is what a connection holds just before a statement runs on it,
// read on that connection by a query of the tracer's own: inside the
// statement's transaction when it has one.
type r1ConnState struct {
	sql                                string
	mode                               pgx.QueryExecMode // the option it was sent with, 0 = the pool's default
	pid                                uint32
	txStatus                           byte // 'T' inside a transaction block, 'I' outside
	sort, incSort, indexScan, readOnly string
	txStart                            string // transaction_timestamp(): one value per transaction
}

// r1ConnProbe is a tracer that reads r1ConnState before every statement
// match admits. Its own query matches nothing, so it does not read itself.
type r1ConnProbe struct {
	t     *testing.T
	match func(sql string) bool
	mu    sync.Mutex
	seen  []r1ConnState
}

func (p *r1ConnProbe) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !p.match(d.SQL) {
		return ctx
	}
	st := r1ConnState{sql: d.SQL, pid: conn.PgConn().PID(), txStatus: conn.PgConn().TxStatus()}
	if len(d.Args) > 0 {
		st.mode, _ = d.Args[0].(pgx.QueryExecMode)
	}
	if err := conn.QueryRow(ctx, `SELECT current_setting('enable_sort'), current_setting('enable_incremental_sort'),
		current_setting('enable_indexscan'), current_setting('transaction_read_only'), transaction_timestamp()::text`, pgx.QueryExecModeSimpleProtocol).
		Scan(&st.sort, &st.incSort, &st.indexScan, &st.readOnly, &st.txStart); err != nil {
		p.t.Errorf("probe before %q: %v", d.SQL, err)
	}
	p.mu.Lock()
	p.seen = append(p.seen, st)
	p.mu.Unlock()
	return ctx
}

func (p *r1ConnProbe) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *r1ConnProbe) take() []r1ConnState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.seen
	p.seen = nil
	return s
}

// r1Probed is a pool on pool's database whose statements on context_blocks
// the returned probe reads before they run.
func r1Probed(t *testing.T, pool *pgxpool.Pool) (*pgxpool.Pool, *r1ConnProbe) {
	t.Helper()
	pr := &r1ConnProbe{t: t, match: func(sql string) bool { return strings.Contains(sql, "context_blocks") }}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = pr
	if cfg.MaxConns < 4 {
		cfg.MaxConns = 4 // a statement sent on the pool beside the transaction finds a connection
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, pr
}

// r1Issues writes n issues of status into scope through the production write
// path (store.InsertIssueBlock), every third with the label a.
func r1Issues(t *testing.T, pool *pgxpool.Pool, scope, status string, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i := 0; i < n; i++ {
		var meta map[string]any
		if i%3 == 0 {
			meta = map[string]any{"labels": []string{"a"}}
		}
		if _, err := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: fmt.Sprintf("r1 %s %d", status, i),
			Content: "r1 issue body", Metadata: meta, Status: status}, []string{scope}); err != nil {
			t.Fatalf("insert issue: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRangeReadsR1ListsReadInTheirReadOnlyTransaction (review round 1 of
// #536, L2): every statement of a list call runs on the connection of the
// call's one transaction, inside it, read-only, with the settings the call
// chose — read on that connection just before each statement. A statement
// sent on the pool beside the transaction runs on another connection,
// outside any transaction, with sorts on, and the planner was free to sort
// every issue of a status again; a transaction begun without READ ONLY lets
// a statement of the read write.
func TestRangeReadsR1ListsReadInTheirReadOnlyTransaction(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope, other = "r1-tx", "r1-tx-2"
	for _, s := range []struct {
		status string
		n      int
	}{{"open", 30}, {"doing", 8}, {"done", 2}} {
		r1Issues(t, pool, scope, s.status, s.n)
		r1Issues(t, pool, other, s.status, s.n)
	}
	probed, pr := r1Probed(t, pool)
	statuses := []string{"open", "doing", "done"}
	for _, c := range []struct {
		name string
		call func() error
		// want is the setting of the page's statements: the walk with sorts
		// off, or the status's bitmap with index scans off.
		want  func(st r1ConnState) bool
		pages int
	}{
		{"created list, all statuses", func() error {
			_, _, err := store.ListIssuesByCreated(ctx, probed, store.IssueReadQuery{Scope: scope, Limit: 10})
			return err
		}, func(st r1ConnState) bool { return st.sort == "off" && st.incSort == "off" }, 1},
		{"created list, a status of two issues", func() error {
			_, _, err := store.ListIssuesByCreated(ctx, probed, store.IssueReadQuery{Scope: scope, Status: "done", Limit: 10})
			return err
		}, func(st r1ConnState) bool { return st.indexScan == "off" && st.sort == "on" }, 1},
		{"board", func() error {
			_, err := store.BoardColumns(ctx, probed, scope, statuses, nil, 10)
			return err
		}, func(st r1ConnState) bool { return st.sort == "off" && st.incSort == "off" }, 3},
		{"workflow list, two scopes", func() error {
			_, _, err := store.ListWorkflowBlocks(ctx, probed, store.WorkflowListQuery{Scopes: []string{scope, other}, TypeName: store.IssueTypeName, Statuses: statuses, Limit: 10})
			return err
		}, func(st r1ConnState) bool { return st.sort == "off" && st.incSort == "off" }, 6},
	} {
		pr.take()
		if err := c.call(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		seen := pr.take()
		if len(seen) == 0 {
			t.Fatalf("%s: no statement on context_blocks was traced", c.name)
		}
		pages := 0
		for _, st := range seen {
			first := strings.Fields(st.sql)
			label := fmt.Sprintf("%s: the statement %q", c.name, strings.Join(first[:min(len(first), 6)], " "))
			if st.pid != seen[0].pid || st.txStart != seen[0].txStart {
				t.Errorf("%s ran on backend %d in the transaction begun %s, the call's first statement on %d in the one begun %s: not in the call's transaction",
					label, st.pid, st.txStart, seen[0].pid, seen[0].txStart)
			}
			if st.txStatus != 'T' {
				t.Errorf("%s ran outside a transaction block (status %q)", label, st.txStatus)
			}
			if st.readOnly != "on" {
				t.Errorf("%s ran in a transaction that is not read-only", label)
			}
			if strings.Contains(st.sql, "ORDER BY") {
				pages++
				if !c.want(st) {
					t.Errorf("%s ran with enable_sort=%s enable_incremental_sort=%s enable_indexscan=%s, not the settings of its form",
						label, st.sort, st.incSort, st.indexScan)
				}
			}
		}
		if pages != c.pages {
			t.Errorf("%s: %d page statements, want %d", c.name, pages, c.pages)
		}
	}
}

// TestRangeReadsR1CreatedListTakesTheBitmapBelowItsCount (review round 1 of
// #536, L1): a status of fewer issues than createdListFewIssues is read
// through its bitmap — index scans off, sorts on, planned with the status on
// every execution — and one of as many through the walk with sorts off. At a
// page of 1 the count is 64 issues, 192 with a label filter.
func TestRangeReadsR1CreatedListTakesTheBitmapBelowItsCount(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "r1-few"
	r1Issues(t, pool, scope, "below", 63)
	r1Issues(t, pool, scope, "at", 64)
	probed, pr := r1Probed(t, pool)
	for _, c := range []struct {
		name, status string
		labels       []string
		bitmap       bool
	}{
		{"63 issues", "below", nil, true},
		{"64 issues", "at", nil, false},
		{"64 issues, a label filter", "at", []string{"a"}, true},
	} {
		pr.take()
		rows, _, err := store.ListIssuesByCreated(ctx, probed, store.IssueReadQuery{Scope: scope, Status: c.status, Labels: c.labels, Limit: 1})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows, want a page of 1", c.name, len(rows))
		}
		var page *r1ConnState
		for _, st := range pr.take() {
			if strings.Contains(st.sql, "ORDER BY b.created_at DESC") {
				st := st
				page = &st
			}
		}
		if page == nil {
			t.Fatalf("%s: the page was not traced", c.name)
		}
		gotBitmap := page.indexScan == "off" && page.sort == "on"
		gotWalk := page.indexScan == "on" && page.sort == "off" && page.incSort == "off"
		switch {
		case c.bitmap && !gotBitmap:
			t.Errorf("%s: the page ran with enable_indexscan=%s enable_sort=%s, want the status's bitmap (index scans off, sorts on)", c.name, page.indexScan, page.sort)
		case !c.bitmap && !gotWalk:
			t.Errorf("%s: the page ran with enable_indexscan=%s enable_sort=%s enable_incremental_sort=%s, want the walk (sorts off)", c.name, page.indexScan, page.sort, page.incSort)
		}
		if c.bitmap && page.mode != pgx.QueryExecModeCacheDescribe {
			t.Errorf("%s: the bitmap's page was sent in query mode %v, want an unnamed statement planned on every execution (QueryExecModeCacheDescribe)", c.name, page.mode)
		}
		if !c.bitmap && page.mode != 0 {
			t.Errorf("%s: the walk's page was sent in query mode %v, want the pool's default", c.name, page.mode)
		}
	}

	// The bitmap's page never runs on the statement the walk's pages leave
	// prepared on a connection: a plan the cache built there under sorts off
	// is a walk, and the cache would serve it under the bitmap's settings.
	cfg := pool.Config()
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	executions := func() int64 {
		var n int64
		if err := one.QueryRow(ctx, `SELECT COALESCE(sum(generic_plans + custom_plans), 0) FROM pg_prepared_statements
			WHERE statement LIKE '%ORDER BY b.created_at DESC%'`, pgx.QueryExecModeSimpleProtocol).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := 0; i < 6; i++ {
		if _, _, err := store.ListIssuesByCreated(ctx, one, store.IssueReadQuery{Scope: scope, Limit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	before := executions()
	if before != 6 {
		t.Fatalf("after six walks on one connection: %d executions of the prepared page, want 6", before)
	}
	if _, _, err := store.ListIssuesByCreated(ctx, one, store.IssueReadQuery{Scope: scope, Status: "below", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if after := executions(); after != before {
		t.Errorf("the bitmap's page ran on the walk's prepared statement (%d executions, %d before)", after, before)
	}
}

// TestRangeReadsR1CreatedListWithoutDescriptionCache (review round 1 of
// #536, L1): the status's bitmap is planned with the status on every
// execution (store.CustomPlanMode) and answers on a pool whose connection
// string turns pgx's description cache off, past the five executions after
// which a named statement turns generic.
func TestRangeReadsR1CreatedListWithoutDescriptionCache(t *testing.T) {
	_, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	cfg, err := pgxpool.ParseConfig(dsn + sep + "description_cache_capacity=0")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r1Issues(t, pool, "r1-dc", "done", 3)
	for i := 0; i < 7; i++ {
		rows, _, err := store.ListIssuesByCreated(ctx, pool, store.IssueReadQuery{Scope: "r1-dc", Status: "done", Limit: 10})
		if err != nil {
			t.Fatalf("created list with the description cache off: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("created list with the description cache off: %d rows, want 3", len(rows))
		}
	}
}

// TestRangeReadsR1IssueSearchModeFollowsTheIndex (review round 1 of #536,
// M2): the search by update is planned with its word on every execution
// while idx_blocks_scope_updated stands, and goes through the pool's
// statement cache, as before #536, while it does not — where the cached
// statement's generic plan, a BitmapAnd of the word and the scope, read 70 to
// 179 buffers for a word rare in a project of 60 000 issues and the custom
// plan read and sorted all of them (3 597; TestRangeReadsR1IssueSearchByUpdate
// with CTX_I536_R1_COST=1). The search by creation is planned on every
// execution either way.
func TestRangeReadsR1IssueSearchModeFollowsTheIndex(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	r1Issues(t, pool, "r1-mode", "open", 3)
	for _, without := range []bool{false, true} {
		if without {
			if _, err := pool.Exec(ctx, `DROP INDEX idx_blocks_scope_updated`); err != nil {
				t.Fatal(err)
			}
		}
		store.ResetListIndexReadingsForTest()
		traced, tr := rrTraced(t, pool)
		for _, sort := range []string{store.SortUpdated, store.SortCreated} {
			tr.take()
			rows, _, err := store.SearchIssues(ctx, traced, store.IssueReadQuery{Scope: "r1-mode", Q: "body", Sort: sort, Limit: 10})
			if err != nil {
				t.Fatalf("search (sort %s, index dropped: %v): %v", sort, without, err)
			}
			if len(rows) != 3 {
				t.Fatalf("search (sort %s, index dropped: %v): %d rows, want 3", sort, without, len(rows))
			}
			st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "plainto_tsquery") })
			want := pgx.QueryExecModeCacheDescribe
			if without && sort == store.SortUpdated {
				want = 0 // the pool's default: pgx's statement cache
			}
			if st.mode != want {
				t.Errorf("search (sort %s, idx_blocks_scope_updated dropped: %v): sent in query mode %v, want %v", sort, without, st.mode, want)
			}
		}
	}
}

// r1Guard is the scope of the guard list's fixture: r1GuardPairs pairs the
// guard flagged, beside r1GuardOther plain blocks of another scope.
const (
	r1Guard      = "r1-guard"
	r1GuardPairs = 100
	r1GuardOther = 20_000
)

// r1GuardSeed writes the pairs through the production write path
// (store.UpsertBlock, store.StoreEmbedding) and lets the guard flag them
// (guard.RunGuardBatch), then the other scope's blocks in bulk.
func r1GuardSeed(ctx context.Context, pool *pgxpool.Pool) error {
	for i := 0; i < r1GuardPairs; i++ {
		for j, vec := range [][]float32{gkVec(2*i, 1.0, 0, 0), gkVec(2*i, 0.96, 2*i+1, 0.28)} {
			title := fmt.Sprintf("r1-guard-%03d-%d", i, j)
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "guard list body of "+title, nil, nil, r1Guard, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				return fmt.Errorf("upsert %s: %w", title, err)
			}
			if err := store.StoreEmbedding(ctx, pool, b.ID, "r1-model", vec); err != nil {
				return fmt.Errorf("embed %s: %w", title, err)
			}
		}
	}
	processed, err := guard.RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), 4*r1GuardPairs+10, guard.Search{EfSearch: 800, ExactMaxCandidates: 400})
	if err != nil {
		return fmt.Errorf("guard batch: %w", err)
	}
	if processed != 2*r1GuardPairs {
		return fmt.Errorf("guard batch: processed %d, want %d", processed, 2*r1GuardPairs)
	}
	for _, st := range []string{
		fmt.Sprintf(`INSERT INTO context_blocks (category, title, content, scope, type_name)
			SELECT 'learnings', 'r1 guard other ' || i, 'other body ' || i, 'r1-guard-other', 'knowledge' FROM generate_series(1, %d) i`, r1GuardOther),
		`VACUUM (ANALYZE) context_blocks`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			return fmt.Errorf("%w\n%s", err, st)
		}
	}
	return nil
}

// TestRangeReadsR1GuardListPagesBeforeItsProbes (review round 1 of #536, L2):
// the guard list chooses its page among the scope's flagged blocks before it
// probes the matched blocks, so a page of 5 among 200 flagged blocks probes
// 5. The bound: the heap pages that hold the scope's flagged blocks (the
// page is chosen among all of them), the index pages of the flagged set, a
// primary-key probe of 4 buffers per row of the page, and the plan's top. A
// statement that probes before it limits — the limit moved out of the inner
// select — probes all 200 (about 3 buffers each) or joins a sequential scan
// of the table's 20 000 other blocks.
func TestRangeReadsR1GuardListPagesBeforeItsProbes(t *testing.T) {
	pool := testdb.SetupSeededTestDB(t, fmt.Sprintf("i536-r1-guard-%d-%d", r1GuardPairs, r1GuardOther), r1GuardSeed)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	const limit = 5
	traced, tr := rrTraced(t, pool)
	tr.take()
	items, err := store.GuardList(ctx, traced, []string{r1Guard}, "", "", nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != limit {
		t.Fatalf("guard list: %d items, want a page of %d", len(items), limit)
	}
	for _, it := range items {
		if it.MatchedID == nil || *it.MatchedID == "" {
			t.Fatalf("guard list: item %s names no matched block, so the page probes nothing", it.ID)
		}
	}
	st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "guard_matched_id") })
	var heapPages int64
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT (ctid::text::point)[0]) FROM context_blocks
		WHERE scope = $1 AND guard_status NOT IN ('active', 'clean')`, r1Guard).Scan(&heapPages); err != nil {
		t.Fatal(err)
	}
	bound := heapPages + 4*limit + 24
	for _, p := range rrPlans {
		c := planners.explain(pool, p.rpc, p.generic, st)
		t.Logf("guard list, a page of %d among %d flagged blocks (%s): %d buffers, bound %d (%d heap pages of the flagged blocks); %s",
			limit, 2*r1GuardPairs, rrPlanName(p.rpc, p.generic), c.buffers, bound, heapPages, c.indexes())
		if c.buffers > bound {
			t.Errorf("guard list, a page of %d among %d flagged blocks (%s): %d buffers, more than %d: it probes more than its page",
				limit, 2*r1GuardPairs, rrPlanName(p.rpc, p.generic), c.buffers, bound)
		}
	}
}

// r1Page is one call's answer: rows and cursor.
type r1Page struct {
	rows   []store.WorkflowBlockRow
	cursor *store.WorkflowCursor
}

func (a r1Page) diff(b r1Page) string {
	if len(a.rows) != len(b.rows) {
		return fmt.Sprintf("%d rows against %d", len(a.rows), len(b.rows))
	}
	for i := range a.rows {
		x, y := a.rows[i], b.rows[i]
		if x.ID != y.ID || x.Scope != y.Scope || x.TypeName != y.TypeName || x.Title != y.Title || x.WorkflowStatus != y.WorkflowStatus || !x.UpdatedAt.Equal(y.UpdatedAt) {
			return fmt.Sprintf("row %d: %+v against %+v", i, x, y)
		}
	}
	if (a.cursor == nil) != (b.cursor == nil) {
		return fmt.Sprintf("cursor %v against %v", a.cursor, b.cursor)
	}
	if a.cursor != nil && (a.cursor.ID != b.cursor.ID || a.cursor.Status != b.cursor.Status ||
		!a.cursor.UpdatedAt.Equal(b.cursor.UpdatedAt) || !a.cursor.CreatedAt.Equal(b.cursor.CreatedAt)) {
		return fmt.Sprintf("cursor %+v against %+v", *a.cursor, *b.cursor)
	}
	return ""
}

// r1BaseRead runs a statement of the base (r1BaseSearchUpdated,
// r1BaseSearchCreated, r1BaseCreatedList) on the pool in its default mode,
// as the base sent it, and computes the base's cursor.
func r1BaseRead(t *testing.T, pool *pgxpool.Pool, sql, sort string, limit int, args ...any) r1Page {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	defer rows.Close()
	var out r1Page
	var last struct {
		created time.Time
		row     store.WorkflowBlockRow
	}
	for rows.Next() {
		var r store.WorkflowBlockRow
		var created time.Time
		if err := rows.Scan(&r.ID, &r.Scope, &r.TypeName, &r.Title, &r.WorkflowStatus, &r.UpdatedAt, &created); err != nil {
			t.Fatal(err)
		}
		out.rows = append(out.rows, r)
		last.created, last.row = created, r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out.rows) == limit {
		out.cursor = &store.WorkflowCursor{Status: last.row.WorkflowStatus, ID: last.row.ID}
		if sort == store.SortCreated {
			out.cursor.CreatedAt = last.created
		} else {
			out.cursor.UpdatedAt = last.row.UpdatedAt
		}
	}
	return out
}

// r1Clamp is the base's clamp of a page size.
func r1Clamp(n int) int {
	if n <= 0 {
		return store.DefaultWorkflowListLimit
	}
	return min(n, store.MaxWorkflowListLimit)
}

// r1Keyset is the base's first-page keyset or the cursor's.
func r1Keyset(c *store.WorkflowCursor, sort string) (pgtype.Timestamptz, string) {
	if c == nil {
		return r1Inf, r1MaxUUID
	}
	if sort == store.SortCreated {
		return pgtype.Timestamptz{Time: c.CreatedAt, Valid: true}, c.ID
	}
	return pgtype.Timestamptz{Time: c.UpdatedAt, Valid: true}, c.ID
}

func r1Labels(l []string) any {
	if len(l) > 0 {
		return l
	}
	return nil
}

// TestRangeReadsR1SameRows (review round 1 of #536): the issue search, with
// and without idx_blocks_scope_updated, and the created list answer the rows
// and the cursor of the base's statements for every parameter set below —
// each followed for up to three pages by its cursor — and the statements
// they send answer the base's rows on every plan (rrSame) for every tenth.
func TestRangeReadsR1SameRows(t *testing.T) {
	ctx := context.Background()
	p := &rrPlanners{t: t}
	scopes := []string{r1Proj, r1Proj, r1Small, r1Other, "r1-none"}
	words := []string{r1Common, r1Zebra, r1Stripes, r1Seldom, r1Common + " " + r1Zebra, "nowhereword"}
	statuses := []string{"", "", "done", "triage", "review", "doing", "blocked", "open", "nope"}
	labels := [][]string{nil, nil, {"rare"}, {"a"}, {"a", "rare"}, {"zz"}}
	limits := []int{0, 1, 2, 7, 50, 100, 1000}
	calls, sets, planned := 0, 0, 0
	forms := map[string]int{} // the forms the calls sent: each must occur
	for _, without := range []bool{false, true} {
		db := r1Store(t, r1Tenth, without)
		same := &rrSame{p: p, db: db}
		traced, tr := rrTraced(t, db)
		r := rand.New(rand.NewSource(536))
		pick := func(n int) int { return r.Intn(n) }
		for i := 0; i < 120; i++ {
			sets++
			q := store.IssueReadQuery{Scope: scopes[pick(len(scopes))], Q: words[pick(len(words))], Status: statuses[pick(len(statuses))],
				Labels: labels[pick(len(labels))], Limit: limits[pick(len(limits))], Sort: []string{store.SortUpdated, store.SortCreated, ""}[pick(3)]}
			baseSQL := r1BaseSearchUpdated
			if q.Sort == store.SortCreated {
				baseSQL = r1BaseSearchCreated
			}
			for page := 0; page < 3; page++ {
				calls++
				tr.take()
				rows, cur, err := store.SearchIssues(ctx, traced, q)
				if err != nil {
					t.Fatalf("search %+v: %v", q, err)
				}
				ts, id := r1Keyset(q.Cursor, q.Sort)
				base := r1BaseRead(t, db, baseSQL, q.Sort, r1Clamp(q.Limit), q.Scope, store.IssueTypeName, q.Status, q.Q, ts, id, r1Labels(q.Labels), r1Clamp(q.Limit))
				if d := (r1Page{rows, cur}).diff(base); d != "" {
					t.Errorf("search %+v (index of 195: %v), page %d: %s", q, !without, page, d)
				}
				st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "plainto_tsquery") })
				forms[fmt.Sprintf("search, index of 195 %v, planned each time %v", !without, st.mode != 0)]++
				if i%10 == 0 && page == 0 {
					planned++
					same.check(fmt.Sprintf("search %+v", q), rrStmt{sql: baseSQL, args: st.args}, st)
				}
				if cur == nil {
					break
				}
				q.Cursor = cur
			}
		}
		if without {
			continue
		}
		for i := 0; i < 120; i++ {
			sets++
			q := store.IssueReadQuery{Scope: scopes[pick(len(scopes))], Status: statuses[pick(len(statuses))],
				Labels: labels[pick(len(labels))], Limit: limits[pick(len(limits))]}
			for page := 0; page < 3; page++ {
				calls++
				tr.take()
				rows, cur, err := store.ListIssuesByCreated(ctx, traced, q)
				if err != nil {
					t.Fatalf("created list %+v: %v", q, err)
				}
				ts, id := r1Keyset(q.Cursor, store.SortCreated)
				base := r1BaseRead(t, db, r1BaseCreatedList, store.SortCreated, r1Clamp(q.Limit), q.Scope, store.IssueTypeName, q.Status, ts, id, r1Labels(q.Labels), r1Clamp(q.Limit))
				if d := (r1Page{rows, cur}).diff(base); d != "" {
					t.Errorf("created list %+v, page %d: %s", q, page, d)
				}
				st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "ORDER BY b.created_at DESC") })
				forms[fmt.Sprintf("created list, the status's bitmap %v", st.local["enable_indexscan"] == "off")]++
				if i%10 == 0 && page == 0 {
					planned++
					same.check(fmt.Sprintf("created list %+v", q), rrStmt{sql: r1BaseCreatedList, args: st.args}, st)
				}
				if cur == nil {
					break
				}
				q.Cursor = cur
			}
		}
	}
	for _, f := range []string{
		"search, index of 195 true, planned each time true", "search, index of 195 false, planned each time false",
		"search, index of 195 false, planned each time true", // the search by creation
		"created list, the status's bitmap true", "created list, the status's bitmap false",
	} {
		if forms[f] == 0 {
			t.Errorf("no call sent the form %q", f)
		}
	}
	t.Logf("%d parameter sets, %d calls compared with the base's statements (forms %v), %d sent statements compared on four plans", sets, calls, forms, planned)
}
