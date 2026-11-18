//go:build integration

// In-package test on purpose (as guard_explain_integration_test.go): it runs
// the ACTUAL guardPickSQL and pendingStatsSQL — the guard batch's pick and the
// statement behind manage guard-stats' pending_count and dirty_since since
// migration 181 (issue #341) — and the two statements of the binary before
// migration 182 (issue #360).
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

// pendingStatsBuffersPerWaiting bounds the shared buffers PendingStats' one
// pass may read per block the pick would take, in the whole store (custom and
// generic plan, all scopes or one). Since migration 182 idx_guard_pending is
// (type_name, created_at) with every condition of the pick an index can hold
// (issue #360): the type allowlist is its index condition, so the statement
// reads the entries of the checked types that wait and the heap rows behind
// them. Measured on the production image: 0.059 per waiting block at 100 000
// waiting (issue #341); the bound is twice that.
const pendingStatsBuffersPerWaiting = 0.12

// pendingBuffersPerScopeRow covers the one part of the count that does not
// follow the waiting blocks: a generic plan joins idx_context_scope in a
// BitmapAnd and reads its entries for every row of the caller's scopes
// (measured 0.00084 per row at 1 200 000 rows, issue #360; a plan choice
// before 182 as well). Twice that, per row of the scopes the count is asked
// for.
const pendingBuffersPerScopeRow = 0.002

// pickBuffersPerTaken and pickBuffersPerWaiting bound the pick: it reads the
// waiting entries of the checked types, sorts them by created_at, and fetches
// and locks the rows it returns. Measured on the production image (issue
// #360): 109 buffers for 100 taken of 100 waiting, 1 283 for 100 taken of
// 10 000 waiting; the bounds are about twice that.
const (
	pickBuffersPerTaken   = 2.5
	pickBuffersPerWaiting = 0.25
)

// pendingStatsBufferSlack covers the fixed part: the index metapages and
// root pages, the registry-free plan's catalog reads.
const pendingStatsBufferSlack = 64

// guardPickLimit is the batch limit of the scheduler (guardBatchLimit).
const guardPickLimit = 100

// v560PickSQL and v560PendingStatsSQL are the pick and the count of a binary
// before migration 182 (v5.59.0, v5.60.0), as it sends them: the statements
// that run on the index of 182 during a rolling deploy and after a rollback
// of the binary.
const (
	v560PickSQL = `SELECT id, type_name FROM context_blocks
			WHERE NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND NOT embedding_stale
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'
		  AND type_name = ANY($1::text[])
			ORDER BY created_at ASC
			LIMIT $2
			FOR NO KEY UPDATE SKIP LOCKED`
	v560PendingStatsSQL = `SELECT count(*)::int, min(updated_at) FROM context_blocks
	WHERE scope = ANY($1::text[])
	  AND NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND NOT embedding_stale
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'
		  AND type_name = ANY($2::text[])`
)

// permutedPickSQL is the pick with its conjuncts in reverse order and <>
// for !=: the planner's proof that the statement implies the index predicate
// hangs on neither (issue #360).
const permutedPickSQL = `SELECT id, type_name FROM context_blocks
	WHERE type_name = ANY($1::text[]) AND lifecycle_state = 'knowledge' AND category <> 'index'
	  AND NOT embedding_stale AND embedding IS NOT NULL AND (metadata->>'guard_checked_at') IS NULL AND NOT is_archived
	ORDER BY created_at ASC
	LIMIT $2
	FOR NO KEY UPDATE SKIP LOCKED`

type pendingStatsCostDB struct {
	t     *testing.T
	pool  *pgxpool.Pool
	types []string // the GuardCheckTypes allowlist of the shipped registry
	have  int
	// foreign counts the unchecked blocks with a vector the pick filters out
	// (seedForeign).
	foreign int
}

func newPendingStatsCostDB(t *testing.T) *pendingStatsCostDB {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// The HNSW index is not read by the count; dropping it in this throwaway
	// database keeps the seed of 100 000 vectors in seconds. User triggers
	// off during the seed (guard_explain_integration_test.go: the NOTIFY row
	// trigger makes a large insert quadratic); the full-text trigger stays.
	for _, s := range []string{
		`DROP INDEX IF EXISTS idx_embedding_hnsw`,
		`ALTER TABLE context_blocks DISABLE TRIGGER USER`,
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `ALTER TABLE context_blocks ENABLE TRIGGER USER`) })
	types := reg.Snapshot().GuardCheckTypes()
	if len(types) == 0 || !contains(types, "knowledge") || contains(types, "tool-evidence") {
		t.Fatalf("fixture: the shipped registry checks %v; the test needs knowledge checked and tool-evidence not", types)
	}
	return &pendingStatsCostDB{t: t, pool: pool, types: types}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// seed adds waiting blocks over 100 scopes (pc-0 … pc-99) and, if checked > 0,
// as many checked blocks (not waiting) in the same scopes. Waiting blocks
// alternate between knowledge and reference, two checked types.
func (d *pendingStatsCostDB) seed(waiting, checked int) {
	d.t.Helper()
	ctx := context.Background()
	if waiting > 0 {
		if _, err := d.pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model)
			SELECT 'pc', 'pc-w-' || ($1::int + g), 'waiting ' || g, 'pc-' || (($1::int + g) % 100),
			       CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'reference' END,
			       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q341'
			  FROM generate_series(1, $2::int) g`, d.have, waiting); err != nil {
			d.t.Fatal(err)
		}
		d.have += waiting
	}
	if checked > 0 {
		if _, err := d.pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, metadata)
			SELECT 'pc', 'pc-c-' || g, 'checked ' || g, 'pc-' || (g % 100), 'knowledge',
			       jsonb_build_object('guard_checked_at', '2026-11-18T00:00:00Z', 'guard_status', 'clean')
			  FROM generate_series(1, $1::int) g`, checked); err != nil {
			d.t.Fatal(err)
		}
	}
	if _, err := d.pool.Exec(ctx, `VACUUM ANALYZE context_blocks`); err != nil {
		d.t.Fatal(err)
	}
}

// seedForeign adds n unchecked blocks with a vector that the pick filters
// out, over the same 100 scopes and OLDER than every waiting block (the pick
// orders by created_at, so a walk in that order meets them first), in the
// four kinds the pick's predicate has beyond the index predicate before
// migration 182: 50 % a type with guard.check = false (tool-evidence), 30 %
// a lifecycle other than knowledge (synthesis), 10 % a stale vector, 10 %
// category index (system-meta, a checked type).
func (d *pendingStatsCostDB) seedForeign(n int) {
	d.t.Helper()
	ctx := context.Background()
	if _, err := d.pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model,
			lifecycle_state, embedding_stale, created_at, updated_at)
		SELECT CASE WHEN m = 9 THEN 'index' ELSE 'pc' END,
		       'pc-f-' || g, 'foreign ' || g, 'pc-' || (g % 100),
		       CASE WHEN m < 5 THEN 'tool-evidence' WHEN m = 9 THEN 'system-meta' ELSE 'knowledge' END,
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       CASE WHEN m BETWEEN 5 AND 7 THEN 'synthesis' ELSE 'knowledge' END,
		       m = 8,
		       now() - interval '300 days' + g * interval '1 millisecond',
		       now() - interval '300 days' + g * interval '1 millisecond'
		  FROM (SELECT g, g % 10 AS m FROM generate_series($1::int + 1, $1::int + $2::int) g) s`, d.foreign, n); err != nil {
		d.t.Fatal(err)
	}
	d.foreign += n
	if _, err := d.pool.Exec(ctx, `VACUUM ANALYZE context_blocks`); err != nil {
		d.t.Fatal(err)
	}
}

type pendingStatsRun struct {
	buffers int64
	ms      float64
	n       int
	plan    string
	conds   string // "index: condition" of every index node
}

// explain runs a prepared statement once under EXPLAIN (ANALYZE, BUFFERS) as
// a custom plan or under plan_cache_mode = force_generic_plan, and returns
// the plan's buffers and shape; args are SQL literals.
func (d *pendingStatsCostDB) explain(stmt, ptypes, args string, generic bool) pendingStatsRun {
	d.t.Helper()
	ctx := context.Background()
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	defer conn.Release()
	mode := "force_custom_plan"
	if generic {
		mode = "force_generic_plan"
	}
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
		d.t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET plan_cache_mode; DEALLOCATE q360") }()
	if _, err := conn.Exec(ctx, "PREPARE q360("+ptypes+") AS "+stmt); err != nil {
		d.t.Fatal(err)
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE q360("+args+")").Scan(&raw); err != nil {
		d.t.Fatal(err)
	}
	var out []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		d.t.Fatalf("explain json: %v", err)
	}
	r := pendingStatsRun{ms: out[0].ExecutionTime, plan: planNodes(out[0].Plan), conds: strings.Join(indexConds(out[0].Plan), "; ")}
	r.buffers = int64(out[0].Plan["Shared Hit Blocks"].(float64) + out[0].Plan["Shared Read Blocks"].(float64))
	return r
}

func lit(xs []string) string { return "'{" + strings.Join(xs, ",") + "}'::text[]" }

// runCount explains PendingStats' statement (or another count of the same
// parameters) and reads its result.
func (d *pendingStatsCostDB) runCount(stmt string, scopes []string, generic bool) pendingStatsRun {
	d.t.Helper()
	r := d.explain(stmt, "text[], text[]", lit(scopes)+", "+lit(d.types), generic)
	if err := d.pool.QueryRow(context.Background(), stmt, scopes, d.types).Scan(&r.n, new(*time.Time)); err != nil {
		d.t.Fatal(err)
	}
	return r
}

// runPick explains a pick statement with the batch limit; the result is the
// number of rows the plan returned.
func (d *pendingStatsCostDB) runPick(stmt string, generic bool) pendingStatsRun {
	d.t.Helper()
	r := d.explain(stmt, "text[], int", lit(d.types)+", "+fmt.Sprint(guardPickLimit), generic)
	rows, err := d.pool.Query(context.Background(), stmt, d.types, guardPickLimit)
	if err != nil {
		d.t.Fatal(err)
	}
	for rows.Next() {
		r.n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		d.t.Fatal(err)
	}
	return r
}

func planNodes(pl map[string]any) string {
	s := fmt.Sprint(pl["Node Type"])
	if ix, ok := pl["Index Name"]; ok {
		s += "(" + fmt.Sprint(ix) + ")"
	}
	if kids, ok := pl["Plans"].([]any); ok {
		var ks []string
		for _, k := range kids {
			ks = append(ks, planNodes(k.(map[string]any)))
		}
		s += "[" + strings.Join(ks, ",") + "]"
	}
	return s
}

// indexConds lists "index: Index Cond" for every index node of the plan.
func indexConds(pl map[string]any) []string {
	var out []string
	if ix, ok := pl["Index Name"]; ok {
		out = append(out, fmt.Sprintf("%v: %v", ix, pl["Index Cond"]))
	}
	if kids, ok := pl["Plans"].([]any); ok {
		for _, k := range kids {
			out = append(out, indexConds(k.(map[string]any))...)
		}
	}
	return out
}

func allPendingScopes() []string {
	s := make([]string, 100)
	for i := range s {
		s[i] = fmt.Sprintf("pc-%d", i)
	}
	return s
}

// waitingIn counts the blocks the pick would take within scopes (nil: the
// whole store), by the pick's own predicate, and the rows of those scopes.
func (d *pendingStatsCostDB) waitingIn(scopes []string) (waiting, rows int) {
	d.t.Helper()
	q := `SELECT count(*) FILTER (WHERE ` + guardPendingWhere("$2") + `)::int, count(*)::int
		FROM context_blocks WHERE ($1::text[] IS NULL OR scope = ANY($1::text[]))`
	if err := d.pool.QueryRow(context.Background(), q, scopes, d.types).Scan(&waiting, &rows); err != nil {
		d.t.Fatal(err)
	}
	return waiting, rows
}

// usesTheTypeKey reports whether the plan reads idx_guard_pending with the
// type allowlist as its index condition.
func usesTheTypeKey(r pendingStatsRun) bool {
	for _, c := range strings.Split(r.conds, "; ") {
		if strings.HasPrefix(c, "idx_guard_pending: ") && strings.Contains(c, "type_name") {
			return true
		}
	}
	return false
}

// check runs the count in four shapes (all 100 scopes / one scope, custom /
// generic), for the statement of this binary and the one before migration
// 182, against the bound for the waiting blocks and the rows of the scopes
// asked for.
func (d *pendingStatsCostDB) check(label string, wantAll, wantOne int) {
	d.t.Helper()
	waiting, _ := d.waitingIn(nil)
	for _, c := range []struct {
		name   string
		scopes []string
		want   int
	}{{"all scopes", allPendingScopes(), wantAll}, {"one scope of 100", []string{"pc-7"}, wantOne}} {
		_, rows := d.waitingIn(c.scopes)
		bound := int64(float64(waiting)*pendingStatsBuffersPerWaiting+float64(rows)*pendingBuffersPerScopeRow) + pendingStatsBufferSlack
		for _, st := range []struct{ name, sql string }{{"count", pendingStatsSQL}, {"count of v5.60", v560PendingStatsSQL}} {
			for _, generic := range []bool{false, true} {
				r := d.runCount(st.sql, c.scopes, generic)
				d.t.Logf("%s, %s, %s, generic=%v: n=%d waiting=%d foreign=%d scope rows=%d buffers=%d (bound %d) %.1f ms plan %s [%s]",
					label, st.name, c.name, generic, r.n, waiting, d.foreign, rows, r.buffers, bound, r.ms, r.plan, r.conds)
				if r.n != c.want {
					d.t.Errorf("%s, %s, %s, generic=%v: counted %d, want %d", label, st.name, c.name, generic, r.n, c.want)
				}
				if r.buffers > bound {
					d.t.Errorf("%s, %s, %s, generic=%v: %d buffers for %d waiting blocks beside %d foreign entries — over the bound %d (%.2f per waiting block + %.3f per row of the scopes + %d): the count reads more than the waiting entries",
						label, st.name, c.name, generic, r.buffers, waiting, d.foreign, bound, pendingStatsBuffersPerWaiting, pendingBuffersPerScopeRow, pendingStatsBufferSlack)
				}
				if waiting+d.foreign >= 1000 && strings.Contains(r.plan, "Seq Scan") {
					d.t.Errorf("%s, %s, %s, generic=%v: plan seq-scans: %s", label, st.name, c.name, generic, r.plan)
				}
				if d.foreign >= 10000 && !usesTheTypeKey(r) {
					d.t.Errorf("%s, %s, %s, generic=%v: the plan does not read idx_guard_pending by type: %s [%s]", label, st.name, c.name, generic, r.plan, r.conds)
				}
			}
		}
	}
}

// checkPick runs the pick of this binary and the one before migration 182,
// custom and generic, against the bound for the blocks it takes and the
// blocks that wait.
func (d *pendingStatsCostDB) checkPick(label string, wantTaken int) {
	d.t.Helper()
	waiting, _ := d.waitingIn(nil)
	bound := int64(float64(wantTaken)*pickBuffersPerTaken+float64(waiting)*pickBuffersPerWaiting) + pendingStatsBufferSlack
	for _, st := range []struct{ name, sql string }{{"pick", guardPickSQL}, {"pick of v5.60", v560PickSQL}, {"pick, conjuncts permuted", permutedPickSQL}} {
		for _, generic := range []bool{false, true} {
			r := d.runPick(st.sql, generic)
			d.t.Logf("%s, %s, generic=%v: took=%d waiting=%d foreign=%d buffers=%d (bound %d) %.2f ms plan %s [%s]",
				label, st.name, generic, r.n, waiting, d.foreign, r.buffers, bound, r.ms, r.plan, r.conds)
			if r.n != wantTaken {
				d.t.Errorf("%s, %s, generic=%v: took %d, want %d", label, st.name, generic, r.n, wantTaken)
			}
			if r.buffers > bound {
				d.t.Errorf("%s, %s, generic=%v: %d buffers to take %d of %d waiting blocks beside %d foreign entries — over the bound %d (%.2f per taken + %.2f per waiting + %d): the pick walks entries it never takes",
					label, st.name, generic, r.buffers, r.n, waiting, d.foreign, bound, pickBuffersPerTaken, pickBuffersPerWaiting, pendingStatsBufferSlack)
			}
			if waiting+d.foreign >= 1000 && strings.Contains(r.plan, "Seq Scan") {
				d.t.Errorf("%s, %s, generic=%v: plan seq-scans: %s", label, st.name, generic, r.plan)
			}
			if d.foreign >= 10000 && !usesTheTypeKey(r) {
				d.t.Errorf("%s, %s, generic=%v: the plan does not read idx_guard_pending by type: %s [%s]", label, st.name, generic, r.plan, r.conds)
			}
		}
	}
}

// TestPendingStatsCostFollowsTheWaitingBlocks: Pflichttest 8 of issue #341.
// First a store where 1 % of the blocks wait (1 000 of 100 000, the checked
// ones in the same scopes): the count stays within the bound of the 1 000
// waiting blocks, far below one page per 50 blocks of the corpus. Then
// 10 000 and 100 000 waiting blocks; the pick reads and sorts all of them
// (issue #360: the price of the type key in a backlog).
func TestPendingStatsCostFollowsTheWaitingBlocks(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seed(1000, 99000)
	d.check("1 % of 100 000 waiting", 1000, 10)
	d.checkPick("1 % of 100 000 waiting", guardPickLimit)
	d.seed(9000, 0)
	d.check("10 000 waiting", 10000, 100)
	d.checkPick("10 000 waiting", guardPickLimit)
	d.seed(90000, 0)
	d.check("100 000 waiting", 100000, 1000)
	d.checkPick("100 000 waiting", guardPickLimit)
}

// TestGuardPendingCostBesideForeignEntries (issue #360): the pick and the
// count beside unchecked blocks with a vector that the pick filters out, of
// all four kinds (a type with guard.check = false, another lifecycle, a stale
// vector, category index), older than every waiting block. Before migration
// 182 they were entries of idx_guard_pending and every pick walked them
// (measured: the idle pick 585 buffers beside 10 000, 5 832 beside 100 000;
// the count 5 969 beside 100 000 tool-evidence blocks in a generic plan).
// Since 182 the pick and the count read the waiting blocks only, flat over
// 0 / 10 000 / 100 000 foreign entries, and so do the statements of the
// binary before 182 on this index (rolling deploy). Last the backlog: 10 000
// waiting beside 100 000 foreign.
func TestGuardPendingCostBesideForeignEntries(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.checkPick("nothing waits, nothing foreign", 0)
	for _, n := range []int{10000, 90000} {
		d.seedForeign(n)
		d.checkPick(fmt.Sprintf("nothing waits beside %d foreign", d.foreign), 0)
	}
	d.seed(100, 0)
	d.check(fmt.Sprintf("100 waiting beside %d foreign", d.foreign), 100, 1)
	d.checkPick(fmt.Sprintf("100 waiting beside %d foreign", d.foreign), guardPickLimit)
	d.seed(9900, 0)
	d.check(fmt.Sprintf("backlog of 10 000 waiting beside %d foreign", d.foreign), 10000, 100)
	d.checkPick(fmt.Sprintf("backlog of 10 000 waiting beside %d foreign", d.foreign), guardPickLimit)
}

// TestGuardPendingCostHundredWaitingBesideForeignEntries: the count and the
// pick with 100 waiting blocks alone and beside 10 000 foreign entries
// (issue #360; before migration 182 the pick read 691 buffers there).
func TestGuardPendingCostHundredWaitingBesideForeignEntries(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seed(100, 0)
	d.check("100 waiting alone", 100, 1)
	d.checkPick("100 waiting alone", guardPickLimit)
	d.seedForeign(10000)
	d.check("100 waiting beside 10 000 foreign", 100, 1)
	d.checkPick("100 waiting beside 10 000 foreign", guardPickLimit)
}

// seedCheckedCorpus adds n checked blocks with a vector over the same 100
// scopes, older than every waiting block: the bulk of a grown store, which
// neither the pick nor the count may walk.
func (d *pendingStatsCostDB) seedCheckedCorpus(n int) {
	d.t.Helper()
	ctx := context.Background()
	if _, err := d.pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
		SELECT 'pc', 'pc-k-' || ($1::int + g), 'checked ' || g, 'pc-' || (g % 100), CASE WHEN g % 3 = 0 THEN 'reference' ELSE 'knowledge' END,
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       jsonb_build_object('guard_checked_at', '2026-11-18T00:00:00Z', 'guard_status', 'clean'),
		       now() - interval '200 days' + g * interval '1 millisecond', now()
		  FROM generate_series(1, $2::int) g`, d.have, n); err != nil {
		d.t.Fatal(err)
	}
	d.have += n
	if _, err := d.pool.Exec(ctx, `VACUUM ANALYZE context_blocks`); err != nil {
		d.t.Fatal(err)
	}
}

// TestGuardPendingCostBesideACheckedCorpus (issue #360): 100 waiting blocks,
// the newest of the store, beside 10 000 foreign entries and 200 000 checked
// blocks with a vector. The waiting blocks are always the newest (a block
// waits right after its vector is written), so a walk of the store in
// created_at order (idx_context_created) passes the whole corpus before the
// first of them. The pick and the count must read the waiting entries, not
// the corpus, in custom and generic plans. The corpus is written last, so its
// rows lie behind the waiting ones on the heap although they are older (the
// planner's estimate of a created_at walk depends on that correlation; rows
// that were updated after their insert — a vector, a verdict — move too).
func TestGuardPendingCostBesideACheckedCorpus(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seedForeign(10000)
	d.seed(100, 0)
	d.seedCheckedCorpus(200000)
	d.check("100 waiting beside 10 000 foreign and 200 000 checked", 100, 1)
	d.checkPick("100 waiting beside 10 000 foreign and 200 000 checked", guardPickLimit)
}

// TestPendingStatsTimeAtAMillionWaiting is the one-off measurement at the
// target scale (not a time gate): set CTX_GUARD_PENDING_MEASURE=1.
func TestPendingStatsTimeAtAMillionWaiting(t *testing.T) {
	if os.Getenv("CTX_GUARD_PENDING_MEASURE") == "" {
		t.Skip("measurement; set CTX_GUARD_PENDING_MEASURE=1")
	}
	d := newPendingStatsCostDB(t)
	for d.have < 1_000_000 {
		d.seed(100_000, 0)
	}
	for _, c := range []struct {
		name   string
		scopes []string
	}{{"all scopes", allPendingScopes()}, {"one scope of 100", []string{"pc-7"}}} {
		for _, generic := range []bool{false, true} {
			var ms []float64
			var last pendingStatsRun
			for i := 0; i < 5; i++ {
				last = d.runCount(pendingStatsSQL, c.scopes, generic)
				ms = append(ms, last.ms)
			}
			sort.Float64s(ms)
			t.Logf("1 000 000 waiting, %s, generic=%v: n=%d median %.1f ms (runs %v) buffers=%d plan %s", c.name, generic, last.n, ms[2], ms, last.buffers, last.plan)
		}
	}
}
