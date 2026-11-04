// Dispatch table of POST /api/manage — the SINGLE source of routing and admin
// tier for all 93 manage actions (T03-11, design/03 §4.11). It replaces a
// three-level construct: an 18-arm switch in HandleManage, fifteen
// dispatch*Action fan-out methods, and a 211-line parallel tier classifier.
// In that construct every action existed two to four times (routing arm,
// sub-dispatcher arm, sub-sub-dispatcher arm, tier arm), and the two
// structures had to agree by hand — a dispatch arm added without a tier entry
// inherited the fail-open default silently.
//
// The form is taken from blobActions (blob.go), not invented here: a method
// expression signature so a row can name its handler directly, tier and
// routing in the SAME row, one enforcer that reads the row, and handleManage
// with an INJECTED table as the seam the wiring probe dispatches through.
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/embedmigration"
)

// manageActionFunc is the uniform shape of a /api/manage action handler. It is
// a METHOD EXPRESSION signature (the receiver is the first parameter), like
// blobActionFunc: a row can then name the existing method directly, with no
// place to drift away from the method it claims to call.
//
// Only 49 of the 89 original target handlers already carry this exact shape; the rest
// take a subset (no AuthResult, no manageRequest, neither) or derive a
// parameter. Those rows wrap the method expression in one of the adapters
// below — the adapter takes the method as a VALUE, so the handler name still
// stands in the row and the AST identity gate (manage_table_gate_test.go) can
// read it back out.
type manageActionFunc func(*ManageHandler, http.ResponseWriter, *http.Request, *auth.AuthResult, manageRequest)

// manageAction binds ONE dispatchable manage action to its admin tier and its
// handler.
//
// tier vs. tierFn: for 86 actions the tier is a function of the action name
// alone and lives in the tier column. For dream-mode, gaming-mode and
// eject-mode it is a function of (action, data shape) — only the MUTATING
// shape is gated, reading the current mode stays open to every valid key
// (isDreamModeMutation / isGamingModeMutation). A static column cannot carry
// that: tierServerAdmin would 403 a non-admin's status read, tierOpen would
// let any valid key flip the global egress topology. Those three rows set
// tierFn instead and call the existing predicates — the predicates do not
// move, they are only called from the row.
//
// A row must set exactly one of them. A row that sets NEITHER resolves to
// tierUnset and is refused with a 500 (see enforceManageActionTier) — the
// zero value of adminTier used to be tierOpen, which made a forgotten tier
// admit every valid key silently.
type manageAction struct {
	tier   adminTier
	tierFn func(manageRequest) adminTier
	handle manageActionFunc
}

// dropAuth adapts a handler that takes no AuthResult (it reads what it needs
// from the request context, or needs none).
func dropAuth(fn func(*ManageHandler, http.ResponseWriter, *http.Request, manageRequest)) manageActionFunc {
	return func(h *ManageHandler, w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
		fn(h, w, r, req)
	}
}

// dropReq adapts a handler that takes no manageRequest (its action carries no
// payload).
func dropReq(fn func(*ManageHandler, http.ResponseWriter, *http.Request, *auth.AuthResult)) manageActionFunc {
	return func(h *ManageHandler, w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, _ manageRequest) {
		fn(h, w, r, ar)
	}
}

// dropAuthReq adapts a handler that takes neither.
func dropAuthReq(fn func(*ManageHandler, http.ResponseWriter, *http.Request)) manageActionFunc {
	return func(h *ManageHandler, w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, _ manageRequest) {
		fn(h, w, r)
	}
}

// onlyWriter adapts a handler that takes the ResponseWriter alone.
func onlyWriter(fn func(*ManageHandler, http.ResponseWriter)) manageActionFunc {
	return func(h *ManageHandler, w http.ResponseWriter, _ *http.Request, _ *auth.AuthResult, _ manageRequest) {
		fn(h, w)
	}
}

// forgeGated adapts a forge-* handler and keeps the vestibule the removed
// dispatchForgeAction ran before its switch: without a wired sync engine the
// family answers 503 with this exact text, unchanged.
func forgeGated(fn func(*ManageHandler, http.ResponseWriter, *http.Request, manageRequest)) manageActionFunc {
	return func(h *ManageHandler, w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
		if h.forge == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "Sync engine not enabled"})
			return
		}
		fn(h, w, r, req)
	}
}

// withTransition adapts the one handler that takes DERIVED parameters. The
// removed dispatchEmbedMigrationAction mapped an action name to a status
// transition (embed-migration-pause ⇒ running → paused); the mapping now lives
// in the row that declares the action, its only place. Written as an adapter
// rather than a bespoke closure so this row, like the other 88, names its
// handler in the row itself. (The other nine embed-migration-* actions have
// their own handlers: resume reads the CURRENT status, so it has no fixed
// `from` to derive.)
func withTransition(
	fn func(*ManageHandler, http.ResponseWriter, *http.Request, manageRequest, embedmigration.Status, embedmigration.Status),
	from, to embedmigration.Status,
) manageActionFunc {
	return func(h *ManageHandler, w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
		fn(h, w, r, req, from, to)
	}
}

// dreamModeTier / gamingModeTier are the two shape-dependent tier rules. They
// call the existing predicates; the read/write split and its rationale stay
// where they were documented (isDreamModeMutation, isGamingModeMutation).
func dreamModeTier(req manageRequest) adminTier {
	if isDreamModeMutation(req) {
		return tierServerAdmin
	}
	return tierOpen
}

func gamingModeTier(req manageRequest) adminTier {
	if isGamingModeMutation(req) {
		return tierServerAdmin
	}
	return tierOpen
}

// manageActions is the dispatch table: 93 actions, alphabetical, one row each.
//
// The tiers are the pre-table behaviour, action for action — this wave routes
// differently, it does not re-cut permissions.
//
// The policy behind the column (MT T25, 05-A8, design 05 §4.4) is unchanged:
// tierTenantAdmin is granted ONLY to actions whose handlers are ALREADY
// tenant-isolated — the api-key family (T22 scope→tenant, T23 own-tenant list
// filter, T24 404-no-oracle delete), the backend CRUD (T37 scope predicate),
// the disable-profile family, the own reads (tenant-quota-get,
// tenant-usage-get), the own-namespace scope provisioning, the forge family
// (ownsProject) and the model-alias family (store scope predicate + handler
// field tiers, W02-04). Every other gated action stays server-admin, because its
// handler carries no tenant filter — admitting a tenant-admin there would be
// fail-OPEN. tenant-*/tenant-grant-*/block-grant-* are operator actions by
// nature; the dream/gaming mutations are server-global by design (scheduler
// goroutine set / physical GPU lock). Read/write asymmetry is deliberate where
// it appears: quota-get and usage-get are tenant-admin, quota-set and
// limit-set are not — an operator ceiling a tenant could raise is no ceiling.
//
// Eleven rows deserve a note:
// stats, get, list-categories, list-meta, update, delete, guard-list,
// guard-stats, guard-resolve, dream-stats and dream-review had NO explicit
// tier arm before and reached tierOpen through the classifier's fail-open
// default. They are tierOpen here as well — same behaviour, but now a
// deliberate classification instead of a fall-through, which is what makes
// the enumeration gate meaningful.
var manageActions = map[string]manageAction{
	"api-key-create":         {tier: tierTenantAdmin, handle: dropAuth((*ManageHandler).handleApiKeyCreate)},
	"api-key-delete":         {tier: tierTenantAdmin, handle: dropAuth((*ManageHandler).handleApiKeyDelete)},
	"api-key-list":           {tier: tierTenantAdmin, handle: dropAuth((*ManageHandler).handleApiKeyList)},
	"api-key-update":         {tier: tierTenantAdmin, handle: dropAuth((*ManageHandler).handleApiKeyUpdate)},
	"backend-create":         {tier: tierTenantAdmin, handle: (*ManageHandler).handleBackendCreate},
	"backend-delete":         {tier: tierTenantAdmin, handle: (*ManageHandler).handleBackendDelete},
	"backend-list":           {tier: tierTenantAdmin, handle: (*ManageHandler).handleBackendList},
	"backend-reorder":        {tier: tierTenantAdmin, handle: (*ManageHandler).handleBackendReorder},
	"backend-test":           {tier: tierServerAdmin, handle: (*ManageHandler).handleBackendTest},
	"backend-update":         {tier: tierTenantAdmin, handle: (*ManageHandler).handleBackendUpdate},
	"block-grant-create":     {tier: tierServerAdmin, handle: (*ManageHandler).handleBlockGrantCreate},
	"block-grant-list":       {tier: tierServerAdmin, handle: (*ManageHandler).handleBlockGrantList},
	"block-grant-revoke":     {tier: tierServerAdmin, handle: (*ManageHandler).handleBlockGrantRevoke},
	"blocks-audit-start":     {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleBlocksAuditStart)},
	"blocks-audit-status":    {tier: tierServerAdmin, handle: dropAuthReq((*ManageHandler).handleBlocksAuditStatus)},
	"blocks-classify-start":  {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleBlocksClassifyStart)},
	"blocks-classify-status": {tier: tierServerAdmin, handle: dropAuthReq((*ManageHandler).handleBlocksClassifyStatus)},
	"delete":                 {tier: tierOpen, handle: (*ManageHandler).handleDelete},
	"disable-profile-create": {tier: tierTenantAdmin, handle: (*ManageHandler).handleDisableProfileCreate},
	"disable-profile-delete": {tier: tierTenantAdmin, handle: (*ManageHandler).handleDisableProfileDelete},
	"disable-profile-list":   {tier: tierTenantAdmin, handle: dropReq((*ManageHandler).handleDisableProfileList)},
	"disable-profile-toggle": {tier: tierTenantAdmin, handle: (*ManageHandler).handleDisableProfileToggle},
	"disable-profile-update": {tier: tierTenantAdmin, handle: (*ManageHandler).handleDisableProfileUpdate},
	"dream-backoff-restamp":  {tier: tierTenantAdmin, handle: dropReq((*ManageHandler).handleDreamBackoffRestamp)},
	"dream-link-resolve":     {tier: tierOpen, handle: (*ManageHandler).handleDreamLinkResolve},
	"dream-mode":             {tierFn: dreamModeTier, handle: dropAuth((*ManageHandler).handleDreamMode)},
	"dream-review":           {tier: tierOpen, handle: dropReq((*ManageHandler).handleDreamReview)},
	"dream-stats":            {tier: tierOpen, handle: dropReq((*ManageHandler).handleDreamStats)},
	// eject-mode is the canonical shim surface, gaming-mode its shape-compatible
	// alias (AM-7) — one handler, two names, one shape-dependent tier rule.
	"eject-mode":               {tierFn: gamingModeTier, handle: dropAuth((*ManageHandler).handleGamingMode)},
	"embed-migration-abort":    {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationAbort)},
	"embed-migration-cleanup":  {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationCleanup)},
	"embed-migration-confirm":  {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationConfirm)},
	"embed-migration-create":   {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationCreate)},
	"embed-migration-failures": {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationFailures)},
	"embed-migration-pause": {tier: tierServerAdmin,
		handle: withTransition((*ManageHandler).handleEmbedMigrationTransition,
			embedmigration.StatusRunning, embedmigration.StatusPaused)},
	"embed-migration-purge":    {tier: tierServerAdmin, handle: dropAuthReq((*ManageHandler).handleEmbedMigrationPurge)},
	"embed-migration-resume":   {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationResume)},
	"embed-migration-rollback": {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationRollback)},
	"embed-migration-status":   {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleEmbedMigrationStatus)},
	"forge-sync-start":         {tier: tierTenantAdmin, handle: forgeGated((*ManageHandler).handleForgeSyncStart)},
	"forge-sync-status":        {tier: tierTenantAdmin, handle: forgeGated((*ManageHandler).handleForgeSyncStatus)},
	"forge-token-set":          {tier: tierTenantAdmin, handle: forgeGated((*ManageHandler).handleForgeTokenSet)},
	"gaming-mode":              {tierFn: gamingModeTier, handle: dropAuth((*ManageHandler).handleGamingMode)},
	"get":                      {tier: tierOpen, handle: (*ManageHandler).handleGet},
	"guard-list":               {tier: tierOpen, handle: (*ManageHandler).handleGuardList},
	"guard-resolve":            {tier: tierOpen, handle: (*ManageHandler).handleGuardResolve},
	"guard-stats":              {tier: tierOpen, handle: dropReq((*ManageHandler).handleGuardStats)},
	"issue-comment-create":     {tier: tierOpen, handle: (*ManageHandler).handleIssueCommentCreate},
	"issue-create":             {tier: tierOpen, handle: (*ManageHandler).handleIssueCreate},
	"issue-get":                {tier: tierOpen, handle: (*ManageHandler).handleIssueGet},
	"issue-link-create":        {tier: tierOpen, handle: (*ManageHandler).handleIssueLinkCreate},
	"issue-link-delete":        {tier: tierOpen, handle: (*ManageHandler).handleIssueLinkDelete},
	"issue-list":               {tier: tierOpen, handle: (*ManageHandler).handleIssueList},
	"issue-update":             {tier: tierOpen, handle: (*ManageHandler).handleIssueUpdate},
	"list-categories":          {tier: tierOpen, handle: dropReq((*ManageHandler).handleListCategories)},
	"list-meta":                {tier: tierOpen, handle: (*ManageHandler).handleListMeta},
	"mcp-client-create":        {tier: tierServerAdmin, handle: (*ManageHandler).handleMCPClientCreate},
	"mcp-client-delete":        {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleMCPClientDelete)},
	"mcp-client-list":          {tier: tierServerAdmin, handle: dropAuthReq((*ManageHandler).handleMCPClientList)},
	// model-alias-* (W02-04): the /v1 model namespace. tierTenantAdmin with
	// the field tiers in the handler (model_alias_manage.go): _global and
	// foreign scopes are 404-equivalent for a tenant-admin, lowering a
	// sensitivity is server-admin + confirm, the shadowing floor and the
	// tenant ceiling hold for every caller.
	"model-alias-create": {tier: tierTenantAdmin, handle: (*ManageHandler).handleModelAliasCreate},
	"model-alias-delete": {tier: tierTenantAdmin, handle: (*ManageHandler).handleModelAliasDelete},
	"model-alias-list":   {tier: tierTenantAdmin, handle: dropReq((*ManageHandler).handleModelAliasList)},
	"model-alias-update": {tier: tierTenantAdmin, handle: (*ManageHandler).handleModelAliasUpdate},
	"oauth-identity-link":      {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleOAuthIdentityLink)},
	"oauth-identity-list":      {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleOAuthIdentityList)},
	"oauth-identity-unlink":    {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleOAuthIdentityUnlink)},
	"oauth-provider-create":    {tier: tierServerAdmin, handle: (*ManageHandler).handleOAuthProviderCreate},
	"oauth-provider-delete":    {tier: tierServerAdmin, handle: dropAuth((*ManageHandler).handleOAuthProviderDelete)},
	"oauth-provider-list":      {tier: tierServerAdmin, handle: dropAuthReq((*ManageHandler).handleOAuthProviderList)},
	"overview-rebuild-start":   {tier: tierServerAdmin, handle: onlyWriter((*ManageHandler).handleOverviewRebuildStart)},
	"project-provision":        {tier: tierServerAdmin, handle: (*ManageHandler).handleProjectProvision},
	"scope-create":             {tier: tierTenantAdmin, handle: (*ManageHandler).handleScopeCreate},
	"scope-list":               {tier: tierTenantAdmin, handle: (*ManageHandler).handleScopeList},
	"scope-overview":           {tier: tierServerAdmin, handle: dropAuthReq((*ManageHandler).handleScopeOverview)},
	"stats":                    {tier: tierOpen, handle: (*ManageHandler).handleStats},
	"tenant-create":            {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantCreate},
	"tenant-delete":            {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantDelete},
	"tenant-get":               {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantGet},
	"tenant-grant-create":      {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantGrantCreate},
	"tenant-grant-delete":      {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantGrantDelete},
	"tenant-grant-list":        {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantGrantList},
	"tenant-limit-set":         {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantLimitSet},
	"tenant-list":              {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantList},
	"tenant-quota-get":         {tier: tierTenantAdmin, handle: (*ManageHandler).handleTenantQuotaGet},
	"tenant-quota-set":         {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantQuotaSet},
	"tenant-update":            {tier: tierServerAdmin, handle: (*ManageHandler).handleTenantUpdate},
	"tenant-usage-get":         {tier: tierTenantAdmin, handle: (*ManageHandler).handleTenantUsageGet},
	"type-create":              {tier: tierServerAdmin, handle: (*ManageHandler).handleTypeCreate},
	"type-delete":              {tier: tierServerAdmin, handle: (*ManageHandler).handleTypeDelete},
	"type-get":                 {tier: tierOpen, handle: (*ManageHandler).handleTypeGet},
	"type-list":                {tier: tierOpen, handle: dropReq((*ManageHandler).handleTypeList)},
	"type-update":              {tier: tierServerAdmin, handle: (*ManageHandler).handleTypeUpdate},
	"update":                   {tier: tierOpen, handle: (*ManageHandler).handleUpdate},
}

// manageActionTier resolves a row's tier for THIS request: the shape-dependent
// rule if the row carries one, the static column otherwise.
func manageActionTier(a manageAction, req manageRequest) adminTier {
	if a.tierFn != nil {
		return a.tierFn(req)
	}
	return a.tier
}

// actionTier reports the admin tier a manage action requires. Thin wrapper
// over actionTierExplicit that discards the known-action bool.
func actionTier(req manageRequest) adminTier {
	t, _ := actionTierExplicit(req)
	return t
}

// actionTierExplicit reports the tier AND whether the action is a row of the
// dispatch table at all. Before the table these were two structures that had
// to agree by hand, and "explicitly classified" meant "someone remembered to
// add a tier arm" — eleven dispatched actions had not been remembered and
// reached tierOpen through a fail-open default. Now routing and tier are the
// same row: known ⇒ classified, by construction.
//
// An UNKNOWN action still reports (tierOpen, false). That is not an admission:
// it never reaches a handler, because the dispatcher answers a table miss with
// its 400 before any gate matters. The failure the tier column has to catch is
// the other one — a KNOWN action whose row declares no tier — and that is
// tierUnset in enforceManageActionTier.
func actionTierExplicit(req manageRequest) (adminTier, bool) {
	a, known := manageActions[req.Action]
	if !known {
		return tierOpen, false
	}
	return manageActionTier(a, req), true
}

// enforceManageActionTier applies the admin tier of a dispatch row and reports
// whether dispatch may proceed; on a violation it has already written the
// response. Mirror of enforceBlobActionTier, with two additions the blob table
// does not need:
//
//   - tierUnset: a row that declares no tier at all. That is a bug in the
//     table, not a caller error, so it is a 500 with a slog.Error — a 403
//     would be a tier oracle, and tierOpen (the pre-tierUnset zero value)
//     would admit every valid key. Every shipped row carries a tier, pinned
//     by manage_table_gate_test.go, so this branch is unreachable in the
//     delivered tree; it is defence in depth against a future row.
//   - the tier may depend on the request (manageActionTier).
//
// The 403 body is the shared requireAdminAction text for both admin tiers —
// no tier oracle for the caller.
func enforceManageActionTier(w http.ResponseWriter, a manageAction, req manageRequest, ar *auth.AuthResult, reqID string) bool {
	switch manageActionTier(a, req) {
	case tierUnset:
		slog.Error("manage: dispatch row carries no admin tier — refusing",
			"action", req.Action, "request_id", reqID)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "Internal server error",
		})
		return false
	case tierServerAdmin:
		return requireAdminAction(w, ar)
	case tierTenantAdmin:
		return requireTenantAdmin(w, ar, ar.TenantID)
	}
	return true // tierOpen
}

// HandleManage processes POST /api/manage.
func (h *ManageHandler) HandleManage(w http.ResponseWriter, r *http.Request) {
	h.handleManage(w, r, manageActions)
}

// handleManage is HandleManage against an INJECTED action table. The seam
// exists for the wiring probe (manage_table_gate_test.go): with 89 of 89 rows
// keeping their pre-table tier, a dispatcher that reads the tier but never
// calls enforceManageActionTier would be indistinguishable from a correct one
// — the probe dispatches a table carrying gated rows through this very
// function and pins that a non-admin is stopped BEFORE the handler runs. Same
// seam, same reason as handleBlobManage.
func (h *ManageHandler) handleManage(w http.ResponseWriter, r *http.Request, actions map[string]manageAction) {
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

	action, known := actions[req.Action]
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "Unknown action",
		})
		return
	}

	// MT T25 (05-A8): two-tier admin gate before dispatch (design 05 §4.4).
	if !enforceManageActionTier(w, action, req, authResult, reqID) {
		return
	}
	action.handle(h, w, r, authResult, req)
}
