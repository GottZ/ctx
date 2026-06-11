package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/go-chi/chi/v5"
)

// settingsRouterAs mounts the PRODUCTION settings chain (MountSettings — the
// same function server.go mounts) behind a middleware that injects ar, and
// fires one request. pool/cfg are nil: any request that passes the admin gate
// and reaches a DB-touching path panics, which the recover converts into the
// red proof that no gate was in the chain.
func settingsRouterAs(t *testing.T, ar *auth.AuthResult, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	// cfg from a clean env build so handlers can render snapshots when needed.
	MountSettings(r, &SettingsHandler{pool: nil, cfg: testCfgStore(t), reload: func(*http.Request) error { return nil }})

	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
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

// testCfgStore builds a snapshot store from a clean env (all registry vars
// emptied, V11 satisfied).
func testCfgStore(t *testing.T) *config.Store {
	t.Helper()
	resetRegistryEnv(t)
	c, _ := config.FromEnv()
	_ = config.Validate(c)
	return config.NewStore(c)
}

func resetRegistryEnv(t *testing.T) {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
}

// SECURITY PROPERTY (G16/F4-O4): every /api/settings route — reads included —
// requires an admin key. Without the gate, any valid key (friend tenant, the
// MCP remote token circulating through claude.ai/Cloudflare) could read the
// effective server config and flip chat.host / inject secret_refs.
//
// Negative probe (2026-06-11): this test was run against MountSettings with
// the RequireAdmin line removed first — every subtest failed (nil-pool panic
// into the DB layer or non-403 status), proving the gate is load-bearing in
// exactly the chain production mounts.
func TestSettingsAdminGate_NonAdmin403(t *testing.T) {
	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/settings", ""},
		{http.MethodGet, "/api/settings/rerank.blend_weight", ""},
		{http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":0.7}`},
		{http.MethodDelete, "/api/settings/rerank.blend_weight", ""},
	}
	for _, c := range cases {
		t.Run(c.method+"_"+c.path, func(t *testing.T) {
			rec := settingsRouterAs(t, nonAdminAR(), c.method, c.path, c.body)
			assertForbiddenAdmin(t, rec)
		})
	}
}

// Admin keys pass the gate and reach the handler's own validation layer —
// 409/404 responses prove the request travelled THROUGH RequireAdmin into
// HandlePut without touching the nil pool.
func TestSettingsAdminGate_AdminPassesGate(t *testing.T) {
	rec := settingsRouterAs(t, adminAR(), http.MethodPut, "/api/settings/server.db_host", `{"value":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (restart-only, i.e. past the admin gate)", rec.Code)
	}
}

func TestHandlePut_UnknownKey404(t *testing.T) {
	rec := settingsRouterAs(t, adminAR(), http.MethodPut, "/api/settings/nope.key", `{"value":1}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown key", rec.Code)
	}
}

func TestHandlePut_RestartOnly409(t *testing.T) {
	rec := settingsRouterAs(t, adminAR(), http.MethodPut, "/api/settings/dream.parallelism", `{"value":4}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "restart-only") || !strings.Contains(errMsg, "CTX_DREAM_PARALLELISM") {
		t.Errorf("error = %q, want restart-only + env var hint", errMsg)
	}
}

// embed.model is mut:"coupled" — a hot flip would change the vector space
// without a re-embed migration. Must stay 409 even though embed.HOST became
// overridable (coupled:embed-cache, X2).
func TestHandlePut_CoupledModel409(t *testing.T) {
	rec := settingsRouterAs(t, adminAR(), http.MethodPut, "/api/settings/embed.model", `{"value":"other-model"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for coupled embed.model", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "re-embed") {
		t.Errorf("error = %q, want re-embed migration hint", errMsg)
	}
}

func TestHandlePut_BoolStrict422(t *testing.T) {
	for _, val := range []string{`"yes"`, `"1"`, `"TRUE"`} {
		rec := settingsRouterAs(t, adminAR(), http.MethodPut, "/api/settings/rerank.enabled",
			fmt.Sprintf(`{"value":%s}`, val))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("value %s: status = %d, want 422 (legacy parser would silently coerce to false)", val, rec.Code)
		}
	}
}

func TestHandlePut_NonScalar422(t *testing.T) {
	for _, body := range []string{`{"value":[1,2]}`, `{"value":{"a":1}}`, `{"value":null}`} {
		rec := settingsRouterAs(t, adminAR(), http.MethodPut, "/api/settings/rerank.blend_weight", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s: status = %d, want 422", body, rec.Code)
		}
	}
}

// Below: §3.4 masking rule (unit surface; the env-plaintext E2E probe lives
// in the integration test).

func TestRenderEffective_EnvSensitiveMasked(t *testing.T) {
	resetRegistryEnv(t)
	const marker = "ENV-PLAINTEXT-MARKER-aaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("CTX_CHAT_API_KEY", marker)
	c, _ := config.FromEnv()

	info, _ := config.KeyByName("chat.api_key")
	got := renderEffective(c, info, nil)
	if got != maskedEnvValue {
		t.Errorf("env-sourced sensitive value = %v, want %q", got, maskedEnvValue)
	}
	if s, _ := got.(string); strings.Contains(s, marker) {
		t.Errorf("marker leaked through the masking rule")
	}
}

func TestRenderEffective_DefaultSensitiveEmpty(t *testing.T) {
	resetRegistryEnv(t)
	c, _ := config.FromEnv()
	info, _ := config.KeyByName("chat.api_key")
	if got := renderEffective(c, info, nil); got != "" {
		t.Errorf("default-sourced sensitive value = %v, want empty", got)
	}
}

// DB-sourced sensitive keys render the secret_ref NAME from the override row
// — never the resolved plaintext sitting in the snapshot.
func TestRenderEffective_DBSensitiveShowsRefName(t *testing.T) {
	resetRegistryEnv(t)
	const plaintext = "RESOLVED-PLAINTEXT-MARKER-bbbbbbbbbbbbbbbbbbbb"
	resolve := func(name string) (string, error) { return plaintext, nil }
	c, issues := config.Build([]config.Override{{Key: "chat.api_key", Value: "prov-main"}}, resolve)
	if config.HasErrors(issues) {
		t.Fatalf("fixture build: %v", issues)
	}
	if c.Source("chat.api_key") != "settings" {
		t.Fatalf("fixture: source = %q, want settings", c.Source("chat.api_key"))
	}

	info, _ := config.KeyByName("chat.api_key")
	overrides := map[string]json.RawMessage{"chat.api_key": json.RawMessage(`"prov-main"`)}
	got := renderEffective(c, info, overrides)
	if got != "prov-main" {
		t.Errorf("db-sourced sensitive value = %v, want the ref name", got)
	}
	if s, _ := got.(string); strings.Contains(s, plaintext) {
		t.Errorf("resolved plaintext leaked into the rendering")
	}
	// Fail-safe floor: even the raw RenderValue path is presence-only.
	if v, _ := config.RenderValue(c, "chat.api_key"); v != "set" {
		t.Errorf("RenderValue fail-safe = %v, want \"set\"", v)
	}
}

func TestRenderEffective_NonSensitiveHostRedacted(t *testing.T) {
	resetRegistryEnv(t)
	t.Setenv("CTX_CHAT_HOST", "http://chat-host:11434")
	c, _ := config.FromEnv()
	info, _ := config.KeyByName("chat.host")
	if got := renderEffective(c, info, nil); got != "http://chat-host:11434" {
		t.Errorf("plain host = %v, want verbatim URL", got)
	}
}

// Below: pure-function helpers.

func TestApiSource(t *testing.T) {
	for in, want := range map[string]string{"settings": "db", "env": "env", "default": "default", "": "default"} {
		if got := apiSource(in); got != want {
			t.Errorf("apiSource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizedJSON(t *testing.T) {
	info := func(typ string) config.KeyInfo { return config.KeyInfo{Type: typ} }
	cases := []struct {
		typ, raw, want string
	}{
		{"float", "0.7", "0.7"},
		{"int", "42", "42"},
		{"int", "007", "7"},
		{"seconds", "420", "420"},
		{"bool", "true", "true"},
		{"hours", "12", "12"},
		{"hours", "45d", `"45d"`},
		{"string", "qwen3.5:9b", `"qwen3.5:9b"`},
		{"float", "kaputt", `"kaputt"`}, // falls through to the build's 422
	}
	for _, c := range cases {
		if got := string(normalizedJSON(info(c.typ), c.raw)); got != c.want {
			t.Errorf("normalizedJSON(%s, %q) = %s, want %s", c.typ, c.raw, got, c.want)
		}
	}
}

func TestPairingWarnings(t *testing.T) {
	resetRegistryEnv(t)
	// Host overridden, protocol still env/default → advisory.
	c1, _ := config.Build([]config.Override{{Key: "chat.host", Value: "http://other:8089"}}, nil)
	if w := pairingWarnings("chat.host", c1); len(w) != 1 || !strings.Contains(w[0], "chat.protocol") {
		t.Errorf("warnings = %v, want one naming chat.protocol", w)
	}
	// Host AND protocol overridden → silent.
	c2, _ := config.Build([]config.Override{
		{Key: "chat.host", Value: "http://other:8089"},
		{Key: "chat.protocol", Value: "openai"},
	}, nil)
	if w := pairingWarnings("chat.host", c2); len(w) != 0 {
		t.Errorf("warnings = %v, want none when paired", w)
	}
	// Non-host keys never warn.
	if w := pairingWarnings("rerank.blend_weight", c1); len(w) != 0 {
		t.Errorf("warnings = %v, want none for non-host key", w)
	}
}

// The X2 admission change: coupled:embed-cache keys (embed.host) ARE
// override-admissible, plain coupled (embed.model) stays rejected.
func TestBuild_EmbedHostOverrideAdmitted(t *testing.T) {
	resetRegistryEnv(t)
	c, issues := config.Build([]config.Override{{Key: "embed.host", Value: "http://new-embed:8090"}}, nil)
	if config.HasErrors(issues) {
		t.Fatalf("build: %v", issues)
	}
	if c.Source("embed.host") != "settings" || c.Embed.Host != "http://new-embed:8090" {
		t.Errorf("embed.host override not admitted: %q (source %q)", c.Embed.Host, c.Source("embed.host"))
	}

	c2, _ := config.Build([]config.Override{{Key: "embed.model", Value: "other"}}, nil)
	if c2.Source("embed.model") == "settings" {
		t.Errorf("embed.model (coupled) must NOT be override-admissible")
	}
}
