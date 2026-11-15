//go:build integration

// Issue #239, the background arm's side: backfillStep peeks the oldest
// pending block of the tenant's owned scopes on idx_embedding_pending
// (created_at), and when that block has no embed chain it narrows the pick to
// the classes the chain serves and peeks again, filtering scope and class row
// by row. Each pick so walked past every pending block no embed backend may
// receive and, on the generic plan a driver-cached statement switches to
// after its fifth execution, past the pending blocks of every scope the
// tenant does not own.
//
// The cost pin: every pick statement of one backfillStep costs the same
// number of buffers whether 10 000 or 100 000 such rows are pending, on the
// generic plan and on the custom plan. The fixture, oldest first, per size n: n credentials
// blocks of the tenant's scope (the embed pool takes no credentials); n
// servable blocks of a scope no tenant here owns; n blocks of an owned scope
// whose floor is above every embed backend; then the one servable block S of
// the tenant. The owned pass picks S; the legacy unbounded pass (owned ==
// nil) picks the oldest block of the scope nobody owns.
//
//	go test -tags=integration ./internal/events/ -run TestBackfillStepPickCost -count=1 -v
package events

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/testdb"
)

// pickTracer keeps the backfill pick statements a pool runs.
type pickTracer struct {
	mu    sync.Mutex
	stmts []pickStmt
}

type pickStmt struct {
	sql  string
	args []any
}

func (p *pickTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(d.SQL), "SELECT") &&
		strings.Contains(d.SQL, "FROM context_blocks") && strings.Contains(d.SQL, "embedding IS NULL") {
		p.mu.Lock()
		p.stmts = append(p.stmts, pickStmt{d.SQL, d.Args})
		p.mu.Unlock()
	}
	return ctx
}

func (p *pickTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *pickTracer) take() []pickStmt {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stmts
	p.stmts = nil
	return s
}

// pickBufferCap bounds the buffers of one pick statement of the fixture: it
// has at most eight branches (two owned scopes times four sensitivities, or
// four sensitivities unbound), each an index descent plus a heap page or
// two; a pick that walks rows it does not return reads hundreds at the
// smaller size.
const pickBufferCap = 200

const (
	stepCostOwned   = "bsc-owned"
	stepCostLifted  = "bsc-lifted"
	stepCostForeign = "bsc-foreign"
)

// seedStepCost grows the fixture of the file comment from its current size
// to n. Group g's row i stands at 2026-01-01 + g days + i ms, so every
// group is older than the next one and all are older than S.
func seedStepCost(t *testing.T, pool *pgxpool.Pool, from, n int) {
	t.Helper()
	ctx := context.Background()
	if from == 0 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity, created_at, updated_at)
			 VALUES ('learnings', 'bsc servable', 'the block the owned pass picks', $1, 'public', '2026-02-01', now())`,
			stepCostOwned); err != nil {
			t.Fatalf("seed S: %v", err)
		}
	}
	for g, grp := range []struct{ scope, sens string }{
		{stepCostOwned, "credentials"},
		{stepCostForeign, "public"},
		{stepCostLifted, "public"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity, created_at, updated_at)
			 SELECT 'learnings', 'bsc ' || $1 || ' ' || i, 'pending row ' || i, $1, $2,
			        timestamptz '2026-01-01' + make_interval(days => $3::int, secs => i / 1000.0), now()
			   FROM generate_series($4::int + 1, $5::int) i`,
			grp.scope, grp.sens, g, from, n); err != nil {
			t.Fatalf("seed %s: %v", grp.scope, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func TestBackfillStepPickCost(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	tracer := &pickTracer{}
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	pcfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)

	srv := headOfLineEmbedServer(t)
	noCreds := embedPoolRow("embed-no-credentials", srv.URL, 100)
	noCreds.Trust = backends.TrustNoCredentials
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{noCreds})
	cfg := &config.Config{
		EmbedBackfill: config.EmbedBackfillConfig{Interval: time.Hour, BackoffBase: time.Hour, BackoffCap: 24 * time.Hour},
	}
	cfg.Pool.ScopeSensitivityFloor = config.ScopeFloor{stepCostLifted: backends.SensCredentials}
	s := NewScheduler(traced, config.NewStore(cfg), bpool, StartupConfig{DreamEnabled: false})
	s.SetDispatcher(d)

	passes := []struct {
		name, tenant, want string
		owned              []string
	}{
		{"owned", stepCostOwned, "bsc servable", []string{stepCostOwned, stepCostLifted}},
		{"unbounded", "_global", "bsc " + stepCostForeign + " 1", nil},
	}
	sizes := []int{10_000, 100_000}
	got := make(map[string][][]testdb.PlanCost)
	from := 0
	for _, n := range sizes {
		seedStepCost(t, pool, from, n)
		from = n
		for _, p := range passes {
			tracer.take()
			ok, err := s.backfillOneEmbedding(ctx, s.newRouter(cfg, p.tenant), cfg, p.owned)
			if err != nil || !ok {
				t.Fatalf("%s at %d: backfilled=%v err=%v, want true/nil", p.name, n, ok, err)
			}
			stmts := tracer.take()
			var title string
			if err := pool.QueryRow(ctx, `SELECT title FROM context_blocks WHERE embedding IS NOT NULL`).Scan(&title); err != nil {
				t.Fatalf("%s at %d: the embedded block: %v", p.name, n, err)
			}
			if title != p.want {
				t.Fatalf("%s at %d: embedded %q, want %q", p.name, n, title, p.want)
			}
			// Back to pending, so every statement is read against the queue
			// it picked from.
			if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = NULL, embed_model = NULL WHERE embedding IS NOT NULL`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			t.Logf("%s pass at %d rows per group:", p.name, n)
			// Generic and custom plan of each statement, in that order: the
			// first five executions of a driver-cached statement run custom
			// plans, and the plan cache keeps them while they look cheaper.
			costs := make([]testdb.PlanCost, 0, 2*len(stmts))
			for i, st := range stmts {
				generic := testdb.ExplainGeneric(t, pool, st.sql, st.args...)
				custom := testdb.ExplainCustom(t, pool, st.sql, st.args...)
				t.Logf("statement %d: generic %v, custom %v", i+1, generic, custom)
				costs = append(costs, generic, custom)
			}
			got[p.name] = append(got[p.name], costs)
		}
	}
	for _, p := range passes {
		small, large := got[p.name][0], got[p.name][1]
		if len(small) != len(large) {
			t.Fatalf("%s: %d statements at %d rows, %d at %d", p.name, len(small), sizes[0], len(large), sizes[1])
		}
		for i := range small {
			// And small: one index descent and a heap page or two per branch.
			for _, c := range []testdb.PlanCost{small[i], large[i]} {
				if c.Buffers > pickBufferCap {
					t.Errorf("statement %d (%s plan): %d buffers, more than %d — the pick reads rows it does not return",
						i/2+1, [2]string{"generic", "custom"}[i%2], c.Buffers, pickBufferCap)
				}
			}
			// Constant: ten times the rows may deepen an index by a level,
			// which adds a buffer per descent; anything that walks the rows
			// adds thousands.
			if limit := small[i].Buffers + small[i].Buffers/2 + 8; large[i].Buffers > limit {
				t.Errorf("%s pass, statement %d (%s plan): %d buffers at %d rows per group, %d at %d — the pick grows with the rows it cannot return (limit %d)",
					p.name, i/2+1, [2]string{"generic", "custom"}[i%2], small[i].Buffers, sizes[0], large[i].Buffers, sizes[1], limit)
			}
		}
	}
}
