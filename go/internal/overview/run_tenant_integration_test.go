//go:build integration

package overview_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestRebuildOfADeletedTenantWritesNothing (issue #555, review round): a
// rebuild of a tenant's partition that began before the tenant's delete and
// persists after its first step writes nothing — the persist transaction
// holds the run tenant's gate (Options.RunTenant, store.HoldRunTenant) and
// finds the tenant no longer active. Before, the persist of a partition
// without nodes wrote the meta rows of the tenant's scopes, which outlived
// the delete.
func TestRebuildOfADeletedTenantWritesNothing(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, "ovrun", "ovrun")
	if err != nil {
		t.Fatal(err)
	}
	const scope = "ovrun:main"
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
		t.Fatal(err)
	}
	opts := overview.Options{Resolution: 1.0, VisibleTypes: []string{"knowledge"}, OverviewTypes: []string{"knowledge"},
		ScopeFilter: []string{scope}, RunTenant: tn.ID}
	if _, err := overview.Rebuild(ctx, pool, opts); err != nil {
		t.Fatalf("rebuild of the active tenant: %v", err)
	}
	if err := store.PruneTenant(ctx, pool, tn.ID, store.AuditStamp{}); err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	_, err = overview.Rebuild(ctx, pool, opts)
	if !errors.Is(err, store.ErrRunTenantGone) {
		t.Fatalf("a rebuild for the deleted tenant: %v, want ErrRunTenantGone", err)
	}
	if _, err := overview.StartRun(ctx, pool, overview.RunStart{Tenant: tn.ID, ScopeSet: []string{scope}, Resolution: 1.0}); !errors.Is(err, store.ErrRunTenantGone) {
		t.Fatalf("a run journal row for the deleted tenant: %v, want ErrRunTenantGone", err)
	}
	var meta, runs int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM graph_overview_meta WHERE scope = $1)::int,
		(SELECT count(*) FROM graph_overview_run WHERE $1 = ANY(scope_set))::int`, scope).Scan(&meta, &runs); err != nil || meta != 0 || runs != 0 {
		t.Fatalf("%d meta rows and %d run journal rows of the deleted tenant's scope (%v), want 0", meta, runs, err)
	}
}
