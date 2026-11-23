//go:build integration

package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DeploymentRandomPageCost is the random_page_cost a deployment plans with:
// PostgreSQL's default, which the production image (db-image/) keeps and the
// repository's docker-compose.yml does not change. The test container starts
// with it on every image (runPostgres), so a test that sets no value of its
// own gets a deployment's plan — on the CI image too, whose timescaledb-tune
// would set 1.1 (issue #514: two cost pins and a plan pin passed at 1.1 in
// every pull request and failed at 4).
const DeploymentRandomPageCost = "4"

// RandomPageCosts are the random_page_cost values a plan or cost pin states
// its assertions under: 1.1, what timescaledb-tune sets (the CI image's own
// value, and what an operator tuning for SSD storage sets), and
// DeploymentRandomPageCost (docs/development.md, "Integration tests against
// the production image"). For an equality on scope plus an order that a
// second index also delivers, the value decides which index the planner walks
// (issues #512, #513), so a pin whose plan depends on it checks both.
var RandomPageCosts = []string{"1.1", DeploymentRandomPageCost}

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
