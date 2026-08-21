//go:build integration

// Integration probes for the F2-W5 settings API against a real PG18
// testcontainer — the full handler→store→trigger→reload pipeline:
//
//   - PUT/GET/DELETE roundtrip with hot snapshot effect and audit attribution
//   - 422 BEFORE persist (invalid value leaves no row, no audit)
//   - §3.4 masking E2E: env plaintext marker never appears in ANY settings
//     response (GET list, GET single, DELETE value). Since β8 the marker rides
//     on CONTEXT_DB_PASSWORD — server.db_password is the only sensitive key
//     the registry has left, and it is env-only by construction
//   - row roundtrip over the reload path: an SQL-written override row reaches
//     the snapshot, renders on GET and falls back on DELETE (live hot key)
//   - retired embed row (β7): a psql-shaped row on a cut embed key is dropped
//     with the unknown-key WARN and flushes NOTHING — the settings-side
//     embed-cache flush left with the tuple, an admitted live write is the
//     negative probe next to it
//   - NOTIFY handler: events.SettingsWriteHandler reloads the snapshot
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsAPI -count=1 -v
package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
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
	// gives DELETE a visible env revert target; CONTEXT_DB_PASSWORD carries the
	// leak-scan marker.
	//
	// MARKER VEHICLE (β8): the marker used to ride on CTX_CHAT_API_KEY. With
	// the chat tuple gone, server.db_password is the registry's ONLY sensitive
	// key — secret:"presence", env-only, mut:"restart". That makes it the right
	// carrier for the env half of §3.4 (an env-sourced sensitive value renders
	// as maskedEnvValue on every surface) and, at the same time, structurally
	// unable to carry the settings-row half: admitOverride drops a restart-class
	// row before it can reach a snapshot.
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.4")
	const envMarker = "ENV-PLAINTEXT-MARKER-aaaaaaaaaaaaaaaaaaaaaaaa"
	t.Setenv("CONTEXT_DB_PASSWORD", envMarker)

	// Master key so settings.Reload runs its real crypto wiring. The SEALED
	// SECRET this fixture used to plant (prov-main + a resolved-plaintext
	// marker) went with the chat tuple in β8: no registry key is sensitive any
	// more, so nothing on the settings side can hold a secret_ref, and a scan
	// for its plaintext in a settings response would be green by construction.
	// The sealed-secret strecke is pinned where it still has a subject — the
	// secrets API and the backend pool (sealbox_handler_integration_test.go,
	// sealbox_tenant_integration_test.go) and, at config level, on the
	// injected-registry vehicle (config/synthreg_test.go).
	rawKey := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(rawKey); err != nil {
		t.Fatalf("rand: %v", err)
	}
	t.Setenv(sealbox.EnvKey, hex.EncodeToString(rawKey))
	t.Setenv(sealbox.EnvKeyPrev, "")

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

	// SecretRefGate died with the chat tuple in β8. It pinned the ORDER of two
	// gates on a sensitive key: the superseded 409 fires BEFORE any secret_ref
	// format/existence check, so a provider-key-shaped plaintext could never
	// reach table or audit through PUT /api/settings/{key}.
	//
	// It has no vehicle left, on either half of its subject. chat.api_key was
	// the registry's LAST secret:"fp" key, and it was also the last key
	// carrying the superseded marker — so after the cut there is no key that is
	// sensitive, and none that is superseded either. server.db_password is the
	// only sensitive key left, but it is mut:"restart": HandlePut answers the
	// restart-only 409 and admitOverride drops any row on it, so it can never
	// reach the secret_ref gate this test stood in front of. What the retired
	// keys answer now is a plain 404 (unknown key, decision E13) — pinned in a
	// later wave, deliberately not here.
	//
	// Where the statements live now:
	//   - secret_ref resolution + the ref-value leak guard: the injected-
	//     registry vehicle, config/synthreg_test.go
	//     (TestSynthSecretRefResolutionEndToEnd,
	//     TestSynthSecretRefWarningNeverEchoesTheRefValue).
	//   - the masking rule per source: settings_test.go, on synthetic KeyInfos
	//     (TestRenderEffective_*) plus the env half E2E below.
	//   - the superseded 409 branch itself: nowhere any more. β9 removed the
	//     whole mechanic (E11); the branch it guarded no longer exists, and a
	//     PUT on a retired name answers the ordinary 404.

	t.Run("MaskingE2E_NoPlaintextInAnyResponse", func(t *testing.T) {
		scan := func(rec *httptest.ResponseRecorder, label string) {
			t.Helper()
			if body := rec.Body.String(); strings.Contains(body, envMarker) {
				t.Errorf("%s leaks the env plaintext marker: %s", label, body)
			}
		}

		// GET list while server.db_password is env-sourced: the ONLY sensitive
		// key the registry still has, and the marker must not survive it.
		rec := api.do(t, http.MethodGet, "/api/settings", "")
		scan(rec, "GET list (env-sourced)")
		var list struct {
			Settings []settingView `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("unmarshal list: %v", err)
		}
		var seen bool
		for _, s := range list.Settings {
			if s.Key == "server.db_password" {
				seen = true
				if s.Value != maskedEnvValue {
					t.Errorf("GET server.db_password value = %v, want %q", s.Value, maskedEnvValue)
				}
			}
		}
		if !seen {
			t.Fatal("server.db_password missing from the list — the scan above proved nothing")
		}

		// GET single on the same key: same masking, same scan.
		rec = api.do(t, http.MethodGet, "/api/settings/server.db_password", "")
		scan(rec, "GET single (env-sourced sensitive)")
		resp := api.envelope(t, rec)
		setting, _ := resp["setting"].(map[string]any)
		if setting["value"] != maskedEnvValue || setting["source"] != "env" {
			t.Errorf("GET single value/source = %v/%v, want masked/env", setting["value"], setting["source"])
		}

		// The ROW-SOURCED SENSITIVE half of this subtest died with the chat
		// tuple in β8. It pinned: an SQL-planted secret_ref row is resolved
		// into the snapshot as plaintext, while every read surface renders the
		// harmless ref NAME instead. No registry key can carry that state any
		// more — server.db_password is sensitive but restart-class, so
		// admitOverride drops its row before a snapshot ever sees it, and no
		// other key is sensitive at all. The statement is pinned on synthetic
		// KeyInfos in settings_test.go (TestRenderEffective_DBSensitiveShowsRefName,
		// including the row-vanished presence floor) and, at config level, on
		// the injected-registry vehicle in config/synthreg_test.go
		// (TestSynthSecretRefResolutionEndToEnd, TestSynthMaskSecretPerClass).
		//
		// Its NEGATIVE-PROBE value is kept below on a live non-sensitive key:
		// an SQL-written row must still reach the snapshot, render on GET and
		// fall back on DELETE — that is the pipeline the dead half rode on, and
		// without it "nothing leaked" would be provable by a reload that simply
		// never runs.
	})

	// SQLRowRoundtripOverReload is the surviving half of the old masking E2E:
	// row written around the API (psql/break-glass shape) → reload → snapshot →
	// GET renders it → DELETE → snapshot falls back. digest.mode is hot,
	// global-only and unvalidated, so it carries the pipeline without pulling a
	// validator into the statement.
	t.Run("SQLRowRoundtripOverReload", func(t *testing.T) {
		const rowValue = "sql-planted-mode"
		{
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := store.UpsertSetting(ctx, tx, "digest.mode", store.GlobalScope, json.RawMessage(`"`+rowValue+`"`), nil); err != nil {
				t.Fatalf("upsert digest.mode: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := settings.Reload(ctx, pool, cfgStore); err != nil {
				t.Fatalf("reload: %v", err)
			}
		}
		if got := cfgStore.Snapshot().Digest.Mode; got != rowValue {
			t.Fatalf("snapshot digest.mode = %q, want the planted row value", got)
		}
		rec := api.do(t, http.MethodGet, "/api/settings/digest.mode", "")
		resp := api.envelope(t, rec)
		setting, _ := resp["setting"].(map[string]any)
		if setting["value"] != rowValue || setting["source"] != "db" {
			t.Errorf("GET value/source = %v/%v, want %q/db", setting["value"], setting["source"], rowValue)
		}

		// DELETE reverts to the registry default (no CTX_DIGEST_MODE set).
		rec = api.do(t, http.MethodDelete, "/api/settings/digest.mode", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d body=%s", rec.Code, rec.Body.String())
		}
		resp = api.envelope(t, rec)
		if resp["source"] != "default" || resp["value"] == rowValue {
			t.Errorf("DELETE response = %v/%v, want the default value/source", resp["value"], resp["source"])
		}
		if got := cfgStore.Snapshot().Digest.Mode; got == rowValue {
			t.Errorf("snapshot still carries the deleted row value %q", got)
		}
	})

	// RetiredEmbedRowNoFlush is the β7 gate of design/01 §7 W6 —
	// "embed-bezogene Row-Änderung erzeugt WARN-Drop und KEINEN Flush" — and
	// the successor of X2FlushProbe, which died with the settings-side flush.
	//
	// What X2FlushProbe asserted: a PUT on embed.host is 409-closed and flushes
	// nothing, a non-coupled write flushes nothing, and the DELETE revert of a
	// legacy override row DOES flush, because it moved the effective embed
	// host. The last of those three is what β7 removed — the effective embed
	// host no longer exists as a config value, so a row on the key cannot move
	// anything and Reload has no flush left to owe. This test states the new
	// end condition on the same real pipeline: SQL row on a cut key ⇒ dropped
	// with a WARN ⇒ cache untouched, on a cache that was deliberately seeded
	// first so an accidental flush would be visible as a count of 0.
	t.Run("RetiredEmbedRowNoFlush", func(t *testing.T) {
		seedCache()
		if cacheCount() != 1 {
			t.Fatalf("seed failed")
		}
		// Negative probe kept from X2FlushProbe: a live, admitted write must
		// not flush either — otherwise "no flush" would be provable by a
		// reload path that simply never runs.
		rec := api.do(t, http.MethodPut, "/api/settings/rerank.blend_weight", `{"value":0.6}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d", rec.Code)
		}
		if cacheCount() != 1 {
			t.Errorf("admitted non-embed write flushed the cache")
		}

		// The row shape the field will actually meet: written around the API
		// (psql / a pre-cut binary), then picked up by a reload.
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

		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		reloadErr := settings.Reload(ctx, pool, cfgStore)
		slog.SetDefault(prevLogger)
		if reloadErr != nil {
			t.Fatalf("reload: %v", reloadErr)
		}

		out := logBuf.String()
		if !strings.Contains(out, "embed.host") || !strings.Contains(out, "unknown settings key") {
			t.Errorf("log = %q, want the retired embed.host row dropped with the unknown-key WARN", out)
		}
		if strings.Contains(out, "flushed context_embed_cache") {
			t.Errorf("log = %q, want no flush line — the settings-side flush left with the tuple", out)
		}
		if cacheCount() != 1 {
			t.Errorf("a dropped embed.host row flushed the cache — nothing on the settings side may still do that")
		}

		// And the DELETE of that row — the one path that DID flush before β7 —
		// is now an ordinary unknown-key 404 that leaves the cache alone.
		rec = api.do(t, http.MethodDelete, "/api/settings/embed.host", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("DELETE embed.host status = %d, want 404 (unknown key, E13)", rec.Code)
		}
		if cacheCount() != 1 {
			t.Errorf("DELETE on a retired key flushed the cache")
		}
	})

	t.Run("NotifyHandlerReloadsSnapshot", func(t *testing.T) {
		// Naked SQL write (the psql/break-glass shape), then the listener
		// handler — the snapshot must pick the value up without any API call.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		// Vehicle swap (β8): chat.model left the registry; dream.language is a
		// live hot key and carries the same statement unchanged.
		if err := store.UpsertSetting(ctx, tx, "dream.language", store.GlobalScope, json.RawMessage(`"pt-br"`), nil); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// Settings-entity payload: the backend pool is not consulted on this
		// path (G25 added the parameter for entity=context_backends reloads).
		h := events.NewSettingsWriteHandler(pool, cfgStore, backends.NewPool(pool, nil), nil)
		if err := h.HandleNotification(ctx, &pgconn.Notification{Channel: "ctx_settings_write", Payload: `{"entity":"context_settings","key":"dream.language","op":"INSERT"}`}, nil); err != nil {
			t.Fatalf("notify handler: %v", err)
		}
		if got := cfgStore.Snapshot().Dream.Language; got != "pt-br" {
			t.Errorf("snapshot dream.language = %q, want pt-br (NOTIFY reload path)", got)
		}
	})
}
