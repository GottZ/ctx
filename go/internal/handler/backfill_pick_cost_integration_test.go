//go:build integration

// Issue #239: the pick of the query path's embed backfill read
// idx_embedding_pending (created_at) and filtered scope and class row by row.
// Every pick walked past each pending block that no embed backend may receive
// and, on the generic plan a driver-cached statement switches to after its
// fifth execution, past the pending blocks of every scope the caller cannot
// read.
//
// The cost pin: every pick statement of a request costs the same number of
// buffers whether 10 000 or 100 000 such rows are pending, on the generic plan and
// on the custom plan. The fixture holds, per size n: one servable block S of the
// caller; n/2 credentials blocks of the caller older than S and n/2 younger
// (the embed pool takes no credentials); n blocks of a caller scope whose
// floor is above every embed backend, younger than S; n servable blocks of a
// scope the caller cannot read, younger than S. Every pick returns S.
//
//	go test -tags=integration ./internal/handler/ -run TestBackfillPickCost -count=1 -v
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// costRecorder is the database under backfillPicker: it runs every pick and
// keeps its statement for the cost reading.
type costRecorder struct {
	pool  *pgxpool.Pool
	stmts []costStmt
}

type costStmt struct {
	sql  string
	args []any
}

func (r *costRecorder) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.stmts = append(r.stmts, costStmt{sql, args})
	return r.pool.QueryRow(ctx, sql, args...)
}

// pickBufferCap bounds the buffers of one pick statement of the fixture: it
// has at most sixteen branches (the caller's two scopes times four
// sensitivities, each for the unembedded and the stale set), each an index
// descent plus a heap page or two, and the gates of the stale branches; a
// pick that walks rows it does not return reads hundreds at the smaller
// size.
const pickBufferCap = 200

const (
	costCaller  = "bpc-caller"
	costLifted  = "bpc-lifted"
	costForeign = "bpc-foreign"
)

// seedPickCost grows the fixture of the file comment from its current size
// to n: rows from..n of each group, at distances of a millisecond from S.
func seedPickCost(t *testing.T, pool *pgxpool.Pool, from, n int) {
	t.Helper()
	ctx := context.Background()
	if from == 0 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity, created_at, updated_at)
			 VALUES ('learnings', 'bpc servable', 'the one block every pick returns', $1, 'public', '2026-01-01', now())`,
			costCaller); err != nil {
			t.Fatalf("seed S: %v", err)
		}
	}
	// Every group comes twice: unembedded, and marked stale (issue #198,
	// part B; without a vector, which puts the stale rows in the unembedded
	// set as well), so the stale branches of every pick have the same rows
	// they cannot return as the unembedded ones.
	for _, g := range []struct {
		scope, sens string
		sign, share int // sign: older (-1) or younger (+1) than S; share: 2 = half of n
		stale       bool
	}{
		{costCaller, "credentials", -1, 2, false},
		{costCaller, "credentials", +1, 2, false},
		{costLifted, "public", +1, 1, false},
		{costForeign, "public", +1, 1, false},
		{costCaller, "credentials", -1, 2, true},
		{costCaller, "credentials", +1, 2, true},
		{costLifted, "public", +1, 1, true},
		{costForeign, "public", +1, 1, true},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity, embedding_stale, created_at, updated_at)
			 SELECT 'learnings', 'bpc ' || $2 || ' ' || $3::int || ' ' || $6::boolean || ' ' || i, 'pending row ' || i, $1, $2, $6,
			        timestamptz '2026-01-01' + $3::int * make_interval(secs => i / 1000.0), now()
			   FROM generate_series($4::int + 1, $5::int) i`,
			g.scope, g.sens, g.sign, from/g.share, n/g.share, g.stale); err != nil {
			t.Fatalf("seed %s/%s: %v", g.scope, g.sens, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// pickCostFixture is the pool and floor of the fixture: a shared embed
// backend that takes no credentials, and the lifted scope floored to
// credentials, so none of its blocks is servable.
func pickCostFixture() (backends.TrustCeilings, config.ScopeFloor, []string) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		{ID: "g", Name: "shared-nocred", Scope: backends.GlobalScope, Roles: []string{backends.RoleEmbed}, Enabled: true, Trust: backends.TrustNoCredentials},
	})
	return p.TrustCeilings(backends.RoleEmbed), config.ScopeFloor{costLifted: backends.SensCredentials}, []string{costCaller, costLifted}
}

// pickCostVariant runs the picks of one request shape and returns the
// generic-plan buffers of every pick statement it issued, in order. Every
// pick must return S.
func pickCostVariant(t *testing.T, pool *pgxpool.Pool, armRuns bool, attempts int) []testdb.PlanCost {
	t.Helper()
	ceilings, floor, readScopes := pickCostFixture()
	rec := &costRecorder{pool: pool}
	p := newBackfillPicker(rec, armRuns, ceilings, floor, readScopes)
	p.backoffBase, p.backoffCap = time.Hour, 24*time.Hour
	for attempt := 0; attempt < attempts; attempt++ {
		row, err := p.next(context.Background(), attempt)
		if err != nil {
			t.Fatalf("pick %d: %v", attempt, err)
		}
		if row.title != "bpc servable" {
			t.Fatalf("pick %d returned %q (%s, %s), want the servable block", attempt, row.title, row.scope, row.sens)
		}
	}
	// Generic and custom plan of each statement, in that order: the first
	// five executions of a driver-cached statement run custom plans, and the
	// plan cache keeps them while they look cheaper than the generic one.
	costs := make([]testdb.PlanCost, 0, 2*len(rec.stmts))
	for i, s := range rec.stmts {
		generic := testdb.ExplainGeneric(t, pool, s.sql, s.args...)
		custom := testdb.ExplainCustom(t, pool, s.sql, s.args...)
		t.Logf("statement %d: generic %v, custom %v", i+1, generic, custom)
		costs = append(costs, generic, custom)
	}
	return costs
}

func TestBackfillPickCost(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	variants := []struct {
		name     string
		armRuns  bool
		attempts int
	}{
		// The caller's newest, the only pick while the arm runs.
		{"arm_on_caller_end", true, 1},
		// Arm off: the old end (the head of the blocks that overran, then the
		// oldest of any scope), then the caller's newest.
		{"arm_off_both_ends", false, 2},
	}
	sizes := []int{10_000, 100_000}
	got := make(map[string][][]testdb.PlanCost)
	from := 0
	for _, n := range sizes {
		seedPickCost(t, pool, from, n)
		from = n
		for _, v := range variants {
			t.Logf("%s at %d rows per group:", v.name, n)
			got[v.name] = append(got[v.name], pickCostVariant(t, pool, v.armRuns, v.attempts))
		}
	}
	for _, v := range variants {
		small, large := got[v.name][0], got[v.name][1]
		if len(small) != len(large) {
			t.Fatalf("%s: %d statements at %d rows, %d at %d", v.name, len(small), sizes[0], len(large), sizes[1])
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
				t.Errorf("%s, statement %d (%s plan): %d buffers at %d rows per group, %d at %d — the pick grows with the rows it cannot return (limit %d)",
					v.name, i/2+1, [2]string{"generic", "custom"}[i%2], small[i].Buffers, sizes[0], large[i].Buffers, sizes[1], limit)
			}
		}
	}
}
