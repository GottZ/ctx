//go:build integration

// §F-ops (2): backend-update {"enabled":false} is the documented instant brake.
// The update path re-judged the WHOLE row through ValidateBackend, the
// embed-equivalence claim and the api_key_ref gate on every write, so a row
// written before one of those gates existed answered 422 to its own brake —
// for a server-admin as well. A payload that is exactly {"enabled":false} now
// skips those gates; every other payload stays fully gated.
//
//	go test -tags=integration ./internal/handler/ -run TestBackendUpdateBrake -count=1 -v
package handler

import (
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

func TestBackendUpdateBrake_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ph := setupProfileHarness(t)
	seedSealedName(t, ph.pool, "openrouter-key", store.GlobalScope)

	// Two tenant rows the way a pre-gate install may hold them, written by SQL
	// (the API refuses both today): one names the operator's _global secret
	// without the tenant's opt-in, one points at a private address.
	legacy := map[string]struct{ id, field string }{}
	for _, row := range []struct{ name, url, ref, field string }{
		{"legacy-ref", "https://legacy.example.com", "openrouter-key", "api_key_ref"},
		{"legacy-lan", "http://10.0.0.5:11434", "", "base_url"},
	} {
		var id string
		if err := ph.pool.QueryRow(ph.ctx,
			`INSERT INTO context_backends (name, base_url, scope, roles, model_map, api_key_ref)
			 VALUES ($1, $2, 'tenant-a', '{chat}', '{"default":"stub-model"}', $3) RETURNING id`,
			row.name, row.url, row.ref).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", row.name, err)
		}
		legacy[row.name] = struct{ id, field string }{id, row.field}
	}
	rearm := func(id string) {
		t.Helper()
		if _, err := ph.pool.Exec(ph.ctx, `UPDATE context_backends SET enabled = true, priority = 0 WHERE id = $1`, id); err != nil {
			t.Fatalf("re-arm: %v", err)
		}
		if err := ph.bp.Reload(ph.ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}
	dbEnabled := func(id string) bool {
		t.Helper()
		var on bool
		if err := ph.pool.QueryRow(ph.ctx, `SELECT enabled FROM context_backends WHERE id = $1`, id).Scan(&on); err != nil {
			t.Fatalf("read enabled: %v", err)
		}
		return on
	}

	for _, actor := range []struct {
		name string
		ar   *auth.AuthResult
	}{{"server-admin", ph.admin}, {"tenant-admin", ph.ten}} {
		for name, row := range legacy {
			t.Run(actor.name+"/"+name, func(t *testing.T) {
				rearm(row.id)

				// Anything beside enabled:false is a full write and still meets the
				// gate the legacy row fails.
				rec := ph.doRaw(actor.ar, map[string]any{"action": "backend-update", "id": row.id,
					"data": map[string]any{"enabled": false, "priority": 5}})
				if rec.Code != http.StatusUnprocessableEntity || !backendFieldErrors(t, rec.Body.String(), row.field) {
					t.Fatalf("enabled:false + priority on %s = %d, want 422 on %s; body=%s", name, rec.Code, row.field, rec.Body.String())
				}
				if !dbEnabled(row.id) {
					t.Fatalf("%s disabled by a 422 write", name)
				}

				// The pure brake goes through. RED before the exception: the same
				// 422 as above.
				rec = ph.doRaw(actor.ar, map[string]any{"action": "backend-update", "id": row.id,
					"data": map[string]any{"enabled": false}})
				if rec.Code != http.StatusOK {
					t.Fatalf("brake on %s = %d, want 200; body=%s", name, rec.Code, rec.Body.String())
				}
				if dbEnabled(row.id) {
					t.Errorf("brake on %s answered 200 but the row is still enabled", name)
				}
				for _, b := range ph.bp.Snapshot() {
					if b.ID == row.id && b.Enabled {
						t.Errorf("brake on %s: the pool snapshot still serves the row", name)
					}
				}

				// Re-arming is not a brake: enabled:true meets the gate again.
				rec = ph.doRaw(actor.ar, map[string]any{"action": "backend-update", "id": row.id,
					"data": map[string]any{"enabled": true}})
				if rec.Code != http.StatusUnprocessableEntity {
					t.Errorf("enabled:true on %s = %d, want 422; body=%s", name, rec.Code, rec.Body.String())
				}
			})
		}
	}
}
