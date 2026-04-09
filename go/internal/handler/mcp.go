// Package handler — MCP (Model Context Protocol) server for ctx.
// Exposes ctx tools (query, store, search, get, dream) via the
// Streamable HTTP transport for remote MCP clients like Claude Code.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPConfig holds the configuration needed for MCP tool handlers.
type MCPConfig struct {
	Pool          *pgxpool.Pool
	EmbedHost     string
	EmbedAPIKey   string
	EmbedModel    string
	EmbedNumCtx   int
	ChatHost      string
	ChatAPIKey    string
	ChatModel     string
	ChatThink     *bool
	RerankEnabled bool
	Timezone      *time.Location
	QueryHandler  http.Handler // The full /api/query handler (with scheduler wiring).
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

type noOutput struct{}

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

func mcpQueryHandler(cfg MCPConfig) mcp.ToolHandlerFor[queryInput, noOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input queryInput) (*mcp.CallToolResult, noOutput, error) {
		if input.Question == "" {
			return errResult("question is required"), noOutput{}, nil
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
			return errResult("failed to parse query response"), noOutput{}, err
		}
		if qr.Error != "" {
			return errResult(qr.Error), noOutput{}, nil
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
		}, noOutput{}, nil
	}
}

func mcpStoreHandler(cfg MCPConfig) mcp.ToolHandlerFor[storeInput, noOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input storeInput) (*mcp.CallToolResult, noOutput, error) {
		if input.Category == "" || input.Title == "" || input.Content == "" {
			return errResult("category, title, and content are required"), noOutput{}, nil
		}

		ar := AuthResultFromContext(ctx)
		scope := "private"
		if ar != nil {
			scope = ar.HomeScope
		}

		// Hash NOOP check.
		existingID, err := store.HashNOOPCheck(ctx, cfg.Pool, input.Content, scope, input.Category, input.Title)
		if err == nil && existingID != "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{textContent(fmt.Sprintf("No change needed — identical content already exists (id: %s)", existingID))},
			}, noOutput{}, nil
		}

		// Upsert.
		block, err := store.UpsertBlock(ctx, cfg.Pool, input.Category, input.Title, input.Content, input.Tags, input.Metadata, scope, false)
		if err != nil {
			return errResult(fmt.Sprintf("store failed: %v", err)), noOutput{}, nil
		}

		// Temporal enrichment (inline, not async — MCP calls are not latency-sensitive).
		times := store.ExtractDates(block.Content)
		_ = store.UpdateContentTimes(ctx, cfg.Pool, block.ID, times)
		_ = store.PopulateTemporal(ctx, cfg.Pool, block.ID, times, block.CreatedAt)

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(fmt.Sprintf("Stored: %s (id: %s, category: %s)", block.Title, block.ID, block.Category))},
		}, noOutput{}, nil
	}
}

func mcpSearchHandler(cfg MCPConfig) mcp.ToolHandlerFor[searchInput, noOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, noOutput, error) {
		ar := AuthResultFromContext(ctx)
		scopes := []string{"private"}
		if ar != nil {
			scopes = ar.ReadScopes
		}

		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}

		results, err := store.SearchBlocks(ctx, cfg.Pool, input.Query, scopes, input.Category, input.Tags, limit, true)
		if err != nil {
			return errResult(fmt.Sprintf("search failed: %v", err)), noOutput{}, nil
		}

		data, _ := json.MarshalIndent(results, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(data))},
		}, noOutput{}, nil
	}
}

func mcpGetHandler(cfg MCPConfig) mcp.ToolHandlerFor[getInput, noOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input getInput) (*mcp.CallToolResult, noOutput, error) {
		if input.ID == "" {
			return errResult("id is required"), noOutput{}, nil
		}

		ar := AuthResultFromContext(ctx)
		scopes := []string{"private"}
		if ar != nil {
			scopes = ar.ReadScopes
		}

		block, err := store.GetBlock(ctx, cfg.Pool, input.ID, scopes)
		if err != nil {
			return errResult(fmt.Sprintf("get failed: %v", err)), noOutput{}, nil
		}

		data, _ := json.MarshalIndent(block, "", "  ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(string(data))},
		}, noOutput{}, nil
	}
}

func mcpRecentHandler(cfg MCPConfig) mcp.ToolHandlerFor[recentInput, noOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input recentInput) (*mcp.CallToolResult, noOutput, error) {
		ar := AuthResultFromContext(ctx)
		scopes := []string{"private"}
		if ar != nil {
			scopes = ar.ReadScopes
		}

		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}

		query := `SELECT id, title, category, LEFT(content, 200) AS preview, updated_at
			FROM context_blocks
			WHERE NOT is_archived AND scope = ANY($1)`
		args := []any{scopes}

		if input.Category != "" {
			query += ` AND category = $2`
			args = append(args, input.Category)
		}
		query += ` ORDER BY updated_at DESC LIMIT ` + fmt.Sprintf("%d", limit)

		rows, err := cfg.Pool.Query(ctx, query, args...)
		if err != nil {
			return errResult(fmt.Sprintf("recent failed: %v", err)), noOutput{}, nil
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
			}, noOutput{}, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{textContent(sb.String())},
		}, noOutput{}, nil
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

func (r *responseRecorder) Header() http.Header         { return r.headers }
func (r *responseRecorder) WriteHeader(code int)         { r.statusCode = code }
func (r *responseRecorder) Write(b []byte) (int, error)  { r.body = append(r.body, b...); return len(b), nil }

func backfillEmbeddings(ctx context.Context, cfg MCPConfig) {
	for {
		var blockID, title, content string
		err := cfg.Pool.QueryRow(ctx,
			`SELECT id, title, content FROM context_blocks
			WHERE embedding IS NULL AND NOT is_archived LIMIT 1`).Scan(&blockID, &title, &content)
		if err != nil {
			break
		}
		embedText := title + "\n\n" + content
		vec, err := embed.Embed(ctx, cfg.EmbedHost, cfg.EmbedAPIKey, cfg.EmbedModel, embedText, embed.PrefixDocument, cfg.EmbedNumCtx)
		if err != nil {
			slog.Warn("mcp backfill: embed failed", "block_id", blockID, "error", err)
			break
		}
		if err := store.StoreEmbedding(ctx, cfg.Pool, blockID, vec); err != nil {
			break
		}
	}
}
