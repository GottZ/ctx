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
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DreamController is the interface for controlling dream mode from the manage handler.
type DreamController interface {
	SetDreamMode(mode int32, throttleInterval time.Duration)
	GetDreamMode() (mode int32, throttleInterval time.Duration)
}

// OverviewController is the scheduler surface behind overview-rebuild-start:
// kick the cluster-overview rebuild ahead of its interval. The production
// scheduler implements it; wired via SetOverviewController (server.go, the
// SetForgeController pattern — no constructor churn), nil ⇒ 503.
type OverviewController interface {
	KickOverviewRebuild() bool
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
	// blocktypes feeds the T4 re-classify hook on the update path (design/01
	// §4.5, seam 5): a title/metadata update re-runs the auto-classifier for
	// type_source='auto' blocks. nil in tests without classify wiring — the
	// hook is then skipped (pre-T4 behaviour: update never classified).
	blocktypes *blocktype.Registry
	// forge feeds the forge-* sync family (I-F). Wired via SetForgeController
	// (server.go) rather than the constructor to avoid churning its 28 call
	// sites; nil ⇒ the forge-* actions answer 503.
	forge ForgeController
	// admitter is the dispatch admission layer for the backend-test chat
	// probe (MW3, design/01 §4.6 N8b: I-D1 knows no exception — every
	// exception is a future unadmitted call site). Wired via SetAdmitter
	// (server.go, same rationale as SetForgeController); nil ⇒ the probe
	// reports an admission error instead of making an unadmitted wire call.
	admitter dispatch.Admitter
	// openBox builds the sealbox for oauth-provider-create's client_secret
	// sealing (OAuth L3, 04-W4). A field so tests can inject keys without
	// mutating process env (SecretsHandler pattern); nil falls back to
	// sealbox.FromEnv lazily (sealboxOrNil) — no constructor churn.
	openBox func() (*sealbox.Box, error)
	// overview feeds overview-rebuild-start (A2-Gate der Scharfschaltung).
	// Wired via SetOverviewController (server.go); nil ⇒ the action answers 503.
	overview OverviewController
}

// SetOverviewController wires the scheduler's overview-kick surface.
// Boot happens-before: called in server.go before the server serves.
func (h *ManageHandler) SetOverviewController(oc OverviewController) { h.overview = oc }

// SetAdmitter wires the ONE process-wide dispatch admission layer (MW3).
// Boot happens-before: called in NewRouter before the server serves.
func (h *ManageHandler) SetAdmitter(a dispatch.Admitter) {
	h.admitter = a
}

// dreamLinkableTypes resolves the request's dream-linkable type allowlist
// (WF T8) for the dream Stats/QueueDepth/Backoff reads. nil registry (test
// wiring — production always passes it, cmd/ctxd/server.go) degrades
// fail-closed to an empty allowlist with a WARN: the counters then read 0
// instead of silently falling back to a compiled-in policy.
func (h *ManageHandler) dreamLinkableTypes(ctx context.Context) []string {
	if h.blocktypes == nil {
		slog.Warn("manage: block-type registry not wired — dream counters run fail-closed empty")
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx).DreamLinkableTypes()
}

// NewManageHandler creates a new ManageHandler. cfg is the runtime-config
// snapshot source (F1-W6): dream-stats renders the back-off policy from a
// per-request snapshot, so /api/manage shows the generation the scheduler
// actually runs — not a boot copy that would lie after a settings flip.
// backendPool feeds the backend-* actions (F3-P1) including the synchronous
// post-mutation reload. auditController feeds blocks-audit-* (G41); the
// production scheduler implements both controller interfaces. settingsReload
// is the synchronous post-write config reload for gaming-mode (F3-P6).
func NewManageHandler(pool *pgxpool.Pool, cfg ConfigStore, dreamController DreamController, backendPool *backends.Pool, auditController AuditController, settingsReload func(context.Context) error, quota *backends.QuotaAccountant, blocktypes *blocktype.Registry) *ManageHandler {
	return &ManageHandler{pool: pool, cfg: cfg, dreamController: dreamController, backendPool: backendPool, auditController: auditController, settingsReload: settingsReload, quota: quota, blocktypes: blocktypes}
}

type manageRequest struct {
	Action   string          `json:"action"`
	ID       string          `json:"id"`
	Data     json.RawMessage `json:"data"`
	Category string          `json:"category"`
	Status   string          `json:"status"`
	Limit    int             `json:"limit"`
	// Types/TypesExclude (WF T10): opt-in server-side type filters for
	// list-meta (bind parameters, design/01 §7-T10 R1 — never a client
	// filter over paginated lists at the 1M+/10k-issues target scale).
	// BlockRolesExclude is the documented legacy alias for TypesExclude
	// (seam 17 — the pre-071 wire name); both present ⇒ the UNION applies
	// (monotone-restrictive: more excluded = narrower, no silent precedence).
	Types             []string `json:"types"`
	TypesExclude      []string `json:"types_exclude"`
	BlockRolesExclude []string `json:"block_roles_exclude"`
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
		h.handleStats(w, r, authResult, req)
	case "get":
		h.handleGet(w, r, authResult, req)
	case "list-categories":
		h.handleListCategories(w, r, authResult)
	case "list-meta":
		h.handleListMeta(w, r, authResult, req)
	case "update":
		h.handleUpdate(w, r, authResult, req)
	case "delete":
		h.handleDelete(w, r, authResult, req)
	case "guard-list", "guard-stats", "guard-resolve":
		// Folded into one arm (WF T10): the type-* family consumed the last
		// cyclop headroom (§9.2 conflict surface, max-complexity 25) — the
		// guard trio moves to the established dispatch* helper pattern.
		h.dispatchGuardAction(w, r, authResult, req)
	case "dream-stats", "dream-review", "dream-mode", "dream-link-resolve",
		// dream-backoff-restamp (Settings-Kurven-Welle): re-evaluates every
		// cooldown stamp under the current policy after a dream.backoff_*
		// save — folded into the dream family arm (cyclop budget); the tier
		// (tierTenantAdmin, own-entitlement scope binding) lives in
		// actionTier, not here (routing ⟂ tier).
		"dream-backoff-restamp":
		// Dream family folded into one arm (dream-link-resolve wave,
		// 2026-07-26) — the guard-trio dispatch* idiom: the fold FREES two
		// HandleManage branches while adding the curation action (cyclop
		// budget, max-complexity 25). Tier split (dream-mode mutation =
		// server-admin, rest open) lives in actionTier, not here.
		h.dispatchDreamAction(w, r, authResult, req)
	case "gaming-mode", "eject-mode",
		"disable-profile-list", "disable-profile-create", "disable-profile-update",
		"disable-profile-delete", "disable-profile-toggle":
		// Abschaltprofil-Familie (092, U01-W3, design/01 §4.3 + AM-5/AM-7) in EINEM
		// case-Arm (cyclop-Budget, max-complexity 25): eject-mode ist die KANONISCHE
		// Shim-Fläche, gaming-mode der shape-kompatible Alias (beide → Profil
		// '_global'/'eject'); die disable-profile-* Actions sind tierTenantAdmin
		// (actionTierExplicit + S9-Gate), Isolation liegt im Handler+Store.
		h.dispatchDisableProfileAction(w, r, authResult, req)
	case "mcp-client-create", "mcp-client-list", "mcp-client-delete",
		// oauth-provider-* (OAuth Achse 04 W4/L3, design/04 §3.2): folded into
		// this case arm to add NO new HandleManage branch (cyclop budget,
		// max-complexity 25). Both families are the operator-global OAuth
		// config surface and share the tier: tierServerAdmin in actionTier
		// (routing ⟂ tier), pinned by the S9 enumeration gate.
		"oauth-provider-create", "oauth-provider-list", "oauth-provider-delete",
		// oauth-identity-* (OAuth R5, design/05 §4.5, E4b admin-invite): the
		// operator pre-links external identities to principals — the only
		// provisioning path. Folded into this arm (cyclop budget, same
		// pattern as oauth-provider-*); tierServerAdmin, S9-pinned.
		"oauth-identity-link", "oauth-identity-list", "oauth-identity-unlink":
		h.dispatchMCPClientAction(w, r, authResult, req)
	case "backend-create", "backend-update", "backend-reorder", "backend-delete", "backend-list", "backend-test",
		// embed-migration-* (Evokoa-Clean-Room design/04 §7 W04-7): the re-embed
		// migration control surface — folded into the backend case arm (both are
		// server-admin embedding/backend operator families) so HandleManage adds NO
		// new branch (cyclop budget, max-complexity 25, the same folding idiom the
		// guard/oauth/quota families use). dispatchBackendAction re-routes the
		// embed-migration-* prefix to dispatchEmbedMigrationAction; the tier
		// (tierServerAdmin, the global/scope-free vector space + block-ID
		// disclosure, §5 Bruchpfad 9/10) is decided in actionTier, not here
		// (routing ⟂ tier), and the EXPLICIT entries are S9-pinned.
		"embed-migration-create", "embed-migration-status", "embed-migration-pause",
		"embed-migration-resume", "embed-migration-abort", "embed-migration-confirm",
		"embed-migration-rollback", "embed-migration-cleanup", "embed-migration-purge",
		"embed-migration-failures":
		h.dispatchBackendAction(w, r, authResult, req)
	case "tenant-quota-get", "tenant-quota-set":
		h.dispatchQuotaAction(w, r, authResult, req)
	case "api-key-create", "api-key-list", "api-key-delete", "api-key-update":
		h.dispatchAPIKeyAction(w, r, req)
	case "tenant-create", "tenant-list", "tenant-get", "tenant-update", "tenant-delete",
		// project-provision (I-I, design/02 §4.6): the server-admin compound
		// (tenant + scope + owner key + project row + repo-agent key + quota).
		// Routed via the tenant dispatcher (it creates a tenant); tierServerAdmin
		// is decided in actionTier, pinned by the S9 enumeration gate.
		"project-provision",
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
	case "overview-rebuild-start":
		h.handleOverviewRebuildStart(w)
	case "type-list", "type-get", "type-create", "type-update", "type-delete":
		// Block-type registry family (WF T10, design/01 §7-T10). Tier split
		// lives in actionTier (§5.4-N1: dispatch arm AND tier entry land in
		// the SAME wave — the dispatcher default is fail-open tierOpen).
		h.dispatchTypeAction(w, r, authResult, req)
	case "issue-create", "issue-update", "issue-get", "issue-list",
		"issue-comment-create", "issue-link-create", "issue-link-delete",
		// Achse-02 forge sync family (I-F, design/02 §4.3/§4.5): folded into this
		// ONE Achse-02 case arm (cyclop budget, max-complexity 25 — no new
		// HandleManage branch). dispatchIssueAction re-routes forge-* to
		// dispatchForgeAction; the actionTier split (issue=open, forge=tenant-admin)
		// lives in actionTier, not here (routing ⟂ tier), and both are pinned by the
		// S9 enumeration gate (§5.1).
		"forge-token-set", "forge-sync-start", "forge-sync-status":
		// Achse-02 issue/comment family (design/02 §4.3, K2 Store+Tier form):
		// the OPERATOR transport over the store issue logic (store.InsertIssue-
		// Block &c), mirroring the type-* operator transport — the primary UI
		// surface is the REST /api/project issue family (W6/W7) over the SAME
		// store functions (one logic, two transports). All tierOpen; scope
		// isolation lives in the store layer (§5.2). The actionTier entry is
		// mandatory (§4.3) and pinned by the S9 enumeration gate.
		h.dispatchIssueAction(w, r, authResult, req)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "Unknown action",
		})
	}
}

// dispatchGuardAction fans the guard-* read/resolve actions out (split from
// HandleManage's switch for the cyclomatic budget when the type-* family
// landed, WF T10; all tierOpen — auth + scope only, unchanged).
func (h *ManageHandler) dispatchGuardAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "guard-list":
		h.handleGuardList(w, r, ar, req)
	case "guard-stats":
		h.handleGuardStats(w, r, ar)
	case "guard-resolve":
		h.handleGuardResolve(w, r, ar, req)
	}
}

// dispatchMCPClientAction fans the mcp-client-* AND oauth-provider-* actions
// out (split from HandleManage's switch for cyclomatic budget; all
// server-admin-gated upstream). The oauth-provider family (OAuth L3, 04-W4)
// rides this dispatcher because both are operator-global OAuth config.
func (h *ManageHandler) dispatchMCPClientAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "mcp-client-create":
		h.handleMCPClientCreate(w, r, ar, req)
	case "mcp-client-list":
		h.handleMCPClientList(w, r)
	case "mcp-client-delete":
		h.handleMCPClientDelete(w, r, req)
	case "oauth-provider-create":
		h.handleOAuthProviderCreate(w, r, ar, req)
	case "oauth-provider-list":
		h.handleOAuthProviderList(w, r)
	case "oauth-provider-delete":
		h.handleOAuthProviderDelete(w, r, req)
	case "oauth-identity-link", "oauth-identity-list", "oauth-identity-unlink":
		// R5 identity family (05 §4.5) — rides this dispatcher like the
		// provider family: both are operator-global OAuth trust config.
		h.dispatchOAuthIdentityAction(w, r, ar, req)
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
	case "api-key-update":
		h.handleApiKeyUpdate(w, r, req)
	}
}

// handleOverviewRebuildStart kicks the cluster-overview rebuild ahead of its
// interval (A2-Gate: der Engine-Cut soll seine Grabstein-Evidenz nicht sechs
// Stunden vertagen). Asynchronous by design — the rebuild runs on the
// scheduler loop with its usual yield/journal/timeout semantics; progress is
// read from graph_overview_run, not from this response.
func (h *ManageHandler) handleOverviewRebuildStart(w http.ResponseWriter) {
	if h.overview == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": "overview controller not wired"})
		return
	}
	armed := h.overview.KickOverviewRebuild()
	note := "rebuild kicked — follow graph_overview_run for the journal row"
	if !armed {
		note = "kick already pending — coalesced, no second run queued"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "armed": armed, "note": note})
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

// actionTier reports the admin tier for dispatch. Thin wrapper over
// actionTierExplicit that discards the explicit-classification bool.
func actionTier(req manageRequest) adminTier {
	t, _ := actionTierExplicit(req)
	return t
}

// actionTierExplicit reports the tier AND whether the action was EXPLICITLY
// classified (a switch case) rather than falling through to the fail-open
// default. The S9 enumeration gate (action_tier_gate_test) asserts every
// DISPATCHED manage action is explicitly classified: a new dispatch arm added
// without an actionTier entry makes ok=false ⇒ the gate goes RED (§5.1/S9 — the
// dispatcher default is fail-open tierOpen, so a forgotten entry would silently
// admit every valid key).
func actionTierExplicit(req manageRequest) (adminTier, bool) {
	switch req.Action {
	// Per-tenant tier: tenant-isolated handlers (T22/T23/T24 — L1/L2/L3 closed).
	case "api-key-create", "api-key-list", "api-key-delete",
		// api-key-update (BE6-6, design/04 §D-C): tenant-isolated like the sibling
		// key actions — store.UpdateApiKey constrains on tenant_id and the
		// per-resource owner-delegation gate (R-DELEGATE/R-OWNERKEY) lives in the
		// handler + store, so the A8 isolation precondition holds. tierTenantAdmin
		// (NOT server-admin, the K10 trap): an OWNER delegates roles; a non-owner
		// tenant-admin is blocked IN the handler (AM4), not at the tier — admitting
		// it here is exactly D3 self-service.
		"api-key-update",
		// backend-create/update/delete/list: tenant-isolated by T37 (04-W5) —
		// create forces scope=ar.HomeScope (server-admin may choose), update/
		// delete gate on scope=ANY in the store layer (foreign/_global ⇒ 404),
		// list filters to _global ∪ own. The per-resource target-tenant check
		// thus lives IN the handler, exactly the A8 precondition. backend-test
		// stays server-admin below (it reaches an arbitrary backend by id with
		// the resolved key — NOT tenant-filtered, so admitting a tenant-admin
		// would be fail-OPEN, T25-LEHRE: isolate first, only then promote).
		"backend-create", "backend-update", "backend-delete", "backend-list",
		// backend-reorder: same T37 isolation as the sibling CRUD — the ladder
		// rewrite locks ONLY rows matching backendWriteScopes (store scope
		// predicate in the FOR UPDATE statement), a foreign/unknown id in
		// data.order is a uniform 422 (no oracle). A tenant-admin reorders
		// exactly its own subset; a server-admin the global set.
		"backend-reorder",
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
		"scope-create", "scope-list",
		// forge-token-set/forge-sync-start/forge-sync-status (Achse-02 I-F,
		// design/02 §4.3): tierTenantAdmin — they inject a PAT / trigger outbound
		// sync, so a plain member must not reach them, and the per-project
		// ownership check in dispatchForgeAction (ownsProject) IS the A8 isolation
		// precondition (a tenant-admin of tenant A gets a uniform 404 on tenant B's
		// project). The EXPLICIT entry is mandatory (§5.1) — a forge-* dispatch arm
		// without it would inherit the fail-open tierOpen default; the S9
		// enumeration gate pins it RED-then-GREEN.
		"forge-token-set", "forge-sync-start", "forge-sync-status":
		return tierTenantAdmin, true
	case "mcp-client-create", "mcp-client-list", "mcp-client-delete",
		// oauth-provider-* (OAuth L3, 04-W4): the external-login IdP allowlist
		// is the INV-C trust anchor — whoever writes it decides which issuers
		// ctx trusts (an attacker-registered IdP would mint arbitrary
		// identities), and the rows are operator-global (tenant-blind, like
		// context_oauth_clients). create carries the client_secret in transit,
		// list discloses the IdP topology → ALL THREE server-admin. The
		// EXPLICIT entries are mandatory (§5.1, fail-open tierOpen default);
		// the S9 enumeration gate pins them.
		"oauth-provider-create", "oauth-provider-list", "oauth-provider-delete",
		// oauth-identity-* (OAuth R5, 05 §4.5): identity bindings decide WHO
		// a login resolves to — the same trust altitude as the provider
		// allowlist, so ALL THREE are server-admin (S9-pinned).
		"oauth-identity-link", "oauth-identity-list", "oauth-identity-unlink",
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
		// overview-rebuild-start: a manual kick recomputes every tenant
		// partition's clusters ahead of cadence — operator-scale compute
		// spend on a server-global arm (same reasoning as blocks-audit).
		"overview-rebuild-start",
		// tenant-* lifecycle (MT T05a/T05b, Achse 01): the list discloses tenant
		// topology, create/update mutate the owner register (suspend = a
		// system-wide access cut at the next auth via the 060 ctx_auth gate),
		// delete is the destructive full-prune. tenant-grant-* (T17, 02-V4)
		// widen another tenant's read_scopes. Both are operator-level: stay
		// server-admin (a tenant-admin manages within, never across, tenants).
		"tenant-create", "tenant-list", "tenant-get", "tenant-update", "tenant-delete",
		// project-provision (I-I, design/02 §4.6/E4): CREATES a tenant — the
		// tenant-allocation authority is server-admin (like tenant-create). The
		// EXPLICIT entry is mandatory (§5.1): a dispatched action without it would
		// inherit the fail-open tierOpen default; the S9 enumeration gate pins it.
		"project-provision",
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
		"block-grant-create", "block-grant-list", "block-grant-revoke",
		// embed-migration-* (Evokoa-Clean-Room design/04 §7 W04-7): the re-embed
		// migration control surface — ALL server-admin. The vector space is global
		// and scope-free (no per-tenant migration exists conceptually), and the
		// rich status view / failures list disclose block-IDs and last_error infra
		// details across ALL scopes (§5 Bruchpfad 9/10). create/confirm/rollback
		// mutate the whole corpus' embedding space; a tenant-admin must never reach
		// them. The EXPLICIT entries are mandatory (§5.1 fail-open tierOpen
		// default); the S9 enumeration gate pins them RED-then-GREEN.
		"embed-migration-create", "embed-migration-status", "embed-migration-pause",
		"embed-migration-resume", "embed-migration-abort", "embed-migration-confirm",
		"embed-migration-rollback", "embed-migration-cleanup", "embed-migration-purge",
		"embed-migration-failures",
		// type-create/update/delete (WF T10, design/01 §5.4): editing a type
		// config SWITCHES block visibility (excluded→full-pass), and tier 1
		// only has the server-global '_global' namespace — so the mutations
		// are server-admin. The tenant-row tier (tierTenantAdmin + hard
		// ar.TenantID scope binding in the handler) is wave T12. This entry
		// lands in the SAME wave as the dispatch arm (§5.4-N1: the dispatcher
		// default is fail-open tierOpen — a forgotten entry would admit every
		// valid key; the DB-less 403 gate probe pins it).
		"type-create", "type-update", "type-delete":
		return tierServerAdmin, true
	case "type-list", "type-get":
		// Deliberately open (design/01 §5.4): every UI needs type metadata
		// for badges. The HANDLER scopes the rows to '_global' ∪ the caller's
		// own tenant namespace (K-T1: gate admits only, handler scopes).
		return tierOpen, true
	case "issue-create", "issue-update", "issue-get", "issue-list",
		"issue-comment-create", "issue-link-create", "issue-link-delete":
		// Achse-02 issue/comment family (design/02 §4.3, K2 Store+Tier form):
		// tierOpen — scope isolation is enforced in the store layer
		// (writableBlockScopes / ReadScopes, §5.2), not by an admin tier. The
		// EXPLICIT entry is mandatory (§4.3 "verbindlicher actionTier-Eintrag pro
		// Action"): without it these dispatched actions would inherit the
		// fail-open tierOpen default silently — the S9 enumeration gate pins it.
		return tierOpen, true
	case "dream-mode":
		if isDreamModeMutation(req) {
			return tierServerAdmin, true
		}
		return tierOpen, true
	case "dream-backoff-restamp":
		// A corpus mutation, but tenant-isolated by construction: the handler
		// binds the UPDATE to HomeScope + AllowedScopes (never the grant-widened
		// ReadScopes), so a caller only re-schedules its OWN blocks — the A8
		// precondition "open only what is already isolated". tierTenantAdmin
		// matches the settings surface that triggers it (RequireAdminOrTenantAdmin
		// gates the dream.backoff_* PUTs); a plain member must not bulk-rewrite
		// dream scheduling. EXPLICIT entry mandatory (§5.1, S9-pinned).
		return tierTenantAdmin, true
	case "dream-link-resolve":
		// Dream-link curation (2026-07-26): tierOpen like guard-resolve —
		// the write gate is writableBlockScopes in the store layer
		// (source-block scope, uniform not found, no existence oracle), not
		// an admin tier. The EXPLICIT entry is mandatory (§5.1: the
		// dispatcher default is fail-open tierOpen — for an OPEN action the
		// tier would not change, but the S9 enumeration gate pins every
		// dispatched action as a deliberate classification, not a
		// fall-through).
		return tierOpen, true
	case "gaming-mode", "eject-mode":
		// Only the MUTATING shape is gated: an ungated toggle would let any
		// tenant key flip the whole system's egress topology (herbert out ⇒
		// synthesis goes external via OpenRouter — cost + egress-character
		// change, design 03 §2.6). The toggle targets the '_global' eject
		// profile — a physical GPU host, not a tenant concept — so server-global
		// by design. Status read stays open (U01-E6: legacy tierOpen surface).
		// eject-mode (AM-7 canonical) and gaming-mode (alias) share this arm;
		// both EXPLICIT entries are mandatory (S9 fail-open probe).
		if isGamingModeMutation(req) {
			return tierServerAdmin, true
		}
		return tierOpen, true
	case "disable-profile-list", "disable-profile-create", "disable-profile-update",
		"disable-profile-delete", "disable-profile-toggle":
		// Abschaltprofile (092, U01-W3, AM-5 VOLL). tierTenantAdmin — NOT
		// server-admin: the handler + store are tenant-isolated (scope predicate
		// = profileWriteScopes; create forces scope to ar.HomeScope for a
		// tenant-admin; a foreign/_global profile matches zero rows → 404), so
		// this satisfies the A8 precondition "open only what is already isolated".
		// A tenant-admin may thus manage its OWN-scope profiles and READ _global
		// ones (list), but never mutate a _global profile (gate g). The EXPLICIT
		// entry is mandatory (§5.1): a dispatched action without it inherits the
		// fail-open tierOpen default; the S9 enumeration gate pins it RED-then-GREEN.
		return tierTenantAdmin, true
	default:
		return tierOpen, false
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

func (h *ManageHandler) handleStats(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
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
	if drift, ok := h.driftCensus(w, r, ar, req); ok && drift != nil {
		resp["drift"] = drift
	} else if !ok {
		return // driftCensus already wrote the error response
	}

	// Dream backlog + incoming forecast at a glance — surfaces whether the GPU
	// is busy now and how much load is queued to drop out of cooldown soon.
	// Non-fatal: stats stand on their own if the dream queue probe fails.
	if queue, derr := dream.QueueDepth(ctx, h.pool, ar.ReadScopes, h.dreamLinkableTypes(ctx)); derr == nil {
		resp["dream_queue"] = queue
	} else {
		slog.Warn("manage: dream queue probe failed", "error", derr, "request_id", reqID)
	}

	writeJSON(w, http.StatusOK, resp)
}

// driftCensusRequest is the OPT-IN payload of the stats action's drift section
// (design 04 §4.7, wave B-W5). Absent or false ⇒ the stats response is
// byte-identical to what it always was.
type driftCensusRequest struct {
	// Drift asks for the per-type census alone. Supplying GoldIDs implies it,
	// so the sweep driver sends one field, not two.
	Drift   bool     `json:"drift"`
	GoldIDs []string `json:"gold_ids"`
}

// driftCensus renders the ADDITIVE drift section of the stats response.
//
// Three properties, in the order they matter:
//
//  1. It is opt-in. A stats request that does not ask gets the response it
//     always got — no new key, no extra query, no cost. The census is four
//     aggregates over context_blocks grouped by type; at the 1M+ target that is
//     a scan, and it must never ride along on the ordinary stats poll (the
//     statusline calls that endpoint).
//  2. It is server-admin only. The section discloses the type composition of
//     the whole visible corpus and the lifecycle stamps of addressed blocks —
//     server-global observability, the same class /api/status' db section is
//     gated at. The stats ACTION itself stays tierOpen: a non-admin asking for
//     the section gets 403 and an unchanged response otherwise, so the gate
//     adds a capability rather than removing one.
//  3. It is scope-filtered anyway. store.GetDriftCensus takes ar.ReadScopes
//     like every other read — the admin gate is on top of the scope predicate,
//     never instead of it.
//
// Returns (section, true) on success, (nil, true) when nothing was asked for,
// and (nil, false) after having written an error response itself.
func (h *ManageHandler) driftCensus(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) (*store.DriftCensus, bool) {
	if len(req.Data) == 0 || string(req.Data) == "null" {
		return nil, true
	}
	var d driftCensusRequest
	if err := json.Unmarshal(req.Data, &d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Invalid data payload for stats",
		})
		return nil, false
	}
	if !d.Drift && len(d.GoldIDs) == 0 {
		return nil, true
	}
	if !requireAdminAction(w, ar) {
		return nil, false
	}

	ctx := r.Context()
	census, err := store.GetDriftCensus(ctx, h.pool, ar.ReadScopes, h.retrievableTypes(ctx), d.GoldIDs)
	if err != nil {
		slog.Error("manage: drift census error", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return nil, false
	}
	return census, true
}

// retrievableTypes is the retrieval allowlist from the per-request registry
// snapshot — the same source the query path reads it from. An unwired registry
// yields an empty list, which makes the census report zero retrievable blocks
// rather than silently counting excluded types as retrievable.
func (h *ManageHandler) retrievableTypes(ctx context.Context) []string {
	if h.blocktypes == nil {
		slog.Warn("manage: block-type registry not wired — drift census reports no retrievable types")
		return nil
	}
	return h.blocktypes.SnapshotForRequest(ctx).VisibleTypes()
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

func (h *ManageHandler) handleListMeta(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	reqID := RequestIDFromContext(ctx)

	// WF T10: opt-in server-side type filters; block_roles_exclude is the
	// legacy alias for types_exclude — both present ⇒ union (see manageRequest).
	blocks, err := store.ListMeta(ctx, h.pool, ar.ReadScopes, req.Types, unionExcludes(req.TypesExclude, req.BlockRolesExclude))
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

	// Type re-classification (WF T10, D4: REST only): registry-validated,
	// fail-closed — an unwired registry rejects instead of writing an
	// unvalidated name (422). The store then sets type_source='manual',
	// which permanently overrides the auto-classifier (T4 semantics).
	if data.Type != nil {
		if msg := h.validateBlockTypeName(ctx, *data.Type); msg != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"success": false, "error": msg,
			})
			return
		}
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

	// WF T4 (design/01 §4.5, seam 5): re-run the auto-classifier when title or
	// metadata changed — safe because ClassifyBlockAfterUpsert only writes
	// type_source='auto' blocks (manual wins permanently, sensitivity_source
	// pattern). Deliberately match-only: a block whose new title stops matching
	// keeps its current type (the hook promotes, never demotes). Non-fatal.
	if h.blocktypes != nil && (data.Title != nil || data.Metadata != nil) {
		set := h.blocktypes.SnapshotForRequest(ctx)
		if _, err := store.ClassifyBlockAfterUpsert(ctx, h.pool, set, block.ID, block.Title, block.Metadata); err != nil {
			slog.Warn("manage: re-classify on update failed", "error", err, "block_id", block.ID, "request_id", reqID)
		}
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

	items, err := store.GuardList(ctx, h.pool, ar.ReadScopes, req.Category, req.Status, req.Types, limit)
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

	// Parse resolution (and the optional batch id list) from data.
	var resolveData struct {
		Resolution string   `json:"resolution"`
		IDs        []string `json:"ids"`
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

	if req.ID == "" && len(resolveData.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required field: id (or data.ids)",
		})
		return
	}
	if req.ID != "" && len(resolveData.IDs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Provide either id or data.ids, not both",
		})
		return
	}

	if resolveData.Resolution != "archive" && resolveData.Resolution != "keep" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Resolution must be 'archive' or 'keep'",
		})
		return
	}

	// Batch path: every id is accounted for (resolved or skipped+reason);
	// the single-id wire shape below stays byte-identical.
	if len(resolveData.IDs) > 0 {
		resolved, skipped, err := store.GuardResolveBatch(ctx, h.pool, resolveData.IDs, resolveData.Resolution, writableBlockScopes(ar))
		if err != nil {
			slog.Error("manage: guard-resolve batch error", "error", err, "request_id", reqID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"success": false, "error": "Internal server error",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"action":         "guard-resolve",
			"success":        true,
			"resolution":     resolveData.Resolution,
			"resolved_count": len(resolved),
			"skipped_count":  len(skipped),
			"resolved":       resolved,
			"skipped":        skipped,
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
	// ONE policy snapshot per request (WF T8): all three counters read the
	// same dream-linkable allowlist generation.
	linkable := h.dreamLinkableTypes(ctx)
	total, checked, linked, pendingRecheck, err := dream.Stats(ctx, h.pool, ar.ReadScopes, linkable)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream stats failed",
		})
		return
	}
	queue, err := dream.QueueDepth(ctx, h.pool, ar.ReadScopes, linkable)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "dream queue depth failed",
		})
		return
	}
	// The request's one config snapshot (§2.3) — dream-stats is the only
	// manage action that consumes config, so the read lives here, not in the
	// dispatch. The rendered policy is the generation currently in effect.
	backoff, err := dream.ComputeBackoffStats(ctx, h.pool, ar.ReadScopes, linkable, h.cfg.Snapshot().DreamBackoff()) //nolint:forbidigo // MT 06 BLIND: dream back-off is a server-global scheduler policy (the dream loop is process-wide), not tenant-scoped.
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
	total, checked, linked, pendingRecheck, err := dream.Stats(ctx, h.pool, ar.ReadScopes, h.dreamLinkableTypes(ctx))
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
	// NOT pinned (M119): a pinned link IS the completed human review — it has
	// no business re-appearing in the candidate queue, however low the LLM's
	// self-assessment was.
	rows, err := h.pool.Query(ctx,
		`SELECT dl.source_block_id::text, dl.target_block_id::text, dl.relationship,
			dl.raw_confidence, dl.confidence, dl.scope, dl.rationale,
			s.title AS source_title, t.title AS target_title
		FROM context_dream_links dl
		JOIN context_blocks s ON s.id = dl.source_block_id
		JOIN context_blocks t ON t.id = dl.target_block_id
		WHERE dl.raw_confidence < 0.7
		  AND NOT dl.pinned
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
		var rationale *string
		var rawConfidence, confidence float64
		if err := rows.Scan(&sourceID, &targetID, &rel, &rawConfidence, &confidence, &scope, &rationale, &sourceTitle, &targetTitle); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"source_id":      sourceID,
			"target_id":      targetID,
			"relationship":   rel,
			"raw_confidence": rawConfidence,
			"confidence":     confidence,
			"rationale":      rationale,
			"source_title":   sourceTitle,
			"target_title":   targetTitle,
		})
	}
	return results, rows.Err()
}

func (h *ManageHandler) fetchSupersedesPairs(ctx context.Context, ar *auth.AuthResult) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT dl.source_block_id::text, dl.target_block_id::text,
			dl.confidence, dl.pinned, dl.rationale,
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
		var rationale *string
		var pinned bool
		var confidence, sourceQuality, targetQuality float64
		if err := rows.Scan(&sourceID, &targetID, &confidence, &pinned, &rationale, &sourceTitle, &sourceQuality, &targetTitle, &targetQuality); err != nil {
			return nil, err
		}
		// Welle 46 Convention-Switch (2026-05-22): "A supersedes B" → A=source=newer,
		// B=target=outdated. Source is the new replacement, target is the retired block.
		// source_id/target_id/relationship are the resolve identifiers for
		// dream-link-resolve (additive — the new_/old_ shape stays untouched).
		results = append(results, map[string]any{
			"new_block_id": sourceID,
			"new_title":    sourceTitle,
			"new_quality":  sourceQuality,
			"old_block_id": targetID,
			"old_title":    targetTitle,
			"old_quality":  targetQuality,
			"confidence":   confidence,
			"source_id":    sourceID,
			"target_id":    targetID,
			"relationship": "supersedes",
			"pinned":       pinned,
			"rationale":    rationale,
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

	// as_of is the merge floor (U01-W7, §4.5-4): the DreamTile splices this
	// mutation answer into the held status instead of a stale reload, and the
	// client's asOfFloor needs a timestamp to order it against late SSE frames.
	// Server time here (the mode flip is in-memory + immediate, no reload).
	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"mode":     data.Mode,
		"interval": currentInterval.Seconds(),
		"as_of":    serverNow(),
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
		// redirect_uris (02-W2, §4a′): the pre-registration allowlist 03 will
		// match exactly. Optional today — clients registered without it keep
		// '{}' and the static S2 allowlist until 03 enforcement (then a
		// redirect-less client matches nothing, by design).
		RedirectURIs []string `json:"redirect_uris"`
	}
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &data)
	}
	if data.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "label is required"})
		return
	}
	for _, uri := range data.RedirectURIs {
		if err := validateRegisteredRedirectURI(uri); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
			return
		}
	}

	client, secret, err := store.RegisterOAuthClient(r.Context(), h.pool, store.RegisterOAuthClientSpec{
		Label:        data.Label,
		RedirectURIs: data.RedirectURIs,
		Source:       "admin",
		CreatedBy:    ar.ApiKeyID,
	})
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
		"redirect_uris": client.RedirectURIs,
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
	// WriteScopes (078, E4b): explicit scopes the key may WRITE to beyond home_scope.
	// Must be ⊆ allowed_scopes ∪ {home_scope} — validated below (path (a)); the empty
	// default reproduces v4.2.x (home_scope ∪ shared-when-allowed) exactly.
	WriteScopes []string `json:"write_scopes,omitempty"`
	TenantID    string   `json:"tenant_id,omitempty"` // SERVER-ADMIN ONLY
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
		data.Label, data.HomeScope, data.AllowedScopes, data.WriteScopes, bindingTenant, "member", maxKeys)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrKeyQuotaExceeded):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "tenant key quota exceeded"})
		case errors.Is(err, store.ErrTenantNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "tenant not found"})
		case errors.Is(err, store.ErrWriteScopeNotAllowed):
			// 078 (E4b) path (a): a write_scope ⊄ allowed_scopes ∪ {home_scope} — a
			// blind-writer. Client error, not a 500 (mirrors the reserved-scope 400).
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
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
		"write_scopes":   key.WriteScopes, // 078: echoed so the caller sees the effective set.
		"tenant_role":    key.TenantRole,  // Always 'member' on create (AM3).
		"api_key":        plaintext,       // Shown once.
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

// apiKeyUpdateRequest is the JSON shape under req.Data for api-key-update
// (design/04 §D-C). ONLY tenant_role + active are mutable — label / home_scope /
// allowed_scopes are DELIBERATELY excluded (an allowed_scopes change would need
// the T22 analog against the target tenant, and a home_scope change moves the
// key's tenant membership; both out of scope, design/04 §D-C). TenantRole is a
// plain string ("" = leave unchanged); Active is a *bool so an absent field
// ("leave unchanged") is distinct from an explicit false (the present-flag).
type apiKeyUpdateRequest struct {
	ID         string `json:"id"`
	TenantRole string `json:"tenant_role,omitempty"`
	Active     *bool  `json:"active,omitempty"`
	// WriteScopes (078, E4b): a *[]string so absent ("leave unchanged") is distinct
	// from an explicit [] ("clear the set"). Validated ⊆ existing allowed_scopes ∪
	// {home_scope} in the store (path (a) on update; home/allowed are not mutable
	// here, so the persisted row is the authority).
	WriteScopes *[]string `json:"write_scopes,omitempty"`
}

// handleApiKeyUpdate mutates the tenant_role and/or active flag of one api key
// (BE6-6, design/04 §D-C). The escalation guards (AM4) live in
// apiKeyUpdateAuthorize; the owner-protection (R-OWNERKEY, AM6(b)) and last-owner
// (AM6(a)) guards live transactionally in store.UpdateApiKey. A non-server-admin
// reaches only keys in its own tenant; a foreign / absent / malformed id resolves
// to changed=false → 200 {success:false,"key not found"} — no existence oracle
// (L2, identical to api-key-delete). On success the patched row is re-read and
// returned as {success:true, key} (frozen FE contract ApiKeyUpdateResult).
func (h *ManageHandler) handleApiKeyUpdate(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data apiKeyUpdateRequest
	if len(req.Data) > 0 {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "Invalid data: expected {id, tenant_role?, active?}",
			})
			return
		}
	}
	if data.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id is required"})
		return
	}
	if data.TenantRole == "" && data.Active == nil && data.WriteScopes == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "nothing to update (provide tenant_role, active and/or write_scopes)",
		})
		return
	}

	ar := AuthResultFromContext(r.Context())
	if ar == nil {
		// Defense in depth — the admin gate already rejected anon callers.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// AM4 escalation guard (design/04 §D-C): a tenant_role change is OWNER-only.
	// callerMayManageOwner (server-admin OR owner) is also the authority the store
	// requires to touch an OWNER key (R-OWNERKEY); a true `handled` means it has
	// already written the 400/403. Extracted to keep this handler under the cyclop
	// ceiling (mirrors resolveKeyMintTenant).
	callerMayManageOwner, handled := apiKeyUpdateAuthorize(w, ar, data.TenantRole)
	if handled {
		return
	}

	// "" tenant_role → nil pointer (leave the role unchanged); the present-flag
	// Active is passed through untouched (nil = unchanged, COALESCE in the store).
	var rolePtr *string
	if data.TenantRole != "" {
		rolePtr = &data.TenantRole
	}

	updatedKey, changed, err := store.UpdateApiKey(r.Context(), h.pool, data.ID, ar.TenantID,
		ar.IsServerAdmin(), callerMayManageOwner, rolePtr, data.Active, data.WriteScopes)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOwnerProtected):
			writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "owner key may only be managed by an owner or server-admin"})
		case errors.Is(err, store.ErrLastOwner):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "cannot remove the last active owner of the tenant"})
		case errors.Is(err, store.ErrWriteScopeNotAllowed):
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		default:
			slog.Error("manage: update api key failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		}
		return
	}
	if !changed {
		// 404-equivalent, uniform for absent / foreign-tenant / malformed — never an
		// existence oracle for another tenant's key (L2, mirrors api-key-delete).
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "key not found"})
		return
	}

	// store.UpdateApiKey RETURNed the patched row, so the response carries it
	// directly (frozen FE contract ApiKeyUpdateResult.key, design/03 §5) — no
	// second round-trip to re-read.
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "key": updatedKey})
}

// apiKeyUpdateAuthorize applies the AM4 escalation guard for an api-key-update
// (design/04 §D-C). When a tenant_role change is requested it must name a valid
// role (else 400) AND the caller must be permitted to delegate roles — a
// server-admin or an OWNER of its tenant. A non-owner tenant-admin (it cleared the
// tierTenantAdmin gate) is rejected 403, so admin→owner self-elevation is
// structurally impossible. The returned callerMayManageOwner (server-admin OR
// owner — the exported equivalent of Role.delegates(), identical to
// handleApiKeyDelete) is the SAME authority store.UpdateApiKey needs to mutate an
// OWNER key (R-OWNERKEY). handled=true means a 4xx was already written.
func apiKeyUpdateAuthorize(w http.ResponseWriter, ar *auth.AuthResult, tenantRole string) (callerMayManageOwner, handled bool) {
	callerMayManageOwner = ar.IsServerAdmin() || ar.TenantRole == auth.RoleOwner
	if tenantRole == "" {
		return callerMayManageOwner, false
	}
	if !auth.ValidRole(tenantRole) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "tenant_role must be owner, admin, or member"})
		return callerMayManageOwner, true
	}
	// AM4: role delegation is an OWNER power — a non-owner tenant-admin must NOT set
	// tenant_role (else admin→owner self-elevation). !callerMayManageOwner is exactly
	// "neither server-admin nor owner".
	if !callerMayManageOwner {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "role change requires owner"})
		return callerMayManageOwner, true
	}
	return callerMayManageOwner, false
}
