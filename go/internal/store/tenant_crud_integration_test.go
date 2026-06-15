//go:build integration

// Integration test for Multi-Tenant wave T05a (tenant lifecycle CRUD store
// functions: CreateTenant / ListTenants / GetTenant / UpdateTenant). T05a is the
// CRUD-core slice of masterplan wave T05 (Achse 01-T5); the heavier T05 concerns
// are carved out along the design's own seams and deferred:
//   - T05b = tenant-delete = full-prune (FK-ordered batched DELETE, design/01
//     §4.3.1 / §6.3 N11 scaling seam — "NICHT metadata-only", so it is NOT a
//     simple CASCADE delete and is genuinely its own wave).
//   - T05c = E6 engine-turn suspend gate (addendum §6.4, chat/engine.go:208).
// This file covers only the create/list/get/update store layer. The reserved-
// slug + admin gates live at the handler layer (tenant_manage_gate_test.go).
//
// RED (without the store/tenant.go CRUD additions): the package fails to COMPILE
// — store.CreateTenant / ListTenants / GetTenant / UpdateTenant /
// ErrTenantNotFound / ErrTenantSlugExists / store.Tenant are undefined (the
// honest red for a new-symbol wave). GREEN: compiles and all sub-cases pass.
//
// pgCode, defaultTenantID are declared in tenants_hybrid_integration_test.go
// (same store_test package) and reused here.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantCRUD -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantCRUD_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) CreateTenant inserts a tenant with status defaulting to 'active' and a
	// generated UUID; the slug is stored verbatim.
	t.Run("create_defaults_active", func(t *testing.T) {
		tn, err := store.CreateTenant(ctx, pool, "t05-acme", "Acme Corp")
		if err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		if tn.ID == "" || tn.Slug != "t05-acme" || tn.DisplayName != "Acme Corp" {
			t.Fatalf("CreateTenant = %+v, want slug=t05-acme name='Acme Corp' non-empty id", tn)
		}
		if tn.Status != "active" {
			t.Fatalf("new tenant status = %q, want 'active' (default)", tn.Status)
		}
	})

	// (2) A duplicate slug (incl. a second 'default') is rejected with the typed
	// ErrTenantSlugExists (mapped to 409 at the handler), NOT a raw pg error. The
	// default tenant (059) already holds slug 'default'.
	t.Run("duplicate_slug_rejected", func(t *testing.T) {
		if _, err := store.CreateTenant(ctx, pool, "t05-dup", "first"); err != nil {
			t.Fatalf("first create: %v", err)
		}
		_, err := store.CreateTenant(ctx, pool, "t05-dup", "second")
		if !errors.Is(err, store.ErrTenantSlugExists) {
			t.Fatalf("duplicate slug err = %v, want ErrTenantSlugExists", err)
		}
		// A second 'default' is the same 409 path (slug is not '_'-prefixed, so
		// only UNIQUE protects it — masterplan T05 gate).
		_, err = store.CreateTenant(ctx, pool, "default", "shadow default")
		if !errors.Is(err, store.ErrTenantSlugExists) {
			t.Fatalf("second 'default' err = %v, want ErrTenantSlugExists", err)
		}
	})

	// (3) ListTenants returns all tenants incl. the 059 default.
	t.Run("list_includes_default_and_created", func(t *testing.T) {
		if _, err := store.CreateTenant(ctx, pool, "t05-list", "Listed"); err != nil {
			t.Fatalf("create: %v", err)
		}
		tenants, err := store.ListTenants(ctx, pool)
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		seen := map[string]bool{}
		for _, tn := range tenants {
			seen[tn.Slug] = true
		}
		if !seen["default"] || !seen["t05-list"] {
			t.Fatalf("ListTenants slugs missing default/t05-list: %v", seen)
		}
	})

	// (4) GetTenant by id; an unknown id is ErrTenantNotFound (the 404 path).
	t.Run("get_and_not_found", func(t *testing.T) {
		tn, err := store.GetTenant(ctx, pool, defaultTenantID)
		if err != nil {
			t.Fatalf("GetTenant(default): %v", err)
		}
		if tn.Slug != "default" {
			t.Fatalf("GetTenant(default).Slug = %q, want 'default'", tn.Slug)
		}
		_, err = store.GetTenant(ctx, pool, "11111111-2222-3333-4444-555566667777")
		if !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("GetTenant(unknown) err = %v, want ErrTenantNotFound", err)
		}
		// A non-empty MALFORMED id must ALSO be ErrTenantNotFound (404, no
		// existence oracle) — not a 22P02 → 500 that distinguishes "malformed"
		// from "well-formed-but-absent" (adversarial-verify finding).
		_, err = store.GetTenant(ctx, pool, "not-a-uuid")
		if !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("GetTenant(malformed) err = %v, want ErrTenantNotFound (no 22P02 oracle)", err)
		}
	})

	// (5) UpdateTenant changes status (the suspend/reactivate path) and/or
	// display_name with empty-means-skip patch semantics; an unknown id is
	// ErrTenantNotFound. Suspend takes effect at the next auth via the 060
	// ctx_auth status gate (already built) — the per-turn live-session cut is E6
	// (T05c), not this wave.
	t.Run("update_status_and_not_found", func(t *testing.T) {
		tn, err := store.CreateTenant(ctx, pool, "t05-upd", "Updatable")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		suspended, err := store.UpdateTenant(ctx, pool, tn.ID, "suspended", "")
		if err != nil {
			t.Fatalf("UpdateTenant(suspended): %v", err)
		}
		if suspended.Status != "suspended" {
			t.Fatalf("status after update = %q, want 'suspended'", suspended.Status)
		}
		if suspended.DisplayName != "Updatable" {
			t.Fatalf("display_name should be unchanged (empty=skip), got %q", suspended.DisplayName)
		}
		// Reactivate + rename in one call.
		reactivated, err := store.UpdateTenant(ctx, pool, tn.ID, "active", "Renamed")
		if err != nil {
			t.Fatalf("UpdateTenant(active,Renamed): %v", err)
		}
		if reactivated.Status != "active" || reactivated.DisplayName != "Renamed" {
			t.Fatalf("after reactivate+rename = %+v, want active/Renamed", reactivated)
		}
		_, err = store.UpdateTenant(ctx, pool, "11111111-2222-3333-4444-555566667777", "suspended", "")
		if !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("UpdateTenant(unknown) err = %v, want ErrTenantNotFound", err)
		}
	})
}
