package handler

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// MT 04-W6/A0 (design/04 §3): the additive server-admin scope-overview read.
// These probes are DB-less — the tier gate fires BEFORE the store (a nil pool
// would panic, the missing-gate proof, same doctrine as admin_gate_test.go) and
// the golden shape is asserted on the exported struct's JSON tags. The full
// GROUP-BY-without-readScopes-filter behaviour is proved end-to-end by the central
// integration verification; here the no-filter guarantee is STRUCTURAL: ScopeOverviews
// takes no readScopes parameter, so it cannot filter by scope.

// TestScopeOverview_TierServerAdmin pins the action to the server-admin tier —
// the unscoped global aggregate must NOT be reachable by a tenant-admin.
func TestScopeOverview_TierServerAdmin(t *testing.T) {
	if got := actionTier(manageRequest{Action: "scope-overview"}); got != tierServerAdmin {
		t.Fatalf("actionTier(scope-overview) = %d, want tierServerAdmin (%d)", got, tierServerAdmin)
	}
}

// TestScopeOverview_NonAdmin403: a plain valid key is rejected at the tier gate.
// RED against an ungated chain: the request would fall through to the nil store
// pool and panic.
func TestScopeOverview_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{"action": "scope-overview"})
	assertForbiddenAdmin(t, rec)
}

// TestScopeOverview_TenantAdmin403: a tenant-admin of its own tenant is ALSO
// rejected — scope-overview is a global landscape, server-admin only (it stays in
// tierServerAdmin, never tierTenantAdmin). The 403 body is the no-oracle "admin
// key required".
func TestScopeOverview_TenantAdmin403(t *testing.T) {
	rec := manageReqAs(t, tenantAdminAR(auth.RoleAdmin), map[string]any{"action": "scope-overview"})
	assertForbiddenAdmin(t, rec)
}

// TestScopeOverview_GoldenShape freezes the wire contract with the frontend
// ScopeOverview type (design/04 §3): EXACTLY the fields scope/block_count/
// key_count/tenant_id, no more, no fewer. A rename here silently breaks the
// client. tenant_id must serialize as null (not "") for an unmapped system scope —
// the pointer carries that distinction.
func TestScopeOverview_GoldenShape(t *testing.T) {
	b, err := json.Marshal(store.ScopeOverview{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"block_count", "key_count", "scope", "tenant_id"}
	if len(got) != len(want) {
		t.Fatalf("field set = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("field set = %v, want exactly %v", got, want)
		}
	}
	if string(m["tenant_id"]) != "null" {
		t.Errorf("tenant_id zero value = %s, want null (pointer ⇒ unmapped scope serializes as null)", m["tenant_id"])
	}
}
