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
	"github.com/GottZ/ctx/internal/backends"
)

// manageReqWithPool mirrors manageReqAs but wires a (DB-less) backend pool —
// the confirm/validation paths run BEFORE any transaction, so a nil pgx pool
// only panics when a probe wrongly reaches the store layer (red proof, same
// doctrine as admin_gate_test.go).
func manageReqWithPool(t *testing.T, ar *auth.AuthResult, bp *backends.Pool, body any) *httptest.ResponseRecorder {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	h := NewManageHandler(nil, nil, nil, bp, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("handler panicked (reached the store layer — a gate is missing before it): %v", r)
		}
	}()
	h.HandleManage(rec, req)
	return rec
}

// TestBackendActionsRequireAdmin: every backend-* action (reads INCLUDED —
// the list discloses egress topology) answers 403 to a valid non-admin key.
// Probed against a nil store pool: reaching the store would panic, which is
// exactly the missing-gate proof.
func TestBackendActionsRequireAdmin(t *testing.T) {
	for _, action := range []string{
		"backend-create", "backend-update", "backend-delete", "backend-list", "backend-test",
	} {
		rec := manageReqWithPool(t, nonAdminAR(), nil, map[string]any{
			"action": action, "id": "0190", "data": map[string]any{"name": "x"},
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with non-admin key: status %d, want 403", action, rec.Code)
		}
	}
}

// TestBackendCreateConfirmRequired: creating above the public DDL default
// without confirm_trust_elevation is a 400 — without the create-side gate,
// the update confirm would be trivially bypassed via direct create or
// delete+create (design §3.4 point 6).
func TestBackendCreateConfirmRequired(t *testing.T) {
	spec := map[string]any{
		"name": "sneaky", "base_url": "https://api.example.com/v1",
		"trust": "full-trust",
		"roles": []string{"synthesis"}, "model_map": map[string]any{"default": "m"},
	}
	rec := manageReqWithPool(t, adminAR(), backends.NewPool(nil, nil), map[string]any{
		"action": "backend-create", "data": spec,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create without confirm: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "confirm_trust_elevation") {
		t.Fatalf("error does not name the confirm flag: %s", rec.Body.String())
	}

	// public trust (the default) needs no confirm — but then trips the
	// VALIDATION (not the store: nil pgx pool would panic) only if invalid.
	// A fully valid public create WOULD reach the store, so this probe stops
	// at a validation error by omitting model coverage.
	spec["trust"] = "public"
	spec["model_map"] = map[string]any{}
	rec = manageReqWithPool(t, adminAR(), backends.NewPool(nil, nil), map[string]any{
		"action": "backend-create", "data": spec,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("public create with bad model_map: status %d, want 422", rec.Code)
	}
}

// TestBackendUpdateConfirmRequired: raising trust toward full-trust without
// the confirm flag is 400; lowering is free. Seeded snapshot, no DB.
func TestBackendUpdateConfirmRequired(t *testing.T) {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "0190-test", Name: "cloud", Host: "https://api.example.com/v1",
		Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderGeneric,
		Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
		Roles:    []string{backends.RoleSynthesis},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Enabled:  true,
	}})

	rec := manageReqWithPool(t, adminAR(), bp, map[string]any{
		"action": "backend-update", "id": "0190-test",
		"data": map[string]any{"trust": "full-trust"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("elevation without confirm: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "confirm_trust_elevation") {
		t.Fatalf("error does not name the confirm flag: %s", rec.Body.String())
	}

	// Unknown id: 404 before any trust logic.
	rec = manageReqWithPool(t, adminAR(), bp, map[string]any{
		"action": "backend-update", "id": "missing",
		"data": map[string]any{"enabled": false},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status %d, want 404", rec.Code)
	}
}

// TestBackendCreateValidation422: the 3.4 validation classes surface as 422
// with field errors — denylist header, bad locality, embed+external.
func TestBackendCreateValidation422(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		hint string
	}{
		{"credential header", map[string]any{
			"name": "x", "base_url": "https://api.example.com/v1",
			"roles": []string{"synthesis"}, "model_map": map[string]any{"default": "m"},
			"extra_headers": map[string]string{"Authorization": "Bearer sk-…"},
		}, "api_key_ref"},
		{"public host declared lan", map[string]any{
			"name": "x", "base_url": "https://api.example.com/v1", "locality": "lan",
			"roles": []string{"synthesis"}, "model_map": map[string]any{"default": "m"},
		}, "external"},
		{"external embed without proof", map[string]any{
			"name": "x", "base_url": "https://api.example.com/v1",
			"roles": []string{"embed"}, "model_map": map[string]any{"default": "m"},
		}, "embed_equivalence_verified"},
	}
	for _, c := range cases {
		rec := manageReqWithPool(t, adminAR(), backends.NewPool(nil, nil), map[string]any{
			"action": "backend-create", "data": c.data,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422 (body %s)", c.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), c.hint) {
			t.Errorf("%s: error lacks hint %q: %s", c.name, c.hint, rec.Body.String())
		}
	}
}

// TestBackendListNeverLeaksResolvedKeys: the list view carries api_key_ref
// (the name) and never a resolved key value.
func TestBackendListNeverLeaksResolvedKeys(t *testing.T) {
	const resolved = "sk-resolved-0123456789abcdef"
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "0190-test", Name: "cloud", Host: "https://api.example.com/v1",
		Protocol: backends.ProtocolOpenAI, Trust: backends.TrustNoCredentials,
		Locality: backends.LocalityExternal, Roles: []string{backends.RoleSynthesis},
		APIKeyRef: "openrouter-api-key", APIKey: resolved, Enabled: true,
	}})

	rec := manageReqWithPool(t, adminAR(), bp, map[string]any{"action": "backend-list"})
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, resolved) {
		t.Fatal("resolved key value leaked into backend-list")
	}
	if !strings.Contains(body, "openrouter-api-key") {
		t.Fatal("api_key_ref (name) missing from backend-list")
	}
}
