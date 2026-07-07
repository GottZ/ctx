//go:build integration

// Integration probes for Web-UX U01-W3 (disable-profile manage actions + the
// eject/gaming shim) against a real PG18 testcontainer. Each sub-probe maps to
// a §7-W3 gate (a–g); the wiring is a DB-backed pool + a ManageHandler with a
// per-request AuthResult, driving /api/manage exactly like the production path.
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/handler/ \
//	  -run 'TestDisableProfile|TestEjectShim' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type profileHarness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	bp    *backends.Pool
	h     *ManageHandler
	ctx   context.Context
	byID  map[string]string // name → backend id
	admin *auth.AuthResult
	ten   *auth.AuthResult
}

func insertBackendRoles(t *testing.T, pool *pgxpool.Pool, name, scope string, roles []string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_backends (name, base_url, scope, roles) VALUES ($1,$2,$3,$4) RETURNING id`,
		name, "http://"+name, scope, roles).Scan(&id); err != nil {
		t.Fatalf("insert backend %s: %v", name, err)
	}
	return id
}

func setupProfileHarness(t *testing.T) *profileHarness {
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

	// _global backends: two chat (chat stays served when one is disabled), one
	// SOLE rerank (disabling it blacks out rerank), one SOLE _global embed.
	byID := map[string]string{}
	byID["chat-a"] = insertBackendRoles(t, pool, "chat-a", "_global", []string{"chat"})
	byID["chat-b"] = insertBackendRoles(t, pool, "chat-b", "_global", []string{"chat"})
	byID["only-rerank"] = insertBackendRoles(t, pool, "only-rerank", "_global", []string{"rerank"})
	byID["gpu-embed"] = insertBackendRoles(t, pool, "gpu-embed", "_global", []string{"embed"})
	// tenant-private backends (scope tenant-a): a chat + an embed. The embed is
	// the ONLY embed anywhere besides the _global gpu-embed — gate e proves it
	// does NOT save the _global blackout.
	byID["tenant-chat"] = insertBackendRoles(t, pool, "tenant-chat", "tenant-a", []string{"chat"})
	byID["tenant-embed"] = insertBackendRoles(t, pool, "tenant-embed", "tenant-a", []string{"embed"})

	bp := backends.NewPool(pool, nil)
	if err := bp.Reload(ctx); err != nil {
		t.Fatalf("pool reload: %v", err)
	}

	// config store + synchronous settings reload for the shim dual-write path.
	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(envCfg)
	if err := settings.Reload(ctx, pool, cfgStore); err != nil {
		t.Fatalf("settings reload: %v", err)
	}
	reload := func(ctx context.Context) error { return settings.Reload(ctx, pool, cfgStore) }

	h := NewManageHandler(pool, cfgStore, nil, bp, nil, reload, nil, nil)

	// Real key rows so the actor id is a valid UUID (the 092 audit trigger casts
	// ctx.api_key_id::uuid on every write) and audit attribution resolves.
	// The key's home scope is irrelevant here (only a valid UUID actor id is
	// needed); the AuthResult HomeScope/tier is set independently below.
	adminKey, _, err := store.CreateApiKey(ctx, pool, "srv-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}
	tenKey, _, err := store.CreateApiKey(ctx, pool, "ten-admin", "tenant-a", nil, "")
	if err != nil {
		t.Fatalf("create tenant key: %v", err)
	}
	admin := &auth.AuthResult{
		ApiKeyID: adminKey.ID, HomeScope: "_global", ReadScopes: []string{"_global"},
		IsValid: true, IsAdmin: true,
	}
	ten := &auth.AuthResult{
		ApiKeyID: tenKey.ID, HomeScope: "tenant-a", TenantID: "tenant-a",
		TenantRole: auth.RoleAdmin, ReadScopes: []string{"tenant-a"},
		IsValid: true, IsAdmin: false,
	}
	return &profileHarness{t: t, pool: pool, bp: bp, h: h, ctx: ctx, byID: byID, admin: admin, ten: ten}
}

// do drives one /api/manage request as ar with the given action + data map.
func (ph *profileHarness) do(ar *auth.AuthResult, action string, data map[string]any) *httptest.ResponseRecorder {
	ph.t.Helper()
	body := map[string]any{"action": action}
	if data != nil {
		body["data"] = data
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(ph.ctx, authResultKey, ar))
	rec := httptest.NewRecorder()
	ph.h.HandleManage(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return m
}

// createProfileSQL seeds one _global profile + one member (setup determinism,
// separate from the create-action gates).
func (ph *profileHarness) createProfileSQL(name string, active bool, memberName string) {
	ph.t.Helper()
	var pid string
	if err := ph.pool.QueryRow(ph.ctx,
		`INSERT INTO context_disable_profiles (scope,name,label,active) VALUES ('_global',$1,$2,$3) RETURNING id`,
		name, name, active).Scan(&pid); err != nil {
		ph.t.Fatalf("seed profile %s: %v", name, err)
	}
	if memberName != "" {
		if _, err := ph.pool.Exec(ph.ctx,
			`INSERT INTO context_disable_profile_backends (profile_id, backend_id) VALUES ($1,$2)`,
			pid, ph.byID[memberName]); err != nil {
			ph.t.Fatalf("seed member: %v", err)
		}
	}
}

// Gate (a): toggling a profile active whose activation blacks out a role, WITHOUT
// confirm_role_blackout, is 422 with the role list. Control: a profile that does
// NOT black out any role (chat survives on chat-b) toggles to 200.
func TestDisableProfileToggle_BlackoutRequiresConfirm(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("rr", false, "only-rerank") // sole rerank → blackout
	ph.createProfileSQL("cc", false, "chat-a")      // chat-b remains → no blackout

	rec := ph.do(ph.admin, "disable-profile-toggle", map[string]any{"name": "rr", "active": true})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blackout toggle without confirm = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	roles, _ := m["roles_blacked_out"].([]any)
	if len(roles) != 1 || roles[0] != "rerank" {
		t.Errorf("roles_blacked_out = %v, want [rerank]", m["roles_blacked_out"])
	}

	// Control: no blackout → 200.
	rec = ph.do(ph.admin, "disable-profile-toggle", map[string]any{"name": "cc", "active": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("non-blackout toggle = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// With explicit confirm the blackout toggle succeeds (activation stays possible).
	rec = ph.do(ph.admin, "disable-profile-toggle", map[string]any{"name": "rr", "active": true, "confirm_role_blackout": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("blackout toggle WITH confirm = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// Gate (e): roles_blacked_out is computed against the _global backend set, so a
// role served only by a tenant-private backend (tenant-embed) still counts as
// blacked out when the sole _global embed backend is disabled. dry_run inspects
// the impact without writing.
func TestDisableProfileToggle_BlackoutAgainstGlobalOnly(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("ee", false, "gpu-embed") // sole _global embed

	rec := ph.do(ph.admin, "disable-profile-toggle", map[string]any{"name": "ee", "active": true, "dry_run": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry_run = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	impact, _ := m["impact"].(map[string]any)
	roles, _ := impact["roles_blacked_out"].([]any)
	found := false
	for _, r := range roles {
		if r == "embed" {
			found = true
		}
	}
	if !found {
		t.Errorf("roles_blacked_out = %v, want to contain embed (tenant-embed must NOT save it)", roles)
	}
	if impact["embed_degraded"] != true {
		t.Errorf("embed_degraded = %v, want true", impact["embed_degraded"])
	}
	// dry_run must not have written.
	p, _ := store.GetDisableProfile(ph.ctx, ph.pool, "_global", "ee")
	if p.Active {
		t.Error("dry_run persisted active=true — must be a no-op write")
	}
}

// Gate (d): deleting the reserved eject profile is 422.
func TestDisableProfileDelete_ReservedGuard(t *testing.T) {
	ph := setupProfileHarness(t)
	rec := ph.do(ph.admin, "disable-profile-delete", map[string]any{"name": "eject"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("delete reserved eject = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	// It still exists.
	p, _ := store.GetDisableProfile(ph.ctx, ph.pool, "_global", "eject")
	if p == nil {
		t.Fatal("eject profile was deleted despite the reserved guard")
	}
}

// Gate (c): the eject/gaming shim bypasses the blackout gate — `eject-mode on`
// and `gaming-mode on` return 200 even when the eject profile blacks out rerank
// (legacy gaming was never blackout-gated).
func TestEjectShim_NoBlackout422(t *testing.T) {
	ph := setupProfileHarness(t)
	// Make the eject profile's member the sole rerank → activating blacks out rerank.
	var ejectID string
	if err := ph.pool.QueryRow(ph.ctx, `SELECT id FROM context_disable_profiles WHERE scope='_global' AND name='eject'`).Scan(&ejectID); err != nil {
		t.Fatalf("get eject id: %v", err)
	}
	if _, err := ph.pool.Exec(ph.ctx,
		`INSERT INTO context_disable_profile_backends (profile_id, backend_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		ejectID, ph.byID["only-rerank"]); err != nil {
		t.Fatalf("add eject member: %v", err)
	}

	for _, action := range []string{"eject-mode", "gaming-mode"} {
		rec := ph.do(ph.admin, action, map[string]any{"mode": "on"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s on = %d, want 200 (shim bypasses blackout gate); body=%s", action, rec.Code, rec.Body.String())
		}
	}
	// The dual-write landed: eject profile active AND gaming.active set.
	p, _ := store.GetDisableProfile(ph.ctx, ph.pool, "_global", "eject")
	if !p.Active {
		t.Error("eject profile not active after shim on")
	}
	var gaming string
	if err := ph.pool.QueryRow(ph.ctx, `SELECT value::text FROM context_settings WHERE key='gaming.active' AND scope='_global'`).Scan(&gaming); err != nil {
		t.Fatalf("read gaming.active: %v", err)
	}
	if gaming != "true" {
		t.Errorf("gaming.active = %q, want true (dual-write)", gaming)
	}
}

// Gate (f): eject-mode read == gaming-mode read (byte-identical shape) and both
// reflect the eject profile state.
func TestEjectShim_ReadShapeIdentical(t *testing.T) {
	ph := setupProfileHarness(t)
	// Flip on so the shape carries active + a member name.
	var ejectID string
	_ = ph.pool.QueryRow(ph.ctx, `SELECT id FROM context_disable_profiles WHERE scope='_global' AND name='eject'`).Scan(&ejectID)
	_, _ = ph.pool.Exec(ph.ctx, `INSERT INTO context_disable_profile_backends (profile_id, backend_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, ejectID, ph.byID["only-rerank"])
	if rec := ph.do(ph.admin, "eject-mode", map[string]any{"mode": "on"}); rec.Code != http.StatusOK {
		t.Fatalf("eject-mode on = %d; body=%s", rec.Code, rec.Body.String())
	}

	ejectRead := ph.do(ph.ten, "eject-mode", map[string]any{}) // tierOpen read
	gamingRead := ph.do(ph.ten, "gaming-mode", map[string]any{})
	if ejectRead.Code != http.StatusOK || gamingRead.Code != http.StatusOK {
		t.Fatalf("reads = %d / %d, want 200/200", ejectRead.Code, gamingRead.Code)
	}
	if !bytes.Equal(ejectRead.Body.Bytes(), gamingRead.Body.Bytes()) {
		t.Fatalf("eject-mode read != gaming-mode read:\n eject=%s\ngaming=%s", ejectRead.Body.String(), gamingRead.Body.String())
	}
	m := decode(t, ejectRead)
	g, _ := m["gaming"].(map[string]any)
	if g["active"] != true {
		t.Errorf("read active = %v, want true (reflects eject profile)", g["active"])
	}
	db, _ := g["disabled_backends"].([]any)
	if len(db) != 1 || db[0] != "only-rerank" {
		t.Errorf("disabled_backends = %v, want [only-rerank]", g["disabled_backends"])
	}
}

// Gate (b): the shim dual-write is atomic. A forced failure at the commit
// boundary (beforeGamingCommit seam) leaves NEITHER the profile nor the settings
// row written — no divergent state.
func TestEjectShim_DualWriteAtomic(t *testing.T) {
	ph := setupProfileHarness(t)
	beforeGamingCommit = func() error { return errors.New("forced commit-boundary failure") }
	defer func() { beforeGamingCommit = nil }()

	rec := ph.do(ph.admin, "eject-mode", map[string]any{"mode": "on"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("forced-failure toggle = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	// Neither row moved: eject.active still false AND no gaming.active=true.
	p, _ := store.GetDisableProfile(ph.ctx, ph.pool, "_global", "eject")
	if p.Active {
		t.Error("eject.active=true after a rolled-back dual-write (divergent!)")
	}
	var gaming *string
	_ = ph.pool.QueryRow(ph.ctx, `SELECT value::text FROM context_settings WHERE key='gaming.active' AND scope='_global'`).Scan(&gaming)
	if gaming != nil && *gaming == "true" {
		t.Error("gaming.active=true after a rolled-back dual-write (divergent!)")
	}
}

// Gate (g), AM-5: tenant-admin scope rules. Own-scope profile with an own
// backend = 200; the same create with a _global backend = 422; toggling a
// _global profile = 404 (uniform no-oracle); server-admin may toggle it.
func TestDisableProfile_TenantScopeRules(t *testing.T) {
	ph := setupProfileHarness(t)
	ph.createProfileSQL("cc", false, "chat-a") // a _global profile

	// tenant-admin creates a profile with its OWN backend → 200, scope forced tenant-a.
	rec := ph.do(ph.ten, "disable-profile-create", map[string]any{
		"name": "mine", "members": []string{"tenant-chat"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant create own = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	created, _ := store.GetDisableProfile(ph.ctx, ph.pool, "tenant-a", "mine")
	if created == nil {
		t.Fatal("tenant profile 'mine' not created in scope tenant-a")
	}

	// tenant-admin creates a profile naming a _global backend → 422 (AM-5).
	rec = ph.do(ph.ten, "disable-profile-create", map[string]any{
		"name": "bad", "members": []string{"chat-a"},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant create with _global member = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// tenant-admin toggling a _global profile (explicit scope) → 404 (no oracle).
	rec = ph.do(ph.ten, "disable-profile-toggle", map[string]any{"name": "cc", "scope": "_global", "active": true})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("tenant toggle _global = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	// server-admin may toggle the _global profile (cc does not black out chat).
	rec = ph.do(ph.admin, "disable-profile-toggle", map[string]any{"name": "cc", "active": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("server-admin toggle _global = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// tenant-admin list sees _global 'cc' (read) + own 'mine'.
	rec = ph.do(ph.ten, "disable-profile-list", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant list = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	profiles, _ := m["profiles"].([]any)
	names := map[string]bool{}
	for _, p := range profiles {
		pm, _ := p.(map[string]any)
		names[pm["name"].(string)] = true
	}
	if !names["cc"] || !names["mine"] {
		t.Errorf("tenant list names = %v, want cc (read) + mine (own)", names)
	}
}
