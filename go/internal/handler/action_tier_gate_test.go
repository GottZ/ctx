package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// TestActionTier_Classification pins the full design 05 §4.4 tier table on the
// pure function (store-free). It is the completeness assertion: every gated
// action lands in the intended tier, and no read path is accidentally gated.
func TestActionTier_Classification(t *testing.T) {
	cases := []struct {
		action string
		data   string // raw req.Data ("" = absent)
		want   adminTier
	}{
		// per-tenant (isolated handlers T22/T23/T24; backend-* by T37/04-W5)
		{"api-key-create", "", tierTenantAdmin},
		{"api-key-list", "", tierTenantAdmin},
		{"api-key-delete", "", tierTenantAdmin},
		{"backend-create", "", tierTenantAdmin},
		{"backend-update", "", tierTenantAdmin},
		{"backend-delete", "", tierTenantAdmin},
		{"backend-list", "", tierTenantAdmin},
		{"tenant-quota-get", "", tierTenantAdmin}, // read own quota (transparency)
		// server-global (not yet isolated, or operator-level by nature)
		{"mcp-client-create", "", tierServerAdmin},
		{"mcp-client-list", "", tierServerAdmin},
		{"mcp-client-delete", "", tierServerAdmin},
		// backend-test reaches an arbitrary backend by id with its resolved key
		// and is NOT tenant-filtered → stays server-admin (T37 isolated the
		// CRUD/list, deliberately not the probe).
		{"backend-test", "", tierServerAdmin},
		// tenant-quota-set is an operator ceiling — a tenant raising its own
		// budget would void it, so the WRITE stays server-admin (read above).
		{"tenant-quota-set", "", tierServerAdmin},
		{"blocks-audit-start", "", tierServerAdmin},
		{"blocks-audit-status", "", tierServerAdmin},
		{"blocks-classify-start", "", tierServerAdmin},
		{"blocks-classify-status", "", tierServerAdmin},
		{"tenant-create", "", tierServerAdmin},
		{"tenant-list", "", tierServerAdmin},
		{"tenant-get", "", tierServerAdmin},
		{"tenant-update", "", tierServerAdmin},
		{"tenant-delete", "", tierServerAdmin},
		{"tenant-grant-create", "", tierServerAdmin},
		{"tenant-grant-list", "", tierServerAdmin},
		{"tenant-grant-delete", "", tierServerAdmin},
		// dream/gaming: only the mutating shape is gated; read stays open
		{"dream-mode", "", tierOpen},
		{"dream-mode", `{"mode":"off"}`, tierServerAdmin},
		{"gaming-mode", "", tierOpen},
		{"gaming-mode", `{"mode":"on"}`, tierServerAdmin},
		// ungated read/CRUD paths (auth + scope only)
		{"get", "", tierOpen},
		{"stats", "", tierOpen},
		{"update", "", tierOpen},
		{"delete", "", tierOpen},
		{"guard-list", "", tierOpen},
		{"dream-stats", "", tierOpen},
		{"unknown-action", "", tierOpen},
	}
	for _, c := range cases {
		req := manageRequest{Action: c.action}
		if c.data != "" {
			req.Data = json.RawMessage(c.data)
		}
		if got := actionTier(req); got != c.want {
			t.Errorf("actionTier(%q, data=%q) = %d, want %d", c.action, c.data, got, c.want)
		}
	}
}

// Gate tests for Multi-Tenant wave T25 (05-A8): the action-tier cut that turns
// the binary admin gate (actionRequiresAdmin) into a two-tier classification
// (server-admin vs tenant-admin, design 05 §4.4). They run DB-less against a nil
// store pool (manageReqAs, admin_gate_test.go): every gate must fire BEFORE the
// store layer, so reaching it would panic — the missing-gate proof.
//
// Decision verified against the primary source (design/05 §7 pausability
// invariant + the live handlers, W3/W9): A8 grants the tenant-admin tier ONLY to
// api-key-create/list/delete, whose handlers are already tenant-isolated
// (T22/T23/T24). mcp-client-*, backend-*, blocks-audit/classify-*, tenant-* and
// tenant-grant-* STAY server-admin — their handlers carry no tenant filter yet
// (handleMCPClientList takes no AuthResult; handleBackendList ignores it;
// dispatchBlocksAction passes none), so opening them now would be fail-OPEN.
// dream-/gaming-mode mutations are server-global by design and stay server-admin.

// tenantAdminAR builds a non-server-admin key that IS a tenant-admin of its own
// tenant (role owner or admin). ctx_auth guarantees a non-empty tenant_id for
// every valid key, so the fixture sets one (unlike the pre-tenant nonAdminAR).
func tenantAdminAR(role auth.Role) *auth.AuthResult {
	ar := nonAdminAR()
	ar.TenantID = "tenant-a"
	ar.TenantRole = role
	return ar
}

// memberAR builds a member of its own tenant: a valid key that is NOT an admin
// of any kind (L4 self-escalation probe).
func memberAR() *auth.AuthResult {
	ar := nonAdminAR()
	ar.TenantID = "tenant-a"
	ar.TenantRole = auth.RoleMember
	return ar
}

// TestActionTier_TenantAdmin_ApiKeyCreate_PassesGate: a tenant-admin (owner OR
// admin) clears the tier gate for api-key-create — proven by reaching the label
// validation (400), which sits AFTER the gate but BEFORE the store. This is the
// "A8 activates A5-A7" property: T22's mint-scope handler stops being dormant.
//
// RED before T25: requireAdminAction rejected every non-server-admin with 403,
// so the request never reached the label check.
func TestActionTier_TenantAdmin_ApiKeyCreate_PassesGate(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		rec := manageReqAs(t, tenantAdminAR(role), map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "", "home_scope": "tenant-a"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("role %q: status = %d, want 400 (past the tier gate, at label validation)", role, rec.Code)
		}
	}
}

// TestActionTier_TenantAdmin_ApiKeyDelete_PassesGate: tenant-admin clears the
// gate for api-key-delete too — empty id is a 400 before the store. RED: 403.
func TestActionTier_TenantAdmin_ApiKeyDelete_PassesGate(t *testing.T) {
	rec := manageReqAs(t, tenantAdminAR(auth.RoleAdmin), map[string]any{
		"action": "api-key-delete",
		"data":   map[string]any{"id": ""},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (id required, past the tier gate)", rec.Code)
	}
}

// TestActionTier_Member_ApiKeyCreate_403: L4 self-escalation — a member of its
// OWN tenant still cannot mint keys. administers() is false for member, so the
// tenant-admin tier rejects it. (Already 403 today via the binary gate; this
// guards that the finer cut does not accidentally let members through.)
func TestActionTier_Member_ApiKeyCreate_403(t *testing.T) {
	rec := manageReqAs(t, memberAR(), map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "x", "home_scope": "tenant-a"},
	})
	assertForbiddenAdmin(t, rec)
}

// TestActionTier_TenantAdmin_ServerAdminActions_403 is the load-bearing
// fail-closed guard of this wave (§7 pausability invariant): the actions whose
// handlers are NOT yet tenant-isolated STAY server-admin. A tenant-admin must
// get 403 — never reach a handler that would disclose foreign-tenant data or
// trigger a server-wide mutation. If a future change drops one of these into the
// tenant-admin tier without first isolating its handler, this test goes red.
func TestActionTier_TenantAdmin_ServerAdminActions_403(t *testing.T) {
	bodies := []map[string]any{
		{"action": "backend-test", "id": "x"},
		{"action": "mcp-client-list"},
		{"action": "mcp-client-create", "data": map[string]any{"label": "x"}},
		{"action": "blocks-audit-status"},
		{"action": "blocks-classify-status"},
		{"action": "tenant-list"},
		{"action": "tenant-grant-list"},
		{"action": "gaming-mode", "data": map[string]any{"mode": "on"}},
		{"action": "dream-mode", "data": map[string]any{"mode": "off"}},
	}
	for _, body := range bodies {
		rec := manageReqAs(t, tenantAdminAR(auth.RoleAdmin), body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("action %v with tenant-admin: status = %d, want 403 (stays server-admin)", body["action"], rec.Code)
		}
	}
}
