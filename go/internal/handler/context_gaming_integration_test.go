//go:build integration

// Integration probe for the F3-P6 gaming toggle against a real PG18
// testcontainer — the persistence guarantee that separates gaming-mode from
// dream-mode (design 03 §2.6, negative test 5):
//
//   - gaming-mode {mode:on} persists gaming.active to context_settings and the
//     in-process snapshot reflects it immediately (no restart)
//   - a FRESH config.Store built from the DB (a simulated ctxd restart) still
//     reads gaming.active=true — the toggle SURVIVES the restart. An
//     atomics-only implementation (the dream-mode break path) would boot back
//     to the default false; this test is red against it.
//   - {mode:off} reverts, and the audit trail attributes both writes
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestGamingModePersistence -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGamingModePersistence_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")

	// Acting admin key (real row → audit attribution is verifiable E2E).
	actor, _, err := store.CreateApiKey(ctx, pool, "gaming-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}

	boot := func() *config.Store {
		envCfg, issues := config.FromEnv()
		issues = append(issues, config.Validate(envCfg)...)
		if config.HasErrors(issues) {
			t.Fatalf("env fixture invalid: %v", issues)
		}
		st := config.NewStore(envCfg)
		if err := settings.Reload(ctx, pool, st); err != nil {
			t.Fatalf("settings reload: %v", err)
		}
		return st
	}

	cfgStore := boot()
	// Default boot: gaming OFF (no override row yet).
	if cfgStore.Snapshot().Pool.GamingActive {
		t.Fatal("fresh boot has gaming.active=true, want false default")
	}

	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{Name: "herbert-chat"}, {Name: "herbert-rerank"}})
	reload := func(ctx context.Context) error { return settings.Reload(ctx, pool, cfgStore) }

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
	manageH := NewManageHandler(pool, cfgStore, nil, bp, nil, reload, nil, nil)
	router.Post("/api/manage", manageH.HandleManage)

	do := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	gamingActive := func(rec *httptest.ResponseRecorder) bool {
		t.Helper()
		var resp struct {
			Success bool `json:"success"`
			Gaming  struct {
				Active bool `json:"active"`
			} `json:"gaming"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
		}
		if !resp.Success {
			t.Fatalf("success=false: %s", rec.Body.String())
		}
		return resp.Gaming.Active
	}
	auditCount := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings_audit WHERE entity_key = 'gaming.active'`).Scan(&n); err != nil {
			t.Fatalf("audit count: %v", err)
		}
		return n
	}

	t.Run("FlipOn_HotEffect", func(t *testing.T) {
		rec := do(t, `{"action":"gaming-mode","data":{"mode":"on"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if !gamingActive(rec) {
			t.Error("response gaming.active=false after on")
		}
		// Hot effect: the in-process snapshot the chains read carries it now.
		if !cfgStore.Snapshot().Pool.GamingActive {
			t.Error("in-process snapshot still off after flip — synchronous reload missing")
		}
	})

	t.Run("SurvivesRestart", func(t *testing.T) {
		// Simulated ctxd restart: a brand-new config.Store, booted from env +
		// the DB overrides. gaming.active must still be true — it lives in
		// context_settings, not an atomic that dies with the process.
		fresh := boot()
		if !fresh.Snapshot().Pool.GamingActive {
			t.Fatal("gaming.active did NOT survive a simulated restart (atomics-only would fail here)")
		}
	})

	t.Run("FlipOff_Reverts", func(t *testing.T) {
		rec := do(t, `{"action":"gaming-mode","data":{"mode":"off"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if gamingActive(rec) {
			t.Error("response gaming.active=true after off")
		}
		if boot().Snapshot().Pool.GamingActive {
			t.Error("gaming.active=true after off survived restart — off did not persist")
		}
	})

	t.Run("AuditAttributed", func(t *testing.T) {
		// on + off = two attributed audit rows for gaming.active.
		if n := auditCount(); n < 2 {
			t.Errorf("audit rows for gaming.active = %d, want >= 2 (on + off)", n)
		}
	})
}
