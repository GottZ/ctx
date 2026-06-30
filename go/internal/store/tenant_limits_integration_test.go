//go:build integration

// Integration test for wave BEQ-1a (migration 069 "tenant_limits"), STORE part.
//
// 069 adds the typed structural per-tenant caps max_scopes/max_keys (INTEGER,
// CHECK >= 0, NULL = unlimited) to context_tenants, with a column DEFAULT of
// 25/50 (a concrete fail-closed cap for every new and existing tenant) and a
// one-off seed that sets the system/default tenant (00000000-...-0000000d3fa0)
// to NULL = unlimited. This file exercises ONLY the store accessors that read
// and write those columns:
//
//	store.TenantLimits(tenantID)            → (maxScopes, maxKeys *int)
//	store.SetTenantLimits(tenantID, ms, mk) → error
//	store.GetTenant(id).MaxScopes/.MaxKeys  (echoed via shared tenantCols/scanTenant)
//
// The handler (handleTenantLimitSet, actionTier, create-seeding, the >= 0 /
// both-required validation) is a LATER wave (BEQ-1b) and is NOT tested here.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantLimits -count=1 -v
package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// intp is a tiny *int constructor for the limit literals under test.
func intp(i int) *int { return &i }

// fmtLimit renders a *int limit, with nil as the "unlimited" sentinel.
func fmtLimit(p *int) string {
	if p == nil {
		return "nil (unlimited)"
	}
	return strconv.Itoa(*p)
}

// wantLimit asserts a *int limit equals an expected value, treating nil as
// "unlimited". label disambiguates which of the two caps failed.
func wantLimit(t *testing.T, label string, got, want *int) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Fatalf("%s = %s, want %s", label, fmtLimit(got), fmtLimit(want))
	case *got != *want:
		t.Fatalf("%s = %s, want %s", label, fmtLimit(got), fmtLimit(want))
	}
}

// insertProbeTenant creates a fresh tenant via a raw INSERT (no seeding), so it
// inherits the 069 column DEFAULT (max_scopes=25, max_keys=50). Returns its id.
func insertProbeTenant(t *testing.T, pool *pgxpool.Pool, ctx context.Context, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name)
		 VALUES ($1, 'beq1a-probe') RETURNING id::text`, slug).Scan(&id); err != nil {
		t.Fatalf("insert probe tenant slug=%q: %v", slug, err)
	}
	return id
}

func TestTenantLimits_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// The system/default tenant is seeded NULL/NULL by 069 step 2 (explicit
	// unlimited). It carries the legacy private/work/shared scopes and is the one
	// tenant that must never be capped.
	t.Run("default_tenant_unlimited", func(t *testing.T) {
		ms, mk, err := store.TenantLimits(ctx, pool, defaultTenantID)
		if err != nil {
			t.Fatalf("TenantLimits(default): %v", err)
		}
		wantLimit(t, "default max_scopes", ms, nil)
		wantLimit(t, "default max_keys", mk, nil)
	})

	// A freshly INSERTed tenant inherits the 069 column DEFAULT (25/50) — no
	// seeding step needed; the fail-closed cap applies to every new tenant.
	t.Run("fresh_tenant_default_cap", func(t *testing.T) {
		id := insertProbeTenant(t, pool, ctx, "beq1a-default-cap")
		ms, mk, err := store.TenantLimits(ctx, pool, id)
		if err != nil {
			t.Fatalf("TenantLimits(fresh): %v", err)
		}
		wantLimit(t, "fresh max_scopes", ms, intp(25))
		wantLimit(t, "fresh max_keys", mk, intp(50))
	})

	// SetTenantLimits writes concrete caps; TenantLimits reads them back.
	t.Run("set_then_read", func(t *testing.T) {
		id := insertProbeTenant(t, pool, ctx, "beq1a-set-read")
		if err := store.SetTenantLimits(ctx, pool, id, intp(10), intp(20)); err != nil {
			t.Fatalf("SetTenantLimits(10,20): %v", err)
		}
		ms, mk, err := store.TenantLimits(ctx, pool, id)
		if err != nil {
			t.Fatalf("TenantLimits after set: %v", err)
		}
		wantLimit(t, "set max_scopes", ms, intp(10))
		wantLimit(t, "set max_keys", mk, intp(20))

		// nil/nil clears both columns to SQL NULL = unlimited.
		if err := store.SetTenantLimits(ctx, pool, id, nil, nil); err != nil {
			t.Fatalf("SetTenantLimits(nil,nil): %v", err)
		}
		ms, mk, err = store.TenantLimits(ctx, pool, id)
		if err != nil {
			t.Fatalf("TenantLimits after nil-set: %v", err)
		}
		wantLimit(t, "cleared max_scopes", ms, nil)
		wantLimit(t, "cleared max_keys", mk, nil)
	})

	// GetTenant echoes the limits in the Tenant struct (shared tenantCols/scanTenant).
	t.Run("get_tenant_echoes_limits", func(t *testing.T) {
		id := insertProbeTenant(t, pool, ctx, "beq1a-get-echo")
		if err := store.SetTenantLimits(ctx, pool, id, intp(7), intp(13)); err != nil {
			t.Fatalf("SetTenantLimits(7,13): %v", err)
		}
		tn, err := store.GetTenant(ctx, pool, id)
		if err != nil {
			t.Fatalf("GetTenant: %v", err)
		}
		wantLimit(t, "GetTenant max_scopes", tn.MaxScopes, intp(7))
		wantLimit(t, "GetTenant max_keys", tn.MaxKeys, intp(13))

		// The default tenant echoes nil/nil through the same path.
		dt, err := store.GetTenant(ctx, pool, defaultTenantID)
		if err != nil {
			t.Fatalf("GetTenant(default): %v", err)
		}
		wantLimit(t, "GetTenant default max_scopes", dt.MaxScopes, nil)
		wantLimit(t, "GetTenant default max_keys", dt.MaxKeys, nil)
	})

	// Unknown / malformed id → ErrTenantNotFound on both accessors (404, no oracle).
	t.Run("unknown_tenant_not_found", func(t *testing.T) {
		const absentUUID = "00000000-0000-0000-0000-0000000beef0"
		const malformed = "not-a-uuid"

		if _, _, err := store.TenantLimits(ctx, pool, absentUUID); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("TenantLimits(absent): err=%v, want ErrTenantNotFound", err)
		}
		if _, _, err := store.TenantLimits(ctx, pool, malformed); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("TenantLimits(malformed): err=%v, want ErrTenantNotFound (22P02 mapped)", err)
		}
		if _, _, err := store.TenantLimits(ctx, pool, ""); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("TenantLimits(empty): err=%v, want ErrTenantNotFound", err)
		}

		if err := store.SetTenantLimits(ctx, pool, absentUUID, intp(1), intp(2)); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("SetTenantLimits(absent): err=%v, want ErrTenantNotFound (0 rows affected)", err)
		}
		if err := store.SetTenantLimits(ctx, pool, malformed, intp(1), intp(2)); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("SetTenantLimits(malformed): err=%v, want ErrTenantNotFound (22P02 mapped)", err)
		}
		if err := store.SetTenantLimits(ctx, pool, "", intp(1), intp(2)); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("SetTenantLimits(empty): err=%v, want ErrTenantNotFound", err)
		}
	})
}
