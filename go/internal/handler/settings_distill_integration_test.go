//go:build integration

// Wave A03-W03-3 — the distiller config group on the WIRE. The group's own
// unit probes live in internal/config; what only an end-to-end probe can say is
// the part the wave gate asks for: that the keys actually reach
// GET /api/settings with a description and their default, and that the two
// fail-closed enums produce a 422 through the real handler → store → reload
// pipeline rather than merely a config.Issue in a slice.
//
// The distinction matters because a validation issue becomes a 422 only
// indirectly: config.Build's dropOffenders attributes a SeverityError to its
// override BY FIELD and drops it, and the handler then sees that the write did
// not apply. A check whose Field is not the canonical key would validate
// correctly in a unit test and answer 200 here.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsDistill -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestSettingsDistill_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")

	actor, _, err := store.CreateApiKey(ctx, pool, "settings-distill-actor", "private", nil, "")
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

	// The wave's green gate: EVERY distill key is served, each with a
	// description and its registry default. The probe is the DEFAULT VALUE in
	// the response, not the documentation — a key with a blank description or a
	// missing default would render as a dead row in the settings UI.
	t.Run("GroupIsServedWithDescriptionsAndDefaults", func(t *testing.T) {
		rec := api.do(t, http.MethodGet, "/api/settings", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/settings = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Settings []struct {
				Key        string          `json:"key"`
				Type       string          `json:"type"`
				Mutability string          `json:"mutability"`
				Value      json.RawMessage `json:"value"`
				Default    json.RawMessage `json:"default"`
				Desc       string          `json:"description"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got := map[string]string{}
		for _, s := range resp.Settings {
			if len(s.Key) < 8 || s.Key[:8] != "distill." {
				continue
			}
			if s.Desc == "" {
				t.Errorf("%s reaches the API without a description", s.Key)
			}
			if len(s.Default) == 0 {
				t.Errorf("%s reaches the API without a default", s.Key)
			}
			got[s.Key] = string(s.Value)
		}
		if len(got) == 0 {
			t.Fatal("no distill.* key in GET /api/settings — the group never reached the registry")
		}
		// The values whose drift would be a posture change, asserted as the
		// SERVED value rather than as a struct field. distill.ctx_enabled joined
		// them in wave A02-4: it is the second source's master switch, and an
		// install that never asked for a distiller must not start deriving
		// blocks from its own session transcripts either.
		for key, want := range map[string]string{
			"distill.enabled":           `false`,
			"distill.ctx_enabled":       `false`,
			"distill.block_sensitivity": `"credentials"`,
			"distill.scope":             `""`,
		} {
			if got[key] != want {
				t.Errorf("GET /api/settings %s = %s, want %s", key, got[key], want)
			}
		}
	})

	// V22 on the wire. "shared" is refused, the empty string — the DEFAULT and
	// the inheritance path — is accepted, and the accept is asserted next to
	// the refusal so a check that simply rejected everything would not pass.
	t.Run("ScopeSharedIs422_EmptyAccepted", func(t *testing.T) {
		rec := api.do(t, http.MethodPut, "/api/settings/distill.scope", `{"value":"shared"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT distill.scope=shared = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = 'distill.scope'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("422 persisted a row — validation must run BEFORE persist")
		}

		rec = api.do(t, http.MethodPut, "/api/settings/distill.scope", `{"value":""}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT distill.scope=\"\" = %d, want 200 (the inheritance path); body=%s", rec.Code, rec.Body.String())
		}
		if got := cfgStore.Snapshot().Distill.Scope; got != "" {
			t.Errorf("snapshot distill.scope = %q, want empty", got)
		}
		api.do(t, http.MethodDelete, "/api/settings/distill.scope", "")
	})

	// V23 on the wire. "public" is refused; "personal" — a DOWNGRADE from the
	// credentials default — is a plain accept, which is the observable half of
	// the decision not to tag the key guard:"sensitivity-downgrade".
	t.Run("SensitivityPublicIs422_PersonalAccepted", func(t *testing.T) {
		rec := api.do(t, http.MethodPut, "/api/settings/distill.block_sensitivity", `{"value":"public"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT distill.block_sensitivity=public = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}

		rec = api.do(t, http.MethodPut, "/api/settings/distill.block_sensitivity", `{"value":"personal"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT distill.block_sensitivity=personal = %d, want 200 (no downgrade guard on this key); body=%s",
				rec.Code, rec.Body.String())
		}
		if got := cfgStore.Snapshot().Distill.BlockSensitivity; string(got) != "personal" {
			t.Errorf("snapshot distill.block_sensitivity = %q, want personal (hot effect)", got)
		}
		api.do(t, http.MethodDelete, "/api/settings/distill.block_sensitivity", "")
	})

	// V24 on the wire: the budget coupling refuses an OVERRIDE that the
	// compiled defaults still satisfy. This is the half the static gate in
	// promptguard structurally cannot see.
	t.Run("BudgetCouplingIs422", func(t *testing.T) {
		rec := api.do(t, http.MethodPut, "/api/settings/distill.rows_per_call", `{"value":12}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PUT distill.rows_per_call=12 = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		rec = api.do(t, http.MethodPut, "/api/settings/distill.rows_per_call", `{"value":5}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT distill.rows_per_call=5 = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		api.do(t, http.MethodDelete, "/api/settings/distill.rows_per_call", "")
	})
}
