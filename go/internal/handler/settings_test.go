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
// effective server config and flip any hot key under it. (The concrete abuse
// this line named until β8 — flip chat.host, inject a secret_ref — retired
// with the chat tuple; the gate's subject is the whole namespace, not one key.)
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

// TestMutabilityBlockCoupledBranches pins the two coupled arms of
// mutabilityBlock on synthetic KeyInfos. Both classes lost their last registry
// carriers with the embed tuple in β7 — embed.model was the only mut:"coupled"
// key, embed.host/protocol the only coupled:embed-cache ones — so a
// registry-backed vehicle no longer exists, while the branches themselves stay
// live vocabulary (validMut, config/registry.go) that the next key taking
// either tag will land in.
//
// The HTTP half this test used to carry (PUT /api/settings/embed.model ⇒ 409
// with the backend-pool pointer, "superseded wins over coupled") went with the
// key in β7, and the precedence statement it was reduced to went with the
// mechanic in β9 (E11): mutabilityBlock has no superseded arm left to be first.
// What remains here are the two coupled arms themselves, plus the live hot key
// as the registry-backed negative control that keeps the synthetic positives
// from being tautologies.
func TestMutabilityBlockCoupledBranches(t *testing.T) {
	msg, blocked := mutabilityBlock(config.KeyInfo{Key: "future.coupled", Mutability: "coupled"})
	if !blocked || !strings.Contains(msg, "re-embed") {
		t.Errorf("plain coupled branch = (%q, %v), want blocked with re-embed hint", msg, blocked)
	}

	// coupled:embed-cache is the ADMITTED arm: it must fall through unblocked,
	// exactly like "hot".
	if msg, blocked := mutabilityBlock(config.KeyInfo{
		Key: "future.embedcache", Mutability: "coupled:embed-cache",
	}); blocked {
		t.Errorf("coupled:embed-cache branch = (%q, %v), want unblocked (X2 admission)", msg, blocked)
	}

	// Registry-backed negative control (inherited from the retired
	// TestMutabilityBlockSuperseded): a live hot key stays writable, so the two
	// synthetic positives above are not asserting a function that blocks
	// everything.
	hot, ok := config.KeyByName("dream.backoff_factor")
	if !ok {
		t.Fatal("dream.backoff_factor missing from registry")
	}
	if _, blocked := mutabilityBlock(hot); blocked {
		t.Error("dream.backoff_factor (hot) must stay writable")
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
//
// VEHICLE NOTE (β8): the sensitive arms of renderEffective used to be driven
// by config.KeyByName("chat.api_key") — the registry's last secret:"fp" key.
// After the chat tuple left, NO registry key is Sensitive that could ever
// reach this function: server.db_password is secret:"presence", but it is also
// mut:"restart", so admitOverride drops its row long before a snapshot or an
// override map carries it.
//
// renderEffective takes a config.KeyInfo by VALUE, so the branch selector is a
// plain struct field, not a registry lookup — the tests below build the
// KeyInfo SYNTHETICALLY and put a LIVE key NAME inside it (digest.mode: hot,
// global-only, unvalidated string), so c.Source(info.Key) still resolves
// through the real snapshot machinery ("env" / "settings" / default). Only the
// Sensitive bit is synthetic; every code path under test is production.

func TestRenderEffective_EnvSensitiveMasked(t *testing.T) {
	resetRegistryEnv(t)
	const marker = "ENV-PLAINTEXT-MARKER-aaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("CTX_DIGEST_MODE", marker)
	c, _ := config.FromEnv()

	info := config.KeyInfo{Key: "digest.mode", Sensitive: true}
	if c.Source(info.Key) != "env" {
		t.Fatalf("fixture: source = %q, want env", c.Source(info.Key))
	}
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
	info := config.KeyInfo{Key: "digest.mode", Sensitive: true}
	if c.Source(info.Key) != "default" && c.Source(info.Key) != "" {
		t.Fatalf("fixture: source = %q, want default", c.Source(info.Key))
	}
	if got := renderEffective(c, info, nil); got != "" {
		t.Errorf("default-sourced sensitive value = %v, want empty", got)
	}
}

// DB-sourced sensitive keys render the secret_ref NAME from the override row
// — never the resolved plaintext sitting in the snapshot.
//
// The DIVERGENCE between snapshot and row is what the branch exists for, and
// it is reproduced faithfully here: the fixture writes the "plaintext" into
// the snapshot field (Build with the marker as the override value) while the
// override MAP carries the harmless ref name — exactly the shape the live
// secret_ref resolver produced when chat.api_key still existed.
func TestRenderEffective_DBSensitiveShowsRefName(t *testing.T) {
	resetRegistryEnv(t)
	const plaintext = "RESOLVED-PLAINTEXT-MARKER-bbbbbbbbbbbbbbbbbbbb"
	c, issues := config.Build([]config.Override{{Key: "digest.mode", Value: plaintext}}, nil)
	if config.HasErrors(issues) {
		t.Fatalf("fixture build: %v", issues)
	}
	if c.Source("digest.mode") != "settings" {
		t.Fatalf("fixture: source = %q, want settings", c.Source("digest.mode"))
	}
	if c.Digest.Mode != plaintext {
		t.Fatalf("fixture: snapshot must carry the resolved-plaintext stand-in, got %q", c.Digest.Mode)
	}

	info := config.KeyInfo{Key: "digest.mode", Sensitive: true}
	overrides := map[string]json.RawMessage{"digest.mode": json.RawMessage(`"prov-main"`)}
	got := renderEffective(c, info, overrides)
	if got != "prov-main" {
		t.Errorf("db-sourced sensitive value = %v, want the ref name", got)
	}
	if s, _ := got.(string); strings.Contains(s, plaintext) {
		t.Errorf("resolved plaintext leaked into the rendering")
	}

	// Presence floor: the row vanished between load and render — the branch
	// must fall back to "set", never to the snapshot value.
	if got := renderEffective(c, info, nil); got != "set" {
		t.Errorf("row-vanished floor = %v, want \"set\"", got)
	}
	if s, _ := renderEffective(c, info, nil).(string); strings.Contains(s, plaintext) {
		t.Errorf("presence floor leaked the snapshot value")
	}

	// The "even the raw config.RenderValue path is presence-only" assertion
	// this test used to carry needed a REGISTRY-sensitive key: RenderValue
	// reads the Sensitive class off the entry, which a synthetic KeyInfo
	// cannot reach. It moved to the injected-registry vehicle in
	// config/synthreg_test.go (TestSynthMaskSecretPerClass, the
	// renderField(..., SurfaceAPI) == "set" arm).
}

// TestRenderEffective_NonSensitiveVerbatim is what is left of
// TestRenderEffective_NonSensitiveHostRedacted. The non-sensitive arm —
// renderEffective delegates to config.RenderValue and returns the value
// verbatim — keeps its subject on any live key and is asserted below on a real
// registry KeyInfo.
//
// The HOST half died with chat.host in β8: it was the registry's last ".host"
// key, and the namespace convention (redactHostURL runs on the key SUFFIX, so
// it guards the next .host key somebody adds) cannot be pinned on a faked
// KeyInfo here — the redaction happens inside config.renderField, which
// resolves the entry out of the real registry. That statement now lives on the
// injected-registry vehicle: config/synthreg_test.go, TestSynthHostKeyIsRedacted.
func TestRenderEffective_NonSensitiveVerbatim(t *testing.T) {
	resetRegistryEnv(t)
	t.Setenv("CTX_DIGEST_MODE", "compact")
	c, _ := config.FromEnv()
	info, ok := config.KeyByName("digest.mode")
	if !ok {
		t.Fatal("digest.mode missing from registry")
	}
	if info.Sensitive {
		t.Fatal("digest.mode became sensitive — this test needs the non-sensitive arm")
	}
	if got := renderEffective(c, info, nil); got != "compact" {
		t.Errorf("non-sensitive value = %v, want the verbatim value", got)
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

// TestBuild_EmbedHostOverrideAdmitted died with the embed tuple in β7. It
// pinned the X2 admission change at config.Build level: coupled:embed-cache
// keys (embed.host) ARE override-admissible, plain coupled (embed.model) is
// not. Both classes are unoccupied after the cut, and config.Build resolves
// against the REAL registry (entryByKey, build.go) — a synthetic key cannot
// reach that path, so the statement has no vehicle left at this level; it is
// design/01 §3's "Klassen bleiben gültig, unbesetzt". What is still asserted
// synthetically is the handler-side arm of the same distinction, in
// TestMutabilityBlockCoupledBranches above. The admission branch in
// config/build.go itself is exercised by its restart cases
// (config/build_test.go, TestBuildAdmissionRejections).

// TestMutabilityBlockSuperseded died with its subject in β9 (E11). It pinned
// the superseded gate of Entflechtungs-Welle Stufe 1: a PUT on an
// f3:context_backends key answered 409 with a pointer to the backend pool,
// because a settings override on such a key would have been a dead bootstrap
// seed that LOOKS live while the pool serves something else.
//
// Its subject had already lost its registry carrier in β8 (chat.host was the
// last of the six tuples) and was pinned on a synthetic KeyInfo from then on.
// With KeyInfo.Superseded and the branch it selected removed, a PUT on one of
// the 29 retired names takes the ordinary unknownKey path — 404, not 409 (E13);
// that contract is pinned in β10, deliberately not here. The live-hot negative
// control this test carried moved into TestMutabilityBlockCoupledBranches
// above.
