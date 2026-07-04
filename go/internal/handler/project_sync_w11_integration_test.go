//go:build integration

// W11 REST sync-trigger gates (design/03 §4.2/§4.4/§4.6, decision E6=a). Drives
// POST/GET /api/project/{id}/sync through the PRODUCTION MountProjectSync chain
// (RequireMember admits + AuthResult injector), so every 200/403/404/409/429 probe
// exercises what server.go wires. Each negative first confirmed RED against the
// removed gate (RED-PROOF notes in the return):
//
//   - POST without WRITE scope (readable-only) ⇒ 403, StartSync never called (E6=a);
//   - POST on a scope the caller cannot READ ⇒ 404 uniform (no existence oracle);
//   - per-project rate limit ⇒ 429 + retry_after_s; a SECOND key of the same project
//     also 429 (budget is per-PROJECT, not per api_key_id — RED with I6 counting);
//   - ErrSyncRunning ⇒ 409, ErrSyncSaturated ⇒ 409 + retry_after_s, ErrNoTenant ⇒ 422;
//   - happy start ⇒ 200 {run}; GET status ⇒ 200 with the register's sync_status.
//
// Run: `go test -tags=integration ./internal/handler/ -run TestProjectSyncW11 -count=1 -v`.
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stubSync is a SyncController that returns a canned status/error and counts
// StartSync calls (to prove a gate refused BEFORE the engine ran).
type stubSync struct {
	startErr error
	status   forge.SyncStatus
	calls    atomic.Int32
}

func (s *stubSync) StartSync(context.Context, store.ProjectRow, bool) (forge.SyncStatus, error) {
	s.calls.Add(1)
	return s.status, s.startErr
}
func (s *stubSync) Status(string) forge.SyncStatus { return s.status }

func w11Do(t *testing.T, pool *pgxpool.Pool, ctrl SyncController, cfg ConfigStore, ar *auth.AuthResult, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	r := chi.NewRouter()
	if ar != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
			})
		})
	}
	MountProjectSync(r, NewProjectSyncHandler(pool, ctrl, cfg))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// cfgWithSyncRate builds a ConfigStore whose project.sync.rate_limit is n.
func cfgWithSyncRate(n int) ConfigStore {
	c := &config.Config{}
	c.Project.Sync.RateLimit = n
	return config.NewStore(c)
}

func TestProjectSyncW11_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	pid, scope := w6SeedProject(t, pool, "w11rest")
	writer := w7Writer(scope)
	syncPath := "/api/project/" + pid + "/sync"

	t.Run("tier_write_scope_gate_403", func(t *testing.T) {
		// A readable-but-not-writable caller (E6=a: sync needs WRITE). RED-PROOF:
		// with the writableBlockScopes check removed this returned 200.
		ctrl := &stubSync{}
		rec := w11Do(t, pool, ctrl, nil, w7ReadOnly(scope), http.MethodPost, syncPath)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-only start: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
		if ctrl.calls.Load() != 0 {
			t.Fatalf("StartSync called %d times on a 403, want 0", ctrl.calls.Load())
		}
	})

	t.Run("foreign_scope_404", func(t *testing.T) {
		// A caller who cannot even READ the project scope ⇒ 404 uniform.
		ar := &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, HomeScope: "x:home", ReadScopes: []string{"x:home"}}
		rec := w11Do(t, pool, &stubSync{}, nil, ar, http.MethodPost, syncPath)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("foreign start: status %d, want 404", rec.Code)
		}
	})

	t.Run("start_200", func(t *testing.T) {
		ctrl := &stubSync{status: forge.SyncStatus{ProjectID: pid, Running: true, RunID: "run-1"}}
		rec := w11Do(t, pool, ctrl, nil, writer, http.MethodPost, syncPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("start: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		if body["success"] != true {
			t.Fatalf("start success=%v, want true", body["success"])
		}
		if _, ok := body["run"].(map[string]any); !ok {
			t.Fatalf("start missing run object (body=%s)", rec.Body.String())
		}
		if ctrl.calls.Load() != 1 {
			t.Fatalf("StartSync called %d times, want 1", ctrl.calls.Load())
		}
	})

	t.Run("rate_limit_429_shared_across_keys", func(t *testing.T) {
		// Seed the rate substrate to the cap (limit=1, one run in the window).
		if _, err := store.StartSyncRun(context.Background(), pool, pid); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		cfg := cfgWithSyncRate(1)
		ctrl := &stubSync{}
		rec := w11Do(t, pool, ctrl, cfg, writer, http.MethodPost, syncPath)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("rate-limited start: status %d, want 429 (body=%s)", rec.Code, rec.Body.String())
		}
		if _, ok := w6DecodeBody(t, rec)["retry_after_s"]; !ok {
			t.Errorf("429 body missing retry_after_s")
		}
		// A DIFFERENT key of the same project shares the budget ⇒ also 429. RED-PROOF:
		// per-api_key_id counting (the I6 dimension) would let this second key start.
		writer2 := w7Writer(scope)
		rec2 := w11Do(t, pool, ctrl, cfg, writer2, http.MethodPost, syncPath)
		if rec2.Code != http.StatusTooManyRequests {
			t.Fatalf("second key start: status %d, want 429 (budget is per-project)", rec2.Code)
		}
		if ctrl.calls.Load() != 0 {
			t.Fatalf("StartSync called %d times on 429, want 0", ctrl.calls.Load())
		}
	})

	t.Run("engine_error_mappings", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			want int
		}{
			{"running_409", forge.ErrSyncRunning, http.StatusConflict},
			{"saturated_409", forge.ErrSyncSaturated, http.StatusConflict},
			{"no_tenant_422", forge.ErrNoTenant, http.StatusUnprocessableEntity},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				ctrl := &stubSync{startErr: tc.err}
				rec := w11Do(t, pool, ctrl, nil, writer, http.MethodPost, syncPath)
				if rec.Code != tc.want {
					t.Fatalf("%s: status %d, want %d (body=%s)", tc.name, rec.Code, tc.want, rec.Body.String())
				}
				if tc.err == forge.ErrSyncSaturated {
					if _, ok := w6DecodeBody(t, rec)["retry_after_s"]; !ok {
						t.Errorf("saturated 409 missing retry_after_s")
					}
				}
			})
		}
	})

	t.Run("status_get_200", func(t *testing.T) {
		rec := w11Do(t, pool, &stubSync{}, nil, writer, http.MethodGet, syncPath)
		if rec.Code != http.StatusOK {
			t.Fatalf("status GET: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		if _, ok := body["sync_status"]; !ok {
			t.Errorf("status GET missing sync_status (body=%s)", rec.Body.String())
		}
		if _, ok := body["recent_runs"]; !ok {
			t.Errorf("status GET missing recent_runs")
		}
	})
}
