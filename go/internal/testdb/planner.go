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
// connection plans with random_page_cost = value (WithSessionSettings).
func WithRandomPageCost(t testing.TB, pool *pgxpool.Pool, value string) *pgxpool.Pool {
	t.Helper()
	return WithSessionSettings(t, pool, map[string]string{"random_page_cost": value})
}

// WithSessionSettings returns a pool on the database of pool whose every
// connection runs with the given settings. They are startup parameters of
// each connection, so they hold for every statement on it — prepared ones
// and their generic plans included — and survive a RESET (ExplainGeneric
// resets plan_cache_mode only); they win over a PGOPTIONS setting of the test
// binary, and a statement's own SET still wins over them. Each setting is
// read back on the new pool. The pool is closed when the test ends.
func WithSessionSettings(t testing.TB, pool *pgxpool.Pool, settings map[string]string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	cfg := pool.Config() // a copy: the caller's pool keeps its parameters
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	for k, v := range settings {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testdb: pool with %v: %v", settings, err)
	}
	t.Cleanup(p.Close)
	for k, v := range settings {
		var got string
		if err := p.QueryRow(ctx, "SELECT current_setting($1)", k).Scan(&got); err != nil {
			t.Fatalf("testdb: pool with %v: %v", settings, err)
		}
		if got != v {
			t.Fatalf("testdb: pool with %s = %s runs with %s", k, v, got)
		}
	}
	return p
}
