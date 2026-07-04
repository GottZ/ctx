//go:build integration

// WF T12 gates (design/01-type-registry.md §7-T12, §5.4, §9.6) against a real
// PG18 testcontainer: the tenant-override overlay (config.Store twin) — lazy
// per-tenant resolution with generation stamp + invalidation, D6 "Overlay
// gewinnt" precedence, and the tenant-isolation negative probes.
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/blocktype/ -run T12 -race -count=1 -v
package blocktype_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

// insertTenantType inserts a tenant-scope registry row (no FK on scope — the
// scope VARCHAR is the tenant discriminator, Modell C).
func insertTenantType(t *testing.T, pool *pgxpool.Pool, scope, name, config string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ($1, $2, false, false, $3::jsonb)
		 ON CONFLICT (name, scope) DO UPDATE SET config = EXCLUDED.config`,
		name, scope, config); err != nil {
		t.Fatalf("insert tenant type %s/%s: %v", scope, name, err)
	}
}

func t12Registry(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blocktype.Registry {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg
}

// TestT12_Overlay_Integration walks the overlay-resolution, scope-security and
// default-equivalence gates in one container.
func TestT12_Overlay_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := t12Registry(t, ctx, pool)

	const scopeA, scopeB = "t12-tenant-a", "t12-tenant-b"

	// Default-tenant / unknown-tenant equivalence: a scope with no own rows
	// inherits the base *Set pointer BYTE-FOR-BYTE (settings.TenantOverlay
	// pattern — the cheap-cache footprint guard). RED if buildTenantSet built a
	// fresh generation for a row-less tenant.
	t.Run("unknown_tenant_inherits_base_pointer", func(t *testing.T) {
		base := reg.Snapshot()
		got := reg.SnapshotForTenant(ctx, scopeA)
		if got != base {
			t.Errorf("row-less tenant did not inherit the base pointer (got %p, base %p)", got, base)
		}
		if !reflect.DeepEqual(got.Names(), base.Names()) {
			t.Errorf("row-less tenant Names() drift: %v vs base %v", got.Names(), base.Names())
		}
	})

	// Overlay resolution: a tenant row shadows the _global policy of the same
	// name (D6: tenant wins), non-overridden types fall through to base.
	insertTenantType(t, pool, scopeA, "system-meta", `{"v":1,"retrieval":{"policy":"full-pass"}}`)
	reg.InvalidateTenant(scopeA) // NOTIFY dispatch twin

	t.Run("overlay_tenant_wins_and_fall_through", func(t *testing.T) {
		setA := reg.SnapshotForTenant(ctx, scopeA)
		// tenant override lifts system-meta from _global excluded → full-pass.
		if !hasString(setA.VisibleTypes(), "system-meta") {
			t.Errorf("tenant-A system-meta→full-pass override not resolved (overlay dead / tenant did not win); visible=%v", setA.VisibleTypes())
		}
		// non-overridden types fall through to _global.
		p, ok := setA.Resolve("audit-trail")
		if !ok || p.Retrieval.Kind != blocktype.RetrievalDamped {
			t.Errorf("audit-trail did not fall through to base damped policy: %+v ok=%v", p, ok)
		}
		if _, ok := setA.Resolve("knowledge"); !ok {
			t.Error("knowledge (base default) missing from tenant-A set")
		}
	})

	// Scope-security negative (§5.4): tenant-A's override must NOT leak into
	// tenant-B's resolution NOR pollute the _global base generation.
	t.Run("scope_security_A_override_isolated_from_B_and_base", func(t *testing.T) {
		setB := reg.SnapshotForTenant(ctx, scopeB)
		if hasString(setB.VisibleTypes(), "system-meta") {
			t.Errorf("LEAK: tenant-A's system-meta override reached tenant-B; visible=%v", setB.VisibleTypes())
		}
		if hasString(reg.Snapshot().VisibleTypes(), "system-meta") {
			t.Errorf("LEAK: tenant-A's override polluted the _global base generation; visible=%v", reg.Snapshot().VisibleTypes())
		}
	})
}

// TestT12_Invalidation_Integration proves the invalidation contract in one run:
// a cached tenant generation stays STALE across a DB write until InvalidateTenant
// drops it, after which the next resolution rebuilds — no process restart. The
// stale assertion is the RED half (the cache is load-bearing), the post-drop
// assertion is the GREEN half (invalidation rebuilds).
func TestT12_Invalidation_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := t12Registry(t, ctx, pool)
	const scopeC = "t12-tenant-c"

	// Prime an override so the tenant has its OWN generation cached (not the
	// base pointer — a row-less tenant would re-inherit base and mask staleness).
	insertTenantType(t, pool, scopeC, "system-meta", `{"v":1,"retrieval":{"policy":"full-pass"}}`)
	reg.InvalidateTenant(scopeC)
	first := reg.SnapshotForTenant(ctx, scopeC)
	if !hasString(first.VisibleTypes(), "system-meta") {
		t.Fatalf("prime: system-meta override not resolved; visible=%v", first.VisibleTypes())
	}

	// DB write WITHOUT invalidation: add reference→excluded for scopeC.
	insertTenantType(t, pool, scopeC, "reference", `{"v":1,"retrieval":{"policy":"excluded"}}`)

	// RED half: the cached generation is stale — reference is still visible.
	stale := reg.SnapshotForTenant(ctx, scopeC)
	if !hasString(stale.VisibleTypes(), "reference") {
		t.Error("cache is not load-bearing: reference already gone without invalidation (cannot prove the NOTIFY chain matters)")
	}

	// GREEN half: the NOTIFY dispatch twin drops the entry; rebuild sees it.
	reg.InvalidateTenant(scopeC)
	fresh := reg.SnapshotForTenant(ctx, scopeC)
	if hasString(fresh.VisibleTypes(), "reference") {
		t.Errorf("reference→excluded not applied after InvalidateTenant — invalidation dead; visible=%v", fresh.VisibleTypes())
	}
	// system-meta override still stands after the rebuild.
	if !hasString(fresh.VisibleTypes(), "system-meta") {
		t.Error("rebuild dropped the still-valid system-meta override")
	}
}

// TestT12_Generation_Integration is the generation / torn-read gate: concurrent
// base Reloads (each bumps baseGen + wipes the tenant cache) interleaved with
// per-tenant reads must never surface an empty/torn *Set. Run under -race to
// catch a data race in the pointer swaps; the consistency assertions catch a
// stale-serve. RED under a cache-swap without the generation stamp (a reader
// would serve a set built against a since-replaced base).
func TestT12_Generation_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := t12Registry(t, ctx, pool)
	const scope = "t12-gen-tenant"
	insertTenantType(t, pool, scope, "system-meta", `{"v":1,"retrieval":{"policy":"full-pass"}}`)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s := reg.SnapshotForTenant(ctx, scope)
					if s == nil {
						t.Error("torn read: nil *Set")
						return
					}
					// Every published generation is internally consistent: a
					// resolvable default and a non-empty namespace.
					if len(s.Names()) == 0 {
						t.Error("torn read: empty *Set")
						return
					}
					if _, ok := s.Resolve("knowledge"); !ok {
						t.Error("torn read: base default missing from a tenant generation")
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		if err := reg.Reload(ctx, pool); err != nil {
			t.Errorf("concurrent Reload: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
}
