//go:build integration

package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RandomPageCosts are the random_page_cost values a plan or cost pin states
// its assertions under. 1.1 is what timescaledb-tune sets in the CI image
// (timescale/timescaledb-ha); 4 is PostgreSQL's default, which the production
// image (db-image/) and a deployment whose configuration sets memory only
// keep (docs/development.md, "Integration tests against the production
// image"). For an equality on scope plus an order that a second index also
// delivers, the value decides which index the planner walks (issues #512,
// #513), so a pin checked at the container's own setting only passes on the
// image it happens to run on.
var RandomPageCosts = []string{"1.1", "4"}

// WithRandomPageCost returns a pool on the database of pool whose every
// connection plans with random_page_cost = value. The value is a startup
// parameter of each connection, so it holds for every statement on it —
// prepared ones and their generic plans included — and survives a RESET
// (ExplainGeneric resets plan_cache_mode only); it wins over a PGOPTIONS
// setting of the test binary, and a statement's own SET still wins over it.
// The pool is closed when the test ends.
func WithRandomPageCost(t testing.TB, pool *pgxpool.Pool, value string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	cfg := pool.Config() // a copy: the caller's pool keeps its parameters
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["random_page_cost"] = value
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testdb: pool with random_page_cost %s: %v", value, err)
	}
	t.Cleanup(p.Close)
	var got string
	if err := p.QueryRow(ctx, "SHOW random_page_cost").Scan(&got); err != nil {
		t.Fatalf("testdb: pool with random_page_cost %s: %v", value, err)
	}
	if got != value {
		t.Fatalf("testdb: pool with random_page_cost %s runs with %s", value, got)
	}
	return p
}
