//go:build integration

// Integration gates for OAuth wave R6 (design/05 §4.6, negativ-geprobt)
// against a real PG18 testcontainer:
//
//   - GET /api/session/keys via cookie session lists BOTH keys of the
//     multi-tenant principal, active_now marks the logged-in one
//   - POST /api/session/select-key onto the second key (WITH csrf token) →
//     200 + NEW csrf_token + NEW cookies; whoami afterwards shows tenant Y
//     (switch happened); the OLD session cookie answers 401 — the switch
//     mints a fresh family and revokes the old one (mint-fresh, never
//     in-place)
//   - INV-A no-union probe: the post-switch AuthResult carries NO X scopes
//   - foreign key (second principal) → 403, the session stays intact
//   - select-key without a cookie session (X-Context-Key header) → 400
//   - select-key without the CSRF token → 403 (the middleware gate)
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSessionKeysR6 -count=1 -v
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// skServer stands up the R6 routes exactly as server.go mounts them: login
// public, keys/select-key INSIDE the Auth group (so the CSRF gate is probed
// through the REAL middleware chain).
func skServer(pool *pgxpool.Pool) *httptest.Server {
	sessH := NewSessionHandler(pool)
	whoH := NewWhoamiHandler(pool)
	r := chi.NewRouter()
	r.Post("/auth/login", sessH.HandleLogin)
	r.Group(func(r chi.Router) {
		r.Use(Auth(pool))
		r.Get("/api/whoami", whoH.HandleWhoami)
		r.Get("/api/session/keys", sessH.HandleSessionKeys)
		r.Post("/api/session/select-key", sessH.HandleSelectKey)
	})
	return httptest.NewServer(r)
}

// skScopes flattens a whoami read_scopes JSON array into a string set.
func skScopes(body map[string]any) map[string]bool {
	set := map[string]bool{}
	if raw, ok := body["read_scopes"].([]any); ok {
		for _, s := range raw {
			if str, ok := s.(string); ok {
				set[str] = true
			}
		}
	}
	return set
}

func TestSessionKeysR6_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Principal P1 with keys in TWO tenants (synthetic multi-tenant fixture,
	// same shape as the S3 INV-A probe) + a second principal P2 with its own
	// key Z for the foreign-key gate.
	var p1, p2 string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('sk-person-1') RETURNING id::text`).Scan(&p1); err != nil {
		t.Fatalf("seed principal 1: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_principals (display_name) VALUES ('sk-person-2') RETURNING id::text`).Scan(&p2); err != nil {
		t.Fatalf("seed principal 2: %v", err)
	}
	keyX := "aa22" + strings.Repeat("ef", 30)
	keyY := "bb33" + strings.Repeat("ef", 30)
	keyZ := "cc44" + strings.Repeat("ef", 30)
	_, keyXID := otSeedTenantKey(t, pool, "sk-x", "sk-scope-x", keyX, p1)
	_, keyYID := otSeedTenantKey(t, pool, "sk-y", "sk-scope-y", keyY, p1)
	_, keyZID := otSeedTenantKey(t, pool, "sk-z", "sk-scope-z", keyZ, p2)

	srv := skServer(pool)
	defer srv.Close()

	// --- login P1 on key X → session 1 ----------------------------------------
	loginResp, loginBody := asDo(t, http.MethodPost, srv.URL+"/auth/login",
		`{"api_key":"`+keyX+`"}`, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d, want 200", loginResp.StatusCode)
	}
	csrf1, _ := loginBody["csrf_token"].(string)
	sess1 := cookieByName(loginResp, sessionCookieName)
	if csrf1 == "" || sess1 == nil {
		t.Fatal("login must deliver csrf_token + session cookie")
	}
	withSess1 := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess1.Value})
	}

	// --- Gate 1: keys list shows BOTH, active_now marks X ----------------------
	listResp, listBody := asDo(t, http.MethodGet, srv.URL+"/api/session/keys", "", withSess1)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("keys list: got %d, want 200", listResp.StatusCode)
	}
	entries, _ := listBody["keys"].([]any)
	if len(entries) != 2 {
		t.Fatalf("keys list: got %d entries, want 2 (both keys of the principal)", len(entries))
	}
	seen := map[string]bool{} // api_key_id → active_now
	for _, e := range entries {
		m, _ := e.(map[string]any)
		id, _ := m["api_key_id"].(string)
		activeNow, _ := m["active_now"].(bool)
		seen[id] = activeNow
		if _, has := m["key_hash"]; has {
			t.Fatal("keys list must not leak key_hash")
		}
	}
	if !seen[keyXID] || seen[keyYID] {
		t.Fatalf("active_now wrong: X=%v (want true) Y=%v (want false)", seen[keyXID], seen[keyYID])
	}
	if _, listed := seen[keyZID]; listed {
		t.Fatal("keys list leaks the foreign principal's key")
	}

	// --- Gate 6: select-key WITHOUT csrf → 403 (middleware gate) ---------------
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/session/select-key",
		`{"api_key_id":"`+keyYID+`"}`, withSess1); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("select-key without CSRF: got %d, want 403", resp.StatusCode)
	}

	// --- Gate 5: select-key without cookie session (header) → 400 --------------
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/session/select-key",
		`{"api_key_id":"`+keyYID+`"}`, func(r *http.Request) {
			r.Header.Set("X-Context-Key", keyX)
		}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("select-key via header credential: got %d, want 400", resp.StatusCode)
	}

	// --- Gate 2: switch to key Y (mint-fresh) ----------------------------------
	selResp, selBody := asDo(t, http.MethodPost, srv.URL+"/api/session/select-key",
		`{"api_key_id":"`+keyYID+`"}`, func(r *http.Request) {
			withSess1(r)
			r.Header.Set("X-CSRF-Token", csrf1)
		})
	if selResp.StatusCode != http.StatusOK {
		t.Fatalf("select-key: got %d, want 200", selResp.StatusCode)
	}
	csrf2, _ := selBody["csrf_token"].(string)
	if csrf2 == "" || csrf2 == csrf1 {
		t.Fatalf("select-key must mint a NEW csrf_token (old=%q new=%q)", csrf1, csrf2)
	}
	sess2 := cookieByName(selResp, sessionCookieName)
	ref2 := cookieByName(selResp, refreshCookieName)
	if sess2 == nil || ref2 == nil || sess2.Value == sess1.Value {
		t.Fatal("select-key must set NEW session+refresh cookies")
	}
	withSess2 := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess2.Value})
	}

	// whoami on the NEW session shows tenant Y — the switch happened.
	whoResp, whoBody := asDo(t, http.MethodGet, srv.URL+"/api/whoami", "", withSess2)
	if whoResp.StatusCode != http.StatusOK {
		t.Fatalf("whoami after switch: got %d, want 200", whoResp.StatusCode)
	}
	if whoBody["tenant_slug"] != "sk-y" || whoBody["api_key_id"] != keyYID {
		t.Fatalf("whoami after switch: tenant_slug=%v api_key_id=%v, want sk-y/%s",
			whoBody["tenant_slug"], whoBody["api_key_id"], keyYID)
	}
	// The OLD session cookie is dead — the switch minted a new family and
	// revoked the old one.
	if resp, _ := asDo(t, http.MethodGet, srv.URL+"/api/whoami", "", withSess1); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session after switch: got %d, want 401 (family revoked)", resp.StatusCode)
	}
	var oldOverlay int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_web_sessions WHERE id = $1`, sess1.Value).Scan(&oldOverlay); err != nil || oldOverlay != 0 {
		t.Fatalf("old overlay after switch: count=%d err=%v, want 0", oldOverlay, err)
	}

	// --- Gate 3: INV-A no-union — no X scopes in the post-switch result --------
	scopes := skScopes(whoBody)
	if scopes["sk-scope-x"] {
		t.Fatalf("INV-A violated: post-switch read_scopes still contain sk-scope-x: %v", scopes)
	}
	if !scopes["sk-scope-y"] {
		t.Fatalf("post-switch read_scopes miss the Y home scope: %v", scopes)
	}

	// --- Gate 4: foreign key (principal P2) → 403, session intact --------------
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/session/select-key",
		`{"api_key_id":"`+keyZID+`"}`, func(r *http.Request) {
			withSess2(r)
			r.Header.Set("X-CSRF-Token", csrf2)
		}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("select-key foreign key: got %d, want 403", resp.StatusCode)
	}
	// Malformed uuid folds into the same reject, no 22P02 500.
	if resp, _ := asDo(t, http.MethodPost, srv.URL+"/api/session/select-key",
		`{"api_key_id":"not-a-uuid"}`, func(r *http.Request) {
			withSess2(r)
			r.Header.Set("X-CSRF-Token", csrf2)
		}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("select-key malformed uuid: got %d, want 403", resp.StatusCode)
	}
	if resp, body := asDo(t, http.MethodGet, srv.URL+"/api/whoami", "", withSess2); resp.StatusCode != http.StatusOK || body["tenant_slug"] != "sk-y" {
		t.Fatalf("session after foreign-key attempt: got %d/%v, want 200/sk-y (intact)", resp.StatusCode, body["tenant_slug"])
	}
}
