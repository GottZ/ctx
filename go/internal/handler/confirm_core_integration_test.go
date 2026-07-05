//go:build integration

// F6-C6 D-W6c integration probes against a real PG18 testcontainer — the
// consolidated confirm core (confirm_core.go) exercised CROSS-SURFACE:
//
//  1. A chat ctx_update is ALWAYS staged (op 'update', origin 'chat', TOCTOU
//     pin captured) — the target block stays untouched until the confirm.
//  2. POST /api/confirm executes the chat-staged update (the ConfirmCard
//     path): field-list semantics apply, the row is consumed.
//  3. An MCP-staged update (origin 'mcp') confirms over HTTP — one core, two
//     stage surfaces, one confirm sequence.
//  4. TOCTOU drift via HTTP is a 409 WITHOUT consuming the token (D1-M3 on
//     the shared core); the direct edit wins, no lost update executes.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestUpdateConfirmCrossSurface -count=1 -v
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

func TestUpdateConfirmCrossSurface(t *testing.T) {
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

	keyRow, keyPlain, err := store.CreateApiKey(ctx, pool, "w6c-key", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	runner := &chatStageRunner{pool: pool, cfg: cfgStore}
	confirmH := NewConfirmHandler(pool, nil)
	router := chi.NewRouter()
	router.Group(func(r chi.Router) {
		r.Use(Auth(pool))
		r.Post("/api/confirm", confirmH.HandleConfirm)
	})

	authedCtx := func() context.Context {
		req := httptest.NewRequest(http.MethodPost, "/probe", nil)
		req.Header.Set("Authorization", "Bearer "+keyPlain)
		var got context.Context
		Auth(pool)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = r.Context()
		})).ServeHTTP(httptest.NewRecorder(), req)
		if got == nil || AuthResultFromContext(got) == nil {
			t.Fatalf("auth probe failed to resolve an AuthResult")
		}
		return got
	}
	postConfirm := func(hash string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"payload_hash": hash})
		req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+keyPlain)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	contentOf := func(id string) string {
		var c string
		if err := pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatalf("read content: %v", err)
		}
		return c
	}
	openCount := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL`, keyRow.ID).Scan(&n); err != nil {
			t.Fatalf("open count: %v", err)
		}
		return n
	}

	seed, err := store.UpsertBlock(ctx, pool, "test", "w6c-target", "original content", []string{"keep"}, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed block: %v", err)
	}

	var chatHash string
	t.Run("chat ctx_update is ALWAYS staged and touches nothing", func(t *testing.T) {
		newContent := "chat updated content"
		staged, reject, err := runner.StageUpdate(authedCtx(), seed.ID, nil, nil, &newContent, nil, nil)
		if err != nil || reject != "" {
			t.Fatalf("stage update: err=%v reject=%q", err, reject)
		}
		if staged == nil || staged.Op != "update" || staged.TargetID != seed.ID {
			t.Fatalf("staged card payload wrong: %+v", staged)
		}
		if len(staged.UpdateFields) != 1 || staged.UpdateFields[0] != "content" {
			t.Fatalf("update_fields = %v, want [content]", staged.UpdateFields)
		}
		chatHash = staged.PayloadHash
		if got := contentOf(seed.ID); got != "original content" {
			t.Fatalf("chat update must NOT execute directly (content=%q) — default-confirm broken", got)
		}
		var origin string
		if err := pool.QueryRow(ctx, `SELECT origin FROM context_pending_writes WHERE api_key_id = $1 AND payload_hash = $2`, keyRow.ID, chatHash).Scan(&origin); err != nil {
			t.Fatalf("read origin: %v", err)
		}
		if origin != "chat" {
			t.Fatalf("origin = %q, want chat", origin)
		}
	})

	t.Run("HTTP confirm executes the chat-staged update", func(t *testing.T) {
		rec := postConfirm(chatHash)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"op":"update"`) {
			t.Fatalf("confirm = %d %s, want 200 with op=update", rec.Code, rec.Body.String())
		}
		if got := contentOf(seed.ID); got != "chat updated content" {
			t.Fatalf("confirmed update did not land (content=%q)", got)
		}
		if got := openCount(); got != 0 {
			t.Fatalf("stage not consumed (open=%d)", got)
		}
	})

	t.Run("MCP-staged update confirms over HTTP (cross-surface)", func(t *testing.T) {
		title := "w6c via mcp"
		res, _, err := mcpStageUpdate(authedCtx(), MCPConfig{Pool: pool, Cfg: cfgStore},
			AuthResultFromContext(authedCtx()), updateInput{ID: seed.ID, Title: &title}, []string{"title"})
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("mcp stage update: err=%v res=%+v (want IsError=true staged response)", err, res)
		}
		var hash string
		if err := pool.QueryRow(ctx, `SELECT payload_hash FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL ORDER BY created_at DESC LIMIT 1`, keyRow.ID).Scan(&hash); err != nil {
			t.Fatalf("read staged hash: %v", err)
		}
		var origin string
		if err := pool.QueryRow(ctx, `SELECT origin FROM context_pending_writes WHERE api_key_id = $1 AND payload_hash = $2`, keyRow.ID, hash).Scan(&origin); err != nil {
			t.Fatalf("read origin: %v", err)
		}
		if origin != "mcp" {
			t.Fatalf("origin = %q, want mcp", origin)
		}
		rec := postConfirm(hash)
		if rec.Code != http.StatusOK {
			t.Fatalf("cross-surface confirm = %d: %s", rec.Code, rec.Body.String())
		}
		var gotTitle string
		if err := pool.QueryRow(ctx, `SELECT title FROM context_blocks WHERE id = $1`, seed.ID).Scan(&gotTitle); err != nil {
			t.Fatalf("read title: %v", err)
		}
		if gotTitle != title {
			t.Fatalf("title = %q, want %q", gotTitle, title)
		}
	})

	t.Run("TOCTOU drift via HTTP is 409 without burning the token", func(t *testing.T) {
		newContent := "will be stale"
		staged, reject, err := runner.StageUpdate(authedCtx(), seed.ID, nil, nil, &newContent, nil, nil)
		if err != nil || reject != "" {
			t.Fatalf("stage update: err=%v reject=%q", err, reject)
		}
		// Direct edit AFTER staging — the staged card is now rendered against
		// a stale base (the D1-M3 lost-update scenario).
		direct := "directly edited content"
		if _, _, err := store.UpdateBlock(ctx, pool, seed.ID, store.UpdateBlockData{Content: &direct}, []string{"private"}); err != nil {
			t.Fatalf("direct edit: %v", err)
		}
		before := openCount()
		rec := postConfirm(staged.PayloadHash)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed since this update was staged") {
			t.Fatalf("drift confirm = %d %s, want 409 lost-update reject", rec.Code, rec.Body.String())
		}
		if got := openCount(); got != before {
			t.Fatalf("drift reject consumed the token (%d -> %d)", before, got)
		}
		if got := contentOf(seed.ID); got != direct {
			t.Fatalf("lost update executed anyway (content=%q)", got)
		}
	})
}
