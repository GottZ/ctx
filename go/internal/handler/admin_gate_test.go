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
)

// manageReqAs builds a /api/manage POST with the given auth result injected
// and returns the recorded response. Panics from nil-pool dereference are
// converted into t.Fatal — before the admin gate existed, several of these
// probes crashed INTO the store layer, which is exactly the red proof.
func manageReqAs(t *testing.T, ar *auth.AuthResult, body any) *httptest.ResponseRecorder {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	h := NewManageHandler(nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))

	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handler panicked (reached the store layer — no gate before it): %v", r)
		}
	}()
	h.HandleManage(rec, req)
	return rec
}

func nonAdminAR() *auth.AuthResult {
	return &auth.AuthResult{
		ApiKeyID:      "non-admin-key-id",
		HomeScope:     "private",
		AllowedScopes: []string{"shared"},
		ReadScopes:    []string{"private", "shared"},
		IsValid:       true,
		IsAdmin:       false,
	}
}

func adminAR() *auth.AuthResult {
	ar := nonAdminAR()
	ar.IsAdmin = true
	return ar
}

func assertForbiddenAdmin(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ok, _ := resp["success"].(bool); ok {
		t.Error("success = true on 403")
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "admin") {
		t.Errorf("error = %q, want it to mention 'admin'", errMsg)
	}
}

// SECURITY PROPERTY (K2/G03, BREAKING): the existing manage actions
// api-key-create/list/delete, mcp-client-*, and mutating dream-mode require
// an admin key. Before the gate, EVERY valid key of any home_scope could
// mint keys for arbitrary scopes (read access to foreign tenants), list or
// revoke all keys, manage MCP clients, and flip dream mode.
//
// Negative probe (2026-06-10): this test was written and run against the
// ungated HandleManage first — every subtest failed (nil-pool panic into the
// store layer or non-403 status), proving no gate existed in the chain.

func TestManageAdminGate_ApiKeyCreate_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "x", "home_scope": "tenant-a"},
	})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_ApiKeyList_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{"action": "api-key-list"})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_ApiKeyDelete_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "api-key-delete",
		"data":   map[string]any{"id": "0000-irrelevant"},
	})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_MCPClientCreate_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "mcp-client-create",
		"data":   map[string]any{"label": "x"},
	})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_MCPClientList_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{"action": "mcp-client-list"})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_MCPClientDelete_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "mcp-client-delete",
		"data":   map[string]any{"client_id": "x"},
	})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_DreamModeMutation_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "dream-mode",
		"data":   map[string]any{"mode": "off"},
	})
	assertForbiddenAdmin(t, rec)
}

// gaming-mode mutation is admin-gated (design 03 §2.6): an ungated toggle
// would let any tenant key flip the whole system's egress topology (herbert
// out ⇒ synthesis goes external via OpenRouter — cost + egress-character
// change by a non-admin). The gate fires before any cfg/pool access, so a
// nil-everything handler is sufficient to probe it.
func TestManageAdminGate_GamingModeMutation_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "gaming-mode",
		"data":   map[string]any{"mode": "on"},
	})
	assertForbiddenAdmin(t, rec)
}

// blocks-audit-* in full (G41): start triggers bulk sensitivity downgrades
// (the opsec direction), status discloses block titles + classification
// topology. Both gated — including the read.
func TestManageAdminGate_BlocksAuditStart_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{
		"action": "blocks-audit-start",
		"data":   map[string]any{"dry_run": true},
	})
	assertForbiddenAdmin(t, rec)
}

func TestManageAdminGate_BlocksAuditStatus_NonAdmin403(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{"action": "blocks-audit-status"})
	assertForbiddenAdmin(t, rec)
}

// Admin keys pass the gate; nil auditController yields 503 — past the gate,
// before any scheduler/store access.
func TestManageAdminGate_BlocksAudit_AdminPassesGate(t *testing.T) {
	rec := manageReqAs(t, adminAR(), map[string]any{"action": "blocks-audit-status"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (nil auditController)", rec.Code)
	}
}

// dream-mode WITHOUT data is a read (current mode) and stays open to every
// valid key — K2 gates only the mutating shape. nil dreamController yields
// 503 here, which proves the request passed the admin gate.
func TestManageAdminGate_DreamModeRead_NonAdminAllowed(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{"action": "dream-mode"})
	if rec.Code == http.StatusForbidden {
		t.Fatalf("dream-mode read got 403 — read path must stay non-admin (K2: mutating only)")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (nil dreamController)", rec.Code)
	}
}

// Admin keys pass the gate and reach the validation layer (400 = past gate).
func TestManageAdminGate_ApiKeyCreate_AdminPassesGate(t *testing.T) {
	rec := manageReqAs(t, adminAR(), map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "", "home_scope": "tenant-a"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (label validation, i.e. past the admin gate)", rec.Code)
	}
}

// Non-admin-gated actions remain reachable for plain valid keys (stats needs
// a pool, but validation-only paths prove dispatch is unchanged).
func TestManageAdminGate_GetWithoutID_NonAdminStill400(t *testing.T) {
	rec := manageReqAs(t, nonAdminAR(), map[string]any{"action": "get"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (missing id) — non-gated actions must not require admin", rec.Code)
	}
}

// SECURITY PROPERTY (G03): scope names with a leading '_' are SYSTEM-
// RESERVED ('_global' is the settings identity sentinel, 051). api-key-create
// must reject them, otherwise a key with home_scope='_global' would collide
// with the global settings identity once per-tenant resolution lands.
//
// Negative probe (2026-06-10): red before the gate — the request fell
// through to store.CreateApiKey (nil-pool panic), proving nothing rejected
// the reserved prefix.

func TestApiKeyCreate_ReservedHomeScope400(t *testing.T) {
	rec := manageReqAs(t, adminAR(), map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "x", "home_scope": "_x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for home_scope '_x'", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "reserved") {
		t.Errorf("error = %q, want it to mention 'reserved'", errMsg)
	}
}

func TestApiKeyCreate_ReservedGlobalSentinel400(t *testing.T) {
	rec := manageReqAs(t, adminAR(), map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "x", "home_scope": "_global"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for home_scope '_global'", rec.Code)
	}
}

func TestApiKeyCreate_ReservedAllowedScope400(t *testing.T) {
	rec := manageReqAs(t, adminAR(), map[string]any{
		"action": "api-key-create",
		"data": map[string]any{
			"label":          "x",
			"home_scope":     "tenant-a",
			"allowed_scopes": []string{"shared", "_x"},
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for '_x' in allowed_scopes", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "reserved") {
		t.Errorf("error = %q, want it to mention 'reserved'", errMsg)
	}
}

// --- RequireAdmin middleware (Gate-Helfer for the settings/secrets routes;
// G03 ships it, the API waves mount it) ---.

func TestRequireAdmin_NonAdmin403(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, nonAdminAR()))
	rec := httptest.NewRecorder()
	RequireAdmin(next).ServeHTTP(rec, req)
	assertForbiddenAdmin(t, rec)
}

func TestRequireAdmin_NoAuthResult403(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	RequireAdmin(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without auth result", rec.Code)
	}
}

func TestRequireAdmin_Admin200(t *testing.T) {
	var reached bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, adminAR()))
	rec := httptest.NewRecorder()
	RequireAdmin(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !reached {
		t.Errorf("status = %d, reached = %v, want 200 + handler reached", rec.Code, reached)
	}
}

// --- RequireAdminOrTenantAdmin middleware (T37b/04-W5: admits server-admins AND
// tenant-admins of their own tenant; the handler then scopes the payload) ---.

func serveGateOrTenant(t *testing.T, ar *auth.AuthResult) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/llmlog", nil)
	if ar != nil {
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	}
	rec := httptest.NewRecorder()
	RequireAdminOrTenantAdmin(next).ServeHTTP(rec, req)
	return rec
}

func TestRequireAdminOrTenantAdmin(t *testing.T) {
	cases := []struct {
		name string
		ar   *auth.AuthResult
		want int
	}{
		{"server-admin", adminAR(), http.StatusOK},
		{"tenant-admin owner", tenantAdminAR(auth.RoleOwner), http.StatusOK},
		{"tenant-admin admin", tenantAdminAR(auth.RoleAdmin), http.StatusOK},
		{"member", memberAR(), http.StatusForbidden},
		{"valid non-admin no tenant", nonAdminAR(), http.StatusForbidden},
		{"no auth result", nil, http.StatusForbidden},
	}
	for _, c := range cases {
		if got := serveGateOrTenant(t, c.ar).Code; got != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, got, c.want)
		}
	}
}
