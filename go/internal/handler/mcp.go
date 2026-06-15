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
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
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
	Category string         `json:"category" jsonschema:"block category (e.g. decisions, learnings, infrastructure)"`
	Title    string         `json:"title" jsonschema:"block title (upserts on same category+title+scope)"`
	Content  string         `json:"content" jsonschema:"block content (max 50KB)"`
	Tags     []string       `json:"tags,omitempty" jsonschema:"optional tags for filtering"`
	Metadata map[string]any `json:"metadata,omitempty" jsonschema:"optional metadata object"`
	Sensitivity string     `json:"sensitivity,omitempty" jsonschema:"content sensitivity for trust gating: credentials|personal|internal|public (default from settings, fail-closed)"`
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

		// Hash NOOP check.
		existingID, err := store.HashNOOPCheck(ctx, cfg.Pool, input.Content, scope, input.Category, input.Title)
		if err == nil && existingID != "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("No change needed — identical content already exists (id: %s)", existingID))},
			}, nil, nil
		}

		// Sensitivity: request > settings default (mirrors /api/store, F3 §3.5).
		var sens store.SensitivityWrite
		if cfg.Cfg != nil {
			sens.Value = cfg.Cfg.Snapshot().Pool.DefaultBlockSensitivity
		}
		if input.Sensitivity != "" {
			s := backends.Sensitivity(input.Sensitivity)
			if !backends.ValidSensitivity(s) {
				return errResult("invalid sensitivity: must be credentials|personal|internal|public"), nil, nil
			}
			sens = store.SensitivityWrite{Value: s, Manual: true}
		}

		// Upsert.
		block, err := store.UpsertBlock(ctx, cfg.Pool, input.Category, input.Title, input.Content, input.Tags, input.Metadata, scope, false, sens)
		if err != nil {
			return errResult(fmt.Sprintf("store failed: %v", err)), nil, nil
		}

		// Welle 44: Auto-classify block_role + is_meta from metadata + title.
		// MCP audit-blocks (welle promotes, ctx-system docs) used to need a
		// follow-up SQL UPDATE — now handled inline.
		_, _, _ = store.ClassifyBlockAfterUpsert(ctx, cfg.Pool, block.ID, block.Title, block.Metadata)

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

		results, err := store.SearchBlocks(ctx, cfg.Pool, input.Query, scopes, input.Category, input.Tags, limit, true, nil)
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

		resolvedID, matches, err := store.ResolveBlockID(ctx, cfg.Pool, input.ID, scopes)
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

		block, err := store.GetBlock(ctx, cfg.Pool, resolvedID, scopes)
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
		query := `SELECT id, title, category, LEFT(content, 200) AS preview, updated_at
			FROM context_blocks
			WHERE NOT is_archived AND scope = ANY($1::text[])`
		args := []any{scopes}

		if input.Category != "" {
			query += ` AND category = $2`
			args = append(args, input.Category)
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

func (r *responseRecorder) Header() http.Header        { return r.headers }
func (r *responseRecorder) WriteHeader(code int)        { r.statusCode = code }
func (r *responseRecorder) Write(b []byte) (int, error) { r.body = append(r.body, b...); return len(b), nil }

// Flush and SetWriteDeadline are no-ops so HandleQuery's body-heartbeat
// (always armed when rerank is on, query.go) does not emit a "not flushable" /
// "cannot extend write deadline" warning per internal delegation — that
// regression alarm must stay sharp on the REAL HTTP path. The heartbeat's
// leading-whitespace writes land in the recorded body; RFC-8259 tolerates
// leading whitespace, so the JSON decode stays correct. Covers the MCP query
// delegation and the F6 ctx_query tool delegation alike.
func (r *responseRecorder) Flush()                          {}
func (r *responseRecorder) SetWriteDeadline(time.Time) error { return nil }
