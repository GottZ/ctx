//go:build integration

// Integration test for the manage write-scope fix: `manage update`/`delete`
// must operate over a key's WRITE-eligible scope set — home_scope plus 'shared'
// when that collaboration scope is in allowed_scopes — exactly like /api/store
// already allows on CREATE. Before the fix, both handlers resolved the id and
// gated the mutation on home_scope ALONE, so a block living in `shared` was
// "Block not found" on update/delete even though the key may freely create one
// there (write-in, never-out). A full UUID resolves unambiguously; the gate is a
// permission check, and it must cover the write scopes, not just home_scope.
//
//	go test -tags=integration ./internal/handler/ -run TestManageWriteScope -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// callManage posts a manage action with the AuthResult injected into the request
// context (so dispatch + scope gating run end to end) and returns status + body.
func callManage(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action, id string, data map[string]any) (int, map[string]any) {
	t.Helper()
	payload := map[string]any{"action": action}
	if id != "" {
		payload["id"] = id
	}
	if data != nil {
		payload["data"] = data
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func TestManageWriteScope_SharedBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := t43Handler(pool)

	sharedBlock := bgBlock(t, pool, "shared", "ws-shared-block")
	sharedBlock2 := bgBlock(t, pool, "shared", "ws-shared-block-2")
	workBlock := bgBlock(t, pool, "work", "ws-work-block")

	// home=private; 'shared' is writable (in allowed), 'work' is read-only by the
	// store write policy (only home_scope + shared are write scopes).
	ar := func() *auth.AuthResult {
		return &auth.AuthResult{
			IsValid:       true,
			HomeScope:     "private",
			AllowedScopes: []string{"shared", "work"},
			ReadScopes:    []string{"private", "shared", "work"},
			TenantID:      store.DefaultTenantID,
		}
	}

	// The block lives in shared and the key may write shared → update by full UUID
	// must succeed. (RED before fix: home_scope-only gate → "Block not found".)
	t.Run("update_shared_by_full_uuid", func(t *testing.T) {
		status, resp := callManage(t, h, ar(), "update", sharedBlock,
			map[string]any{"content": "updated via manage"})
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("update shared block by full uuid: status %d resp %v — want success", status, resp)
		}
	})

	// An unambiguous partial UUID resolves within the write scopes too.
	t.Run("update_shared_by_partial_uuid", func(t *testing.T) {
		status, resp := callManage(t, h, ar(), "update", sharedBlock2[:13],
			map[string]any{"content": "partial update"})
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("update shared block by partial uuid: status %d resp %v — want success", status, resp)
		}
	})

	// delete is the same gate; a shared block must be archivable by its owner-key.
	t.Run("delete_shared_by_full_uuid", func(t *testing.T) {
		status, resp := callManage(t, h, ar(), "delete", sharedBlock, nil)
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("delete shared block: status %d resp %v — want success", status, resp)
		}
	})

	// Negative — no rights widening: 'work' is in allowed_scopes but is NOT a write
	// scope (store policy = home + shared only), so a work block stays untouchable.
	t.Run("update_work_block_stays_read_only", func(t *testing.T) {
		_, resp := callManage(t, h, ar(), "update", workBlock,
			map[string]any{"content": "should not apply"})
		if resp["success"] == true {
			t.Fatalf("update work block succeeded — 'work' must stay read-only (not a write scope); resp %v", resp)
		}
	})

	// Negative — a key WITHOUT 'shared' in allowed_scopes cannot write shared.
	t.Run("update_shared_denied_without_allowed", func(t *testing.T) {
		arNoShared := &auth.AuthResult{
			IsValid: true, HomeScope: "private",
			AllowedScopes: []string{}, ReadScopes: []string{"private"},
			TenantID: store.DefaultTenantID,
		}
		_, resp := callManage(t, h, arNoShared, "update", sharedBlock2,
			map[string]any{"content": "nope"})
		if resp["success"] == true {
			t.Fatalf("update shared block without shared-allowed succeeded — must be denied; resp %v", resp)
		}
	})
}
