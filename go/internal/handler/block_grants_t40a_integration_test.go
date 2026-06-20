//go:build integration

// Integration test for Multi-Tenant wave T40a (Achse 07, design/07 §4/§5.5):
// the REQUEST-PATH side of the block-level "Abruf-only-OR". T40a live-wires the
// grant resolution into exactly four request paths — the three MCP handlers
// (search/get/recent) and context_manage.go handleGet — each resolving the
// caller-tenant grant set via store.GrantedBlockIDs and threading it into the
// scope/grant visibility OR.
//
// G1 (Abruf, request path): a grantee whose readScopes do NOT include the owner
// scope still reads a granted block through MCP get/search/recent + manage-get;
// without a grant it gets the indistinguishable 404 / empty.
//
// G9 (oracle, design/07 §5.5): manage-get with the full UUID of a FOREIGN,
// UNGRANTED block answers "Block not found" AND writes 0 context_access_log rows
// for that block_id (the LogAccess goroutine moved AFTER the block!=nil check —
// a 404 leaves no log trace). WITH a grant the block is visible AND exactly one
// access_log row appears.
//
//	go test -tags=integration ./internal/handler/ -run TestBlockGrantsT40a -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

// bgTenant registers one tenant and returns its UUID.
func bgTenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	return id
}

func bgMapScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("map scope %s: %v", scope, err)
	}
}

func bgBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'secret content of ' || $1, $2) RETURNING id::text`,
		title, scope).Scan(&id); err != nil {
		t.Fatalf("seed block %s (scope %s): %v", title, scope, err)
	}
	return id
}

func bgGrant(t *testing.T, pool *pgxpool.Pool, blockID, granteeTenant string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_block_grants (block_id, grantee_tenant) VALUES ($1::uuid, $2::uuid)`,
		blockID, granteeTenant); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
}

// bgKey mints a real api_keys row (so LogAccess's uuid cast on api_key_id holds)
// and returns its UUID.
func bgKey(t *testing.T, pool *pgxpool.Pool, keyHash, homeScope string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_api_keys (key_hash, label, home_scope) VALUES ($1,$2,$3) RETURNING id::text`,
		keyHash, keyHash, homeScope).Scan(&id); err != nil {
		t.Fatalf("seed api_key %s: %v", keyHash, err)
	}
	return id
}

func bgAccessLogCount(t *testing.T, pool *pgxpool.Pool, blockID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_access_log WHERE block_id = $1::uuid`, blockID).Scan(&n); err != nil {
		t.Fatalf("count access_log for %s: %v", blockID, err)
	}
	return n
}

func TestBlockGrantsT40a_RequestPaths_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	const scopeC = "t40h-c" // owner scope
	const scopeA = "t40h-a" // grantee home scope
	owner := bgTenant(t, pool, "t40h-owner")
	grantee := bgTenant(t, pool, "t40h-grantee")
	bgMapScope(t, pool, scopeC, owner)
	bgMapScope(t, pool, scopeA, grantee)

	blockB := bgBlock(t, pool, scopeC, "t40h-shared-B")

	// Grantee AuthResult: home scope A, read scope A, tenant = grantee. It does
	// NOT carry scope C — only the grant makes B visible.
	granteeAR := func() *auth.AuthResult {
		return &auth.AuthResult{IsValid: true, HomeScope: scopeA, ReadScopes: []string{scopeA}, TenantID: grantee}
	}
	granteeCtx := func() context.Context {
		return context.WithValue(context.Background(), authResultKey, granteeAR())
	}

	cfg := MCPConfig{Pool: pool}

	// Pre-grant: MCP get/search must NOT surface B.
	t.Run("G1_MCP_no_grant_invisible", func(t *testing.T) {
		res, _, err := mcpGetHandler(cfg)(granteeCtx(), nil, getInput{ID: blockB})
		if err != nil {
			t.Fatalf("mcp get transport: %v", err)
		}
		// A full UUID passes ResolveBlockID's bypass; GetBlock then re-gates it
		// with the scope/grant OR and returns nil → the handler marshals a JSON
		// `null` body (not an errResult). The leak check is what matters: neither
		// the block id nor its secret content/title may appear.
		body := mcpResultText(res)
		if strings.Contains(body, "t40h-shared-B") || strings.Contains(body, "secret content") {
			t.Fatalf("mcp get pre-grant leaked B (body=%s)", body)
		}
		sres, _, err := mcpSearchHandler(cfg)(granteeCtx(), nil, searchInput{Query: ""})
		if err != nil {
			t.Fatalf("mcp search transport: %v", err)
		}
		if strings.Contains(mcpResultText(sres), blockB) {
			t.Fatalf("mcp search pre-grant leaked B id into scope A")
		}
	})

	// Grant B → grantee.
	bgGrant(t, pool, blockB, grantee)

	t.Run("G1_MCP_get_visible_with_grant", func(t *testing.T) {
		res, _, err := mcpGetHandler(cfg)(granteeCtx(), nil, getInput{ID: blockB})
		if err != nil {
			t.Fatalf("mcp get transport: %v", err)
		}
		if res.IsError || !strings.Contains(mcpResultText(res), blockB) {
			t.Fatalf("mcp get post-grant did NOT return B (IsError=%t, body=%s)", res.IsError, mcpResultText(res))
		}
	})

	t.Run("G1_MCP_search_visible_with_grant", func(t *testing.T) {
		res, _, err := mcpSearchHandler(cfg)(granteeCtx(), nil, searchInput{Query: ""})
		if err != nil {
			t.Fatalf("mcp search transport: %v", err)
		}
		if !strings.Contains(mcpResultText(res), blockB) {
			t.Fatalf("mcp search post-grant did NOT include B (body=%s)", mcpResultText(res))
		}
	})

	t.Run("G1_MCP_recent_visible_with_grant", func(t *testing.T) {
		res, _, err := mcpRecentHandler(cfg)(granteeCtx(), nil, recentInput{})
		if err != nil {
			t.Fatalf("mcp recent transport: %v", err)
		}
		if res.IsError || !strings.Contains(mcpResultText(res), blockB) {
			t.Fatalf("mcp recent post-grant did NOT include B (IsError=%t, body=%s)", res.IsError, mcpResultText(res))
		}
	})

	t.Run("G1_MCP_no_auth_still_fails_closed", func(t *testing.T) {
		// Even with B granted, an unauthenticated MCP call (ar==nil) fails closed.
		res, _, err := mcpGetHandler(cfg)(context.Background(), nil, getInput{ID: blockB})
		if err != nil {
			t.Fatalf("mcp get transport: %v", err)
		}
		if !res.IsError || strings.Contains(mcpResultText(res), "secret content") {
			t.Fatalf("mcp get without auth leaked granted block (IsError=%t)", res.IsError)
		}
	})
}

// TestBlockGrantsT40a_ManageGetOracle_Integration is the G9 oracle gate against
// context_manage.go handleGet: a 404 on a foreign/ungranted block writes 0
// access_log rows; a visible (granted) block writes exactly 1.
func TestBlockGrantsT40a_ManageGetOracle_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	const scopeC = "t40o-c"
	const scopeA = "t40o-a"
	owner := bgTenant(t, pool, "t40o-owner")
	grantee := bgTenant(t, pool, "t40o-grantee")
	bgMapScope(t, pool, scopeC, owner)
	bgMapScope(t, pool, scopeA, grantee)

	// Two foreign blocks in scope C: one stays ungranted (oracle probe), one is
	// granted (visible probe).
	ungranted := bgBlock(t, pool, scopeC, "t40o-foreign-ungranted")
	granted := bgBlock(t, pool, scopeC, "t40o-foreign-granted")
	bgGrant(t, pool, granted, grantee)

	// Grantee key (real api_keys row so LogAccess's api_key_id cast holds).
	keyID := bgKey(t, pool, "t40o-grantee-key", scopeA)

	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)
	callGet := func(blockID string) map[string]any {
		ar := &auth.AuthResult{
			IsValid: true, HomeScope: scopeA, ReadScopes: []string{scopeA},
			TenantID: grantee, ApiKeyID: keyID,
		}
		raw, _ := json.Marshal(map[string]any{"id": blockID})
		rec := httptest.NewRecorder()
		h.handleGet(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), ar, manageRequest{ID: blockID, Data: raw})
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode manage-get: %v (body %s)", err, rec.Body.String())
		}
		return resp
	}

	// settleLog waits for the async LogAccess goroutine to settle, then returns
	// the access_log count for the block. We poll briefly for the POSITIVE case;
	// for the NEGATIVE case (expect 0) the poll just gives the goroutine time to
	// have fired if it were going to.
	settleLog := func(blockID string, want int) int {
		var n int
		for i := 0; i < 40; i++ {
			n = bgAccessLogCount(t, pool, blockID)
			if n >= want {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		// One extra settle so a spurious write would have had time to land.
		time.Sleep(50 * time.Millisecond)
		return bgAccessLogCount(t, pool, blockID)
	}

	t.Run("G9_404_leaves_no_access_log", func(t *testing.T) {
		resp := callGet(ungranted)
		if ok, _ := resp["success"].(bool); ok {
			t.Fatalf("manage-get on foreign ungranted block succeeded, want 404 (resp %v)", resp)
		}
		if got := settleLog(ungranted, 1); got != 0 {
			t.Fatalf("manage-get 404 wrote %d access_log row(s) for the ungranted block, want 0 (G9 oracle)", got)
		}
	})

	t.Run("G9_visible_grant_logs_one", func(t *testing.T) {
		resp := callGet(granted)
		if ok, _ := resp["success"].(bool); !ok {
			t.Fatalf("manage-get on granted block did NOT succeed (resp %v)", resp)
		}
		if got := settleLog(granted, 1); got != 1 {
			t.Fatalf("manage-get on visible granted block wrote %d access_log row(s), want exactly 1", got)
		}
	})
}
