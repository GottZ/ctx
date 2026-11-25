//go:build integration

package store_test

// The flat seed of FullGraph after the review of #536's first form: a reader
// of at most 32 scopes reads one arm per scope merged in their order, a
// reader of more scopes or with a category filter the planned form. Cost pins
// on a store of 40 interleaved scopes, and the equality of the rows with the
// statement before #536 over generated parameter sets.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestRangeReadsFullGraphSeed' -count=1 -v
//	CTX_TEST_SEED_SETS=2000 … (the full equality probe; default 240 sets)

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedScopes are the interleaved scopes of the many-scopes store: 1 000
// blocks each, written in turn, so every scope spans the store's whole time.
const (
	seedScopes     = 40
	seedScopeRows  = 1_000
	seedMergeLimit = 32 // store.fullGraphSeedMergeScopes
)

func seedScope(i int) string { return fmt.Sprintf("ms%02d", i) }

func seedScopeList(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = seedScope(i)
	}
	return out
}

// seedManyScopes fills the many-scopes store in bulk (INSERT … SELECT, the
// production triggers run) and a scope of 300 blocks older than all of them.
func seedManyScopes(ctx context.Context, pool *pgxpool.Pool) error {
	for _, st := range []string{
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'ms old ' || i, 'old body ' || i, 'ms-old', 'knowledge',
		        timestamptz '2025-01-01' + make_interval(secs => i), timestamptz '2025-01-01' + make_interval(secs => i)
		   FROM generate_series(1, 300) i`,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'ms ' || i, 'body ' || i, 'ms' || lpad((i % ` + strconv.Itoa(seedScopes) + `)::text, 2, '0'), 'knowledge',
		        timestamptz '2026-01-01' + make_interval(secs => i), timestamptz '2026-01-01' + make_interval(secs => i)
		   FROM generate_series(1, ` + strconv.Itoa(seedScopes*seedScopeRows) + `) i`,
		`VACUUM (ANALYZE) context_blocks`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			return fmt.Errorf("%w\n%s", err, st)
		}
	}
	return nil
}

// TestRangeReadsFullGraphSeedReadsOnePage (review of #536, finding H1): the
// seed reads a page — the rows it answers, a descent per read scope — for a
// reader of many interleaved scopes, on either form. The first form of #536
// read a page per read scope whether the merge kept its rows or not: every
// block of 20 scopes of 1 000 (20 000 rows) for a page of 1 500, 302 998
// buffers for 200 scopes of 5 000 at 1 000 000 rows. At 32 read scopes the
// seed reads the merge form, from 33 on the planned form (the planner walks
// idx_context_created through blocks its scopes hold most of).
func TestRangeReadsFullGraphSeedReadsOnePage(t *testing.T) {
	db := testdb.SetupSeededTestDB(t, fmt.Sprintf("i536-seed-%d-%d", seedScopes, seedScopeRows), seedManyScopes)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	const limit = 1500
	know := []string{"knowledge"}
	for _, c := range []struct {
		scopes  int
		planned bool
	}{{1, false}, {20, false}, {seedMergeLimit, false}, {seedMergeLimit + 1, true}, {seedScopes, true}} {
		name := fmt.Sprintf("a reader of %d of %d interleaved scopes", c.scopes, seedScopes)
		traced, tr := rrTraced(t, db)
		tr.take()
		res, err := store.FullGraph(ctx, traced, store.EgoParams{Limit: limit, EdgeLimit: 4000}, seedScopeList(c.scopes), nil, know)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := limit
		if c.scopes == 1 {
			want = seedScopeRows
		}
		if len(res.Nodes) != want {
			t.Fatalf("%s: %d nodes, want %d", name, len(res.Nodes), want)
		}
		st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "left(") && strings.Contains(sql, "context_blocks") })
		if merge := strings.Contains(st.sql, "UNION ALL"); merge == c.planned {
			t.Errorf("%s: sent the merge form %v, want the merge form %v", name, merge, !c.planned)
		}
		// The page: its rows (a buffer each where the walk of one scope's
		// range meets each row on another heap page), a descent of four
		// buffers per read scope, and a constant for the top of the plan.
		bound := int64(limit+1) + 4*int64(c.scopes) + 32
		for _, p := range rrPlans {
			if p.generic == c.planned {
				continue // the plan the form runs: the merge form's generic, the planned form's custom
			}
			got := planners.explain(db, p.rpc, p.generic, st)
			t.Logf("%s (%s): %d buffers; %s", name, rrPlanName(p.rpc, p.generic), got.buffers, got.indexes())
			if got.buffers > bound {
				t.Errorf("%s (%s): %d buffers, more than %d: it reads more than a page", name, rrPlanName(p.rpc, p.generic), got.buffers, bound)
			}
		}
	}
}

// seedHalves fills a store of two scopes of 20 000 blocks each: half-old
// written before half-new.
func seedHalves(ctx context.Context, pool *pgxpool.Pool) error {
	for _, st := range []string{
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'old half ' || i, 'body ' || i, 'half-old', 'knowledge',
		        timestamptz '2025-01-01' + make_interval(secs => i), timestamptz '2025-01-01' + make_interval(secs => i)
		   FROM generate_series(1, 20000) i`,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'new half ' || i, 'body ' || i, 'half-new', 'knowledge',
		        timestamptz '2026-01-01' + make_interval(secs => i), timestamptz '2026-01-01' + make_interval(secs => i)
		   FROM generate_series(1, 20000) i`,
		`VACUUM (ANALYZE) context_blocks`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			return fmt.Errorf("%w\n%s", err, st)
		}
	}
	return nil
}

// TestRangeReadsFullGraphSeedWalksTheScopeItReads (review of #536): the
// merge form's arm walks its scope's range on idx_context_scope_active, also
// for a scope that holds half of the corpus and is older than the other half,
// at either random_page_cost — observed on the index scans the seed starts
// (pg_stat_user_indexes, flushed by the backend that ran it). A custom plan
// estimates such a scope from its statistics and at random_page_cost 4
// walked idx_context_created through the newer half instead (4 895 buffers
// against 77 at 200 000 rows), which is why the merge form runs its generic
// plan.
func TestRangeReadsFullGraphSeedWalksTheScopeItReads(t *testing.T) {
	db := testdb.SetupSeededTestDB(t, "i536-seed-halves-20000", seedHalves)
	ctx := context.Background()
	for _, rpc := range testdb.RandomPageCosts {
		cfg := db.Config()
		cfg.MaxConns = 1 // one backend: the statements, the flush and the reading
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET random_page_cost = "+rpc)
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		scans := func() (created, scope int64) {
			t.Helper()
			if _, err := pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname = 'idx_context_created'),
				(SELECT idx_scan FROM pg_stat_user_indexes WHERE indexrelname = 'idx_context_scope_active')`).Scan(&created, &scope); err != nil {
				t.Fatal(err)
			}
			return created, scope
		}
		for i := 0; i < 7; i++ { // a connection's statement turns generic after five executions by itself
			c0, s0 := scans()
			res, err := store.FullGraph(ctx, pool, store.EgoParams{Limit: 1500, EdgeLimit: 1}, []string{"half-old"}, nil, []string{"knowledge"})
			if err != nil {
				t.Fatal(err)
			}
			c1, s1 := scans()
			if len(res.Nodes) != 1500 || res.Nodes[0].Title != "old half 20000" {
				t.Fatalf("random_page_cost %s: %d nodes, first %+v", rpc, len(res.Nodes), res.Nodes[0])
			}
			if c1 != c0 || s1 == s0 {
				t.Errorf("random_page_cost %s, execution %d: %d scans of idx_context_created, %d of idx_context_scope_active; want none and at least one",
					rpc, i+1, c1-c0, s1-s0)
			}
		}
		pool.Close()
	}
}

// seedHidden fills a store of a scope of 20 000 blocks of a type the reader
// does not see (checkpoints), interleaved with other scopes' blocks, a few
// visible blocks of it older than them, and a scope of 2 000 visible blocks
// newer than all of it.
func seedHidden(ctx context.Context, pool *pgxpool.Pool) error {
	for _, st := range []string{
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'hid old ' || i, 'body ' || i, 'hid', 'knowledge', timestamptz '2025-06-01' + make_interval(secs => i), timestamptz '2025-06-01'
		   FROM generate_series(1, 200) i`,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'hid ' || i, 'body ' || i, CASE WHEN i % 5 = 0 THEN 'hid' ELSE 'hid-other' END,
		        CASE WHEN i % 5 = 0 THEN 'checkpoint' ELSE 'knowledge' END,
		        timestamptz '2026-01-01' + make_interval(secs => i), timestamptz '2026-01-01'
		   FROM generate_series(1, 100000) i`,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'vis ' || i, 'body ' || i, 'vis', 'knowledge', timestamptz '2027-01-01' + make_interval(secs => i), timestamptz '2027-01-01'
		   FROM generate_series(1, 2000) i`,
		`VACUUM (ANALYZE) context_blocks`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			return fmt.Errorf("%w\n%s", err, st)
		}
	}
	return nil
}

// TestRangeReadsFullGraphSeedSkipsHiddenTypes (review of #536, finding F1 of
// round 1): the merge form checks the type allowlist on what the merge pulls,
// not inside an arm. The MergeAppend reads every arm's head before its first
// row; an arm that filtered the type itself read every hidden block its scope
// held before its first visible one (20 000 checkpoints here, newer than that
// scope's visible blocks), on every call, though another read scope filled
// the page (8 690 buffers against 77 for 40 000 at 200 000 rows).
func TestRangeReadsFullGraphSeedSkipsHiddenTypes(t *testing.T) {
	db := testdb.SetupSeededTestDB(t, "i536-seed-hidden-100000", seedHidden)
	planners := &rrPlanners{t: t}
	ctx := context.Background()
	for _, c := range []struct {
		scopes []string
		limit  int
	}{{[]string{"hid", "vis"}, 1500}, {[]string{"hid", "vis"}, 5}, {[]string{"vis", "hid", "hid-other"}, 201}} {
		name := fmt.Sprintf("readers %v, limit %d", c.scopes, c.limit)
		traced, tr := rrTraced(t, db)
		tr.take()
		res, err := store.FullGraph(ctx, traced, store.EgoParams{Limit: c.limit, EdgeLimit: 1}, c.scopes, nil, []string{"knowledge"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(res.Nodes) != c.limit || !strings.HasPrefix(res.Nodes[0].Title, "vis ") {
			t.Fatalf("%s: %d nodes, first %q", name, len(res.Nodes), res.Nodes[0].Title)
		}
		st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "left(") && strings.Contains(sql, "context_blocks") })
		if !strings.Contains(st.sql, "UNION ALL") {
			t.Fatalf("%s: sent the planned form", name)
		}
		// The page's rows of vis (a few per heap page), a descent of four
		// buffers per arm, and the plan's top.
		bound := int64(c.limit/8) + 4*int64(len(c.scopes)) + 32
		for _, rpc := range testdb.RandomPageCosts {
			got := planners.explain(db, rpc, true, st)
			t.Logf("%s (generic plan at random_page_cost %s): %d buffers; %s", name, rpc, got.buffers, got.indexes())
			if got.buffers > bound {
				t.Errorf("%s (generic plan at random_page_cost %s): %d buffers, more than %d: an arm read the hidden blocks of its scope", name, rpc, got.buffers, bound)
			}
		}
	}
}

// TestRangeReadsFullGraphSeedWithoutItsIndex (review of #536): without
// idx_context_scope_active — a database the schema-contract check reports —
// the seed of a reader of few scopes is read on the planned form. The merge
// form's arms had no sort-free path but a walk of idx_context_created each,
// and read the corpus behind an old read scope: 52 596 buffers against 16 at
// 1 000 000 rows. Here: 300 old blocks behind 20 000 newer ones.
func TestRangeReadsFullGraphSeedWithoutItsIndex(t *testing.T) {
	db := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, st := range []string{
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'ni old ' || i, 'body ' || i, 'ni-old', 'knowledge', timestamptz '2025-01-01' + make_interval(secs => i), timestamptz '2025-01-01'
		   FROM generate_series(1, 300) i`,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'ni new ' || i, 'body ' || i, 'ni-new', 'knowledge', timestamptz '2026-01-01' + make_interval(secs => i), timestamptz '2026-01-01'
		   FROM generate_series(1, 20000) i`,
		`DROP INDEX idx_context_scope_active`,
		`VACUUM (ANALYZE) context_blocks`,
	} {
		if _, err := db.Exec(ctx, st); err != nil {
			t.Fatalf("%v\n%s", err, st)
		}
	}
	traced, tr := rrTraced(t, db)
	planners := &rrPlanners{t: t}
	tr.take()
	res, err := store.FullGraph(ctx, traced, store.EgoParams{Limit: 1500, EdgeLimit: 1}, []string{"ni-old"}, nil, []string{"knowledge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 300 {
		t.Fatalf("%d nodes, want 300", len(res.Nodes))
	}
	st := rrFind(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "left(") && strings.Contains(sql, "context_blocks") })
	if strings.Contains(st.sql, "UNION ALL") || st.mode != pgx.QueryExecModeCacheDescribe {
		t.Fatalf("without idx_context_scope_active the seed sent the merge form (mode %v); want the planned form", st.mode)
	}
	for _, rpc := range testdb.RandomPageCosts {
		got := planners.explain(db, rpc, false, st)
		t.Logf("custom plan at random_page_cost %s: %d buffers; %s", rpc, got.buffers, got.indexes())
		if got.buffers > 64 {
			t.Errorf("custom plan at random_page_cost %s: %d buffers for 300 rows behind 20 000 newer ones", rpc, got.buffers)
		}
	}
}

// seedEnvInt reads a positive integer from the environment, else def.
func seedEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// seedPlanPool is a one-connection pool on dsn planning with random_page_cost
// rpc and the custom or the generic plan for every prepared statement, the
// unnamed ones of store.CustomPlanMode included.
func seedPlanPool(t *testing.T, dsn, rpc string, generic bool) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	mode := "force_custom_plan"
	if generic {
		mode = "force_generic_plan"
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET random_page_cost = "+rpc+"; SET plan_cache_mode = "+mode)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type seedRow struct {
	id, title, cat, scope string
	created               time.Time
}

func (r seedRow) String() string {
	return r.id + "|" + r.title + "|" + r.cat + "|" + r.scope + "|" + r.created.UTC().Format(time.RFC3339Nano)
}

// seedOld runs the seed statement of 5a28c475 (rrOldFullGraphSeed) the way
// FullGraph sent it, and cuts it to the limit with the truncation flag.
func seedOld(ctx context.Context, pool *pgxpool.Pool, p store.EgoParams, scopes, grants, types []string) ([]seedRow, bool, error) {
	if grants == nil {
		grants = []string{}
	}
	var cats []string
	if len(p.Categories) > 0 {
		cats = p.Categories
	}
	rows, err := pool.Query(ctx, rrOldFullGraphSeed, scopes, grants, types, p.Limit+1, cats, p.CreatedAfter, p.CreatedBefore)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []seedRow
	for rows.Next() {
		var r seedRow
		if err := rows.Scan(&r.id, &r.title, &r.cat, &r.scope, &r.created); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > p.Limit {
		return out[:p.Limit], true, nil
	}
	return out, false, nil
}

// TestRangeReadsFullGraphSeedSameRows (review of #536): FullGraph's seed
// answers the rows of the statement before #536 in the same order, with the
// same truncation flag, for generated parameter sets under random_page_cost
// 1.1 and 4, custom and generic plan. The corpus holds groups of up to 100
// blocks of one created_at (random and time-ordered ids), archived blocks,
// four categories and four types over 45 scopes;
// a set reads 1-5 or 30-45 scopes (both forms, the boundary at 32 and 33), a
// scope named twice, scopes without blocks, up to 20 granted blocks (in and
// outside the read scopes, archived, of no block), limits 1-3000, a category
// in a third of the sets, created windows on the instant of a block and a
// microsecond beside it; 20 more sets grant a group of blocks of one
// created_at and cut it with a smaller page.
func TestRangeReadsFullGraphSeedSameRows(t *testing.T) {
	admin, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(int64(seedEnvInt("CTX_TEST_SEED_RANDOM", 536))))
	trows, err := admin.Query(ctx, `SELECT name FROM context_block_types WHERE scope = '_global' ORDER BY name LIMIT 4`)
	if err != nil {
		t.Fatal(err)
	}
	types, err := pgx.CollectRows(trows, pgx.RowTo[string])
	if err != nil || len(types) != 4 {
		t.Fatalf("types %v: %v", types, err)
	}
	var scopes []string
	for i := 0; i < 45; i++ {
		scopes = append(scopes, fmt.Sprintf("g%02d", i))
	}
	cats := []string{"ca", "cb", "cc", "cd"}
	var cat, title, scope, typ []string
	var created []time.Time
	var randomID, archived []bool
	cur := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	const corpus = 4_500
	for len(scope) < corpus {
		group := 1
		if rng.Intn(10) < 3 {
			group = 1 + rng.Intn(100)
		}
		cur = cur.Add(time.Duration(rng.Intn(3)) * time.Second) // 0: the next group ties with this one
		for g := 0; g < group && len(scope) < corpus; g++ {
			cat = append(cat, cats[rng.Intn(len(cats))])
			title = append(title, strings.Repeat("t", rng.Intn(200))+strconv.Itoa(len(scope)))
			if rng.Intn(3) == 0 {
				scope = append(scope, scopes[0]) // one large scope
			} else {
				scope = append(scope, scopes[rng.Intn(len(scopes))])
			}
			typ = append(typ, types[rng.Intn(len(types))])
			created = append(created, cur)
			randomID = append(randomID, rng.Intn(2) == 0)
			archived = append(archived, rng.Intn(10) == 0)
		}
	}
	rows, err := admin.Query(ctx, `INSERT INTO context_blocks (id, category, title, content, scope, type_name, created_at, updated_at, is_archived)
		SELECT CASE WHEN r THEN gen_random_uuid() ELSE uuidv7() END, c, ti, 'x', s, ty, cr, cr, a
		  FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::timestamptz[], $6::bool[], $7::bool[]) AS u(c, ti, s, ty, cr, r, a)
		RETURNING id::text, created_at`, cat, title, scope, typ, created, randomID, archived)
	if err != nil {
		t.Fatal(err)
	}
	type block struct {
		id      string
		created time.Time
	}
	var blocks []block
	for rows.Next() {
		var b block
		if err := rows.Scan(&b.id, &b.created); err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	readable := append(append([]string{}, scopes...), "g-none", "")
	limits := []int{1, 2, 3, 5, 10, 31, 32, 33, 99, 100, 101, 250, 1499, 1500, 3000}
	type set struct {
		p                     store.EgoParams
		scopes, grants, types []string
	}
	nSets := seedEnvInt("CTX_TEST_SEED_SETS", 240)
	sets := make([]set, 0, nSets)
	for len(sets) < nSets {
		var s set
		n := 1 + rng.Intn(5)
		if rng.Intn(4) == 0 {
			n = 30 + rng.Intn(16)
		}
		for _, i := range rng.Perm(len(readable))[:n] {
			s.scopes = append(s.scopes, readable[i])
		}
		if rng.Intn(4) == 0 {
			s.scopes = append(s.scopes, s.scopes[0]) // named twice
		}
		if rng.Intn(4) > 0 {
			for j := rng.Intn(21); j > 0; j-- {
				s.grants = append(s.grants, blocks[rng.Intn(len(blocks))].id)
			}
			if rng.Intn(10) == 0 {
				s.grants = append(s.grants, "00000000-0000-0000-0000-000000000001")
			}
		}
		for _, i := range rng.Perm(len(types))[:1+rng.Intn(len(types))] {
			s.types = append(s.types, types[i])
		}
		s.p.Limit = limits[rng.Intn(len(limits))]
		s.p.EdgeLimit = 4000
		if rng.Intn(3) == 0 {
			s.p.Categories = []string{append(append([]string{}, cats...), "zz")[rng.Intn(len(cats)+1)]}
			if rng.Intn(2) == 0 {
				s.p.Categories = append(s.p.Categories, "cc")
			}
		}
		instant := func() *time.Time {
			b := blocks[rng.Intn(len(blocks))]
			c := b.created.Add(time.Duration(rng.Intn(3)-1) * time.Microsecond)
			return &c
		}
		if rng.Intn(3) == 0 {
			s.p.CreatedAfter = instant()
		}
		if rng.Intn(3) == 0 {
			s.p.CreatedBefore = instant()
		}
		sets = append(sets, s)
	}
	// Granted blocks of one created_at, more of them than the page: the
	// granted arm's order among them is its id tiebreak (the merge and the
	// statement before cut the tie by id).
	ties := map[time.Time][]string{}
	for _, b := range blocks {
		ties[b.created] = append(ties[b.created], b.id)
	}
	tieSets := 0
	for _, ids := range ties {
		if len(ids) < 6 || tieSets == 20 {
			continue
		}
		tieSets++
		sets = append(sets, set{
			p:      store.EgoParams{Limit: 1 + tieSets%4, EdgeLimit: 4000},
			scopes: []string{"g-none"},
			grants: ids,
			types:  types,
		})
	}
	if tieSets == 0 {
		t.Fatal("the corpus holds no group of six blocks of one created_at")
	}
	diffs, checked, longest, truncated, planned := 0, 0, 0, 0, 0
	for _, rpc := range testdb.RandomPageCosts {
		for _, generic := range []bool{false, true} {
			pool := seedPlanPool(t, dsn, rpc, generic)
			for i, s := range sets {
				ref, refTrunc, err := seedOld(ctx, pool, s.p, s.scopes, s.grants, s.types)
				if err != nil {
					t.Fatalf("set %d, old statement: %v", i, err)
				}
				res, err := store.FullGraph(ctx, pool, s.p, s.scopes, s.grants, s.types)
				if err != nil {
					t.Fatalf("set %d: FullGraph: %v", i, err)
				}
				checked++
				got := make([]seedRow, len(res.Nodes))
				for k, n := range res.Nodes {
					got[k] = seedRow{n.ID, n.Title, n.Category, n.Scope, n.CreatedAt}
				}
				longest = max(longest, len(got))
				if refTrunc {
					truncated++
				}
				if len(s.p.Categories) > 0 || len(seedDistinct(s.scopes)) > seedMergeLimit {
					planned++
				}
				same := len(got) == len(ref) && refTrunc == res.Truncated
				for k := 0; same && k < len(got); k++ {
					same = got[k].String() == ref[k].String()
				}
				if !same {
					diffs++
					if diffs <= 10 {
						t.Errorf("set %d (random_page_cost %s, generic %v): scopes %v, %d grants, types %v, limit %d, categories %v, after %v, before %v: %d rows (truncated %v), the old statement %d (truncated %v)",
							i, rpc, generic, s.scopes, len(s.grants), s.types, s.p.Limit, s.p.Categories, s.p.CreatedAfter, s.p.CreatedBefore, len(got), res.Truncated, len(ref), refTrunc)
					}
				}
			}
		}
	}
	t.Logf("FullGraph seed: %d executions (%d sets x 4 plans), %d of the planned form, %d truncated, longest answer %d, %d blocks; %d differences",
		checked, len(sets), planned, truncated, longest, len(blocks), diffs)
	if planned == 0 || planned == checked || truncated == 0 {
		t.Errorf("the sets do not exercise both forms and a truncated page (planned %d of %d, truncated %d)", planned, checked, truncated)
	}
}

// seedDistinct returns xs without repeats.
func seedDistinct(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
