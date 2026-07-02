// WF T10 tier-gate probes for the manage type-* family (design/01 §5.4/§7-T10).
// DB-less against a nil store pool (manageReqAs): every gate must fire BEFORE
// the store layer — reaching it panics, which manageReqAs converts into the
// missing-gate failure. That construction makes these probes TRUE fail-open
// detectors: remove the actionTier entry for a mutation and the dispatcher
// default (tierOpen, context_manage.go) lets the request through to the nil
// pool ⇒ red. Verified red during the wave build (entry commented out ⇒
// TestActionTier_Classification + these probes fail), green with the entry.
package handler

import (
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// typeMutationBodies are VALID payloads: they must clear every pre-store
// validation (strict decode, scope binding, caps, DecodePolicy) so that a
// missing tier gate demonstrably reaches the store layer (panic ⇒ red).
func typeMutationBodies() []map[string]any {
	return []map[string]any{
		{"action": "type-create", "data": map[string]any{
			"name":   "probe-type",
			"config": map[string]any{"v": 1, "retrieval": map[string]any{"policy": "full-pass"}},
		}},
		{"action": "type-update", "id": "0198b0d2-0000-7000-8000-000000000001", "data": map[string]any{
			"display_name": "Probe",
		}},
		{"action": "type-delete", "id": "0198b0d2-0000-7000-8000-000000000001"},
	}
}

// TestActionTier_Member_TypeMutations_403: the §5.4-N1 fail-open probe. A
// member key on any type mutation gets 403 AT THE TIER GATE — never reaches
// the store. RED without the actionTier entry (default tierOpen would
// dispatch into the nil pool).
func TestActionTier_Member_TypeMutations_403(t *testing.T) {
	for _, body := range typeMutationBodies() {
		rec := manageReqAs(t, memberAR(), body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("action %v with member key: status = %d, want 403 (tier gate)", body["action"], rec.Code)
		}
	}
}

// TestActionTier_TenantAdmin_TypeMutations_403: the T10 scope-binding probe
// (§5.4 R1). In tier 1 the type mutations are server-admin — a tenant-admin
// pointing an explicit scope='_global' at the payload is rejected at the
// tier, so the namespace authority is never caller-chosen. (T12 opens the
// tenant tier; the handler then binds hard to ar.TenantID — S2 pattern.)
func TestActionTier_TenantAdmin_TypeMutations_403(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		rec := manageReqAs(t, tenantAdminAR(role), map[string]any{
			"action": "type-create",
			"data": map[string]any{
				"name":   "probe-type",
				"scope":  "_global",
				"config": map[string]any{"v": 1},
			},
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("role %q: type-create scope=_global status = %d, want 403", role, rec.Code)
		}
	}
}

// TestActionTier_TypeGet_Open: the reads are tierOpen (UI badges) — a member
// clears the gate, proven by reaching the id validation (400), which sits
// AFTER the gate but BEFORE the store (the api-key-create PassesGate pattern).
func TestActionTier_TypeGet_Open(t *testing.T) {
	rec := manageReqAs(t, memberAR(), map[string]any{"action": "type-get"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("type-get without id as member: status = %d, want 400 (past the open gate, at id validation)", rec.Code)
	}
}
