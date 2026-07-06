package main

import (
	"context"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/camo"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
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
// so a config replace is live from the next request on. dispatcher is the ONE
// process-wide dispatch admission layer (MW3, I-D1) feeding the query
// pipeline, the daily-synthesis trigger and the backend chat probe.
func NewRouter(ctx context.Context, pool *pgxpool.Pool, cfgStore *config.Store, scheduler *events.Scheduler, backendPool *backends.Pool, blocktypeReg *blocktype.Registry, projectHub *events.ProjectHub, dispatcher *dispatch.Dispatcher) *chi.Mux {
	r := chi.NewRouter()

	// MT 06-C5: wire the request→tenant-scope hook so SnapshotForRequest can
	// derive the caller's tenant from the auth result. This is the cycle-free
	// seam — config cannot import handler, so the handler wrapper is injected
	// here, where main already holds both packages. Set before any route is
	// mounted (and well before the server serves a request), so the
	// unsynchronized write in config rides the boot happens-before, exactly as
	// SetOverlay (main.go) does. Inert until a tenant has settings rows.
	config.SetRequestScopeHook(handler.RequestTenantScope)

	// WF T12: the block-type registry resolves per-tenant overlays off the SAME
	// request-scope seam — SnapshotForRequest derives the caller's tenant from
	// the auth result (never a caller argument, §5.4). blocktype cannot import
	// handler (cycle), so main injects the identical cycle-free wrapper here.
	// Inert until a tenant has its own context_block_types rows (buildTenantSet
	// then inherits the base pointer byte-for-byte — default-tenant equivalence).
	blocktype.SetRequestScopeHook(handler.RequestTenantScope)

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
	queryHandler := handler.NewQueryHandler(pool, cfgStore, backendPool, quota, blocktypeReg, dispatcher)
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
	// MW3: the backend-test chat probe acquires through the dispatcher (N8b).
	manageH.SetAdmitter(dispatcher)
	// Forge PULL sync engine (Achse-02 I-F). Fail-closed gates: the tenant gate
	// is store.TenantStatusForScope (found=false ⇒ skip + disable, S13); the
	// issue-policy gate refuses a run unless the registry resolves an `issue`
	// type with digest.include=false (§6.4 — else a 10k-issue import floods the
	// topic map). NOTE: builtin.go compiles issue/comment into the base set, so
	// this gate passes in production; it fires on a tenant override that sets
	// digest.include=true or a Set genuinely lacking issue (see I-F return).
	issuePolicyOK := func(ctx context.Context, scope string) (bool, string) {
		set := blocktypeReg.SnapshotForTenant(ctx, scope)
		pol, ok := set.Resolve("issue")
		if !ok {
			return false, "issue type not registered — deploy Achse-01 T3 + Welle I-C first"
		}
		if pol.Digest.Include {
			return false, "issue policy has digest.include=true — would flood the topic map at 10k+ issues/repo"
		}
		return true, ""
	}
	tenantStatus := func(ctx context.Context, scope string) (string, bool, error) {
		return store.TenantStatusForScope(ctx, pool, scope)
	}
	forgeSync := forge.NewSyncManager(pool, tenantStatus, issuePolicyOK)
	// W11: size the process-global sync concurrency semaphore from config
	// (project.sync.max_concurrent, §4.4). Boot-only (restart-only key), before the
	// router serves — the SetApplyIssues happens-before pattern, no resize race.
	forgeSync.SetMaxConcurrent(cfgStore.Snapshot().Project.Sync.MaxConcurrent) //nolint:forbidigo // MT 06 BLIND: boot-time read of the restart-only, global-only project.sync.max_concurrent — no tenant exists at router construction, and a process-global semaphore cannot carry a per-tenant override.
	// I-G Pull-APPLY: the 3-way direction logic + block/comment create-update +
	// mapping + references edges (design/02 §4.5.2/§4.5.7). The applier resolves
	// the issue type config per scope for the workflow_status mapping (the
	// metadata-only fallback when the registry is not resolvable, §4.5.4).
	forgeApplier := forge.NewApplier(pool, blocktypeReg.SnapshotForTenant)
	forgeSync.SetApplyIssues(forgeApplier.ApplyIssues)
	forgeSync.SetApplyComments(forgeApplier.ApplyComments)
	// I-H push: drains ctx-ahead entities back onto the forge (create/update as a
	// field-diff, comment-create, "#L<seq>"→"#<nr>" rename cascade) under a
	// token-scoped content-POST throttle. push_enabled defaults false (§5.6), so
	// the pass is a no-op until a tenant-admin opens the write channel.
	forgePusher := forge.NewPusher(pool, blocktypeReg.SnapshotForTenant)
	forgeSync.SetPush(forgePusher.PushProject)
	manageH.SetForgeController(forgeSync)
	// W13: the webhook inbox arm fires a debounced forge sync per project the same
	// SyncManager drives (one run engine — REST trigger, manage trigger, webhook
	// trigger). Wired here (post-Run, main.go) via the mu-guarded setter; the
	// status is discarded (a benign ErrSyncRunning/ErrSyncSaturated is logged by
	// the arm, not fatal — the delivery is a non-authoritative trigger, §5.3).
	scheduler.SetWebhookSyncTrigger(func(ctx context.Context, project store.ProjectRow) error {
		_, err := forgeSync.StartSync(ctx, project, false)
		return err
	})
	whoamiH := handler.NewWhoamiHandler(pool)
	// Camo image proxy service (design 07-camo). Fail-closed: disabled unless
	// CTX_CAMO_ENABLED is set AND the sealbox master key is present; a misconfig
	// logs and disables rather than crashing boot. Shared by both /api/img routes.
	camoH := handler.NewCamoHandler(camo.NewFromEnv())
	blobH := handler.NewBlobHandler(pool, cfgStore)
	digestH := handler.NewDigestHandler(pool, blocktypeReg)
	// Welle 42: daily synthesis manual trigger. Chains over the pool's digest
	// role at constant internal (G28/E6) — the same gate as the scheduler's
	// 03:00 iteration. MW3/E-U2: interactive classification + per-principal
	// concurrency brake live in the handler.
	synthH := handler.NewSynthesizeHandler(pool, backendPool, blocktypeReg, dispatcher)

	// Status dashboard (F4-W6/G33): ONE process-wide collector feeds GET
	// /api/status (and, in W7/G34, SSE) from a cache — N pollers cost one
	// refresh, not N. scheduler supplies the dream mode (GetDreamMode).
	statusCollector := handler.NewStatusCollector(pool, backendPool, scheduler, cfgStore, blocktypeReg, dispatcher)
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
		// Camo image proxy — MINT half (auth-gated, rate-limited). The FE has no
		// secret, so it asks the server to sign the foreign image URLs it renders
		// (design 07-camo §4.2, D2b). The matching auth-LESS fetch route is
		// mounted below, outside the Auth group.
		r.Post("/api/img/sign", camoH.HandleSign)
		// Block-type registry surface (workflow W1 reads + W2 writes); both
		// gates live inside the mount (RequireMember for GET,
		// RequireAdminOrTenantAdmin for PUT/DELETE, design/03 §5.1). The 1 MB
		// body cap is the enclosing group's DefaultMaxBodySize (above).
		handler.MountTypes(r, handler.NewTypesHandler(pool, blocktypeReg))
		// Project register — reads member-gated (scope-read), writes tenant-admin
		// (workflow W4); both gate groups live inside the mount (design/03 §5.1).
		handler.MountProject(r, handler.NewProjectHandler(pool).WithHub(projectHub))
		// Project issue READ surface (workflow W6): list / detail+thread / comments
		// / board under /api/project/{id}/issues*, member-gated (scope-read) inside
		// the mount (design/03 §4.2/§6.1). Needs the type registry to resolve the
		// board/list status set from policy data.
		handler.MountProjectIssues(r, handler.NewProjectIssuesHandler(pool, blocktypeReg).WithConfig(cfgStore))
		// Project forge-sync TRIGGER + status poll (workflow W11): POST/GET
		// /api/project/{id}/sync. POST is write-scope-gated (E6=a) + per-project rate
		// limited (project.sync.rate_limit) under the global concurrency semaphore; GET
		// is member scope-read. It shells the SAME forge.SyncManager the manage forge-
		// sync-start action drives (one run engine, two transports).
		handler.MountProjectSync(r, handler.NewProjectSyncHandler(pool, forgeSync, cfgStore))
		// Project webhook-secret lifecycle (workflow W13): POST/DELETE
		// /api/project/{id}/webhook-secret, tenant-admin inside the mount (§5.1). The
		// secret is server-generated (crypto/rand, reveal-once) and sealed in the
		// PROJECT scope under the server-fixed name — NOT via /api/secrets, whose
		// writeScope could never reach the project scope (§5.6).
		handler.MountProjectWebhookSecret(r, handler.NewWebhookSecretHandler(pool))
		// Project SSE domain-event stream (workflow W9): GET /api/project/events,
		// member-gated inside the mount, scope-filtered at the projectHub fan-out
		// (design/03 §4.5). Separate from the server-admin telemetry /api/events.
		handler.MountProjectEvents(r, handler.NewProjectEventsHandler(projectHub, pool, cfgStore))
		// Digest — Topic map generation
		r.Post("/api/digest", digestH.HandleDigest)
		// Synthesize — manual daily synthesis trigger (Welle 42). Wrapped in
		// WithScheduler as the THIRD herald entry (MW2, design/01 §4.5 + §4.6
		// N8a signal side): the synchronously waiting daily-API user counts as
		// interactive demand from HTTP ingress on — visible to the LLM-free
		// arms via activeQueries today and InteractiveDemand() from MW15 on.
		// The 03:00 scheduler path stays herald-free (background); the
		// interactive CLASSIFICATION + per-principal concurrency brake are the
		// coupled MW3 wave (E-U2, §4.6 N8a).
		r.Post("/api/synthesize/daily", handler.WithScheduler(scheduler, synthH.HandleDaily))
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
		chatH := handler.NewChatHandler(pool, cfgStore, backendPool, http.HandlerFunc(queryHTTPHandler), blocktypeReg, dispatcher)
		r.Post("/api/chat/stream", handler.WithScheduler(scheduler, chatH.HandleStream))
		r.Get("/api/chat/sessions", chatH.HandleListSessions)
		r.Get("/api/chat/sessions/{id}", chatH.HandleGetSession)
		r.Delete("/api/chat/sessions/{id}", chatH.HandleDeleteSession)
		// Write-confirmation (F6-C6 D-W6b): the SPA ConfirmCard executes a
		// staged chat write here. Header-auth ONLY (this Auth group; no cookie
		// path exists server-wide, D1-m4) — never mount outside the group.
		confirmH := handler.NewConfirmHandler(pool, blocktypeReg)
		r.Post("/api/confirm", confirmH.HandleConfirm)
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

	// Webhook inbound (workflow W13) — the FIRST unauthenticated write surface,
	// deliberately OUTSIDE the /api Auth group: GitHub POSTs here with no api key,
	// so HMAC-SHA256 over the raw body against the per-project sealed secret
	// replaces Auth(pool) (naht N2). Only the 1 MB body cap fronts it (§5.3 order:
	// Body-Cap → Lookup → HMAC → Rate-Limit → INSERT, all inside the handler). The
	// SPA reserves the /webhooks prefix (index.ts RESERVED_SERVER_PREFIXES, U04).
	webhookH := handler.NewWebhookGitHubHandler(pool, cfgStore)
	r.Group(func(r chi.Router) {
		r.Use(handler.MaxBodySize(DefaultMaxBodySize))
		r.Post("/webhooks/github/{project_id}", webhookH.HandleWebhook)
	})

	// Camo image proxy — FETCH half (auth-LESS, like the webhook: a browser <img>
	// carries no API key, so the HMAC signature IS the capability, verified before
	// any upstream request). Mounted OUTSIDE the Auth group; the /api prefix is
	// reserved server-side (index.ts RESERVED_SERVER_PREFIXES), so no SPA route
	// collides. GET only — no body cap needed (design 07-camo §4.1/§4.2, D2b).
	r.Group(func(r chi.Router) {
		r.Get("/api/img/{sig}", camoH.HandleFetch)
	})

	// Embedded SPA — mounted last: chi matches registered routes first, only
	// unknown paths fall through (history-API fallback for HTML navigations
	// only; mistyped API URLs stay 404, known path + wrong method stays 405).
	r.NotFound(web.Handler().ServeHTTP)

	return r
}
