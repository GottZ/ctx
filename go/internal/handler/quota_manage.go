package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// Per-tenant quota management actions (MT T36b, 04-W4). tenant-quota-set is
// SERVER-admin only (the quota is an OPERATOR cost ceiling — a tenant that could
// raise its own budget would make the ceiling meaningless; fail-closed, the
// plan-wide doctrine). tenant-quota-get admits a tenant-admin for its OWN scope
// (transparency — OE-2: a tenant owner wants to see its own limits) and a
// server-admin for any scope. The enforcement reads context_tenant_quota live
// (T36a); a set refreshes the accountant synchronously so it takes effect at once.

// dispatchQuotaAction fans the tenant-quota-* actions out (split from
// HandleManage's switch for cyclomatic budget, like the other dispatch* helpers).
func (h *ManageHandler) dispatchQuotaAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "tenant-quota-get":
		h.handleTenantQuotaGet(w, r, ar, req)
	case "tenant-quota-set":
		h.handleTenantQuotaSet(w, r, ar, req)
	}
}

// quotaSpec is the tenant-quota-set payload. Pointer budget fields distinguish
// "absent/unlimited" (null) from a real limit; on_exceed defaults to
// external_off, enabled defaults to true.
type quotaSpec struct {
	Scope          string   `json:"scope"`
	DailyCostUSD   *float64 `json:"daily_cost_usd"`
	MonthlyCostUSD *float64 `json:"monthly_cost_usd"`
	DailyCalls     *int     `json:"daily_calls"`
	OnExceed       string   `json:"on_exceed"`
	Enabled        *bool    `json:"enabled"`
}

// quotaView renders a policy for the wire. A nil policy (no row) is the
// fail-open "unlimited" state — explicit so a tenant-admin sees it as such.
func quotaView(scope string, q *backends.TenantQuota) map[string]any {
	if q == nil {
		return map[string]any{"scope": scope, "enabled": false, "unlimited": true}
	}
	return map[string]any{
		"scope":            scope,
		"daily_cost_usd":   q.DailyCostUSD,
		"monthly_cost_usd": q.MonthlyCostUSD,
		"daily_calls":      q.DailyCalls,
		"on_exceed":        q.OnExceed,
		"enabled":          q.Enabled,
	}
}

// handleTenantQuotaGet reads one tenant scope's quota. A tenant-admin is pinned
// to its OWN scope (ar.HomeScope, payload ignored — never a foreign tenant's
// budget); a server-admin may target any scope via the payload, defaulting to
// its own.
func (h *ManageHandler) handleTenantQuotaGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	scope := ar.HomeScope
	if ar.IsServerAdmin() {
		if s := quotaTargetScope(req); s != "" {
			scope = s
		}
	}
	q, _, err := store.GetTenantQuota(r.Context(), h.pool, scope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "quota": quotaView(scope, q)})
}

// handleTenantQuotaSet writes one tenant scope's quota (server-admin only via the
// action tier). The scope must be a real tenant scope — never _global or a
// '_'-reserved system scope (063 keeps the global default in a settings key, not
// this table). on_exceed is validated before the DB CHECK for a clean 422.
func (h *ManageHandler) handleTenantQuotaSet(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if len(req.Data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "data payload required"})
		return
	}
	var spec quotaSpec
	if err := json.Unmarshal(req.Data, &spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "unparseable data payload"})
		return
	}
	if spec.Scope == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "scope required"})
		return
	}
	if spec.Scope == backends.GlobalScope || strings.HasPrefix(spec.Scope, "_") {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "quota scope must be a tenant scope, not _global or a reserved '_' scope (the global default lives in the pool.default_tenant_quota setting)",
		})
		return
	}
	onExceed := spec.OnExceed
	if onExceed == "" {
		onExceed = backends.QuotaExceedExternalOff
	}
	if onExceed != backends.QuotaExceedBlock && onExceed != backends.QuotaExceedExternalOff {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false, "error": `on_exceed must be "block" or "external_off"`,
		})
		return
	}
	enabled := true
	if spec.Enabled != nil {
		enabled = *spec.Enabled
	}
	q := backends.TenantQuota{
		DailyCostUSD:   spec.DailyCostUSD,
		MonthlyCostUSD: spec.MonthlyCostUSD,
		DailyCalls:     spec.DailyCalls,
		OnExceed:       onExceed,
		Enabled:        enabled,
	}
	if err := store.UpsertTenantQuota(ctx, h.pool, spec.Scope, q, actorID(r)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	// Refresh the accountant so the new policy is live immediately (the 063
	// NOTIFY also fires, but the accountant polls on TTL — make a set instant,
	// mirroring the gaming-mode synchronous reload).
	if h.quota != nil {
		_ = h.quota.RefreshNow(ctx)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "quota": quotaView(spec.Scope, &q)})
}

// quotaTargetScope extracts an optional target scope from the payload (server-
// admin get path).
func quotaTargetScope(req manageRequest) string {
	if len(req.Data) == 0 {
		return ""
	}
	var d struct {
		Scope string `json:"scope"`
	}
	if json.Unmarshal(req.Data, &d) != nil {
		return ""
	}
	return d.Scope
}
