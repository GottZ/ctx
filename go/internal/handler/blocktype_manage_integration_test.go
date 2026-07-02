//go:build integration

// WF T10 integration gates (design/01 §7-T10) against a real PG18
// testcontainer: the manage type-* family end-to-end through HandleManage
// (tier gate → dispatch → store → 072 audit trigger), the store/update
// `type` field with the manual-override roundtrip (T4 semantics), and the
// attribution probe (via='api' + api_key_id — RED with a plain pool.Exec,
// which the 072 trigger records as via='sql'/NULL).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestTypeManage -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// typeManageReq drives HandleManage with a real pool handler and ar in the
// auth context (the manageReqAs shape, minus the nil-pool panic guard).
func typeManageReq(t *testing.T, h *ManageHandler, ar *auth.AuthResult, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec
}

func decodeResp(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return resp
}

// lastTypeAudit reads the newest context_settings_audit row for one type name.
func lastTypeAudit(t *testing.T, pool *pgxpool.Pool, name string) (action string, apiKeyID, actorLabel, via, requestID *string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT action, api_key_id::text, actor_label, metadata->>'via', metadata->>'request_id'
		   FROM context_settings_audit
		  WHERE entity_type = 'block_type' AND entity_key = $1
		  ORDER BY created_at DESC, id DESC LIMIT 1`, name).
		Scan(&action, &apiKeyID, &actorLabel, &via, &requestID)
	if err != nil {
		t.Fatalf("read type audit for %q: %v", name, err)
	}
	return action, apiKeyID, actorLabel, via, requestID
}

func TestTypeManage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	actor, _, err := store.CreateApiKey(ctx, pool, "type-it-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}
	adminAR := &auth.AuthResult{
		IsValid: true, IsAdmin: true, ApiKeyID: actor.ID,
		HomeScope: "private", ReadScopes: []string{"private"},
	}
	memberAR := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID,
		HomeScope: "private", ReadScopes: []string{"private"},
		TenantID: "tenant-a", TenantRole: auth.RoleMember,
	}

	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	t.Run("member_type_create_403", func(t *testing.T) {
		rec := typeManageReq(t, h, memberAR, map[string]any{
			"action": "type-create",
			"data":   map[string]any{"name": "issue-probe", "config": map[string]any{"v": 1}},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member type-create: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("create_valid_and_audit_via_api", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-create",
			"data": map[string]any{
				"name":         "issue-probe",
				"display_name": "Issue (Probe)",
				"description":  "T10 gate fixture",
				"config": map[string]any{
					"v":         1,
					"retrieval": map[string]any{"policy": "full-pass"},
					"digest":    map[string]any{"include": false},
				},
			},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("type-create: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		resp := decodeResp(t, rec)
		typ, _ := resp["type"].(map[string]any)
		if typ["name"] != "issue-probe" || typ["scope"] != "_global" || typ["builtin"] != false {
			t.Errorf("created row = %v, want name=issue-probe scope=_global builtin=false", typ)
		}

		// Attribution probe (§3.2 R1): the 072 trigger MUST see the tx GUCs —
		// via='api' + api_key_id + actor_label. RED with plain pool.Exec.
		action, apiKeyID, actorLabel, via, requestID := lastTypeAudit(t, pool, "issue-probe")
		if action != "insert" {
			t.Errorf("audit action = %q, want insert", action)
		}
		if apiKeyID == nil || *apiKeyID != actor.ID {
			t.Errorf("audit api_key_id = %v, want %s", apiKeyID, actor.ID)
		}
		if actorLabel == nil || *actorLabel != "type-it-actor" {
			t.Errorf("audit actor_label = %v, want type-it-actor", actorLabel)
		}
		if via == nil || *via != "api" {
			t.Errorf("audit via = %v, want api (plain Exec would record sql/NULL)", via)
		}
		_ = requestID // empty in httptest (no request-id middleware) — via/api_key_id carry the probe

		// Sync-reload probe: the registry resolves the new type at once.
		if _, ok := reg.Snapshot().Resolve("issue-probe"); !ok {
			t.Errorf("registry snapshot does not resolve issue-probe after create (sync reload missing)")
		}
	})

	t.Run("create_invalid_config_422_with_field_path", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-create",
			"data": map[string]any{
				"name":   "typo-probe",
				"config": map[string]any{"v": 1, "guards": map[string]any{"check": false}},
			},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("typo config: status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "guards") {
			t.Errorf("422 error does not name the offending key: %s", body)
		}
		// Cross-field rule through the SAME authority: damped without factor.
		rec = typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-create",
			"data": map[string]any{
				"name":   "damped-probe",
				"config": map[string]any{"v": 1, "retrieval": map[string]any{"policy": "damped"}},
			},
		})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "damping_factor") {
			t.Fatalf("damped without factor: status = %d body = %s, want 422 naming damping_factor", rec.Code, rec.Body.String())
		}
	})

	t.Run("create_caps_422", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-create",
			"data": map[string]any{
				"name":         "caps-probe",
				"display_name": strings.Repeat("x", 201),
				"config":       map[string]any{"v": 1},
			},
		})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "display_name") {
			t.Fatalf("display_name cap: status = %d body = %s, want 422 naming display_name", rec.Code, rec.Body.String())
		}
	})

	t.Run("create_foreign_scope_422", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-create",
			"data":   map[string]any{"name": "scope-probe", "scope": "private", "config": map[string]any{"v": 1}},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("non-_global scope: status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("list_open_and_scope_filtered", func(t *testing.T) {
		rec := typeManageReq(t, h, memberAR, map[string]any{"action": "type-list"})
		if rec.Code != http.StatusOK {
			t.Fatalf("member type-list: status = %d (open read)", rec.Code)
		}
		resp := decodeResp(t, rec)
		types, _ := resp["types"].([]any)
		names := map[string]bool{}
		for _, x := range types {
			row := x.(map[string]any)
			names[row["name"].(string)] = true
			if row["scope"] != "_global" {
				t.Errorf("type-list leaked non-_global row: %v (K-T1 scope filter)", row)
			}
		}
		for _, want := range []string{"knowledge", "reference", "audit-trail", "system-meta", "issue-probe"} {
			if !names[want] {
				t.Errorf("type-list missing %q (got %v)", want, names)
			}
		}
	})

	t.Run("get_by_name_and_404", func(t *testing.T) {
		rec := typeManageReq(t, h, memberAR, map[string]any{"action": "type-get", "id": "issue-probe"})
		if rec.Code != http.StatusOK {
			t.Fatalf("type-get by name: status = %d", rec.Code)
		}
		rec = typeManageReq(t, h, memberAR, map[string]any{"action": "type-get", "id": "does-not-exist"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("type-get missing: status = %d, want 404", rec.Code)
		}
	})

	t.Run("update_config_and_registry_visible", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-update", "id": "audit-trail",
			"data": map[string]any{"config": map[string]any{
				"v":         1,
				"retrieval": map[string]any{"policy": "damped", "damping_factor": 0.5},
			}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("type-update builtin config: status = %d (body %s) — builtin CONFIG must be editable", rec.Code, rec.Body.String())
		}
		p, ok := reg.Snapshot().Resolve("audit-trail")
		if !ok || p.Retrieval.DampingFactor != 0.5 {
			t.Errorf("registry after update: factor = %v ok=%v, want 0.5 (sync reload)", p.Retrieval.DampingFactor, ok)
		}
		action, _, _, via, _ := lastTypeAudit(t, pool, "audit-trail")
		if action != "update" || via == nil || *via != "api" {
			t.Errorf("update audit = (%q, %v), want (update, api)", action, via)
		}
	})

	t.Run("update_strict_payload_rejects_identity_fields", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{
			"action": "type-update", "id": "issue-probe",
			"data": map[string]any{"name": "renamed"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("rename attempt: status = %d, want 400 (strict decode — identity is immutable)", rec.Code)
		}
	})

	t.Run("delete_builtin_409", func(t *testing.T) {
		rec := typeManageReq(t, h, adminAR, map[string]any{"action": "type-delete", "id": "knowledge"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("builtin delete: status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_block_types WHERE name='knowledge'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("knowledge row after rejected delete: n=%d err=%v", n, err)
		}
	})

	t.Run("delete_in_use_409_names_archived_split", func(t *testing.T) {
		// One ACTIVE and one ARCHIVED reference — the archived one alone must
		// also block (§5.1(c) R1: unarchive would resurface an orphan).
		for i, archived := range []bool{false, true} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (category, title, content, scope, type_name, is_archived)
				 VALUES ('t10', $1, 'c', 'private', 'issue-probe', $2)`,
				fmt.Sprintf("ref-%d", i), archived); err != nil {
				t.Fatalf("insert ref block: %v", err)
			}
		}
		rec := typeManageReq(t, h, adminAR, map[string]any{"action": "type-delete", "id": "issue-probe"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("in-use delete: status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "1 active") || !strings.Contains(body, "1 archived") {
			t.Errorf("in-use error does not name the active/archived split: %s", body)
		}
	})

	t.Run("delete_unreferenced_ok", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE type_name='issue-probe'`); err != nil {
			t.Fatalf("clear refs: %v", err)
		}
		rec := typeManageReq(t, h, adminAR, map[string]any{"action": "type-delete", "id": "issue-probe"})
		if rec.Code != http.StatusOK {
			t.Fatalf("delete: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		if _, ok := reg.Snapshot().Resolve("issue-probe"); ok {
			t.Errorf("registry still resolves issue-probe after delete (sync reload missing)")
		}
		action, _, _, via, _ := lastTypeAudit(t, pool, "issue-probe")
		if action != "delete" || via == nil || *via != "api" {
			t.Errorf("delete audit = (%q, %v), want (delete, api)", action, via)
		}
	})
}

// TestTypeManage_StoreTypeField_Integration: the store/update `type` field
// (WF T10 #5) + the manual-override roundtrip (T4 semantics): an explicit
// type sets type_source='manual', and NO follow-up edit re-classifies.
func TestTypeManage_StoreTypeField_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	actor, _, err := store.CreateApiKey(ctx, pool, "type-store-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}
	ar := &auth.AuthResult{
		IsValid: true, IsAdmin: true, ApiKeyID: actor.ID,
		HomeScope: "private", ReadScopes: []string{"private"},
	}

	sh := NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{}}, reg)
	mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	postStore := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw)))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		sh.HandleStore(rec, req)
		return rec
	}
	typeOf := func(id string) (name, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).Scan(&name, &source); err != nil {
			t.Fatalf("read type: %v", err)
		}
		return name, source
	}

	t.Run("store_unknown_type_422", func(t *testing.T) {
		rec := postStore(map[string]any{
			"category": "t10", "title": "Unknown Type", "content": "c", "type": "no-such-type",
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("unknown type: status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}
	})

	var blockID string
	t.Run("store_with_type_sets_manual", func(t *testing.T) {
		rec := postStore(map[string]any{
			"category": "t10", "title": "Manual Typed", "content": "plain content", "type": "reference",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("store: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Block struct {
				ID             string `json:"id"`
				TypeName       string `json:"type"`
				TypeSource     string `json:"type_source"`
				LifecycleState string `json:"lifecycle_state"`
			} `json:"block"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		blockID = resp.Block.ID
		if resp.Block.TypeName != "reference" || resp.Block.TypeSource != "manual" || resp.Block.LifecycleState != "knowledge" {
			t.Errorf("response block = %+v, want type=reference type_source=manual lifecycle_state=knowledge", resp.Block)
		}
	})

	t.Run("content_update_does_not_reclassify", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"content": "session handover audit welle recurrent"})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
		mh.handleUpdate(rec, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)), ar,
			manageRequest{Action: "update", ID: blockID, Data: raw})
		if rec.Code != http.StatusOK {
			t.Fatalf("update: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		if name, source := typeOf(blockID); name != "reference" || source != "manual" {
			t.Errorf("after content update: (%q, %q), want (reference, manual)", name, source)
		}
	})

	t.Run("title_pattern_update_does_not_reclassify_manual", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"title": "Session 99 Handover Protokoll"})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
		mh.handleUpdate(rec, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)), ar,
			manageRequest{Action: "update", ID: blockID, Data: raw})
		if rec.Code != http.StatusOK {
			t.Fatalf("update: status = %d", rec.Code)
		}
		if name, source := typeOf(blockID); name != "reference" || source != "manual" {
			t.Errorf("after audit-pattern title update: (%q, %q), want (reference, manual) — manual wins permanently", name, source)
		}
	})

	t.Run("manage_update_type_field", func(t *testing.T) {
		// Unknown name rejects (registry validation in the update path).
		raw, _ := json.Marshal(map[string]any{"type": "no-such-type"})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
		mh.handleUpdate(rec, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)), ar,
			manageRequest{Action: "update", ID: blockID, Data: raw})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("unknown type on update: status = %d, want 422", rec.Code)
		}
		// Known name re-types with manual provenance.
		raw, _ = json.Marshal(map[string]any{"type": "audit-trail"})
		rec = httptest.NewRecorder()
		mh.handleUpdate(rec, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)), ar,
			manageRequest{Action: "update", ID: blockID, Data: raw})
		if rec.Code != http.StatusOK {
			t.Fatalf("type update: status = %d (body %s)", rec.Code, rec.Body.String())
		}
		if name, source := typeOf(blockID); name != "audit-trail" || source != "manual" {
			t.Errorf("after type update: (%q, %q), want (audit-trail, manual)", name, source)
		}
	})
}
