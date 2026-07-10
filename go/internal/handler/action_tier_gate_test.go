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
		// oauth-provider-* (OAuth L3, 04-W4): the external-login IdP allowlist
		// is the INV-C trust anchor and operator-global → all server-admin.
		{"oauth-provider-create", "", tierServerAdmin},
		{"oauth-provider-list", "", tierServerAdmin},
		{"oauth-provider-delete", "", tierServerAdmin},
		// oauth-identity-* (OAuth R5, 05 §4.5, E4b admin-invite): identity
		// bindings decide WHO a login resolves to — provider-allowlist trust
		// altitude.
		{"oauth-identity-link", "", tierServerAdmin},
		{"oauth-identity-list", "", tierServerAdmin},
		{"oauth-identity-unlink", "", tierServerAdmin},
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
		// project-provision (I-I): CREATES a tenant → server-admin (E4). Removing
		// its actionTier entry drops it to the fail-open tierOpen default → this row
		// turns RED (the S9 probe).
		{"project-provision", "", tierServerAdmin},
		{"tenant-list", "", tierServerAdmin},
		{"tenant-get", "", tierServerAdmin},
		{"tenant-update", "", tierServerAdmin},
		{"tenant-delete", "", tierServerAdmin},
		{"tenant-grant-create", "", tierServerAdmin},
		{"tenant-grant-list", "", tierServerAdmin},
		{"tenant-grant-delete", "", tierServerAdmin},
		// type-* (WF T10, design/01 §5.4): mutations switch block visibility
		// policy and tier 1 only has the server-global '_global' namespace →
		// server-admin. THIS table is the direct fail-open probe: the
		// dispatcher default is tierOpen, so removing the actionTier entry
		// turns these rows red (a forgotten entry would admit every valid key).
		{"type-create", "", tierServerAdmin},
		{"type-update", "", tierServerAdmin},
		{"type-delete", "", tierServerAdmin},
		// type-list/get are deliberately open (UI badges); handler-scoped
		// to '_global' ∪ own tenant namespace (K-T1).
		{"type-list", "", tierOpen},
		{"type-get", "", tierOpen},
		// dream/gaming: only the mutating shape is gated; read stays open
		{"dream-mode", "", tierOpen},
		{"dream-mode", `{"mode":"off"}`, tierServerAdmin},
		{"gaming-mode", "", tierOpen},
		{"gaming-mode", `{"mode":"on"}`, tierServerAdmin},
		// eject-mode (AM-7 canonical) mirrors the gaming-mode alias exactly:
		// read open (U01-E6 legacy surface), mutation server-admin (it flips the
		// _global eject profile = system egress topology).
		{"eject-mode", "", tierOpen},
		{"eject-mode", `{"mode":"on"}`, tierServerAdmin},
		// Abschaltprofile (U01-W3, AM-5 VOLL): tierTenantAdmin — the handler +
		// store are tenant-isolated (scope predicate), so a tenant-admin manages
		// its OWN-scope profiles and reads _global ones; it can never mutate a
		// _global profile (store scope gate → 404).
		{"disable-profile-list", "", tierTenantAdmin},
		{"disable-profile-create", "", tierTenantAdmin},
		{"disable-profile-update", "", tierTenantAdmin},
		{"disable-profile-delete", "", tierTenantAdmin},
		{"disable-profile-toggle", "", tierTenantAdmin},
		// Achse-02 issue/comment family (I-D, design/02 §4.3): all tierOpen —
		// scope isolation is in the store layer, not an admin tier. The EXPLICIT
		// entries are mandatory (§4.3); the enumeration gate below proves a
		// missing entry (fail-open tierOpen default, S9) goes red.
		{"issue-create", "", tierOpen},
		{"issue-update", "", tierOpen},
		{"issue-get", "", tierOpen},
		{"issue-list", "", tierOpen},
		{"issue-comment-create", "", tierOpen},
		{"issue-link-create", "", tierOpen},
		{"issue-link-delete", "", tierOpen},
		// Achse-02 forge sync family (I-F, design/02 §4.3): tierTenantAdmin —
		// PAT injection / outbound sync trigger, ownership re-checked per project.
		{"forge-token-set", "", tierTenantAdmin},
		{"forge-sync-start", "", tierTenantAdmin},
		{"forge-sync-status", "", tierTenantAdmin},
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

// TestActionTier_IssueFamilyExplicitlyTiered is the Achse-02 I-D S9 fail-open
// probe (design/02 §5.1 / §7): the manage dispatcher default is tierOpen, so a
// dispatched action WITHOUT an actionTier entry silently inherits tierOpen (a
// forgotten gated action would admit every valid key). Each issue-* action this
// wave dispatches MUST be EXPLICITLY classified — actionTierExplicit returns
// ok=true. Remove any issue arm from actionTier and its row here turns RED
// (proven: the entries + dispatch arm land in the SAME commit). This is the
// forward guard the design mandates: a future dispatched action (forge-repo-* in
// I-F, tenant-admin) added without its actionTier entry is caught the same way.
func TestActionTier_IssueFamilyExplicitlyTiered(t *testing.T) {
	issueActions := []string{
		"issue-create", "issue-update", "issue-get", "issue-list",
		"issue-comment-create", "issue-link-create", "issue-link-delete",
	}
	for _, a := range issueActions {
		tier, explicit := actionTierExplicit(manageRequest{Action: a})
		if !explicit {
			t.Errorf("action %q is dispatched but NOT explicitly tiered (fail-open tierOpen default, S9)", a)
		}
		if tier != tierOpen {
			t.Errorf("action %q tier = %d, want tierOpen (store-layer isolation)", a, tier)
		}
	}
	// Control: a truly unknown action DOES fall through to the fail-open default
	// (explicit=false) — the exact property the explicit entries above guard
	// against. If this ever reported true, the detection mechanism is broken.
	if _, explicit := actionTierExplicit(manageRequest{Action: "definitely-not-an-action"}); explicit {
		t.Error("unknown action reported as explicitly tiered — S9 fail-open detection broken")
	}
}

// TestActionTier_ForgeFamilyExplicitlyTiered is the Achse-02 I-F S9 fail-open
// probe (design/02 §5.1): each forge-* action this wave dispatches MUST be
// EXPLICITLY classified tierTenantAdmin. Remove any forge arm from actionTier and
// its row here turns RED (the entries + dispatch arm land in the SAME commit).
// The I-D IssueFamily test enumerates a HARDCODED list — it does NOT auto-extend
// to new families, so I-F adds its own enumeration here (documented drift from
// the "erweitert sich automatisch" assumption).
func TestActionTier_ForgeFamilyExplicitlyTiered(t *testing.T) {
	for _, a := range []string{"forge-token-set", "forge-sync-start", "forge-sync-status"} {
		tier, explicit := actionTierExplicit(manageRequest{Action: a})
		if !explicit {
			t.Errorf("action %q is dispatched but NOT explicitly tiered (fail-open tierOpen default, S9)", a)
		}
		if tier != tierTenantAdmin {
			t.Errorf("action %q tier = %d, want tierTenantAdmin (credential injection / outbound trigger)", a, tier)
		}
	}
}

// TestActionTier_ProvisionExplicitlyTiered is the I-I S9 fail-open probe:
// project-provision is dispatched (via the tenant family) and MUST be EXPLICITLY
// classified tierServerAdmin — it CREATES a tenant (the allocation authority is
// server-admin, E4). Remove its actionTier arm and this turns RED (the entry +
// dispatch arm land in the SAME commit).
func TestActionTier_ProvisionExplicitlyTiered(t *testing.T) {
	tier, explicit := actionTierExplicit(manageRequest{Action: "project-provision"})
	if !explicit {
		t.Error("project-provision is dispatched but NOT explicitly tiered (fail-open tierOpen default, S9)")
	}
	if tier != tierServerAdmin {
		t.Errorf("project-provision tier = %d, want tierServerAdmin (tenant-creation authority, E4)", tier)
	}
}

// TestActionTier_OAuthProviderFamilyExplicitlyTiered is the OAuth-L3 (04-W4)
// S9 fail-open probe (design/02 §5.1 pattern): every oauth-provider-* action
// this wave dispatches MUST be EXPLICITLY classified tierServerAdmin — the
// provider table is the INV-C trust anchor (an attacker-registered IdP would
// mint arbitrary identities). Remove any arm from actionTier and its row here
// turns RED (the entries + dispatch arm land in the SAME commit).
func TestActionTier_OAuthProviderFamilyExplicitlyTiered(t *testing.T) {
	for _, a := range []string{"oauth-provider-create", "oauth-provider-list", "oauth-provider-delete"} {
		tier, explicit := actionTierExplicit(manageRequest{Action: a})
		if !explicit {
			t.Errorf("action %q is dispatched but NOT explicitly tiered (fail-open tierOpen default, S9)", a)
		}
		if tier != tierServerAdmin {
			t.Errorf("action %q tier = %d, want tierServerAdmin (INV-C trust anchor, operator-global)", a, tier)
		}
	}
}

// TestActionTier_OAuthIdentityFamilyExplicitlyTiered is the R5 S9 fail-open
// probe (design/05 §4.5, E4b admin-invite): every oauth-identity-* action
// MUST be EXPLICITLY tierServerAdmin — identity bindings decide WHO a login
// resolves to.
func TestActionTier_OAuthIdentityFamilyExplicitlyTiered(t *testing.T) {
	for _, a := range []string{"oauth-identity-link", "oauth-identity-list", "oauth-identity-unlink"} {
		tier, explicit := actionTierExplicit(manageRequest{Action: a})
		if !explicit {
			t.Errorf("action %q is dispatched but NOT explicitly tiered (fail-open tierOpen default, S9)", a)
		}
		if tier != tierServerAdmin {
			t.Errorf("action %q tier = %d, want tierServerAdmin (login-resolution trust anchor)", a, tier)
		}
	}
}

// TestActionTier_DisableProfileFamilyExplicitlyTiered is the U01-W3 S9 fail-open
// probe (design/01 §4.3): every disable-profile-* action this wave dispatches
// MUST be EXPLICITLY classified tierTenantAdmin. Remove any arm from actionTier
// and its row here turns RED (the entries + dispatch arm land in the SAME commit).
// eject-mode (AM-7 canonical, alias of gaming-mode) is enumerated too: its
// mutation MUST be tierServerAdmin (system egress topology).
func TestActionTier_DisableProfileFamilyExplicitlyTiered(t *testing.T) {
	for _, a := range []string{
		"disable-profile-list", "disable-profile-create", "disable-profile-update",
		"disable-profile-delete", "disable-profile-toggle",
	} {
		tier, explicit := actionTierExplicit(manageRequest{Action: a})
		if !explicit {
			t.Errorf("action %q is dispatched but NOT explicitly tiered (fail-open tierOpen default, S9)", a)
		}
		if tier != tierTenantAdmin {
			t.Errorf("action %q tier = %d, want tierTenantAdmin (tenant-isolated handler + store scope gate)", a, tier)
		}
	}
	// eject-mode: read open, mutation server-admin (byte-identical to gaming-mode).
	if tier, explicit := actionTierExplicit(manageRequest{Action: "eject-mode"}); !explicit || tier != tierOpen {
		t.Errorf("eject-mode read tier = %d explicit=%v, want tierOpen/true", tier, explicit)
	}
	if tier, explicit := actionTierExplicit(manageRequest{Action: "eject-mode", Data: json.RawMessage(`{"mode":"on"}`)}); !explicit || tier != tierServerAdmin {
		t.Errorf("eject-mode mutation tier = %d explicit=%v, want tierServerAdmin/true", tier, explicit)
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
		{"action": "oauth-provider-list"},
		{"action": "oauth-provider-create", "data": map[string]any{"slug": "x"}},
		{"action": "oauth-provider-delete", "data": map[string]any{"slug": "x"}},
		{"action": "blocks-audit-status"},
		{"action": "blocks-classify-status"},
		{"action": "tenant-list"},
		{"action": "project-provision", "data": map[string]any{"identity": "manual:x"}},
		{"action": "tenant-grant-list"},
		{"action": "gaming-mode", "data": map[string]any{"mode": "on"}},
		{"action": "eject-mode", "data": map[string]any{"mode": "on"}},
		{"action": "dream-mode", "data": map[string]any{"mode": "off"}},
	}
	for _, body := range bodies {
		rec := manageReqAs(t, tenantAdminAR(auth.RoleAdmin), body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("action %v with tenant-admin: status = %d, want 403 (stays server-admin)", body["action"], rec.Code)
		}
	}
}
