// Package handler — MCP (Model Context Protocol) server for ctx.
// Exposes ctx tools (query, store, search, get, dream) via the
// Streamable HTTP transport for remote MCP clients like Claude Code.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPConfig holds the wiring needed for the MCP tool handlers. The tools
// consume no runtime configuration (F1-W4): the query tool delegates to the
// QueryHandler, whose per-request config snapshot applies; store/search/get/
// recent are pure pool operations — block embeddings are generated
// asynchronously by the scheduler backfill, not here.
type MCPConfig struct {
	Pool         *pgxpool.Pool
	QueryHandler http.Handler // The full /api/query handler (with scheduler wiring).
	// Cfg feeds the store tool's sensitivity default
	// (pool.default_block_sensitivity, F3 §3.5) — one snapshot per call.
	Cfg ConfigStore
	// Blocktypes feeds the store tool's classify hook (WF T4) — registry
	// snapshot per call, never the compiled-in builtin set. nil in tests
	// without classify wiring: the hook then errors and is logged, the block
	// stays at the default type.
	Blocktypes *blocktype.Registry
}

// NewMCPHandler creates a Streamable HTTP handler for the MCP protocol.
// Auth is handled by wrapping this handler with the existing Auth middleware,
// extracting X-Context-Key (or Authorization: Bearer) from the request.
func NewMCPHandler(cfg MCPConfig) http.Handler {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "ctx",
			Version: "0.25.3",
		},
		&mcp.ServerOptions{
			Instructions: "ctx is a persistent knowledge store with hybrid retrieval. Use 'query' for questions, 'store' to save knowledge, 'search' for lightweight lookups.",
		},
	)

	registerTools(server, cfg)

	return mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:    true, // No session state needed — each call is independent.
			JSONResponse: true, // Prefer JSON over SSE for simple request/response tools.
		},
	)
}

// Tool definitions.

type queryInput struct {
	Question string `json:"question" jsonschema:"the question to answer using hybrid search + LLM synthesis"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max sources to return (default 5)"`
}

type storeInput struct {
	Category    string         `json:"category" jsonschema:"block category (e.g. decisions, learnings, infrastructure)"`
	Title       string         `json:"title" jsonschema:"block title (upserts on same category+title+scope)"`
	Content     string         `json:"content" jsonschema:"block content (max 50KB)"`
	Tags        []string       `json:"tags,omitempty" jsonschema:"optional tags for filtering"`
	Metadata    map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object"`
	Sensitivity string         `json:"sensitivity,omitempty" jsonschema:"content sensitivity for trust gating: credentials|personal|internal|public (default from settings, fail-closed)"`
}

type searchInput struct {
	Query    string   `json:"query,omitempty" jsonschema:"search text"`
	Category string   `json:"category,omitempty" jsonschema:"filter by category"`
	Tags     []string `json:"tags,omitempty" jsonschema:"filter by tags"`
	Limit    int      `json:"limit,omitempty" jsonschema:"max results (default 10)"`
}

type getInput struct {
	ID string `json:"id" jsonschema:"block UUID"`
}

type recentInput struct {
	Limit    int    `json:"limit,omitempty" jsonschema:"max blocks to return (default 10, max 50)"`
	Category string `json:"category,omitempty" jsonschema:"filter by category"`
	// WF T10: opt-in server-side type filters (bind parameters; the recent
	// surface lives here + the chat ctx_recent tool — there is no REST
	// /api/recent route). block_roles_exclude is the legacy alias for
	// types_exclude (seam 17); both present ⇒ union.
	Types             []string `json:"types,omitempty" jsonschema:"only these block types (e.g. knowledge, audit-trail)"`
	TypesExclude      []string `json:"types_exclude,omitempty" jsonschema:"exclude these block types"`
	BlockRolesExclude []string `json:"block_roles_exclude,omitempty" jsonschema:"legacy alias for types_exclude"`
}

func registerTools(server *mcp.Server, cfg MCPConfig) {
	// query
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Description: "Hybrid semantic + fulltext search with LLM synthesis. Ask a question, get a sourced answer.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpQueryHandler(cfg))

	// store
	mcp.AddTool(server, &mcp.Tool{
		Name:        "store",
		Description: "Save or update a knowledge block. Upserts on (category, title, scope).",
	}, mcpStoreHandler(cfg))

	// search
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Lightweight search without LLM synthesis. Returns matching blocks with preview.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpSearchHandler(cfg))

	// get
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get",
		Description: "Fetch a full block by ID.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpGetHandler(cfg))

	// recent
	mcp.AddTool(server, &mcp.Tool{
		Name:        "recent",
		Description: "List recently created or updated blocks. Useful for temporal queries like 'what was saved this week'.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, mcpRecentHandler(cfg))

	// confirm (F6-C6 D-W5): executes a staged write by payload hash. Only
	// meaningful for confirm_writes keys — for everyone else every hash is a
	// generic miss (the store tool never stages for them).
	mcp.AddTool(server, &mcp.Tool{
		Name:        "confirm",
		Description: "Execute a staged write. A key with the confirm_writes capability gets store calls STAGED (response carries a payload_hash); calling confirm with that hash executes the exact server-held write.",
	}, mcpConfirmHandler(cfg))

	// W12: issue-content write tools (issue_create/issue_comment/issue_state,
	// E5 (a) — create+comment+state, no delete). Registered in their own file.
	registerIssueTools(server, cfg)
}

// Tool handlers.

func mcpQueryHandler(cfg MCPConfig) mcp.ToolHandlerFor[queryInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input queryInput) (*mcp.CallToolResult, any, error) {
		if input.Question == "" {
			return errResult("question is required"), nil, nil
		}
		limit := input.Limit
		if limit <= 0 {
			limit = 5
		}

		// Delegate to the internal /api/query handler for full pipeline
		// (temporal, embedding, RRF, gravity, rerank, supersedes, synthesis).
		// The ctx already carries the AuthResult from MCP auth middleware.
		body, _ := json.Marshal(map[string]any{"query": input.Question, "limit": limit})
		internalReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/query", io.NopCloser(strings.NewReader(string(body))))
		internalReq.Header.Set("Content-Type", "application/json")

		rec := &responseRecorder{headers: make(http.Header)}
		cfg.QueryHandler.ServeHTTP(rec, internalReq)

		var qr struct {
			Answer     string `json:"answer"`
			Confidence string `json:"confidence"`
			Sources    []struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Category string `json:"category"`
			} `json:"sources"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.body, &qr); err != nil {
			return errResult("failed to parse query response"), nil, err
		}
		if qr.Error != "" {
			return errResult(qr.Error), nil, nil
		}

		var sb strings.Builder
		sb.WriteString(qr.Answer)
		if len(qr.Sources) > 0 {
			sb.WriteString("\n\nSources:\n")
			for i, s := range qr.Sources {
				fmt.Fprintf(&sb, "[%d] %s (%s) id:%s\n", i+1, s.Title, s.Category, s.ID)
			}
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(sb.String())},
		}, nil, nil
	}
}

func mcpStoreHandler(cfg MCPConfig) mcp.ToolHandlerFor[storeInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input storeInput) (*mcp.CallToolResult, any, error) {
		if input.Category == "" || input.Title == "" || input.Content == "" {
			return errResult("category, title, and content are required"), nil, nil
		}

		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scope := ar.HomeScope

		// Hash NOOP check. Runs BEFORE the D-W5 stage branch on purpose: an
		// identical-content call is a no-op for flagged keys too — no card
		// for a write that would change nothing (D-W2 note 3).
		existingID, err := store.HashNOOPCheck(ctx, cfg.Pool, input.Content, scope, input.Category, input.Title)
		if err == nil && existingID != "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("No change needed — identical content already exists (id: %s)", existingID))},
			}, nil, nil
		}

		// D-W5 branch (F6-C6): a confirm_writes key (090) stages instead of
		// executing — the staged path runs the FULL direct-path gate set
		// (runStageWriteGates) and answers IsError=true (D3-C3). Handler-level
		// on purpose — digest/dream write through store.UpsertBlock internally
		// and must never self-stage. Keys without the flag fall through to the
		// unchanged direct path below (fail-open, D-E2).
		if ar.ConfirmWrites {
			return mcpStageStore(ctx, cfg, ar, input)
		}

		// Sensitivity: request > settings default (mirrors /api/store, F3 §3.5).
		// MT 06-C5: per-tenant via the request context (the MCP ctx carries the
		// resolved AuthResult, used for the scope above) — the tenant's own
		// DefaultBlockSensitivity override applies, else the _global default.
		var sens store.SensitivityWrite
		if cfg.Cfg != nil {
			sens.Value = cfg.Cfg.SnapshotForRequest(ctx).Pool.DefaultBlockSensitivity
		}
		if input.Sensitivity != "" {
			s := backends.Sensitivity(input.Sensitivity)
			if !backends.ValidSensitivity(s) {
				return errResult("invalid sensitivity: must be credentials|personal|internal|public"), nil, nil
			}
			sens = store.SensitivityWrite{Value: s, Manual: true}
		}

		// Upsert.
		block, err := store.UpsertBlock(ctx, cfg.Pool, input.Category, input.Title, input.Content, input.Tags, input.Metadata, scope, false, sens, "")
		if err != nil {
			return errResult(fmt.Sprintf("store failed: %v", err)), nil, nil
		}

		// Welle 44 / WF T4: Auto-classify type_name from the registry
		// snapshot. MCP audit-blocks (welle promotes, ctx-system docs) used to
		// need a follow-up SQL UPDATE — handled inline. Errors are LOGGED, not
		// silently dropped (T4 gate: the pre-T4 `_, _, _ =` discarded a failed
		// classification without a trace).
		var classifySet *blocktype.Set
		if cfg.Blocktypes != nil {
			classifySet = cfg.Blocktypes.SnapshotForRequest(ctx)
		}
		if _, err := store.ClassifyBlockAfterUpsert(ctx, cfg.Pool, classifySet, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("mcp: auto-classify failed", "error", err, "block_id", block.ID)
		}

		// Temporal enrichment (inline, not async — MCP calls are not latency-sensitive).
		times := store.ExtractDates(block.Content)
		_ = store.UpdateContentTimes(ctx, cfg.Pool, block.ID, times)
		_ = store.PopulateTemporal(ctx, cfg.Pool, block.ID, times, block.CreatedAt)

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Stored: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
		}, nil, nil
	}
}

func mcpSearchHandler(cfg MCPConfig) mcp.ToolHandlerFor[searchInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scopes := ar.ReadScopes

		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}

		grants := resolveGrants(ctx, cfg.Pool, ar)
		results, err := store.SearchBlocks(ctx, cfg.Pool, input.Query, scopes, input.Category, input.Tags, limit, true, nil, grants, nil, nil)
		if err != nil {
			return errResult(fmt.Sprintf("search failed: %v", err)), nil, nil
		}

		data, _ := json.MarshalIndent(results, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(data))},
		}, nil, nil
	}
}

func mcpGetHandler(cfg MCPConfig) mcp.ToolHandlerFor[getInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getInput) (*mcp.CallToolResult, any, error) {
		if input.ID == "" {
			return errResult("id is required"), nil, nil
		}

		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scopes := ar.ReadScopes

		grants := resolveGrants(ctx, cfg.Pool, ar)
		resolvedID, matches, err := store.ResolveBlockID(ctx, cfg.Pool, input.ID, scopes, grants)
		if err != nil {
			if errors.Is(err, store.ErrAmbiguousID) {
				body := map[string]any{
					"error":   "Ambiguous id prefix — re-specify with a longer prefix or full id",
					"matches": matches,
				}
				data, _ := json.MarshalIndent(body, "", "  ")
				return errResult(string(data)), nil, nil
			}
			return errResult(fmt.Sprintf("resolve failed: %v", err)), nil, nil
		}
		if resolvedID == "" {
			return errResult("block not found"), nil, nil
		}

		block, err := store.GetBlock(ctx, cfg.Pool, resolvedID, scopes, grants)
		if err != nil {
			return errResult(fmt.Sprintf("get failed: %v", err)), nil, nil
		}

		data, _ := json.MarshalIndent(block, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(data))},
		}, nil, nil
	}
}

func mcpRecentHandler(cfg MCPConfig) mcp.ToolHandlerFor[recentInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input recentInput) (*mcp.CallToolResult, any, error) {
		ar := AuthResultFromContext(ctx)
		if ar == nil { // T07/L7 fail-closed (design/01 §5.4): never fall back to the default tenant
			return errResult("unauthorized: no resolved tenant identity"), nil, nil
		}
		scopes := ar.ReadScopes

		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}

		if err := store.RequireScopes(scopes); err != nil { // T07 fail-closed: this INLINE query bypasses the store-layer guards
			return errResult("unauthorized: no resolved scopes"), nil, nil
		}
		grants := resolveGrants(ctx, cfg.Pool, ar)
		// $1=scopes, $2=grants (block-grant OR-arm, T40a). The mandatory
		// parentheses keep NOT is_archived OUTSIDE the scope/grant OR (a granted
		// archived block must not leak). category, if present, shifts to $3.
		query := `SELECT id, title, category, LEFT(content, 200) AS preview, updated_at
			FROM context_blocks
			WHERE NOT is_archived AND ( scope = ANY($1::text[]) OR id = ANY($2::uuid[]) )`
		args := []any{scopes, grants}

		if input.Category != "" {
			args = append(args, input.Category)
			query += fmt.Sprintf(` AND category = $%d`, len(args))
		}
		// WF T10: opt-in server-side type filters (bind parameters; alias
		// union — see recentInput).
		if len(input.Types) > 0 {
			args = append(args, input.Types)
			query += fmt.Sprintf(` AND type_name = ANY($%d::text[])`, len(args))
		}
		if excl := unionExcludes(input.TypesExclude, input.BlockRolesExclude); len(excl) > 0 {
			args = append(args, excl)
			query += fmt.Sprintf(` AND NOT (type_name = ANY($%d::text[]))`, len(args))
		}
		query += ` ORDER BY updated_at DESC LIMIT ` + fmt.Sprintf("%d", limit)

		rows, err := cfg.Pool.Query(ctx, query, args...)
		if err != nil {
			return errResult(fmt.Sprintf("recent failed: %v", err)), nil, nil
		}
		defer rows.Close()

		var sb strings.Builder
		i := 0
		for rows.Next() {
			var id, title, category, preview string
			var updatedAt time.Time
			if err := rows.Scan(&id, &title, &category, &preview, &updatedAt); err != nil {
				continue
			}
			i++
			fmt.Fprintf(&sb, "[%d] %s (%s, %s) id:%s\n    %s\n", i, title, category, updatedAt.Format("2006-01-02"), id, preview)
		}

		if i == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent("No blocks found.")},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(sb.String())},
		}, nil, nil
	}
}

// Helpers.

// resolveGrants resolves the block-grant set for the caller's tenant (T40a,
// design/07 §4) and is FAIL-CLOSED for grant visibility: on any resolver error
// it logs and returns an empty set so the read proceeds scope-only — a grant
// lookup failure must never crash a read or silently widen visibility.
//
// TENANT-DECISION(authresult-tenantid-shape): the design/07 briefing assumed
// auth.AuthResult.TenantID *string (nullable). The canonical type is a plain
// string (auth.go:24), empty in the sentinel/no-tenant paths. store.GrantedBlockIDs
// already short-circuits an empty/whitespace tenantID to []string{}, so passing
// ar.TenantID directly preserves the intended semantics (empty tenant ⇒ '{}'
// ⇒ no-op OR-arm) without a nil deref. design/07 §4.1.
func resolveGrants(ctx context.Context, pool *pgxpool.Pool, ar *auth.AuthResult) []string {
	grants, err := store.GrantedBlockIDs(ctx, pool, ar.TenantID)
	if err != nil {
		// fail-closed for grant visibility: proceed scope-only, never crash the read
		slog.Warn("mcp: resolve granted block ids failed — scope-only visibility", "error", err)
		return []string{}
	}
	return grants
}

func textContent(text string) mcp.Content {
	return &mcp.TextContent{Text: text}
}

func errResult(msg string) *mcp.CallToolResult {
	r := &mcp.CallToolResult{
		Content: []mcp.Content{textContent(msg)},
	}
	r.IsError = true
	return r
}

// responseRecorder captures an HTTP response for internal handler delegation.
type responseRecorder struct {
	headers    http.Header
	body       []byte
	statusCode int
}

func (r *responseRecorder) Header() http.Header  { return r.headers }
func (r *responseRecorder) WriteHeader(code int) { r.statusCode = code }
func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}

// Flush and SetWriteDeadline are no-ops so HandleQuery's body-heartbeat
// (always armed when rerank is on, query.go) does not emit a "not flushable" /
// "cannot extend write deadline" warning per internal delegation — that
// regression alarm must stay sharp on the REAL HTTP path. The heartbeat's
// leading-whitespace writes land in the recorded body; RFC-8259 tolerates
// leading whitespace, so the JSON decode stays correct. Covers the MCP query
// delegation and the F6 ctx_query tool delegation alike.
func (r *responseRecorder) Flush()                           {}
func (r *responseRecorder) SetWriteDeadline(time.Time) error { return nil }
