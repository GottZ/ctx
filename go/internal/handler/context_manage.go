package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DreamController is the interface for controlling dream mode from the manage handler.
type DreamController interface {
	SetDreamMode(mode int32, throttleInterval time.Duration)
	GetDreamMode() (mode int32, throttleInterval time.Duration)
}

// ManageHandler handles POST /api/manage.
type ManageHandler struct {
	pool            *pgxpool.Pool
	cfg             ConfigStore
	dreamController DreamController
	backendPool     *backends.Pool
	auditController AuditController
	// settingsReload re-builds the config snapshot from context_settings after
	// a gaming-mode write (the cfg interface can't bind settings.Reload itself
	// — config must not import store). Bound to settings.Reload(pool, cfg) in
	// server.go; nil in tests that don't exercise gaming-mode mutations.
	settingsReload func(context.Context) error
	// quota feeds the tenant-quota-* actions (T36b): a set refreshes it
	// synchronously so the new policy is live at once. nil in tests that don't
	// exercise quota mutations (the get path then reads the table directly).
	quota *backends.QuotaAccountant
}

// NewManageHandler creates a new ManageHandler. cfg is the runtime-config
// snapshot source (F1-W6): dream-stats renders the back-off policy from a
// per-request snapshot, so /api/manage shows the generation the scheduler
// actually runs — not a boot copy that would lie after a settings flip.
// backendPool feeds the backend-* actions (F3-P1) including the synchronous
// post-mutation reload. auditController feeds blocks-audit-* (G41); the
// production scheduler implements both controller interfaces. settingsReload
// is the synchronous post-write config reload for gaming-mode (F3-P6).
func NewManageHandler(pool *pgxpool.Pool, cfg ConfigStore, dreamController DreamController, backendPool *backends.Pool, auditController AuditController, settingsReload func(context.Context) error, quota *backends.QuotaAccountant) *ManageHandler {
	return &ManageHandler{pool: pool, cfg: cfg, dreamController: dreamController, backendPool: backendPool, auditController: auditController, settingsReload: settingsReload, quota: quota}
}

type manageRequest struct {
	Action   string          `json:"action"`
	ID       string          `json:"id"`
	Data     json.RawMessage `json:"data"`
	Category string          `json:"category"`
	Status   string          `json:"status"`
	Limit    int             `json:"limit"`
}

// HandleManage dispatches CRUD and guard management actions.
func (h *ManageHandler) HandleManage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// Auth from middleware context.
	authResult := AuthResultFromContext(ctx)
	if authResult == nil || !authResult.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// Parse body.
	var req manageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("manage: invalid request body", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid request body",
		})
		return
	}

	// MT T25 (05-A8): two-tier admin gate before dispatch (design 05 §4.4).
	if !enforceActionTier(w, req, authResult) {
		return
	}

	switch req.Action {
	case "stats":
		h.handleStats(w, r, authResult)
	case "get":
		h.handleGet(w, r, authResult, req)
	case "list-categories":
		h.handleListCategories(w, r, authResult)
	case "list-meta":
		h.handleListMeta(w, r, authResult)
	case "update":
		h.handleUpdate(w, r, authResult, req)
	case "delete":
		h.handleDelete(w, r, authResult, req)
	case "guard-list":
		h.handleGuardList(w, r, authResult, req)
	case "guard-stats":
		h.handleGuardStats(w, r, authResult)
	case "guard-resolve":
		h.handleGuardResolve(w, r, authResult, req)
	case "dream-stats":
		h.handleDreamStats(w, r, authResult)
	case "dream-review":
		h.handleDreamReview(w, r, authResult)
	case "dream-mode":
		h.handleDreamMode(w, r, req)
	case "gaming-mode":
		h.handleGamingMode(w, r, req)
	case "mcp-client-create", "mcp-client-list", "mcp-client-delete":
		h.dispatchMCPClientAction(w, r, authResult, req)
	case "backend-create", "backend-update", "backend-delete", "backend-list", "backend-test":
		h.dispatchBackendAction(w, r, authResult, req)
	case "tenant-quota-get", "tenant-quota-set":
		h.dispatchQuotaAction(w, r, authResult, req)
	case "api-key-create", "api-key-list", "api-key-delete":
		h.dispatchAPIKeyAction(w, r, req)
	case "tenant-create", "tenant-list", "tenant-get", "tenant-update", "tenant-delete",
		"tenant-grant-create", "tenant-grant-list", "tenant-grant-delete",
		// tenant-limit-set / tenant-usage-get (BEQ-1b, design/02 §3): set the
		// structural per-tenant caps (server-admin) and read the own usage+limits
		// (tenant-admin). They ROUTE through the tenant dispatcher but split on TIER
		// in actionTier (set = tierServerAdmin, usage-get = tierTenantAdmin), not
		// here (routing ⟂ tier). Folded into this case arm to add NO new HandleManage
		// branch (cyclop budget, max-complexity 25).
		"tenant-limit-set", "tenant-usage-get",
		// scope-overview (MT 04-W6/A0, design/04 §3) rides the same server-admin
		// dispatcher + tier as the tenant family: an additive READ of the per-scope
		// counts + scope→tenant mapping. Folded into this case arm (no new branch)
		// to keep HandleManage under the cyclop budget (max-complexity 25).
		"scope-overview",
		// scope-create/scope-list (BE5-3, Masterplan K1): self-service scope
		// provisioning + the tenant's own scope inventory. They ROUTE through the
		// tenant dispatcher but are tierTenantAdmin (not server-admin) — the tier is
		// decided in actionTier, not here (routing ⟂ tier). Folded into this case arm
		// to add NO new HandleManage branch (cyclop budget, max-complexity 25).
		"scope-create", "scope-list",
		// block-grant-* (T43, 07-W6) ride the same grant-family dispatcher + tier
		// (server-admin) as tenant-grant-*; the per-block ownership gate lives in the
		// handler. Folding them into this one case arm keeps HandleManage under the
		// cyclop budget (no new branch).
		"block-grant-create", "block-grant-list", "block-grant-revoke":
		h.dispatchTenantAction(w, r, authResult, req)
	case "blocks-audit-start", "blocks-audit-status", "blocks-classify-start", "blocks-classify-status":
		h.dispatchBlocksAction(w, r, req)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "Unknown action",
		})
	}
}

// dispatchMCPClientAction fans the mcp-client-* actions out (split from
// HandleManage's switch for cyclomatic budget; all admin-gated upstream).
func (h *ManageHandler) dispatchMCPClientAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "mcp-client-create":
		h.handleMCPClientCreate(w, r, ar, req)
	case "mcp-client-list":
		h.handleMCPClientList(w, r)
	case "mcp-client-delete":
		h.handleMCPClientDelete(w, r, req)
	}
}

// dispatchAPIKeyAction fans the api-key-* actions out (same split).
func (h *ManageHandler) dispatchAPIKeyAction(w http.ResponseWriter, r *http.Request, req manageRequest) {
	switch req.Action {
	case "api-key-create":
		h.handleApiKeyCreate(w, r, req)
	case "api-key-list":
		h.handleApiKeyList(w, r, req)
	case "api-key-delete":
		h.handleApiKeyDelete(w, r, req)
	}
}

// dispatchBlocksAction routes the block-corpus maintenance actions (G41 audit +
// G40 classify) — kept out of HandleManage's switch so the hot dispatcher stays
// under the cyclomatic budget (mirrors dispatchBackendAction/dispatchAPIKeyAction).
func (h *ManageHandler) dispatchBlocksAction(w http.ResponseWriter, r *http.Request, req manageRequest) {
	switch req.Action {
	case "blocks-audit-start":
		h.handleBlocksAuditStart(w, r, req)
	case "blocks-audit-status":
		h.handleBlocksAuditStatus(w, r)
	case "blocks-classify-start":
		h.handleBlocksClassifyStart(w, r, req)
	case "blocks-classify-status":
		h.handleBlocksClassifyStatus(w, r)
	}
}

// adminTier classifies how a manage action is gated (MT T25, 05-A8). It
// replaces the binary actionRequiresAdmin (052/G03) with the two-tier cut of
// design 05 §4.4: server-global actions stay server-admin, the per-tenant key
// actions become tenant-admin-capable.
type adminTier int

const (
	tierOpen        adminTier = iota // no admin gate (auth + scope only)
	tierTenantAdmin                  // server-admin OR tenant-admin of own tenant
	tierServerAdmin                  // server-admin only (server-global semantics)
)

// actionTier reports the admin tier a manage action requires (MT T25, 05-A8,
// design 05 §4.4). dream-mode/gaming-mode are special-cased: only the mutating
// shape is gated; reading the current mode stays open (tierOpen) to every valid
// key.
//
// The tenant-admin tier is granted ONLY to actions whose handlers are already
// tenant-isolated — today that is api-key-create/list/delete (T22 scope→tenant
// via context_tenant_scopes, T23 own-tenant list filter, T24 404-no-oracle
// delete). Every other gated action STAYS server-admin: their handlers carry no
// tenant filter yet (handleMCPClientList takes no AuthResult, handleBackendList
// ignores it, dispatchBlocksAction passes none), so admitting a tenant-admin
// before T26(A9)/T37/the audit wave would be fail-OPEN. This keeps the §7
// pausability invariant: A8 opens only what is already isolated, never something
// closed today. tenant-*/tenant-grant-* are operator actions by nature (owner-
// register lifecycle / cross-tenant read grants, Achse 01/02). dream-/gaming-mode
// mutations are server-global by design (scheduler goroutine set / physical GPU
// lock, §4.4).
// enforceActionTier applies the two-tier admin gate (MT T25, 05-A8) for req and
// reports whether dispatch may proceed. On a tier violation it has already
// written the 403. Server-global actions require a server-admin; per-tenant
// actions (key-*) also admit a tenant-admin of the caller's own tenant — the
// fine-grained per-resource target-tenant check then lives IN the handler
// (T22/T23/T24 against the payload). tierOpen skips the gate. Extracted from
// HandleManage to keep the dispatcher under the cyclomatic budget (mirrors the
// dispatch* helpers).
func enforceActionTier(w http.ResponseWriter, req manageRequest, ar *auth.AuthResult) bool {
	switch actionTier(req) {
	case tierServerAdmin:
		return requireAdminAction(w, ar)
	case tierTenantAdmin:
		return requireTenantAdmin(w, ar, ar.TenantID)
	}
	return true // tierOpen
}

func actionTier(req manageRequest) adminTier {
	switch req.Action {
	// Per-tenant tier: tenant-isolated handlers (T22/T23/T24 — L1/L2/L3 closed).
	case "api-key-create", "api-key-list", "api-key-delete",
		// backend-create/update/delete/list: tenant-isolated by T37 (04-W5) —
		// create forces scope=ar.HomeScope (server-admin may choose), update/
		// delete gate on scope=ANY in the store layer (foreign/_global ⇒ 404),
		// list filters to _global ∪ own. The per-resource target-tenant check
		// thus lives IN the handler, exactly the A8 precondition. backend-test
		// stays server-admin below (it reaches an arbitrary backend by id with
		// the resolved key — NOT tenant-filtered, so admitting a tenant-admin
		// would be fail-OPEN, T25-LEHRE: isolate first, only then promote).
		"backend-create", "backend-update", "backend-delete", "backend-list",
		// tenant-quota-get: a tenant-admin reads its OWN quota (transparency,
		// OE-2; the handler pins the scope to ar.HomeScope). The mutating
		// tenant-quota-set stays server-admin below — the quota is an operator
		// cost ceiling, a tenant raising its own budget would void it (fail-closed).
		"tenant-quota-get",
		// tenant-usage-get (BEQ-1b, design/02 §3 / design/05 Cross-Doc #3): a
		// tenant-admin reads its OWN structural usage (scope_count/key_count) +
		// limits. Tenant-isolated by the handler, which PINS a non-server-admin
		// onto ar.TenantID (req.ID is read ONLY for a server-admin, byte-analog
		// handleTenantQuotaGet) — so a tenant-admin can never count a foreign
		// tenant (no cross-tenant counts oracle). The mutating tenant-limit-set
		// stays server-admin below (a tenant raising its own ceiling would void it).
		"tenant-usage-get",
		// scope-create/scope-list (BE5-3, Masterplan K1): tenant-isolated by
		// construction — handleScopeCreate binds to ar.TenantID and prefixes the
		// scope with the DB slug (never the payload), and handleScopeList filters on
		// ar.TenantID. So a tenant-admin can ONLY provision/enumerate its OWN
		// '<slug>:' namespace — exactly the A8 precondition "open only what is
		// already isolated". Routing lives in the tenant family, but the TIER is
		// tenant-admin (Masterplan K10 trap: do NOT follow the routing arm into
		// tierServerAdmin below, that would break D2 self-service).
		"scope-create", "scope-list":
		return tierTenantAdmin
	case "mcp-client-create", "mcp-client-list", "mcp-client-delete",
		// backend-test: reads/probes a backend by id with its resolved key and
		// is NOT tenant-filtered (poolBackendByID scans all) → server-admin.
		"backend-test",
		// tenant-quota-set: the quota is an operator cost ceiling — a tenant-
		// admin must NOT raise its own budget (that would void the limit), so
		// the write stays server-admin (the read is tenant-admin above).
		"tenant-quota-set",
		// tenant-limit-set (BEQ-1b, design/02 §3b): the structural per-tenant cap
		// (max_scopes/max_keys) is an OPERATOR ceiling — a tenant-admin raising its
		// OWN limit would hollow it out (the same fail-closed doctrine as
		// tenant-quota-set). So the WRITE stays server-admin; only the READ
		// (tenant-usage-get) is tenant-admin above. K10 tier-asymmetry: set ≠ get.
		"tenant-limit-set",
		// blocks-audit-* (G41): start causes bulk sensitivity downgrades (the
		// opsec direction), status discloses block titles/classification
		// topology. Handler takes no AuthResult (→ server-admin until isolated).
		"blocks-audit-start", "blocks-audit-status",
		// blocks-classify-* (G40): a corpus-wide mutation (even if upgrade-only)
		// + the status/samples disclose block titles/topology — same surface.
		"blocks-classify-start", "blocks-classify-status",
		// tenant-* lifecycle (MT T05a/T05b, Achse 01): the list discloses tenant
		// topology, create/update mutate the owner register (suspend = a
		// system-wide access cut at the next auth via the 060 ctx_auth gate),
		// delete is the destructive full-prune. tenant-grant-* (T17, 02-V4)
		// widen another tenant's read_scopes. Both are operator-level: stay
		// server-admin (a tenant-admin manages within, never across, tenants).
		"tenant-create", "tenant-list", "tenant-get", "tenant-update", "tenant-delete",
		"tenant-grant-create", "tenant-grant-list", "tenant-grant-delete",
		// scope-overview (MT 04-W6/A0, design/04 §3): an additive server-admin READ
		// — per-scope counts (blocks + keys) + the scope→tenant mapping, for the
		// scope landscape, the tenant-delete blast-radius, and the QuotaForm's
		// tenant→scope source. The GROUP BY is deliberately UNSCOPED (no readScopes
		// filter): tierServerAdmin justifies the global aggregate, and only counts
		// leave the store — never block content, so no cross-tenant content leak.
		"scope-overview",
		// block-grant-* (T43, 07-W6): the row-level share. create can exfiltrate a
		// block across the tenant boundary, so it stays server-admin AND carries a
		// hard per-block ownership gate IN the handler (design/07 §5.1) — the tier
		// gate alone (server-global is_admin) is NOT sufficient.
		"block-grant-create", "block-grant-list", "block-grant-revoke":
		return tierServerAdmin
	case "dream-mode":
		if isDreamModeMutation(req) {
			return tierServerAdmin
		}
		return tierOpen
	case "gaming-mode":
		// Only the MUTATING shape is gated: an ungated toggle would let any
		// tenant key flip the whole system's egress topology (herbert out ⇒
		// synthesis goes external via OpenRouter — cost + egress-character
		// change, design 03 §2.6). gaming.active gates a physical GPU host, not
		// a tenant concept — server-global by design. Status read stays open.
		if isGamingModeMutation(req) {
			return tierServerAdmin
		}
		return tierOpen
	default:
		return tierOpen
	}
}

// requireAdminAction gates a server-global manage action on the server-admin
// tier (M052 is_admin — BREAKING for non-admin keys). After MT T25 this is the
// tierServerAdmin path: operator-level actions (mcp/backend/audit/classify,
// tenant lifecycle + grants, dream/gaming mutations) that act across the whole
// process, not within one tenant. The per-tenant key actions use
// requireTenantAdmin instead — the finer cut the old is_admin-only TODO called
// for. Before any admin gate existed, EVERY valid key could mint keys for
// arbitrary scopes, revoke keys, and manage MCP clients (negatively probed red
// 2026-06-10, admin_gate_test.go).
func requireAdminAction(w http.ResponseWriter, ar *auth.AuthResult) bool {
	if ar == nil || !ar.IsValid || !ar.IsAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false, "error": "admin key required",
		})
		return false
	}
	return true
}

// requireTenantAdmin gates a per-tenant manage action (MT T25, 05-A8): a
// server-admin (authority over every tenant) OR a tenant-admin of targetTenant
// passes; everyone else gets 403. At the dispatch gate targetTenant is the
// caller's own tenant (ar.TenantID) — the tier hurdle "may you administer your
// own tenant". The fine-grained per-resource target-tenant check (is THIS
// key/scope actually in your tenant) lives in the handler (design 05 §4.2: the
// check is no longer only at dispatch but also against the payload — T22
// firstScopeOutsideTenant, T23 list filter, T24 404-no-oracle delete).
//
// server-admin is short-circuited (§4.3 #2: "server-admin OR tenant-admin of the
// target tenant") so a degenerate empty ar.TenantID never locks the operator
// out; the empty-target fail-closed guard stays sharp inside IsTenantAdminOf for
// the in-handler payload checks, where targetTenant comes from caller-supplied
// data. The 403 body is identical to requireAdminAction — no tier oracle.
func requireTenantAdmin(w http.ResponseWriter, ar *auth.AuthResult, targetTenant string) bool {
	if ar.IsServerAdmin() || ar.IsTenantAdminOf(targetTenant) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"success": false, "error": "admin key required",
	})
	return false
}

// isDreamModeMutation reports whether a dream-mode request changes state
// (non-empty data payload) as opposed to reading the current mode.
func isDreamModeMutation(req manageRequest) bool {
	return len(req.Data) > 0 && string(req.Data) != "null"
}

// isGamingModeMutation reports whether a gaming-mode request carries a mode
// flip (non-empty data) as opposed to a status read ({} or absent data). Same
// read/write split convention as dream-mode (design 03 §2.6).
func isGamingModeMutation(req manageRequest) bool {
	if len(req.Data) == 0 || string(req.Data) == "null" || string(req.Data) == "{}" {
		return false
	}
	var d struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(req.Data, &d); err != nil {
		// Malformed data on a gaming-mode call: treat as a mutation so it hits
		// the admin gate (and then the handler's 422), never a silent read.
		return true
	}
	return d.Mode != ""
}

func (h *ManageHandler) handleStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	stats, err := store.GetStats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: stats error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	resp := map[string]any{
		"action":  "stats",
		"success": true,
		"stats":   stats,
	}

	// Dream backlog + incoming forecast at a glance — surfaces whether the GPU
	// is busy now and how much load is queued to drop out of cooldown soon.
	// Non-fatal: stats stand on their own if the dream queue probe fails.
	if queue, derr := dream.QueueDepth(ctx, h.pool, ar.ReadScopes); derr == nil {
		resp["dream_queue"] = queue
	} else {
		slog.Warn("manage: dream queue probe failed", "error", derr, "request_id", reqID)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *ManageHandler) handleGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	// Resolve the block-grant set for the caller's tenant (T40a, design/07 §4):
	// a granted block becomes visible via the additive OR-arm. Fail-closed for
	// grant visibility (resolveGrants logs + returns '{}') — never crash the read.
	grants := resolveGrants(ctx, h.pool, ar)
	resolvedID, matches, err := store.ResolveBlockID(ctx, h.pool, req.ID, ar.ReadScopes, grants)
	if err != nil {
		if errors.Is(err, store.ErrAmbiguousID) {
			writeJSON(w, http.StatusOK, map[string]any{
				"action":  "get",
				"success": false,
				"error":   "Ambiguous id prefix",
				"matches": matches,
			})
			return
		}
		// Prefix-too-short and other validation errors → 400.
		if strings.Contains(err.Error(), "prefix must be at least") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		slog.Error("manage: resolve id error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolvedID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	block, err := store.GetBlock(ctx, h.pool, resolvedID, ar.ReadScopes, grants)
	if err != nil {
		slog.Error("manage: get error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	// G9 oracle fix (design/07 §5.5): log only a visible block — a 404 must leave
	// no access_log trace. A full-UUID bypass in ResolveBlockID returns the id
	// unconditionally; logging BEFORE GetBlock re-gates would write one access_log
	// row even for a foreign/ungranted block, an existence oracle over the log
	// channel. Logging here (block != nil) closes it. Log against the resolved ID.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := store.LogAccess(bgCtx, h.pool, ar.ApiKeyID, resolvedID, "manage-get"); err != nil {
			slog.Error("manage: access log error", "error", err, "request_id", reqID)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "get",
		"success": true,
		"block":   block,
	})
}

func (h *ManageHandler) handleListCategories(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	categories, err := store.ListCategories(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: list-categories error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":     "list-categories",
		"success":    true,
		"categories": categories,
	})
}

func (h *ManageHandler) handleListMeta(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	blocks, err := store.ListMeta(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: list-meta error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "list-meta",
		"success": true,
		"blocks":  blocks,
	})
}

func (h *ManageHandler) handleUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}
	if len(req.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: data",
		})
		return
	}

	var data store.UpdateBlockData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		slog.Warn("manage: invalid update data", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid data format",
		})
		return
	}

	// Size limits (match context_store.go limits).
	if msg := blockSizeLimit(strOrEmpty(data.Category), strOrEmpty(data.Title), strOrEmpty(data.Content)); msg != "" {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"success": false, "error": msg,
		})
		return
	}

	// Scope write restriction on update — the target scope must be one the key
	// may write (writableBlockScopes, same gate as /api/store create).
	if data.Scope != nil {
		if !contains(writableBlockScopes(ar), *data.Scope) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"success": false, "error": "Cannot set scope to requested value",
			})
			return
		}
	}

	// Resolve within the write-eligible scopes (home_scope ∪ shared-if-allowed):
	// a full UUID is unambiguous and the scope set is the permission filter; a
	// partial id disambiguates over the same set (ambiguity → matches list).
	// grantedBlockIDs nil: a block grant is read-only (design/07 §2.5) and MUST
	// NOT widen a write path's candidate set.
	resolvedID, matches, err := store.ResolveBlockID(ctx, h.pool, req.ID, writableBlockScopes(ar), nil)
	if err != nil {
		if errors.Is(err, store.ErrAmbiguousID) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"action":  "update",
				"success": false,
				"error":   "Ambiguous id prefix — re-specify with a longer prefix or full id",
				"matches": matches,
			})
			return
		}
		if strings.Contains(err.Error(), "prefix must be at least") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		slog.Error("manage: resolve id error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolvedID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	if status, msg := h.applySensitivityGuard(ctx, ar, reqID, resolvedID, &data); msg != "" {
		writeJSON(w, status, map[string]any{"success": false, "error": msg})
		return
	}

	block, needsReEmbed, err := store.UpdateBlock(ctx, h.pool, resolvedID, data, writableBlockScopes(ar))
	if err != nil {
		slog.Error("manage: update error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	// Re-extract temporal data when content changes.
	if data.Content != nil {
		times := store.ExtractDates(block.Content)
		if err := store.UpdateContentTimes(ctx, h.pool, block.ID, times); err != nil {
			slog.Error("manage: content_times update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
		// Always populate: createdAt is included as meta-anchor even without content times.
		if err := store.PopulateTemporal(ctx, h.pool, block.ID, times, block.CreatedAt); err != nil {
			slog.Error("manage: temporal populate failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}

	// Clear embedding so scheduler backfill regenerates it.
	if needsReEmbed {
		if err := store.ClearEmbedding(ctx, h.pool, block.ID); err != nil {
			slog.Error("manage: clear embedding failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "update",
		"success": true,
		"block":   block,
	})
}

// strOrEmpty dereferences an optional update field for the shared size check.
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// applySensitivityGuard enforces the F3 §3.5 downgrade guard on a block
// update: lowering has the same flow direction as a backend trust elevation —
// more content may reach less trusted backends — and carries the same
// friction (confirm flag + metadata audit who/when/from→to). Upgrades stay
// free. Empty msg = proceed; otherwise (status, msg) is the error response.
func (h *ManageHandler) applySensitivityGuard(ctx context.Context, ar *auth.AuthResult, reqID, resolvedID string, data *store.UpdateBlockData) (int, string) {
	if data.Sensitivity == nil {
		return 0, ""
	}
	newSens := backends.Sensitivity(*data.Sensitivity)
	if !backends.ValidSensitivity(newSens) {
		return http.StatusBadRequest, "Invalid sensitivity: must be credentials|personal|internal|public"
	}
	curSens, found, err := store.GetBlockSensitivity(ctx, h.pool, resolvedID, writableBlockScopes(ar))
	if err != nil {
		slog.Error("manage: sensitivity read error", "error", err, "request_id", reqID)
		return http.StatusInternalServerError, "Internal server error"
	}
	if !found || newSens.Rank() >= curSens.Rank() {
		return 0, ""
	}
	if !data.ConfirmSensitivityDowngrade {
		return http.StatusBadRequest, fmt.Sprintf(
			"sensitivity downgrade %s → %s opens this block to lower-trust backends — repeat with \"confirm_sensitivity_downgrade\": true",
			curSens, newSens)
	}
	data.SensitivityAudit = map[string]any{
		"by":   ar.ApiKeyID,
		"at":   time.Now().UTC().Format(time.RFC3339),
		"from": string(curSens),
		"to":   string(newSens),
	}
	slog.Warn("manage: confirmed sensitivity downgrade",
		"block_id", resolvedID, "from", string(curSens), "to", string(newSens),
		"api_key_id", ar.ApiKeyID, "request_id", reqID)
	return 0, ""
}

func (h *ManageHandler) handleDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	// Resolve within the write-eligible scopes — see handleUpdate for rationale.
	// grantedBlockIDs nil: a read-only block grant must not widen a delete path.
	resolvedID, matches, err := store.ResolveBlockID(ctx, h.pool, req.ID, writableBlockScopes(ar), nil)
	if err != nil {
		if errors.Is(err, store.ErrAmbiguousID) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"action":  "delete",
				"success": false,
				"error":   "Ambiguous id prefix — re-specify with a longer prefix or full id",
				"matches": matches,
			})
			return
		}
		if strings.Contains(err.Error(), "prefix must be at least") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": err.Error(),
			})
			return
		}
		slog.Error("manage: resolve id error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if resolvedID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	block, err := store.DeleteBlock(ctx, h.pool, resolvedID, writableBlockScopes(ar))
	if err != nil {
		slog.Error("manage: delete error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "delete",
		"success": true,
		"deleted": block,
	})
}

func (h *ManageHandler) handleGuardList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}

	items, err := store.GuardList(ctx, h.pool, ar.ReadScopes, req.Category, req.Status, limit)
	if err != nil {
		slog.Error("manage: guard-list error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":  "guard-list",
		"success": true,
		"count":   len(items),
		"blocks":  items,
	})
}

func (h *ManageHandler) handleGuardStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	stats, err := store.GetGuardStats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		slog.Error("manage: guard-stats error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}

	resp := map[string]any{
		"action":            "guard-stats",
		"success":           true,
		"total_blocks":      stats.TotalBlocks,
		"active":            stats.Active,
		"clean":             stats.Clean,
		"needs_review":      stats.NeedsReview,
		"near_duplicate":    stats.NearDuplicate,
		"unchecked":         stats.Unchecked,
		"archived_dups":     stats.ArchivedDups,
		"write_log_entries": stats.WriteLogEntries,
		"dirty_since":       stats.DirtySince,
		"last_guard_at":     stats.LastGuardAt,
		"pending_count":     stats.PendingCount,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *ManageHandler) handleGuardResolve(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id",
		})
		return
	}

	// Parse resolution from data.
	var resolveData struct {
		Resolution string `json:"resolution"`
	}
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &resolveData); err != nil {
			slog.Warn("manage: invalid resolve data", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data format",
			})
			return
		}
	}

	if resolveData.Resolution != "archive" && resolveData.Resolution != "keep" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Resolution must be 'archive' or 'keep'",
		})
		return
	}

	block, err := store.GuardResolve(ctx, h.pool, req.ID, resolveData.Resolution, writableBlockScopes(ar))
	if err != nil {
		slog.Error("manage: guard-resolve error", "error", err, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return
	}
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false, "error": "Block not found",
		})
		return
	}

	// Map guard_status to resolution string for response.
	resolution := "keep"
	if block.GuardStatus == "archived_dup" {
		resolution = "archive"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":     "guard-resolve",
		"success":    true,
		"resolved":   block,
		"resolution": resolution,
	})
}

func (h *ManageHandler) handleDreamStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()
	total, checked, linked, pendingRecheck, err := dream.Stats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream stats failed",
		})
		return
	}
	queue, err := dream.QueueDepth(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream queue depth failed",
		})
		return
	}
	// The request's one config snapshot (§2.3) — dream-stats is the only
	// manage action that consumes config, so the read lives here, not in the
	// dispatch. The rendered policy is the generation currently in effect.
	backoff, err := dream.ComputeBackoffStats(ctx, h.pool, ar.ReadScopes, h.cfg.Snapshot().DreamBackoff()) //nolint:forbidigo // MT 06 BLIND: dream back-off is a server-global scheduler policy (the dream loop is process-wide), not tenant-scoped.
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream backoff stats failed",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action":          "dream-stats",
		"success":         true,
		"total_blocks":    total,
		"dream_checked":   checked,
		"dream_links":     linked,
		"coverage_pct":    float64(checked) / float64(max(total, 1)) * 100,
		"unchecked":       total - checked,
		"pending_recheck": pendingRecheck,
		// Actionable backlog (PickBlock eligibility) + incoming-load forecast.
		"queue": queue,
		// Active back-off policy + corpus maturity distribution (how far blocks have cooled off).
		"backoff": backoff,
	})
}

func (h *ManageHandler) handleDreamReview(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult) {
	ctx := r.Context()

	// 1. Stats overview.
	total, checked, linked, pendingRecheck, err := dream.Stats(ctx, h.pool, ar.ReadScopes)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream review failed",
		})
		return
	}

	// 2. Low-confidence links (candidates for human review).
	lowConfLinks, err := h.fetchLowConfidenceLinks(ctx, ar)
	if err != nil {
		slog.Warn("dream-review: low confidence fetch failed", "error", err)
	}

	// 3. Supersedes pairs.
	supersedesPairs, err := h.fetchSupersedesPairs(ctx, ar)
	if err != nil {
		slog.Warn("dream-review: supersedes fetch failed", "error", err)
	}

	// 4. Recently checked blocks (last 10).
	recentBlocks, err := h.fetchRecentDreamBlocks(ctx, ar)
	if err != nil {
		slog.Warn("dream-review: recent blocks fetch failed", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":           "dream-review",
		"success":          true,
		"total_blocks":     total,
		"dream_checked":    checked,
		"dream_links":      linked,
		"pending_recheck":  pendingRecheck,
		"low_confidence":   lowConfLinks,
		"supersedes_pairs": supersedesPairs,
		"recent_checked":   recentBlocks,
	})
}

func (h *ManageHandler) fetchLowConfidenceLinks(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT dl.source_block_id::text, dl.target_block_id::text, dl.relationship,
			dl.raw_confidence, dl.confidence, dl.scope,
			s.title AS source_title, t.title AS target_title
		FROM context_dream_links dl
		JOIN context_blocks s ON s.id = dl.source_block_id
		JOIN context_blocks t ON t.id = dl.target_block_id
		WHERE dl.raw_confidence < 0.7
		  AND dl.scope = ANY($1::text[])
		ORDER BY dl.raw_confidence ASC
		LIMIT 20`,
		ar.ReadScopes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var sourceID, targetID, rel, scope, sourceTitle, targetTitle string
		var rawConfidence, confidence float64
		if err := rows.Scan(&sourceID, &targetID, &rel, &rawConfidence, &confidence, &scope, &sourceTitle, &targetTitle); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"source_id":      sourceID,
			"target_id":      targetID,
			"relationship":   rel,
			"raw_confidence": rawConfidence,
			"confidence":     confidence,
			"source_title":   sourceTitle,
			"target_title":   targetTitle,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) fetchSupersedesPairs(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT dl.source_block_id::text, dl.target_block_id::text,
			dl.confidence,
			s.title AS source_title, s.quality_score AS source_quality,
			t.title AS target_title, t.quality_score AS target_quality
		FROM context_dream_links dl
		JOIN context_blocks s ON s.id = dl.source_block_id
		JOIN context_blocks t ON t.id = dl.target_block_id
		WHERE dl.relationship = 'supersedes'
		  AND dl.scope = ANY($1::text[])
		ORDER BY dl.confidence DESC
		LIMIT 20`,
		ar.ReadScopes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var sourceID, targetID, sourceTitle, targetTitle string
		var confidence, sourceQuality, targetQuality float64
		if err := rows.Scan(&sourceID, &targetID, &confidence, &sourceTitle, &sourceQuality, &targetTitle, &targetQuality); err != nil {
			return nil, err
		}
		// Welle 46 Convention-Switch (2026-05-22): "A supersedes B" → A=source=newer,
		// B=target=outdated. Source is the new replacement, target is the retired block.
		results = append(results, map[string]any{
			"new_block_id": sourceID,
			"new_title":    sourceTitle,
			"new_quality":  sourceQuality,
			"old_block_id": targetID,
			"old_title":    targetTitle,
			"old_quality":  targetQuality,
			"confidence":   confidence,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) fetchRecentDreamBlocks(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT cb.id::text, cb.title, cb.category, cb.quality_score, cb.dream_checked_at,
			COALESCE(lc.cnt, 0)::int AS link_count
		FROM context_blocks cb
		LEFT JOIN (
			SELECT block_id, count(*) AS cnt FROM (
				SELECT source_block_id AS block_id FROM context_dream_links
				UNION ALL
				SELECT target_block_id AS block_id FROM context_dream_links
			) sub GROUP BY block_id
		) lc ON lc.block_id = cb.id
		WHERE cb.dream_checked_at IS NOT NULL
		  AND NOT cb.is_archived
		  AND cb.scope = ANY($1::text[])
		ORDER BY cb.dream_checked_at DESC
		LIMIT 10`,
		ar.ReadScopes,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, title, category string
		var quality float64
		var checkedAt time.Time
		var linkCount int
		if err := rows.Scan(&id, &title, &category, &quality, &checkedAt, &linkCount); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"id":            id,
			"title":         title,
			"category":      category,
			"quality_score": quality,
			"dream_checked": checkedAt.Format(time.RFC3339),
			"link_count":    linkCount,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) handleDreamMode(w http.ResponseWriter, _ *http.Request, req manageRequest) {
	if h.dreamController == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "Dream not enabled",
		})
		return
	}

	// No data = return current mode.
	if len(req.Data) == 0 || string(req.Data) == "null" {
		mode, interval := h.dreamController.GetDreamMode()
		writeJSON(w, http.StatusOK, map[string]any{
			"success":  true,
			"mode":     dreamModeStr(mode),
			"interval": interval.Seconds(),
		})
		return
	}

	var data struct {
		Mode     string `json:"mode"`
		Interval int    `json:"interval"` // seconds, 0 = default
	}
	if err := json.Unmarshal(req.Data, &data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid data: expected {mode, interval}",
		})
		return
	}

	var mode int32
	switch data.Mode {
	case "on":
		mode = 0 // DreamModeOn
	case "throttled":
		mode = 1 // DreamModeThrottled
	case "off":
		mode = 2 // DreamModeOff
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid mode: use on, throttled, or off",
		})
		return
	}

	var interval time.Duration
	if data.Interval > 0 {
		interval = time.Duration(data.Interval) * time.Second
	}

	h.dreamController.SetDreamMode(mode, interval)
	_, currentInterval := h.dreamController.GetDreamMode()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"mode":     data.Mode,
		"interval": currentInterval.Seconds(),
	})
}

func dreamModeStr(mode int32) string {
	switch mode {
	case 1:
		return "throttled"
	case 2:
		return "off"
	default:
		return "on"
	}
}

func (h *ManageHandler) handleMCPClientCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var data struct {
		Label string `json:"label"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "label is required"})
		return
	}

	client, secret, err := store.CreateOAuthClient(r.Context(), h.pool, data.Label, ar.ApiKeyID)
	if err != nil {
		slog.Error("manage: create oauth client failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"client_id":     client.ClientID,
		"client_secret": secret, // Shown once.
		"label":         client.Label,
	})
}

func (h *ManageHandler) handleMCPClientList(w http.ResponseWriter, r *http.Request) {
	clients, err := store.ListOAuthClients(r.Context(), h.pool)
	if err != nil {
		slog.Error("manage: list oauth clients failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "clients": clients})
}

func (h *ManageHandler) handleMCPClientDelete(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data struct {
		ClientID string `json:"client_id"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.ClientID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "client_id is required"})
		return
	}

	deleted, err := store.DeleteOAuthClient(r.Context(), h.pool, data.ClientID)
	if err != nil {
		slog.Error("manage: delete oauth client failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "client not found or already inactive"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": data.ClientID})
}

// firstReservedScope returns the first '_'-prefixed scope among homeScope
// and allowedScopes, or "" if none. The underscore namespace is reserved for
// system sentinels ('_global', migration 051).
func firstReservedScope(homeScope string, allowedScopes []string) string {
	if strings.HasPrefix(homeScope, "_") {
		return homeScope
	}
	for _, s := range allowedScopes {
		if strings.HasPrefix(s, "_") {
			return s
		}
	}
	return ""
}

// firstScopeOutsideTenant returns the first requested scope — homeScope first,
// then allowedScopes in order — that is NOT in ownedScopes, or "" when every
// requested scope is owned. ownedScopes is a tenant's context_tenant_scopes set
// (store.TenantScopes). It is the pure decision behind the T22/05-A5 mint gate
// (Leak-Pfad L3): a non-server-admin may mint keys only for scopes its own tenant
// owns. An empty ownedScopes makes every requested scope "outside" — the
// fail-closed default for a caller whose tenant owns nothing.
func firstScopeOutsideTenant(homeScope string, allowedScopes, ownedScopes []string) string {
	owned := make(map[string]struct{}, len(ownedScopes))
	for _, s := range ownedScopes {
		owned[s] = struct{}{}
	}
	if _, ok := owned[homeScope]; !ok {
		return homeScope
	}
	for _, s := range allowedScopes {
		if _, ok := owned[s]; !ok {
			return s
		}
	}
	return ""
}

// apiKeyCreateRequest is the JSON shape under req.Data for api-key-create.
// home_scope is REQUIRED as of v2.0.0 — empty values yield 400.
//
// TenantID is SERVER-ADMIN ONLY (S2/AM2, design/04 §D-B): only a server-admin
// may target a foreign tenant; a non-server-admin that sets it is rejected 403
// before it can ever bind a key outside its own tenant. There is deliberately NO
// role field — a freshly minted key is ALWAYS 'member' (AM3): owner/admin roles
// arise only via the tenant-create owner bootstrap and the owner-gated
// api-key-update path, never via self-service create.
type apiKeyCreateRequest struct {
	Label         string   `json:"label"`
	HomeScope     string   `json:"home_scope"`
	AllowedScopes []string `json:"allowed_scopes,omitempty"`
	TenantID      string   `json:"tenant_id,omitempty"` // SERVER-ADMIN ONLY
}

func (h *ManageHandler) handleApiKeyCreate(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data apiKeyCreateRequest
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data: expected {label, home_scope, allowed_scopes?}",
			})
			return
		}
	}

	if data.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "label is required"})
		return
	}
	// v2.0.0 breaking change: home_scope must be explicit. No default to
	// 'private' — callers must declare the tenant boundary at creation time.
	if data.HomeScope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "home_scope is required",
		})
		return
	}
	// Scope-format gate (G03): the '_' prefix is SYSTEM-RESERVED ('_global'
	// is the settings identity sentinel, 051). A key carrying '_global'
	// would collide with the global settings row identity once per-tenant
	// resolution lands. Go-side validation, no DB CHECK (v2.0.0 line).
	// Negatively probed red before the gate (admin_gate_test.go).
	if reserved := firstReservedScope(data.HomeScope, data.AllowedScopes); reserved != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "scope names starting with '_' are reserved: " + reserved,
		})
		return
	}

	ar := AuthResultFromContext(r.Context())
	if ar == nil {
		// Defense in depth — the admin gate already rejected anon callers.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// S2 (design/04 §D-B): resolve and authorize the tenant the key binds to. The
	// helper carries the AM2/T22 gates and writes its own 4xx/5xx response; a true
	// `handled` means it already answered and we must stop. Extracted to keep this
	// handler under the cyclop ceiling (the foreign-target vs self-mint branching
	// would otherwise push it over).
	bindingTenant, handled := h.resolveKeyMintTenant(w, r, data, ar)
	if handled {
		return
	}

	// Limits are ALWAYS fetched for the BINDING (target) tenant, NEVER ar.TenantID —
	// otherwise a server-admin targeting tenant B would enforce A's cap. The read is
	// FAIL-CLOSED (S3): a transient error is a 500, never a silent default to
	// unlimited (a pool-exhaustion fault would otherwise void max_keys). An
	// absent/unknown bindingTenant surfaces ErrTenantNotFound here AND again under
	// MintKeyWithQuota's FOR UPDATE — either way a 404, no oracle.
	_, maxKeys, err := store.TenantLimits(r.Context(), h.pool, bindingTenant)
	if errors.Is(err, store.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		return
	}
	if err != nil {
		slog.Error("manage: tenant limits lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	// MT T06: the key is bound to bindingTenant (creator's tenant for a self-mint,
	// the targeted tenant for a server-admin mint). role is FIXED "member" (AM3) —
	// the only create-path role. MintKeyWithQuota enforces the max_keys cap under a
	// context_tenants-row lock (race-tight, active-only), the structural limit a
	// self-service tenant cannot exceed.
	key, plaintext, err := store.MintKeyWithQuota(r.Context(), h.pool,
		data.Label, data.HomeScope, data.AllowedScopes, bindingTenant, "member", maxKeys)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrKeyQuotaExceeded):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "tenant key quota exceeded"})
		case errors.Is(err, store.ErrTenantNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		default:
			slog.Error("manage: create api key failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"id":             key.ID,
		"label":          key.Label,
		"home_scope":     key.HomeScope,
		"allowed_scopes": key.AllowedScopes,
		"tenant_role":    key.TenantRole, // Always 'member' on create (AM3).
		"api_key":        plaintext,      // Shown once.
	})
}

// resolveKeyMintTenant decides, and authorizes, the tenant a freshly minted api
// key binds to (S2, design/04 §D-B). It returns the binding tenant and
// handled=false on success, or "" and handled=true once it has written a
// 4xx/5xx response (the caller must then stop). Three cases:
//
//   - data.TenantID set: SERVER-ADMIN ONLY (AM2). A non-server-admin caller can
//     NEVER target a foreign tenant — it is rejected 403 before the field is ever
//     read as a binding, so the field is dead for it. The server-admin must still
//     pick scopes the TARGET tenant owns (foreign-target T22 → 403 otherwise), so
//     it cannot mint a cross-tenant write-key.
//   - data.TenantID empty, non-server-admin: the existing self-mint T22 gate —
//     every requested scope (home + allowed) must belong to ar.TenantID (403).
//   - data.TenantID empty, server-admin: self-mint, no T22 (byte-identical to the
//     pre-BE6 server-admin path; §5.4 pausability).
func (h *ManageHandler) resolveKeyMintTenant(w http.ResponseWriter, r *http.Request, data apiKeyCreateRequest, ar *auth.AuthResult) (string, bool) {
	if data.TenantID != "" {
		if !ar.IsServerAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "tenant_id is server-admin only"})
			return "", true
		}
		// foreign-target T22: a server-admin minting for tenant B must use B's scopes.
		if h.rejectScopeOutsideTenant(w, r, data, data.TenantID) {
			return "", true
		}
		return data.TenantID, false
	}
	if !ar.IsServerAdmin() {
		// existing self-mint T22, byte-identical to the pre-BE6 gate (05-A5, L3).
		if h.rejectScopeOutsideTenant(w, r, data, ar.TenantID) {
			return "", true
		}
	}
	return ar.TenantID, false
}

// rejectScopeOutsideTenant runs the T22 mint gate (05-A5, Leak-Pfad L3): every
// requested scope (home + allowed) must belong to ownerTenant in
// context_tenant_scopes (store.TenantScopes). In Modell C the scope→tenant map is
// the table, NOT the naive home_scope==TenantID compare (tenant_id is a UUID,
// scope is the data discriminator → never string-equal). A foreign/unowned scope
// is privilege escalation into a foreign corpus → 403. An empty ownerTenant yields
// an empty owned set → every scope is "outside" → 403 (fail-closed). It writes the
// 403/500 and returns true when it has handled the response; false when every
// requested scope is owned and the caller may proceed.
func (h *ManageHandler) rejectScopeOutsideTenant(w http.ResponseWriter, r *http.Request, data apiKeyCreateRequest, ownerTenant string) bool {
	owned, err := store.TenantScopes(r.Context(), h.pool, ownerTenant)
	if err != nil {
		slog.Error("manage: tenant scopes lookup failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return true
	}
	if outside := firstScopeOutsideTenant(data.HomeScope, data.AllowedScopes, owned); outside != "" {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false,
			"error":   "cannot create keys outside your tenant",
		})
		return true
	}
	return false
}

func (h *ManageHandler) handleApiKeyList(w http.ResponseWriter, r *http.Request, req manageRequest) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil {
		// Defense in depth — the admin gate already rejected anon callers.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	tenantFilter, activeOnly := resolveApiKeyListParams(req.Data, ar)
	keys, err := store.ListApiKeys(r.Context(), h.pool, tenantFilter, activeOnly)
	if err != nil {
		slog.Error("manage: list api keys failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "keys": keys})
}

// resolveApiKeyListParams derives the tenant filter and active-only flag for an
// api-key-list call (design 05 §6.2). A server-admin lists every tenant (empty
// filter); a non-server-admin is scoped to its own tenant (L1 — never enumerate
// foreign keys). active_only defaults to true (the named behavior change): an
// absent field returns only ACTIVE keys, and the full set incl. soft-deleted
// needs an explicit active_only=false (a *bool tells absent from explicit false).
//
// TENANT-DECISION(apikey-list-default): activeOnly=true default chosen
// (partial-index coverage + L1 isolation) — re-decidable to a false default if
// back-compat full-visibility outweighs index coverage.
func resolveApiKeyListParams(rawData json.RawMessage, ar *auth.AuthResult) (tenantFilter string, activeOnly bool) {
	activeOnly = true
	var body struct {
		ActiveOnly *bool `json:"active_only"`
	}
	if len(rawData) > 0 {
		_ = json.Unmarshal(rawData, &body)
	}
	if body.ActiveOnly != nil {
		activeOnly = *body.ActiveOnly
	}
	if !ar.IsServerAdmin() {
		tenantFilter = ar.TenantID
	}
	return tenantFilter, activeOnly
}

func (h *ManageHandler) handleApiKeyDelete(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data struct {
		ID string `json:"id"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id is required"})
		return
	}
	ar := AuthResultFromContext(r.Context())
	if ar == nil {
		// Defense in depth — the admin gate already rejected anon callers.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	// D-D: api-key-delete is the REAL revoke path and is tierTenantAdmin (owner AND
	// admin pass), so the Last-Owner + Owner-Protection riegel must gate it here too.
	// An admin (callerMayManageOwner=false) may not revoke an owner key, and no
	// caller may revoke the last active owner (design/04 §D-D, AM6).
	callerMayManageOwner := ar.IsServerAdmin() || ar.TenantRole == auth.RoleOwner
	deleted, err := store.DeleteApiKey(r.Context(), h.pool, data.ID, ar.TenantID, ar.IsServerAdmin(), callerMayManageOwner)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOwnerProtected):
			writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "owner key may only be managed by an owner or server-admin"})
		case errors.Is(err, store.ErrLastOwner):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "cannot revoke the last active owner of the tenant"})
		default:
			slog.Error("manage: delete api key failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		}
		return
	}
	if !deleted {
		// 404-equivalent, uniform for absent / already-inactive / foreign-tenant
		// / malformed — never an existence oracle for another tenant's key (L2).
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": data.ID})
}
