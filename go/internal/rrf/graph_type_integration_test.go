//go:build integration

// Issue #384 on both GraphExpand arms and the cluster injection: a block a
// stage brings in carries the type_name of its row, as a fusion hit does
// (ctx_rrf returns it). The hydrates did not read it, and the answer derives
// the foreign-text flag and the aggregate fold from the type.
//
//	go test -tags=integration ./internal/rrf/ -run TestStageHydratesCarryTheType -count=1 -v
package rrf_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStageHydratesCarryTheType_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w057-type"
	snap := w57DiffFixture(t, pool, scope)
	// The neighbours of S1 get a type other than the column default, so an
	// empty or a defaulted name cannot pass.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET type_name = 'reference' WHERE id = ANY($1::uuid[])`,
		[]string{w57N1, w57DH}); err != nil {
		t.Fatalf("retype: %v", err)
	}
	types := []string{"knowledge", "reference"}
	seeds := []rrf.SearchResult{{ID: w57S1, Title: "S1", RRFScore: 1.0, Scope: scope, TypeName: "knowledge"}}
	cfg := w57Cfg()
	cfg.HopDepth = 1

	for _, arm := range []struct {
		name  string
		cache rrf.ExpandCache
	}{{"sql", rrf.ExpandCache{}}, {"cache", rrf.ExpandCache{Snapshot: snap}}} {
		t.Run(arm.name, func(t *testing.T) {
			out, err := rrf.GraphExpandCached(ctx, pool, seeds, []string{scope}, nil, types, rrf.RequestFilter{}, cfg, arm.cache)
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			seen := 0
			for _, r := range out {
				if !r.ViaGraph {
					continue
				}
				seen++
				if r.TypeName != "reference" {
					t.Errorf("neighbour %s: type %q, want %q from its row", r.ID, r.TypeName, "reference")
				}
			}
			if seen == 0 {
				t.Fatalf("premise: no neighbour came in (%v)", w57Render(out))
			}
		})
	}

	t.Run("cluster", func(t *testing.T) {
		for i := 0; i < 12; i++ {
			c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
		}
		c9Sibling(t, pool, c9SiblingID(0), "private", c3ClusterID(0), 10, false)
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET type_name = 'reference' WHERE id = $1::uuid`, c9SiblingID(0)); err != nil {
			t.Fatalf("retype sibling: %v", err)
		}
		got, _, err := rrf.ClusterBoost(ctx, pool, c3Results(12, "private"), nil, []string{"private"}, nil, c9Types(), rrf.RequestFilter{}, c9Cfg(2), c3Now())
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		injected := 0
		for _, r := range got {
			if !r.ViaCluster {
				continue
			}
			injected++
			if r.TypeName != "reference" {
				t.Errorf("injected %s: type %q, want %q from its row", r.ID, r.TypeName, "reference")
			}
		}
		if injected == 0 {
			t.Fatalf("premise: nothing was injected")
		}
	})
}
