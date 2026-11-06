//go:build integration

// W06-11 (Fremd-Review 2, R2-1): what a settings PUT answers under V48, the
// rule between gateway.consumer_concurrency_default (C) and
// dispatch.external_queue_per_consumer (the cap). V48 hangs its issue on C
// alone, so the two keys answer differently — pinned here over the real
// handler → store → reload pipeline:
//
//	go test -tags=integration ./internal/handler/ -run TestSettingsAPI_ConsumerConcurrency -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestSettingsAPI_ConsumerConcurrencyAgainstTheQueueCap: a PUT of C that
// breaks V48 is a 422 and changes nothing. A PUT that lowers the cap under a
// stored C is a 200 with a warning — the stored C is dropped and C falls back
// to the default, so the effective C never exceeds the cap — and a 422 only
// when the default breaks the rule too. With the cap off (0) C has no ceiling.
func TestSettingsAPI_ConsumerConcurrencyAgainstTheQueueCap(t *testing.T) {
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
	actor, _, err := store.CreateApiKey(ctx, pool, "w06-11-actor", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatal(envIssues)
	}
	cfgStore := config.NewStore(envCfg)
	ar := &auth.AuthResult{ApiKeyID: actor.ID, HomeScope: "private", ReadScopes: []string{"private"}, IsValid: true, IsAdmin: true}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountSettings(router, NewSettingsHandler(pool, cfgStore))
	api := &settingsAPI{router: router, cfg: cfgStore, pool: pool}

	const conc, queueCap = "gateway.consumer_concurrency_default", "dispatch.external_queue_per_consumer"
	effective := func() (int, int) {
		c := cfgStore.Snapshot()
		return c.Gateway.ConsumerConcurrencyDefault, c.Dispatch.ExternalQueuePerConsumer
	}
	if c, q := effective(); c != 2 || q != 8 {
		t.Fatalf("precondition: defaults C %d, cap %d, want 2 and 8", c, q)
	}
	for i, st := range []struct {
		key, val        string
		status          int
		wantC, wantCap  int
		dropsTheStoredC bool
	}{
		{conc, "0", http.StatusUnprocessableEntity, 2, 8, false},
		{conc, "-1", http.StatusUnprocessableEntity, 2, 8, false},
		{conc, "9", http.StatusUnprocessableEntity, 2, 8, false},
		{conc, "8", http.StatusOK, 8, 8, false},
		{queueCap, "4", http.StatusOK, 2, 4, true},
		{conc, "3", http.StatusOK, 3, 4, false},
		{queueCap, "1", http.StatusUnprocessableEntity, 3, 4, false},
		{queueCap, "0", http.StatusOK, 3, 0, false},
		{conc, "1000", http.StatusOK, 1000, 0, false},
		{queueCap, "8", http.StatusOK, 2, 8, true},
	} {
		rec := api.do(t, http.MethodPut, "/api/settings/"+st.key, `{"value":`+st.val+`}`)
		c, q := effective()
		t.Logf("step %d: PUT %s=%s -> %d, effective C %d, cap %d | %s", i, st.key, st.val, rec.Code, c, q, rec.Body.String())
		if rec.Code != st.status {
			t.Errorf("step %d: PUT %s=%s answers %d, want %d", i, st.key, st.val, rec.Code, st.status)
		}
		if c != st.wantC || q != st.wantCap {
			t.Errorf("step %d: after PUT %s=%s effective C %d, cap %d, want %d and %d", i, st.key, st.val, c, q, st.wantC, st.wantCap)
		}
		dropped := false // a 200 that names the stored C among its warnings
		if rec.Code == http.StatusOK {
			warnings, _ := api.envelope(t, rec)["warnings"].([]any)
			for _, w := range warnings {
				if msg, _ := w.(string); strings.HasPrefix(msg, conc+": settings override dropped") {
					dropped = true
				}
			}
		}
		if dropped != st.dropsTheStoredC {
			t.Errorf("step %d: PUT %s=%s warns that the stored C is dropped: %v, want %v", i, st.key, st.val, dropped, st.dropsTheStoredC)
		}
		if q > 0 && c > q {
			t.Errorf("step %d: effective C %d exceeds the cap %d", i, c, q)
		}
	}
}
