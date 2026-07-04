//go:build integration

// W12 MCP issue-tool gates (design/03 §7-W12). Drives the three tool handlers
// (issue_create/issue_comment/issue_state) directly with an injected AuthResult +
// a booted type registry, against a live PG18 testdb. Proven RED-first (each RED
// noted; the RED reproduction is a temporary edit of the handler, re-run, revert):
//
//   - FAIL-CLOSED: ar==nil ⇒ errResult and NOTHING is written (RED: drop the
//     ar==nil guard ⇒ create writes to HomeScope, IsError=false).
//   - TEMPLATE BLAST-RADIUS (§5.5): a template key (home=project, allowed=[],
//     write=[]) can create/comment/state ONLY in its own scope; commenting on a
//     shared-scope issue fails closed. A key WITH shared in allowed+write CAN
//     comment on the shared issue (the documented non-template behaviour). RED for
//     the closed case: pass a wider writable set ⇒ the shared comment succeeds.
//   - STATE-TRANSITION POLICY: an out-of-policy target ⇒ errResult (NOT a silent
//     success); a configured transition ⇒ success. RED: skip ValidateTransition ⇒
//     the invalid target persists with IsError=false.
//
// Run: go test -tags=integration ./internal/handler/ -run TestMCPIssue -count=1 -v
package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

// w12Ctx injects ar into a fresh context (the auth middleware's job in production).
func w12Ctx(ar *auth.AuthResult) context.Context {
	if ar == nil {
		return context.Background()
	}
	return context.WithValue(context.Background(), authResultKey, ar)
}

// w12Cfg wires an MCP config with the pool + a booted registry (issue policy).
func w12Cfg(t *testing.T, pool *pgxpool.Pool) MCPConfig {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return MCPConfig{Pool: pool, Blocktypes: reg}
}

func w12Writer(scope string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, HomeScope: scope, ReadScopes: []string{scope}}
}

func TestMCPIssueToolsFailClosedWithoutAuth_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfg := w12Cfg(t, pool)
	ctx := w12Ctx(nil) // NO AuthResult

	// Seed one issue in the default tenant scope so comment/state have a target
	// they would touch IF the guard were absent.
	_, scope := w6SeedProject(t, pool, "w12fc")
	iss := w6SeedIssue(t, pool, scope, "backlog", "fc target")

	t.Run("issue_create", func(t *testing.T) {
		res, _, err := mcpIssueCreateHandler(cfg)(ctx, nil, issueCreateInput{Title: "should not persist"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("create without auth: IsError=false (leaked: %s)", mcpText(res))
		}
		var n int
		if e := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_blocks WHERE title LIKE '%should not persist%'`).Scan(&n); e != nil {
			t.Fatalf("verify: %v", e)
		}
		if n != 0 {
			t.Fatalf("create without auth WROTE %d block(s)", n)
		}
	})
	t.Run("issue_comment", func(t *testing.T) {
		res, _, err := mcpIssueCommentHandler(cfg)(ctx, nil, issueCommentInput{IssueID: iss, Content: "x"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("comment without auth: IsError=false")
		}
	})
	t.Run("issue_state", func(t *testing.T) {
		res, _, err := mcpIssueStateHandler(cfg)(ctx, nil, issueStateInput{IssueID: iss, Status: "done"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("state without auth: IsError=false")
		}
		var st string
		if e := pool.QueryRow(context.Background(),
			`SELECT COALESCE(workflow_status,'') FROM context_blocks WHERE id=$1`, iss).Scan(&st); e != nil {
			t.Fatalf("verify: %v", e)
		}
		if st != "backlog" {
			t.Fatalf("state without auth MUTATED status to %q (want unchanged backlog)", st)
		}
	})
}

func TestMCPIssueToolsTemplateBlastRadius_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfg := w12Cfg(t, pool)

	_, projScope := w6SeedProject(t, pool, "w12tpl")

	// A "shared" scope with an issue the template key must NOT be able to reach.
	// The default-tenant shared scope already exists (migration seeds); seed an
	// issue there.
	sharedIssue := w6SeedIssue(t, pool, "shared", "backlog", "shared target")

	// Template key: home=project, allowed=[], write=[] ⇒ writableBlockScopes=[proj].
	tmpl := w12Writer(projScope)

	t.Run("template_create_in_home_ok", func(t *testing.T) {
		res, _, err := mcpIssueCreateHandler(cfg)(w12Ctx(tmpl), nil, issueCreateInput{Title: "home issue"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if res.IsError {
			t.Fatalf("template create in home scope failed: %s", mcpText(res))
		}
		if !strings.Contains(mcpText(res), `"scope": "`+projScope+`"`) {
			t.Errorf("template create landed outside home scope: %s", mcpText(res))
		}
	})

	t.Run("template_comment_shared_blocked", func(t *testing.T) {
		// Blast-radius: the template key cannot comment on a shared-scope issue —
		// shared ∉ writableBlockScopes(tmpl) ⇒ ErrLinkScopeViolation ⇒ errResult.
		res, _, err := mcpIssueCommentHandler(cfg)(w12Ctx(tmpl), nil, issueCommentInput{IssueID: sharedIssue, Content: "reach"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("template comment on shared issue SUCCEEDED — blast radius breached: %s", mcpText(res))
		}
		// No comment persisted under the shared issue.
		var n int
		if e := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_blocks WHERE parent_id=$1::uuid AND type_name='comment'`, sharedIssue).Scan(&n); e != nil {
			t.Fatalf("verify: %v", e)
		}
		if n != 0 {
			t.Fatalf("template comment WROTE %d comment(s) to the shared issue", n)
		}
	})

	t.Run("shared_write_key_comment_shared_ok", func(t *testing.T) {
		// Contrast (documented non-template behaviour): a key WITH shared in
		// allowed+write CAN comment on the shared issue. The blast-radius sentence
		// is about the TEMPLATE, not a universal MCP guarantee.
		shWriter := &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember,
			HomeScope: projScope, ReadScopes: []string{projScope, "shared"},
			AllowedScopes: []string{"shared"}, WriteScopes: []string{"shared"},
		}
		res, _, err := mcpIssueCommentHandler(cfg)(w12Ctx(shWriter), nil, issueCommentInput{IssueID: sharedIssue, Content: "allowed"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if res.IsError {
			t.Fatalf("shared-write key comment on shared issue FAILED (want success): %s", mcpText(res))
		}
	})
}

func TestMCPIssueStateTransitionPolicy_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	cfg := w12Cfg(t, pool)

	_, scope := w6SeedProject(t, pool, "w12st")
	iss := w6SeedIssue(t, pool, scope, "backlog", "transition target")
	writer := w12Writer(scope)

	t.Run("invalid_transition_errors", func(t *testing.T) {
		res, _, err := mcpIssueStateHandler(cfg)(w12Ctx(writer), nil, issueStateInput{IssueID: iss, Status: "not-a-status"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("invalid transition: IsError=false — a bad target must be an error, not a silent success (%s)", mcpText(res))
		}
		var st string
		if e := pool.QueryRow(context.Background(),
			`SELECT COALESCE(workflow_status,'') FROM context_blocks WHERE id=$1`, iss).Scan(&st); e != nil {
			t.Fatalf("verify: %v", e)
		}
		if st != "backlog" {
			t.Fatalf("invalid transition MUTATED status to %q (want unchanged backlog)", st)
		}
	})

	t.Run("valid_transition_ok", func(t *testing.T) {
		res, _, err := mcpIssueStateHandler(cfg)(w12Ctx(writer), nil, issueStateInput{IssueID: iss, Status: "done"})
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if res.IsError {
			t.Fatalf("valid transition backlog→done errored: %s", mcpText(res))
		}
		var st string
		if e := pool.QueryRow(context.Background(),
			`SELECT COALESCE(workflow_status,'') FROM context_blocks WHERE id=$1`, iss).Scan(&st); e != nil {
			t.Fatalf("verify: %v", e)
		}
		if st != "done" {
			t.Fatalf("valid transition did not persist: status=%q want done", st)
		}
	})
}

// compile-time guard: the three handlers keep the mcp ToolHandlerFor shape.
var (
	_ mcp.ToolHandlerFor[issueCreateInput, any]  = mcpIssueCreateHandler(MCPConfig{})
	_ mcp.ToolHandlerFor[issueCommentInput, any] = mcpIssueCommentHandler(MCPConfig{})
	_ mcp.ToolHandlerFor[issueStateInput, any]   = mcpIssueStateHandler(MCPConfig{})
)
