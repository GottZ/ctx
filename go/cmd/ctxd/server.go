package main

import (
	"context"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/settings"
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
// is the runtime-config snapshot store: every config-consuming handler reads
// one snapshot per request from it (F1-W4–W7) — no handler holds a boot copy,
// so a config replace is live from the next request on.
func NewRouter(ctx context.Context, pool *pgxpool.Pool, cfgStore *config.Store, scheduler *events.Scheduler, backendPool *backends.Pool, blocktypeReg *blocktype.Registry) *chi.Mux {
	r := chi.NewRouter()

	// MT 06-C5: wire the request→tenant-scope hook so SnapshotForRequest can
	// derive the caller's tenant from the auth result. This is the cycle-free
	// seam — config cannot import handler, so the handler wrapper is injected
	// here, where main already holds both packages. Set before any route is
	// mounted (and well before the server serves a request), so the
	// unsynchronized write in config rides the boot happens-before, exactly as
	// SetOverlay (main.go) does. Inert until a tenant has settings rows.
	config.SetRequestScopeHook(handler.RequestTenantScope)

	// Global middleware
	r.Use(handler.SecurityHeaders)
	r.Use(handler.RequestID)
	r.Use(handler.Logger)
	r.Use(handler.Recovery)

	// Health check (no auth, no body). blocktypeReg feeds the
	// blocktype_registry degradation field (WF T3, design 01 §4.3).
	h := handler.NewHealthHandler(pool, cfgStore, backendPool, blocktypeReg)
	r.Get("/health", h.Health)

	// OAuth 2.1 endpoints for MCP remote auth (no auth middleware — these ARE the auth flow).
	oauthH := handler.NewOAuthHandler(pool)
	r.Get("/.well-known/oauth-authorization-server", oauthH.Metadata)
	r.Get("/.well-known/oauth-protected-resource", oauthH.ProtectedResource)
	r.HandleFunc("/authorize", oauthH.Authorize) // GET = form, POST = submit
	r.Post("/token", oauthH.Token)

	// Per-tenant quota accountant (T36, 04-W4): one process-wide instance, a
	// lock-free TTL-cached per-tenant cost/call rollup feeding the synthesis
	// gate. 30s TTL (§6.2) — the cost SUM over the 1M+ llm_log hypertable is
	// never run per request.
	quota := backends.NewQuotaAccountant(pool, 30*time.Second)

	// All authenticated routes in a single group with Auth middleware as first defense line.
	queryHandler := handler.NewQueryHandler(pool, cfgStore, backendPool, quota, blocktypeReg)
	storeH := handler.NewStoreHandler(pool, cfgStore, blocktypeReg)
	searchH := handler.NewSearchHandler(pool, cfgStore)
	graphH := handler.NewGraphHandler(pool, cfgStore, blocktypeReg)
	overviewH := handler.NewGraphOverviewHandler(pool, cfgStore)
	// gamingReload re-builds the config snapshot from context_settings after a
	// gaming-mode write (F3-P6), so the toggle hits the next chain without a
	// restart (same path PUT /api/settings uses).
	gamingReload := func(ctx context.Context) error {
		return settings.Reload(ctx, pool, cfgStore)
	}
	manageH := handler.NewManageHandler(pool, cfgStore, scheduler, backendPool, scheduler, gamingReload, quota, blocktypeReg)
	whoamiH := handler.NewWhoamiHandler(pool)
	blobH := handler.NewBlobHandler(pool, cfgStore)
	digestH := handler.NewDigestHandler(pool, blocktypeReg)
	// Welle 42: daily synthesis manual trigger. Chains over the pool's digest
	// role at constant internal (G28/E6) — the same gate as the scheduler's
	// 03:00 iteration.
	synthH := handler.NewSynthesizeHandler(pool, backendPool, blocktypeReg)

	// Status dashboard (F4-W6/G33): ONE process-wide collector feeds GET
	// /api/status (and, in W7/G34, SSE) from a cache — N pollers cost one
	// refresh, not N. scheduler supplies the dream mode (GetDreamMode).
	statusCollector := handler.NewStatusCollector(pool, backendPool, scheduler, cfgStore, blocktypeReg)
	statusH := handler.NewStatusHandler(statusCollector)
	llmlogH := handler.NewLLMLogHandler(pool, cfgStore)
	// SSE live updates (F4-W7/G34): GET /api/events broadcasts from the SAME
	// collector — one diff per tick fans out to all connections. ctx is the
	// server lifecycle (cancelled on shutdown) so streams end before
	// srv.Shutdown waits on them.
	eventsH := handler.NewEventsHandler(ctx, pool, statusCollector, cfgStore)

	// ── MCP endpoint (Streamable HTTP, authenticated) ──────────────
	queryHTTPHandler := handler.WithScheduler(scheduler, queryHandler.HandleQuery)
	mcpH := handler.NewMCPHandler(handler.MCPConfig{
		Pool:         pool,
		QueryHandler: http.HandlerFunc(queryHTTPHandler),
		Cfg:          cfgStore,
		Blocktypes:   blocktypeReg,
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
		// Graph overview — scope-pure Louvain cluster supergraph (F5-W6, gated)
		r.Get("/api/graph/overview", overviewH.HandleOverview)
		// Whoami — key identity for the SPA login gate (F4-W3)
		r.Get("/api/whoami", whoamiH.HandleWhoami)
		// Manage — CRUD + Guard API
		r.Post("/api/manage", manageH.HandleManage)
		// Block-type registry surface (workflow W1 reads + W2 writes); both
		// gates live inside the mount (RequireMember for GET,
		// RequireAdminOrTenantAdmin for PUT/DELETE, design/03 §5.1). The 1 MB
		// body cap is the enclosing group's DefaultMaxBodySize (above).
		handler.MountTypes(r, handler.NewTypesHandler(pool, blocktypeReg))
		// Project register — reads member-gated (scope-read), writes tenant-admin
		// (workflow W4); both gate groups live inside the mount (design/03 §5.1).
		handler.MountProject(r, handler.NewProjectHandler(pool))
		// Digest — Topic map generation
		r.Post("/api/digest", digestH.HandleDigest)
		// Synthesize — manual daily synthesis trigger (Welle 42)
		r.Post("/api/synthesize/daily", synthH.HandleDaily)
		// Settings — runtime overrides (F2-W5); admin-gated inside the mount.
		handler.MountSettings(r, handler.NewSettingsHandler(pool, cfgStore))
		// Secrets — write-only sealed credentials (F2-W6); admin-gated inside.
		handler.MountSecrets(r, handler.NewSecretsHandler(pool, cfgStore))
		// SSE live stream (F4-W7/G34) — SERVER-admin only: the broadcast fans
		// ONE global diff (status + backends + EVERY tenant's llmcalls) to all
		// subscribers; a per-tenant SSE broadcast is an architecture change
		// (T37d), so the push path stays closed to tenant-admins — no push leak
		// (K-T1: the pull is per-tenant, the push is not opened). Tenant-admins
		// get their live-ish telemetry by POLLING /api/status + /api/llmlog
		// (the deliberate T37d interim); when SSE lands this gate becomes
		// RequireAdminOrTenantAdmin (anchor 1, events.go T37d migration map).
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireAdmin)
			r.Get("/api/events", eventsH.HandleEvents)
		})
		// Pull telemetry — admin OR tenant-admin (T37b/T37c, 04-W5): the gate
		// admits a tenant-admin, the HANDLER scopes the payload to the caller's
		// tenant (HandleLLMLog filters rows by api_key_id; HandleStatus serves
		// the reduced per-tenant view — own backends + own 24h rollup, no
		// server-global telemetry). A server-admin sees everything. Gate + filter
		// ship together (K-T1).
		r.Group(func(r chi.Router) {
			r.Use(handler.RequireAdminOrTenantAdmin)
			r.Get("/api/status", statusH.HandleStatus)
			r.Get("/api/llmlog", llmlogH.HandleLLMLog)
		})
		// Web-chat (F6-C4/G37): POST /api/chat/stream runs one turn and streams
		// SSE — wrapped in WithScheduler so dream yields the single llama.cpp
		// slot during a turn (like /api/query). The ctx_query tool delegates to
		// the SAME scheduler-wrapped query handler. Session routes are the
		// read-lastig GET/DELETE companions (no LLM, no scheduler signal).
		chatH := handler.NewChatHandler(pool, cfgStore, backendPool, http.HandlerFunc(queryHTTPHandler))
		r.Post("/api/chat/stream", handler.WithScheduler(scheduler, chatH.HandleStream))
		r.Get("/api/chat/sessions", chatH.HandleListSessions)
		r.Get("/api/chat/sessions/{id}", chatH.HandleGetSession)
		r.Delete("/api/chat/sessions/{id}", chatH.HandleDeleteSession)
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
