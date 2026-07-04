//go:build integration

// I-F manage-transport gates (design/02 §4.3/§5.1): the forge-* family through
// the PRODUCTION HandleManage chain (tier gate + dispatch + ownership + error
// mapping + token non-leak). The sync ENGINE is proven in internal/forge; here
// the ForgeController is a mock so the handler contract is isolated.
//
//	go test -tags=integration ./internal/handler/ -run TestForgeManage -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type mockForge struct {
	startErr    error
	startStatus forge.SyncStatus
	setTokenErr error
	tokenGot    string
}

func (m *mockForge) StartSync(_ context.Context, p store.ProjectRow, _ bool) (forge.SyncStatus, error) {
	if m.startErr != nil {
		return forge.SyncStatus{}, m.startErr
	}
	m.startStatus.ProjectID = p.ID
	return m.startStatus, nil
}
func (m *mockForge) Status(id string) forge.SyncStatus { return forge.SyncStatus{ProjectID: id} }
func (m *mockForge) SetToken(_ context.Context, _ store.ProjectRow, pt string) error {
	m.tokenGot = pt
	return m.setTokenErr
}

func forgeManageDo(t *testing.T, pool *pgxpool.Pool, fc ForgeController, ar *auth.AuthResult, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	h.SetForgeController(fc)
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if ar != nil {
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	}
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec
}

func seedForgeProject(t *testing.T, pool *pgxpool.Pool, slug string) (tenantID string, proj store.ProjectRow) {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	row, _, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID: tn.ID, ScopeName: tn.Slug + ":repo", Identity: "github:a/" + slug,
		Forge: json.RawMessage(`{"kind":"github","owner":"o","repo":"r"}`),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return tn.ID, *row
}

func TestForgeManage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	tenantID, proj := seedForgeProject(t, pool, "ifmanage")
	admin := w4TenantAdmin(tenantID, proj.Scope)

	// Member (non-admin) is rejected by the tier gate BEFORE dispatch (403).
	t.Run("MemberRejectedByTier", func(t *testing.T) {
		rec := forgeManageDo(t, pool, &mockForge{}, memberAR(), map[string]any{
			"action": "forge-sync-start", "data": map[string]any{"project_id": proj.ID},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member: status %d, want 403 (tier gate)", rec.Code)
		}
	})

	// A foreign / absent project id is a uniform 404 (no existence oracle).
	t.Run("ForeignProject404", func(t *testing.T) {
		other := w4TenantAdmin("00000000-0000-0000-0000-000000000000", "x:y")
		rec := forgeManageDo(t, pool, &mockForge{}, other, map[string]any{
			"action": "forge-sync-status", "data": map[string]any{"project_id": proj.ID},
		})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign: status %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// sync-start success ⇒ 200 with the run.
	t.Run("StartSuccess", func(t *testing.T) {
		rec := forgeManageDo(t, pool, &mockForge{startStatus: forge.SyncStatus{Running: true}}, admin, map[string]any{
			"action": "forge-sync-start", "data": map[string]any{"project_id": proj.ID},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("start: status %d (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// Double-start ⇒ 409 (the S7 signal surfaced by the engine).
	t.Run("DoubleStart409", func(t *testing.T) {
		rec := forgeManageDo(t, pool, &mockForge{startErr: forge.ErrSyncRunning}, admin, map[string]any{
			"action": "forge-sync-start", "data": map[string]any{"project_id": proj.ID},
		})
		if rec.Code != http.StatusConflict {
			t.Fatalf("double-start: status %d, want 409", rec.Code)
		}
	})

	// No issue policy ⇒ 422 with the clear §6.4 message.
	t.Run("PolicyRefused422", func(t *testing.T) {
		rec := forgeManageDo(t, pool, &mockForge{startErr: forge.ErrIssuePolicy}, admin, map[string]any{
			"action": "forge-sync-start", "data": map[string]any{"project_id": proj.ID},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("policy: status %d, want 422", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "issue type policy") {
			t.Fatalf("policy refusal lacks a clear message: %s", rec.Body.String())
		}
	})

	// token-set ⇒ 200 token_set:true, and the PAT never appears in the response.
	t.Run("TokenSetNoLeak", func(t *testing.T) {
		mf := &mockForge{}
		const pat = "ghp_supersecretpattoken"
		rec := forgeManageDo(t, pool, mf, admin, map[string]any{
			"action": "forge-token-set", "data": map[string]any{"project_id": proj.ID, "token": pat},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("token-set: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		if mf.tokenGot != pat {
			t.Fatalf("controller did not receive the token")
		}
		if strings.Contains(rec.Body.String(), pat) {
			t.Fatalf("PAT leaked into token-set response: %s", rec.Body.String())
		}
	})

	// sync-status exposes token_set=bool (never the ref/PAT). Seed the ref column
	// directly (the mock does not persist) and scan the body.
	t.Run("StatusTokenSetBoolOnly", func(t *testing.T) {
		const secretName = "forge.token." + "seedref"
		if _, err := pool.Exec(context.Background(),
			`UPDATE context_projects SET token_secret = $2 WHERE id = $1::uuid`, proj.ID, secretName); err != nil {
			t.Fatalf("seed token ref: %v", err)
		}
		rec := forgeManageDo(t, pool, &mockForge{}, admin, map[string]any{
			"action": "forge-sync-status", "data": map[string]any{"project_id": proj.ID},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d (body=%s)", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp["token_set"] != true {
			t.Fatalf("token_set = %v, want true", resp["token_set"])
		}
		if strings.Contains(rec.Body.String(), secretName) {
			t.Fatalf("secret ref name leaked into status: %s", rec.Body.String())
		}
	})

	// 503 when the engine is not wired.
	t.Run("NoEngine503", func(t *testing.T) {
		h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil) // no SetForgeController
		raw, _ := json.Marshal(map[string]any{"action": "forge-sync-status", "data": map[string]any{"project_id": proj.ID}})
		req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, admin))
		rec := httptest.NewRecorder()
		h.HandleManage(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("no-engine: status %d, want 503", rec.Code)
		}
	})
}
