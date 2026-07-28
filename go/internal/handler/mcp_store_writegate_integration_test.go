//go:build integration

// H-W8 integration probes: the direct MCP store arm must feed AND obey the
// write rate limit, exactly like the REST /api/store arm.
//
// Ist-Stand before H-W8: store.LogAccess(…, "write") existed in
// internal/handler/{ingest,context_store,project_issues_write}.go only — NEVER
// in mcp*.go. store.CheckRateLimit counts context_access_log rows with
// action='write', so a purely MCP-writing client had writeCount==0 forever and
// query.rate_limit_write could never bite (stage_gates.go:45-48 names it).
//
// Subtests (brief names in parentheses):
//
//	WritesAccessLog           (TestMCPStore_WritesAccessLog)   — RED pre-H-W8: 0 rows.
//	RateLimited               (TestMCPStore_RateLimited)       — RED pre-H-W8: 3rd write succeeds.
//	LimitZeroDisabled         (TestMCPStore_LimitZeroDisabled) — guard rail: RED against a
//	                          missing `limit > 0` guard ("0 means no writes"), green either way today.
//	RESTWriteCounterUnchanged (Gate g)                          — no-regression: the REST arm still
//	                          books EXACTLY one write row, no double accounting.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPStoreWriteAccounting -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestMCPStoreWriteAccounting(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	// writeRows counts the write-action log rows of one key — the exact
	// population store.CheckRateLimit aggregates over.
	writeRows := func(keyID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = 'write'`, keyID).Scan(&n); err != nil {
			t.Fatalf("count write rows: %v", err)
		}
		return n
	}
	// waitWriteRows polls until the key has at least want rows (the REST arm
	// books its write log in a background goroutine) and returns the count.
	waitWriteRows := func(keyID string, want int) int {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			n := writeRows(keyID)
			if n >= want || time.Now().After(deadline) {
				return n
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	mkKey := func(name string) (string, context.Context) {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, name, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return row.ID, mcpAuthCtx(t, ctx, pool, plain)
	}
	// mcpCfgWithLimit wires the MCP tools with a fixed query.rate_limit_write.
	mcpCfgWithLimit := func(limit int) MCPConfig {
		return MCPConfig{
			Pool:       pool,
			Cfg:        staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitWrite: limit}}},
			Blocktypes: reg,
		}
	}

	t.Run("WritesAccessLog", func(t *testing.T) {
		keyID, keyCtx := mkKey("hw8-log")
		storeTool := mcpStoreHandler(mcpCfgWithLimit(0))

		r, _, err := storeTool(keyCtx, nil, storeInput{
			Category: "test", Title: "hw8-log-block", Content: "mcp write must be booked",
		})
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if r.IsError {
			t.Fatalf("store errored: %s", resultText(t, r))
		}

		var blockID string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM context_blocks WHERE title = 'hw8-log-block'`).Scan(&blockID); err != nil {
			t.Fatalf("read block id: %v", err)
		}

		// RED pre-H-W8: no MCP path ever called LogAccess ⇒ 0.
		if got := writeRows(keyID); got != 1 {
			t.Fatalf("MCP store booked %d write rows for the acting key, want exactly 1", got)
		}
		var loggedBlock, loggedKey string
		if err := pool.QueryRow(ctx,
			`SELECT block_id::text, api_key_id::text FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = 'write'`, keyID).Scan(&loggedBlock, &loggedKey); err != nil {
			t.Fatalf("read write row: %v", err)
		}
		if loggedBlock != blockID {
			t.Errorf("write row block_id = %s, want the stored block %s", loggedBlock, blockID)
		}
		if loggedKey != keyID {
			t.Errorf("write row api_key_id = %s, want the acting key %s", loggedKey, keyID)
		}
	})

	t.Run("RateLimited", func(t *testing.T) {
		keyID, keyCtx := mkKey("hw8-limited")
		storeTool := mcpStoreHandler(mcpCfgWithLimit(2))

		for i := 1; i <= 2; i++ {
			r, _, err := storeTool(keyCtx, nil, storeInput{
				Category: "test",
				Title:    fmt.Sprintf("hw8-limited-%d", i),
				Content:  fmt.Sprintf("budgeted write %d", i),
			})
			if err != nil {
				t.Fatalf("store %d: %v", i, err)
			}
			if r.IsError {
				t.Fatalf("store %d must fit the budget, got: %s", i, resultText(t, r))
			}
		}
		// Errorf, not Fatalf: the third-write assertion below is the actual
		// H-W8 claim and must still run (and stay RED) pre-fix.
		if got := writeRows(keyID); got != 2 {
			t.Errorf("two MCP writes booked %d rows, want 2 (the limiter reads exactly these)", got)
		}

		// RED pre-H-W8: the third write goes through — writeCount stayed 0.
		r, _, err := storeTool(keyCtx, nil, storeInput{
			Category: "test", Title: "hw8-limited-3", Content: "over budget",
		})
		if err != nil {
			t.Fatalf("store 3 returned a protocol error, want an IsError result: %v", err)
		}
		if !r.IsError {
			t.Fatalf("third MCP write must be rate-limited, got success: %s", resultText(t, r))
		}
		if txt := resultText(t, r); !strings.Contains(txt, "Rate limit exceeded") {
			t.Errorf("rejection text = %q, want it to name the rate limit", txt)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE title = 'hw8-limited-3'`).Scan(&n); err != nil {
			t.Fatalf("count rejected block: %v", err)
		}
		if n != 0 {
			t.Errorf("rate-limited call still wrote %d block(s), want 0", n)
		}
		if got := writeRows(keyID); got != 2 {
			t.Errorf("rate-limited call booked a write row (%d rows), want it to stay at 2", got)
		}
	})

	t.Run("LimitZeroDisabled", func(t *testing.T) {
		// Guard rail (not RED against today's Ist-Stand): rate_limit_write = 0
		// means DISABLED, never "zero writes allowed". RED against an
		// implementation that drops the `limit > 0` guard.
		keyID, keyCtx := mkKey("hw8-zero")
		storeTool := mcpStoreHandler(mcpCfgWithLimit(0))

		for i := 1; i <= 3; i++ {
			r, _, err := storeTool(keyCtx, nil, storeInput{
				Category: "test",
				Title:    fmt.Sprintf("hw8-zero-%d", i),
				Content:  fmt.Sprintf("unlimited write %d", i),
			})
			if err != nil {
				t.Fatalf("store %d: %v", i, err)
			}
			if r.IsError {
				t.Fatalf("limit 0 must disable the throttle, store %d rejected: %s", i, resultText(t, r))
			}
		}
		if got := writeRows(keyID); got != 3 {
			t.Errorf("three unthrottled MCP writes booked %d rows, want 3", got)
		}
	})

	t.Run("RESTWriteCounterUnchanged", func(t *testing.T) {
		// Gate g: the REST arm keeps booking EXACTLY one write row per store —
		// the new MCP booking must not leak into the shared write path.
		row, _, err := store.CreateApiKey(ctx, pool, "hw8-rest", "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create rest key: %v", err)
		}
		ar := &auth.AuthResult{
			IsValid: true, ApiKeyID: row.ID,
			HomeScope: "private", ReadScopes: []string{"private"},
		}
		sh := NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{}}, reg)

		body, _ := json.Marshal(map[string]any{
			"category": "test", "title": "hw8-rest-block", "content": "rest write stays single-booked",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body)))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		sh.HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST store: status %d (body %s)", rec.Code, rec.Body.String())
		}

		if got := waitWriteRows(row.ID, 1); got != 1 {
			t.Fatalf("REST store booked %d write rows, want exactly 1 (no double accounting)", got)
		}
		// Settle window: a second booking would surface within it.
		time.Sleep(500 * time.Millisecond)
		if got := writeRows(row.ID); got != 1 {
			t.Fatalf("REST store settled at %d write rows, want exactly 1", got)
		}
	})
}
