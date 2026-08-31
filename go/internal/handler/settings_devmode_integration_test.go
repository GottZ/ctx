//go:build integration

// C6-C: the operational recipe for tenant.devmode, executed rather than
// asserted. docs/operations.md tells an operator to flip the flag for ONE
// tenant with a PUT under that tenant's admin key; this probe runs exactly that
// call through the production settings router and checks what the operator is
// promised — 200, the row at the TENANT scope (not _global), the effective
// value flipped for that tenant and unchanged for its neighbour, and DELETE
// restoring the sealing default.
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsTenantDevmode -count=1 -v
package handler

import (
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSettingsTenantDevmodePut_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// A clean env fixture, exactly like TestSettingsTenantAPI_Integration: the
	// registry's only required env value is the DB password.
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")

	api := newTenantAPI(t, pool)

	const key = "/api/settings/tenant.devmode"

	// The default generation seals: nothing is set anywhere yet.
	if api.cfg.Snapshot().Tenant.Devmode {
		t.Fatalf("test premise broken: tenant.devmode must default to false")
	}

	// The documented call: the tenant's OWN admin key, value true.
	rec := api.as(tenantAdmin("devmode-a")).do(t, http.MethodPut, key, `{"value": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant-admin PUT tenant.devmode = %d, want 200 (red if the key were global-only: "+
			"toOverrides would drop the tenant row and the candidate build would 422) body=%s",
			rec.Code, rec.Body.String())
	}

	// The row lands at the TENANT scope. _global must stay untouched — the
	// per-tenant flip may not become a server-wide unseal by accident.
	if v, ok := scopeRowValue(t, pool, "tenant.devmode", "devmode-a"); !ok || v != "true" {
		t.Errorf("tenant.devmode row at scope devmode-a = %q ok=%v, want true", v, ok)
	}
	if v, ok := scopeRowValue(t, pool, "tenant.devmode", store.GlobalScope); ok {
		t.Errorf("tenant.devmode must NOT have written a _global row, got %q", v)
	}

	// Effective value: on for A, still sealing for its neighbour and for the
	// server-global base generation.
	ctxA := t.Context()
	if !api.cfg.SnapshotForTenant(ctxA, "devmode-a").Tenant.Devmode {
		t.Errorf("tenant devmode-a: effective Devmode = false, want true after its own PUT")
	}
	if api.cfg.SnapshotForTenant(ctxA, "devmode-b").Tenant.Devmode {
		t.Errorf("tenant devmode-b: effective Devmode = true, want false (A's flip is not B's)")
	}
	if api.cfg.Snapshot().Tenant.Devmode {
		t.Errorf("_global base generation flipped, want false (a tenant PUT is not an operator decision)")
	}

	// And the write path derived from it agrees — the query pipeline reads the
	// flag through this derivation, not off the struct field.
	if !api.cfg.SnapshotForTenant(ctxA, "devmode-a").SynthesisSettings().Devmode {
		t.Errorf("SynthesisSettings().Devmode = false for devmode-a, want true")
	}

	// DELETE restores the default. Sealing must be one call away, always.
	if rec := api.as(tenantAdmin("devmode-a")).do(t, http.MethodDelete, key, ""); rec.Code != http.StatusOK {
		t.Fatalf("tenant-admin DELETE tenant.devmode = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if api.cfg.SnapshotForTenant(t.Context(), "devmode-a").Tenant.Devmode {
		t.Errorf("tenant devmode-a: Devmode still true after DELETE, want the sealing default back")
	}
}
