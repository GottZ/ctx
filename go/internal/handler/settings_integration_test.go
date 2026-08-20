//go:build integration

// Integration probes for the F2-W5 settings API against a real PG18
// testcontainer — the full handler→store→trigger→reload pipeline:
//
//   - PUT/GET/DELETE roundtrip with hot snapshot effect and audit attribution
//   - 422 BEFORE persist (invalid value leaves no row, no audit)
//   - §3.4 masking E2E: env plaintext marker never appears in ANY settings
//     response (PUT previous.value, DELETE value, GET value)
//   - secret_ref gate: sensitive keys are superseded backend-tuple keys —
//     the 409 pool pointer fires before any format check, nothing persists
//     (the living provider-key path is the pool row's api_key_ref)
//   - X2 flush probe: PUT embed.host is 409-closed (no flush on a blocked
//     write); the DELETE revert of a legacy override row still flushes,
//     a non-coupled write does not (negative probe)
//   - NOTIFY handler: events.SettingsWriteHandler reloads the snapshot
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsAPI -count=1 -v
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type settingsAPI struct {
	router *chi.Mux
	cfg    *config.Store
	pool   *pgxpool.Pool
}

func (s *settingsAPI) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func (s *settingsAPI) envelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestSettingsAPI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Clean env base + the fixtures the probes need. CTX_RERANK_BLEND_WEIGHT
	// gives DELETE a visible env revert target; CTX_CHAT_API_KEY carries the
	// leak-scan marker.
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.4")
	const envMarker = "ENV-PLAINTEXT-MARKER-aaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("CTX_CHAT_API_KEY", envMarker)

	// Master key + one sealed secret through the real crypto path, so the
	// secret_ref candidate build resolves.
	rawKey := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatalf("rand: %v", err)
	}
	keyHex := hex.EncodeToString(rawKey)
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")
	box, err := sealbox.New(keyHex, "")
	if err != nil {
		t.Fatalf("sealbox: %v", err)
	}
	const secretPlain = "RESOLVED-PLAINTEXT-MARKER-bbbbbbbbbbbbbbbbbbbb"
	nonce, ct, err := box.Seal("prov-main", store.GlobalScope, []byte(secretPlain))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	{
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.PutSecret(ctx, tx, "prov-main", store.GlobalScope, nonce, ct, 1, nil); err != nil {
			t.Fatalf("put secret: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit secret: %v", err)
		}
	}

	// Acting admin key (real row → audit attribution is verifiable E2E).
	actor, _, err := store.CreateApiKey(ctx, pool, "settings-api-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}

	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}
	cfgStore := config.NewStore(envCfg)

	ar := &auth.AuthResult{
		ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{"private"}, IsValid: true, IsAdmin: true,
	}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountSettings(router, NewSettingsHandler(pool, cfgStore))
	api := &settingsAPI{router: router, cfg: cfgStore, pool: pool}

	auditCount := func(key string) int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings_audit WHERE entity_key = $1`, key).Scan(&n); err != nil {
			t.Fatalf("audit count: %v", err)
		}
		return n
	}
	cacheCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_cache`).Scan(&n); err != nil {
			t.Fatalf("cache count: %v", err)
		}
		return n
	}
	seedCache := func() {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
			 VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
			[]byte("flush-probe-hash"), "test-model",
			pgvec.NewVector(make([]float32, 1024)), "flush probe"); err != nil {
			t.Fatalf("seed cache: %v", err)
		}
	}

	t.Run("PutRoundtrip_HotEffect_Audit", func(t *testing.T) {
		rec := api.do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":"0.7"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
		}
		resp := api.envelope(t, rec)
		if resp["source"] != "db" || resp["value"] != 0.7 {
			t.Errorf("PUT response value/source = %v/%v, want 0.7/db (string input normalized)", resp["value"], resp["source"])
		}
		prev, _ := resp["previous"].(map[string]any)
		if prev["value"] != 0.4 || prev["source"] != "env" {
			t.Errorf("previous = %v, want 0.4/env", prev)
		}
		// Hot effect: the snapshot the query path reads carries the new value.
		if got := cfgStore.Snapshot().Rerank.BlendWeight; got != 0.7 {
			t.Errorf("snapshot blend_weight = %v, want 0.7 (hot reload)", got)
		}
		// DB row carries the NORMALIZED number, not the input string.
		var rawVal string
		if err := pool.QueryRow(ctx,
			`SELECT value::text FROM context_settings WHERE key = 'rerank.blend_weight'`).Scan(&rawVal); err != nil {
			t.Fatalf("row: %v", err)
		}
		if rawVal != "0.7" {
			t.Errorf("persisted JSONB = %s, want 0.7 (number)", rawVal)
		}

		// GET single: value/source + the trigger audit row with attribution.
		rec = api.do(t, http.MethodGet, "/api/settings/rerank.blend_weight", "")
		resp = api.envelope(t, rec)
		setting, _ := resp["setting"].(map[string]any)
		if setting["value"] != 0.7 || setting["source"] != "db" {
			t.Errorf("GET value/source = %v/%v, want 0.7/db", setting["value"], setting["source"])
		}
		audit, _ := resp["audit"].([]any)
		if len(audit) == 0 {
			t.Fatalf("GET audit empty, want the set row")
		}
		newest, _ := audit[0].(map[string]any)
		if newest["action"] != "set" || newest["via"] != "api" || newest["actor_label"] != "settings-api-actor" {
			t.Errorf("audit row = %v, want set/api/settings-api-actor", newest)
		}
	})

	t.Run("DeleteRevertsToEnv", func(t *testing.T) {
		rec := api.do(t, http.MethodDelete, "/api/settings/rerank.blend_weight", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d body=%s", rec.Code, rec.Body.String())
		}
		resp := api.envelope(t, rec)
		if resp["value"] != 0.4 || resp["source"] != "env" {
			t.Errorf("DELETE response = %v/%v, want 0.4/env", resp["value"], resp["source"])
		}
		if got := cfgStore.Snapshot().Rerank.BlendWeight; got != 0.4 {
			t.Errorf("snapshot blend_weight = %v, want env 0.4 after revert", got)
		}
		rec = api.do(t, http.MethodDelete, "/api/settings/rerank.blend_weight", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("second DELETE = %d, want 404 (no override set)", rec.Code)
		}
	})

	t.Run("InvalidValue422BeforePersist", func(t *testing.T) {
		before := auditCount("rerank.blend_weight")
		snapBefore := cfgStore.Snapshot()
		rec := api.do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":1.5}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = 'rerank.blend_weight'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("422 persisted a row — validation must run BEFORE persist")
		}
		if got := auditCount("rerank.blend_weight"); got != before {
			t.Errorf("422 grew the audit trail (%d → %d)", before, got)
		}
		if cfgStore.Snapshot() != snapBefore {
			t.Errorf("422 swapped the snapshot")
		}
	})

	t.Run("SecretRefGate", func(t *testing.T) {
		// Entflechtungs-Welle Stufe 1: every sensitive settings key is a
		// superseded backend-tuple key (chat.api_key & co) — the 409 pool
		// pointer fires BEFORE any secret_ref format/existence check, so a
		// provider-key-shaped plaintext can never reach table or audit via
		// this route anymore. The living path for provider keys is the pool
		// row's api_key_ref (`ctx backends` + `ctx secrets`).
		for _, body := range []string{`{"value":"SK-PROVIDER-KEY-SHAPED-VALUE"}`, `{"value":"ghost-ref"}`} {
			rec := api.do(t, http.MethodPut, "/api/settings/chat.api_key", body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("PUT %s status = %d, want 409 (superseded gate)", body, rec.Code)
			}
			resp := api.envelope(t, rec)
			if msg, _ := resp["error"].(string); !strings.Contains(msg, "backend pool") {
				t.Errorf("error = %q, want the backend-pool pointer", msg)
			}
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = 'chat.api_key'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("blocked PUT persisted a row")
		}
	})

	t.Run("MaskingE2E_NoPlaintextInAnyResponse", func(t *testing.T) {
		scan := func(rec *httptest.ResponseRecorder, label string) {
			t.Helper()
			body := rec.Body.String()
			if strings.Contains(body, envMarker) {
				t.Errorf("%s leaks the env plaintext marker: %s", label, body)
			}
			if strings.Contains(body, secretPlain) {
				t.Errorf("%s leaks the resolved secret plaintext: %s", label, body)
			}
		}

		// GET list while chat.api_key is env-sourced.
		rec := api.do(t, http.MethodGet, "/api/settings", "")
		scan(rec, "GET list (env-sourced)")
		var list struct {
			Settings []settingView `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("unmarshal list: %v", err)
		}
		for _, s := range list.Settings {
			if s.Key == "chat.api_key" && s.Value != maskedEnvValue {
				t.Errorf("GET chat.api_key value = %v, want %q", s.Value, maskedEnvValue)
			}
		}

		// db-sourced state via SQL + reload (the psql/break-glass shape) —
		// the settings PUT route 409s on superseded keys since the
		// Entflechtungs-Welle, but a legacy override row can still EXIST
		// (pre-wave installs, direct SQL), and every read surface must mask
		// it exactly as before.
		{
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := store.UpsertSetting(ctx, tx, "chat.api_key", store.GlobalScope, json.RawMessage(`"prov-main"`), nil); err != nil {
				t.Fatalf("upsert secret_ref: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := settings.Reload(ctx, pool, cfgStore); err != nil {
				t.Fatalf("reload: %v", err)
			}
		}
		// The snapshot resolved the plaintext (that is the point) — but no
		// response surface may carry it.
		if got := cfgStore.Snapshot().Chat.APIKey; got != secretPlain {
			t.Fatalf("snapshot Chat.APIKey = %q, want the resolved secret", got)
		}
		rec = api.do(t, http.MethodGet, "/api/settings/chat.api_key", "")
		scan(rec, "GET single (db-sourced)")
		resp := api.envelope(t, rec)
		setting, _ := resp["setting"].(map[string]any)
		if setting["value"] != "prov-main" {
			t.Errorf("GET db-sourced sensitive value = %v, want ref name", setting["value"])
		}

		// DELETE reverts to env: the response value is the masked placeholder.
		rec = api.do(t, http.MethodDelete, "/api/settings/chat.api_key", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d", rec.Code)
		}
		scan(rec, "DELETE value")
		resp = api.envelope(t, rec)
		if resp["value"] != maskedEnvValue || resp["source"] != "env" {
			t.Errorf("DELETE response = %v/%v, want masked/env", resp["value"], resp["source"])
		}
		if got := cfgStore.Snapshot().Chat.APIKey; got != envMarker {
			t.Errorf("snapshot after revert = %q, want the env value", got)
		}
	})

	t.Run("X2FlushProbe", func(t *testing.T) {
		seedCache()
		if cacheCount() != 1 {
			t.Fatalf("seed failed")
		}
		// Non-coupled write: cache must survive (negative probe).
		rec := api.do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":0.6}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d", rec.Code)
		}
		if cacheCount() != 1 {
			t.Errorf("non-coupled write flushed the cache")
		}
		// PUT embed.host is CLOSED since the Entflechtungs-Welle (superseded
		// 409 fires before the coupled path) — and the blocked write must
		// not flush anything either.
		rec = api.do(t, http.MethodPut, "/api/settings/embed.host", `{"value":"http://flush-probe:9999"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("PUT embed.host status = %d, want 409 (superseded)", rec.Code)
		}
		if cacheCount() != 1 {
			t.Errorf("blocked embed.host PUT flushed the cache")
		}
		// The living coupled path is the DELETE revert of a legacy override
		// row (allowed for superseded keys — cleanup): seed the row via SQL,
		// then DELETE — the effective embed host changes, so the X2 flush
		// obligation still fires.
		{
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := store.UpsertSetting(ctx, tx, "embed.host", store.GlobalScope, json.RawMessage(`"http://legacy-override:9999"`), nil); err != nil {
				t.Fatalf("upsert embed.host: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := settings.Reload(ctx, pool, cfgStore); err != nil {
				t.Fatalf("reload: %v", err)
			}
		}
		seedCache()
		rec = api.do(t, http.MethodDelete, "/api/settings/embed.host", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE embed.host status = %d body=%s", rec.Code, rec.Body.String())
		}
		if cacheCount() != 0 {
			t.Errorf("embed.host DELETE (revert) did not flush the cache (X2)")
		}
	})

	t.Run("NotifyHandlerReloadsSnapshot", func(t *testing.T) {
		// Naked SQL write (the psql/break-glass shape), then the listener
		// handler — the snapshot must pick the value up without any API call.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := store.UpsertSetting(ctx, tx, "chat.model", store.GlobalScope, json.RawMessage(`"sql-edited-model"`), nil); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// Settings-entity payload: the backend pool is not consulted on this
		// path (G25 added the parameter for entity=context_backends reloads).
		h := events.NewSettingsWriteHandler(pool, cfgStore, backends.NewPool(pool, nil), nil)
		if err := h.HandleNotification(ctx, &pgconn.Notification{Channel: "ctx_settings_write", Payload: `{"entity":"context_settings","key":"chat.model","op":"INSERT"}`}, nil); err != nil {
			t.Fatalf("notify handler: %v", err)
		}
		if got := cfgStore.Snapshot().Chat.Model; got != "sql-edited-model" {
			t.Errorf("snapshot chat.model = %q, want sql-edited-model (NOTIFY reload path)", got)
		}
	})
}
