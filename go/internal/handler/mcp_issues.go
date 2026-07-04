// Package handler — MCP issue-content write tools (workflow W12, design/03 §7-W12,
// E5 decision (a): create + comment + state, NO delete). These three tools expose
// the SAME store primitives as the REST W7 surface and the manage transport — one
// logic, three transports (the §4.1 house pattern). No new store logic ships here.
//
// SCOPE MODEL (design/03 §5.5, differs from REST): the MCP tools carry NO project
// parameter. A create always writes the caller key's HomeScope (exactly like the
// `store` tool, mcp.go). A comment/state targets an existing issue BY ID; the
// write is gated by writableBlockScopes(ar) — the SAME single block-write formula
// (context_store.go: home ∪ write_scopes∩readable ∪ shared-if-allowed) the REST
// and manage paths use. Foreign-scope via MCP arrives only with F6-C6 (Fremd-Scope
// write-confirmation), which this wave does not build.
//
// AGENT-KEY-TEMPLATE CONTRACT (design/03 §4.8 Regel 4, §5.5 Blast-Radius): the
// blast-radius claim — "an agent may only touch its own project" — holds ONLY for
// a key minted from the template (home_scope = the project scope, allowed_scopes
// = [], write_scopes = []). For such a key writableBlockScopes(ar) == [HomeScope],
// so a comment/state on any block outside that scope fails closed (ErrLinkScope /
// ErrIssueNotFound → errResult, uniform no-oracle). A key that additionally holds
// `shared` in allowed+write CAN write shared — that is the correct, documented
// behaviour; the blast-radius sentence is a statement about the TEMPLATE, not a
// universal MCP guarantee.
//
// FAIL-CLOSED (T07/L7, design/01 §5.4): every handler requires a resolved
// AuthResult; ar == nil ⇒ errResult, never a fall-back to the default tenant. In
// the live mount /mcp sits behind Auth(pool) (server.go), so ar==nil is
// unreachable in production — this is defense-in-depth against a mount/middleware
// regression, mirroring the query/store/search/get/recent tools.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// issueCreateInput is the issue_create tool argument shape. Title is REQUIRED (no
// omitempty ⇒ jsonschema required); everything else is optional. There is
// deliberately NO scope field — the write scope is the key's HomeScope (§5.5).
type issueCreateInput struct {
	Title    string         `json:"title" jsonschema:"issue title (human title; a ctx #L<seq> prefix is added automatically). Writes to your key's home scope."`
	Content  string         `json:"content,omitempty" jsonschema:"issue body markdown (max 50KB)"`
	Tags     []string       `json:"tags,omitempty" jsonschema:"optional tags"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object (e.g. labels)"`
	Status   string         `json:"status,omitempty" jsonschema:"initial workflow status; must be a valid entry status for the issue type (default from type policy)"`
}

// issueCommentInput is the issue_comment tool argument shape. IssueID is REQUIRED;
// the comment ALWAYS inherits the parent issue's scope (never the request), gated
// by writableBlockScopes(ar).
type issueCommentInput struct {
	IssueID  string         `json:"issue_id" jsonschema:"UUID of the parent issue to comment on (must be in a scope your key may write)"`
	Content  string         `json:"content,omitempty" jsonschema:"comment body markdown (max 50KB)"`
	Author   string         `json:"author,omitempty" jsonschema:"comment author label (default 'anon')"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object"`
}

// issueStateInput is the issue_state tool argument shape. Both fields REQUIRED. The
// transition is validated against POLICY DATA (set.ValidateTransition) — an
// out-of-policy target is an error, never a silent success.
type issueStateInput struct {
	IssueID string `json:"issue_id" jsonschema:"UUID of the issue to transition (must be in a scope your key may write)"`
	Status  string `json:"status" jsonschema:"target workflow status; must be a valid transition from the current status (policy-validated)"`
}

// Tool descriptions carry the template contract so an agent operator reads the
// blast-radius conditioning at the tool surface, not only in docs (§5.5).
const (
	issueCreateDesc  = "Create an issue in your key's home scope. E5 issue-content write (no delete). The initial workflow status comes from type policy; a supplied status must be a valid entry status. Agent-key-template contract: a key minted from the project template (home_scope=project, allowed=[], write=[]) can only touch that project's scope."
	issueCommentDesc = "Add a comment to an issue by id. The comment inherits the parent issue's scope; you must hold write on that scope (writableBlockScopes). Agent-key-template keys are bounded to their own project scope."
	issueStateDesc   = "Transition an issue's workflow status by id. The target must be a valid transition from the current status under type policy (an invalid target is an error, not a no-op). You must hold write on the issue's scope."
)

// registerIssueTools adds the three W12 issue-content write tools. Called from
// registerTools (mcp.go). Kept in its own function so the tool set is one grep.
func registerIssueTools(server *mcp.Server, cfg MCPConfig) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "issue_create",
		Description: issueCreateDesc,
	}, mcpIssueCreateHandler(cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "issue_comment",
		Description: issueCommentDesc,
	}, mcpIssueCommentHandler(cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "issue_state",
		Description: issueStateDesc,
	}, mcpIssueStateHandler(cfg))
}

func mcpIssueCreateHandler(cfg MCPConfig) mcp.ToolHandlerFor[issueCreateInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input issueCreateInput) (*mcp.CallToolResult, any, error) {
		if input.Title == "" {
			return errResult("title is required"), nil, nil
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed: never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		set := cfg.issueSet(ctx)
		if set == nil {
			return errResult("type registry unavailable"), nil, nil
		}
		// Initial status from policy DATA; a client-supplied status must be a valid
		// ENTRY transition ("" → status). Swapping the registry set changes this
		// verdict with no Go rebuild (design/03 §4.3), identical to REST/manage.
		status := set.WorkflowInitial(store.IssueTypeName)
		if input.Status != "" {
			if err := set.ValidateTransition(store.IssueTypeName, "", input.Status); err != nil {
				return mcpIssueError("issue_create", err), nil, nil
			}
			status = input.Status
		}
		scope := ar.HomeScope
		b, err := issueTx(ctx, cfg.Pool, func(tx pgx.Tx) (*store.Block, error) {
			return store.InsertIssueBlock(ctx, tx, store.IssueFields{
				Scope: scope, Title: input.Title, Content: input.Content,
				Tags: input.Tags, Metadata: input.Metadata, Status: status,
			}, writableBlockScopes(ar))
		})
		if err != nil {
			return mcpIssueError("issue_create", err), nil, nil
		}
		return mcpIssueResult("issue", b), nil, nil
	}
}

func mcpIssueCommentHandler(cfg MCPConfig) mcp.ToolHandlerFor[issueCommentInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input issueCommentInput) (*mcp.CallToolResult, any, error) {
		if !uuidRe.MatchString(input.IssueID) {
			return errResult("not found"), nil, nil // malformed id ⇒ uniform no-oracle
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		b, err := issueTx(ctx, cfg.Pool, func(tx pgx.Tx) (*store.Block, error) {
			return store.InsertCommentBlock(ctx, tx, input.IssueID, store.CommentFields{
				Author: input.Author, Content: input.Content, Metadata: input.Metadata,
			}, writableBlockScopes(ar))
		})
		if err != nil {
			return mcpIssueError("issue_comment", err), nil, nil
		}
		return mcpIssueResult("comment", b), nil, nil
	}
}

func mcpIssueStateHandler(cfg MCPConfig) mcp.ToolHandlerFor[issueStateInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input issueStateInput) (*mcp.CallToolResult, any, error) {
		if !uuidRe.MatchString(input.IssueID) {
			return errResult("not found"), nil, nil
		}
		if input.Status == "" {
			return errResult("status is required"), nil, nil
		}
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		set := cfg.issueSet(ctx)
		if set == nil {
			return errResult("type registry unavailable"), nil, nil
		}
		status := input.Status
		b, err := issueTx(ctx, cfg.Pool, func(tx pgx.Tx) (*store.Block, error) {
			return store.UpdateIssueBlock(ctx, tx, input.IssueID, store.IssueUpdate{
				Status: &status,
			}, set, writableBlockScopes(ar))
		})
		if err != nil {
			return mcpIssueError("issue_state", err), nil, nil
		}
		return mcpIssueResult("issue", b), nil, nil
	}
}

// issueSet resolves the request's block-type registry snapshot, or nil (with a
// WARN) when the registry is not wired — the caller then fails closed. Mirrors the
// manage-transport issueSet (context_manage_issues.go) for the MCP config.
func (cfg MCPConfig) issueSet(ctx context.Context) *blocktype.Set {
	if cfg.Blocktypes == nil {
		slog.Warn("mcp: block-type registry not wired — issue tools fail closed")
		return nil
	}
	return cfg.Blocktypes.SnapshotForRequest(ctx)
}

// mcpIssueResult serialises the SAME wire envelope as the REST W7 write handlers
// (project_issues_write.go) — {success, render:'untrusted', <key>:block} — so the
// contract-freeze golden (contract_freeze_golden_test.go, K5) can assert MCP↔REST
// field-name parity. The envelope keys are literals HERE (rename ⇒ golden red);
// the block field names come from the shared store.Block json tags (rename a tag
// ⇒ BOTH transports drift, the golden catches it on either side). key is "issue"
// or "comment".
func mcpIssueResult(key string, b *store.Block) *mcp.CallToolResult {
	env := map[string]any{
		"success": true,
		"render":  "untrusted",
		key:       b,
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return errResult(fmt.Sprintf("serialize failed: %v", err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{textContent(string(data))}}
}

// mcpIssueError maps the issue store sentinels onto MCP errResults, MIRRORING the
// REST writeIssueStoreError status mapping (context_manage_issues.go) so the two
// transports agree on which conditions are errors: not-found/scope-violation ⇒
// uniform "not found" (no existence oracle); non-writable requested scope ⇒ "scope
// not writable"; an out-of-policy transition / body-cap ⇒ the 422-equivalent
// message; everything else ⇒ a generic message (logged, no wire detail). Every
// branch sets IsError (errResult) — an invalid transition is NEVER a silent
// success (the state-transition-policy gate).
func mcpIssueError(tool string, err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, store.ErrIssueNotFound), errors.Is(err, store.ErrLinkScopeViolation):
		return errResult("not found")
	case errors.Is(err, store.ErrIssueScope):
		return errResult("scope not writable")
	case errors.Is(err, store.ErrCommentParentRequired):
		return errResult("comment requires an issue_id")
	case errors.Is(err, store.ErrIssueBody):
		return errResult("body exceeds 50 KB cap")
	case errors.Is(err, blocktype.ErrInvalidTransition),
		errors.Is(err, blocktype.ErrNoWorkflow),
		errors.Is(err, blocktype.ErrUnknownType):
		return errResult(err.Error()) // 422-equivalent: policy-validated transition
	default:
		slog.Error("mcp "+tool+" error", "error", err)
		return errResult("issue write failed")
	}
}
