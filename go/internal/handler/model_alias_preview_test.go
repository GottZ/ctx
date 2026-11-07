package handler

// Unit gates of model-alias-preview (wave W05-04, design/05 §4.3): the tier
// (server-admin only, B-8), the request validation, the role mode
// ("suggest from pool", design/05 §3.3), the consumer mapping and the
// preview context. The chain mode end to end is the integration test
// (model_alias_preview_integration_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
)

// previewHandler is a store-less ManageHandler with model-alias-preview
// wired to fakes: the chain previewer counts its calls, the role source
// knows chat only.
func previewHandler(calls *int) *ManageHandler {
	h := NewManageHandler(nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetChainPreview(func(context.Context, *auth.ConsumerAuth, string) (oai.ChainPreview, error) {
		*calls++
		return oai.ChainPreview{Chain: []oai.PreviewRow{}, Excluded: []oai.PreviewExclusion{}, Notes: []string{}}, nil
	}, func(role string) string {
		if role == "chat" {
			return "qwen38-27b"
		}
		return ""
	})
	return h
}

func previewCall(t *testing.T, h *ManageHandler, ar *auth.AuthResult, data any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"action": "model-alias-preview", "data": data})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("model-alias-preview panicked: %v", r)
		}
	}()
	h.HandleManage(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// TestModelAliasPreviewTierServerAdminOnly is gate W05-04 (c): a tenant-admin
// (owner or admin of its tenant) gets 403 admin key required, the
// server-admin gets the answer — both halves, so neither an action that
// refuses everybody nor one open to tenant-admins passes. Red against the
// row at tierTenantAdmin (mutant mut/preview-tier-tenant-admin.md) and
// against the stub (503 for the server-admin).
func TestModelAliasPreviewTierServerAdminOnly(t *testing.T) {
	var calls int
	h := previewHandler(&calls)
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		status, resp := previewCall(t, h, tenantAdminAR(role), map[string]any{"role": "chat"})
		if status != http.StatusForbidden || resp["error"] != "admin key required" {
			t.Errorf("W05-04 tier: tenant-%s → %d %v, want 403 admin key required", role, status, resp)
		}
	}
	status, resp := previewCall(t, h, adminAR(), map[string]any{"role": "chat"})
	if status != http.StatusOK || resp["success"] != true || resp["primary_model"] != "qwen38-27b" {
		t.Errorf("W05-04 tier: server-admin → %d %v, want 200 with primary_model", status, resp)
	}
}

// TestModelAliasPreviewValidation: every malformed request is a 400 with
// success:false before any read — neither mode, both modes, chain mode
// without consumer_id, an unknown role, an unknown field (B-18: no free
// sensitivity parameter exists, a "sensitivity" field is refused). Red
// against the stub (503) and against a handler that ignores unknown fields.
func TestModelAliasPreviewValidation(t *testing.T) {
	var calls int
	h := previewHandler(&calls)
	bad := []map[string]any{
		{},
		{"alias": "chat", "role": "chat", "consumer_id": "00000000-0000-4000-8000-0000000000c1"},
		{"alias": "chat"},
		{"role": "vision"},
		{"alias": "chat", "consumer_id": "00000000-0000-4000-8000-0000000000c1", "sensitivity": "public"},
		{"role": "chat", "consumer_id": "00000000-0000-4000-8000-0000000000c1"},
	}
	for _, data := range bad {
		status, resp := previewCall(t, h, adminAR(), data)
		if status != http.StatusBadRequest || resp["success"] != false {
			t.Errorf("W05-04 validation: %v → %d %v, want 400 success:false", data, status, resp)
		}
	}
	if calls != 0 {
		t.Errorf("W05-04 validation: previewer called %d times for malformed requests, want 0", calls)
	}
}

// TestModelAliasPreviewRoleMode: the suggestion mode answers the pool's
// primary model of the role, "" when the pool offers none (nothing
// offered, fail-closed, design/02 E-02-13) — never the chain previewer.
func TestModelAliasPreviewRoleMode(t *testing.T) {
	var calls int
	h := previewHandler(&calls)
	for role, want := range map[string]string{"chat": "qwen38-27b", "rerank": ""} {
		status, resp := previewCall(t, h, adminAR(), map[string]any{"role": role})
		if status != http.StatusOK || resp["success"] != true || resp["role"] != role || resp["primary_model"] != want {
			t.Errorf("W05-04 role mode %s: %d %v, want 200 primary_model %q", role, status, resp, want)
		}
	}
	if calls != 0 {
		t.Errorf("W05-04 role mode: chain previewer called %d times, want 0", calls)
	}
}

// TestModelAliasPreviewUnwired: without SetChainPreview the action is 503
// service_unavailable — never a chain from a half-wired server. A pin of the
// fail-closed default (green against the stub by construction).
func TestModelAliasPreviewUnwired(t *testing.T) {
	h := NewManageHandler(nil, nil, nil, nil, nil, nil, nil, nil)
	status, resp := previewCall(t, h, adminAR(), map[string]any{"role": "chat"})
	if status != http.StatusServiceUnavailable || resp["code"] != "service_unavailable" || resp["success"] != false {
		t.Errorf("W05-04 unwired: %d %v, want 503 service_unavailable", status, resp)
	}
}

// TestPreviewConsumerAuthMapping: the consumer a preview runs for carries the
// columns the door's ctx_model_auth row would — valid, tenant, scope, kind,
// all_models, floor, weight, lane, peer binding — and no key id. Red
// against the stub (nil).
func TestPreviewConsumerAuthMapping(t *testing.T) {
	scope, peer := "acme:main", "0123456789abcdef0123456789abcdef"
	c := store.ModelConsumer{ID: "c1", TenantID: "t1", Kind: "ctx-peer", TenantScope: &scope, AllModels: true,
		PromptSensitivity: "personal", FairWeight: 7, AdmissionLane: "urgent", Active: true, PeerInstanceID: &peer}
	got := previewConsumerAuth(c)
	want := &auth.ConsumerAuth{ConsumerID: "c1", TenantID: "t1", TenantScope: scope, Kind: auth.KindCtxPeer, AllModels: true,
		PromptSensitivity: "personal", FairWeight: 7, AdmissionLane: "urgent", IsValid: true, PeerInstanceID: peer}
	if got == nil || *got != *want {
		t.Errorf("W05-04 consumer mapping: %+v, want %+v", got, want)
	}
	c.TenantScope, c.PeerInstanceID, c.Kind = nil, nil, "app"
	if got := previewConsumerAuth(c); got == nil || got.TenantScope != "" || got.Kind != auth.KindApp || got.PeerInstanceID != "" {
		t.Errorf("W05-04 consumer mapping without scope: %+v, want empty scope, app, unbound", got)
	}
}

// TestPreviewContextIsTheConsumers: the preview context carries the consumer
// in the door's key, NOT the admin's knowledge identity (with both,
// RequestTenantScope stays on the base snapshot and the tenant overlay of the
// preview differs from the request's), and the PeerIngress policy of a
// headerless request. Red against the stub (ctx unchanged: the admin's home
// scope, no consumer, the default policy).
func TestPreviewContextIsTheConsumers(t *testing.T) {
	admin := context.WithValue(context.Background(), authResultKey, adminAR())
	ca := &auth.ConsumerAuth{ConsumerID: "c1", TenantID: "t1", TenantScope: "acme:main", Kind: auth.KindApp, IsValid: true,
		PromptSensitivity: string(backends.SensPublic)}
	ctx := previewContext(admin, ca)
	var problems []string
	if AuthResultFromContext(ctx) != nil {
		problems = append(problems, "the admin's AuthResult is still visible")
	}
	if ConsumerAuthFromContext(ctx) != ca {
		problems = append(problems, "the consumer is not in the door's key")
	}
	if s := RequestTenantScope(ctx); s != "acme:main" {
		problems = append(problems, "RequestTenantScope = "+s)
	}
	if p := federation.PolicyFrom(ctx); p.Required != backends.SensPublic || p.EgressLocal {
		problems = append(problems, "policy "+string(p.Required))
	}
	if len(problems) > 0 {
		t.Errorf("W05-04 preview context: %s", strings.Join(problems, "; "))
	}
}
