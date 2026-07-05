//go:build integration

// F6-C6 D-W5 integration probes against a real PG18 testcontainer (migrations
// 089+090 applied by the runner):
//
//  1. A key WITHOUT confirm_writes keeps the direct MCP store path — block
//     written immediately, nothing staged (fail-open, D-E2).
//  2. A flagged key gets STAGED: IsError=true (D3-C3 — a legacy client must
//     never read "staged" as success), a pending row exists, NO block exists.
//  3. confirm with the returned hash executes the exact write and consumes
//     the row (exactly once).
//  4. A replayed confirm gets the generic miss (no oracle).
//  5. A foreign key's confirm gets the generic miss and leaves the stage open.
//  6. A stage whose scope the key can no longer write is rejected WITHOUT
//     consuming the token (D1-M1 on lookup, not consume).
//  7. A gate violation (oversize) rejects BEFORE anything is staged (D1-M2).
//  8. The hash binds the post-detector sensitivity: a staged credentials hit
//     executes as credentials (D-W2 binding, end to end).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPStageConfirm -count=1 -v
package handler

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var hashRe = regexp.MustCompile(`payload_hash: ([0-9a-f]{64})`)

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil || len(r.Content) == 0 {
		t.Fatalf("empty tool result")
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type %T", r.Content[0])
	}
	return tc.Text
}

func TestMCPStageConfirm(t *testing.T) {
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
	confirmTool := mcpConfirmHandler(mcpCfg)

	// Two principals, authenticated over the REAL ctx_auth path (090).
	_, directPlain, err := store.CreateApiKey(ctx, pool, "w5-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "w5-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create flagged key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, flaggedRow.ID); err != nil {
		t.Fatalf("opt in flagged key: %v", err)
	}
	authCtx := func(plain string) context.Context {
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate: %v (valid=%v)", err, ar != nil && ar.IsValid)
		}
		return context.WithValue(ctx, authResultKey, ar)
	}
	directCtx := authCtx(directPlain)
	flaggedCtx := authCtx(flaggedPlain)

	pendingCount := func(keyID string) int {
		var n int
		query := `SELECT count(*) FROM context_pending_writes`
		args := []any{}
		if keyID != "" {
			query += ` WHERE api_key_id = $1`
			args = append(args, keyID)
		}
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("pending count: %v", err)
		}
		return n
	}
	blockCount := func(title string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("block count: %v", err)
		}
		return n
	}

	t.Run("key without flag keeps the direct path", func(t *testing.T) {
		r, _, err := storeTool(directCtx, nil, storeInput{Category: "test", Title: "w5-direct-block", Content: "direct content"})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if r.IsError {
			t.Fatalf("direct path errored: %s", resultText(t, r))
		}
		if got := blockCount("w5-direct-block"); got != 1 {
			t.Fatalf("direct block count = %d, want 1", got)
		}
		if got := pendingCount(""); got != 0 {
			t.Fatalf("stray pending rows: %d", got)
		}
	})

	var stagedHash string
	t.Run("flagged key stages with IsError=true and no block", func(t *testing.T) {
		r, _, err := storeTool(flaggedCtx, nil, storeInput{Category: "test", Title: "w5-staged-block", Content: "staged content"})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if !r.IsError {
			t.Fatalf("staged response must be IsError=true (D3-C3), got IsError=false: %s", resultText(t, r))
		}
		text := resultText(t, r)
		m := hashRe.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("staged response carries no payload_hash: %s", text)
		}
		stagedHash = m[1]
		if got := blockCount("w5-staged-block"); got != 0 {
			t.Fatalf("staged write must not create a block yet (count=%d)", got)
		}
		if got := pendingCount(flaggedRow.ID); got != 1 {
			t.Fatalf("pending count = %d, want 1", got)
		}
	})

	t.Run("confirm executes exactly the staged write and consumes the row", func(t *testing.T) {
		r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: stagedHash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if r.IsError {
			t.Fatalf("confirm errored: %s", resultText(t, r))
		}
		if got := blockCount("w5-staged-block"); got != 1 {
			t.Fatalf("confirmed block count = %d, want 1", got)
		}
		var open int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL`, flaggedRow.ID).Scan(&open); err != nil {
			t.Fatalf("open count: %v", err)
		}
		if open != 0 {
			t.Fatalf("stage not consumed (open=%d)", open)
		}
	})

	t.Run("replayed confirm gets the generic miss", func(t *testing.T) {
		r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: stagedHash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !r.IsError || !strings.Contains(resultText(t, r), "no confirmable staged write") {
			t.Fatalf("replay must yield the generic miss, got: %s", resultText(t, r))
		}
		if got := blockCount("w5-staged-block"); got != 1 {
			t.Fatalf("replay must not double-execute (count=%d)", got)
		}
	})

	t.Run("foreign key confirm misses and leaves the stage open", func(t *testing.T) {
		r, _, err := storeTool(flaggedCtx, nil, storeInput{Category: "test", Title: "w5-foreign-probe", Content: "foreign probe"})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		hash := hashRe.FindStringSubmatch(resultText(t, r))[1]

		fr, _, err := confirmTool(directCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !fr.IsError || !strings.Contains(resultText(t, fr), "no confirmable staged write") {
			t.Fatalf("foreign confirm must yield the generic miss, got: %s", resultText(t, fr))
		}
		var open int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL`, flaggedRow.ID).Scan(&open); err != nil {
			t.Fatalf("open count: %v", err)
		}
		if open != 1 {
			t.Fatalf("foreign confirm must not consume the stage (open=%d)", open)
		}
		if got := blockCount("w5-foreign-probe"); got != 0 {
			t.Fatalf("foreign confirm must not execute (count=%d)", got)
		}
	})

	t.Run("shrunk scope rejects without burning the token (D1-M1)", func(t *testing.T) {
		// Simulate rights that shrank between stage and confirm: a stage row
		// whose scope the key cannot write (home=private, no shared/work
		// rights). Seeded through the store layer — over MCP this state
		// arises when key rights are mutated after staging.
		cw := store.CanonicalWrite{Op: "store", Scope: "work", Category: "test", Title: "w5-shrunk", Content: "shrunk"}
		hash, canonical, err := cw.PayloadHash()
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if _, err := store.StagePendingWrite(ctx, pool, flaggedRow.ID, "work", "store", "mcp", canonical, hash, 0); err != nil {
			t.Fatalf("stage: %v", err)
		}

		r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if !r.IsError || !strings.Contains(resultText(t, r), "no longer writable") {
			t.Fatalf("shrunk-scope confirm must reject with the scope message, got: %s", resultText(t, r))
		}
		if got := blockCount("w5-shrunk"); got != 0 {
			t.Fatalf("shrunk-scope confirm must not execute (count=%d)", got)
		}
		var open int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1 AND payload_hash = $2 AND consumed_at IS NULL`, flaggedRow.ID, hash).Scan(&open); err != nil {
			t.Fatalf("open count: %v", err)
		}
		if open != 1 {
			t.Fatalf("scope rejection must not consume the token (open=%d)", open)
		}
	})

	t.Run("gate violation rejects before anything is staged (D1-M2)", func(t *testing.T) {
		before := pendingCount(flaggedRow.ID)
		r, _, err := storeTool(flaggedCtx, nil, storeInput{Category: "test", Title: "w5-oversize", Content: strings.Repeat("x", 50*1024+1)})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if !r.IsError || !strings.Contains(resultText(t, r), "50KB") {
			t.Fatalf("oversize stage must reject with the size gate, got: %s", resultText(t, r))
		}
		if got := pendingCount(flaggedRow.ID); got != before {
			t.Fatalf("gate violation staged a row anyway (%d -> %d)", before, got)
		}
	})

	t.Run("hash binds the post-detector sensitivity end to end", func(t *testing.T) {
		content := "leaked key AKIA1234567890ABCDEF do not store"
		r, _, err := storeTool(flaggedCtx, nil, storeInput{Category: "test", Title: "w5-detector", Content: content})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		hash := hashRe.FindStringSubmatch(resultText(t, r))[1]
		cr, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if cr.IsError {
			t.Fatalf("confirm errored: %s", resultText(t, cr))
		}
		var sens string
		if err := pool.QueryRow(ctx, `SELECT sensitivity FROM context_blocks WHERE title = 'w5-detector'`).Scan(&sens); err != nil {
			t.Fatalf("read sensitivity: %v", err)
		}
		if sens != "credentials" {
			t.Fatalf("detector sensitivity not bound through the stage (got %q, want credentials)", sens)
		}
	})
}
