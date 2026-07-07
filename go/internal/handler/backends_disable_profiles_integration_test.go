//go:build integration

// Integration probes for Web-UX U01-W4 (backendSpec.disable_profiles + join-sync
// + scope rule, design/01 §4.3/§5.2). They reuse the U01-W3 profileHarness (real
// PG18 testcontainer, DB-backed pool + ManageHandler) and drive backend-update /
// backend-create exactly like production. Each sub-probe maps to a §7-W4 gate.
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/handler/ \
//	  -run 'TestBackendDisableProfiles' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/jackc/pgx/v5"
)

// doRaw drives one /api/manage request as ar with a fully-formed body map (the W3
// `do` helper only carries action+data; backend-* additionally needs a top-level
// id, so this variant takes the whole body).
func (ph *profileHarness) doRaw(ar *auth.AuthResult, body map[string]any) *httptest.ResponseRecorder {
	ph.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(ph.ctx, authResultKey, ar))
	rec := httptest.NewRecorder()
	ph.h.HandleManage(rec, req)
	return rec
}

// doID drives one /api/manage request as ar with a top-level id (backend-* needs
// it) plus the given action + data map.
func (ph *profileHarness) doID(ar *auth.AuthResult, action, id string, data map[string]any) *httptest.ResponseRecorder {
	ph.t.Helper()
	body := map[string]any{"action": action, "id": id}
	if data != nil {
		body["data"] = data
	}
	return ph.doRaw(ar, body)
}

// seedProfileScoped inserts one profile in an arbitrary scope (createProfileSQL
// is _global-only) and returns its id.
func (ph *profileHarness) seedProfileScoped(scope, name string, active bool) string {
	ph.t.Helper()
	var id string
	if err := ph.pool.QueryRow(ph.ctx,
		`INSERT INTO context_disable_profiles (scope,name,label,active) VALUES ($1,$2,$2,$3) RETURNING id`,
		scope, name, active).Scan(&id); err != nil {
		ph.t.Fatalf("seed profile %s/%s: %v", scope, name, err)
	}
	return id
}

// reloadPool refreshes the pool snapshot after SQL-seeding profiles (the SQL
// bypasses the handler's post-mutation reload; resolveDisableProfiles reads the
// live snapshot, exactly as production does after disable-profile-create fires
// reloadAfterMutation).
func (ph *profileHarness) reloadPool() {
	ph.t.Helper()
	if err := ph.bp.Reload(ph.ctx); err != nil {
		ph.t.Fatalf("pool reload: %v", err)
	}
}

// memberProfileNames reads the CURRENT join truth for one backend (by harness
// name), ORDER BY profile name — independent of the pool snapshot.
func (ph *profileHarness) memberProfileNames(backendName string) []string {
	ph.t.Helper()
	rows, err := ph.pool.Query(ph.ctx, `
		SELECT p.name FROM context_disable_profile_backends m
		  JOIN context_disable_profiles p ON p.id = m.profile_id
		 WHERE m.backend_id = $1 ORDER BY p.name`, ph.byID[backendName])
	if err != nil {
		ph.t.Fatalf("read membership of %s: %v", backendName, err)
	}
	defer rows.Close()
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		ph.t.Fatalf("collect membership of %s: %v", backendName, err)
	}
	return names
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Gate (a)+(g): a tenant-admin attaches its OWN backend to a _global profile
// (self-disable, §5.2) → 200 + join exists; and to its OWN-scope profile → 200.
func TestBackendDisableProfiles_TenantAttachesOwn(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("gp", false, "")            // _global profile, inactive
	ph.seedProfileScoped("tenant-a", "mine", false) // own-scope profile
	ph.reloadPool()

	rec := ph.doID(ph.ten, "backend-update", ph.byID["tenant-chat"],
		map[string]any{"disable_profiles": []string{"gp", "mine"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant attach own backend to _global+own profile = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := ph.memberProfileNames("tenant-chat"); !eqStrs(got, []string{"gp", "mine"}) {
		t.Fatalf("membership = %v, want [gp mine]", got)
	}
	// The response echoes disable_profiles (sorted names).
	m := decode(t, rec)
	b, _ := m["backend"].(map[string]any)
	dp, _ := b["disable_profiles"].([]any)
	if len(dp) != 2 || dp[0] != "gp" || dp[1] != "mine" {
		t.Errorf("response disable_profiles = %v, want [gp mine]", b["disable_profiles"])
	}
}

// Gate (b): a tenant-admin updating a FOREIGN (_global) backend is a uniform 404
// (no membership write reached, no oracle) — the row scope gate fires first.
func TestBackendDisableProfiles_ForeignRow404(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("gp", false, "")

	rec := ph.doID(ph.ten, "backend-update", ph.byID["chat-a"],
		map[string]any{"disable_profiles": []string{"gp"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("tenant update of _global backend = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := ph.memberProfileNames("chat-a"); len(got) != 0 {
		t.Errorf("membership of chat-a = %v, want empty (write must not have reached)", got)
	}
}

// Gate (c): a server-admin may attach any backend to any _global profile.
func TestBackendDisableProfiles_ServerAdminAny(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("gp", false, "")
	ph.reloadPool()

	rec := ph.doID(ph.admin, "backend-update", ph.byID["chat-a"],
		map[string]any{"disable_profiles": []string{"gp"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("server-admin attach = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := ph.memberProfileNames("chat-a"); !eqStrs(got, []string{"gp"}) {
		t.Fatalf("membership = %v, want [gp]", got)
	}
}

// Gate (d): an unknown/invisible profile name ⇒ 422 with the name, no write.
func TestBackendDisableProfiles_UnknownProfile422(t *testing.T) {
	ph := setupProfileHarness(t)

	rec := ph.doID(ph.admin, "backend-update", ph.byID["chat-a"],
		map[string]any{"disable_profiles": []string{"does-not-exist"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown profile = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if msg, _ := m["error"].(string); !strings.Contains(msg, "does-not-exist") {
		t.Errorf("error = %q, want to name the unknown profile", m["error"])
	}

	// A tenant-admin naming a FOREIGN tenant's profile is equally 422 (invisible).
	// Reload so the profile IS in the snapshot — this proves the visibility filter,
	// not mere staleness.
	ph.seedProfileScoped("tenant-b", "secret", false)
	ph.reloadPool()
	rec = ph.doID(ph.ten, "backend-update", ph.byID["tenant-chat"],
		map[string]any{"disable_profiles": []string{"secret"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant naming foreign-tenant profile = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// Gate (e): nil vs [] semantics. An update WITHOUT the disable_profiles key leaves
// the membership untouched; an update WITH an empty array clears it.
func TestBackendDisableProfiles_NilVsEmpty(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("gp", false, "")
	ph.reloadPool()

	// Arm: attach chat-a to gp.
	if rec := ph.doID(ph.admin, "backend-update", ph.byID["chat-a"],
		map[string]any{"disable_profiles": []string{"gp"}}); rec.Code != http.StatusOK {
		t.Fatalf("arm attach = %d; body=%s", rec.Code, rec.Body.String())
	}

	// nil (key absent): an unrelated field-only update must NOT touch membership.
	if rec := ph.doID(ph.admin, "backend-update", ph.byID["chat-a"],
		map[string]any{"priority": 7}); rec.Code != http.StatusOK {
		t.Fatalf("no-op update = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := ph.memberProfileNames("chat-a"); !eqStrs(got, []string{"gp"}) {
		t.Fatalf("after absent-key update membership = %v, want [gp] (nil = no change)", got)
	}

	// [] (key present, empty): clears all memberships.
	if rec := ph.doID(ph.admin, "backend-update", ph.byID["chat-a"],
		map[string]any{"disable_profiles": []string{}}); rec.Code != http.StatusOK {
		t.Fatalf("clear update = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := ph.memberProfileNames("chat-a"); len(got) != 0 {
		t.Fatalf("after []-update membership = %v, want empty ([] = remove all)", got)
	}
}

// Gate (f): backend-list carries disable_profiles including membership in an
// INACTIVE profile (the W6 checkbox dialog needs it). Also proves it is distinct
// from disabled_by_profiles (which stays absent while the profile is inactive).
func TestBackendDisableProfiles_ListShowsInactiveMembership(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("gp", false, "") // INACTIVE
	ph.reloadPool()
	if rec := ph.doID(ph.admin, "backend-update", ph.byID["chat-a"],
		map[string]any{"disable_profiles": []string{"gp"}}); rec.Code != http.StatusOK {
		t.Fatalf("attach = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec := ph.do(ph.admin, "backend-list", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("backend-list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	list, _ := m["backends"].([]any)
	var chatA map[string]any
	for _, e := range list {
		bm, _ := e.(map[string]any)
		if bm["name"] == "chat-a" {
			chatA = bm
		}
	}
	if chatA == nil {
		t.Fatal("chat-a not in backend-list")
	}
	dp, _ := chatA["disable_profiles"].([]any)
	if len(dp) != 1 || dp[0] != "gp" {
		t.Errorf("chat-a disable_profiles = %v, want [gp] (inactive membership must show)", chatA["disable_profiles"])
	}
	// disabled_by_profiles is the ACTIVE subset — absent while gp is inactive.
	if _, present := chatA["disabled_by_profiles"]; present {
		t.Errorf("disabled_by_profiles present while gp inactive: %v", chatA["disabled_by_profiles"])
	}
}

// backend-create also accepts disable_profiles (join synced in the insert tx).
func TestBackendDisableProfiles_CreateAttaches(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("gp", false, "")
	ph.reloadPool()

	rec := ph.doRaw(ph.admin, map[string]any{
		"action": "backend-create",
		"data": map[string]any{
			"name": "fresh", "base_url": "http://fresh", "roles": []string{"chat"},
			"model_map":        map[string]any{"default": "stub-model"},
			"disable_profiles": []string{"gp"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create with disable_profiles = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var pid string
	if err := ph.pool.QueryRow(ph.ctx, `
		SELECT p.name FROM context_disable_profile_backends m
		  JOIN context_disable_profiles p ON p.id = m.profile_id
		  JOIN context_backends b ON b.id = m.backend_id
		 WHERE b.name = 'fresh'`).Scan(&pid); err != nil {
		t.Fatalf("read fresh membership: %v", err)
	}
	if pid != "gp" {
		t.Errorf("fresh membership = %q, want gp", pid)
	}
}
