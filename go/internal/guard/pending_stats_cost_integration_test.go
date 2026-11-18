//go:build integration

// In-package test on purpose (as guard_explain_integration_test.go): it runs
// the ACTUAL pendingStatsSQL, the statement behind manage guard-stats'
// pending_count and dirty_since since migration 181 (issue #341).
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

	"github.com/GottZ/ctx/internal/testdb"
)

// pendingStatsBuffersPerWaiting bounds the shared buffers PendingStats' one
// pass may read per waiting block of the whole store (custom and generic
// plan, all scopes or one): the statement reads idx_guard_pending, whose
// entries are the waiting blocks only, and the heap rows behind them (plus,
// for a scope filter, that scope's entries of idx_context_scope). Measured on
// the production image: 0.057 per waiting block globally, 0.011 for one
// scope of 100 at 100 000 waiting blocks (issue #341); the bound is twice the
// global figure.
const pendingStatsBuffersPerWaiting = 0.12

// pendingStatsBufferSlack covers the fixed part: the index metapages and
// root pages, the registry-free plan's catalog reads.
const pendingStatsBufferSlack = 64

type pendingStatsCostDB struct {
	t     *testing.T
	pool  *pgxpool.Pool
	types []string
	have  int
}

func newPendingStatsCostDB(t *testing.T) *pendingStatsCostDB {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
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
	return &pendingStatsCostDB{t: t, pool: pool, types: []string{"knowledge"}}
}

// seed adds waiting blocks over 100 scopes (pc-0 … pc-99) and, if checked > 0,
// as many checked blocks (not waiting) in the same scopes.
func (d *pendingStatsCostDB) seed(waiting, checked int) {
	d.t.Helper()
	ctx := context.Background()
	if waiting > 0 {
		if _, err := d.pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model)
			SELECT 'pc', 'pc-w-' || ($1::int + g), 'waiting ' || g, 'pc-' || (($1::int + g) % 100), 'knowledge',
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

type pendingStatsRun struct {
	buffers int64
	ms      float64
	n       int
	plan    string
}

// run explains PendingStats' statement once, as a custom plan or under
// plan_cache_mode = force_generic_plan through a prepared statement.
func (d *pendingStatsCostDB) run(scopes []string, generic bool) pendingStatsRun {
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
	defer func() { _, _ = conn.Exec(ctx, "RESET plan_cache_mode; DEALLOCATE q341_pending") }()
	if _, err := conn.Exec(ctx, "PREPARE q341_pending(text[], text[]) AS "+pendingStatsSQL); err != nil {
		d.t.Fatal(err)
	}
	lit := func(xs []string) string { return "'{" + strings.Join(xs, ",") + "}'::text[]" }
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE q341_pending("+lit(scopes)+", "+lit(d.types)+")").Scan(&raw); err != nil {
		d.t.Fatal(err)
	}
	var out []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		d.t.Fatalf("explain json: %v", err)
	}
	r := pendingStatsRun{ms: out[0].ExecutionTime, plan: planNodes(out[0].Plan)}
	r.buffers = int64(out[0].Plan["Shared Hit Blocks"].(float64) + out[0].Plan["Shared Read Blocks"].(float64))
	if err := conn.QueryRow(ctx, "EXECUTE q341_pending("+lit(scopes)+", "+lit(d.types)+")").Scan(&r.n, new(*time.Time)); err != nil {
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

func allPendingScopes() []string {
	s := make([]string, 100)
	for i := range s {
		s[i] = fmt.Sprintf("pc-%d", i)
	}
	return s
}

// check runs the four shapes (all 100 scopes / one scope, custom / generic)
// against the bound for the waiting blocks of the whole store.
func (d *pendingStatsCostDB) check(label string, waitingTotal, wantAll, wantOne int) {
	d.t.Helper()
	bound := int64(float64(waitingTotal)*pendingStatsBuffersPerWaiting) + pendingStatsBufferSlack
	for _, c := range []struct {
		name   string
		scopes []string
		want   int
	}{{"all scopes", allPendingScopes(), wantAll}, {"one scope of 100", []string{"pc-7"}, wantOne}} {
		for _, generic := range []bool{false, true} {
			r := d.run(c.scopes, generic)
			d.t.Logf("%s, %s, generic=%v: n=%d buffers=%d (bound %d) %.1f ms plan %s", label, c.name, generic, r.n, r.buffers, bound, r.ms, r.plan)
			if r.n != c.want {
				d.t.Errorf("%s, %s, generic=%v: counted %d, want %d", label, c.name, generic, r.n, c.want)
			}
			if r.buffers > bound {
				d.t.Errorf("%s, %s, generic=%v: %d buffers for %d waiting blocks — over the bound %d (%.2f per waiting block + %d): the count grows with something else than the waiting blocks",
					label, c.name, generic, r.buffers, waitingTotal, bound, pendingStatsBuffersPerWaiting, pendingStatsBufferSlack)
			}
			if strings.Contains(r.plan, "Seq Scan") {
				d.t.Errorf("%s, %s, generic=%v: plan seq-scans: %s", label, c.name, generic, r.plan)
			}
		}
	}
}

// TestPendingStatsCostFollowsTheWaitingBlocks: Pflichttest 8 of issue #341.
// First a store where 1 % of the blocks wait (1 000 of 100 000, the checked
// ones in the same scopes): the count stays within the bound of the 1 000
// waiting blocks, far below one page per 50 blocks of the corpus. Then
// 10 000 and 100 000 waiting blocks.
func TestPendingStatsCostFollowsTheWaitingBlocks(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seed(1000, 99000)
	d.check("1 % of 100 000 waiting", 1000, 1000, 10)
	d.seed(9000, 0)
	d.check("10 000 waiting", 10000, 10000, 100)
	d.seed(90000, 0)
	d.check("100 000 waiting", 100000, 100000, 1000)
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
				last = d.run(c.scopes, generic)
				ms = append(ms, last.ms)
			}
			sort.Float64s(ms)
			t.Logf("1 000 000 waiting, %s, generic=%v: n=%d median %.1f ms (runs %v) buffers=%d plan %s", c.name, generic, last.n, ms[2], ms, last.buffers, last.plan)
		}
	}
}
