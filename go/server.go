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
	// BlobMaxBodySize is the body size limit for blob-store (75 MB: 50MB binary + base64 overhead).
	BlobMaxBodySize = 75 << 20 // 75 MB
)

// NewRouter creates the chi router with all routes and middleware.
func NewRouter(pool *pgxpool.Pool, cfg Config, scheduler *events.Scheduler) *chi.Mux {
	r := chi.NewRouter()

	// Global middleware
	r.Use(handler.RequestID)
	r.Use(handler.Logger)
	r.Use(handler.Recovery)

	// Health check (no auth, no body)
	h := handler.NewHealthHandler(pool, cfg.OllamaHost)
	r.Get("/health", h.Health)

	// All authenticated routes in a single group with Auth middleware as first defense line.
	queryHandler := handler.NewQueryHandler(pool, cfg.OllamaHost, cfg.OllamaEmbedModel, cfg.OllamaChatModel, cfg.RerankEnabled)
	storeH := handler.NewStoreHandler(pool, cfg.OllamaHost, cfg.OllamaEmbedModel)
	searchH := handler.NewSearchHandler(pool)
	manageH := handler.NewManageHandler(pool, cfg.OllamaHost, cfg.OllamaEmbedModel)
	blobH := handler.NewBlobHandler(pool)
	digestH := handler.NewDigestHandler(pool)

	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Use(handler.MaxBodySize(DefaultMaxBodySize))

		// Context Agent — Query pipeline (RRF + LLM synthesis)
		r.Post("/api/query", handler.WithScheduler(scheduler, queryHandler.HandleQuery))
		r.Post("/webhook/context-agent", handler.WithScheduler(scheduler, queryHandler.HandleQuery))

		// Context Store — Upsert + Auto-Embedding
		r.Post("/webhook/context-store", storeH.HandleStore)

		// Context Search — Lightweight FTS (no LLM)
		r.Post("/webhook/context-search", searchH.HandleSearch)

		// Context Manage — CRUD + Guard API
		r.Post("/webhook/context-manage", manageH.HandleManage)

		// Blob Storage — fetch, search, manage (default body limit)
		r.Post("/webhook/blob-fetch", blobH.HandleBlobFetch)
		r.Post("/webhook/blob-search", blobH.HandleBlobSearch)
		r.Post("/webhook/blob-manage", blobH.HandleBlobManage)

		// Digest — Topic map generation
		r.Post("/webhook/context-digest", digestH.HandleDigest)
	})

	// Blob Store — larger body limit (75 MB for base64-encoded files)
	r.Group(func(r chi.Router) {
		r.Use(handler.Auth(pool))
		r.Use(handler.MaxBodySize(BlobMaxBodySize))
		r.Post("/webhook/blob-store", blobH.HandleBlobStore)
	})

	return r
}
