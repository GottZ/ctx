//go:build integration

// Integration test for Multi-Tenant wave T43 (Achse 07-W6, design/07 §4.7/§5.1/
// §5.2): the WRITE side of block-level sharing — the block-grant-create/list/
// revoke manage-actions, their hard per-block OWNERSHIP gate (§5.1) and the
// cross-tenant OPT-IN gate (§5.2).
//
// The tier gate (requireAdminAction) only checks the server-global is_admin; the
// ownership gate is the ONLY thing between an admin and cross-tenant exfiltration,
// so it is probed directly:
//
//   - G6 (ownership): create on a block the caller-tenant does NOT own → 403;
//     create on a block in an UNMAPPED Altbestands-scope → 403 (unresolvability
//     fails closed, not "owned").
//   - G7 (cross-tenant gate): cross-tenant create with opt-in=false → 403;
//     intra-tenant create → 200; cross-tenant with opt-in=true → 200; an
//     unresolvable (unregistered) grantee → 403 (treated as cross-tenant).
//   - CRUD + revocation: create → list shows it → revoke → GrantedBlockIDs empty.
//   - Pausable: an empty grant set leaves the read path byte-identical (asserted
//     via GrantedBlockIDs before any grant).
//
//	go test -tags=integration ./internal/handler/ -run TestBlockGrantT43 -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// t43GhostTenant is a syntactically valid UUID never registered as a tenant — the
// unresolvable-grantee probe for G7.
const t43GhostTenant = "99999999-8888-7777-6666-555544443333"

// callBlockGrant posts a block-grant action through HandleManage with the given
// AuthResult in the request context (so the admin tier gate + dispatch + gates all
// run) and returns the HTTP status + decoded body.
func callBlockGrant(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action string, data map[string]string, id string) (int, map[string]any) {
	t.Helper()
	payload := map[string]any{"action": action}
	if data != nil {
		payload["data"] = data
	}
	if id != "" {
		payload["id"] = id
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s response: %v (body %s)", action, err, rec.Body.String())
	}
	return rec.Code, resp
}

func TestBlockGrantT43_OwnershipAndCrossTenantGates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	const scopeC = "t43-owner-c"   // owner scope
	const scopeA = "t43-grantee-a" // grantee scope
	const scopeOrphan = "t43-orphan"
	owner := bgTenant(t, pool, "t43-owner")
	grantee := bgTenant(t, pool, "t43-grantee")
	bgMapScope(t, pool, scopeC, owner)
	bgMapScope(t, pool, scopeA, grantee)

	ownerBlock := bgBlock(t, pool, scopeC, "t43-owner-block")      // owned by `owner`
	foreignBlock := bgBlock(t, pool, scopeA, "t43-foreign-block")  // owned by `grantee`
	orphanBlock := bgBlock(t, pool, scopeOrphan, "t43-orphan-blk") // scope mapped to NO tenant

	// Server-admin key whose tenant is the OWNER. is_admin=true clears the tier
	// gate; the ownership gate is the real test.
	adminAR := func() *auth.AuthResult {
		return &auth.AuthResult{
			IsValid: true, IsAdmin: true, HomeScope: scopeC,
			ReadScopes: []string{scopeC}, TenantID: owner, ApiKeyID: "",
		}
	}

	// Pausable: with no grants the read path sees nothing extra.
	t.Run("pausable_empty_grant_set", func(t *testing.T) {
		ids, err := store.GrantedBlockIDs(context.Background(), pool, grantee)
		if err != nil {
			t.Fatalf("GrantedBlockIDs: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("pre-grant GrantedBlockIDs = %v, want empty (byte-identical scope-only path)", ids)
		}
	})

	// G6(a): the owner-tenant does NOT own scopeA → create on foreignBlock is denied.
	t.Run("G6a_create_on_unowned_block_denied", func(t *testing.T) {
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-create",
			map[string]string{"block_id": foreignBlock, "grantee_tenant": owner}, "")
		if status != http.StatusForbidden {
			t.Fatalf("create on unowned block: status %d, want 403 (ownership gate); resp %v", status, resp)
		}
	})

	// G6(b): a block in an Altbestands-scope with NO context_tenant_scopes mapping →
	// the scope is in no tenant's owned set → DENY (unresolvability fails closed).
	t.Run("G6b_create_on_orphan_scope_denied", func(t *testing.T) {
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-create",
			map[string]string{"block_id": orphanBlock, "grantee_tenant": owner}, "")
		if status != http.StatusForbidden {
			t.Fatalf("create on unmapped-scope block: status %d, want 403 (unresolvable scope must not pass as owned); resp %v", status, resp)
		}
	})

	// G7: cross-tenant create with opt-in OFF (default) is denied.
	t.Run("G7_cross_tenant_optin_off_denied", func(t *testing.T) {
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-create",
			map[string]string{"block_id": ownerBlock, "grantee_tenant": grantee}, "")
		if status != http.StatusForbidden {
			t.Fatalf("cross-tenant create with opt-in off: status %d, want 403; resp %v", status, resp)
		}
		// And no row leaked in.
		ids, _ := store.GrantedBlockIDs(context.Background(), pool, grantee)
		if len(ids) != 0 {
			t.Fatalf("denied cross-tenant create still wrote a grant (GrantedBlockIDs=%v)", ids)
		}
	})

	// G7: an unresolvable (unregistered) grantee is treated as cross-tenant → 403
	// (opt-in off). The grantee FK never even runs — the gate fires first.
	t.Run("G7_unresolvable_grantee_denied", func(t *testing.T) {
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-create",
			map[string]string{"block_id": ownerBlock, "grantee_tenant": t43GhostTenant}, "")
		if status != http.StatusForbidden {
			t.Fatalf("unresolvable grantee create: status %d, want 403 (treat-as-cross-tenant DENY); resp %v", status, resp)
		}
	})

	// G7: intra-tenant create (grantee == owner tenant) is always allowed.
	t.Run("G7_intra_tenant_create_allowed", func(t *testing.T) {
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-create",
			map[string]string{"block_id": ownerBlock, "grantee_tenant": owner}, "")
		if status != http.StatusOK {
			t.Fatalf("intra-tenant create: status %d, want 200; resp %v", status, resp)
		}
		if ok, _ := resp["success"].(bool); !ok {
			t.Fatalf("intra-tenant create not success: %v", resp)
		}
	})

	// G7: cross-tenant create SUCCEEDS once the owner tenant opts in.
	t.Run("G7_cross_tenant_optin_on_allowed", func(t *testing.T) {
		seedSettingScope(t, pool, store.AllowCrossTenantBlockGrantKey, owner, `true`)
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-create",
			map[string]string{"block_id": ownerBlock, "grantee_tenant": grantee}, "")
		if status != http.StatusOK {
			t.Fatalf("cross-tenant create with opt-in on: status %d, want 200; resp %v", status, resp)
		}
		ids, _ := store.GrantedBlockIDs(context.Background(), pool, grantee)
		if !containsStr(ids, ownerBlock) {
			t.Fatalf("opted-in cross-tenant grant not visible to grantee (GrantedBlockIDs=%v)", ids)
		}
	})

	// CRUD: list shows the owner's grants; revoke removes it immediately.
	t.Run("CRUD_list_and_revoke", func(t *testing.T) {
		status, resp := callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-list", nil, ownerBlock)
		if status != http.StatusOK {
			t.Fatalf("list: status %d, want 200; resp %v", status, resp)
		}
		grants, _ := resp["grants"].([]any)
		if len(grants) == 0 {
			t.Fatalf("list returned no grants for an owner that made 2 (intra + cross); resp %v", resp)
		}

		// Revoke the cross-tenant grant → grantee loses sight immediately.
		status, resp = callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-revoke",
			map[string]string{"block_id": ownerBlock, "grantee_tenant": grantee}, "")
		if status != http.StatusOK {
			t.Fatalf("revoke: status %d, want 200; resp %v", status, resp)
		}
		ids, _ := store.GrantedBlockIDs(context.Background(), pool, grantee)
		if containsStr(ids, ownerBlock) {
			t.Fatalf("revoked grant still visible to grantee (GrantedBlockIDs=%v) — revoke must be immediate", ids)
		}

		// Revoke on a block the caller does NOT own is denied (ownership gate).
		status, _ = callBlockGrant(t, t43Handler(pool), adminAR(), "block-grant-revoke",
			map[string]string{"block_id": foreignBlock, "grantee_tenant": owner}, "")
		if status != http.StatusForbidden {
			t.Fatalf("revoke on unowned block: status %d, want 403", status)
		}
	})
}

// h builds a ManageHandler with only the pool (the block-grant actions need no
// dream/backend/quota controllers).
func t43Handler(pool *pgxpool.Pool) *ManageHandler {
	return NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
