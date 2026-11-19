//go:build integration

// Issue #372, part 4: the owner's floor of the query path's embed backfill
// (backfillFloor) was resolved outside embed_backfill.sync_budget. An owner's
// configuration that has to be built (the first request after a settings
// change, or a cold cache) was built on a context of its own of up to 5 s,
// and the request waited for the build whatever its budget said. The wait
// now ends with the budget: the backfill is skipped for that request, and the
// build goes on and is cached, so a later request applies the floor.
//
// On the #253 fixture: the caller's tenant was granted the foreign scope,
// whose owner has a floor row, and the arm runs, so the pick reaches the
// caller's read scopes. The overlay is the production one behind a delay.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryBackfillOwnerFloorWait -count=1 -v
package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
)

func TestQueryBackfillOwnerFloorWait(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	const buildDelay = 2 * time.Second

	f, rec := bfsSetup(t), ownfNewRecorder(t)
	ownfSetFloor(t, f, bfsForeignScope, map[string]string{bfsForeignScope: string(backends.SensInternal)})
	granted := ownfGrantedCaller(t, f)
	f.write(t, f.callerCtx, bfsCallerTitle)

	base := bw2Config()
	cs := config.NewStore(base)
	var builds atomic.Int32
	cs.SetOverlay(func(ctx context.Context, b *config.Config, scope string) (*config.Config, error) {
		builds.Add(1)
		select {
		case <-time.After(buildDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return settings.TenantOverlay(f.pool)(ctx, b, scope)
	})
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "of-embed", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustNoCredentials, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
	})
	h := NewQueryHandler(f.pool, ownfConfigStore{Store: cs, request: base}, bpool, nil, f.reg, snapshotTestAdmitter(t))
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 4, SyncBudget: 300 * time.Millisecond, Interval: 15 * time.Second,
		MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}
	readScopes := AuthResultFromContext(granted).ReadScopes

	start := time.Now()
	got := h.backfillPending(granted, nil, readScopes, h.embedAdmission(), cfg)
	took := time.Since(start)
	if took > time.Second {
		t.Errorf("the backfill step took %v with a sync budget of 300 ms: the request waited for the owner's build (%v)", took, buildDelay)
	}
	if got != 0 || rec.received(bfsCallerTitle) {
		t.Errorf("backfilled %d (caller block sent: %v) before the owner's floor was known, want 0 and nothing sent",
			got, rec.received(bfsCallerTitle))
	}

	// The build went on after the request left it, and its generation is
	// cached: a later request applies the floor without another build.
	deadline := time.Now().Add(buildDelay + 3*time.Second)
	for {
		if _, resolved := cs.SnapshotForTenantResolved(context.Background(), bfsForeignScope); resolved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the owner's configuration was never built: the request's end canceled the build")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.backfillPending(granted, nil, readScopes, h.embedAdmission(), cfg); got != 1 {
		t.Errorf("after the build: backfilled %d, want the caller's block", got)
	}
	if n := builds.Load(); n != 1 {
		t.Errorf("owner builds = %d, want 1", n)
	}
}

// TestQueryBackfillOwnerFloorBuildsOnlyAFloorRow: a configuration without a
// floor row of its own inherits the base's floor, which the base generation
// holds; the request path does not build the owner's generation to read it.
func TestQueryBackfillOwnerFloorBuildsOnlyAFloorRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	f, rec := bfsSetup(t), ownfNewRecorder(t)
	granted := ownfGrantedCaller(t, f)
	f.write(t, f.foreignCtx, bfsForeignTitle)

	base := bw2Config()
	cs := config.NewStore(base)
	var builds atomic.Int32
	cs.SetOverlay(func(ctx context.Context, b *config.Config, scope string) (*config.Config, error) {
		builds.Add(1)
		return settings.TenantOverlay(f.pool)(ctx, b, scope)
	})
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "of-embed", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustNoCredentials, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
	})
	h := NewQueryHandler(f.pool, ownfConfigStore{Store: cs, request: base}, bpool, nil, f.reg, snapshotTestAdmitter(t))
	cfg := &config.Config{EmbedBackfill: config.EmbedBackfillConfig{
		SyncCap: 4, SyncBudget: 10 * time.Second, Interval: 15 * time.Second,
		MaxTokens: 1_000_000, BackoffBase: time.Minute, BackoffCap: time.Hour,
	}}
	if got := h.backfillPending(granted, nil, AuthResultFromContext(granted).ReadScopes, h.embedAdmission(), cfg); got != 1 {
		t.Fatalf("backfilled %d, want the foreign block of the granted scope", got)
	}
	if n := builds.Load(); n != 0 {
		t.Errorf("owner builds = %d for a configuration without a floor row, want 0", n)
	}
}
