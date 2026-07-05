//go:build integration

// F6-C6 D-W6a integration probes (real PG18 testcontainer, migrations 089+090):
//
//  1. A key WITHOUT confirm_writes updates directly — block changed in place.
//  2. A flagged key gets the update STAGED: IsError=true, block unchanged.
//  3. confirm executes exactly the staged update and consumes the row.
//  4. TOCTOU (D1-M3): the block changes between stage and confirm — the
//     confirm rejects WITHOUT consuming the token (lost-update protection).
//  5. Replayed update-confirm gets the generic miss.
//  6. Field-list semantics: tags [] through the staged dance CLEARS the tags
//     (UpdateFields is the authority, not value presence).
//  7. A block outside the key's writable scopes is a miss for both paths.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPUpdate -count=1 -v
package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mcpAuthCtx authenticates a plain key over the REAL ctx_auth path (090) and
// returns a ctx carrying the AuthResult, like the MCP auth middleware does.
func mcpAuthCtx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, plain string) context.Context {
	t.Helper()
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v (valid=%v)", err, ar != nil && ar.IsValid)
	}
	return context.WithValue(ctx, authResultKey, ar)
}

func TestMCPUpdate(t *testing.T) {
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

	mcpCfg := MCPConfig{Pool: pool, Cfg: cfgStore}
	storeTool := mcpStoreHandler(mcpCfg)
	updateTool := mcpUpdateHandler(mcpCfg)
	confirmTool := mcpConfirmHandler(mcpCfg)

	_, directPlain, err := store.CreateApiKey(ctx, pool, "w6a-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "w6a-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create flagged key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, flaggedRow.ID); err != nil {
		t.Fatalf("opt in flagged key: %v", err)
	}
	directCtx := mcpAuthCtx(t, ctx, pool, directPlain)
	flaggedCtx := mcpAuthCtx(t, ctx, pool, flaggedPlain)

	// Target fixture: one block per probe, created over the direct path.
	mkBlock := func(title string) string {
		r, _, err := storeTool(directCtx, nil, storeInput{Category: "test", Title: title, Content: "base content", Tags: []string{"keep", "me"}})
		if err != nil || r.IsError {
			t.Fatalf("fixture store %s failed: %v %v", title, err, r)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("fixture id: %v", err)
		}
		return id
	}
	blockContent := func(id string) string {
		var c string
		if err := pool.QueryRow(ctx, `SELECT content FROM context_blocks WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatalf("read content: %v", err)
		}
		return c
	}
	openStages := func(hash string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND payload_hash = $2 AND consumed_at IS NULL`, flaggedRow.ID, hash).Scan(&n); err != nil {
			t.Fatalf("open stages: %v", err)
		}
		return n
	}
	strptr := func(s string) *string { return &s }

	t.Run("key without flag updates directly", func(t *testing.T) {
		id := mkBlock("w6a-direct-target")
		r, _, err := updateTool(directCtx, nil, updateInput{ID: id, Content: strptr("updated directly")})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if r.IsError {
			t.Fatalf("direct update errored: %s", resultText(t, r))
		}
		if got := blockContent(id); got != "updated directly" {
			t.Fatalf("content = %q, want %q", got, "updated directly")
		}
	})

	var stagedHash string
	var stagedID string
	t.Run("flagged key stages the update with IsError=true and no change", func(t *testing.T) {
		stagedID = mkBlock("w6a-staged-target")
		r, _, err := updateTool(flaggedCtx, nil, updateInput{ID: stagedID, Content: strptr("updated via confirm")})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if !r.IsError {
			t.Fatalf("staged update must be IsError=true (D3-C3), got IsError=false: %s", resultText(t, r))
		}
		m := hashRe.FindStringSubmatch(resultText(t, r))
		if m == nil {
			t.Fatalf("staged update carries no payload_hash: %s", resultText(t, r))
		}
		stagedHash = m[1]
		if got := blockContent(stagedID); got != "base content" {
			t.Fatalf("staged update must not touch the block (content=%q)", got)
		}
	})

	t.Run("confirm executes exactly the staged update and consumes the row", func(t *testing.T) {
		r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: stagedHash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if r.IsError {
			t.Fatalf("confirm errored: %s", resultText(t, r))
		}
		if got := blockContent(stagedID); got != "updated via confirm" {
			t.Fatalf("content = %q, want %q", got, "updated via confirm")
		}
		if got := openStages(stagedHash); got != 0 {
			t.Fatalf("stage not consumed (open=%d)", got)
		}
	})

	t.Run("replayed update-confirm gets the generic miss", func(t *testing.T) {
		r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: stagedHash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !r.IsError || !strings.Contains(resultText(t, r), "no confirmable staged write") {
			t.Fatalf("replay must yield the generic miss, got: %s", resultText(t, r))
		}
	})

	t.Run("TOCTOU drift between stage and confirm rejects without burning the token", func(t *testing.T) {
		id := mkBlock("w6a-toctou-target")
		r, _, err := updateTool(flaggedCtx, nil, updateInput{ID: id, Content: strptr("staged edit")})
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		hash := hashRe.FindStringSubmatch(resultText(t, r))[1]

		// The block changes AFTER staging (a concurrent direct writer).
		dr, _, err := updateTool(directCtx, nil, updateInput{ID: id, Content: strptr("concurrent edit")})
		if err != nil || dr.IsError {
			t.Fatalf("concurrent edit failed: %v %v", err, dr)
		}

		cr, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !cr.IsError || !strings.Contains(resultText(t, cr), "changed since this update was staged") {
			t.Fatalf("TOCTOU drift must reject with the lost-update message, got: %s", resultText(t, cr))
		}
		if got := blockContent(id); got != "concurrent edit" {
			t.Fatalf("drifted confirm must not execute (content=%q)", got)
		}
		if got := openStages(hash); got != 1 {
			t.Fatalf("TOCTOU rejection must not consume the token (open=%d)", got)
		}
	})

	t.Run("tags [] through the staged dance clears the tags (UpdateFields authority)", func(t *testing.T) {
		id := mkBlock("w6a-clear-tags")
		r, _, err := updateTool(flaggedCtx, nil, updateInput{ID: id, Tags: []string{}})
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if !r.IsError {
			t.Fatalf("expected staged response, got success: %s", resultText(t, r))
		}
		hash := hashRe.FindStringSubmatch(resultText(t, r))[1]
		cr, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if cr.IsError {
			t.Fatalf("confirm errored: %s", resultText(t, cr))
		}
		var tagCount int
		if err := pool.QueryRow(ctx, `SELECT coalesce(array_length(tags, 1), 0) FROM context_blocks WHERE id = $1`, id).Scan(&tagCount); err != nil {
			t.Fatalf("read tags: %v", err)
		}
		if tagCount != 0 {
			t.Fatalf("tags not cleared (len=%d) — UpdateFields list must carry the clear", tagCount)
		}
	})

	t.Run("block outside writable scopes is a miss for both paths", func(t *testing.T) {
		// A block in scope 'work' — neither key may write there.
		var foreignID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ('test', 'w6a-foreign-scope', 'x', 'work') RETURNING id`,
		).Scan(&foreignID); err != nil {
			t.Fatalf("seed foreign block: %v", err)
		}
		for name, c := range map[string]context.Context{"direct": directCtx, "flagged": flaggedCtx} {
			r, _, err := updateTool(c, nil, updateInput{ID: foreignID, Content: strptr("nope")})
			if err != nil {
				t.Fatalf("%s update: %v", name, err)
			}
			if !r.IsError || !strings.Contains(resultText(t, r), "not found") {
				t.Fatalf("%s update on foreign scope must miss, got: %s", name, resultText(t, r))
			}
		}
	})
}
