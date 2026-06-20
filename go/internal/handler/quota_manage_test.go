package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// MT T36b (04-W4): tenant-quota-set validation gates, tested DB-less — each
// probe stops at the validation layer BEFORE the store (a nil pool would panic,
// which is the missing-gate proof). The set/get round-trip + tenant-admin
// scope-pinning are proved against PG18 in quota_manage_integration_test.go.

func TestQuotaSetRejectsReservedScope(t *testing.T) {
	bp := backends.NewPool(nil, nil)
	for _, scope := range []string{"_global", "_anything"} {
		rec := manageReqWithPool(t, serverAdminScopedAR(), bp, map[string]any{
			"action": "tenant-quota-set",
			"data":   map[string]any{"scope": scope, "daily_cost_usd": 1.0},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("scope %q: status %d, want 400 (reserved scope rejected)", scope, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "tenant scope") {
			t.Errorf("scope %q: error should name the tenant-scope rule: %s", scope, rec.Body.String())
		}
	}
}

func TestQuotaSetScopeRequired(t *testing.T) {
	bp := backends.NewPool(nil, nil)
	rec := manageReqWithPool(t, serverAdminScopedAR(), bp, map[string]any{
		"action": "tenant-quota-set",
		"data":   map[string]any{"daily_cost_usd": 1.0},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "scope required") {
		t.Fatalf("empty scope: status %d body %s, want 400 scope required", rec.Code, rec.Body.String())
	}
}

func TestQuotaSetBadOnExceed(t *testing.T) {
	bp := backends.NewPool(nil, nil)
	rec := manageReqWithPool(t, serverAdminScopedAR(), bp, map[string]any{
		"action": "tenant-quota-set",
		"data":   map[string]any{"scope": "tenant-a", "on_exceed": "yolo"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad on_exceed: status %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestQuotaSetTenantAdmin403: a tenant-admin must NOT set a quota (operator
// ceiling — the action-tier gate keeps tenant-quota-set server-admin).
func TestQuotaSetTenantAdmin403(t *testing.T) {
	bp := backends.NewPool(nil, nil)
	rec := manageReqWithPool(t, tenantAdminScopedAR("tenant-a"), bp, map[string]any{
		"action": "tenant-quota-set",
		"data":   map[string]any{"scope": "tenant-a", "daily_cost_usd": 999.0},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tenant-admin set: status %d, want 403 (set stays server-admin)", rec.Code)
	}
}
