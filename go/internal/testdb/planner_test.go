//go:build integration

package testdb

import (
	"context"
	"testing"
)

// TestWithRandomPageCostHoldsOnEveryConnection: every connection of the pool
// plans with the value, a RESET ALL keeps it (it is the session default), and
// the caller's pool keeps its own.
func TestWithRandomPageCostHoldsOnEveryConnection(t *testing.T) {
	pool := SetupTestDB(t)
	ctx := context.Background()
	var own string
	if err := pool.QueryRow(ctx, "SHOW random_page_cost").Scan(&own); err != nil {
		t.Fatal(err)
	}
	for _, v := range RandomPageCosts {
		p := WithRandomPageCost(t, pool, v)
		for i := 0; i < 3; i++ {
			conn, err := p.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			for _, st := range []string{"", "RESET ALL"} {
				if st != "" {
					if _, err := conn.Exec(ctx, st); err != nil {
						t.Fatal(err)
					}
				}
				var got string
				if err := conn.QueryRow(ctx, "SHOW random_page_cost").Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != v {
					t.Errorf("connection %d of the pool with %s, after %q: random_page_cost %s", i, v, st, got)
				}
			}
		}
	}
	var after string
	if err := pool.QueryRow(ctx, "SHOW random_page_cost").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != own {
		t.Errorf("the caller's pool runs with random_page_cost %s, had %s", after, own)
	}
}
