//go:build integration

// L7 fail-closed (T07, design/01 §5.1/§5.4): the four MCP tool handlers must NOT
// fall back to the default-tenant scope `private` when no AuthResult is resolved.
// They must fail closed (errResult), never read or write the default tenant.
//
// In the live mount /mcp sits behind Auth(pool) (cmd/ctxd/server.go:91-93), which
// 401s an absent/invalid key BEFORE the handler runs — so ar==nil is UNREACHABLE
// in production today. This is therefore DEFENSE-IN-DEPTH, not a patch of an
// active exploit: a middleware regression, a new mount that forgets Auth, a test
// harness, or a future internal caller could reach a handler without auth, and
// the old `scope := "private"` default would then silently touch the default
// tenant. The handler must be safe regardless of the mount.
//
// RED (pre-T07): with no AuthResult in context the read handlers return the
// seeded default-tenant block (a leak) and the store handler WRITES to private —
// IsError is false. GREEN: each handler returns errResult (IsError true) and the
// store handler persists nothing.
//
//	go test -tags=integration ./internal/handler/ -run TestMCPHandlersFailClosed -count=1 -v
package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedMCPBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'secret content', $2) RETURNING id::text`,
		title, scope).Scan(&id); err != nil {
		t.Fatalf("seed mcp block: %v", err)
	}
	return id
}

func mcpResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestMCPHandlersFailClosedWithoutAuth_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background() // NO AuthResult → AuthResultFromContext(ctx) == nil

	const secretTitle = "l7-default-tenant-secret"
	blockID := seedMCPBlock(t, pool, "private", secretTitle)
	cfg := MCPConfig{Pool: pool}

	t.Run("search", func(t *testing.T) {
		res, _, err := mcpSearchHandler(cfg)(ctx, nil, searchInput{Query: ""})
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("search without auth: IsError=false, want fail-closed errResult (leaked: %s)", mcpResultText(res))
		}
	})

	t.Run("get", func(t *testing.T) {
		res, _, err := mcpGetHandler(cfg)(ctx, nil, getInput{ID: blockID})
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("get without auth: IsError=false, want fail-closed (leaked block %s)", blockID)
		}
		if strings.Contains(mcpResultText(res), secretTitle) {
			t.Fatalf("get without auth leaked the default-tenant block title")
		}
	})

	t.Run("recent", func(t *testing.T) {
		res, _, err := mcpRecentHandler(cfg)(ctx, nil, recentInput{})
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("recent without auth: IsError=false, want fail-closed (leaked: %s)", mcpResultText(res))
		}
	})

	t.Run("store", func(t *testing.T) {
		const wTitle = "l7-write-without-auth"
		res, _, err := mcpStoreHandler(cfg)(ctx, nil, storeInput{
			Category: "learnings", Title: wTitle, Content: "should never be written", Sensitivity: "internal",
		})
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("store without auth: IsError=false, want fail-closed (wrote to default tenant)")
		}
		var n int
		if e := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title=$1 AND scope='private'`, wTitle).Scan(&n); e != nil {
			t.Fatalf("verify write: %v", e)
		}
		if n != 0 {
			t.Fatalf("store without auth WROTE %d block(s) to the default-tenant scope", n)
		}
	})
}

// A VALID AuthResult carrying an EMPTY ReadScopes set must also fail closed in
// the MCP recent handler. The auth layer never produces this today (ctx_auth
// seeds read_scopes with ARRAY[home_scope], so a valid key always has ≥1 scope),
// but recent runs its OWN inline query rather than store.RecentBlocks — so it
// bypasses the store-layer guards and needs its own RequireScopes check. Without
// it a regression in the auth layer would turn this path into a
// `scope = ANY('{}')` fail-open while every other read path is closed.
func TestMCPRecentFailClosedOnEmptyScopes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	seedMCPBlock(t, pool, "private", "recent-empty-scope-probe")
	ctx := context.WithValue(context.Background(), authResultKey,
		&auth.AuthResult{IsValid: true, HomeScope: "private", ReadScopes: []string{}})
	cfg := MCPConfig{Pool: pool}

	res, _, err := mcpRecentHandler(cfg)(ctx, nil, recentInput{})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("recent with valid auth but EMPTY scopes: IsError=false, want fail-closed (leaked: %s)", mcpResultText(res))
	}
}
