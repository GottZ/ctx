package main

import (
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/handler"
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

// NewRouter creates the chi router with all routes and middleware.
func NewRouter(pool *pgxpool.Pool, cfg Config, scheduler *events.Scheduler) *chi.Mux {
	r := chi.NewRouter()

	// Global middleware
	r.Use(handler.SecurityHeaders)
	r.Use(handler.RequestID)
	r.Use(handler.Logger)
	r.Use(handler.Recovery)

	// Health check (no auth, no body)
	h := handler.NewHealthHandler(pool, cfg.EmbedHost, cfg.ChatHost, cfg.DreamHost)
	r.Get("/health", h.Health)

	// All authenticated routes in a single group with Auth middleware as first defense line.
	chatThink := parseThinkMode(cfg.ChatThink)
	queryHandler := handler.NewQueryHandler(pool, cfg.ChatHost, cfg.ChatAPIKey, cfg.EmbedHost, cfg.EmbedAPIKey, cfg.EmbedModel, cfg.EmbedNumCtx, cfg.ChatModel, chatThink, cfg.RerankEnabled, cfg.Timezone, cfg.RateLimitRead)
	storeH := handler.NewStoreHandler(pool, cfg.RateLimitWrite)
	searchH := handler.NewSearchHandler(pool, cfg.RateLimitRead)
	manageH := handler.NewManageHandler(pool, scheduler)
	blobH := handler.NewBlobHandler(pool, cfg.RateLimitWrite)
	digestH := handler.NewDigestHandler(pool)

	// ── API routes (canonical) ──────────────────────────────────────
	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Use(handler.MaxBodySize(DefaultMaxBodySize))

		// Query — RRF + LLM synthesis
		r.Post("/api/query", handler.WithScheduler(scheduler, queryHandler.HandleQuery))
		// Store — Upsert + Auto-Embedding
		r.Post("/api/store", storeH.HandleStore)
		// Search — Lightweight FTS (no LLM)
		r.Post("/api/search", searchH.HandleSearch)
		// Manage — CRUD + Guard API
		r.Post("/api/manage", manageH.HandleManage)
		// Digest — Topic map generation
		r.Post("/api/digest", digestH.HandleDigest)
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

	return r
}
