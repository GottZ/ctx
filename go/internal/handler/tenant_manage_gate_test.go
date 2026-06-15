package handler

import (
	"net/http"
	"strings"
	"testing"
)

// Gate tests for Multi-Tenant wave T05a (tenant lifecycle manage-actions). These
// run DB-less against a nil store pool (manageReqWithPool): every gate must fire
// BEFORE the store layer, so reaching the store would panic — the missing-gate
// proof (same doctrine as backends_gate_test.go / admin_gate_test.go).
//
// RED (before T05a): tenant-* are unknown actions → HandleManage's default arm
// (400 "unknown action"), so the admin gate returns 400 not 403, and there is no
// reserved-slug 400 path. GREEN: tenant-* are admin-gated (403 for non-admin) and
// tenant-create rejects a '_'-prefixed slug with a 400 that names the reservation.

// TestTenantActionsRequireAdmin: every tenant-* lifecycle action answers 403 to a
// valid non-admin key (admin-gated via actionRequiresAdmin). Probed against a nil
// store pool — reaching the store would panic, which is the missing-gate proof.
func TestTenantActionsRequireAdmin(t *testing.T) {
	for _, action := range []string{
		"tenant-create", "tenant-list", "tenant-get", "tenant-update", "tenant-delete",
	} {
		rec := manageReqWithPool(t, nonAdminAR(), nil, map[string]any{
			"action": action, "id": "00000000-0000-0000-0000-0000000d3fa0",
			"data": map[string]any{"slug": "x", "display_name": "x"},
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with non-admin key: status %d, want 403", action, rec.Code)
		}
	}
}

// TestTenantDeleteDefaultGuard: tenant-delete on the default tenant is a 400 that
// NAMES the guard (the prune-default-guard, T05b — a full-prune of the default
// tenant would destroy the entire single-tenant corpus). The guard runs in the
// handler before store.PruneTenant, so the nil pool never panics. Asserting the
// body (not just 400) keeps this honest: before T05b "tenant-delete" was an
// unknown action whose 400 body said "unknown action", not the guard (RED).
func TestTenantDeleteDefaultGuard(t *testing.T) {
	rec := manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-delete",
		"id":     "00000000-0000-0000-0000-0000000d3fa0",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-delete default tenant: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "default tenant") {
		t.Fatalf("400 body does not name the default-tenant guard: %s", rec.Body.String())
	}
}

// TestTenantCreateReservedSlug: tenant-create with a '_'-prefixed slug is a 400
// (the reservedSlug gate, design/01 §4.3 Finding 17 — a separate gate from
// firstReservedScope, which checks scopes not a tenant slug). The check runs
// before the store, so the nil pool never panics. The 'default' slug is NOT
// '_'-prefixed and is a 409-via-UNIQUE at the store layer (tested in
// tenant_crud_integration_test.go), NOT a 400 here.
func TestTenantCreateReservedSlug(t *testing.T) {
	rec := manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-create",
		"data":   map[string]any{"slug": "_global", "display_name": "evil"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-create slug='_global': status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reserved") {
		t.Fatalf("400 body does not name the slug reservation: %s", rec.Body.String())
	}

	// An empty slug is also a 400 (required field) — still before the store.
	rec = manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-create",
		"data":   map[string]any{"slug": "", "display_name": "x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-create slug='': status %d, want 400", rec.Code)
	}

	// A leading-whitespace slug must NOT bypass the reserved gate: " _global"
	// must be normalized (trimmed) before the '_'-prefix check, so it is rejected
	// 400 and never reaches the store (adversarial-verify finding). Against the
	// pre-fix raw-HasPrefix code this passes the gate and hits the nil pool →
	// the manageReqWithPool panic-catch reports "reached the store layer" (RED).
	rec = manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-create",
		"data":   map[string]any{"slug": " _global", "display_name": "x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-create slug=' _global' (leading space): status %d, want 400 (no whitespace bypass)", rec.Code)
	}
}
