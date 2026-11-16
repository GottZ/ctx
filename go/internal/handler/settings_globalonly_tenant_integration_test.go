//go:build integration

// A tenant-admin's PUT on a global-only key is refused even when the operator
// already holds a _global row of that key. The candidate build alone did not
// refuse it: the _global row made the key read as "applied from the DB", so
// the PUT answered 200 and wrote a tenant-scope row. The overlay ignores such
// a row, but tenant.allow_shared_secrets is read directly at the tenant scope
// (store.TenantAllowsSharedSecrets), where that row decides for the tenant.
// tenant.allow_cross_tenant_block_grant is read at the tenant id, not the
// scope, and graph_cache.enabled only through the overlay: for both the row
// was dead, and both must be refused all the same.
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
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password") // the env base requires one
	t.Setenv(sealbox.EnvKey, freshMasterKey(t))
	t.Setenv(sealbox.EnvKeyPrev, "")
	api := newTenantAPI(t, pool)

	for _, c := range []struct{ key, global, tenant string }{
		// read directly at the tenant scope (store.TenantAllowsSharedSecrets)
		{store.AllowSharedSecretsKey, `{"value":false}`, `{"value":true}`},
		// read directly, but at the tenant id: a row at the home scope is dead
		{store.AllowCrossTenantBlockGrantKey, `{"value":false}`, `{"value":true}`},
		// read only through the overlay, which drops a tenant row: a dead row
		{"graph_cache.enabled", `{"value":false}`, `{"value":true}`},
	} {
		t.Run(c.key, func(t *testing.T) {
			if !config.IsGlobalOnly(c.key) {
				t.Fatalf("%s is not global-only; the case pins nothing", c.key)
			}
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
			if v, ok := scopeRowValue(t, pool, c.key, store.GlobalScope); !ok || v != "false" {
				t.Errorf("_global row = %q ok=%v, want false unchanged", v, ok)
			}
		})
	}
	if ok, err := store.TenantAllowsSharedSecrets(context.Background(), pool, "tenantg"); err != nil || ok {
		t.Errorf("TenantAllowsSharedSecrets(tenantg) = %v, %v — want false: the opt-in is the operator's", ok, err)
	}
	// Without a _global row the candidate build already refused it; the guard
	// keeps that answer.
	if rec := api.as(tenantAdmin("tenanth")).do(t, http.MethodPut, "/api/settings/graph_cache.rebuild_interval", `{"value":60}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("tenant-admin PUT on a global-only key without a _global row = %d body=%s, want 422", rec.Code, rec.Body.String())
	}
	// A tenant-overridable key stays tenant-writable.
	if rec := api.as(tenantAdmin("tenantg")).do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":0.4}`); rec.Code != http.StatusOK {
		t.Errorf("tenant-admin PUT on a tenant-overridable key = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
}
