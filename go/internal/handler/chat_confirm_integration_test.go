//go:build integration

// F6-C6 D-W6b integration probes against a real PG18 testcontainer:
//
//  1. A chat write is ALWAYS staged — default-confirm by birth: the key has
//     NO confirm_writes flag, yet nothing hits context_blocks (DECISIONS
//     §Klarstellung D-E1/E2 — the chat harness has no own gating layer).
//  2. POST /api/confirm with the Bearer header executes the staged write and
//     consumes the row (mounted behind handler.Auth, exactly the production
//     chain).
//  3. A cookie-only request (no auth header) is 401 — the confirm surface is
//     header-auth only (D1-m4); the write stays staged.
//  4. A replayed confirm gets the generic 404 miss (no double execute).
//  5. A gate violation (oversize) rejects before anything is staged.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestChatStageConfirm -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestChatStageConfirm(t *testing.T) {
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
	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(envCfg)
	if err := settings.Reload(ctx, pool, cfgStore); err != nil {
		t.Fatalf("settings reload: %v", err)
	}

	// A plain key WITHOUT confirm_writes — the chat surface must stage anyway.
	keyRow, keyPlain, err := store.CreateApiKey(ctx, pool, "w6b-chat", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	var flagged bool
	if err := pool.QueryRow(ctx, `SELECT confirm_writes FROM context_api_keys WHERE id = $1`, keyRow.ID).Scan(&flagged); err != nil {
		t.Fatalf("read flag: %v", err)
	}
	if flagged {
		t.Fatalf("fixture key must NOT carry confirm_writes — chat staging is flag-independent")
	}

	runner := &chatStageRunner{pool: pool, cfg: cfgStore}
	confirmH := NewConfirmHandler(pool, nil)

	// The PRODUCTION mount chain: Auth middleware, then the route (server.go).
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(Auth(pool))
		r.Post("/api/confirm", confirmH.HandleConfirm)
	})

	authedCtx := func() context.Context {
		req := httptest.NewRequest(http.MethodPost, "/probe", nil)
		req.Header.Set("Authorization", "Bearer "+keyPlain)
		// Resolve over the real middleware path so the AuthResult matches
		// exactly what HandleStream would put in the tool context.
		var got context.Context
		Auth(pool)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = r.Context()
		})).ServeHTTP(httptest.NewRecorder(), req)
		if got == nil || AuthResultFromContext(got) == nil {
			t.Fatalf("auth probe failed to resolve an AuthResult")
		}
		return got
	}

	blockCount := func(title string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("block count: %v", err)
		}
		return n
	}
	openCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL`, keyRow.ID).Scan(&n); err != nil {
			t.Fatalf("open count: %v", err)
		}
		return n
	}
	postConfirm := func(hash string, withAuth bool, cookieOnly bool) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"payload_hash": hash})
		req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+keyPlain)
		}
		if cookieOnly {
			req.AddCookie(&http.Cookie{Name: "ctx_session", Value: keyPlain})
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	var stagedHash string
	t.Run("chat write is ALWAYS staged (default-confirm by birth)", func(t *testing.T) {
		staged, reject, err := runner.StageWrite(authedCtx(), "test", "w6b-block", "chat staged content", []string{"b", "a"}, nil)
		if err != nil || reject != "" {
			t.Fatalf("stage: err=%v reject=%q", err, reject)
		}
		if staged == nil || staged.PayloadHash == "" {
			t.Fatalf("no staged payload returned")
		}
		stagedHash = staged.PayloadHash
		if got := blockCount("w6b-block"); got != 0 {
			t.Fatalf("chat write must NOT execute directly (blocks=%d) — default-confirm broken", got)
		}
		var origin string
		if err := pool.QueryRow(ctx, `SELECT origin FROM context_pending_writes WHERE api_key_id = $1 AND payload_hash = $2`, keyRow.ID, stagedHash).Scan(&origin); err != nil {
			t.Fatalf("read origin: %v", err)
		}
		if origin != "chat" {
			t.Fatalf("origin = %q, want chat", origin)
		}
	})

	t.Run("confirm via Bearer header executes and consumes", func(t *testing.T) {
		rec := postConfirm(stagedHash, true, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("confirm = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if got := blockCount("w6b-block"); got != 1 {
			t.Fatalf("confirmed block count = %d, want 1", got)
		}
		if got := openCount(); got != 0 {
			t.Fatalf("stage not consumed (open=%d)", got)
		}
	})

	t.Run("cookie-only confirm is 401 (D1-m4 header-auth only)", func(t *testing.T) {
		staged, reject, err := runner.StageWrite(authedCtx(), "test", "w6b-cookie-block", "cookie probe", nil, nil)
		if err != nil || reject != "" {
			t.Fatalf("stage: err=%v reject=%q", err, reject)
		}
		rec := postConfirm(staged.PayloadHash, false, true)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("cookie-only confirm = %d, want 401: %s", rec.Code, rec.Body.String())
		}
		if got := blockCount("w6b-cookie-block"); got != 0 {
			t.Fatalf("cookie-only confirm must not execute (blocks=%d)", got)
		}
		if got := openCount(); got != 1 {
			t.Fatalf("cookie-only confirm must leave the stage open (open=%d)", got)
		}
	})

	t.Run("replayed confirm gets the generic 404 miss", func(t *testing.T) {
		rec := postConfirm(stagedHash, true, false)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no confirmable staged write") {
			t.Fatalf("replay = %d %s, want the generic 404 miss", rec.Code, rec.Body.String())
		}
		if got := blockCount("w6b-block"); got != 1 {
			t.Fatalf("replay double-executed (blocks=%d)", got)
		}
	})

	t.Run("gate violation rejects before anything is staged", func(t *testing.T) {
		before := openCount()
		staged, reject, err := runner.StageWrite(authedCtx(), "test", "w6b-oversize", strings.Repeat("x", 50*1024+1), nil, nil)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if staged != nil || !strings.Contains(reject, "50KB") {
			t.Fatalf("oversize must gate-reject pre-stage: staged=%v reject=%q", staged, reject)
		}
		if got := openCount(); got != before {
			t.Fatalf("gate violation staged a row anyway (%d -> %d)", before, got)
		}
	})
}
