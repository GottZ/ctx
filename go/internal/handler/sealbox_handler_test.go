package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/go-chi/chi/v5"
)

// secretsRouterAs mirrors settingsRouterAs: the PRODUCTION secrets chain
// (MountSecrets) with ar injected and nil pool — passing the gate without
// hitting an early validation layer panics, which is the red proof.
func secretsRouterAs(t *testing.T, ar *auth.AuthResult, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	h := &SecretsHandler{
		pool:   nil,
		cfg:    testCfgStore(t),
		reload: func(*http.Request) error { return nil },
		// The real no-key failure shape: New("") names the env var, no values.
		openBox: func() (*sealbox.Box, error) { return sealbox.New("", "") },
	}
	MountSecrets(r, h)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	defer func() {
		if rc := recover(); rc != nil {
			t.Fatalf("handler panicked (reached the DB layer — no gate before it): %v", rc)
		}
	}()
	r.ServeHTTP(rec, req)
	return rec
}

// SECURITY PROPERTY (G17): every /api/secrets route requires an admin key —
// PUT is provider-credential injection, GET enumerates the vault, DELETE is
// denial of service on the failover chain.
//
// Negative probe (2026-06-11): run against MountSecrets with the
// RequireAdmin line removed first — GET/DELETE panicked into the nil pool,
// PUT answered non-403 — proving the gate is load-bearing in the production
// chain.
func TestSecretsAdminGate_NonAdmin403(t *testing.T) {
	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/secrets", ""},
		{http.MethodPut, "/api/secrets/prov-main", `{"value":"x"}`},
		{http.MethodDelete, "/api/secrets/prov-main", ""},
	}
	for _, c := range cases {
		t.Run(c.method+"_"+c.path, func(t *testing.T) {
			rec := secretsRouterAs(t, nonAdminAR(), c.method, c.path, c.body)
			assertForbiddenAdmin(t, rec)
		})
	}
}

// Admin keys pass the gate and reach the handler's validation layer — the
// 422/503 responses fire BEFORE any pool access.
func TestSecretsAdminGate_AdminPassesGate(t *testing.T) {
	rec := secretsRouterAs(t, adminAR(), http.MethodPut, "/api/secrets/INVALID-NAME", `{"value":"x"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (name validation, i.e. past the admin gate)", rec.Code)
	}
}

func TestSecretsPut_EmptyValue422(t *testing.T) {
	rec := secretsRouterAs(t, adminAR(), http.MethodPut, "/api/secrets/prov-main", `{"value":""}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 for empty value", rec.Code)
	}
}

// Without a usable master key the PUT is a 503 naming the env var — secrets
// are optional infrastructure, not a 500 and never a silent accept.
func TestSecretsPut_NoMasterKey503(t *testing.T) {
	rec := secretsRouterAs(t, adminAR(), http.MethodPut, "/api/secrets/prov-main", `{"value":"sk-something"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without master key", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, sealbox.EnvKey) {
		t.Errorf("error = %q, want it to name %s", errMsg, sealbox.EnvKey)
	}
	if strings.Contains(rec.Body.String(), "sk-something") {
		t.Errorf("error path echoed the submitted value")
	}
}

// TestSecretRefsRemediationBranches pins all three arms of
// secretRefs.remediation without a DB. The BACKENDS-ONLY arm is the only one a
// live reference scan can still produce (β8: no registry key is sensitive, so
// secretRefs.settings is always empty in practice) — the settings-only and
// MIXED arms would otherwise have lost their coverage together with
// chat.api_key, while the code stays live and load-bearing for the day a
// settings-side secret carrier returns.
//
// It lives in the unit file, not next to the guard in
// sealbox_handler_integration_test.go: it needs no database, and a pin behind
// the integration tag would sit out every `-short` run and every CI stage that
// has no PostgreSQL — which is most of them.
func TestSecretRefsRemediationBranches(t *testing.T) {
	const name = "prov-x"
	cases := []struct {
		label      string
		refs       secretRefs
		wantSub    []string
		wantAbsent []string
	}{
		{
			label:      "backends only",
			refs:       secretRefs{backends: []string{"be-1"}},
			wantSub:    []string{name, "api_key_ref"},
			wantAbsent: []string{"/api/settings/"},
		},
		{
			label:      "settings only",
			refs:       secretRefs{settings: []string{"future.secret_key"}},
			wantSub:    []string{name, "/api/settings/"},
			wantAbsent: []string{"api_key_ref"},
		},
		{
			label:   "mixed",
			refs:    secretRefs{settings: []string{"future.secret_key"}, backends: []string{"be-1"}},
			wantSub: []string{name, "api_key_ref", "/api/settings/"},
		},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := c.refs.remediation(name)
			for _, sub := range c.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("remediation = %q, want it to name %q", got, sub)
				}
			}
			for _, sub := range c.wantAbsent {
				if strings.Contains(got, sub) {
					t.Errorf("remediation = %q, must not name %q (wrong way out for this reference type)", got, sub)
				}
			}
		})
	}
}
