package main

import (
	"net/http"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/web"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// DefaultMaxBodySize is the default body size limit for all endpoints (1 MB).
	DefaultMaxBodySize = 1 << 20 // 1 MB
	// IngestMaxBodySize is the body size limit for /api/ingest (10 MB for bulk chunks).
	IngestMaxBodySize = 10 << 20 // 10 MB
	// BlobMaxBodySize is the body size limit for blob-store (75 MB: 50MB binary + base64 overhead).
	BlobMaxBodySize = 75 << 20 // 75 MB
)

// NewRouter creates the chi router with all routes and middleware. cfgStore
// is the runtime-config snapshot store (F1-W4): the QueryHandler reads one
// snapshot per request from it; the remaining handlers still consume the
// legacy bridge view until their store adoption (F1-W6/W7).
func NewRouter(pool *pgxpool.Pool, cfg Config, cfgStore *config.Store, scheduler *events.Scheduler) *chi.Mux {
	r := chi.NewRouter()

	// Global middleware
	r.Use(handler.SecurityHeaders)
	r.Use(handler.RequestID)
	r.Use(handler.Logger)
	r.Use(handler.Recovery)

	// Health check (no auth, no body)
	h := handler.NewHealthHandler(pool, cfg.EmbedHost, cfg.ChatHost, cfg.DreamHost)
	r.Get("/health", h.Health)

	// OAuth 2.1 endpoints for MCP remote auth (no auth middleware — these ARE the auth flow).
	oauthH := handler.NewOAuthHandler(pool)
	r.Get("/.well-known/oauth-authorization-server", oauthH.Metadata)
	r.Get("/.well-known/oauth-protected-resource", oauthH.ProtectedResource)
	r.HandleFunc("/authorize", oauthH.Authorize) // GET = form, POST = submit
	r.Post("/token", oauthH.Token)

	// All authenticated routes in a single group with Auth middleware as first defense line.
	queryHandler := handler.NewQueryHandler(pool, cfgStore)
	storeH := handler.NewStoreHandler(pool, cfg.RateLimitWrite)
	searchH := handler.NewSearchHandler(pool, cfg.RateLimitRead)
	graphH := handler.NewGraphHandler(pool, cfg.RateLimitRead)
	manageH := handler.NewManageHandler(pool, scheduler)
	whoamiH := handler.NewWhoamiHandler(pool)
	blobH := handler.NewBlobHandler(pool, cfg.RateLimitWrite)
	digestH := handler.NewDigestHandler(pool)
	// Welle 42: daily synthesis manual trigger. Uses the dream model + dream
	// chat host so the trigger and the scheduled 03:00-local goroutine share
	// one Ollama backend. Falls back to ChatModel when DreamModel is empty.
	dreamThink := parseThinkMode(cfg.DreamThink)
	if cfg.DreamThink == "" {
		dreamThink = parseThinkMode(cfg.ChatThink)
	}
	dreamModel := cfg.DreamModel
	if dreamModel == "" {
		dreamModel = cfg.ChatModel
	}
	dreamOpts := dream.DreamOptions()
	if cfg.DreamNumCtx > 0 {
		dreamOpts.NumCtx = cfg.DreamNumCtx
	} else if cfg.ChatNumCtx > 0 {
		// Consistency: chat and dream share one Ollama model (same 27B). When no
		// dedicated DreamNumCtx is set, the daily-synthesis chat-model request
		// must carry the same num_ctx as every other chat-model call site so
		// Ollama keeps a single runner (distinct num_ctx → extra runner → VRAM OOM).
		dreamOpts.NumCtx = cfg.ChatNumCtx
	}
	synthH := handler.NewSynthesizeHandler(pool, cfg.DreamHost, cfg.DreamAPIKey, dreamModel, dreamThink, dreamOpts)

	// ── MCP endpoint (Streamable HTTP, authenticated) ──────────────
	queryHTTPHandler := handler.WithScheduler(scheduler, queryHandler.HandleQuery)
	mcpH := handler.NewMCPHandler(handler.MCPConfig{
		Pool:         pool,
		QueryHandler: http.HandlerFunc(queryHTTPHandler),
	})
	// MCP endpoint — auth middleware injects AuthResult into context.
	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Handle("/mcp", mcpH)
	})

	// ── API routes (canonical) ──────────────────────────────────────
	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Use(handler.MaxBodySize(DefaultMaxBodySize))

		// Query — RRF + LLM synthesis
		r.Post("/api/query", queryHTTPHandler)
		// Store — Upsert + Auto-Embedding
		r.Post("/api/store", storeH.HandleStore)
		// Search — Lightweight FTS (no LLM)
		r.Post("/api/search", searchH.HandleSearch)
		// Graph — scope-filtered k-hop ego subgraph (read-only, no LLM)
		r.Get("/api/graph/ego", graphH.HandleEgo)
		// Whoami — key identity for the SPA login gate (F4-W3)
		r.Get("/api/whoami", whoamiH.HandleWhoami)
		// Manage — CRUD + Guard API
		r.Post("/api/manage", manageH.HandleManage)
		// Digest — Topic map generation
		r.Post("/api/digest", digestH.HandleDigest)
		// Synthesize — manual daily synthesis trigger (Welle 42)
		r.Post("/api/synthesize/daily", synthH.HandleDaily)
		// Blob — fetch, search, manage
		r.Post("/api/blob/fetch", blobH.HandleBlobFetch)
		r.Post("/api/blob/search", blobH.HandleBlobSearch)
		r.Post("/api/blob/manage", blobH.HandleBlobManage)
	})

	// Ingest — larger body limit (10 MB for bulk chunk import)
	ingestH := handler.NewIngestHandler(pool)
	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Use(handler.MaxBodySize(IngestMaxBodySize))
		r.Post("/api/ingest", ingestH.HandleIngest)
	})

	// Blob Store — larger body limit (75 MB for base64-encoded files)
	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Use(handler.MaxBodySize(BlobMaxBodySize))
		r.Post("/api/blob/store", blobH.HandleBlobStore)
	})

	// Embedded SPA — mounted last: chi matches registered routes first, only
	// unknown paths fall through (history-API fallback for HTML navigations
	// only; mistyped API URLs stay 404, known path + wrong method stays 405).
	r.NotFound(web.Handler().ServeHTTP)

	return r
}
