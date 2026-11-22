//go:build integration

// Issue #491 point 6, the load side: a type row whose forge_state_map
// contradicts its terminal set (closed onto a status that is not terminal,
// open onto a terminal one) can be stored today — the write gate that
// refuses it is new — and a row written past the API is stored either way.
// Loading such a row must not fail the registry, neither the '_global'
// reload (a failed reload falls back to the builtin set and degrades
// /health) nor a tenant's set (a failed build serves the base set instead of
// the tenant's own types); it loads, and the registry says what is wrong
// with it.
//
//	go test -tags=integration ./internal/blocktype/ -run TestForgeStateMapLoads -count=1 -v
package blocktype_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

const i491BadIssueType = `{"v":1,"overview":{"include":false},"digest":{"include":false},
	"workflow":{"states":["triage","review","done"],"initial":"triage","terminal":["done"],
	            "forge_state_map":{"open":"triage","closed":"review"}}}`

func TestForgeStateMapLoadsWithAWarning(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("tenant row", func(t *testing.T) {
		reg := t12Registry(t, ctx, pool)
		const scope = "i491-tenant"
		insertTenantType(t, pool, scope, "issue", i491BadIssueType)
		capture := installCapture(t)
		reg.InvalidateTenant(scope)
		set := reg.SnapshotForTenant(ctx, scope)
		p, ok := set.Resolve("issue")
		if !ok || p.Scope != scope || p.Workflow.ForgeStateMap["closed"] != "review" {
			t.Fatalf("the tenant's issue type did not load (resolved %v, scope %q): the tenant would be served the base set", ok, p.Scope)
		}
		if !capture.has(slog.LevelWarn, "forge_state_map", "") {
			t.Errorf("no warning names the forge_state_map that contradicts the terminal set")
		}
	})

	t.Run("_global row", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types SET config = jsonb_set(config, '{workflow,forge_state_map,closed}', '"in-progress"')
			  WHERE name = 'issue' AND scope = '_global'`); err != nil {
			t.Fatalf("plant the _global row: %v", err)
		}
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		reg.Boot(ctx, pool)
		if reg.Health() != blocktype.HealthOK {
			t.Fatalf("registry boot degraded (%s) on a forge_state_map that contradicts the terminal set", reg.Health())
		}
		if p, ok := reg.Snapshot().Resolve("issue"); !ok || p.Workflow.ForgeStateMap["closed"] != "in-progress" {
			t.Fatalf("the stored _global issue row did not load: %+v", p.Workflow.ForgeStateMap)
		}
		if !capture.has(slog.LevelWarn, "forge_state_map", "") {
			t.Errorf("no warning names the forge_state_map that contradicts the terminal set")
		}
	})
}
