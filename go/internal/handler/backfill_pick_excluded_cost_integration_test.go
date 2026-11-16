//go:build integration

// Issue #291, part 1: a block of a type that is never embedded (retrieval
// policy excluded, the shipped checkpoint type) stayed in the pending indexes
// for good, and every pick of its class walked past it row by row: the pick's
// cost grew with the number of such rows.
//
// The cost pin: every pick statement of a request costs the same number of
// buffers whether 10 000 or 100 000 checkpoint rows are pending in the
// caller's own scope at a sensitivity every backend takes, half older and half
// younger than the one servable block S, on the generic plan and on the
// custom plan. Every pick returns S.
//
//	go test -tags=integration ./internal/handler/ -run TestBackfillPickExcludedTypeCost -count=1 -v
package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// seedExcludedPickCost grows the fixture from its current size to n: S once,
// then rows from..n of the two checkpoint groups, at distances of a
// millisecond from S.
func seedExcludedPickCost(t *testing.T, pool *pgxpool.Pool, from, n int) {
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
	for _, sign := range []int{-1, +1} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, created_at, updated_at)
			 SELECT 'hermes-checkpoint', 'bpx checkpoint ' || $2::int || ' ' || i, 'never embedded ' || i, 'checkpoint', $1, 'public',
			        timestamptz '2026-01-01' + $2::int * make_interval(secs => i / 1000.0), now()
			   FROM generate_series($3::int + 1, $4::int) i`,
			costCaller, sign, from/2, n/2); err != nil {
			t.Fatalf("seed checkpoint rows (%+d): %v", sign, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func TestBackfillPickExcludedTypeCost(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	variants := []struct {
		name     string
		armRuns  bool
		attempts int
	}{
		// The caller's newest, the only pick while the arm runs.
		{"arm_on_caller_end", true, 1},
		// Arm off: the old end (unbound, oldest first), then the caller's
		// newest.
		{"arm_off_both_ends", false, 2},
	}
	sizes := []int{10_000, 100_000}
	got := make(map[string][][]testdb.PlanCost)
	from := 0
	for _, n := range sizes {
		seedExcludedPickCost(t, pool, from, n)
		from = n
		for _, v := range variants {
			t.Logf("%s at %d checkpoint rows:", v.name, n)
			got[v.name] = append(got[v.name], pickCostVariant(t, pool, v.armRuns, v.attempts))
		}
	}
	for _, v := range variants {
		small, large := got[v.name][0], got[v.name][1]
		if len(small) != len(large) {
			t.Fatalf("%s: %d statements at %d rows, %d at %d", v.name, len(small), sizes[0], len(large), sizes[1])
		}
		for i := range small {
			for _, c := range []testdb.PlanCost{small[i], large[i]} {
				if c.Buffers > pickBufferCap {
					t.Errorf("%s, statement %d (%s plan): %d buffers, more than %d — the pick reads checkpoint rows it never returns",
						v.name, i/2+1, [2]string{"generic", "custom"}[i%2], c.Buffers, pickBufferCap)
				}
			}
			if limit := small[i].Buffers + small[i].Buffers/2 + 8; large[i].Buffers > limit {
				t.Errorf("%s, statement %d (%s plan): %d buffers at %d checkpoint rows, %d at %d — the pick grows with the rows it never returns (limit %d)",
					v.name, i/2+1, [2]string{"generic", "custom"}[i%2], small[i].Buffers, sizes[0], large[i].Buffers, sizes[1], limit)
			}
		}
	}
}
