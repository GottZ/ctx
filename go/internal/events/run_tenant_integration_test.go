//go:build integration

package events

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestDigestRunCarriesItsTenant (issue #555, review round): the background
// tenant carries its id, the digest run's context carries it to the run's
// writes, and a write of the run after the tenant's delete began writes
// nothing (store.ErrRunTenantGone) — a digest run that read the tenant before
// its delete and writes its map after it leaves no block.
func TestDigestRunCarriesItsTenant(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, "dgrun", "dgrun")
	if err != nil {
		t.Fatal(err)
	}
	const scope = "dgrun:main"
	if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
		t.Fatal(err)
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	cfg := captureTestConfig(t, 12)
	cfg.Scheduler.ReadScopes = []string{scope}
	s := NewScheduler(pool, config.NewStore(cfg), backends.NewPool(nil, nil), StartupConfig{})
	s.SetBlocktypeRegistry(reg)
	bt, ok := s.backgroundTenantFor(ctx, *tn)
	if !ok || bt.id != tn.ID {
		t.Fatalf("background tenant of %s: %+v %v, want its id", tn.ID, bt, ok)
	}
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return []backgroundTenant{bt} }
	var runCtx context.Context
	s.runDigestFn = func(ctx context.Context, _ *pgxpool.Pool, _ *blocktype.Registry, _, _, _, _ string, _ []string) (digest.Result, error) {
		runCtx = ctx
		return digest.Result{}, nil
	}
	s.runDigest(ctx)
	if runCtx == nil {
		t.Fatal("the digest did not run")
	}
	if _, err := store.UpsertBlock(runCtx, pool, "index", "dgrun map", "the run's map", nil, nil, scope, true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatalf("the run's write while the tenant is active: %v", err)
	}
	if err := store.PruneTenant(ctx, pool, tn.ID, store.AuditStamp{}); err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	if _, err := store.UpsertBlock(runCtx, pool, "index", "dgrun map", "the run's map, after the delete", nil, nil, scope, true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrRunTenantGone) {
		t.Fatalf("the run's write after the delete: %v, want ErrRunTenantGone", err)
	}
}
