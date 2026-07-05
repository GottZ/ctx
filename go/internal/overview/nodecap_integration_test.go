//go:build integration

package overview_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

func memberCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph_cluster_member`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRebuild_NodeCapSkipsFailSafe is the B-W1 gate for the load-bearing
// liveness guard: a node set above MaxNodes skips the rebuild BEFORE any
// teardown, leaving the previous tables untouched (fail-safe). Red probe:
// the identical corpus with MaxNodes=0 (uncapped) rebuilds normally — so the
// skip is attributable to the cap, not to fixture noise.
func TestRebuild_NodeCapSkipsFailSafe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-0000000000a1"
		B = "019d0000-0000-7000-9000-0000000000a2"
		C = "019d0000-0000-7000-9000-0000000000a3"
	)
	insBlock(t, pool, A, "private", "learnings", "A")
	insBlock(t, pool, B, "private", "learnings", "B")
	insBlock(t, pool, C, "private", "learnings", "C")
	insLink(t, pool, A, B, 0.9)
	insLink(t, pool, B, C, 0.9)

	types := []string{"knowledge"}

	// Red probe first (uncapped run populates the tables).
	stats, err := overview.Rebuild(ctx, pool, overview.Options{
		Resolution: 1.0, VisibleTypes: types, OverviewTypes: types, MaxNodes: 0,
	})
	if err != nil {
		t.Fatalf("uncapped rebuild: %v", err)
	}
	if stats.Skipped {
		t.Fatalf("uncapped rebuild skipped (%s) — red probe broken", stats.SkipReason)
	}
	if got := memberCount(t, pool); got != 3 {
		t.Fatalf("uncapped rebuild wrote %d members, want 3", got)
	}

	// Capped run: 3 nodes > cap of 2 ⇒ skip, previous tables intact.
	stats, err = overview.Rebuild(ctx, pool, overview.Options{
		Resolution: 1.0, VisibleTypes: types, OverviewTypes: types, MaxNodes: 2,
	})
	if err != nil {
		t.Fatalf("capped rebuild returned error, want fail-safe skip: %v", err)
	}
	if !stats.Skipped || stats.SkipReason != "node-cap" {
		t.Fatalf("capped rebuild: Skipped=%v reason=%q, want skip with node-cap", stats.Skipped, stats.SkipReason)
	}
	if got := memberCount(t, pool); got != 3 {
		t.Fatalf("capped skip mutated the tables: %d members, want previous 3 (fail-safe broken)", got)
	}
}
