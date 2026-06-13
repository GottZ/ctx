// backend-test OpenRouter detail checks + the confirm_data_collection gate
// (G29 / F3-P5). Mock-served — no live key in CI.
package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// orTestServer mocks the two OpenRouter detail endpoints: /key (auth) and
// the public /endpoints/zdr (model_id is the match field — inventory dump
// shape). The chat model under test has 2 ZDR endpoints, a stranger has 1.
func orTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/key":
			_, _ = w.Write([]byte(`{"data":{"limit_remaining":24.31,"usage":1.07,"is_free_tier":false}}`))
		case "/v1/endpoints/zdr":
			_, _ = w.Write([]byte(`{"data":[
				{"model_id":"qwen/qwen3.6-27b","provider_name":"DeepInfra"},
				{"model_id":"qwen/qwen3.6-27b","provider_name":"SiliconFlow"},
				{"model_id":"qwen/qwen3-32b","provider_name":"Groq"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestBackendTestOpenRouterDetails: an openrouter-class backend-test reports
// credits_remaining from /key and the default model's ZDR endpoint count —
// the number that predicts whether the forced zdr:true leaves a non-empty
// provider set (0 ⇒ permanent ClassNoProviders).
func TestBackendTestOpenRouterDetails(t *testing.T) {
	srv := orTestServer(t)
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "0190-or", Name: "openrouter", Host: srv.URL,
		Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderOpenRouter,
		Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
		Roles:    []string{backends.RoleSynthesis},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "qwen/qwen3.6-27b"}},
		APIKey:   "sk-or-test", APIKeyRef: "openrouter-api-key", Enabled: true,
	}})

	rec := manageReqWithPool(t, adminAR(), bp, map[string]any{
		"action": "backend-test", "id": "0190-or",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("backend-test: status %d (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"credits_remaining":24.31`, `"usage_usd":1.07`, `"zdr_endpoints":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("backend-test response lacks %s: %s", want, body)
		}
	}
}

// TestBackendTestGenericNoOpenRouterBlock: a generic backend reports no
// openrouter detail object — the checks are provider-class-bound.
func TestBackendTestGenericNoOpenRouterBlock(t *testing.T) {
	srv := orTestServer(t)
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "0190-gen", Name: "cloud", Host: srv.URL,
		Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderGeneric,
		Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
		Roles:    []string{backends.RoleSynthesis},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Enabled:  true,
	}})

	rec := manageReqWithPool(t, adminAR(), bp, map[string]any{
		"action": "backend-test", "id": "0190-gen",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("backend-test: status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"openrouter"`) {
		t.Errorf("generic backend carries an openrouter detail block: %s", rec.Body.String())
	}
}

// TestBackendCreateDataCollectionConfirm: arming the non-ZDR escape on create
// without confirm_data_collection is 400 (the same create-side bypass logic
// as the trust confirm); WITH the confirm the gate opens — proven by reaching
// the 422 validation behind it instead of the 400.
func TestBackendCreateDataCollectionConfirm(t *testing.T) {
	spec := map[string]any{
		"name": "openrouter", "base_url": "https://openrouter.ai/api/v1",
		"provider_class": "openrouter",
		"roles":          []string{"synthesis"}, "model_map": map[string]any{"default": "qwen/qwen3.6-27b"},
		"metadata": map[string]any{"allow_data_collection": true},
	}
	rec := manageReqWithPool(t, adminAR(), backends.NewPool(nil, nil), map[string]any{
		"action": "backend-create", "data": spec,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("escape without confirm: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "confirm_data_collection") {
		t.Fatalf("error does not name the confirm flag: %s", rec.Body.String())
	}

	// With the confirm the gate passes; an empty model_map stops the probe at
	// the 422 validation (a fully valid create would reach the nil store).
	spec["confirm_data_collection"] = true
	spec["model_map"] = map[string]any{}
	rec = manageReqWithPool(t, adminAR(), backends.NewPool(nil, nil), map[string]any{
		"action": "backend-create", "data": spec,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("confirmed escape: status %d, want 422 from validation (body %s)", rec.Code, rec.Body.String())
	}
}

// TestBackendUpdateDataCollectionConfirm: the confirm fires only when the
// escape ARMS (off → on); a backend already carrying it updates
// friction-free (mirrors trustRankRose).
func TestBackendUpdateDataCollectionConfirm(t *testing.T) {
	seed := func(armed bool) *backends.Pool {
		b := backends.Backend{
			ID: "0190-or", Name: "openrouter", Host: "https://openrouter.ai/api/v1",
			Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderOpenRouter,
			Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal,
			Roles:    []string{backends.RoleSynthesis},
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "qwen/qwen3.6-27b"}},
			Enabled:  true,
		}
		if armed {
			b.Metadata = map[string]any{"allow_data_collection": true}
		}
		bp := backends.NewPool(nil, nil)
		bp.SeedSnapshotForTest([]backends.Backend{b})
		return bp
	}

	// Arming without confirm: 400.
	rec := manageReqWithPool(t, adminAR(), seed(false), map[string]any{
		"action": "backend-update", "id": "0190-or",
		"data": map[string]any{"metadata": map[string]any{"allow_data_collection": true}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("arming without confirm: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "confirm_data_collection") {
		t.Fatalf("error does not name the confirm flag: %s", rec.Body.String())
	}

	// Already armed: an unrelated update passes the gate — proven by reaching
	// the 422 validation (emptied model_map), not the 400.
	rec = manageReqWithPool(t, adminAR(), seed(true), map[string]any{
		"action": "backend-update", "id": "0190-or",
		"data": map[string]any{"model_map": map[string]any{}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("already-armed update: status %d, want 422 from validation (body %s)", rec.Code, rec.Body.String())
	}
}
