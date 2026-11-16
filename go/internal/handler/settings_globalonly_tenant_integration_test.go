//go:build integration

// A tenant-admin's PUT on a global-only key is refused even when the operator
// already holds a _global row of that key (found while gating mcp.instructions,
// GottZ/ctx#315). The candidate build alone did not refuse it: the _global row
// made the key read as "applied from the DB", so the PUT answered 200 and
// wrote a tenant-scope row the overlay then ignores — and the two opt-in flags
// are read directly at the tenant scope, where that row is the tenant's own
// grant.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsGlobalOnlyTenantWrite -count=1 -v
package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSettingsGlobalOnlyTenantWrite_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(sealbox.EnvKey, freshMasterKey(t))
	t.Setenv(sealbox.EnvKeyPrev, "")
	api := newTenantAPI(t, pool)

	for _, c := range []struct{ key, global, tenant string }{
		{store.AllowSharedSecretsKey, `{"value":false}`, `{"value":true}`},
		{store.AllowCrossTenantBlockGrantKey, `{"value":false}`, `{"value":true}`},
		{"mcp.instructions", `{"value":"operator text"}`, `{"value":"tenant text"}`},
	} {
		t.Run(c.key, func(t *testing.T) {
			if rec := api.as(operatorAR()).do(t, http.MethodPut, "/api/settings/"+c.key, c.global); rec.Code != http.StatusOK {
				t.Fatalf("operator PUT = %d body=%s", rec.Code, rec.Body.String())
			}
			rec := api.as(tenantAdmin("tenantg")).do(t, http.MethodPut, "/api/settings/"+c.key, c.tenant)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("tenant-admin PUT on global-only %s with a _global row = %d body=%s, want 422", c.key, rec.Code, rec.Body.String())
			}
			if v, ok := scopeRowValue(t, pool, c.key, "tenantg"); ok {
				t.Errorf("a tenant-scope row was written: %s", v)
			}
		})
	}
	if ok, err := store.TenantAllowsSharedSecrets(context.Background(), pool, "tenantg"); err != nil || ok {
		t.Errorf("TenantAllowsSharedSecrets(tenantg) = %v, %v — want false: a tenant-admin must not grant itself the _global secret fallback", ok, err)
	}
	if ok, err := store.TenantAllowsCrossTenantBlockGrant(context.Background(), pool, "tenantg"); err != nil || ok {
		t.Errorf("TenantAllowsCrossTenantBlockGrant(tenantg) = %v, %v — want false: a tenant-admin must not grant itself cross-tenant block grants", ok, err)
	}
}
